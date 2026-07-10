package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"unicode/utf8"

	qrcode "github.com/skip2/go-qrcode"
	"golang.org/x/term"

	"github.com/richlegrand/bitbang/internal/auth"
	"github.com/richlegrand/bitbang/internal/fileshare"
	"github.com/richlegrand/bitbang/internal/identity"
	"github.com/richlegrand/bitbang/internal/pairing"
	"github.com/richlegrand/bitbang/internal/peer"
	"github.com/richlegrand/bitbang/internal/proxyweb"
	"github.com/richlegrand/bitbang/internal/session"
	"github.com/richlegrand/bitbang/internal/shellweb"
	"github.com/richlegrand/bitbang/internal/signaling"
	"github.com/richlegrand/bitbang/internal/streamtype"
	"github.com/richlegrand/bitbang/internal/videohelper"
)

// maxUnauthSessions bounds how many sessions may sit pre-PIN-auth at once,
// limiting parallel brute-force. A single human needs exactly one.
const maxUnauthSessions int32 = 10

// serveConfig is the assembled per-mode configuration that the shared
// listener loop in startListener uses. Each mode (all / shell / files /
// proxy) populates the fields relevant to it and leaves the rest zero.
type serveConfig struct {
	// Shared flags (all modes).
	server    string
	pin       string
	ephemeral bool
	verbose   bool

	// nocode disables the code-exchange pairing flow. Default is code ON
	// for the `bitbang serve` family — pairing is the expected way new
	// users reach a listener — but a non-interactive deployment (systemd
	// unit, batch job) won't be able to answer the SAS-entry prompt and
	// should pass --nocode to suppress code issuance entirely.
	nocode bool

	// Inherited socketpair FD for an external video helper (-1 = disabled).
	// When set, each session negotiates a secondary video PeerConnection with
	// the browser, relayed to the helper process over this FD.
	videoFD int

	// Identity program name: the persistent key lives at
	// ~/.bitbang/<program>/identity.pem. Lets an embedding process (e.g. the
	// OctoPrint plugin) point us at its existing identity so we share its URL.
	program string

	// Fixed proxy target (host:port). When set (proxy-only mode), every
	// request goes straight to this target — the plain device URL serves the
	// app directly, no path-based target selection / landing page.
	target string

	// forwardClientIP stamps the real browser IP as X-Forwarded-For on
	// proxied requests (fixed-target mode only). Off by default: the
	// OctoPrint plugin enables it ONLY when OctoPrint is configured to make
	// localhost-based trust decisions (autologinLocal etc.), so the common
	// case doesn't trip OctoPrint's "external access" warning needlessly.
	forwardClientIP bool

	// Caps actually enabled in this mode.
	shellEnabled bool
	filesEnabled bool
	proxyEnabled bool

	// Shell-cap configuration (only set when shellEnabled).
	shellCmd         string
	shellMaxSessions int
	shellMirror      bool

	// Files-cap configuration (only set when filesEnabled).
	filesPath   string
	filesUpload bool
}

// runServe — `bitbang serve` — exposes shell + files + proxy. The
// launcher tab serves shell at `/`; the hamburger menu lets users open
// Files or Proxy in new browser tabs. Files-only / Shell-only /
// Proxy-only modes are dedicated subcommands; this mode is the "I want
// everything I can get from a single listener" entry point.
func runServe(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	cfg := serveConfig{shellEnabled: true, filesEnabled: true, proxyEnabled: true}
	registerSharedFlags(fs, &cfg)
	registerShellFlags(fs, &cfg)
	fs.StringVar(&cfg.filesPath, "files", "", "Files path (default: current working directory)")
	fs.BoolVar(&cfg.filesUpload, "files-upload", false, "Allow uploads to the shared directory")

	fs.Parse(reorderArgs(fs, args))

	if cfg.filesPath == "" {
		cwd, err := os.Getwd()
		if err != nil {
			fmt.Fprintf(os.Stderr, "Cannot determine current directory: %v\n", err)
			os.Exit(1)
		}
		cfg.filesPath = cwd
	}

	startListener(cfg)
}

