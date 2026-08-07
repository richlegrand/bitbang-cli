//go:build unix

package share

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/richlegrand/bitbang/internal/streamtype"
)

// requireTmux skips local runs without a supported tmux. Linux CI installs
// tmux so these tests run for pull requests.
func requireTmux(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping tmux integration test in short mode")
	}
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	if err := CheckVersion(NewRunner("")); err != nil {
		t.Skipf("tmux too old: %v", err)
	}
}

// isolatedServer starts a private tmux server on a throwaway socket
// with one session, and kills the whole server at test end. Nothing
// touches the developer's own tmux.
func isolatedServer(t *testing.T) (*ExecRunner, string) {
	t.Helper()
	// Short base path on purpose: a Unix socket path is capped near
	// 104 bytes, and t.TempDir()'s test-name-derived directory blows
	// past that on macOS (/var/folders/...).
	base, err := os.MkdirTemp("/tmp", "bbshare")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	socket := filepath.Join(base, "s")
	r := NewRunner(socket)
	if _, err := r.Run("new-session", "-d", "-s", "shared", "-x", "80", "-y", "24", "cat"); err != nil {
		t.Skipf("cannot start isolated tmux server: %v", err)
	}
	t.Cleanup(func() { _, _ = r.Run("kill-server") })
	return r, socket
}

// attachHandler builds the ShellHandler the share worker would build
// for a peer in the given role.
func attachHandler(socket, session string, viewOnly bool) *streamtype.ShellHandler {
	h := streamtype.NewShell(nil, false)
	argv := []string{"tmux", "-S", socket, "attach-session"}
	if viewOnly {
		argv = append(argv, "-r")
	}
	argv = append(argv, "-t", session)
	h.ForcedArgv = argv
	h.ForcedEnv = AttachEnv(os.Environ())
	h.ViewOnly = viewOnly
	return h
}

// TestAttachDeliversSessionOutput is the end-to-end claim of the
// feature: a peer attaching over the forced argv sees the *running*
// session's screen, not a fresh shell.
func TestAttachDeliversSessionOutput(t *testing.T) {
	requireTmux(t)
	r, socket := isolatedServer(t)

	// Put a recognizable marker on the shared session's screen before
	// anyone attaches -- proving the attach shows existing state.
	if _, err := r.Run("send-keys", "-t", "shared", "MARKER-ALREADY-RUNNING", "Enter"); err != nil {
		t.Fatalf("send-keys: %v", err)
	}

	h := attachHandler(socket, "shared", false)
	s := newTmuxCapture()
	syn, _ := json.Marshal(map[string]interface{}{"type": "shell", "pty": true, "cols": 80, "rows": 24})
	if err := h.OnSYN(s, syn, false); err != nil {
		t.Fatalf("OnSYN: %v", err)
	}
	defer h.Close()

	waitFor(t, "attached client to render the running session", func() bool {
		return strings.Contains(s.text(), "MARKER-ALREADY-RUNNING")
	})

	// tmux itself must consider the client attached (and read-write).
	out, err := r.Run("list-clients", "-t", "shared", "-F", "#{client_readonly}")
	if err != nil {
		t.Fatalf("list-clients: %v", err)
	}
	if out == "" {
		t.Fatal("tmux reports no attached clients")
	}
	if strings.Contains(out, "1") {
		t.Errorf("control peer attached read-only (client_readonly=%q)", out)
	}
}

// TestViewerAttachesReadOnly checks the tmux-side half of the viewer
// boundary: even if the transport gate were bypassed, the client tmux
// hands the viewer is read-only and size-ignoring.
func TestViewerAttachesReadOnly(t *testing.T) {
	requireTmux(t)
	r, socket := isolatedServer(t)

	h := attachHandler(socket, "shared", true)
	s := newTmuxCapture()
	syn, _ := json.Marshal(map[string]interface{}{"type": "shell", "pty": true, "cols": 100, "rows": 40})
	if err := h.OnSYN(s, syn, false); err != nil {
		t.Fatalf("OnSYN: %v", err)
	}
	defer h.Close()

	waitFor(t, "viewer client to register with tmux", func() bool {
		out, _ := r.Run("list-clients", "-t", "shared", "-F", "#{client_readonly},#{client_flags}")
		return strings.TrimSpace(out) != ""
	})

	out, err := r.Run("list-clients", "-t", "shared", "-F", "#{client_readonly},#{client_flags}")
	if err != nil {
		t.Fatalf("list-clients: %v", err)
	}
	line := strings.TrimSpace(strings.Split(out, "\n")[0])
	readonly, flags, _ := strings.Cut(line, ",")
	if readonly != "1" {
		t.Errorf("viewer client_readonly = %q, want 1", readonly)
	}
	if !strings.Contains(flags, "ignore-size") {
		t.Errorf("viewer client_flags = %q, want ignore-size among them", flags)
	}
}

