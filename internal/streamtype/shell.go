package streamtype

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"regexp"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/creack/pty"
	"golang.org/x/term"

	"github.com/richlegrand/bitbang/internal/protocol"
)

// Mirror-line decoration. Every emitted shell mirror line is prefixed
// with mirrorPrefix so it's visually distinct from listener log
// messages (which carry a "YYYY/MM/DD HH:MM:SS " prefix from the
// stdlib logger). When the underlying writer is a TTY, the line is
// further dimmed via SGR 2 so a quick scan separates remote shell
// output from the listener's own activity.
const (
	mirrorPrefix = "│ "
	ansiDim      = "\x1b[2m"
	ansiReset    = "\x1b[0m"
)

// activeShellCount is the process-wide count of in-flight shell
// streams across all sessions. Used by MaxConcurrent enforcement on
// ShellHandler. Lives at package scope because each WebRTC peer
// creates its own ShellHandler instance; the limit needs to apply
// across all of them.
var activeShellCount atomic.Int32

// ansiEscape matches the VT/ANSI escape sequences a shell session
// realistically emits: CSI (ESC [ params intermediates final), OSC
// (ESC ] ... BEL or ST), and the shorter two-byte ESC X forms.
// Anchored nowhere — used with ReplaceAll to strip all matches.
var ansiEscape = regexp.MustCompile(
	"\x1b\\[[0-?]*[ -/]*[@-~]" + // CSI
		"|\x1b\\][^\x07\x1b]*(?:\x07|\x1b\\\\)" + // OSC ... BEL or ST
		"|\x1b[@-Z\\\\-_]") // two-byte ESC X

// otherControl strips C0/C1 control characters except newline and
// tab. Backspace, BEL, vertical tab, and friends are mostly cursor
// gymnastics that don't carry meaning once the line is decoded as
// text.
var otherControl = regexp.MustCompile("[\x00-\x08\x0b-\x1f\x7f]")

// lineMirror is the listener-side mirror filter: ANSI escapes
// stripped, control characters except \n and \t dropped, output
// emitted to the underlying writer one full line at a time. Each
// pumpReader gets its own lineMirror — concurrent shells interleave
// at line granularity, which matches what the old full-passthrough
// mirror did.
//
// Both \n and \r flush a line. \r-terminated lines are treated as
// redraws (e.g. `less`'s status line, a progress bar): consecutive
// identical \r-terminated lines are deduped so 15 redraws of "(END)"
// become one "(END)" line in the log. \n always emits because it's a
// real content boundary.
//
// Trade-off: a prompt that doesn't end with \n or \r won't print
// until something terminates the line. That's the cost of a clean
// per-line listener log; the connector still sees everything in
// real-time on their side.
type lineMirror struct {
	w   io.Writer
	mu  sync.Mutex
	buf bytes.Buffer
	// color is set when the underlying writer is a TTY — we wrap each
	// emitted line in SGR-dim escapes. Piped output skips the
	// escapes so the prefix is the only differentiator (which is the
	// portable signal anyway).
	color bool
	// lastRedraw is the cleaned content of the most recent
	// \r-terminated line. Consecutive identical redraws get suppressed.
	// Reset whenever a \n-terminated line is emitted (real content
	// breaks the redraw run).
	lastRedraw string
}

func newLineMirror(w io.Writer) *lineMirror {
	color := false
	if f, ok := w.(*os.File); ok {
		color = term.IsTerminal(int(f.Fd()))
	}
	return &lineMirror{w: w, color: color}
}

// Write accumulates bytes and flushes on either \n or \r. The
// terminator itself isn't added to the buffer — it's the trigger.
func (m *lineMirror) Write(p []byte) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, b := range p {
		switch b {
		case '\n':
			m.flushLine(false)
		case '\r':
			m.flushLine(true)
		default:
			m.buf.WriteByte(b)
		}
	}
	return len(p), nil
}

