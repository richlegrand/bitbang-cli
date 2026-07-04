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

// runConnect implements `bitbang connect <name-or-URL-or-pair-code> [-- argv...]`.
//
// Three arg shapes are accepted, disambiguated by the deviceNamePattern rule
// (a name starts with a letter and has no URL/code punctuation, so the shapes
// never overlap):
//
//   - A bare name → look it up in the known-hosts table (~/.bitbang/devices.json)
//     and connect directly with the stored uid+access_code.
//   - A 6-digit numeric code → pair flow against /ws/pair. Walks the SAS dance,
//     then continues into the same direct connect using the obtained creds.
//   - Anything else → URL flow. Opens a remote shell to the listener.
//
// Every successful connection is recorded in the table (see recordDevice).
// Pass -name to choose the stored name; without it an auto name (device<N>)
// is assigned and printed.
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
	name := fs.String("name", "", "Name to remember this device under (new devices only; auto-assigned if omitted)")
	relay := fs.Bool("relay", false, "Request a TURN relay up front instead of only on fallback (ICE still prefers direct if it succeeds)")
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

	// Validate -name up front so a bad or already-taken name fails fast,
	// before any pairing or dialing. The authoritative checks live in
	// recordDevice (it knows the UID), but catching the common mistakes here
	// spares the operator a pointless handshake.
	if *name != "" {
		if err := validateDeviceName(*name); err != nil {
			fail("connect: %v", err)
		}
		if _, taken := lookupDeviceByName(*name); taken {
			fail("connect: name %q is already used by another device", *name)
		}
	}

	// Decide where the remoteSpec comes from. The three shapes are mutually
	// exclusive by construction (deviceNamePattern excludes digits-only and
	// URL punctuation), so the order is for clarity, not precedence.
	//
	//   bare name  → known-hosts lookup (direct connect with stored creds)
	//   6 digits   → pair flow, then direct connect with obtained creds
	//   otherwise  → URL form
	//
	// `saved` records whether the host is already persisted, suppressing the
	// post-connect save: a name-resolved host is already in the table, and a
	// paired host is saved at approval time (below) so a flaky reconnect
	// doesn't lose the credentials or burn the one-time code.
	var rs remoteSpec
	var saved bool
	switch {
	case looksLikeDeviceName(urlArg):
		ent, ok := lookupDeviceByName(urlArg)
		if !ok {
			fail("connect: no saved device named %q (expected a saved name, a 6-digit pair code, or a URL)", urlArg)
		}
		if *name != "" {
			fail("connect: %q is already a saved device; renaming via connect isn't supported", urlArg)
		}
		rs = remoteSpec{Server: ent.Server, UID: ent.UID, Code: ent.AccessCode}
		saved = true
	case pairCodePattern.MatchString(urlArg):
		rs = runPairConnect(urlArg, *server, *verbose, *relay)
		// Pairing succeeded (runPairConnect exits on failure). Persist now,
		// before the reconnect dial — the pairing itself was the expensive,
		// one-shot step, so a reconnect hiccup shouldn't discard the result.
		recordAndReport(rs, *name)
		saved = true
	default:
		var ok bool
		rs, ok = parseConnectURL(urlArg)
		if !ok {
			fail("connect: %q is not a saved device name, a 6-digit pair code, or a valid URL", urlArg)
		}
	}

	// Mode decision: PTY only when stdin is a real terminal AND no argv
	// was supplied. With argv, the user wants a one-shot command run
	// non-interactively (suitable for scripting / piping).
	stdinIsTTY := term.IsTerminal(int(os.Stdin.Fd()))
	interactive := stdinIsTTY && len(argv) == 0

	fmt.Fprintf(os.Stderr, "Connecting to %s...\n", rs.Server)
	sess := dialConnect(rs, *verbose, *timeout, *pin, *relay)
	defer sess.Close()
	fmt.Fprintln(os.Stderr, "Connected.")

	// URL-flow hosts are remembered once we've actually connected. Pair and
	// name-resolved hosts are already saved (see above).
	if !saved {
		recordAndReport(rs, *name)
	}

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

// recordAndReport persists a connected/paired host to the known-hosts table
// and prints the outcome. A table failure is never fatal — the session is
// already up — so it only warns. "Saved as" prints only for a newly-created
// entry; reconnecting a known host updates its timestamp silently.
func recordAndReport(rs remoteSpec, name string) {
	savedName, status, err := recordDevice(rs.Server, rs.UID, rs.Code, name)
	switch {
	case err != nil:
		fmt.Fprintf(os.Stderr, "Connected, but couldn't update device table: %v\n", err)
	case status == recordCreatedAuto:
		fmt.Fprintf(os.Stderr, "Saved as %q.  (tip: pass -name <name> to choose your own)\n", savedName)
	case status == recordCreated:
		fmt.Fprintf(os.Stderr, "Saved as %q.\n", savedName)
	}
}

// dialConnect handles the boilerplate: build DialOptions, run the
// handshake, sanity-check that the listener advertises "shell."
func dialConnect(r remoteSpec, verbose bool, timeout time.Duration, suppliedPIN string, relay bool) *client.Session {
	opts := client.DialOptions{
		Server:      r.Server,
		UID:         r.UID,
		Code:        r.Code,
		Caps:        []string{"shell"},
		DialTimeout: timeout,
		ForceRelay:  relay,
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
//
// Fragment grammar (see CONVENTIONS.md): `<code>[!<flags>][/<device-URL>]`.
// For `bitbang connect` the flags and device-URL are irrelevant — we take
// only the code, stopping at the first `!` or `/`.
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
	code := u.Fragment
	if i := strings.IndexAny(code, "!/"); i >= 0 {
		code = code[:i]
	}
	return remoteSpec{
		Server: u.Host,
		UID:    uid,
		Code:   code,
	}, true
}