// An ignore-size viewer must not resize the window while a read-write client
// is attached. A lone viewer still supplies tmux's only available size.
// Pinned under both window-size modes: latest (tmux's default) and smallest
// (a common config, and the mode that would shrink everyone if a viewer's
// size counted). ignore-size must hold regardless of mode.
func TestViewerCannotResizeWhileControlAttached(t *testing.T) {
	requireTmux(t)
	for _, windowSize := range []string{"latest", "smallest"} {
		t.Run(windowSize, func(t *testing.T) {
			r, socket := isolatedServer(t)

			if _, err := r.Run("set-option", "-g", "window-size", windowSize); err != nil {
				t.Fatalf("set window-size: %v", err)
			}

			control := attachHandler(socket, "shared", false)
			controlStream := newTmuxCapture()
			bigSYN, _ := json.Marshal(map[string]interface{}{"type": "shell", "pty": true, "cols": 80, "rows": 24})
			if err := control.OnSYN(controlStream, bigSYN, false); err != nil {
				t.Fatalf("control OnSYN: %v", err)
			}
			defer control.Close()

			waitFor(t, "control client to attach", func() bool {
				out, _ := r.Run("list-clients", "-t", "shared", "-F", "#{client_readonly}")
				return strings.Contains(out, "0")
			})
			time.Sleep(300 * time.Millisecond)
			before, err := r.Run("display-message", "-p", "-t", "shared", "#{window_width}x#{window_height}")
			if err != nil {
				t.Fatalf("display-message: %v", err)
			}

			viewer := attachHandler(socket, "shared", true)
			viewerStream := newTmuxCapture()
			// Deliberately a phone-shaped geometry, far from the controller's.
			smallSYN, _ := json.Marshal(map[string]interface{}{"type": "shell", "pty": true, "cols": 40, "rows": 12})
			if err := viewer.OnSYN(viewerStream, smallSYN, false); err != nil {
				t.Fatalf("viewer OnSYN: %v", err)
			}
			defer viewer.Close()

			waitFor(t, "viewer client to attach", func() bool {
				out, _ := r.Run("list-clients", "-t", "shared", "-F", "#{client_readonly}")
				return strings.Contains(out, "1")
			})
			time.Sleep(500 * time.Millisecond) // let any (unwanted) resize settle

			after, err := r.Run("display-message", "-p", "-t", "shared", "#{window_width}x#{window_height}")
			if err != nil {
				t.Fatalf("display-message: %v", err)
			}
			if after != before {
				t.Errorf("viewer resized the shared window out from under the controller: %s -> %s", before, after)
			}
		})
	}
}

// TestEffectiveWindowSizeResolvesInheritance pins down what the share
// reports about sizing. window-size is a WINDOW option, so the value
// that matters is the one in force for the shared window after
// inheritance -- not the server-wide default, which any window-local
// setting silently beats.
func TestEffectiveWindowSizeResolvesInheritance(t *testing.T) {
	requireTmux(t)
	r, _ := isolatedServer(t)

	// tmux's own default is what makes a share behave: the window
	// follows whoever is currently driving it.
	if got := EffectiveWindowSize(r, "shared"); got != "latest" {
		t.Errorf("default effective window-size = %q, want latest", got)
	}

	// A window-local override must be what we report, because it is
	// what tmux will actually obey.
	if _, err := r.Run("set-option", "-w", "-t", "shared", "window-size", "manual"); err != nil {
		t.Fatalf("set window-local override: %v", err)
	}
	if _, err := r.Run("set-option", "-g", "window-size", "latest"); err != nil {
		t.Fatalf("set global: %v", err)
	}
	if got := EffectiveWindowSize(r, "shared"); got != "manual" {
		t.Errorf("effective window-size = %q, want manual -- the window-local value wins over the global default", got)
	}
}

