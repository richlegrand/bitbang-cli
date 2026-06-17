// Package signaling handles the WebSocket connection to the BitBang signaling
// server. The device announces its UID and public key; the server binds
// hash(pubkey) == UID and accepts the registration. Proof of private-key
// possession is verified end-to-end by the browser (bidirectional verify on
// the WebRTC data channel), not by the signaling server.
package signaling

import (
	"crypto/tls"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/richlegrand/bitbang/internal/identity"
	"github.com/richlegrand/bitbang/internal/protocol"
)

// Message is a generic signaling message.
type Message map[string]interface{}

// Client manages the WebSocket connection to the signaling server.
type Client struct {
	ID       *identity.Identity
	Server   string // hostname, e.g. "bitba.ng"
	ServerWS string // full URL, e.g. "wss://bitba.ng/ws/device/<uid>"
	Verbose  bool

	// WantCode, when true, asks the server to issue a short 6-digit pairing
	// code at register time. The server returns it in the `registered`
	// reply; we expose it on PairingCode for the caller to display. Setting
	// this is the listener's opt-in to the code-exchange pairing flow —
	// without it, connectors can only reach the listener via the full
	// 22-character UID URL.
	WantCode bool

	// PairingCode is the 6-digit code issued by the server when WantCode
	// was true. Empty when WantCode was false, when the server doesn't
	// support pairing, or before the first successful register.
	PairingCode string

	// OnReady, if set, is called after each successful (re)registration
	// with the signaling server. Callers use it to (re)print user-visible
	// info — URL, QR code, etc. — that should resurface after a reconnect,
	// so the operator doesn't have to scroll back to grab the URL.
	// When unset, connectOnce falls back to a one-line "Ready: ..." log.
	OnReady func()

	conn *websocket.Conn

	// writeMu serializes WriteJSON. gorilla/websocket forbids concurrent
	// writes, and we write from both the message-handler goroutine (offers)
	// and pion's OnICECandidate callback (trickle candidates).
	writeMu sync.Mutex
}

// NewClient creates a signaling client for the given server and identity.
func NewClient(server string, id *identity.Identity) *Client {
	ws := fmt.Sprintf("wss://%s/ws/device/%s", server, id.UID)
	return &Client{
		ID:       id,
		Server:   server,
		ServerWS: ws,
	}
}

// URL returns the canonical user-facing URL for this device:
// ``https://<server>/<uid>[?debug]#<code>``. Single source of truth — all
// consumers (CLI banners, reconnect prints, downstream wrappers) should
// read this rather than reconstruct it from Server/ID.UID/ID.Code, since
// the exact shape (query params, fragment placement) is the protocol's
// concern, not theirs. The fragment carries the access code, which the
// signaling server never sees because browsers don't send fragments.
func (c *Client) URL(debug bool) string {
	s := "https://" + c.Server + "/" + c.ID.UID
	if debug {
		s += "?debug"
	}
	return s + "#" + c.ID.Code
}

// Connect connects to the signaling server and registers. On success, it
// calls handler for each incoming message. Reconnects automatically on
// disconnection.
func (c *Client) Connect(handler func(msg Message)) {
	for {
		err := c.connectOnce(handler)
		if err != nil {
			log.Printf("Connection lost: %v, retrying in 3s...", err)
			time.Sleep(3 * time.Second)
		}
	}
}

func (c *Client) connectOnce(handler func(msg Message)) error {
	if c.Verbose {
		log.Printf("Connecting to %s...", c.ServerWS)
	}

	dialer := &websocket.Dialer{
		TLSClientConfig:  &tls.Config{InsecureSkipVerify: true},
		HandshakeTimeout: 10 * time.Second,
	}
	conn, _, err := dialer.Dial(c.ServerWS, nil)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer conn.Close()
	c.conn = conn

	// Register
	if err := c.register(); err != nil {
		return fmt.Errorf("register: %w", err)
	}
	// Single-word post-register marker so a watcher (test harness, log
	// scraper, ops dashboard) has a reliable signal that registration
	// completed. The URL is already printed prominently above (or by the
	// caller via OnReady on reconnect), so re-emitting it here would
	// just be noise.
	log.Printf("Ready")
	if c.OnReady != nil {
		c.OnReady()
	}

	// Message loop
	for {
		var msg Message
		if err := conn.ReadJSON(&msg); err != nil {
			return fmt.Errorf("read: %w", err)
		}
		handler(msg)
	}
}

// Send sends a JSON message to the signaling server.
func (c *Client) Send(msg Message) error {
	if c.conn == nil {
		return fmt.Errorf("not connected")
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return c.conn.WriteJSON(msg)
}

func (c *Client) register() error {
	// Send registration with public key and protocol version. want_code is
	// additive in v3.x — the server returns a 6-digit code in the
	// registered reply when both we set it and the server has the pairing
	// table configured. Old servers ignore the field, new servers without
	// pairing configured return a bare registered.
	reg := Message{
		"type":       "register",
		"uid":        c.ID.UID,
		"public_key": c.ID.PublicB64,
		"protocol":   protocol.ProtocolVersion,
	}
	if c.WantCode {
		reg["want_code"] = true
	}
	c.writeMu.Lock()
	err := c.conn.WriteJSON(reg)
	c.writeMu.Unlock()
	if err != nil {
		return fmt.Errorf("send register: %w", err)
	}

	var msg Message
	if err := c.conn.ReadJSON(&msg); err != nil {
		return fmt.Errorf("read: %w", err)
	}

	switch msg["type"] {
	case "registered":
		// Capture the pairing code if the server returned one. Reset to
		// empty on each reconnect first — a server that loses its pairing
		// table (process restart) re-issues a fresh code, and any stale
		// code we were holding would mislead the operator.
		c.PairingCode = ""
		if code, ok := msg["code"].(string); ok && code != "" {
			c.PairingCode = code
		}
		return nil

	case "error":
		errMsg, _ := msg["message"].(string)
		if errMsg == "protocol_too_old" {
			fmt.Println("\nPlease upgrade bitbangproxy:")
			fmt.Println("  Download latest from https://github.com/richlegrand/bitbangproxy/releases")
			log.Fatal("Protocol version too old")
		}
		return fmt.Errorf("server error: %v", errMsg)

	default:
		return fmt.Errorf("unexpected message type: %v", msg["type"])
	}
}
