package streamtype

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"io"
	"log"
	"os"
	"os/exec"
	"regexp"
	"sync"
	"syscall"
	"time"

	ptylib "github.com/aymanbagabas/go-pty"
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

// liveShells records the in-flight shell streams across all sessions, in
// the order they were admitted. Package scope because each WebRTC peer
// creates its own ShellHandler instance and the MaxConcurrent limit has
// to apply across all of them.
var liveShells shellAdmissions

// shellAdmissions enforces MaxConcurrent by displacing the oldest shell
// rather than turning the newcomer away.
//
// Refusing was the original behavior and it strands the person holding
// the credential: with the default of one session, a shell left open on
// a machine they walked away from locks them out of their own listener,
// and nothing short of finding that machine gets them back in. The limit
// is a posture -- how many shells may be live at once -- not a
// first-come claim on the listener, and displacing the oldest keeps the
// posture while letting the newcomer in. `bitbang share` resolves the
// same contention the same way.
type shellAdmissions struct {
	mu   sync.Mutex
	live []*shellAdmission
}

// shellAdmission is one live shell and how to end it early.
type shellAdmission struct {
	// evict ends this shell, telling its connector why first. Assigned
	// after the process spawns, so it is read under the list's lock.
	evict func()

	// protected marks a shell held on the device owner's own credential.
	// Nobody else may displace it: a link handed to someone else must not
	// be able to end the operator's session on their own machine, which
	// with the default limit of one would be every time they connected.
	// The owner can still displace their own.
	protected bool
}

// admit registers a shell and reports which one it displaced, if any.
// The caller evicts that one outside the lock, because ending a shell
// waits on a process and this lock is on every admission path.
//
// The displaced shell gives up its slot at the moment it is displaced,
// not when its process finally exits -- otherwise a third arrival would
// throw out the newcomer merely because the first one was slow to die.
// Its own release later finds nothing and is a no-op. Two processes do
// overlap briefly while the old one is terminating; blocking a new
// admission until it exits would be worse, since OnSYN runs on the
// session's dispatch path.
//
// A protected caller (the owner) displaces the oldest shell whatever it
// is, including another of their own. An unprotected caller displaces
// only the oldest unprotected one, and is refused -- adm nil -- when
// every live shell is the owner's.
func (a *shellAdmissions) admit(max int, protected bool) (adm *shellAdmission, displaced *shellAdmission) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if max > 0 && len(a.live) >= max {
		i := a.oldestDisplaceable(protected)
		if i < 0 {
			// Every slot is the owner's and this caller is not them.
			return nil, nil
		}
		displaced = a.live[i]
		a.live = append(a.live[:i], a.live[i+1:]...)
	}
	adm = &shellAdmission{protected: protected}
	a.live = append(a.live, adm)
	return adm, displaced
}

// oldestDisplaceable returns the index of the shell to give up, or -1
// when there is none this caller may take. The list is in admission
// order, so the first match is the oldest.
func (a *shellAdmissions) oldestDisplaceable(protected bool) int {
	for i, live := range a.live {
		if protected || !live.protected {
			return i
		}
	}
	return -1
}

// release drops an admission when its stream ends. Safe to call for one
// already displaced.
func (a *shellAdmissions) release(adm *shellAdmission) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for i, live := range a.live {
		if live == adm {
			a.live = append(a.live[:i], a.live[i+1:]...)
			return
		}
	}
}

// count is the number of live shells, for tests.
func (a *shellAdmissions) count() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.live)
}

// setEvict attaches the teardown once the process exists.
func (a *shellAdmissions) setEvict(adm *shellAdmission, evict func()) {
	a.mu.Lock()
	adm.evict = evict
	a.mu.Unlock()
}

func (a *shellAdmissions) evictFunc(adm *shellAdmission) func() {
	a.mu.Lock()
	defer a.mu.Unlock()
	return adm.evict
}

const (
	maxShellBuffered        uint64 = 8 << 20
	shellOutputDrainTimeout        = 5 * time.Second
	shellOutputCloseGrace          = time.Second
)

// shellOutput owns the output-pump lifetime. Cancellation is separate from
// the process lifetime because a pump can be stuck applying data-channel
// backpressure after the process and its PTY have already exited.
type shellOutput struct {
	sync.WaitGroup
	stop     chan struct{}
	stopOnce sync.Once
}

func newShellOutput() *shellOutput {
	return &shellOutput{stop: make(chan struct{})}
}