// runServeShell — `bitbang serve shell` — exposes shell only. No
// hamburger; the entire tab is the shell.
func runServeShell(args []string) {
	fs := flag.NewFlagSet("serve shell", flag.ExitOnError)
	cfg := serveConfig{shellEnabled: true}
	registerSharedFlags(fs, &cfg)
	registerShellFlags(fs, &cfg)
	fs.Parse(reorderArgs(fs, args))
	startListener(cfg)
}

// runServeFiles — `bitbang serve files [PATH]` — exposes files only.
// PATH is positional (defaults to cwd). No hamburger; the tab is the
// file browser.
func runServeFiles(args []string) {
	fs := flag.NewFlagSet("serve files", flag.ExitOnError)
	cfg := serveConfig{filesEnabled: true}
	registerSharedFlags(fs, &cfg)
	fs.BoolVar(&cfg.filesUpload, "upload", false, "Allow uploads to the shared directory")

	fs.Parse(reorderArgs(fs, args))

	// Positional PATH lives in fs.Args() after Parse — at most one.
	switch fs.NArg() {
	case 0:
		cwd, err := os.Getwd()
		if err != nil {
			fmt.Fprintf(os.Stderr, "Cannot determine current directory: %v\n", err)
			os.Exit(1)
		}
		cfg.filesPath = cwd
	case 1:
		cfg.filesPath = fs.Arg(0)
	default:
		fmt.Fprintln(os.Stderr, "bitbang serve files: at most one PATH argument")
		os.Exit(2)
	}

	startListener(cfg)
}

// runServeProxy — `bitbang serve proxy [TARGET]` — exposes an HTTP
// reverse proxy. Without TARGET, runs in dynamic-target mode (landing
// page asks for the host). With TARGET, pins to a single host:port and
// the bare device URL serves that target directly.
//
// TARGET can be supplied either positionally (`serve proxy host:port`)
// or via the shared `-target` flag. If both are given, the positional
// wins — the user typed it more explicitly.
func runServeProxy(args []string) {
	fs := flag.NewFlagSet("serve proxy", flag.ExitOnError)
	cfg := serveConfig{proxyEnabled: true}
	registerSharedFlags(fs, &cfg)
	fs.Parse(reorderArgs(fs, args))

	// Optional positional TARGET. Mirrors `serve files [PATH]`.
	switch fs.NArg() {
	case 0:
		// No positional; cfg.target may already be set via -target flag,
		// or empty (dynamic-target mode).
	case 1:
		cfg.target = fs.Arg(0)
	default:
		fmt.Fprintln(os.Stderr, "bitbang serve proxy: at most one TARGET argument")
		os.Exit(2)
	}

	startListener(cfg)
}

// registerSharedFlags wires --pin, --ephemeral, --server, -v on every
// mode. They have the same semantics across all four runServe*
// functions, so factor them out.
func registerSharedFlags(fs *flag.FlagSet, cfg *serveConfig) {
	fs.StringVar(&cfg.server, "server", "bitba.ng", "Signaling server hostname")
	fs.StringVar(&cfg.pin, "pin", "", "PIN to protect access")
	fs.BoolVar(&cfg.ephemeral, "ephemeral", false, "Use a temporary identity")
	fs.BoolVar(&cfg.verbose, "v", false, "Verbose logging")
	fs.BoolVar(&cfg.nocode, "nocode", false, "Disable code-exchange pairing (operator typed SAS); URL still works")
	fs.IntVar(&cfg.videoFD, "video-fd", -1, "Inherited socketpair FD to a video helper process (-1 = disabled)")
	fs.StringVar(&cfg.program, "program", "", "Identity program-name override; default is derived from the mode/target (key at ~/.bitbang/<program>/identity.pem)")
	fs.StringVar(&cfg.target, "target", "", "Fixed proxy target host:port (proxy-only mode); empty = dynamic from URL")
	fs.BoolVar(&cfg.forwardClientIP, "forward-client-ip", false, "Stamp the real browser IP as X-Forwarded-For (fixed-target mode); enable only when the backend trusts localhost for auth")
}

