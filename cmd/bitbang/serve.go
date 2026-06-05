package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"sync"

	qrcode "github.com/skip2/go-qrcode"

	"github.com/richlegrand/bitbang/internal/auth"
	"github.com/richlegrand/bitbang/internal/fileshare"
	"github.com/richlegrand/bitbang/internal/identity"
	"github.com/richlegrand/bitbang/internal/peer"
	"github.com/richlegrand/bitbang/internal/proxyweb"
	"github.com/richlegrand/bitbang/internal/session"
	"github.com/richlegrand/bitbang/internal/shellweb"
	"github.com/richlegrand/bitbang/internal/signaling"
	"github.com/richlegrand/bitbang/internal/streamtype"
	"github.com/richlegrand/bitbang/internal/videohelper"
)

// serveConfig is the assembled per-mode configuration that the shared
// listener loop in startListener uses. Each mode (all / shell / files /
// proxy) populates the fields relevant to it and leaves the rest zero.
type serveConfig struct {
	// Shared flags (all modes).
	server    string
	pin       string
	ephemeral bool
	verbose   bool

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

// runServeProxy — `bitbang serve proxy` — exposes a dynamic-target
// HTTP reverse proxy. Landing page asks for a target URL; entered
// targets open in new browser tabs that the SWSP layer routes through
// streamtype.HTTPHandler.
func runServeProxy(args []string) {
	fs := flag.NewFlagSet("serve proxy", flag.ExitOnError)
	cfg := serveConfig{proxyEnabled: true}
	registerSharedFlags(fs, &cfg)
	fs.Parse(reorderArgs(fs, args))
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
	fs.IntVar(&cfg.videoFD, "video-fd", -1, "Inherited socketpair FD to a video helper process (-1 = disabled)")
	fs.StringVar(&cfg.program, "program", "bitbang", "Identity program name (~/.bitbang/<program>/identity.pem)")
	fs.StringVar(&cfg.target, "target", "", "Fixed proxy target host:port (proxy-only mode); empty = dynamic from URL")
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

	id, err := identity.Load(cfg.program, cfg.ephemeral)
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
	url := signalingClient.URL(cfg.verbose)

	printReady := func() {
		if qr, err := qrcode.New(url, qrcode.Medium); err == nil {
			fmt.Println(qr.ToSmallString(false))
		}
		fmt.Printf("URL: %s\n", url)
	}

	fmt.Println(banner)
	fmt.Printf("v%s\n\n", version)
	printReady()
	printSharingBlock(cfg, share)

	// PIN status / shell-without-PIN warning.
	if pinAuth.Required() {
		fmt.Println("PIN protection enabled.")
	} else if cfg.shellEnabled {
		fmt.Fprintln(os.Stderr, "⚠ Anyone with this URL gets a shell on this machine.")
		fmt.Fprintln(os.Stderr, "  Use --pin <PIN> for a second factor, or pick a non-shell mode.")
	}
	fmt.Println()

	var mu sync.Mutex
	connections := make(map[string]*peer.Connection)

	firstReady := true
	signalingClient.OnReady = func() {
		if firstReady {
			firstReady = false
			return
		}
		printReady()
	}

	signalingClient.Connect(func(msg signaling.Message) {
		msgType, _ := msg["type"].(string)

		switch msgType {
		case "request":
			clientID, _ := msg["client_id"].(string)

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
				handlers = append(handlers,
					streamtype.NewHTTPProxy(cfg.target, id.UID, cfg.server, cfg.verbose))
			} else {
				localHTTP := streamtype.NewHTTPLocal(httpFront, cfg.verbose)
				var proxyHTTP streamtype.StreamHandler
				if cfg.proxyEnabled {
					proxyHTTP = streamtype.NewHTTPProxy("", id.UID, cfg.server, cfg.verbose)
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

			// Per-connection teardown, run when the data channel closes.
			var onClose []func()
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
			encrypted, _ := msg["encrypted_request"].(string)
			mu.Lock()
			conn := connections[clientID]
			mu.Unlock()
			if conn == nil {
				return
			}
			if err := conn.HandleAnswer(sdp, encrypted); err != nil {
				log.Printf("Failed to handle answer: %v", err)
			}

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
	fmt.Println()
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
		fmt.Println("  • proxy  (any local HTTP service)")
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
