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
	"os"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"golang.org/x/term"

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

	// OnPreempted fires once when the signaling server reports another
	// instance has registered with this UID and taken over the slot. The
	// library has already stopped reconnect by the time this is called —
	// the host application decides what to do (log, exit, restart with a
	// different identity, etc.). Defaults to a library-supplied function
	// that logs a single line; host can replace to override the message,
	// exit the process, suppress entirely (assign func(){}), etc.
	//
	// This callback is the *only* user-visible aspect of preemption. The
	// reconnect-storm prevention (one-way preempted flag → Connect loop
	// returns) is internal and not configurable: without it, two
	// instances racing for the same UID would ping-pong forever.
	OnPreempted func()

	conn *websocket.Conn

	// writeMu serializes WriteJSON. gorilla/websocket forbids concurrent
	// writes, and we write from both the message-handler goroutine (offers)
	// and pion's OnICECandidate callback (trickle candidates).
	writeMu sync.Mutex

	// preempted is set true exactly once when the server reports another
	// instance has taken over this UID. It is the storm-breaker: the
	// reconnect loop in Connect checks this and returns instead of
	// trying again. One-way transition; never cleared.
	preempted bool
}

// NewClient creates a signaling client for the given server and identity.
// OnPreempted is initialized to the library default (one log line); host
// can replace before calling Connect to override.
func NewClient(server string, id *identity.Identity) *Client {
	ws := fmt.Sprintf("wss://%s/ws/device/%s", server, id.UID)
	return &Client{
		ID:          id,
		Server:      server,
		ServerWS:    ws,
		OnPreempted: defaultOnPreempted,
	}
}

// defaultOnPreempted is the library-default OnPreempted callback. Logs
// a single line and returns; the storm-breaker (preempted flag check in
// Connect) is what actually stops the reconnect loop. CLI binaries
// override this with a print-and-exit closure; library users typically
// log into their own logger and/or trigger an application-level reset.
func defaultOnPreempted() {
	log.Printf("Another instance with the same UID has registered. Stopping reconnect.")
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
// disconnection — unless the server reports another instance has
// preempted us, in which case it returns and stays returned.
func (c *Client) Connect(handler func(msg Message)) {
	for {
		err := c.connectOnce(handler)
		if c.preempted {
			// Storm-breaker. Another instance has this UID; reconnecting
			// would just kick them out and trigger their reconnect, ad
			// infinitum. The OnPreempted callback has already fired
			// inside the message loop; nothing more to do here.
			return
		}
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
	// completed. Suppressed on an interactive terminal — the operator
	// already sees the URL block and pair code; a stray "Ready" looks
	// like log noise. Test harnesses pipe stderr, so isatty is false
	// for them and they still see the marker.
	if !term.IsTerminal(int(os.Stderr.Fd())) {
		fmt.Fprintln(os.Stderr, "Ready")
	}
	if c.OnReady != nil {
		c.OnReady()
	}

	// Message loop
	for {
		var msg Message
		if err := conn.ReadJSON(&msg); err != nil {
			return fmt.Errorf("read: %w", err)
		}

		// Intercept the typed preempted error before handing the message
		// to the caller. We don't want the caller to see this — it's a
		// signaling-layer concern, and the caller's handler probably
		// doesn't know what to do with it.
		if mtype, _ := msg["type"].(string); mtype == "error" {
			if reason, _ := msg["message"].(string); reason == "preempted" {
				c.preempted = true
				if c.OnPreempted != nil {
					c.OnPreempted()
				}
				return fmt.Errorf("preempted by another instance")
			}
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