// registerShellFlags wires the shell-specific flags. Used by both
// `serve` (all-mode) and `serve shell` since both expose a shell.
func registerShellFlags(fs *flag.FlagSet, cfg *serveConfig) {
	fs.StringVar(&cfg.shellCmd, "shell-cmd", "", "Shell command to spawn (default: $SHELL or /bin/sh)")
	fs.IntVar(&cfg.shellMaxSessions, "shell-max-sessions", 1, "Max concurrent shell sessions (0 = unlimited)")
	fs.BoolVar(&cfg.shellMirror, "shell-mirror", true, "Mirror shell output to listener console")
}

// startListener is the shared listener loop. Given a populated
// serveConfig, it sets up identity, signaling, the HTTP front-end, and
// the SWSP handler dispatch — then blocks accepting peer requests.
//
// Each mode's runServe* function does mode-specific flag parsing then
// calls in here. Per-cap state (shell mirror, file share) is built
// based on which *Enabled fields are set.
// smallQR renders url as a compact half-block QR for the console. go-qrcode
// only offers a full 4-module quiet zone or none at all (DisableBorder); a
// borderless code scans poorly against the adjacent URL text, so we take the
// borderless bitmap and pad a 1-module quiet zone by hand, then render with the
// same half-block scheme as qrcode.ToSmallString(false) (false → █ light
// margin/module, true → space dark module) so the scan polarity is unchanged.
func smallQR(url string) string {
	qr, err := qrcode.New(url, qrcode.Low)
	if err != nil {
		return ""
	}
	qr.DisableBorder = true
	bits := qr.Bitmap()

	// One light (false) module of quiet zone on every side.
	n := len(bits)
	padded := make([][]bool, n+2)
	for i := range padded {
		padded[i] = make([]bool, n+2)
	}
	for y := 0; y < n; y++ {
		copy(padded[y+1][1:], bits[y])
	}

	// Pack two vertical modules per text row via half-block glyphs.
	var b strings.Builder
	for y := 0; y+1 < len(padded); y += 2 {
		for x := range padded[y] {
			top, bot := padded[y][x], padded[y+1][x]
			switch {
			case top == bot && !top:
				b.WriteString("█")
			case top == bot:
				b.WriteString(" ")
			case !top:
				b.WriteString("▀")
			default:
				b.WriteString("▄")
			}
		}
		b.WriteByte('\n')
	}
	if len(padded)%2 == 1 { // odd height — last row is an upper half only
		for _, dark := range padded[len(padded)-1] {
			if dark {
				b.WriteString(" ")
			} else {
				b.WriteString("▀")
			}
		}
		b.WriteByte('\n')
	}
	return b.String()
}

