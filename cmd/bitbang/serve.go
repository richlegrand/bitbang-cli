package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"sync"

	qrcode "github.com/skip2/go-qrcode"
	"golang.org/x/term"

	"github.com/richlegrand/bitbang/internal/auth"
	"github.com/richlegrand/bitbang/internal/fileshare"
	"github.com/richlegrand/bitbang/internal/identity"
	"github.com/richlegrand/bitbang/internal/peer"
	"github.com/richlegrand/bitbang/internal/session"
	"github.com/richlegrand/bitbang/internal/shellweb"
	"github.com/richlegrand/bitbang/internal/signaling"
	"github.com/richlegrand/bitbang/internal/streamtype"
	"github.com/richlegrand/bitbang/internal/tabweb"
)

// runServe implements `bitbang serve` — the unified multi-cap listener.
// Each cap is opt-in via a flag; the resulting listener exposes the
// union of selected caps over one signaling identity. With multiple
// caps the browser sees a tabbed UI; with a single cap the listener
// reduces to the equivalent single-purpose subcommand (and the page
// goes straight to that cap's UI, no tab strip).
//
//	bitbang serve --files PATH                  # = bitbang fileshare PATH
//	bitbang serve --shell                       # = bitbang shell
//	bitbang serve --files PATH --shell          # combo: tabbed browser
func runServe(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	server := fs.String("server", "bitba.ng", "Signaling server hostname")
	pin := fs.String("pin", "", "PIN to protect access")
	ephemeral := fs.Bool("ephemeral", false, "Use a temporary identity")
	verbose := fs.Bool("v", false, "Verbose logging")

	// File-cap flags.
	filesPath := fs.String("files", "", "Share a file or directory")
	filesUpload := fs.Bool("files-upload", false, "Allow uploads to the shared directory")

	// Shell-cap flags.
	shellEnabled := fs.Bool("shell", false, "Expose a remote shell")
	shellCmd := fs.String("shell-cmd", "", "Shell command to spawn (default: $SHELL or /bin/sh)")
	shellMaxSessions := fs.Int("shell-max-sessions", 1, "Max concurrent shell sessions (0 = unlimited)")
	shellMirror := fs.Bool("shell-mirror", true, "Mirror shell output to listener console")

	fs.Parse(reorderArgs(fs, args))

	// Sanity-check the cap selection: at least one cap must be enabled.
	if *filesPath == "" && !*shellEnabled {
		fmt.Fprintln(os.Stderr, "bitbang serve: no caps enabled — pass at least one of --files PATH, --shell")
		os.Exit(2)
	}

	pinAuth := auth.New(*pin)

	id, err := identity.Load("bitbang-serve", *ephemeral)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Identity error: %v\n", err)
		os.Exit(1)
	}

	// Build the file share once (state shared across all sessions —
	// reads-only by design, plus an upload flag).
	var share *fileshare.FileShare
	if *filesPath != "" {
		share, err = fileshare.New(*filesPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Cannot share %q: %v\n", *filesPath, err)
			os.Exit(1)
		}
		share.UploadEnabled = *filesUpload
	}

	// Default argv for shell. Empty means ShellHandler falls back to
	// $SHELL → /bin/sh at spawn time.
	var shellArgv []string
	if *shellCmd != "" {
		shellArgv = []string{*shellCmd}
	}

	// Build the HTTP front-end. Single-cap → that cap's handler at
	// root. Multi-cap → tab UI at root + each cap's handler mounted
	// under a subpath.
	httpFront := buildServeHTTPHandler(share, *shellEnabled)

	signalingClient := signaling.NewClient(*server, id)
	signalingClient.Verbose = *verbose
	url := signalingClient.URL(*verbose)

	printReady := func() {
		if qr, err := qrcode.New(url, qrcode.Medium); err == nil {
			fmt.Println(qr.ToSmallString(false))
		}
		fmt.Printf("URL: %s\n", url)
	}

	fmt.Println(banner)
	fmt.Printf("v%s\n\n", version)
	printReady()

	// Status block — what's actually enabled.
	if share != nil {
		if share.Mode == fileshare.ModeSend {
			fmt.Printf("Sharing file: %s\n", share.FileName)
		} else {
			fmt.Printf("Sharing directory: %s\n", share.BasePath)
			if share.UploadEnabled {
				fmt.Printf("Uploads enabled\n")
			}
		}
	}
	if *shellEnabled {
		if *shellCmd != "" {
			fmt.Printf("Shell command: %s\n", *shellCmd)
		} else {
			fmt.Printf("Shell command: default ($SHELL or /bin/sh)\n")
		}
		if *shellMaxSessions == 0 {
			fmt.Printf("Max concurrent shell sessions: unlimited\n")
		} else {
			fmt.Printf("Max concurrent shell sessions: %d\n", *shellMaxSessions)
		}
		if *shellMirror {
			fmt.Printf("Mirroring shell output to this console (--shell-mirror=false to disable)\n")
			if term.IsTerminal(int(os.Stdout.Fd())) {
				streamtype.EnableListenerResizeTracking(int(os.Stdout.Fd()))
			}
		}
	}
	if pinAuth.Required() {
		fmt.Printf("PIN protection enabled\n")
	} else if *shellEnabled {
		// No-PIN warning only applies to shell (the cap with arbitrary
		// command execution). File-only listeners without a PIN are a
		// normal use case (drop a folder, send the URL).
		fmt.Fprintln(os.Stderr, "WARNING: no PIN set — anyone with the URL gets a shell on this machine.")
		fmt.Fprintln(os.Stderr, "         Add --pin <PIN> to require authentication.")
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

			// Build per-session handlers. File handler is per-session
			// (its streams map is) but the FileShare backing it is
			// shared. Shell handler is per-session. HTTPLocal wraps
			// the unified HTTP front-end so the browser flow works.
			var handlers []streamtype.StreamHandler
			if share != nil {
				handlers = append(handlers, streamtype.NewFile(share, *verbose))
			}
			var shellHandler *streamtype.ShellHandler
			if *shellEnabled {
				shellHandler = streamtype.NewShell(shellArgv, *verbose)
				shellHandler.MaxConcurrent = *shellMaxSessions
				if *shellMirror {
					shellHandler.StdoutMirror = os.Stdout
					shellHandler.StderrMirror = os.Stderr
				}
				handlers = append(handlers, shellHandler)
			}
			handlers = append(handlers, streamtype.NewHTTPLocal(httpFront, *verbose))

			var sess *session.Session

			conn, err := peer.HandleRequest(msg, signalingClient, id, func(data []byte) {
				if sess != nil {
					sess.HandleMessage(data)
				}
			}, *verbose)
			if err != nil {
				log.Printf("Failed to create peer connection: %v", err)
				return
			}

			sess = session.New(conn.DC, pinAuth, *verbose, handlers...)

			// Kill any shell processes when this connection's data
			// channel closes — without this they outlive the browser
			// tab and keep holding their max-sessions slot.
			if shellHandler != nil {
				conn.OnClose = shellHandler.KillAll
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

// buildServeHTTPHandler composes the HTTP-cap front-end from the set of
// enabled caps. Single-cap listeners get that cap's handler at root
// (so iframe paths like "api/list" or "shell.js" resolve cleanly).
// Multi-cap listeners get a tab UI at root and each cap mounted under
// its own subpath.
func buildServeHTTPHandler(share *fileshare.FileShare, shellEnabled bool) http.Handler {
	var fileH, shellH http.Handler
	if share != nil {
		fileH = share.HTTPHandler()
	}
	if shellEnabled {
		shellH = shellweb.New().HTTPHandler()
	}

	// Single-cap shortcuts: skip the tab UI entirely.
	switch {
	case fileH != nil && shellH == nil:
		return fileH
	case fileH == nil && shellH != nil:
		return shellH
	}

	// Multi-cap: tab strip at root, each cap under a subpath. The
	// relative URLs in browse.html / shell.html (e.g. fetch
	// "api/list", <script src="shell.js">) resolve against the
	// iframe's base URL ("/files/" or "/shell/"), so they tunnel
	// back as "/files/api/list" / "/shell/shell.js" — which the
	// StripPrefix handlers below convert to the same shape the
	// per-cap handler already expects.
	var caps []tabweb.Cap
	mux := http.NewServeMux()
	if fileH != nil {
		caps = append(caps, tabweb.Cap{Name: "files", Label: "Files", URL: "/files/"})
		mux.Handle("/files/", http.StripPrefix("/files", fileH))
	}
	if shellH != nil {
		caps = append(caps, tabweb.Cap{Name: "shell", Label: "Shell", URL: "/shell/"})
		mux.Handle("/shell/", http.StripPrefix("/shell", shellH))
	}
	mux.Handle("/", tabweb.New(caps))
	return mux
}
