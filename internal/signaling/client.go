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
	conn     *websocket.Conn
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
	log.Printf("Ready: https://%s/%s", c.Server, c.ID.UID)

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
	return c.conn.WriteJSON(msg)
}

func (c *Client) register() error {
	// Send registration with public key and protocol version
	reg := Message{
		"type":       "register",
		"uid":        c.ID.UID,
		"public_key": c.ID.PublicB64,
		"protocol":   protocol.ProtocolVersion,
	}
	if err := c.conn.WriteJSON(reg); err != nil {
		return fmt.Errorf("send register: %w", err)
	}

	var msg Message
	if err := c.conn.ReadJSON(&msg); err != nil {
		return fmt.Errorf("read: %w", err)
	}

	switch msg["type"] {
	case "registered":
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
