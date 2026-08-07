// Package share implements the tmux plumbing and on-disk state behind
// `bitbang share` -- publishing a running tmux session as a URL.
//
// The CLI never speaks the tmux protocol: tmux is driven exclusively
// through its command-line interface (via Runner), used both as the
// thing being shared (role-scoped `attach-session` commands run under
// forced argv) and as the background supervisor (a detached management
// session hosts the share worker, so no PID files or setsid are needed).
package share

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Runner executes tmux commands. An interface so share's control flow
// (discovery, supervision, cleanup) is testable against a fake tmux.
type Runner interface {
	// Run executes tmux with the given arguments (after any -S socket
	// preamble) and returns trimmed stdout. A non-zero exit becomes an
	// error carrying tmux's stderr.
	Run(args ...string) (string, error)
	// Socket reports the -S path this runner was built with, or "" for
	// tmux's default socket. Discover compares it against the caller's
	// enclosing server.
	Socket() string
}

// ExecRunner runs the real tmux binary.
type ExecRunner struct {
	Bin  string // tmux binary; "tmux" resolves via PATH
	Sock string // -S socket path; empty = tmux's default socket

	// Timeout bounds one invocation. Zero waits indefinitely, which is
	// right for the server the operator asked about -- they would rather
	// wait than be told a wrong answer quickly. It is wrong for a
	// server reached incidentally, which is what the share sweep does:
	// a tmux client that connects and never gets a reply hangs, and a
	// hang there would be attributed to whatever command was running.
	Timeout time.Duration
}

// NewRunner returns an ExecRunner bound to the given server socket
// ("" = tmux's default socket for this user).
func NewRunner(socket string) *ExecRunner {
	return &ExecRunner{Bin: "tmux", Sock: socket}
}

// Socket implements Runner.
func (r *ExecRunner) Socket() string { return r.Sock }

func (r *ExecRunner) Run(args ...string) (string, error) {
	full := make([]string, 0, len(args)+2)
	if r.Sock != "" {
		full = append(full, "-S", r.Sock)
	}
	full = append(full, args...)
	ctx := context.Background()
	if r.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, r.Timeout)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, r.Bin, full...)
	// A newly forked tmux server can inherit the command's output pipes.
	// WaitDelay bounds Wait after the context kills the client process.
	cmd.WaitDelay = r.Timeout
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("tmux %s: %s", args[0], msg)
	}
	return strings.TrimSpace(stdout.String()), nil
}

// Target identifies the tmux session being shared.
type Target struct {
	Socket      string // tmux server socket path ("" = default socket)
	SessionID   string // stable tmux session ID, e.g. "$3"
	SessionName string // display name at share time (may be renamed later)
}

// SocketFromEnv returns the socket path of the enclosing tmux server,
// or "" when not running inside tmux. $TMUX is "socket,pid,paneidx".
func SocketFromEnv() string {
	env := os.Getenv("TMUX")
	if env == "" {
		return ""
	}
	if i := strings.IndexByte(env, ','); i > 0 {
		return env[:i]
	}
	return ""
}

// Discover resolves the session to share. targetFlag names a session
// (name or $id); empty means the session enclosing this process, which
// requires running inside tmux (display-message then resolves the pane
// from $TMUX/$TMUX_PANE).
//
// The returned Socket is the server's own answer, not the string we
// reached it by. A share is keyed on that path, and one server has many
// spellings -- `$TMUX`'s prefix, an empty default, a symlink, /tmp
// against /private/tmp -- each of which would otherwise key a separate
// share and let a second worker publish a session that is already
// published.
func Discover(r Runner, targetFlag string) (Target, error) {
	args := []string{"display-message", "-p"}
	if targetFlag != "" {
		args = append(args, "-t", targetFlag)
	} else {
		enclosing := SocketFromEnv()
		if enclosing == "" {
			return Target{}, errors.New("not inside tmux -- pass --target SESSION (and --socket PATH for a non-default server)")
		}
		// display-message resolves against the server we are talking
		// to, which is not the one the caller is sitting in when
		// --socket points elsewhere. Publishing a session the user
		// cannot see, read-write, is not a thing to guess at.
		if sock := r.Socket(); sock != "" && !sameSocket(sock, enclosing) {
			return Target{}, fmt.Errorf("--socket %s is a different tmux server than the one you are in (%s) -- pass --target SESSION to say which session to share", sock, enclosing)
		}
	}
	args = append(args, "#{session_id}\t#{session_name}\t#{socket_path}")
	out, err := r.Run(args...)
	if err != nil {
		return Target{}, err
	}
	fields := strings.Split(out, "\t")
	if len(fields) < 2 || !strings.HasPrefix(fields[0], "$") {
		return Target{}, fmt.Errorf("could not resolve tmux session (got %q)", out)
	}
	t := Target{SessionID: fields[0], SessionName: fields[1]}
	if len(fields) > 2 {
		t.Socket = strings.TrimSpace(fields[2])
	}
	return t, nil
}

