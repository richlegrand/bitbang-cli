package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/netip"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/term"

	"github.com/richlegrand/bitbang/internal/client"
	"github.com/richlegrand/bitbang/internal/protocol"
	"github.com/richlegrand/bitbang/internal/signaling"
	"github.com/richlegrand/bitbang/internal/tcpforward"
)

type connectOptions struct {
	verbose  bool
	timeout  time.Duration
	pin      string
	server   string
	name     string
	relay    bool
	gateway  bool
	forwards forwardFlags
	target   string
	argv     []string
}

type forwardFlags []tcpforward.Forward

func (f *forwardFlags) String() string {
	parts := make([]string, len(*f))
	for i, forward := range *f {
		parts[i] = forward.String()
	}
	return strings.Join(parts, ",")
}

func (f *forwardFlags) Set(value string) error {
	forward, err := parseLocalForward(value)
	if err != nil {
		return err
	}
	*f = append(*f, forward)
	return nil
}

func parseLocalForward(value string) (tcpforward.Forward, error) {
	localEnd := strings.IndexByte(value, ':')
	if localEnd <= 0 || localEnd == len(value)-1 {
		return tcpforward.Forward{}, fmt.Errorf("want LOCAL_PORT:REMOTE_HOST:REMOTE_PORT")
	}
	localPort, err := parseForwardPort(value[:localEnd])
	if err != nil {
		return tcpforward.Forward{}, fmt.Errorf("local port: %w", err)
	}
	remoteTarget := value[localEnd+1:]
	host, remotePortText, err := net.SplitHostPort(remoteTarget)
	if err != nil {
		return tcpforward.Forward{}, fmt.Errorf("remote target: %w", err)
	}
	if strings.HasPrefix(remoteTarget, "[") {
		if ip, err := netip.ParseAddr(host); err != nil || !ip.Is6() {
			return tcpforward.Forward{}, fmt.Errorf("remote target: invalid bracketed IPv6 address %q", host)
		}
	}
	remotePort, err := parseForwardPort(remotePortText)
	if err != nil {
		return tcpforward.Forward{}, fmt.Errorf("remote port: %w", err)
	}
	if err := tcpforward.ValidateTarget(host, remotePort); err != nil {
		return tcpforward.Forward{}, err
	}
	return tcpforward.Forward{LocalPort: localPort, Host: host, Port: remotePort}, nil
}

func parseForwardPort(value string) (int, error) {
	if value == "" {
		return 0, fmt.Errorf("port is empty")
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("%q is not a decimal port", value)
		}
	}
	port, err := strconv.Atoi(value)
	if err != nil || port < 1 || port > 65535 {
		return 0, fmt.Errorf("%q is outside 1-65535", value)
	}
	return port, nil
}