// flushLine emits the buffered line (after ANSI/control stripping) to
// the underlying writer. isRedraw=true marks a \r-terminated line, in
// which case the dedup check kicks in. Each emitted line is wrapped
// with mirrorPrefix and (on TTY writers) SGR-dim escapes.
func (m *lineMirror) flushLine(isRedraw bool) {
	if m.buf.Len() == 0 {
		return
	}
	cleaned := ansiEscape.ReplaceAll(m.buf.Bytes(), nil)
	cleaned = otherControl.ReplaceAll(cleaned, nil)
	m.buf.Reset()
	if len(cleaned) == 0 {
		// Pure ANSI / control noise — emit nothing and don't touch
		// the dedup state, so a "real" redraw afterwards still emits.
		return
	}
	if isRedraw {
		if string(cleaned) == m.lastRedraw {
			return
		}
		m.lastRedraw = string(cleaned)
	} else {
		// \n is a content boundary — break the redraw run so the next
		// \r-terminated line emits even if it matches the prior one.
		m.lastRedraw = ""
	}
	if m.color {
		_, _ = io.WriteString(m.w, ansiDim+mirrorPrefix)
		_, _ = m.w.Write(cleaned)
		_, _ = io.WriteString(m.w, ansiReset+"\n")
	} else {
		_, _ = io.WriteString(m.w, mirrorPrefix)
		_, _ = m.w.Write(cleaned)
		_, _ = m.w.Write([]byte{'\n'})
	}
}

// Shell DAT tag bytes — the first byte of every shell DAT frame, telling
// the receiver what to do with the rest of the payload.
const (
	shellTagStdin  byte = 0x00 // client → device: bytes for the process's stdin
	shellTagStdout byte = 0x01 // device → client: bytes from stdout (also stderr in PTY mode)
	shellTagStderr byte = 0x02 // device → client: bytes from stderr (pipe mode only)
	shellTagSignal byte = 0x03 // client → device: signal name (e.g. "INT")
	shellTagResize byte = 0x04 // client → device: [cols:u16][rows:u16] LE
)

// ShellHandler implements StreamHandler for type="shell".
//
// Wire shape (SWSP v3):
//
//	SYN:  {type:"shell", argv:[...], pty:bool, cols?, rows?, env?, cwd?}
//	DAT:  [1 byte tag][payload]
//	      tags: 0=stdin, 1=stdout, 2=stderr, 3=signal, 4=resize
//	FIN:  {exit_code, signal?}  on normal exit
//	      {error:"..."}         on early failure (spawn, etc.)
type ShellHandler struct {
	// DefaultArgv is what gets exec'd when the client doesn't supply an
	// argv (e.g. the listener was started with `bitbang shell --cmd
	// /bin/bash`). Empty means "use $SHELL, or /bin/sh if unset."
	DefaultArgv []string

	// ForcedArgv, if non-nil, locks every connection to this exact
	// argv. Set by `bitbang run` for service-style listeners that
	// expose only one command. Client-supplied argv is ignored.
	ForcedArgv []string

	// MaxConcurrent caps the number of simultaneously-active shell
	// streams across the whole process (not per-session). 0 = no
	// limit. The default for `bitbang shell` is 1 — shell access is
	// strictly more powerful than fileshare/proxy and one trusted
	// user at a time is the sensible posture.
	MaxConcurrent int

	// StdoutMirror / StderrMirror, when non-nil, receive a copy of
	// every byte written to the SWSP stream — i.e. the listener owner
	// gets a live view of what's happening in the shell. In PTY mode
	// the kernel-echoed stdin lands in stdout naturally, so the
	// connector's typing is visible too.
	StdoutMirror io.Writer
	StderrMirror io.Writer

	Verbose bool

	mu      sync.Mutex
	streams map[uint32]*shellSession
}

// shellSession holds the per-stream state: the spawned process plus
// whichever pipe handle(s) we need to ferry stdin to it. In PTY mode
// the same fd is used for both directions, so stdin is nil. In pipe
// mode stdin is the stdin pipe and ptyFile is nil.
type shellSession struct {
	cmd     *exec.Cmd
	ptyFile *os.File       // PTY mode: master side, used for read + write
	stdin   io.WriteCloser // pipe mode: dedicated stdin pipe
}

// NewShell returns a ShellHandler with the given default argv. Pass nil
// or empty to default to $SHELL.
func NewShell(defaultArgv []string, verbose bool) *ShellHandler {
	return &ShellHandler{
		DefaultArgv: defaultArgv,
		Verbose:     verbose,
		streams:     make(map[uint32]*shellSession),
	}
}