// sameSocket reports whether two socket paths name the same tmux
// server. Compared after resolving symlinks, since a path that points
// at the enclosing server is the enclosing server however it is
// spelled; an unresolvable path falls back to a literal match.
func sameSocket(a, b string) bool {
	if a == b {
		return true
	}
	ra, err := filepath.EvalSymlinks(a)
	if err != nil {
		return false
	}
	rb, err := filepath.EvalSymlinks(b)
	if err != nil {
		return false
	}
	return ra == rb
}

var tmuxVersionRe = regexp.MustCompile(`(\d+)\.(\d+)`)

// CheckVersion enforces the tmux >= 3.2 floor required by the read-only
// attach behavior. Unparseable development versions are assumed current.
func CheckVersion(r Runner) error {
	out, err := r.Run("-V")
	if err != nil {
		return err
	}
	m := tmuxVersionRe.FindStringSubmatch(out)
	if m == nil {
		return nil
	}
	major, _ := strconv.Atoi(m[1])
	minor, _ := strconv.Atoi(m[2])
	if major > 3 || (major == 3 && minor >= 2) {
		return nil
	}
	return fmt.Errorf("bitbang share needs tmux >= 3.2 (found %q) for read-only viewers", out)
}

// EffectiveWindowSize reports the window-size in force for the shared
// session's current window, resolving inheritance (-A) so a
// window-local override is visible rather than the server default that
// it beats.
//
// It is only ever read. `window-size` is a WINDOW option, and neither
// way of writing one fits a share: per-window misses any window opened
// later, while -g reaches every unrelated session on the server, still
// loses to a window-local override, and cannot be restored by one share
// acting alone once two of them overlap. tmux already defaults to
// "latest", the value a share wants, so the operator's configuration
// stands and a share reports it instead.
func EffectiveWindowSize(r Runner, sessionID string) string {
	out, err := r.Run("show-options", "-w", "-t", sessionID, "-Aqv", "window-size")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// AttachEnv returns the server-owned environment for the forced tmux
// attach commands: base (the worker's own environment) minus TMUX and
// TMUX_PANE -- the worker itself runs inside the management session,
// and tmux refuses to attach when $TMUX is set -- with TERM defaulted
// so the attach client can render.
//
// A capability-free TERM is treated as unset because tmux refuses to
// attach under `dumb`.
func AttachEnv(base []string) []string {
	out := make([]string, 0, len(base)+1)
	hasTerm := false
	for _, kv := range base {
		if strings.HasPrefix(kv, "TMUX=") || strings.HasPrefix(kv, "TMUX_PANE=") {
			continue
		}
		if term, ok := strings.CutPrefix(kv, "TERM="); ok {
			if usableTERM(term) {
				hasTerm = true
			} else {
				continue
			}
		}
		out = append(out, kv)
	}
	if !hasTerm {
		out = append(out, "TERM=xterm-256color")
	}
	return out
}

// usableTERM reports whether a terminal type can carry a tmux client.
func usableTERM(term string) bool {
	switch term {
	case "", "dumb", "unknown":
		return false
	}
	return true
}

// ShellQuote single-quotes s for POSIX sh, for embedding in the shell
// command string tmux new-session runs.
func ShellQuote(s string) string {
	if s == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
