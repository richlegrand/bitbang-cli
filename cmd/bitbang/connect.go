package main

import (
	"flag"
	"fmt"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/term"

	"github.com/richlegrand/bitbang/internal/client"
)

// runConnect implements `bitbang connect <URL-or-pair-code> [-- argv...]`.
//
// Two arg shapes are accepted:
//
//   - A 6-digit numeric code → pair flow against /ws/pair. Walks the
//     SAS-display dance, saves uid+access_code to ~/.bitbang/devices.json
//     on success, exits. Subsequent connects use the saved URL.
//   - Anything else → URL flow. Opens a remote shell to the listener.
//
// Mode auto-detection (URL flow only):
//   - Interactive: stdin is a TTY and no argv is given. Allocate a PTY
//     on the listener, put local terminal in raw mode, forward
//     keystrokes, render output, watch SIGWINCH for resize.
//   - Non-interactive: argv is given OR stdin is not a TTY. No PTY —
//     just pump stdin/stdout/stderr. Forward Ctrl-C / SIGTERM / SIGHUP
//     as explicit signal frames. Exit with the remote process's exit
//     code (or 128+n for signal exits, like a shell would).
//
// URL forms accepted (no `:/path` component — this is shell, not cp):
//
//	https://bitba.ng/<UID>#<CODE>
//	bitba.ng/<UID>#<CODE>
//	<UID>#<CODE>                  (defaults to bitba.ng)
func runConnect(args []string) {
	fs := flag.NewFlagSet("connect", flag.ExitOnError)
	verbose := fs.Bool("v", false, "Verbose logging")
	timeout := fs.Duration("timeout", 30*time.Second, "Dial timeout")
	pin := fs.String("pin", "", "PIN (skips the interactive prompt)")
	server := fs.String("server", "bitba.ng", "Signaling server (pair-code mode only; URL form carries its own server)")
	fs.Parse(reorderArgs(fs, args))

	posArgs := fs.Args()
	if len(posArgs) < 1 {
		fmt.Fprintln(os.Stderr, "Usage: bitbang connect <URL-or-pair-code> [-- argv...]")
		os.Exit(2)
	}
	urlArg := posArgs[0]

	// Split off the optional `-- argv...` block. The `--` literal marks
	// the boundary between bitbang flags + URL and the remote command.
	var argv []string
	for i, a := range posArgs[1:] {
		if a == "--" {
			argv = posArgs[2+i:]
			break
		}
	}

	// Decide where the remoteSpec comes from. A 6-digit code triggers the
	// pair flow first, then falls through with the just-obtained
	// credentials so we land in a shell exactly like the URL form would.
	// Otherwise parse the URL form directly.
	var rs remoteSpec
	if pairCodePattern.MatchString(urlArg) {
		rs = runPairConnect(urlArg, *server, *verbose)
	} else {
		var ok bool
		rs, ok = parseConnectURL(urlArg)
		if !ok {
			fail("connect: invalid URL: %s", urlArg)
		}
	}

	// Mode decision: PTY only when stdin is a real terminal AND no argv
	// was supplied. With argv, the user wants a one-shot command run
	// non-interactively (suitable for scripting / piping).
	stdinIsTTY := term.IsTerminal(int(os.Stdin.Fd()))
	interactive := stdinIsTTY && len(argv) == 0

	fmt.Fprintf(os.Stderr, "Connecting to %s...\n", rs.Server)
	sess := dialConnect(rs, *verbose, *timeout, *pin)
	defer sess.Close()
	fmt.Fprintln(os.Stderr, "Connected.")

	opts := client.ShellOptions{
		Argv:   argv,
		PTY:    interactive,
		Stdin:  os.Stdin,
		Stdout: os.Stdout,
		Stderr: os.Stderr,
	}

	// restore is a no-op for non-interactive; in interactive mode it
	// puts the local terminal back to cooked mode. We can't rely on
	// `defer restore()` because every exit path below goes through
	// os.Exit, which skips defers — so we call restore explicitly
	// before each os.Exit (sync.Once inside makes double-calls safe).
	restore := func() {}
	if interactive {
		restore = setupInteractive(&opts)
	} else {
		setupNonInteractive(&opts)
	}

	result, err := sess.Shell(opts)
	restore() // BEFORE any os.Exit, including via fail().
	if err != nil {
		fail("connect: %v", err)
	}
	// Exit-code convention: process exited normally → that code. Killed
	// by a signal → 128+(signal number unknown to us, just report 128).
	// Matches the shape `bash` uses so wrapping scripts work predictably.
	if result.Signal != "" {
		os.Exit(128)
	}
	os.Exit(result.ExitCode)
}