func (o *shellOutput) cancel() {
	if o != nil {
		o.stopOnce.Do(func() { close(o.stop) })
	}
}

func (o *shellOutput) cancelled() <-chan struct{} {
	if o == nil {
		return nil
	}
	return o.stop
}

func (o *shellOutput) wait(timeout time.Duration) bool {
	if o == nil {
		return true
	}
	done := make(chan struct{})
	go func() {
		o.Wait()
		close(done)
	}()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-done:
		return true
	case <-timer.C:
		return false
	}
}

func finishOutput(output *shellOutput, timeout time.Duration) bool {
	if output.wait(timeout) {
		return true
	}
	output.cancel()
	_ = output.wait(shellOutputCloseGrace)
	return false
}

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

	// ForcedArgv, when non-empty, locks every connection to this exact
	// command. It also ignores client-supplied argv, environment, and cwd.
	ForcedArgv []string

	// ForcedEnv is the environment used verbatim when ForcedArgv is
	// set. Nil means "inherit the listener's own environment". Ignored
	// when ForcedArgv is empty.
	ForcedEnv []string

	// OwnerCredential marks connections authorized by the device's own
	// code rather than by a link handed to someone else. It only affects
	// displacement: an owner shell cannot be displaced by anyone else,
	// while the owner may still displace their own. Without it, handing
	// out a shell link would let the recipient end the operator's
	// session -- with the default limit of one, on every connection.
	OwnerCredential bool

	// ViewOnly drops stdin, signals, and stdin EOF at the transport layer.
	// Resize stays enabled so each viewer's own PTY matches their terminal;
	// that is per-peer and only becomes a shared-state question when peers
	// attach to one terminal (bitbang share). There the cross-peer guarantee
	// is tmux's, not ours: viewers attach with `tmux attach -r`, which since
	// tmux 3.2 is an alias for read-only,ignore-size, and ignore-size means a
	// client cannot affect the size of other clients. The >= 3.2 floor is
	// enforced by share.CheckVersion (wired at cmd/bitbang/share.go), so -r
	// always carries ignore-size. Pinned by TestViewerAttachesReadOnly and
	// TestViewerCannotResizeWhileControlAttached.
	ViewOnly bool

	// AcquireSlot, if non-nil, replaces the process-wide MaxConcurrent
	// gate with a caller-owned admission policy (bitbang share: one
	// controller slot, N viewer slots). Return a non-nil release func
	// to admit the stream. It is called exactly once when the stream
	// finishes. Return nil plus a client-facing message to refuse it.
	AcquireSlot func() (release func(), errMsg string)

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

	mu                 sync.Mutex
	streams            map[uint32]*shellSession
	closed             bool
	outputDrainTimeout time.Duration
}

// shellSession holds the per-stream state: the spawned process plus
// whichever pipe handle(s) we need to ferry stdin to it. In PTY mode
// the same fd is used for both directions, so stdin is nil. In pipe
// mode stdin is the stdin pipe and ptyFile is nil.
type shellSession struct {
	ioMu    sync.Mutex
	cmd     *exec.Cmd      // pipe mode command
	ptyCmd  *ptylib.Cmd    // PTY/ConPTY mode command
	process *os.Process    // process behind either command type
	ptyFile ptylib.Pty     // PTY mode: master side, used for read + write
	stdin   io.WriteCloser // pipe mode: dedicated stdin pipe
	// pipes are the read ends we own in pipe mode. Closed once output is
	// drained, which also unblocks a pump still waiting on a descriptor
	// some grandchild inherited and kept open.
	pipes  []*os.File
	output *shellOutput // drain output before FIN and terminal close
	done   chan struct{}
}

func (s *shellSession) wait() error {
	if s.ptyCmd != nil {
		return s.ptyCmd.Wait()
	}
	return s.cmd.Wait()
}

func (s *shellSession) writeInput(body []byte) {
	s.ioMu.Lock()
	defer s.ioMu.Unlock()
	if s.ptyFile != nil {
		_, _ = s.ptyFile.Write(body)
	} else if s.stdin != nil {
		_, _ = s.stdin.Write(body)
	}
}

func (s *shellSession) resize(cols, rows int) {
	s.ioMu.Lock()
	defer s.ioMu.Unlock()
	if s.ptyFile != nil {
		_ = s.ptyFile.Resize(cols, rows)
	}
}

func (s *shellSession) closeInput() {
	s.ioMu.Lock()
	defer s.ioMu.Unlock()
	if s.stdin != nil {
		_ = s.stdin.Close()
		s.stdin = nil
	}
}

