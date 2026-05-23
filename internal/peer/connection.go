// Package peer manages WebRTC peer connections with browsers.
//
// For each incoming connection request, it creates a PeerConnection with ICE
// servers from the signaling server, generates an SDP offer with a data channel,
// and handles the answer and trickle ICE candidates.
//
// Bidirectional verify rides on the answer message: the browser delivers an
// RSA-OAEP-encrypted {fingerprint, nonce} payload. The device decrypts,
// confirms the fingerprint matches the SDP, and replies with hash(nonce) on
// stream 0 as soon as the data channel opens. A fingerprint mismatch closes
// the connection without ever sending the nonce hash, which is what protects
// the protocol against a rogue signaling server that rewrites SDPs.
package peer

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"sync"

	"github.com/pion/webrtc/v4"
	"github.com/richlegrand/bitbang/internal/identity"
	"github.com/richlegrand/bitbang/internal/signaling"
)

// OnMessageFunc is called for each data channel message.
type OnMessageFunc func(data []byte)

// Connection represents a WebRTC peer connection with a browser client.
type Connection struct {
	ClientID  string
	PC        *webrtc.PeerConnection
	DC        *webrtc.DataChannel
	sig       *signaling.Client
	OnMessage OnMessageFunc

	// identity holds the device's private key, used to decrypt the
	// browser's encrypted_request payload riding on the SDP answer.
	identity *identity.Identity

	mu sync.Mutex
	// nonce is the random bytes the browser put in the encrypted payload.
	// We hash it and send hash(nonce) on stream 0 once the DC opens, proving
	// to the browser that we hold the private key for this UID's pubkey.
	nonce []byte
	// verifyFailed is set true if HandleAnswer detects a fingerprint
	// mismatch (or any other reason to reject). The OnOpen callback checks
	// this before sending the nonce-hash frame; on failure it closes the
	// connection without sending anything.
	verifyFailed bool
}

// HandleRequest creates a new peer connection in response to a browser's
// connection request. It configures ICE servers, creates the data channel,
// generates an SDP offer, and sends it back via the signaling client.
// The onMessage callback is called for each data channel message.
//
// id is the device's identity — needed to decrypt the bidirectional-verify
// payload that arrives later with the SDP answer.
func HandleRequest(msg signaling.Message, sig *signaling.Client, id *identity.Identity, onMessage OnMessageFunc, verbose bool) (*Connection, error) {
	clientID, _ := msg["client_id"].(string)
	if clientID == "" {
		return nil, fmt.Errorf("missing client_id")
	}

	// Parse ICE servers from the signaling server's request message
	iceServers := parseICEServers(msg)

	config := webrtc.Configuration{
		ICEServers: iceServers,
	}

	pc, err := webrtc.NewPeerConnection(config)
	if err != nil {
		return nil, fmt.Errorf("create peer connection: %w", err)
	}

	conn := &Connection{
		ClientID:  clientID,
		PC:        pc,
		sig:       sig,
		OnMessage: onMessage,
		identity:  id,
	}

	// Create data channel (we are the offerer)
	dc, err := pc.CreateDataChannel("http", nil)
	if err != nil {
		pc.Close()
		return nil, fmt.Errorf("create data channel: %w", err)
	}
	conn.DC = dc

	dc.OnOpen(func() {
		log.Printf("Data channel opened for %s", clientID)
		conn.mu.Lock()
		failed := conn.verifyFailed
		nonce := conn.nonce
		conn.mu.Unlock()
		if failed {
			log.Printf("Verify failed for %s — closing without nonce reply", clientID)
			pc.Close()
			return
		}
		if nonce == nil {
			// Browser did not include the bidirectional-verify payload —
			// reject. Connecting clients must use a bootstrap that supports
			// the encrypted_request field; older browsers won't be served.
			log.Printf("No bidirectional-verify nonce for %s — closing", clientID)
			pc.Close()
			return
		}
		frame, err := buildVerifyNonceHashFrame(nonce)
		if err != nil {
			log.Printf("Build verify_nonce_hash for %s: %v", clientID, err)
			pc.Close()
			return
		}
		if err := dc.Send(frame); err != nil {
			log.Printf("Send verify_nonce_hash for %s: %v", clientID, err)
			pc.Close()
			return
		}
	})

	dcClosed := false

	dc.OnClose(func() {
		dcClosed = true
		log.Printf("Data channel closed for %s", clientID)
		pc.Close()
	})

	dc.OnMessage(func(msg webrtc.DataChannelMessage) {
		if conn.OnMessage != nil {
			conn.OnMessage(msg.Data)
		}
	})

	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		if dcClosed {
			return
		}
		if verbose {
			log.Printf("Connection state for %s: %s", clientID, state.String())
		}
		if state == webrtc.PeerConnectionStateFailed {
			pc.Close()
		}
	})

	// Send trickle ICE candidates to browser via signaling
	pc.OnICECandidate(func(candidate *webrtc.ICECandidate) {
		if candidate == nil {
			return // gathering complete
		}
		candidateJSON := candidate.ToJSON()
		sig.Send(signaling.Message{
			"type":      "candidate",
			"client_id": clientID,
			"candidate": map[string]interface{}{
				"candidate":     candidateJSON.Candidate,
				"sdpMid":        candidateJSON.SDPMid,
				"sdpMLineIndex": candidateJSON.SDPMLineIndex,
			},
		})
	})

	// Create and send offer
	offer, err := pc.CreateOffer(nil)
	if err != nil {
		pc.Close()
		return nil, fmt.Errorf("create offer: %w", err)
	}
	if err := pc.SetLocalDescription(offer); err != nil {
		pc.Close()
		return nil, fmt.Errorf("set local description: %w", err)
	}

	// Wait for ICE gathering to complete so all candidates are in the SDP
	gatherComplete := webrtc.GatheringCompletePromise(pc)
	<-gatherComplete

	// Send the offer with all candidates bundled
	sig.Send(signaling.Message{
		"type":      "offer",
		"client_id": clientID,
		"sdp":       pc.LocalDescription().SDP,
		"streams":   map[string]interface{}{},
	})

	return conn, nil
}