// NewShellRestricted returns a ShellHandler that ignores client-supplied
// argv and always exec's the configured one. For `bitbang run`.
func NewShellRestricted(argv []string, verbose bool) *ShellHandler {
	return &ShellHandler{
		ForcedArgv: argv,
		Verbose:    verbose,
		streams:    make(map[uint32]*shellSession),
	}
}

func (h *ShellHandler) Type() string             { return "shell" }
func (h *ShellHandler) OnConnect(_ string) error { return nil }

// KillAll sends SIGHUP to every active shell process this handler is
// tracking. Called by the listener wire-up when the underlying data
// channel closes — without it, processes outlive the connection,
// keep holding their max-sessions slot, and the next connector hits
// "listener is busy."
//
// SIGHUP is the conventional "your terminal went away" signal. bash
// and most shells exit cleanly on it. The actual cleanup (FIN
// emission, slot release, map removal) still flows through
// waitAndFinish — that goroutine unblocks once cmd.Wait sees the
// process exit.
func (h *ShellHandler) KillAll() {
	h.mu.Lock()
	sessions := make([]*shellSession, 0, len(h.streams))
	for _, sess := range h.streams {
		sessions = append(sessions, sess)
	}
	h.mu.Unlock()
	for _, sess := range sessions {
		if sess.cmd != nil && sess.cmd.Process != nil {
			_ = sess.cmd.Process.Signal(syscall.SIGHUP)
		}
	}
}

// shellOpen is the SYN payload for a shell stream. Kept private; the
// JSON shape on the wire is the contract.
type shellOpen struct {
	Type string            `json:"type"`
	Argv []string          `json:"argv,omitempty"`
	PTY  bool              `json:"pty"`
	Cols int               `json:"cols,omitempty"`
	Rows int               `json:"rows,omitempty"`
	Env  map[string]string `json:"env,omitempty"`
	Cwd  string            `json:"cwd,omitempty"`
}

// OnSYN spawns the process and wires it to the SWSP stream.
func (h *ShellHandler) OnSYN(s Stream, payload []byte, final bool) error {
	var open shellOpen
	if err := json.Unmarshal(payload, &open); err != nil {
		h.sendShellError(s, "bad shell request: "+err.Error())
		return nil
	}

	// Max-concurrent gate. Atomic check-then-increment with rollback
	// if we lose the race; on the happy path waitAndFinish decrements.
	// slotTaken tracks whether we own a slot that still needs
	// releasing; the defer below releases it on any early-return path
	// (spawn failed, bad config, …) but we clear slotTaken once
	// waitAndFinish is launched and assumes ownership.
	slotTaken := false
	if h.MaxConcurrent > 0 {
		if int(activeShellCount.Add(1)) > h.MaxConcurrent {
			activeShellCount.Add(-1)
			log.Printf("Shell rejected: at max-sessions=%d", h.MaxConcurrent)
			h.sendShellError(s, fmt.Sprintf("listener is busy (max %d concurrent shell session(s))", h.MaxConcurrent))
			return nil
		}
		slotTaken = true
	}
	defer func() {
		if slotTaken {
			activeShellCount.Add(-1)
		}
	}()

	// Resolve argv: restricted-mode ours, otherwise client's, otherwise
	// default, otherwise $SHELL, otherwise /bin/sh.
	argv := h.ForcedArgv
	if len(argv) == 0 {
		argv = open.Argv
	}
	if len(argv) == 0 {
		argv = h.DefaultArgv
	}
	if len(argv) == 0 {
		sh := os.Getenv("SHELL")
		if sh == "" {
			sh = "/bin/sh"
		}
		argv = []string{sh}
	}

	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = os.Environ()
	for k, v := range open.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	if open.Cwd != "" {
		cmd.Dir = open.Cwd
	}

	// Defaults: PTY off if the client didn't set it (non-interactive is
	// the safer assumption — the client must explicitly request a PTY).
	// 80x24 if size unspecified, matching standard terminal defaults.
	cols, rows := open.Cols, open.Rows
	if cols == 0 {
		cols = 80
	}
	if rows == 0 {
		rows = 24
	}

	sess := &shellSession{cmd: cmd}
	var stdout, stderr io.Reader

	if open.PTY {
		// PTY mode: one fd handles both directions, stdout+stderr
		// merged. The shell sees a real terminal and emits ANSI escapes,
		// reads passwords with echo off, etc.
		f, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)})
		if err != nil {
			h.sendShellError(s, "spawn failed: "+err.Error())
			return nil
		}
		sess.ptyFile = f
		stdout = f
	} else {
		// Pipe mode: separate stdin/stdout/stderr. Use this for
		// scripted, non-interactive command execution (e.g.
		// `bitbang connect URL -- ls /var/log`).
		sin, err := cmd.StdinPipe()
		if err != nil {
			h.sendShellError(s, "stdin pipe: "+err.Error())
			return nil
		}
		sout, err := cmd.StdoutPipe()
		if err != nil {
			_ = sin.Close()
			h.sendShellError(s, "stdout pipe: "+err.Error())
			return nil
		}
		serr, err := cmd.StderrPipe()
		if err != nil {
			_ = sin.Close()
			h.sendShellError(s, "stderr pipe: "+err.Error())
			return nil
		}
		if err := cmd.Start(); err != nil {
			_ = sin.Close()
			h.sendShellError(s, "spawn failed: "+err.Error())
			return nil
		}
		sess.stdin = sin
		stdout = sout
		stderr = serr
	}

	h.mu.Lock()
	h.streams[s.ID()] = sess
	h.mu.Unlock()

	log.Printf("Shell started: argv=%v pty=%v", argv, open.PTY)

	// Spin up the output pumps and the wait/FIN goroutine. Each runs
	// independently; the wait goroutine cleans up shared state and
	// releases the max-concurrent slot. We clear slotTaken here so
	// the local defer doesn't double-release.
	slotTaken = false
	go h.pumpReader(s, stdout, shellTagStdout)
	if stderr != nil {
		go h.pumpReader(s, stderr, shellTagStderr)
	}
	go h.waitAndFinish(s, cmd, argv, h.MaxConcurrent > 0)

	if final {
		// SYN|FIN means the client won't send any stdin. For pipe mode
		// we close the stdin pipe so the process sees EOF; for PTY mode
		// the process gets nothing on the input fd but the master stays
		// open for output.
		h.closeStdin(s.ID())
	}

	return nil
}