func parseConnectOptions(args []string, output io.Writer) (connectOptions, error) {
	var opts connectOptions
	fs := flag.NewFlagSet("connect", flag.ContinueOnError)
	fs.SetOutput(output)
	fs.BoolVar(&opts.verbose, "v", false, "Verbose logging")
	fs.DurationVar(&opts.timeout, "timeout", 30*time.Second, "Dial timeout")
	fs.StringVar(&opts.pin, "pin", "", "PIN (skips the interactive prompt)")
	fs.StringVar(&opts.server, "server", "bitba.ng", "Signaling server (pair-code mode only; URL form carries its own server)")
	fs.StringVar(&opts.name, "name", "", "Name to remember this device under (new devices only; auto-assigned if omitted)")
	fs.BoolVar(&opts.relay, "relay", false, "Request a TURN relay up front instead of only on fallback (ICE still prefers direct if it succeeds)")
	fs.Var(&opts.forwards, "L", "Forward LOCAL_PORT:REMOTE_HOST:REMOTE_PORT without opening a shell (repeatable; bracket IPv6 hosts)")
	fs.BoolVar(&opts.gateway, "g", false, "Bind forwarded ports on 0.0.0.0 instead of 127.0.0.1")
	if err := fs.Parse(reorderArgs(fs, args)); err != nil {
		return connectOptions{}, err
	}
	if opts.gateway && len(opts.forwards) == 0 {
		return connectOptions{}, fmt.Errorf("-g requires at least one -L forward")
	}
	posArgs := fs.Args()
	if len(posArgs) < 1 {
		return connectOptions{}, fmt.Errorf("missing URL, saved name, or pair code")
	}
	opts.target = posArgs[0]
	for i, arg := range posArgs[1:] {
		if arg == "--" {
			opts.argv = posArgs[2+i:]
			break
		}
	}
	if len(opts.forwards) > 0 && len(opts.argv) > 0 {
		return connectOptions{}, fmt.Errorf("-L cannot be combined with a remote command")
	}
	return opts, nil
}

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
// Successful connections are recorded in the table (see recordDevice),
// except for URLs carrying the !ephemeral flag: a `bitbang share` link
// dies with the share, so saving it would leave a device entry that
// never works again. Pass -name to choose the stored name; without it
// an auto name (device<N>) is assigned and printed.
//
// Mode auto-detection (URL flow only):
//   - Forwarding: one or more -L mappings are given. Open the local listeners
//     and hold them until a signal arrives or the WebRTC session closes. No
//     shell stream is opened.
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
	opts, err := parseConnectOptions(args, os.Stderr)
	if errors.Is(err, flag.ErrHelp) {
		return
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "bitbang connect: %v\n", err)
		fmt.Fprintln(os.Stderr, "Usage: bitbang connect <URL-or-pair-code> [-- argv...]")
		fmt.Fprintln(os.Stderr, "   or: bitbang connect <URL-or-pair-code> -L port:host:port [-L ...] [-g]")
		os.Exit(2)
	}
	urlArg := opts.target

	// Parse the URL shape first, because whether -name means anything
	// depends on it: an ephemeral share URL is never saved, so its name
	// is ignored either way and a name clash is not worth failing over.
	urlSpec, isURL := parseConnectURL(urlArg)
	isURL = isURL && !looksLikeDeviceName(urlArg) && !pairCodePattern.MatchString(urlArg)
	ephemeralURL := isURL && urlSpec.Ephemeral

	// Validate -name up front so a bad or already-taken name fails fast,
	// before any pairing or dialing. The authoritative checks live in
	// recordDevice (it knows the UID), but catching the common mistakes here
	// spares the operator a pointless handshake.
	if opts.name != "" && !ephemeralURL {
		if err := validateDeviceName(opts.name); err != nil {
			fail("connect: %v", err)
		}
		if _, taken := lookupDeviceByName(opts.name); taken {
			fail("connect: name %q is already used by another device", opts.name)
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
		if opts.name != "" {
			fail("connect: %q is already a saved device; renaming via connect isn't supported", urlArg)
		}
		rs = remoteSpec{Server: ent.Server, UID: ent.UID, Code: ent.AccessCode}
		saved = true
	case pairCodePattern.MatchString(urlArg):
		rs = runPairConnect(urlArg, opts.server, opts.verbose, opts.relay)
		// Pairing succeeded (runPairConnect exits on failure). Persist now,
		// before the reconnect dial — the pairing itself was the expensive,
		// one-shot step, so a reconnect hiccup shouldn't discard the result.
		recordAndReport(rs, opts.name)
		saved = true
	default:
		var ok bool
		rs, ok = urlSpec, isURL
		if !ok {
			fail("connect: %q is not a saved device name, a 6-digit pair code, or a valid URL", urlArg)
		}
	}

	fmt.Fprintf(os.Stderr, "Connecting to %s...\n", rs.Server)
	sess := dialConnect(rs, opts.verbose, opts.timeout, opts.pin, opts.relay, len(opts.forwards) > 0)
	var forwarder *client.LocalForwarder
	if len(opts.forwards) > 0 {
		forwarder, err = sess.StartLocalForwarding([]tcpforward.Forward(opts.forwards), opts.gateway)
		if err != nil {
			sess.Close()
			fail("connect: %v", err)
		}
		if opts.gateway {
			fmt.Fprintln(os.Stderr, "Warning: -g makes forwarded ports reachable from the local network.")
		}
		for _, forward := range opts.forwards {
			fmt.Fprintf(os.Stderr, "Forwarding %s -> %s\n", forward.BindAddress(opts.gateway), forward.TargetAddress())
		}
	}
	fmt.Fprintln(os.Stderr, "Connected.")

	// URL-flow hosts are remembered once we've actually connected. Pair and
	// name-resolved hosts are already saved (see above). !ephemeral URLs
	// (bitbang share) carry credentials that die with the share; never
	// saved.
	if !saved {
		if rs.Ephemeral {
			if opts.name != "" {
				fmt.Fprintln(os.Stderr, "Not saving: this is an ephemeral share URL (-name ignored).")
			}
		} else {
			recordAndReport(rs, opts.name)
		}
	}
	if forwarder != nil {
		signals := make(chan os.Signal, 1)
		signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
		sessionClosed := waitForForwardExit(sess.Done(), signals)
		signal.Stop(signals)
		forwarder.Close()
		sess.Close()
		if sessionClosed {
			fail("connect: connection closed")
		}
		return
	}

	// PTY only when stdin is a real terminal AND no argv was supplied.
	// With argv, the user wants a one-shot command run non-interactively
	// (suitable for scripting / piping).
	stdinIsTTY := term.IsTerminal(int(os.Stdin.Fd()))
	interactive := stdinIsTTY && len(opts.argv) == 0

	shellOpts := client.ShellOptions{
		Argv:   opts.argv,
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
	switch {
	case sess.ServerAccess == protocol.AccessView:
		// A share's view URL: output only. No raw mode; the local
		// terminal keeps cooked semantics, so Ctrl-C disconnects
		// instead of becoming an input byte nobody transmits.
		setupViewOnly(&shellOpts)
	case interactive:
		restore = setupInteractive(&shellOpts)
	default:
		setupNonInteractive(&shellOpts)
	}

	result, err := sess.Shell(shellOpts)
	restore() // BEFORE any os.Exit, including via fail().
	sess.Close()
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

// waitForForwardExit blocks until the WebRTC session closes or an operator
// signal arrives. It reports session closure so the caller can distinguish an
// unexpected disconnect from a graceful signal-driven shutdown.
func waitForForwardExit(sessionDone <-chan struct{}, signals <-chan os.Signal) (sessionClosed bool) {
	select {
	case <-sessionDone:
		return true
	case <-signals:
		return false
	}
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
	notifyWindowChanges(winch)
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

// setupViewOnly configures a watch-only session (a bitbang share view
// URL, where the listener granted access="view"). Nothing is
// transmitted but the terminal size: stdin is dropped and no signals
// are forwarded, matching what the listener would enforce anyway.
// Resizes still go out because they only size this viewer's own PTY on
// the far side. Without them the remote grid would not match the local
// window and the rendering would wrap wrongly.
func setupViewOnly(opts *client.ShellOptions) {
	opts.Stdin = nil
	opts.PTY = true
	fmt.Fprintln(os.Stderr, "View-only session: watching, input is not transmitted. Ctrl-C to disconnect.")

	if cols, rows, err := term.GetSize(int(os.Stdout.Fd())); err == nil {
		opts.Cols = cols
		opts.Rows = rows
	}
	resizes := make(chan client.ShellSize, 4)
	winch := make(chan os.Signal, 4)
	notifyWindowChanges(winch)
	go func() {
		for range winch {
			if cols, rows, err := term.GetSize(int(os.Stdout.Fd())); err == nil {
				select {
				case resizes <- client.ShellSize{Cols: cols, Rows: rows}:
				default:
				}
			}
		}
	}()
	opts.Resizes = resizes
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

// dialConnect handles the boilerplate: build DialOptions, run the handshake,
// and sanity-check that the listener advertises the requested stream type.
func dialConnect(r remoteSpec, verbose bool, timeout time.Duration, suppliedPIN string, relay, tcp bool) *client.Session {
	capability := "shell"
	if tcp {
		capability = "tcp"
	}
	opts := client.DialOptions{
		Server:      r.Server,
		UID:         r.UID,
		Code:        r.Code,
		Caps:        []string{capability},
		DialTimeout: timeout,
		ForceRelay:  relay,
		Verbose:     verbose,
		PINPrompt:   makePINPrompt(suppliedPIN),
	}
	sess, err := client.Dial(opts)
	if err != nil {
		fail("connect: %v", err)
	}
	if !hasCap(sess.ServerCaps, capability) {
		sess.Close()
		fail("connect: listener does not advertise the `%s` capability (caps: %v)", capability, sess.ServerCaps)
	}
	return sess
}

// parseConnectURL parses just the URL form (no `:/path` component).
// Sibling to parseRemoteSpec in cp.go, but for the connect case the
// path is meaningless — a shell stream doesn't address a path.
//
// Fragment grammar (see CONVENTIONS.md): `<code>[!<flags>][/<device-URL>]`.
// The device-URL part is irrelevant for connect; of the flags,
// "ephemeral" (bitbang share URLs) suppresses the devices.json save.
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
	code, flags := signaling.ParseFragment(u.Fragment)
	return remoteSpec{
		Server:    u.Host,
		UID:       uid,
		Code:      code,
		Ephemeral: signaling.HasFlag(flags, "ephemeral"),
	}, true
}
