package streamtype

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/creack/pty"
	"golang.org/x/term"

	"github.com/richlegrand/bitbang/internal/protocol"
)

// activeShellCount is the process-wide count of in-flight shell
// streams across all sessions. Used by MaxConcurrent enforcement on
// ShellHandler. Lives at package scope because each WebRTC peer
// creates its own ShellHandler instance; the limit needs to apply
// across all of them.
var activeShellCount atomic.Int32

// Listener-terminal size tracking, used to print client/listener size
// comparison lines when mirroring is on. listenerCols/Rows are the
// current dimensions of the listener's own terminal (as observed
// through SIGWINCH), or 0 if tracking is disabled (stdout not a TTY,
// or flag off). activeShells is the registry of in-flight shell
// sessions across every ShellHandler instance — the SIGWINCH watcher
// iterates it to print comparison lines for each active stream.
var (
	listenerCols atomic.Int32
	listenerRows atomic.Int32

	activeShellsMu sync.Mutex
	activeShells   = map[*shellSession]struct{}{}
)

// EnableListenerResizeTracking starts a background SIGWINCH watcher
// that records the listener's terminal dimensions and prints a
// client-vs-listener comparison line whenever either side resizes.
// Call once at listener startup, after verifying stdout is a TTY.
// fd is the file descriptor to query for size (typically
// int(os.Stdout.Fd())).
func EnableListenerResizeTracking(fd int) {
	if cols, rows, err := term.GetSize(fd); err == nil {
		setListenerSize(cols, rows)
	}
	sigCh := make(chan os.Signal, 4)
	signal.Notify(sigCh, syscall.SIGWINCH)
	go func() {
		for range sigCh {
			if cols, rows, err := term.GetSize(fd); err == nil {
				setListenerSize(cols, rows)
			}
		}
	}()
}

// setListenerSize atomically updates the recorded listener size. If
// either dimension changed, it triggers a comparison print for every
// currently-active shell session.
func setListenerSize(cols, rows int) {
	oldC := listenerCols.Swap(int32(cols))
	oldR := listenerRows.Swap(int32(rows))
	if int(oldC) == cols && int(oldR) == rows {
		return
	}
	activeShellsMu.Lock()
	sessions := make([]*shellSession, 0, len(activeShells))
	for sess := range activeShells {
		sessions = append(sessions, sess)
	}
	activeShellsMu.Unlock()
	for _, sess := range sessions {
		printShellSizeComparison(sess.clientCols, sess.clientRows)
	}
}

// printShellSizeComparison emits a single one-line log message with
// the listener and client dimensions. No interpretation, no coloring,
// no terminal positioning — just the numbers. No-op when listener
// size isn't being tracked (rows==0 means stdout wasn't a TTY at
// startup or the feature is disabled).
func printShellSizeComparison(clientCols, clientRows int) {
	lc := int(listenerCols.Load())
	lr := int(listenerRows.Load())
	if lr == 0 {
		return
	}
	log.Printf("ours: %dx%d theirs: %dx%d", lc, lr, clientCols, clientRows)
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
//
// clientCols/clientRows track the current dimensions the client
// requested (initial values from the SYN, updated on each
// DAT(tag=resize) frame). Used by the listener-side size-comparison
// printer.
type shellSession struct {
	cmd     *exec.Cmd
	ptyFile *os.File       // PTY mode: master side, used for read + write
	stdin   io.WriteCloser // pipe mode: dedicated stdin pipe

	clientCols int
	clientRows int
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

	sess := &shellSession{cmd: cmd, clientCols: cols, clientRows: rows}
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
	activeShellsMu.Lock()
	activeShells[sess] = struct{}{}
	activeShellsMu.Unlock()

	log.Printf("Shell started: argv=%v pty=%v cols=%d rows=%d", argv, open.PTY, cols, rows)
	printShellSizeComparison(cols, rows)

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
// tag, the bytes are also written there — the listener owner gets a
// live view of the shell session in their terminal.
func (h *ShellHandler) pumpReader(s Stream, r io.Reader, tag byte) {
	// Leave 1 byte of headroom for the tag prefix.
	buf := make([]byte, protocol.MaxChunkSize-1)
	const maxBuffered uint64 = 8 << 20
	var mirror io.Writer
	switch tag {
	case shellTagStdout:
		mirror = h.StdoutMirror
	case shellTagStderr:
		mirror = h.StderrMirror
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
		// Track the new client size + reprint the comparison so the
		// listener owner can see whether their terminal still fits.
		sess.clientCols = int(cols)
		sess.clientRows = int(rows)
		printShellSizeComparison(int(cols), int(rows))
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
		activeShellsMu.Lock()
		delete(activeShells, sess)
		activeShellsMu.Unlock()
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