// Sharing must not change global or unrelated-session window sizing.
func TestShareLeavesWindowSizeAlone(t *testing.T) {
	requireTmux(t)
	r, socket := isolatedServer(t)

	if _, err := r.Run("new-session", "-d", "-s", "bystander", "-x", "80", "-y", "24", "cat"); err != nil {
		t.Fatalf("new-session: %v", err)
	}
	// An operator who deliberately configured something other than the
	// default must find it unchanged afterwards.
	if _, err := r.Run("set-option", "-g", "window-size", "smallest"); err != nil {
		t.Fatalf("seed global: %v", err)
	}

	h := attachHandler(socket, "shared", false)
	s := newTmuxCapture()
	syn, _ := json.Marshal(map[string]interface{}{"type": "shell", "pty": true, "cols": 80, "rows": 24})
	if err := h.OnSYN(s, syn, false); err != nil {
		t.Fatalf("OnSYN: %v", err)
	}
	waitFor(t, "client to attach", func() bool {
		out, _ := r.Run("list-clients", "-t", "shared", "-F", "#{client_readonly}")
		return strings.TrimSpace(out) != ""
	})
	_ = EffectiveWindowSize(r, "shared") // the probe the share actually performs
	h.Close()
	waitFor(t, "attach client to exit", func() bool {
		out, _ := r.Run("list-clients", "-t", "shared", "-F", "#{client_readonly}")
		return strings.TrimSpace(out) == ""
	})

	if got, _ := r.Run("show-options", "-gqv", "window-size"); got != "smallest" {
		t.Errorf("global window-size = %q after sharing, want the operator's smallest untouched", got)
	}
	if got := EffectiveWindowSize(r, "bystander"); got != "smallest" {
		t.Errorf("unrelated session's window-size = %q, want smallest -- a share must not reach other sessions", got)
	}
}

func TestSourceSessionLossDetected(t *testing.T) {
	requireTmux(t)
	r, socket := isolatedServer(t)

	// Stand-in for the worker's management session. In production it is
	// always there -- the worker runs inside it -- which is what keeps the
	// server alive to answer after the shared session goes away. Without
	// one, killing the only session takes the server with it and there
	// is nobody left to ask.
	if _, err := r.Run("new-session", "-d", "-s", "mgmt", "cat"); err != nil {
		t.Fatalf("new-session: %v", err)
	}
	id, err := r.Run("display-message", "-p", "-t", "=shared:", "#{session_id}")
	if err != nil {
		t.Fatalf("display-message: %v", err)
	}

	if !strings.HasPrefix(id, "$") {
		t.Fatalf("session id = %q; a watchdog given an empty id matches nothing and fires immediately, "+
			"so the rest of this test would pass without proving anything", id)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	gone := watchSource(ctx, r, id, 20*time.Millisecond)

	// It must be quiet while the session is there, or "it fired after
	// the kill" says nothing about the kill.
	select {
	case <-gone:
		t.Fatal("watchdog reported a session that was running")
	case <-time.After(200 * time.Millisecond):
	}

	if _, err := r.Run("kill-session", "-t", "=shared"); err != nil {
		t.Fatalf("kill-session: %v", err)
	}
	select {
	case <-gone:
	case <-time.After(2 * time.Second):
		t.Fatal("worker watchdog did not report the killed source session")
	}
	_ = socket
}

// TestUnreachableServerDoesNotEndTheShare is the other half, and the
// one that used to be wrong: a socket that cannot be reached answers
// every command with a non-zero exit, exactly as a killed session does.
// Counting those toward a verdict ended a healthy share after fifteen
// seconds of a permission problem that then fixed itself.
func TestUnreachableServerDoesNotEndTheShare(t *testing.T) {
	requireTmux(t)
	if os.Getuid() == 0 {
		t.Skip("root reaches sockets whatever their mode")
	}
	r, socket := isolatedServer(t)
	if _, err := r.Run("new-session", "-d", "-s", "mgmt", "cat"); err != nil {
		t.Fatalf("new-session: %v", err)
	}
	id, err := r.Run("display-message", "-p", "-t", "=shared:", "#{session_id}")
	if err != nil {
		t.Fatalf("display-message: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	gone := watchSource(ctx, r, id, 20*time.Millisecond)

	if err := os.Chmod(socket, 0o000); err != nil {
		t.Fatalf("chmod socket: %v", err)
	}
	select {
	case <-gone:
		_ = os.Chmod(socket, 0o600)
		t.Fatal("an unreachable socket ended a share whose session was running the whole time")
	case <-time.After(500 * time.Millisecond):
	}

	// And it still notices once it can see again.
	if err := os.Chmod(socket, 0o600); err != nil {
		t.Fatalf("restore socket: %v", err)
	}
	if _, err := r.Run("kill-session", "-t", "=shared"); err != nil {
		t.Fatalf("kill-session: %v", err)
	}
	select {
	case <-gone:
	case <-time.After(2 * time.Second):
		t.Fatal("watchdog stayed blind after the socket came back")
	}
}

// TestDiscoverAgainstRealTmux exercises the discovery format string
// against a real tmux rather than a canned reply.
func TestDiscoverAgainstRealTmux(t *testing.T) {
	requireTmux(t)
	r, _ := isolatedServer(t)

	t.Setenv("TMUX", "") // force the explicit-target path
	tgt, err := Discover(r, "shared")
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if !strings.HasPrefix(tgt.SessionID, "$") {
		t.Errorf("SessionID = %q, want a $-prefixed tmux id", tgt.SessionID)
	}
	if tgt.SessionName != "shared" {
		t.Errorf("SessionName = %q, want shared", tgt.SessionName)
	}
}