func (s *shellSession) takePTY() ptylib.Pty {
	s.ioMu.Lock()
	defer s.ioMu.Unlock()
	terminal := s.ptyFile
	s.ptyFile = nil
	return terminal
}

// NewShell returns a ShellHandler with the given default argv. Pass nil
// or empty to default to $SHELL.
func NewShell(defaultArgv []string, verbose bool) *ShellHandler {
	return &ShellHandler{
		DefaultArgv:        defaultArgv,
		Verbose:            verbose,
		streams:            make(map[uint32]*shellSession),
		outputDrainTimeout: shellOutputDrainTimeout,
	}
}

func (h *ShellHandler) Type() string             { return "shell" }
func (h *ShellHandler) OnConnect(_ string) error { return nil }

// Close prevents future spawns and terminates every active process. It first
// requests platform-appropriate termination, then force-kills after a bounded
// grace period. The lock covers process start and registration.
func (h *ShellHandler) Close() {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return
	}
	h.closed = true
	sessions := make([]*shellSession, 0, len(h.streams))
	for _, sess := range h.streams {
		sessions = append(sessions, sess)
	}
	h.mu.Unlock()
	for _, sess := range sessions {
		if sess.process != nil {
			_ = terminateShellProcess(sess.process)
		}
	}
	if len(sessions) == 0 {
		return
	}

	grace := time.NewTimer(2 * time.Second)
	defer grace.Stop()
	for i, sess := range sessions {
		select {
		case <-sess.done:
		case <-grace.C:
			// A process may ignore SIGHUP. Do not let it retain a shell or
			// admission slot after its connection is gone.
			for _, remaining := range sessions[i:] {
				if remaining.process != nil {
					_ = remaining.process.Kill()
				}
			}
			killWait := time.NewTimer(time.Second)
			defer killWait.Stop()
			for _, remaining := range sessions[i:] {
				select {
				case <-remaining.done:
				case <-killWait.C:
					return
				}
			}
			return
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

	// Admission gate. AcquireSlot (caller-owned policy) wins when set;
	// otherwise the process-wide MaxConcurrent list. Either way,
	// `release` owns the slot: the defer below releases it on any
	// early-return path (spawn failed or bad config) and is cleared
	// once waitAndFinish is launched and assumes ownership.
	var release func()
	var admission *shellAdmission
	if h.AcquireSlot != nil {
		rel, errMsg := h.AcquireSlot()
		if rel == nil {
			log.Printf("Shell rejected: %s", errMsg)
			h.sendShellError(s, errMsg)
			return nil
		}
		release = rel
	} else if h.MaxConcurrent > 0 {
		adm, displaced := liveShells.admit(h.MaxConcurrent, h.OwnerCredential)
		if adm == nil {
			// Every slot is held on the owner's own credential and this
			// connection is not. Refusing is the point: a link handed to
			// someone else must not end the operator's session.
			log.Printf("Shell refused: max-sessions=%d, all held by the device owner", h.MaxConcurrent)
			h.sendShellError(s, "the device owner is using the shell; try again later")
			return nil
		}
		if displaced != nil {
			log.Printf("Shell displaced an older session: at max-sessions=%d", h.MaxConcurrent)
			if evict := liveShells.evictFunc(displaced); evict != nil {
				go evict()
			}
		}
		admission = adm
		release = func() { liveShells.release(adm) }
	}
	deferredRelease := release
	defer func() {
		if deferredRelease != nil {
			deferredRelease()
		}
	}()

	// Resolve argv: restricted-mode ours, otherwise client's, otherwise
	// default, otherwise $SHELL, otherwise /bin/sh.
	restricted := len(h.ForcedArgv) > 0
	argv := h.ForcedArgv
	if len(argv) == 0 {
		argv = open.Argv
	}
	if len(argv) == 0 {
		argv = h.DefaultArgv
	}
	if len(argv) == 0 {
		argv = defaultShellArgv()
	}

	cmd := exec.Command(argv[0], argv[1:]...)
	if restricted {
		// Forced argv forces the whole spawn: the client's env and cwd
		// are ignored, or a connector could still steer the pinned
		// command via PATH, loader variables, or its working directory.
		if h.ForcedEnv != nil {
			cmd.Env = h.ForcedEnv
		} else {
			cmd.Env = os.Environ()
		}
	} else {
		cmd.Env = os.Environ()
		for k, v := range open.Env {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
		if open.Cwd != "" {
			cmd.Dir = open.Cwd
		}
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

	sess := &shellSession{output: newShellOutput(), done: make(chan struct{})}
	var stdout, stderr io.Reader

	// Starting and publishing a process is one operation with respect to
	// Close. If Close wins this lock, no command is executed; if OnSYN wins,
	// the process is registered before Close can take its snapshot.
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		h.sendShellError(s, "connection is closed")
		return nil
	}
	if h.streams[s.ID()] != nil {
		h.mu.Unlock()
		h.sendShellError(s, "a shell is already open on this stream")
		return nil
	}

	ptyMode := open.PTY && platformSupportsPTY
	if ptyMode {
		// PTY mode: one fd handles both directions, stdout+stderr
		// merged. The shell sees a real terminal and emits ANSI escapes,
		// reads passwords with echo off, etc.
		terminal, err := ptylib.New()
		if err != nil {
			h.mu.Unlock()
			h.sendShellError(s, "pty failed: "+err.Error())
			return nil
		}
		if err := terminal.Resize(cols, rows); err != nil {
			_ = terminal.Close()
			h.mu.Unlock()
			h.sendShellError(s, "pty resize failed: "+err.Error())
			return nil
		}
		ptyCmd := terminal.Command(argv[0], argv[1:]...)
		ptyCmd.Env = cmd.Env
		ptyCmd.Dir = cmd.Dir
		if err := ptyCmd.Start(); err != nil {
			_ = terminal.Close()
			h.mu.Unlock()
			h.sendShellError(s, "spawn failed: "+err.Error())
			return nil
		}
		sess.ptyCmd = ptyCmd
		sess.process = ptyCmd.Process
		sess.ptyFile = terminal
		stdout = terminal
	} else {
		// Pipe mode: separate stdin/stdout/stderr. Use this for
		// scripted, non-interactive command execution (e.g.
		// `bitbang connect URL -- ls /var/log`).
		sin, err := cmd.StdinPipe()
		if err != nil {
			h.mu.Unlock()
			h.sendShellError(s, "stdin pipe: "+err.Error())
			return nil
		}
		// os.Pipe rather than cmd.StdoutPipe: Wait closes the pipes
		// StdoutPipe hands out, and it runs concurrently with the pumps
		// reading them, so a command that exits before its pump is
		// scheduled loses its output outright. Owning both ends means
		// Wait cannot take the read end away mid-read, and EOF still
		// arrives on its own once every writer is gone -- the child's
		// copy when it exits, ours immediately after Start.
		outR, outW, err := os.Pipe()
		if err != nil {
			_ = sin.Close()
			h.mu.Unlock()
			h.sendShellError(s, "stdout pipe: "+err.Error())
			return nil
		}
		errR, errW, err := os.Pipe()
		if err != nil {
			_ = sin.Close()
			_ = outR.Close()
			_ = outW.Close()
			h.mu.Unlock()
			h.sendShellError(s, "stderr pipe: "+err.Error())
			return nil
		}
		cmd.Stdout = outW
		cmd.Stderr = errW
		if err := cmd.Start(); err != nil {
			_ = sin.Close()
			_ = outR.Close()
			_ = outW.Close()
			_ = errR.Close()
			_ = errW.Close()
			h.mu.Unlock()
			h.sendShellError(s, "spawn failed: "+err.Error())
			return nil
		}
		// The child holds its own copies now. Dropping ours is what
		// lets the reader see EOF when the child goes.
		_ = outW.Close()
		_ = errW.Close()
		sess.cmd = cmd
		sess.process = cmd.Process
		sess.stdin = sin
		sess.pipes = []*os.File{outR, errR}
		stdout = outR
		stderr = errR
	}

	h.streams[s.ID()] = sess
	h.mu.Unlock()

	// The admission can now end this shell if a later one displaces it.
	// Tell the connector before killing the process, or all they see is
	// their shell dying for no stated reason.
	if admission != nil {
		liveShells.setEvict(admission, func() {
			h.sendShellError(s, "another connection took the shell")
			if sess.process != nil {
				_ = terminateShellProcess(sess.process)
			}
		})
	}

	log.Printf("Shell started: argv=%v pty=%v", argv, ptyMode)

	// Spin up the output pumps and the wait/FIN goroutine. Each runs
	// independently; the wait goroutine cleans up shared state and
	// releases the admission slot. We clear deferredRelease here so
	// the local defer doesn't double-release.
	deferredRelease = nil
	sess.output.Add(1)
	go func() {
		defer sess.output.Done()
		h.pumpReader(s, stdout, shellTagStdout, sess.output.cancelled())
	}()
	if stderr != nil {
		sess.output.Add(1)
		go func() {
			defer sess.output.Done()
			h.pumpReader(s, stderr, shellTagStderr, sess.output.cancelled())
		}()
	}
	go h.waitAndFinish(s, sess, argv, release)

	if final && !h.ViewOnly {
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
func (h *ShellHandler) pumpReader(s Stream, r io.Reader, tag byte, cancelled <-chan struct{}) {
	// Leave 1 byte of headroom for the tag prefix.
	buf := make([]byte, protocol.MaxChunkSize-1)
	backpressureTick := time.NewTicker(time.Millisecond)
	defer backpressureTick.Stop()
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
			for s.BufferedAmount() > maxShellBuffered {
				select {
				case <-cancelled:
					return
				case <-backpressureTick.C:
				}
			}
			select {
			case <-cancelled:
				return
			default:
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
		if h.ViewOnly {
			return nil
		}
		sess.writeInput(body)
	case shellTagSignal:
		// In PTY mode, Ctrl-C usually arrives as byte 0x03 in stdin and
		// the kernel converts it to SIGINT — this explicit path is
		// mostly for non-PTY clients and for signals that don't map to
		// a control character (SIGHUP, SIGUSR1, etc.).
		if h.ViewOnly {
			return nil
		}
		if sig := signalFromName(string(body)); sig != nil && sess.process != nil {
			_ = sess.process.Signal(sig)
		}
	case shellTagResize:
		if len(body) < 4 {
			return nil
		}
		cols := binary.LittleEndian.Uint16(body[0:2])
		rows := binary.LittleEndian.Uint16(body[2:4])
		sess.resize(int(cols), int(rows))
	}
	return nil
}

// OnFIN closes the process's stdin (signaling EOF for non-interactive
// commands like `cat` to finish). The process exit is tracked
// separately by waitAndFinish. View-only peers get no stdin-EOF side
// effect either; their FIN is just "stopped watching".
func (h *ShellHandler) OnFIN(s Stream, _ []byte) error {
	if h.ViewOnly {
		return nil
	}
	h.closeStdin(s.ID())
	return nil
}

func (h *ShellHandler) OnReset(s Stream, _, _ string) {
	h.mu.Lock()
	sess := h.streams[s.ID()]
	h.mu.Unlock()
	if sess == nil {
		return
	}
	sess.output.cancel()
	if sess.process != nil {
		_ = terminateShellProcess(sess.process)
	}
	sess.closeInput()
}

func (h *ShellHandler) closeStdin(streamID uint32) {
	h.mu.Lock()
	sess := h.streams[streamID]
	h.mu.Unlock()
	if sess == nil {
		return
	}
	sess.closeInput()
	// PTY mode: we deliberately don't close ptyFile here — that would
	// also stop the output flow. The process sees EOF on stdin when
	// we eventually close the PTY in waitAndFinish (after it exits).
}

// detachSession stops new DAT dispatch before waiting for any in-flight PTY
// operation to finish. The returned PTY is exclusively owned by the shutdown
// path and can be closed without racing input or resize handlers.
func (h *ShellHandler) detachSession(streamID uint32, running *shellSession) ptylib.Pty {
	h.mu.Lock()
	delete(h.streams, streamID)
	h.mu.Unlock()
	return running.takePTY()
}

// waitAndFinish blocks on cmd.Wait(), then emits the FIN trailer with
// the exit code and any terminating signal. Also cleans up per-stream
// state and releases the admission slot reserved by OnSYN.
func (h *ShellHandler) waitAndFinish(s Stream, running *shellSession, argv []string, release func()) {
	err := running.wait()
	terminal := h.detachSession(s.ID(), running)
	if release != nil {
		release()
	}
	close(running.done)

	// Process exit and output EOF are separate events. Platform-specific PTY
	// shutdown drains the final bytes before FIN closes the browser stream.
	var drained bool
	if terminal != nil {
		drained = finishPTY(terminal, running.output, h.outputDrainTimeout)
	} else {
		drained = finishOutput(running.output, h.outputDrainTimeout)
	}
	if !drained {
		log.Printf("Shell output drain timed out: argv=%v", argv)
	}
	// Now that the pumps are done with them -- or have been given up on
	// -- release the read ends. A pump still blocked on one is unblocked
	// by this, which is what Wait used to do before we owned them.
	for _, f := range running.pipes {
		_ = f.Close()
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
