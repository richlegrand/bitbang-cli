package streamtype

import (
	"encoding/binary"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	ptylib "github.com/aymanbagabas/go-pty"
	"github.com/creack/pty"
)

// shellCapture is a Stream that records everything a handler writes,
// so tests can assert on the process's output without a data channel.
type shellCapture struct {
	id  uint32
	mu  sync.Mutex
	dat [][]byte
	syn [][]byte
	fin [][]byte
	// finished closes when the handler emits FIN (process exited).
	finished chan struct{}
	once     sync.Once
}

func newShellCapture() *shellCapture {
	return &shellCapture{id: 1, finished: make(chan struct{})}
}

func (c *shellCapture) ID() uint32          { return c.id }
func (c *shellCapture) ConnectPath() string { return "/" }
func (c *shellCapture) WriteSYN(p []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.syn = append(c.syn, append([]byte(nil), p...))
	return nil
}
func (c *shellCapture) WriteDAT(p []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.dat = append(c.dat, append([]byte(nil), p...))
	return nil
}
func (c *shellCapture) WriteFIN(p []byte) error {
	c.mu.Lock()
	c.fin = append(c.fin, append([]byte(nil), p...))
	c.mu.Unlock()
	c.once.Do(func() { close(c.finished) })
	return nil
}
func (c *shellCapture) SendRaw(_ uint16, p []byte) error { return c.WriteDAT(p) }
func (c *shellCapture) BufferedAmount() uint64           { return 0 }

// stdout returns everything the handler pumped with the stdout tag.
func (c *shellCapture) stdout() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var b strings.Builder
	for _, f := range c.dat {
		if len(f) > 0 && f[0] == shellTagStdout {
			b.Write(f[1:])
		}
	}
	return b.String()
}

func (c *shellCapture) firstSYN() map[string]string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.syn) == 0 {
		return nil
	}
	var m map[string]string
	_ = json.Unmarshal(c.syn[0], &m)
	return m
}

func (c *shellCapture) waitFinished(t *testing.T) {
	t.Helper()
	select {
	case <-c.finished:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for the process to exit")
	}
}

func skipIfWindows(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell semantics required")
	}
}

// TestForcedArgvIgnoresClientArgv is the core of the `bitbang share`
// boundary: a peer pinned to a command must not be able to run
// anything else, no matter what it asks for.
func TestForcedArgvIgnoresClientArgv(t *testing.T) {
	skipIfWindows(t)
	h := NewShell(nil, false)
	h.ForcedArgv = []string{"/bin/echo", "forced"}

	s := newShellCapture()
	syn, _ := json.Marshal(shellOpen{
		Type: "shell",
		Argv: []string{"/bin/echo", "ATTACKER"},
	})
	if err := h.OnSYN(s, syn, false); err != nil {
		t.Fatalf("OnSYN: %v", err)
	}
	s.waitFinished(t)

	out := s.stdout()
	if !strings.Contains(out, "forced") {
		t.Errorf("output %q does not contain the forced command's output", out)
	}
	if strings.Contains(out, "ATTACKER") {
		t.Errorf("client-supplied argv was executed: %q", out)
	}
}