// setupInteractive flips the local terminal into raw mode, installs a
// SIGWINCH handler for resize forwarding, and arranges for terminal
// restoration on signal exits (so an external `kill` doesn't leave the
// terminal in raw mode). Returns the cleanup function the caller MUST
// defer — restore can't be deferred inside this function because the
// defer would fire on return, undoing the raw mode immediately.
func setupInteractive(opts *client.ShellOptions) func() {
	oldState, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err != nil {
		fail("connect: enter raw mode: %v", err)
	}
	// Restore exactly once, whether via normal return (caller's defer)
	// or signal-driven exit (goroutine below). The signal goroutine
	// uses os.Exit, which doesn't run defers — sync.Once keeps the
	// caller's defer from double-restoring on the happy path.
	var restoreOnce sync.Once
	restore := func() {
		restoreOnce.Do(func() {
			_ = term.Restore(int(os.Stdin.Fd()), oldState)
		})
	}

	// Catch SIGHUP / SIGTERM so we restore the terminal before exiting.
	// We deliberately do NOT register SIGINT here: in raw mode the
	// kernel doesn't translate Ctrl-C to SIGINT (it sends byte 0x03
	// as character input), and the remote PTY converts that back to
	// SIGINT on its end. Registering SIGINT here would let Go's
	// runtime intercept external `kill -INT` correctly, but would
	// also potentially interfere if raw-mode setup is incomplete.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGHUP, syscall.SIGTERM)
	go func() {
		<-sigCh
		restore()
		os.Exit(1)
	}()

	// Initial terminal size sent in the SYN; subsequent SIGWINCH
	// events arrive over the resize channel.
	if cols, rows, err := term.GetSize(int(os.Stdin.Fd())); err == nil {
		opts.Cols = cols
		opts.Rows = rows
	}

	resizes := make(chan client.ShellSize, 4)
	winch := make(chan os.Signal, 4)
	signal.Notify(winch, syscall.SIGWINCH)
	go func() {
		for range winch {
			if cols, rows, err := term.GetSize(int(os.Stdin.Fd())); err == nil {
				select {
				case resizes <- client.ShellSize{Cols: cols, Rows: rows}:
				default:
					// Buffer full — drop. Resize events are
					// idempotent enough that missing one is fine.
				}
			}
		}
	}()
	opts.Resizes = resizes
	return restore
}

// setupNonInteractive wires Ctrl-C / SIGTERM / SIGHUP to the explicit
// signal-forwarding channel. The local terminal stays in cooked mode;
// pipes flow through unmodified.
func setupNonInteractive(opts *client.ShellOptions) {
	signals := make(chan string, 4)
	sigCh := make(chan os.Signal, 4)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	go func() {
		for s := range sigCh {
			var name string
			switch s {
			case syscall.SIGINT:
				name = "INT"
			case syscall.SIGTERM:
				name = "TERM"
			case syscall.SIGHUP:
				name = "HUP"
			default:
				continue
			}
			select {
			case signals <- name:
			default:
			}
		}
	}()
	opts.Signals = signals
}

// dialConnect handles the boilerplate: build DialOptions, run the
// handshake, sanity-check that the listener advertises "shell."
func dialConnect(r remoteSpec, verbose bool, timeout time.Duration, suppliedPIN string) *client.Session {
	opts := client.DialOptions{
		Server:      r.Server,
		UID:         r.UID,
		Code:        r.Code,
		Caps:        []string{"shell"},
		DialTimeout: timeout,
		Verbose:     verbose,
		PINPrompt:   makePINPrompt(suppliedPIN),
	}
	sess, err := client.Dial(opts)
	if err != nil {
		fail("connect: %v", err)
	}
	if !hasCap(sess.ServerCaps, "shell") {
		sess.Close()
		fail("connect: listener does not advertise the `shell` capability (caps: %v)", sess.ServerCaps)
	}
	return sess
}

// parseConnectURL parses just the URL form (no `:/path` component).
// Sibling to parseRemoteSpec in cp.go, but for the connect case the
// path is meaningless — a shell stream doesn't address a path.
func parseConnectURL(arg string) (remoteSpec, bool) {
	urlPart := arg
	if !strings.Contains(urlPart, "://") {
		// Bare UID#CODE or server/UID#CODE — normalize to https://.
		if !strings.Contains(urlPart, "/") {
			urlPart = "bitba.ng/" + urlPart
		}
		urlPart = "https://" + urlPart
	}
	u, err := url.Parse(urlPart)
	if err != nil || u.Host == "" {
		return remoteSpec{}, false
	}
	uid := strings.TrimPrefix(u.Path, "/")
	if i := strings.IndexByte(uid, '/'); i >= 0 {
		uid = uid[:i]
	}
	if uid == "" {
		return remoteSpec{}, false
	}
	return remoteSpec{
		Server: u.Host,
		UID:    uid,
		Code:   u.Fragment,
	}, true
}
