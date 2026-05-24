package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"runtime"
	"runtime/debug"
	"sync"

	qrcode "github.com/skip2/go-qrcode"

	"github.com/richlegrand/bitbang/internal/auth"
	"github.com/richlegrand/bitbang/internal/identity"
	"github.com/richlegrand/bitbang/internal/peer"
	"github.com/richlegrand/bitbang/internal/session"
	"github.com/richlegrand/bitbang/internal/signaling"
	"github.com/richlegrand/bitbang/internal/streamtype"
)

// runProxy implements `bitbang proxy`. Reproduces the old bitbangproxy
// behavior — same flags, same wire protocol — but built on the new
// session+streamtype dispatch.
func runProxy(args []string) {
	fs := flag.NewFlagSet("proxy", flag.ExitOnError)
	server := fs.String("server", "bitba.ng", "Signaling server hostname")
	target := fs.String("target", "", "Local server to proxy (e.g. localhost:8080). If not set, target is extracted from the URL.")
	pin := fs.String("pin", "", "PIN to protect proxy access")
	ephemeral := fs.Bool("ephemeral", false, "Use a temporary identity (not saved to disk)")
	verbose := fs.Bool("v", false, "Verbose logging")
	fs.Parse(args)

	pinAuth := auth.New(*pin)

	id, err := identity.Load("bitbangproxy", *ephemeral)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Identity error: %v\n", err)
		os.Exit(1)
	}

	client := signaling.NewClient(*server, id)
	client.Verbose = *verbose
	url := client.URL(*verbose)

	// printReady is the user-facing "here's how to reach me" display. We
	// call it once upfront after the banner and again on every reconnect
	// (wired via client.OnReady below), so the operator can always scroll
	// up a few lines to find the URL and QR after a network blip.
	printReady := func() {
		if qr, err := qrcode.New(url, qrcode.Medium); err == nil {
			fmt.Println(qr.ToSmallString(false))
		}
		fmt.Printf("URL: %s\n", url)
	}

	fmt.Println(banner)
	fmt.Printf("v%s\n", version)
	if *verbose {
		if info, ok := debug.ReadBuildInfo(); ok {
			for _, dep := range info.Deps {
				fmt.Printf("  %s %s\n", dep.Path, dep.Version)
			}
		}
		fmt.Printf("  %s\n", runtime.Version())
	}
	fmt.Println()

	printReady()

	if *target != "" {
		fmt.Printf("Proxying: %s\n", *target)
	} else {
		fmt.Printf("Proxying: dynamic (target from URL)\n")
	}
	if pinAuth.Required() {
		fmt.Printf("PIN protection enabled\n")
	}
	fmt.Println()

	var mu sync.Mutex
	connections := make(map[string]*peer.Connection)

	// Skip the first OnReady — we already printed URL+QR above. Every
	// subsequent invocation (i.e. every reconnect) re-prints the block.
	firstReady := true
	client.OnReady = func() {
		if firstReady {
			firstReady = false
			return
		}
		printReady()
	}

	client.Connect(func(msg signaling.Message) {
		msgType, _ := msg["type"].(string)

		switch msgType {
		case "request":
			clientID, _ := msg["client_id"].(string)
			// peer.HandleRequest logs the "Connection request from <id>
			// (browser_ip=<ip>)" line for us — no need to duplicate here.

			// Build the per-session handler set. HTTP and WS share state
			// via the HTTPHandler's ResolveTarget (so WS streams use the
			// same dynamic-target logic).
			httpHandler := streamtype.NewHTTPProxy(*target, id.UID, *server, *verbose)
			wsHandler := streamtype.NewWebSocket(httpHandler, *verbose)

			var sess *session.Session

			conn, err := peer.HandleRequest(msg, client, id, func(data []byte) {
				if sess != nil {
					sess.HandleMessage(data)
				}
			}, *verbose)
			if err != nil {
				log.Printf("Failed to create peer connection: %v", err)
				return
			}

			sess = session.New(conn.DC, pinAuth, *verbose, httpHandler, wsHandler)

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
				if *verbose {
					log.Printf("Answer for unknown client: %s", clientID)
				}
				return
			}
			if err := conn.HandleAnswer(sdp, encrypted); err != nil {
				log.Printf("Failed to handle answer for %s: %v", clientID, err)
			}

		case "candidate":
			clientID, _ := msg["client_id"].(string)
			candidateData, _ := msg["candidate"].(map[string]interface{})

			mu.Lock()
			conn := connections[clientID]
			mu.Unlock()
			if conn == nil {
				if *verbose {
					log.Printf("Candidate for unknown client: %s", clientID)
				}
				return
			}
			if err := conn.AddICECandidate(candidateData); err != nil {
				if *verbose {
					log.Printf("Failed to add candidate for %s: %v", clientID, err)
				}
			}

		case "error":
			log.Printf("Signaling error: %v", msg["message"])

		default:
			if *verbose {
				log.Printf("Unknown message type: %s", msgType)
			}
		}
	})
}
