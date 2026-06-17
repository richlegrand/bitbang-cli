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

// Signaling manages a WebSocket connection to a signaling-server endpoint.
// Owns the read loop; dispatches incoming messages to callbacks registered
// before the WS opens.
//
// Two endpoint flavors are supported and constructed via separate helpers:
//
//   - New(server, uid) → /ws/client/<uid>  (URL-flow connector)
//   - NewForPair(server) → /ws/pair        (pair-flow connector)
//
// Lifetime is one signaling session per flow. Once the WebRTC data channel
// is up the caller closes this — signaling is only used for the
// offer/answer/candidate exchange and any pair-flow control messages, not
// for ongoing traffic.
type Signaling struct {
	Server string // hostname, e.g. "bitba.ng"
	UID    string // populated for URL flow; empty for pair flow
	path   string // resolved WS path, e.g. /ws/client/<uid> or /ws/pair

	Verbose bool

	// OnOffer / OnCandidate / OnError are invoked from the read goroutine
	// for each matching message. Callbacks must not block the read loop;
	// they should hand off to another goroutine if they do real work.
	OnOffer     func(msg Message)
	OnCandidate func(msg Message)
	OnError     func(message string)

	// Pair-flow callbacks. Set only when the caller is driving a pair
	// flow (typically via NewForPair). nil under the URL flow.
	OnPairRouted   func()
	OnPairApproved func(msg Message)
	OnPairRejected func(reason string)

	conn   *websocket.Conn
	mu     sync.Mutex // guards writes to conn
	closed bool
}

// New constructs a Signaling client for the URL-flow connector — the path
// resolves to /ws/client/<uid>. Defaults Server to bitba.ng when empty so
// callers passing just a UID get the production signaling instance.
func New(server, uid string) *Signaling {
	if server == "" {
		server = "bitba.ng"
	}
	return &Signaling{
		Server: server,
		UID:    uid,
		path:   "/ws/client/" + uid,
	}
}

// NewForPair constructs a Signaling client for the pair-flow connector —
// the path is /ws/pair. UID is unset because pair_init carries the lookup
// code instead.
func NewForPair(server string) *Signaling {
	if server == "" {
		server = "bitba.ng"
	}
	return &Signaling{
		Server: server,
		path:   "/ws/pair",
	}
}

// Connect dials the signaling server and starts the read loop in a
// background goroutine. Returns once the WebSocket is open, before any
// signaling messages have been exchanged.
func (s *Signaling) Connect() error {
	url := fmt.Sprintf("wss://%s%s", s.Server, s.path)
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

// SendPairInit sends the pair-flow initial message with a 6-digit code.
// The signaling server validates the code (sleeping ~3s constant-time to
// brake enumeration), then either routes pair_request to the listener and
// replies pair_routed to us, or returns an "error" message with
// {"message":"unknown_code"}. Only meaningful on a Signaling constructed
// via NewForPair.
func (s *Signaling) SendPairInit(code string) error {
	return s.send(Message{
		"type": "pair_init",
		"code": code,
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

		// Pair-flow control messages — only meaningful under NewForPair,
		// but routed unconditionally so a misconfigured callback fires
		// loud instead of silently dropping.
		case "pair_routed":
			if s.OnPairRouted != nil {
				s.OnPairRouted()
			}
		case "pair_approved":
			if s.OnPairApproved != nil {
				s.OnPairApproved(msg)
			}
		case "pair_rejected":
			if s.OnPairRejected != nil {
				reason, _ := msg["reason"].(string)
				s.OnPairRejected(reason)
			}

		default:
			if s.Verbose {
				fmt.Fprintf(stderr, "[client] ignoring signaling message type=%q\n", t)
			}
		}
	}
}
