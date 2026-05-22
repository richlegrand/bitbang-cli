package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"sync"

	qrcode "github.com/skip2/go-qrcode"

	"github.com/richlegrand/bitbang/internal/auth"
	"github.com/richlegrand/bitbang/internal/fileshare"
	"github.com/richlegrand/bitbang/internal/identity"
	"github.com/richlegrand/bitbang/internal/peer"
	"github.com/richlegrand/bitbang/internal/session"
	"github.com/richlegrand/bitbang/internal/signaling"
	"github.com/richlegrand/bitbang/internal/streamtype"
)

// runFileshare implements `bitbang fileshare <path>`. Shares a file (send
// mode) or directory (browse mode) over WebRTC. Wire-compatible with the
// Python fileshare so the same browser UI works against either listener.
func runFileshare(args []string) {
	fs := flag.NewFlagSet("fileshare", flag.ExitOnError)
	server := fs.String("server", "bitba.ng", "Signaling server hostname")
	pin := fs.String("pin", "", "PIN to protect access")
	ephemeral := fs.Bool("ephemeral", false, "Use a temporary identity")
	upload := fs.Bool("upload", false, "Allow file uploads (browse mode only)")
	verbose := fs.Bool("v", false, "Verbose logging")
	fs.Parse(args)

	if fs.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "Usage: bitbang fileshare <path>")
		os.Exit(2)
	}
	sharePath := fs.Arg(0)

	share, err := fileshare.New(sharePath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Cannot share %q: %v\n", sharePath, err)
		os.Exit(1)
	}
	share.UploadEnabled = *upload

	pinAuth := auth.New(*pin)

	id, err := identity.Load("bitbang-fileshare", *ephemeral)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Identity error: %v\n", err)
		os.Exit(1)
	}

	url := fmt.Sprintf("https://%s/%s", *server, id.UID)
	if *verbose {
		url += "?debug"
	}

	fmt.Println(banner)
	fmt.Printf("v%s\n\n", version)

	if qr, err := qrcode.New(url, qrcode.Medium); err == nil {
		fmt.Println(qr.ToSmallString(false))
	}

	fmt.Printf("URL: %s\n", url)
	if share.Mode == fileshare.ModeSend {
		fmt.Printf("Sharing file: %s\n", share.FileName)
	} else {
		fmt.Printf("Sharing directory: %s\n", share.BasePath)
		if share.UploadEnabled {
			fmt.Printf("Uploads enabled\n")
		}
	}
	if pinAuth.Required() {
		fmt.Printf("PIN protection enabled\n")
	}
	fmt.Println()

	var mu sync.Mutex
	connections := make(map[string]*peer.Connection)

	client := signaling.NewClient(*server, id)
	client.Verbose = *verbose

	client.Connect(func(msg signaling.Message) {
		msgType, _ := msg["type"].(string)

		switch msgType {
		case "request":
			clientID, _ := msg["client_id"].(string)
			log.Printf("Connection request from %s", clientID)

			// Fileshare exposes two stream types over the same session:
			//   - http: browser UI (browse.html/send.html + /api/list,
			//     /api/download, /api/upload, etc.) — wire-compatible
			//     with the Python fileshare.
			//   - file: native SWSP file ops for `bitbang cp`.
			httpHandler := streamtype.NewHTTPLocal(share.HTTPHandler(), *verbose)
			fileHandler := streamtype.NewFile(share, *verbose)

			var sess *session.Session

			conn, err := peer.HandleRequest(msg, client, func(data []byte) {
				if sess != nil {
					sess.HandleMessage(data)
				}
			}, *verbose)
			if err != nil {
				log.Printf("Failed to create peer connection: %v", err)
				return
			}

			sess = session.New(conn.DC, pinAuth, *verbose, httpHandler, fileHandler)

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
			if err := conn.HandleAnswer(sdp); err != nil {
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
