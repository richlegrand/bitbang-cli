// Package signaling handles the WebSocket connection to the BitBang signaling
// server. The device announces its UID and public key; the server binds
// hash(pubkey) == UID and accepts the registration. Proof of private-key
// possession is verified end-to-end by the browser (bidirectional verify on
// the WebRTC data channel), not by the signaling server.
package signaling

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"golang.org/x/term"

	"github.com/pion/webrtc/v4"

	"github.com/richlegrand/bitbang/internal/identity"
	"github.com/richlegrand/bitbang/internal/protocol"
)

// Message is a generic signaling message.
type Message map[string]interface{}

// ErrPreempted means the server accepted another registration for this UID.
var ErrPreempted = errors.New("signaling registration was preempted")

// Client manages the WebSocket connection to the signaling server.
type Client struct {
	ID       *identity.Identity
	Server   string // hostname, e.g. "bitba.ng"
	ServerWS string // full URL, e.g. "wss://bitba.ng/ws/device/<uid>"
	Verbose  bool

	// LatestVersions is the newest released version of each BitBang
	// client project, keyed by product, as reported on the registered
	// reply. nil when the server tracks nothing or predates the field.
	// Read after Connect returns.
	LatestVersions map[string]string

	// OwnICEServers is a pre-parsed operator-supplied ICE server config
	// (--ice-servers). Empty means "let the server decide"; a slice is
	// already nilable, so every caller that ignores this field is fine.
	OwnICEServers []webrtc.ICEServer

	// WantCode, when true, asks the server to issue a short 6-digit pairing
	// code at register time. The server returns it in the `registered`
	// reply; we expose it on PairingCode for the caller to display. Setting
	// this is the listener's opt-in to the code-exchange pairing flow —
	// without it, connectors can only reach the listener via the full
	// 22-character UID URL.
	WantCode bool

	// codeIssued carries a renewed pairing code from the read loop back
	// to whoever asked. Buffered by one so an answer nobody is waiting
	// for does not block the loop.
	codeIssued chan string

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

	ctx    context.Context
	cancel context.CancelFunc
	connMu sync.RWMutex
	conn   *websocket.Conn

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
	ctx, cancel := context.WithCancel(context.Background())
	return &Client{
		ID:          id,
		Server:      server,
		ServerWS:    ws,
		OnPreempted: defaultOnPreempted,
		codeIssued:  make(chan string, 1),
		ctx:         ctx,
		cancel:      cancel,
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
// "https://<server>/<uid>#<code>[!<flags>]". All
// consumers (CLI banners, reconnect prints, downstream wrappers) should
// read this rather than reconstruct it from Server/ID.UID/ID.Code, since
// the exact shape (fragment placement, flag syntax) is the protocol's
// concern, not theirs. The fragment carries the access code and any
// Bitbang flags; the signaling server never sees any of it because
// browsers don't transmit fragments. Grammar and flag list live in
// CONVENTIONS.md.
func (c *Client) URL(debug bool) string {
	if debug {
		return c.CodeURL(c.ID.Code, "debug")
	}
	return c.CodeURL(c.ID.Code)
}

// CodeURL composes a device URL carrying an explicit access code and
// fragment flags. Listeners that issue more than one code per identity
// (bitbang share: control + view) use it directly; URL is the
// single-code case.
//
// Flags are named bare ("ephemeral", "debug") or as "name=value"; the
// grammar uses one "!" followed by a comma-separated list, as defined by
// CONVENTIONS.md and bootstrap.js's parseUrlFlags. Keeping it here means no
// caller has to reproduce it:
//
//	https://<server>/<uid>#<code>[!<flag>[,<flag>]*]
func (c *Client) CodeURL(code string, flags ...string) string {
	s := "https://" + c.Server + "/" + c.ID.UID + "#" + code
	if len(flags) > 0 {
		s += "!" + strings.Join(flags, ",")
	}
	return s
}

// Connect connects to the signaling server and registers. On success, it
// calls handler for each incoming message. It reconnects automatically until
// Stop is called or another instance preempts this registration.
func (c *Client) Connect(handler func(msg Message)) error {
	for {
		err := c.connectOnce(handler)
		if c.preempted {
			// Storm-breaker. Another instance has this UID; reconnecting
			// would just kick them out and trigger their reconnect, ad
			// infinitum. The OnPreempted callback has already fired
			// inside the message loop; nothing more to do here.
			return ErrPreempted
		}
		if c.ctx.Err() != nil {
			return nil
		}
		if err != nil {
			log.Printf("Connection lost: %v, retrying in 3s...", err)
			timer := time.NewTimer(3 * time.Second)
			select {
			case <-timer.C:
			case <-c.ctx.Done():
				timer.Stop()
				return nil
			}
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
	conn, _, err := dialer.DialContext(c.ctx, c.ServerWS, nil)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	c.connMu.Lock()
	if c.ctx.Err() != nil {
		c.connMu.Unlock()
		_ = conn.Close()
		return c.ctx.Err()
	}
	c.conn = conn
	c.connMu.Unlock()
	defer func() {
		c.connMu.Lock()
		if c.conn == conn {
			c.conn = nil
		}
		c.connMu.Unlock()
		_ = conn.Close()
	}()

	// Register
	if err := c.register(conn); err != nil {
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

		// code_issued answers RenewCode. Intercepted here rather than
		// handed to the caller: it updates PairingCode, which is
		// signaling-layer state, and a caller that has never seen the
		// type would only have to ignore it.
		if mtype, _ := msg["type"].(string); mtype == "code_issued" {
			code, _ := msg["code"].(string)
			c.connMu.Lock()
			c.PairingCode = code
			c.connMu.Unlock()
			select {
			case c.codeIssued <- code:
			default: // nobody waiting; the field is updated either way
			}
			continue
		}

		handler(msg)
	}
}

// applyRegistered reads what the server told us on a successful
// registration. Split out of register so it can be tested without a
// socket -- the version table in particular arrives as an untyped JSON
// map, and a silently-failed assertion there would just mean nobody is
// ever told about an update.
func (c *Client) applyRegistered(msg Message) {
	// Reset first. A server that loses its pairing table (process
	// restart) re-issues a fresh code, and any stale code we were
	// holding would mislead the operator.
	c.PairingCode = ""
	if code, ok := msg["code"].(string); ok && code != "" {
		c.PairingCode = code
	}

	// The latest-release table, identical for every device, so receiving
	// it says nothing about this one. Absent from servers that track
	// nothing, and from any server older than the field.
	c.LatestVersions = nil
	if raw, ok := msg["versions"].(map[string]interface{}); ok {
		vs := make(map[string]string, len(raw))
		for k, v := range raw {
			if str, ok := v.(string); ok {
				vs[k] = str
			}
		}
		if len(vs) > 0 {
			c.LatestVersions = vs
		}
	}
}

// RenewPairingCode asks the server for a pairing code, for when the one
// issued at register time has lapsed. Returns the code, or an error.
//
// A server that predates this ignores the message and answers nothing,
// which is why there is a deadline rather than an open wait: silence is
// the expected reply from an older server, not a fault.
func (c *Client) RenewPairingCode(wait time.Duration) (string, error) {
	c.connMu.RLock()
	conn := c.conn
	c.connMu.RUnlock()
	if conn == nil {
		return "", fmt.Errorf("not connected")
	}

	// Drain a stale answer so a slow earlier reply cannot be mistaken for
	// this one.
	select {
	case <-c.codeIssued:
	default:
	}

	c.writeMu.Lock()
	err := conn.WriteJSON(map[string]string{"type": "renew_code"})
	c.writeMu.Unlock()
	if err != nil {
		return "", err
	}

	select {
	case code := <-c.codeIssued:
		if code == "" {
			return "", fmt.Errorf("the server issued no code")
		}
		return code, nil
	case <-time.After(wait):
		return "", fmt.Errorf("no answer; this signaling server may not support renewal")
	}
}

// Send sends a JSON message to the signaling server.
func (c *Client) Send(msg Message) error {
	c.connMu.RLock()
	conn := c.conn
	c.connMu.RUnlock()
	if conn == nil {
		return fmt.Errorf("not connected")
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return conn.WriteJSON(msg)
}

// Stop interrupts an active connection or reconnect delay. It is safe to call
// more than once.
func (c *Client) Stop() {
	c.cancel()
	c.connMu.RLock()
	conn := c.conn
	c.connMu.RUnlock()
	if conn != nil {
		_ = conn.Close()
	}
}

func (c *Client) register(conn *websocket.Conn) error {
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
	if len(c.OwnICEServers) > 0 {
		reg["ice_servers"] = c.OwnICEServers
	}
	c.writeMu.Lock()
	err := conn.WriteJSON(reg)
	c.writeMu.Unlock()
	if err != nil {
		return fmt.Errorf("send register: %w", err)
	}

	var msg Message
	if err := conn.ReadJSON(&msg); err != nil {
		return fmt.Errorf("read: %w", err)
	}

	switch msg["type"] {
	case "registered":
		c.applyRegistered(msg)
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
