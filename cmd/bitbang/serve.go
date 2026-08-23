package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/pion/webrtc/v4"
	qrcode "github.com/skip2/go-qrcode"

	"github.com/richlegrand/bitbang/internal/auth"
	"github.com/richlegrand/bitbang/internal/fileshare"
	"github.com/richlegrand/bitbang/internal/icehelper"
	"github.com/richlegrand/bitbang/internal/identity"
	"github.com/richlegrand/bitbang/internal/links"
	"github.com/richlegrand/bitbang/internal/peerset"
	"github.com/richlegrand/bitbang/internal/signaling"
	"github.com/richlegrand/bitbang/internal/videohelper"
)

// defaultServer is the signaling host every command defaults to.
const defaultServer = "bitba.ng"

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

	// caps is what this mode offers, named in the scope vocabulary links
	// uses. It is the only place the cap set is written down: what to
	// build, what to mount, what to advertise, and what a link may be
	// scoped to all derive from it. See capability.go.
	caps capSet

	// Shell-cap configuration (only meaningful when caps includes shell).
	shellCmd         string
	shellMaxSessions int
	shellMirror      bool

	// Files-cap configuration (only meaningful when caps includes files).
	filesPath   string
	filesUpload bool

	// ICE server path
	iceServersPath string
	iceServers     []webrtc.ICEServer
}