// HandleAnswer sets the remote description from the browser's SDP answer and
// runs the bidirectional-verify decrypt step.
//
// encryptedRequestB64 is the base64 ciphertext the browser sent alongside its
// SDP — RSA-OAEP/SHA-256 of {fingerprint, nonce} encrypted to this device's
// public key. We decrypt, confirm the fingerprint claim matches the
// fingerprint in the SDP, and stash the nonce so it can be hashed and
// returned on stream 0 the moment the data channel opens. On any failure
// (missing payload, decrypt error, fingerprint mismatch) we mark the
// connection as verify-failed; the OnOpen callback will then close without
// sending the nonce reply, which is what makes a rogue relay attack visible
// from the browser's side.
func (c *Connection) HandleAnswer(sdp, encryptedRequestB64 string) error {
	answer := webrtc.SessionDescription{
		Type: webrtc.SDPTypeAnswer,
		SDP:  sdp,
	}
	if err := c.PC.SetRemoteDescription(answer); err != nil {
		return fmt.Errorf("set remote description: %w", err)
	}

	if encryptedRequestB64 == "" {
		c.mu.Lock()
		c.verifyFailed = true
		c.mu.Unlock()
		return fmt.Errorf("missing encrypted_request from browser — bidirectional verify is required")
	}

	ciphertext, err := base64.StdEncoding.DecodeString(encryptedRequestB64)
	if err != nil {
		c.markVerifyFailed()
		return fmt.Errorf("decode encrypted_request: %w", err)
	}

	plaintext, err := c.identity.Decrypt(ciphertext)
	if err != nil {
		c.markVerifyFailed()
		return fmt.Errorf("decrypt encrypted_request: %w", err)
	}

	var req struct {
		Fingerprint string `json:"fingerprint"`
		Nonce       string `json:"nonce"`
	}
	if err := json.Unmarshal(plaintext, &req); err != nil {
		c.markVerifyFailed()
		return fmt.Errorf("parse encrypted_request: %w", err)
	}

	nonceBytes, err := base64.StdEncoding.DecodeString(req.Nonce)
	if err != nil {
		c.markVerifyFailed()
		return fmt.Errorf("decode nonce: %w", err)
	}

	actual := extractFingerprint(sdp)
	if actual == "" {
		c.markVerifyFailed()
		return fmt.Errorf("no sha-256 fingerprint in answer SDP")
	}
	if !fingerprintsEqual(actual, req.Fingerprint) {
		c.markVerifyFailed()
		return fmt.Errorf("DTLS fingerprint mismatch (browser claimed %s, SDP has %s) — possible rogue relay", req.Fingerprint, actual)
	}

	c.mu.Lock()
	c.nonce = nonceBytes
	c.mu.Unlock()
	return nil
}

// markVerifyFailed flips the verify-failed flag under the mutex.
func (c *Connection) markVerifyFailed() {
	c.mu.Lock()
	c.verifyFailed = true
	c.mu.Unlock()
}

// AddICECandidate adds a trickle ICE candidate from the browser.
func (c *Connection) AddICECandidate(candidateData map[string]interface{}) error {
	candidateStr, _ := candidateData["candidate"].(string)
	if candidateStr == "" {
		return nil // empty candidate = end of candidates
	}

	sdpMid, _ := candidateData["sdpMid"].(string)
	sdpMLineIndexFloat, _ := candidateData["sdpMLineIndex"].(float64)
	sdpMLineIndex := uint16(sdpMLineIndexFloat)

	candidate := webrtc.ICECandidateInit{
		Candidate:     candidateStr,
		SDPMid:        &sdpMid,
		SDPMLineIndex: &sdpMLineIndex,
	}

	if err := c.PC.AddICECandidate(candidate); err != nil {
		return fmt.Errorf("add ICE candidate: %w", err)
	}
	return nil
}

// Close closes the peer connection.
func (c *Connection) Close() {
	if c.PC != nil {
		c.PC.Close()
	}
}

// parseICEServers extracts ICE server configuration from the signaling
// server's request message and converts to Pion's format.
func parseICEServers(msg signaling.Message) []webrtc.ICEServer {
	raw, ok := msg["ice_servers"]
	if !ok {
		return nil
	}

	// ice_servers arrives as []interface{} from JSON unmarshaling
	data, err := json.Marshal(raw)
	if err != nil {
		return nil
	}

	// Parse the browser-native iceServers format
	var servers []struct {
		URLs       interface{} `json:"urls"`
		Username   string      `json:"username"`
		Credential string      `json:"credential"`
	}
	if err := json.Unmarshal(data, &servers); err != nil {
		return nil
	}

	var iceServers []webrtc.ICEServer
	for _, s := range servers {
		// urls can be string or []string
		var urls []string
		switch v := s.URLs.(type) {
		case string:
			urls = []string{v}
		case []interface{}:
			for _, u := range v {
				if str, ok := u.(string); ok {
					urls = append(urls, str)
				}
			}
		}

		server := webrtc.ICEServer{URLs: urls}
		if s.Username != "" {
			server.Username = s.Username
			server.Credential = s.Credential
			server.CredentialType = webrtc.ICECredentialTypePassword
		}
		iceServers = append(iceServers, server)
	}

	return iceServers
}
