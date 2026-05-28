package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"sync"

	qrcode "github.com/skip2/go-qrcode"

	"github.com/richlegrand/bitbang/internal/auth"
	"github.com/richlegrand/bitbang/internal/identity"
	"github.com/richlegrand/bitbang/internal/peer"
	"github.com/richlegrand/bitbang/internal/session"
	"github.com/richlegrand/bitbang/internal/signaling"
	"github.com/richlegrand/bitbang/internal/streamtype"
)

// runShell implements `bitbang shell`. Exposes a shell on the device,
// reachable via `bitbang connect URL` (CLI) or — once the browser tab
// ships — directly from any browser. Cap advertised: "shell".
//
// PIN is recommended: shell access grants arbitrary command execution
// on the device. The URL fragment access code is the first line of
// defense; PIN adds a second factor for shared links.
func runShell(args []string) {
	fs := flag.NewFlagSet("shell", flag.ExitOnError)
	server := fs.String("server", "bitba.ng", "Signaling server hostname")
	cmdFlag := fs.String("cmd", "", "Command to spawn (default: $SHELL or /bin/sh)")
	pin := fs.String("pin", "", "PIN to protect shell access")
	ephemeral := fs.Bool("ephemeral", false, "Use a temporary identity")
	verbose := fs.Bool("v", false, "Verbose logging")
	fs.Parse(reorderArgs(fs, args))

	// Default argv: --cmd if given, otherwise empty so ShellHandler
	// falls back to $SHELL → /bin/sh at spawn time.
	var defaultArgv []string
	if *cmdFlag != "" {
		defaultArgv = []string{*cmdFlag}
	}

	pinAuth := auth.New(*pin)

	id, err := identity.Load("bitbang-shell", *ephemeral)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Identity error: %v\n", err)
		os.Exit(1)
	}

	client := signaling.NewClient(*server, id)
	client.Verbose = *verbose
	url := client.URL(*verbose)

	printReady := func() {
		if qr, err := qrcode.New(url, qrcode.Medium); err == nil {
			fmt.Println(qr.ToSmallString(false))
		}
		fmt.Printf("URL: %s\n", url)
	}

	fmt.Println(banner)
	fmt.Printf("v%s\n\n", version)

	printReady()

	if *cmdFlag != "" {
		fmt.Printf("Shell command: %s\n", *cmdFlag)
	} else {
		fmt.Printf("Shell command: default ($SHELL or /bin/sh)\n")
	}
	if pinAuth.Required() {
		fmt.Printf("PIN protection enabled\n")
	} else {
		fmt.Fprintln(os.Stderr, "WARNING: no PIN set — anyone with the URL gets a shell on this machine.")
		fmt.Fprintln(os.Stderr, "         Add --pin <PIN> to require authentication.")
	}
	fmt.Println()

	var mu sync.Mutex
	connections := make(map[string]*peer.Connection)

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

			// One ShellHandler per session — the handler keeps a
			// per-stream map so a single session could host multiple
			// concurrent shells (though the current CLI client only
			// opens one).
			shellHandler := streamtype.NewShell(defaultArgv, *verbose)

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

			sess = session.New(conn.DC, pinAuth, *verbose, shellHandler)

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