// TestForcedArgvIgnoresClientEnvAndCwd covers the subtler half: even
// with argv pinned, honoring client env or cwd would let a connector
// steer the pinned command (PATH, loader variables, working dir).
func TestForcedArgvIgnoresClientEnvAndCwd(t *testing.T) {
	skipIfWindows(t)
	tmp := t.TempDir()

	h := NewShell(nil, false)
	h.ForcedArgv = []string{"/bin/sh", "-c", "echo INJECTED=${INJECTED:-none}; pwd"}
	h.ForcedEnv = []string{"PATH=/usr/bin:/bin", "HOME=" + tmp}

	s := newShellCapture()
	syn, _ := json.Marshal(shellOpen{
		Type: "shell",
		Env:  map[string]string{"INJECTED": "yes", "LD_PRELOAD": "/tmp/evil.so"},
		Cwd:  tmp,
	})
	if err := h.OnSYN(s, syn, false); err != nil {
		t.Fatalf("OnSYN: %v", err)
	}
	s.waitFinished(t)

	out := s.stdout()
	if strings.Contains(out, "INJECTED=yes") {
		t.Errorf("client env reached the forced process: %q", out)
	}
	if !strings.Contains(out, "INJECTED=none") {
		t.Errorf("expected the forced environment, got %q", out)
	}
	// cwd should be the listener's, not the client's request. Compare
	// resolved paths: macOS reports /var/... for /private/var/... temps.
	wd, _ := os.Getwd()
	gotDir := strings.TrimSpace(lastLine(out))
	resolvedTmp, _ := filepath.EvalSymlinks(tmp)
	if gotDir == tmp || gotDir == resolvedTmp {
		t.Errorf("client cwd was honored: %q", gotDir)
	}
	resolvedWD, _ := filepath.EvalSymlinks(wd)
	if gotDir != wd && gotDir != resolvedWD {
		t.Errorf("cwd = %q, want the listener's own %q", gotDir, wd)
	}
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\r\n"), "\n")
	return lines[len(lines)-1]
}

// TestUnrestrictedStillHonorsClientArgv guards the default path: the
// hardening must not change `serve shell` behavior for peers that are
// supposed to choose their own command.
func TestUnrestrictedStillHonorsClientArgv(t *testing.T) {
	skipIfWindows(t)
	h := NewShell([]string{"/bin/echo", "default"}, false)

	s := newShellCapture()
	syn, _ := json.Marshal(shellOpen{Type: "shell", Argv: []string{"/bin/echo", "client"}})
	if err := h.OnSYN(s, syn, false); err != nil {
		t.Fatalf("OnSYN: %v", err)
	}
	s.waitFinished(t)

	if out := s.stdout(); !strings.Contains(out, "client") {
		t.Errorf("client argv was ignored in unrestricted mode: %q", out)
	}
}

// TestViewOnlyDropsInput asserts the transport-level enforcement for
// share viewers: stdin, signals, and the FIN stdin-close side effect
// never reach the process. (tmux attach -r is defense in depth behind
// this, but the listener must not rely on it.)
func TestViewOnlyDropsInput(t *testing.T) {
	skipIfWindows(t)
	h := NewShell(nil, false)
	// Echoes anything it reads; stays alive until stdin closes.
	h.ForcedArgv = []string{"/bin/sh", "-c", "while read -r line; do echo GOT:$line; done; echo EOF"}
	h.ViewOnly = true

	s := newShellCapture()
	syn, _ := json.Marshal(shellOpen{Type: "shell"})
	if err := h.OnSYN(s, syn, false); err != nil {
		t.Fatalf("OnSYN: %v", err)
	}

	// Every input-shaped frame a hostile viewer could send.
	stdin := append([]byte{shellTagStdin}, []byte("hello\n")...)
	if err := h.OnDAT(s, stdin); err != nil {
		t.Fatalf("OnDAT stdin: %v", err)
	}
	sig := append([]byte{shellTagSignal}, []byte("TERM")...)
	if err := h.OnDAT(s, sig); err != nil {
		t.Fatalf("OnDAT signal: %v", err)
	}
	if err := h.OnFIN(s, nil); err != nil {
		t.Fatalf("OnFIN: %v", err)
	}

	time.Sleep(300 * time.Millisecond)
	if out := s.stdout(); strings.Contains(out, "GOT:hello") || strings.Contains(out, "EOF") {
		t.Errorf("view-only peer's input reached the process: %q", out)
	}
	select {
	case <-s.finished:
		t.Error("view-only peer's TERM signal killed the process")
	default:
	}

	h.Close()
	s.waitFinished(t)
}