// runServe — `bitbang serve` — exposes shell + files + proxy. The
// launcher tab serves shell at `/`; the hamburger menu lets users open
// Files or Proxy in new browser tabs. Files-only / Shell-only /
// Proxy-only modes are dedicated subcommands; this mode is the "I want
// everything I can get from a single listener" entry point.
func runServe(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	cfg := serveConfig{caps: capsOf(links.ScopeShell, links.ScopeForward, links.ScopeFiles, links.ScopeProxy)}
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

// runServeShell — `bitbang serve shell` — exposes shell and raw TCP to CLI
// connectors. No hamburger; the entire browser tab is the shell.
func runServeShell(args []string) {
	fs := flag.NewFlagSet("serve shell", flag.ExitOnError)
	// forward rides with shell: `serve shell` offers port forwarding too,
	// and saying so here beats deriving it from where NewTCP is called.
	cfg := serveConfig{caps: capsOf(links.ScopeShell, links.ScopeForward)}
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
	cfg := serveConfig{caps: capsOf(links.ScopeFiles)}
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
	cfg := serveConfig{caps: capsOf(links.ScopeProxy)}
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
	fs.StringVar(&cfg.server, "server", defaultServer, "Signaling server hostname")
	fs.StringVar(&cfg.pin, "pin", "", "PIN to protect access")
	fs.BoolVar(&cfg.ephemeral, "ephemeral", false, "Use a temporary identity")
	fs.BoolVar(&cfg.verbose, "v", false, "Verbose logging")
	fs.BoolVar(&cfg.nocode, "nocode", false, "Disable code-exchange pairing (operator typed SAS); URL still works")
	fs.IntVar(&cfg.videoFD, "video-fd", -1, "Inherited socketpair FD to a video helper process (-1 = disabled)")
	fs.StringVar(&cfg.program, "program", "", "Identity program-name override; default is derived from the mode/target (key at ~/.bitbang/<program>/identity.pem)")
	fs.StringVar(&cfg.target, "target", "", "Fixed proxy target host:port (proxy-only mode); empty = dynamic from URL")
	fs.BoolVar(&cfg.forwardClientIP, "forward-client-ip", false, "Stamp the real browser IP as X-Forwarded-For (fixed-target mode); enable only when the backend trusts localhost for auth")
	fs.StringVar(&cfg.iceServersPath, "ice-servers", "", "Path to the custom JSON ICE server configuration file")
}

// registerShellFlags wires the shell-specific flags. Used by both
// `serve` (all-mode) and `serve shell` since both expose a shell.
func registerShellFlags(fs *flag.FlagSet, cfg *serveConfig) {
	fs.StringVar(&cfg.shellCmd, "shell-cmd", "", "Shell command to spawn (default: "+defaultShellLabel()+")")
	fs.IntVar(&cfg.shellMaxSessions, "shell-max-sessions", defaultShellMaxSessions, "Max concurrent shell sessions (0 = unlimited)")
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
	if cfg.caps.has(links.ScopeFiles) {
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

	if cfg.iceServersPath != "" {
		path, err := resolveFSPath(cfg.iceServersPath)
		if err != nil {
			fail("serve: %v", err)
		}
		fileBytes, err := os.ReadFile(path)
		if err != nil {
			fail("serve: read ICE server file: %v", err)
		}
		cfg.iceServers, err = icehelper.ParseUserICEFile(fileBytes)
		if err != nil {
			fail("serve: %s: %v", path, err)
		}
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

	signalingClient := signaling.NewClient(cfg.server, id)
	signalingClient.OwnICEServers = cfg.iceServers
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

	// The link table lives beside the identity, so it is per program:
	// `serve files -files /srv` and `serve all` derive different program
	// names and therefore have separate tables. An ephemeral identity has
	// no directory to keep one in, so it runs on the implicit row alone.
	linkState, err := newLinkState(program, offeredScopes(cfg), id.Code,
		cfg.ephemeral, signalingClient.CodeURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Link table error: %v\n", err)
		os.Exit(1)
	}

	// Both terminal streams go through holds so the console can pause
	// them: log lines (the std logger writes to stderr) and the shell
	// mirror (stdout). Without this a mirroring session scrolls a prompt
	// away before it can be read.
	logHold := newHoldWriter(os.Stderr)
	mirrorHold := newHoldWriter(os.Stdout)
	log.SetOutput(logHold)
	con := newConsole(logHold, mirrorHold)

	out := newDisplay(url)
	out.ready()
	printSharingBlock(os.Stdout, cfg, share)

	// PIN status / shell-without-PIN warning.
	if pinAuth.Required() {
		fmt.Println("PIN protection enabled.")
	} else if cfg.caps.has(links.ScopeShell) {
		fmt.Fprintf(os.Stderr, "%sWarning: anyone with this URL gets a shell and unrestricted TCP access from this machine.%s\n", out.bold, out.reset)
		fmt.Fprintln(os.Stderr, "  Use --pin <PIN> for a second factor, or pick a non-shell mode.")
	}

	if listing := linkState.listing(out.bold, out.reset); listing != "" {
		fmt.Print(listing)
		fmt.Print(consoleHint())
	}

	l := &listener{
		cfg:       cfg,
		id:        id,
		share:     share,
		shellArgv: shellArgv,
		pinAuth:   pinAuth,
		signaling: signalingClient,
		links:     linkState,
		video:     videoClient,
		peers:     peerset.New[*servePeer](),
		console:   con,
		mirror:    mirrorHold,
	}

	l.watch(out.bold, out.reset)
	con.Watch("console -- try help, or exit to resume output", func(line string) error {
		return l.runCommand(con, line)
	})

	firstReady := true
	signalingClient.OnReady = func() {
		if firstReady {
			firstReady = false
			// First-ready: URL/QR was already printed above (synchronously,
			// before Connect). Print just the pair code now that we've
			// learned it from the registered reply.
			out.pairCode(signalingClient.PairingCode)
			// Same reply carries the latest-release table. Once only:
			// a reconnect loop must not turn this into a nag.
			if notice := updateNotice(signalingClient.LatestVersions, version); notice != "" {
				out.updateAvailable(notice)
			}
			return
		}
		// Reconnect: re-print URL+QR (operator may have scrolled past it
		// during a long-running session) and the freshly-issued code.
		out.ready()
		out.pairCode(signalingClient.PairingCode)
	}

	signalingClient.Connect(l.handleSignal)
}

// resolveFSPath turns a user-supplied path into an absolute one,
// expanding a leading ~ against the home directory. filepath.Abs already
// returns an absolute path unchanged, so that is the whole job.
func resolveFSPath(path string) (string, error) {
	if path == "~" || strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve %s: %w", path, err)
		}
		return filepath.Join(home, strings.TrimPrefix(path[1:], "/")), nil
	}
	return filepath.Abs(path)
}
