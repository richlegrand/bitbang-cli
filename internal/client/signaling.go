// Package client implements the connecting side of a BitBang session — the
// thing that opens a WebSocket to /ws/client/<uid> on the signaling server,
// completes the WebRTC handshake (including bidirectional verify) against
// the listener, and returns a Session the caller can drive SWSP streams on.
//
// This is the mirror of bootstrap.js's BitBangConnection class. The browser
// stays the reference implementation; this package follows its protocol
// shape literally so the same listener serves either client without any
// branching.
package client

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// Message is the wire-level JSON envelope used by the signaling server.
type Message map[string]interface{}

// Signaling manages the WebSocket connection to /ws/client/<uid> on the
// signaling server. Owns the read loop; dispatches incoming messages to
// callbacks registered before Run().
//
// Lifetime is one signaling session. Once the WebRTC data channel is up
// the caller closes this — signaling is only used for the offer/answer/
// candidate exchange, not for ongoing traffic.
type Signaling struct {
	Server string // hostname, e.g. "bitba.ng"
	UID    string

	Verbose bool

	// OnOffer / OnCandidate / OnError are invoked from the read goroutine
	// for each matching message. Callbacks must not block the read loop;
	// they should hand off to another goroutine if they do real work.
	OnOffer     func(msg Message)
	OnCandidate func(msg Message)
	OnError     func(message string)

	conn   *websocket.Conn
	mu     sync.Mutex // guards writes to conn
	closed bool
}

// New constructs a Signaling client. Defaults Server to bitba.ng when
// empty so callers passing just a UID get the production listener.
func New(server, uid string) *Signaling {
	if server == "" {
		server = "bitba.ng"
	}
	return &Signaling{Server: server, UID: uid}
}

// Connect dials the signaling server and starts the read loop in a
// background goroutine. Returns once the WebSocket is open, before any
// signaling messages have been exchanged.
func (s *Signaling) Connect() error {
	url := fmt.Sprintf("wss://%s/ws/client/%s", s.Server, s.UID)
	if s.Verbose {
		fmt.Fprintf(stderr, "[client] dialing %s\n", url)
	}

	dialer := &websocket.Dialer{
		// Self-signed certs are common on dev servers; the production
		// server has a real cert and benefits nothing from cert pinning
		// here either — bidirectional verify on the data channel is the
		// real authenticator. Same posture as the listener-side dialer.
		TLSClientConfig:  &tls.Config{InsecureSkipVerify: true},
		HandshakeTimeout: 10 * time.Second,
	}
	conn, _, err := dialer.Dial(url, nil)
	if err != nil {
		return fmt.Errorf("dial signaling: %w", err)
	}
	s.conn = conn
	go s.readLoop()
	return nil
}

// SendRequest sends the initial "request" message that asks the device to
// produce an offer. SWSP v3 adds caps + version; the listener uses caps
// to know whether the client speaks the stream types it intends to use.
func (s *Signaling) SendRequest(caps []string, version int) error {
	return s.send(Message{
		"type":    "request",
		"caps":    caps,
		"version": version,
	})
}

// SendAnswer sends the SDP answer + encrypted_request (bidirectional
// verify payload). encryptedRequestB64 is the RSA-OAEP ciphertext of
// {fingerprint, nonce, code} encrypted to the device's public key.
func (s *Signaling) SendAnswer(sdp, encryptedRequestB64 string) error {
	return s.send(Message{
		"type":              "answer",
		"sdp":               sdp,
		"encrypted_request": encryptedRequestB64,
	})
}

// SendCandidate forwards a local ICE candidate to the device via the
// signaling server. Sent as the browser-native shape pion/webrtc serializes.
func (s *Signaling) SendCandidate(candidate map[string]interface{}) error {
	return s.send(Message{
		"type":      "candidate",
		"candidate": candidate,
	})
}

func (s *Signaling) send(msg Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conn == nil || s.closed {
		return fmt.Errorf("signaling: not connected")
	}
	return s.conn.WriteJSON(msg)
}

// Close terminates the WebSocket. Idempotent.
func (s *Signaling) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	if s.conn != nil {
		_ = s.conn.Close()
	}
}

func (s *Signaling) readLoop() {
	for {
		_, data, err := s.conn.ReadMessage()
		if err != nil {
			// EOF is normal once the caller closes us. Anything else
			// happens before the DC is up and is surfaced to OnError so
			// the dial can fail fast.
			s.mu.Lock()
			closed := s.closed
			s.mu.Unlock()
			if !closed && s.OnError != nil {
				s.OnError(err.Error())
			}
			return
		}
		var msg Message
		if err := json.Unmarshal(data, &msg); err != nil {
			if s.Verbose {
				fmt.Fprintf(stderr, "[client] dropped non-JSON signaling message: %v\n", err)
			}
			continue
		}
		t, _ := msg["type"].(string)
		switch t {
		case "offer":
			if s.OnOffer != nil {
				s.OnOffer(msg)
			}
		case "candidate":
			if s.OnCandidate != nil {
				s.OnCandidate(msg)
			}
		case "error":
			message, _ := msg["message"].(string)
			if s.OnError != nil {
				s.OnError(message)
			}
		default:
			if s.Verbose {
				fmt.Fprintf(stderr, "[client] ignoring signaling message type=%q\n", t)
			}
		}
	}
}