// TestViewOnlyAllowsResize: resize only sizes the viewer's own PTY, and
// dropping it would desync the remote grid from their viewport.
func TestViewOnlyAllowsResize(t *testing.T) {
	skipIfWindows(t)
	h := NewShell(nil, false)
	h.ForcedArgv = []string{"/bin/sh", "-c", "sleep 5"}
	h.ViewOnly = true

	s := newShellCapture()
	syn, _ := json.Marshal(shellOpen{Type: "shell", PTY: true, Cols: 80, Rows: 24})
	if err := h.OnSYN(s, syn, false); err != nil {
		t.Fatalf("OnSYN: %v", err)
	}

	resize := make([]byte, 5)
	resize[0] = shellTagResize
	binary.LittleEndian.PutUint16(resize[1:3], 100)
	binary.LittleEndian.PutUint16(resize[3:5], 40)
	if err := h.OnDAT(s, resize); err != nil {
		t.Fatalf("OnDAT resize: %v", err)
	}

	h.mu.Lock()
	sess := h.streams[s.ID()]
	h.mu.Unlock()
	if sess == nil || sess.ptyFile == nil {
		t.Fatal("no PTY session was created")
	}
	// pty.Getsize round-trips the ioctl we just performed.
	cols, rows := ptySize(t, sess)
	if cols != 100 || rows != 40 {
		t.Errorf("PTY size = %dx%d, want 100x40 -- viewer resize was dropped", cols, rows)
	}

	h.Close()
	s.waitFinished(t)
}