func startListener(cfg serveConfig) {
	// Build the file share if files enabled.
	var share *fileshare.FileShare
	if cfg.filesEnabled {
		s, err := fileshare.New(cfg.filesPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Cannot share %q: %v\n", cfg.filesPath, err)
			os.Exit(1)
		}
		s.UploadEnabled = cfg.filesUpload
		share = s
	}

	var shellArgv []string
	if cfg.shellCmd != "" {
		shellArgv = []string{cfg.shellCmd}
	}

	pinAuth := auth.New(cfg.pin)

	// Identity is keyed by access scope: shell-bearing configs share the master
	// "bitbang" UID; each single non-shell cap (and each fixed proxy target /
	// file path) gets its own stable UID so distinct tasks coexist on one
	// machine with distinct, scope-limited URLs. See deriveProgram.
	program := deriveProgram(cfg)

	// Hold a per-identity lock so a second local process with the same scope
	// can't silently preempt this one at the signaling server (one connection
	// per UID). Skipped for ephemeral identities (random UID, no collision).
	// The OS releases the lock on exit; a same-process reconnect is unaffected.
	if !cfg.ephemeral {
		lock, holderPID, lockErr := acquireIdentityLock(identity.Dir(program))
		if lockErr == errIdentityBusy {
			who := ""
			if holderPID > 0 {
				who = fmt.Sprintf(" (PID %d)", holderPID)
			}
			fmt.Fprintf(os.Stderr,
				"A bitbang listener is already running for identity %q on this machine%s.\n"+
					"Stop it first, run a different mode/target, or pass --program <name> for a separate identity.\n",
				program, who)
			os.Exit(1)
		} else if lockErr != nil {
			fmt.Fprintf(os.Stderr, "Identity lock error: %v\n", lockErr)
			os.Exit(1)
		}
		defer lock.release()
	}

	id, err := identity.Load(program, cfg.ephemeral)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Identity error: %v\n", err)
		os.Exit(1)
	}

	// Optional video helper: an external process (e.g. Python aiortc driving
	// the camera) reached over an inherited socketpair FD. Each session gets a
	// per-client bridge that negotiates a video PC with the browser.
	var videoClient *videohelper.Client
	if cfg.videoFD >= 0 {
		videoClient, err = videohelper.DialFD(cfg.videoFD)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Video helper error: %v\n", err)
			os.Exit(1)
		}
		log.Printf("Video helper attached on fd %d", cfg.videoFD)
	}

	httpFront := buildServeHTTPHandler(share, cfg.shellEnabled, cfg.proxyEnabled,
		cfg.shellMaxSessions, isAllMode(cfg))

	signalingClient := signaling.NewClient(cfg.server, id)
	signalingClient.Verbose = cfg.verbose
	signalingClient.WantCode = !cfg.nocode
	// Override the library default: for a CLI listener, the right
	// response to preemption is to print a clear line and exit. The
	// library-internal reconnect-storm prevention is unaffected by this
	// override (it runs before the callback fires).
	signalingClient.OnPreempted = func() {
		fmt.Fprintln(os.Stderr, "Another instance with the same UID has taken over. Exiting.")
		os.Exit(2)
	}
	url := signalingClient.URL(cfg.verbose)

	stdoutIsTTY := term.IsTerminal(int(os.Stdout.Fd()))
	termWidth := 0
	if w, _, err := term.GetSize(int(os.Stdout.Fd())); err == nil {
		termWidth = w
	}
	bold, reset := "", ""
	if stdoutIsTTY {
		bold, reset = "\033[1m", "\033[0m"
	}

	// printReady renders the banner, QR code, and URL. On a wide TTY the
	// banner sits to the right of the QR (vertically centered) so the whole
	// startup block stays short enough to fit on one screen — handy for a
	// screen recording. On a narrow or non-TTY output it falls back to the
	// banner stacked above the QR so pipes, logs, and tests stay readable.
	printReady := func() {
		qr := smallQR(url)
		bannerLines := strings.Split(strings.TrimRight(banner, "\n"), "\n")
		bannerLines = append(bannerLines, "bitbang-cli v"+version)
		var qrLines []string
		if qr != "" {
			qrLines = strings.Split(strings.TrimRight(qr, "\n"), "\n")
		}

		bannerWidth := 0
		for _, l := range bannerLines {
			if w := utf8.RuneCountInString(l); w > bannerWidth {
				bannerWidth = w
			}
		}
		const gap = "   "
		qrWidth := 0
		if len(qrLines) > 0 {
			qrWidth = utf8.RuneCountInString(qrLines[0])
		}

		if len(qrLines) > 0 && stdoutIsTTY && termWidth >= qrWidth+len(gap)+bannerWidth {
			// QR flush left keeps its quiet zone against the terminal margin;
			// the banner is centered vertically against the taller QR block.
			off := (len(qrLines) - len(bannerLines)) / 2
			if off < 0 {
				off = 0
			}
			for i, ql := range qrLines {
				if bi := i - off; bi >= 0 && bi < len(bannerLines) {
					fmt.Println(ql + gap + bannerLines[bi])
				} else {
					fmt.Println(ql)
				}
			}
		} else {
			for _, l := range bannerLines {
				fmt.Println(l)
			}
			fmt.Println()
			fmt.Print(qr)
		}
		fmt.Printf("URL: %s\n", url)
	}

	// printPairCode renders the issued pairing code on its own line —
	// the operator shares this verbally so the connector can pair
	// without the full UID URL. Code may be empty when (a) --nocode is
	// set, or (b) the server lacks pairing support. In either case the
	// URL flow still works; we just don't surface a code. Bolded on a
	// TTY so it's easy to spot in the startup block; plain on pipes
	// so log scrapers/tests aren't confused by escape sequences.
	printPairCode := func() {
		if signalingClient.PairingCode != "" {
			fmt.Printf("%sPairing code: %s%s (valid 5 minutes)\n", bold, signalingClient.PairingCode, reset)
		}
	}

	printReady()
	printSharingBlock(cfg, share)

	// PIN status / shell-without-PIN warning.
	if pinAuth.Required() {
		fmt.Println("PIN protection enabled.")
	} else if cfg.shellEnabled {
		fmt.Fprintln(os.Stderr, "⚠ Anyone with this URL gets a shell on this machine.")
		fmt.Fprintln(os.Stderr, "  Use --pin <PIN> for a second factor, or pick a non-shell mode.")
	}

	var mu sync.Mutex
	connections := make(map[string]*peer.Connection)

	// unauthSessions counts live sessions that haven't completed the PIN
	// handshake. Bounds parallel brute-force; released on auth or close.
	var unauthSessions atomic.Int32

	firstReady := true
	signalingClient.OnReady = func() {
		if firstReady {
			firstReady = false
			// First-ready: URL/QR was already printed above (synchronously,
			// before Connect). Print just the pair code now that we've
			// learned it from the registered reply.
			printPairCode()
			return
		}
		// Reconnect: re-print URL+QR (operator may have scrolled past it
		// during a long-running session) and the freshly-issued code.
		printReady()
		printPairCode()
	}

	signalingClient.Connect(func(msg signaling.Message) {
		msgType, _ := msg["type"].(string)

		switch msgType {
		case "request":
			clientID, _ := msg["client_id"].(string)
			// Real browser IP from the signaling server (never client-set);
			// stamped as X-Forwarded-For on proxied requests so the backend
			// sees the true origin instead of our localhost socket peer.
			browserIP, _ := msg["browser_ip"].(string)

			// Cap concurrent un-authenticated sessions to blunt parallel
			// PIN brute-forcing. A connector must already hold the access
			// code to get this far, but without a cap they could open many
			// sessions at once and guess PINs in parallel. Authenticated
			// sessions release their slot (see OnReady below), so legit
			// users never hit this.
			if unauthSessions.Load() >= maxUnauthSessions {
				log.Printf("Rejecting connection from %s: too many pending sessions (%d)", clientID, maxUnauthSessions)
				return
			}

			var handlers []streamtype.StreamHandler
			if share != nil {
				handlers = append(handlers, streamtype.NewFile(share, cfg.verbose))
			}
			var shellHandler *streamtype.ShellHandler
			if cfg.shellEnabled {
				shellHandler = streamtype.NewShell(shellArgv, cfg.verbose)
				shellHandler.MaxConcurrent = cfg.shellMaxSessions
				if cfg.shellMirror {
					shellHandler.StdoutMirror = os.Stdout
					shellHandler.StderrMirror = os.Stderr
				}
				handlers = append(handlers, shellHandler)
			}
			// Fixed-target proxy-only mode (e.g. the OctoPrint plugin): every
			// request goes straight to --target, so the plain device URL serves
			// the app directly — no dispatcher, no landing page.
			if cfg.proxyEnabled && cfg.target != "" && !cfg.shellEnabled && !cfg.filesEnabled {
				// Only forward the client IP when explicitly enabled (the
				// backend trusts localhost for auth); otherwise withhold it so
				// requests look local and don't trip an external-access warning.
				xffIP := ""
				if cfg.forwardClientIP {
					xffIP = browserIP
				}
				httpProxy := streamtype.NewHTTPProxy(cfg.target, id.UID, cfg.server, xffIP, cfg.verbose)
				// Pair a WebSocket handler so ws:// streams resolve to the same
				// target as HTTP (otherwise: "no handler for stream type websocket").
				handlers = append(handlers, httpProxy,
					streamtype.NewWebSocket(httpProxy, xffIP, cfg.verbose))
			} else {
				localHTTP := streamtype.NewHTTPLocal(httpFront, cfg.verbose)
				var proxyHTTP streamtype.StreamHandler
				if cfg.proxyEnabled {
					// Dynamic-target mode: withhold browser_ip so we DON'T inject
					// XFF. This mode proxies arbitrary LAN apps that may rely on
					// requests appearing local; silently forwarding the real IP
					// could break their access control. (Fixed-target/OctoPrint
					// mode above passes it — there the backend is known.)
					p := streamtype.NewHTTPProxy("", id.UID, cfg.server, "", cfg.verbose)
					proxyHTTP = p
					handlers = append(handlers, streamtype.NewWebSocket(p, "", cfg.verbose))
				}
				handlers = append(handlers, newHTTPDispatcher(localHTTP, proxyHTTP))
			}

			var sess *session.Session

			conn, err := peer.HandleRequest(msg, signalingClient, id, func(data []byte) {
				if sess != nil {
					sess.HandleMessage(data)
				}
			}, cfg.verbose)
			if err != nil {
				log.Printf("Failed to create peer connection: %v", err)
				return
			}

			sess = session.New(conn.DC, pinAuth, cfg.verbose, handlers...)

			// Count this session against the unauth cap; release the slot
			// exactly once, whichever comes first: it authenticates (OnReady)
			// or it closes. sync.Once makes the double-path idempotent.
			unauthSessions.Add(1)
			var releaseOnce sync.Once
			release := func() { releaseOnce.Do(func() { unauthSessions.Add(-1) }) }
			sess.OnReady = release

			// Per-connection teardown, run when the data channel closes.
			var onClose []func()
			onClose = append(onClose, release)
			// Kill any shell processes — without this they outlive the
			// browser tab and keep holding their max-sessions slot.
			if shellHandler != nil {
				onClose = append(onClose, shellHandler.KillAll)
			}
			// Tear down the video PC and unregister the bridge.
			if videoClient != nil {
				// Forward the data PC's ICE servers so the video PC can use
				// the same STUN/TURN (needed for peers with no direct path).
				var iceServers []map[string]interface{}
				if raw, ok := msg["ice_servers"].([]interface{}); ok {
					for _, s := range raw {
						if m, ok := s.(map[string]interface{}); ok {
							iceServers = append(iceServers, m)
						}
					}
				}
				bridge := videoClient.Bridge(clientID, iceServers)
				sess.SetVideoBridge(bridge)
				onClose = append(onClose, bridge.Close)
			}
			if len(onClose) > 0 {
				conn.OnClose = func() {
					for _, f := range onClose {
						f()
					}
				}
			}

			mu.Lock()
			connections[clientID] = conn
			mu.Unlock()

		case "answer":
			clientID, _ := msg["client_id"].(string)
			sdp, _ := msg["sdp"].(string)
			mu.Lock()
			conn := connections[clientID]
			mu.Unlock()
			if conn == nil {
				return
			}
			// Pair-flow answers skip the bidirectional-verify decrypt —
			// SAS comparison (run from the data-channel OnOpen) is the
			// substitute, since the connector doesn't hold an access
			// code yet to feed encrypted_request with.
			if conn.PairingMode {
				if err := conn.HandlePairAnswer(sdp); err != nil {
					log.Printf("Failed to handle pair answer: %v", err)
				}
				return
			}
			encrypted, _ := msg["encrypted_request"].(string)
			if err := conn.HandleAnswer(sdp, encrypted); err != nil {
				log.Printf("Failed to handle answer: %v", err)
			}

		case "pair_request":
			clientID, _ := msg["client_id"].(string)
			if clientID == "" {
				return
			}
			conn, err := peer.HandlePairRequest(msg, signalingClient, id,
				pairing.DefaultTTYPrompt, cfg.verbose)
			if err != nil {
				log.Printf("Failed to handle pair request: %v", err)
				return
			}
			mu.Lock()
			connections[clientID] = conn
			mu.Unlock()

		case "candidate":
			clientID, _ := msg["client_id"].(string)
			candidateData, _ := msg["candidate"].(map[string]interface{})
			mu.Lock()
			conn := connections[clientID]
			mu.Unlock()
			if conn == nil {
				return
			}
			_ = conn.AddICECandidate(candidateData)

		case "error":
			log.Printf("Signaling error: %v", msg["message"])
		}
	})
}