// pumpReader copies bytes from r to the stream as DAT(tag, chunk)
// frames until EOF or write error. Each frame is [tag][payload], capped
// at MaxChunkSize total. When a mirror writer is configured for this
// tag, the bytes pass through a lineMirror filter (ANSI stripped,
// emitted one line at a time) before reaching the underlying writer.
// The connector still sees the raw stream over the data channel — the
// filter only affects what the listener owner sees on their console.
func (h *ShellHandler) pumpReader(s Stream, r io.Reader, tag byte) {
	// Leave 1 byte of headroom for the tag prefix.
	buf := make([]byte, protocol.MaxChunkSize-1)
	const maxBuffered uint64 = 8 << 20
	var mirror io.Writer
	switch tag {
	case shellTagStdout:
		if h.StdoutMirror != nil {
			mirror = newLineMirror(h.StdoutMirror)
		}
	case shellTagStderr:
		if h.StderrMirror != nil {
			mirror = newLineMirror(h.StderrMirror)
		}
	}
	for {
		n, err := r.Read(buf)
		if n > 0 {
			if mirror != nil {
				_, _ = mirror.Write(buf[:n])
			}
			for s.BufferedAmount() > maxBuffered {
				time.Sleep(1 * time.Millisecond)
			}
			frame := make([]byte, 1+n)
			frame[0] = tag
			copy(frame[1:], buf[:n])
			if writeErr := s.WriteDAT(frame); writeErr != nil {
				// DC closed mid-stream. The wait goroutine will reap
				// the process; we just stop pumping.
				return
			}
		}
		if err != nil {
			return
		}
	}
}