// TestControlPeerKeepsInput is the counterpart: a controller's input
// must still flow (otherwise the share is useless).
func TestControlPeerKeepsInput(t *testing.T) {
	skipIfWindows(t)
	h := NewShell(nil, false)
	h.ForcedArgv = []string{"/bin/sh", "-c", "while read -r line; do echo GOT:$line; done"}

	s := newShellCapture()
	syn, _ := json.Marshal(shellOpen{Type: "shell"})
	if err := h.OnSYN(s, syn, false); err != nil {
		t.Fatalf("OnSYN: %v", err)
	}
	if err := h.OnDAT(s, append([]byte{shellTagStdin}, []byte("hello\n")...)); err != nil {
		t.Fatalf("OnDAT: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(s.stdout(), "GOT:hello") {
			h.Close()
			s.waitFinished(t)
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	h.Close()
	t.Errorf("controller input never reached the process; output was %q", s.stdout())
}

// TestAcquireSlotGatesAdmission covers the share worker's per-role
// limiters: one controller, N viewers, with the slot freed on exit so
// control hands over after a disconnect.
//
// Each peer gets its own handler sharing one limiter, which is how the
// worker wires it -- a handler belongs to a single connection and is
// closed when that connection dies, so reusing one across peers would
// not model anything that happens.
func TestAcquireSlotGatesAdmission(t *testing.T) {
	skipIfWindows(t)
	var mu sync.Mutex
	used := 0
	acquire := func() (func(), string) {
		mu.Lock()
		defer mu.Unlock()
		if used >= 1 {
			return nil, "share already has a controller"
		}
		used++
		var once sync.Once
		return func() {
			once.Do(func() {
				mu.Lock()
				used--
				mu.Unlock()
			})
		}, ""
	}
	newPeer := func() *ShellHandler {
		h := NewShell(nil, false)
		h.ForcedArgv = []string{"/bin/sh", "-c", "sleep 5"}
		h.AcquireSlot = acquire
		return h
	}
	syn, _ := json.Marshal(shellOpen{Type: "shell"})

	firstH := newPeer()
	first := newShellCapture()
	if err := firstH.OnSYN(first, syn, false); err != nil {
		t.Fatalf("OnSYN first: %v", err)
	}

	secondH := newPeer()
	second := newShellCapture()
	if err := secondH.OnSYN(second, syn, false); err != nil {
		t.Fatalf("OnSYN second: %v", err)
	}
	if errMsg := second.firstSYN()["error"]; !strings.Contains(errMsg, "controller") {
		t.Errorf("second peer got error %q, want the controller-busy refusal", errMsg)
	}

	// Controller disconnects -> the slot frees and the next peer is admitted.
	firstH.Close()
	first.waitFinished(t)

	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		free := used == 0
		mu.Unlock()
		if free {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("controller slot was never released after disconnect")
		}
		time.Sleep(20 * time.Millisecond)
	}

	thirdH := newPeer()
	third := newShellCapture()
	if err := thirdH.OnSYN(third, syn, false); err != nil {
		t.Fatalf("OnSYN third: %v", err)
	}
	if msg := third.firstSYN()["error"]; msg != "" {
		t.Errorf("handover failed: third peer refused with %q", msg)
	}
	thirdH.Close()
	third.waitFinished(t)
}

// TestAcquireSlotOverridesMaxConcurrent: when a caller-owned policy is
// set it fully replaces the process-wide counter, so one share's peers
// can't be throttled by unrelated shell activity in the same process.
func TestAcquireSlotOverridesMaxConcurrent(t *testing.T) {
	skipIfWindows(t)
	h := NewShell(nil, false)
	h.ForcedArgv = []string{"/bin/sh", "-c", "sleep 5"}
	h.MaxConcurrent = 1
	h.AcquireSlot = func() (func(), string) { return func() {}, "" }

	var streams []*shellCapture
	syn, _ := json.Marshal(shellOpen{Type: "shell"})
	for i := 0; i < 3; i++ {
		s := newShellCapture()
		s.id = uint32(1 + 2*i)
		if err := h.OnSYN(s, syn, false); err != nil {
			t.Fatalf("OnSYN %d: %v", i, err)
		}
		if msg := s.firstSYN()["error"]; msg != "" {
			t.Fatalf("peer %d refused (%q) despite the custom policy admitting it", i, msg)
		}
		streams = append(streams, s)
	}
	h.Close()
	for _, s := range streams {
		s.waitFinished(t)
	}
}

func TestDuplicateStreamCannotReplaceRunningShell(t *testing.T) {
	skipIfWindows(t)
	h := NewShell(nil, false)
	h.ForcedArgv = []string{"/bin/sh", "-c", "sleep 5"}
	syn, _ := json.Marshal(shellOpen{Type: "shell"})

	first := newShellCapture()
	if err := h.OnSYN(first, syn, false); err != nil {
		t.Fatal(err)
	}
	duplicate := newShellCapture() // same stream ID as first
	if err := h.OnSYN(duplicate, syn, false); err != nil {
		t.Fatal(err)
	}
	if msg := duplicate.firstSYN()["error"]; !strings.Contains(msg, "already open") {
		t.Fatalf("duplicate stream error = %q", msg)
	}

	h.Close()
	first.waitFinished(t)
}

// ptySize reads back the PTY's window size via the same ioctl path the
// resize handler uses.
func ptySize(t *testing.T, sess *shellSession) (cols, rows int) {
	t.Helper()
	terminal, ok := sess.ptyFile.(ptylib.UnixPty)
	if !ok {
		t.Fatalf("PTY has type %T, want pty.UnixPty", sess.ptyFile)
	}
	ws, err := pty.GetsizeFull(terminal.Master())
	if err != nil {
		t.Fatalf("GetsizeFull: %v", err)
	}
	return int(ws.Cols), int(ws.Rows)
}

func TestClosedHandlerDoesNotExecuteCommand(t *testing.T) {
	skipIfWindows(t)
	marker := filepath.Join(t.TempDir(), "started")
	h := NewShell(nil, false)
	h.ForcedArgv = []string{"/bin/sh", "-c", "touch \"$0\"", marker}

	h.Close()

	s := newShellCapture()
	syn, _ := json.Marshal(shellOpen{Type: "shell", PTY: true})
	if err := h.OnSYN(s, syn, false); err != nil {
		t.Fatalf("OnSYN: %v", err)
	}

	h.mu.Lock()
	tracked := len(h.streams)
	h.mu.Unlock()
	if tracked != 0 {
		t.Errorf("handler tracked %d process(es) after being closed", tracked)
	}

	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("closed handler executed its command; marker stat error = %v", err)
	}
}