// isAllMode reports whether the listener is running in `serve` (all-
// caps) mode rather than a single-cap mode. Used to gate the launcher
// hamburger: only the all-mode launcher tab gets a cap bar.
func isAllMode(cfg serveConfig) bool {
	n := 0
	if cfg.shellEnabled {
		n++
	}
	if cfg.filesEnabled {
		n++
	}
	if cfg.proxyEnabled {
		n++
	}
	return n > 1
}

// launcherCapBarItems composes the dropdown for the all-mode launcher
// tab. Shell appears only when --shell-max-sessions != 1 (otherwise
// the main tab IS the only shell and offering another would just hit
// the session limit). Files and Proxy always appear when enabled.
//
// Items render in this order. The bb-open-cap postMessage sends Path
// up to bootstrap.js, which composes the new-tab URL using the secret
// access code it owns.
func launcherCapBarItems(share *fileshare.FileShare, shellEnabled, proxyEnabled bool, shellMaxSessions int) []shellweb.CapBarItem {
	var items []shellweb.CapBarItem
	if shellEnabled && shellMaxSessions != 1 {
		items = append(items, shellweb.CapBarItem{Label: "Shell", Path: "/"})
	}
	if share != nil {
		items = append(items, shellweb.CapBarItem{Label: "Files", Path: "/files/"})
	}
	if proxyEnabled {
		items = append(items, shellweb.CapBarItem{Label: "Proxy", Path: "/proxy/"})
	}
	return items
}

