package client

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"sync"

	"github.com/pion/webrtc/v4"
)

// Peer wraps a pion PeerConnection set up as the *answerer* (client side
// of a BitBang session). The listener creates the data channel; we accept
// it via OnDataChannel.
//
// Owns the bidirectional-verify dance from the client perspective:
//
//  1. Receive offer from signaling, which carries `device_pubkey`.
//  2. Verify hash(pubkey)[:16] == uid (catches a rogue server that swaps
//     keys for a UID it doesn't own).
//  3. Create SDP answer; extract its DTLS fingerprint; encrypt
//     {fingerprint, nonce, code} with the device's public key.
//  4. Send answer + encrypted_request to signaling.
//  5. When the data channel opens, expect the device's first stream-0
//     frame to be `verify_nonce_hash` with hash == sha256(nonce). Anything
//     else — wrong hash, no frame, payload before verify — fails the
//     connection.
//
// The session layer drives step (5); Peer exposes Nonce() so it can be
// checked against the inbound hash.
type Peer struct {
	PC           *webrtc.PeerConnection
	DC           *webrtc.DataChannel // populated when listener opens it
	devicePubkey *rsa.PublicKey
	uid          string
	code         string

	mu          sync.Mutex
	verifyNonce []byte // populated when answer is built

	// dcReady fires once the data channel transitions to open AND has been
	// stored on Peer. Session.Wait blocks on this before sending control
	// messages.
	dcReady chan struct{}

	// dcMsg is the queue of inbound data-channel messages, drained by the
	// session layer. Buffered so the read goroutine never blocks.
	dcMsg chan []byte

	// dcClosed fires once the data channel transitions to closed. Used so
	// the session layer can shut down its consumer goroutines cleanly.
	dcClosed chan struct{}
}

// NewPeer constructs a Peer ready to receive an offer. The UID and code
// are the values from the URL the user supplied — UID anchors the
// pubkey/UID check, code rides on the bidirectional-verify payload.
func NewPeer(uid, code string, iceServers []webrtc.ICEServer) (*Peer, error) {
	cfg := webrtc.Configuration{ICEServers: iceServers}
	pc, err := webrtc.NewPeerConnection(cfg)
	if err != nil {
		return nil, fmt.Errorf("create peer connection: %w", err)
	}
	p := &Peer{
		PC:       pc,
		uid:      uid,
		code:     code,
		dcReady:  make(chan struct{}),
		dcMsg:    make(chan []byte, 64),
		dcClosed: make(chan struct{}),
	}
	pc.OnDataChannel(p.onDataChannel)
	return p, nil
}

func (p *Peer) onDataChannel(dc *webrtc.DataChannel) {
	p.DC = dc

	dc.OnOpen(func() {
		// Read-loop wiring done before the channel is announced as ready
		// so the first inbound frame (verify_nonce_hash) is captured even
		// if the session layer is slightly slow to start consuming.
		close(p.dcReady)
	})

	dc.OnMessage(func(msg webrtc.DataChannelMessage) {
		select {
		case p.dcMsg <- msg.Data:
		default:
			// Drop on full queue. 64 buffered frames is plenty for the
			// control / handshake phase; once the session is up the
			// consumer drains as fast as frames arrive. A miss here would
			// indicate the session goroutine is stuck.
		}
	})

	dc.OnClose(func() {
		select {
		case <-p.dcClosed:
		default:
			close(p.dcClosed)
		}
	})
}

// HandleOffer parses the offer from signaling, runs the pubkey/UID
// check, builds the SDP answer, and produces the encrypted_request
// payload. The caller forwards (answerSDP, encryptedRequestB64) to
// signaling via SendAnswer.
//
// After this returns, the caller must call AddICECandidate for each
// inbound candidate (from signaling) and forward locally-gathered
// candidates back via signaling.SendCandidate.
func (p *Peer) HandleOffer(msg Message) (string, string, error) {
	sdp, _ := msg["sdp"].(string)
	if sdp == "" {
		return "", "", fmt.Errorf("offer missing sdp")
	}
	devicePubkeyB64, _ := msg["device_pubkey"].(string)
	if devicePubkeyB64 == "" {
		return "", "", fmt.Errorf("offer missing device_pubkey — listener too old or misbehaving")
	}

	pubkey, der, err := importDevicePubkey(devicePubkeyB64)
	if err != nil {
		return "", "", fmt.Errorf("decode device_pubkey: %w", err)
	}
	if got := uidFromPubkeyDER(der); got != p.uid {
		return "", "", fmt.Errorf("pubkey/UID mismatch (server returned key for %s, expected %s)", got, p.uid)
	}
	p.devicePubkey = pubkey

	if err := p.PC.SetRemoteDescription(webrtc.SessionDescription{
		Type: webrtc.SDPTypeOffer,
		SDP:  sdp,
	}); err != nil {
		return "", "", fmt.Errorf("set remote description: %w", err)
	}

	answer, err := p.PC.CreateAnswer(nil)
	if err != nil {
		return "", "", fmt.Errorf("create answer: %w", err)
	}
	if err := p.PC.SetLocalDescription(answer); err != nil {
		return "", "", fmt.Errorf("set local description: %w", err)
	}

	localSDP := p.PC.LocalDescription().SDP
	fp := extractDTLSFingerprint(localSDP)
	if fp == "" {
		return "", "", fmt.Errorf("local SDP has no sha-256 fingerprint")
	}

	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return "", "", fmt.Errorf("generate verify nonce: %w", err)
	}
	p.mu.Lock()
	p.verifyNonce = nonce
	p.mu.Unlock()

	encrypted, err := encryptVerifyPayload(pubkey, fp, nonce, p.code)
	if err != nil {
		return "", "", fmt.Errorf("encrypt verify payload: %w", err)
	}
	return localSDP, encrypted, nil
}