// OnDAT processes inbound DAT frames. The first byte selects the
// action: stdin, signal, or resize.
func (h *ShellHandler) OnDAT(s Stream, payload []byte) error {
	if len(payload) < 1 {
		return nil
	}
	tag := payload[0]
	body := payload[1:]

	h.mu.Lock()
	sess := h.streams[s.ID()]
	h.mu.Unlock()
	if sess == nil {
		return nil
	}

	switch tag {
	case shellTagStdin:
		if sess.ptyFile != nil {
			_, _ = sess.ptyFile.Write(body)
		} else if sess.stdin != nil {
			_, _ = sess.stdin.Write(body)
		}
	case shellTagSignal:
		// In PTY mode, Ctrl-C usually arrives as byte 0x03 in stdin and
		// the kernel converts it to SIGINT — this explicit path is
		// mostly for non-PTY clients and for signals that don't map to
		// a control character (SIGHUP, SIGUSR1, etc.).
		if sig := signalFromName(string(body)); sig != nil && sess.cmd.Process != nil {
			_ = sess.cmd.Process.Signal(sig)
		}
	case shellTagResize:
		if len(body) < 4 {
			return nil
		}
		cols := binary.LittleEndian.Uint16(body[0:2])
		rows := binary.LittleEndian.Uint16(body[2:4])
		if sess.ptyFile != nil {
			_ = pty.Setsize(sess.ptyFile, &pty.Winsize{Cols: cols, Rows: rows})
		}
	}
	return nil
}

// OnFIN closes the process's stdin (signaling EOF for non-interactive
// commands like `cat` to finish). The process exit is tracked
// separately by waitAndFinish.
func (h *ShellHandler) OnFIN(s Stream, _ []byte) error {
	h.closeStdin(s.ID())
	return nil
}

func (h *ShellHandler) closeStdin(streamID uint32) {
	h.mu.Lock()
	sess := h.streams[streamID]
	h.mu.Unlock()
	if sess == nil {
		return
	}
	if sess.stdin != nil {
		_ = sess.stdin.Close()
	}
	// PTY mode: we deliberately don't close ptyFile here — that would
	// also stop the output flow. The process sees EOF on stdin when
	// we eventually close the PTY in waitAndFinish (after it exits).
}

// waitAndFinish blocks on cmd.Wait(), then emits the FIN trailer with
// the exit code and any terminating signal. Also cleans up per-stream
// state (PTY fd, map entry) and — if releaseSlot is true — releases
// the max-concurrent slot that OnSYN reserved.
func (h *ShellHandler) waitAndFinish(s Stream, cmd *exec.Cmd, argv []string, releaseSlot bool) {
	err := cmd.Wait()

	h.mu.Lock()
	sess := h.streams[s.ID()]
	delete(h.streams, s.ID())
	h.mu.Unlock()
	if sess != nil {
		if sess.ptyFile != nil {
			_ = sess.ptyFile.Close()
		}
	}
	if releaseSlot {
		activeShellCount.Add(-1)
	}

	exitCode := 0
	var signalName string
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			if ws, ok := exitErr.Sys().(syscall.WaitStatus); ok {
				if ws.Signaled() {
					signalName = ws.Signal().String()
				}
				exitCode = ws.ExitStatus()
			} else {
				exitCode = -1
			}
		} else {
			// Couldn't even Wait — process state is unknown.
			exitCode = -1
		}
	}

	if signalName != "" {
		log.Printf("Shell exited: argv=%v code=%d signal=%s", argv, exitCode, signalName)
	} else {
		log.Printf("Shell exited: argv=%v code=%d", argv, exitCode)
	}

	finPayload := map[string]interface{}{"exit_code": exitCode}
	if signalName != "" {
		finPayload["signal"] = signalName
	}
	data, _ := json.Marshal(finPayload)
	_ = s.WriteFIN(data)
}

// sendShellError emits a single SYN+FIN with an {error:"..."} payload.
// Used for failures that happen before the process is up (bad JSON,
// spawn error, etc.).
func (h *ShellHandler) sendShellError(s Stream, msg string) {
	hdr, _ := json.Marshal(map[string]string{"error": msg})
	_ = s.WriteSYN(hdr)
	_ = s.WriteFIN(nil)
}

// signalFromName maps the small set of signal names the client may
// send. Names are uppercase, with or without the "SIG" prefix.
// Returns nil for unknown signals (silently dropped).
func signalFromName(name string) os.Signal {
	switch name {
	case "INT", "SIGINT":
		return syscall.SIGINT
	case "TERM", "SIGTERM":
		return syscall.SIGTERM
	case "QUIT", "SIGQUIT":
		return syscall.SIGQUIT
	case "HUP", "SIGHUP":
		return syscall.SIGHUP
	case "USR1", "SIGUSR1":
		return syscall.SIGUSR1
	case "USR2", "SIGUSR2":
		return syscall.SIGUSR2
	case "KILL", "SIGKILL":
		return syscall.SIGKILL
	}
	return nil
}