// printSharingBlock prints the "Sharing:" status block listing each
// enabled cap with its salient configuration.
func printSharingBlock(cfg serveConfig, share *fileshare.FileShare) {
	fmt.Println("Sharing:")
	if cfg.shellEnabled {
		shellLine := "  • shell  ("
		if cfg.shellCmd != "" {
			shellLine += cfg.shellCmd
		} else {
			shellLine += "$SHELL or /bin/sh"
		}
		if cfg.shellMaxSessions == 0 {
			shellLine += ", unlimited concurrent sessions"
		} else if cfg.shellMaxSessions != 1 {
			shellLine += fmt.Sprintf(", max %d concurrent sessions", cfg.shellMaxSessions)
		}
		if cfg.shellMirror {
			shellLine += ", mirroring to console"
		}
		shellLine += ")"
		fmt.Println(shellLine)
	}
	if cfg.filesEnabled && share != nil {
		if share.Mode == fileshare.ModeSend {
			fmt.Printf("  • files  (%s — single file)\n", share.FileName)
		} else {
			fileLine := fmt.Sprintf("  • files  (%s", share.BasePath)
			if share.UploadEnabled {
				fileLine += ", uploads enabled"
			}
			fileLine += ")"
			fmt.Println(fileLine)
		}
	}
	if cfg.proxyEnabled {
		if cfg.target != "" {
			fmt.Printf("  • proxy  (%s)\n", cfg.target)
		} else {
			fmt.Println("  • proxy  (target chosen in browser)")
		}
	}
	fmt.Println()
}