// AddICECandidate adds an inbound trickle candidate from the device.
func (p *Peer) AddICECandidate(candidateData map[string]interface{}) error {
	candidateStr, _ := candidateData["candidate"].(string)
	if candidateStr == "" {
		return nil // end-of-candidates marker
	}
	sdpMid, _ := candidateData["sdpMid"].(string)
	sdpMLineIndexFloat, _ := candidateData["sdpMLineIndex"].(float64)
	sdpMLineIndex := uint16(sdpMLineIndexFloat)
	return p.PC.AddICECandidate(webrtc.ICECandidateInit{
		Candidate:     candidateStr,
		SDPMid:        &sdpMid,
		SDPMLineIndex: &sdpMLineIndex,
	})
}

// OnLocalCandidate registers a callback fired for each locally-gathered
// ICE candidate. The caller forwards each one via signaling.SendCandidate.
// A nil candidate (gathering complete) is not delivered to the callback.
func (p *Peer) OnLocalCandidate(cb func(c map[string]interface{})) {
	p.PC.OnICECandidate(func(candidate *webrtc.ICECandidate) {
		if candidate == nil {
			return
		}
		j := candidate.ToJSON()
		cb(map[string]interface{}{
			"candidate":     j.Candidate,
			"sdpMid":        j.SDPMid,
			"sdpMLineIndex": j.SDPMLineIndex,
		})
	})
}

// Nonce returns the random bytes whose sha-256 the device must return on
// stream 0 as the first frame after the channel opens.
func (p *Peer) Nonce() []byte {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.verifyNonce
}

// DCReady returns a channel that closes when the data channel is open.
func (p *Peer) DCReady() <-chan struct{} { return p.dcReady }

// DCMessages returns the inbound data-channel message queue, drained by
// the session layer.
func (p *Peer) DCMessages() <-chan []byte { return p.dcMsg }

// DCClosed returns a channel that closes when the data channel is closed.
func (p *Peer) DCClosed() <-chan struct{} { return p.dcClosed }

// Close tears down the peer connection.
func (p *Peer) Close() {
	if p.PC != nil {
		_ = p.PC.Close()
	}
}

// ---------------------------------------------------------------------------
// Bidirectional-verify helpers (mirror bootstrap.js's three corresponding
// functions). Kept in this file rather than a separate verify.go to make
// the dependency on RSA-OAEP / SDP fingerprint extraction obvious at the
// call site.
// ---------------------------------------------------------------------------

var fingerprintLine = regexp.MustCompile(`(?i)^a=fingerprint:sha-256\s+([0-9A-F:]+)\s*$`)

// extractDTLSFingerprint pulls the sha-256 DTLS fingerprint out of an SDP
// and normalizes to uppercase. Must match peer/verify.go on the listener
// side byte-for-byte — the device compares the string we send against
// what extractFingerprint returns from its parse of the same SDP.
func extractDTLSFingerprint(sdp string) string {
	for _, line := range strings.Split(sdp, "\n") {
		line = strings.TrimRight(line, "\r")
		if m := fingerprintLine.FindStringSubmatch(line); m != nil {
			return strings.ToUpper(m[1])
		}
	}
	return ""
}

// importDevicePubkey decodes a base64-encoded SPKI DER public key into
// an *rsa.PublicKey. Returns the DER bytes too so the caller can hash
// them for the UID check without re-marshalling.
func importDevicePubkey(b64 string) (*rsa.PublicKey, []byte, error) {
	der, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil, nil, fmt.Errorf("base64 decode: %w", err)
	}
	pub, err := x509.ParsePKIXPublicKey(der)
	if err != nil {
		return nil, nil, fmt.Errorf("parse SPKI: %w", err)
	}
	rsaPub, ok := pub.(*rsa.PublicKey)
	if !ok {
		return nil, nil, fmt.Errorf("device_pubkey is not RSA")
	}
	return rsaPub, der, nil
}

// uidFromPubkeyDER reproduces identity.UIDFromPublicKeyBytes — the first
// 16 bytes of sha-256(DER), base64url-no-padding. Mirrored here rather
// than imported to keep this package free of internal/identity coupling
// (the client never holds a *device* identity; it gets the pubkey on
// the wire).
func uidFromPubkeyDER(der []byte) string {
	hash := sha256.Sum256(der)
	return base64.RawURLEncoding.EncodeToString(hash[:16])
}

// encryptVerifyPayload RSA-OAEP-encrypts {fingerprint, nonce, code} with
// the device's public key. JSON shape and field names match exactly what
// peer/connection.go (HandleAnswer) expects to decrypt.
func encryptVerifyPayload(pubkey *rsa.PublicKey, fingerprint string, nonce []byte, code string) (string, error) {
	payload := struct {
		Fingerprint string `json:"fingerprint"`
		Nonce       string `json:"nonce"`
		Code        string `json:"code,omitempty"`
	}{
		Fingerprint: fingerprint,
		Nonce:       base64.StdEncoding.EncodeToString(nonce),
		Code:        code,
	}
	plain, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	cipher, err := rsa.EncryptOAEP(sha256.New(), rand.Reader, pubkey, plain, nil)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(cipher), nil
}

// expectedNonceHash returns base64(sha256(nonce)) — the value the device
// is expected to put in its verify_nonce_hash control frame. Used by the
// session layer to validate the inbound frame.
func expectedNonceHash(nonce []byte) string {
	h := sha256.Sum256(nonce)
	return base64.StdEncoding.EncodeToString(h[:])
}