// buildServeHTTPHandler composes the in-process HTTP-cap front-end.
//
// In all-mode: the launcher at "/" serves shellweb with the cap-bar
// injected (hamburger + dropdown). "/shell/" serves plain shell (no
// strip — that's a cap-specific tab opened via the hamburger). Files
// at "/files/", proxy landing at "/proxy/".
//
// In single-cap modes: the cap's handler is served at "/" directly,
// no strip, no prefix routing.
//
// Dynamic-target reverse proxying lives at the SWSP layer in
// streamtype.HTTPHandler, dispatched by httpDispatcher based on the
// connect path — those paths never reach this HTTP handler.
func buildServeHTTPHandler(share *fileshare.FileShare, shellEnabled, proxyEnabled bool, shellMaxSessions int, allMode bool) http.Handler {
	var fileH, shellH, proxyH http.Handler
	if share != nil {
		fileH = share.HTTPHandler()
	}
	if shellEnabled {
		shellH = shellweb.New().HTTPHandler()
	}
	if proxyEnabled {
		proxyH = proxyweb.LandingHandler()
	}

	// Single-cap fast path: serve the cap directly so relative URLs
	// in its HTML resolve cleanly.
	if !allMode {
		switch {
		case shellH != nil:
			return shellH
		case fileH != nil:
			return fileH
		case proxyH != nil:
			return proxyH
		}
	}

	// All-mode: build the launcher shell with the cap-bar strip
	// injected. The strip's dropdown anchors postMessage parent to
	// open new tabs (bootstrap.js handles the URL composition).
	launcherItems := launcherCapBarItems(share, shellEnabled, proxyEnabled, shellMaxSessions)
	launcherShell := shellweb.New(shellweb.WithCapBar(launcherItems)).HTTPHandler()

	mux := http.NewServeMux()
	capRoots := map[string]bool{}
	if shellH != nil {
		mux.Handle("/shell/", http.StripPrefix("/shell", shellH))
		capRoots["/shell"] = true
	}
	if fileH != nil {
		mux.Handle("/files/", http.StripPrefix("/files", fileH))
		capRoots["/files"] = true
	}
	if proxyH != nil {
		mux.Handle("/proxy/", http.StripPrefix("/proxy", proxyH))
		capRoots["/proxy"] = true
	}
	// "/" is the launcher tab: shell + strip. (When proxy-only or
	// files-only happens in some future all-mode variant where shell
	// is disabled, fall through to that cap at root.)
	switch {
	case shellEnabled:
		mux.Handle("/", launcherShell)
	case fileH != nil:
		mux.Handle("/", fileH)
	case proxyH != nil:
		mux.Handle("/", proxyH)
	default:
		mux.Handle("/", http.HandlerFunc(http.NotFound))
	}

	// Trailing-slash normalizer for the cap subpath roots. Without
	// this, "/proxy" → 301 → "/proxy/", and the redirect's
	// server-relative Location loses the browser's
	// /__device__/<sessionId>/ prefix.
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if capRoots[r.URL.Path] {
			r.URL.Path += "/"
		}
		mux.ServeHTTP(w, r)
	})
}
