package client

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/pion/webrtc/v4"

	"github.com/richlegrand/bitbang/internal/icehelper"
	"github.com/richlegrand/bitbang/internal/sdp"
)

// Mode is the verification posture this Peer takes for the offer it
// receives. The URL flow (bidirectional verify with an access code) and
// the pair flow (SAS-verified, no access code yet) need different
// HandleOffer behavior; everything else about Peer is identical.
type Mode int

const (
	// ModeURL is the URL-flow connector: the answerer holds an access
	// code, verifies the listener's pubkey hashes to the URL's UID,
	// builds an RSA-OAEP-encrypted {fingerprint, nonce, code} payload,
	// and rides it on the SDP answer.
	ModeURL Mode = iota

	// ModePair is the pair-flow connector: no access code exists yet
	// (the listener delivers one in pair_approved after SAS comparison),
	// no device_pubkey is consulted from the offer, and the SDP answer
	// carries no encrypted_request payload. Channel integrity rides on
	// the SAS the operator types on the listener side, not on
	// bidirectional verify.
	ModePair
)

// Peer wraps a pion PeerConnection set up as the *answerer* (connector
// side of a BitBang session). The listener creates the data channel; we
// accept it via OnDataChannel.
//
// In ModeURL, Peer owns the bidirectional-verify dance from the connector
// perspective:
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
//
// In ModePair, Peer skips steps 1–3 (no pubkey check, no encrypted_request
// payload). HandleOffer returns ("", "", nil) for those outputs; the
// caller sends an answer with sdp but without encrypted_request. The SAS
// comparison the operator performs on the listener side is what protects
// the channel against a rogue relay.
type Peer struct {
	PC           *webrtc.PeerConnection
	DC           *webrtc.DataChannel // populated when listener opens it
	mode         Mode
	devicePubkey *rsa.PublicKey // ModeURL only
	uid          string         // ModeURL only
	code         string         // ModeURL only

	mu          sync.Mutex
	verifyNonce []byte // ModeURL only

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

// NewPeer constructs a Peer in URL mode. The UID and code are the values
// from the URL the user supplied — UID anchors the pubkey/UID check, code
// rides on the bidirectional-verify payload.
func NewPeer(uid, code string, iceServers []webrtc.ICEServer) (*Peer, error) {
	return newPeer(ModeURL, uid, code, iceServers)
}

// NewPairPeer constructs a Peer in pair mode. The pair flow has no UID
// or access code at this point — those arrive in pair_approved after SAS
// verification. Use NewPeer for URL-flow connects where the credentials
// are already known.
func NewPairPeer(iceServers []webrtc.ICEServer) (*Peer, error) {
	return newPeer(ModePair, "", "", iceServers)
}

func newPeer(mode Mode, uid, code string, iceServers []webrtc.ICEServer) (*Peer, error) {
	cfg := webrtc.Configuration{ICEServers: iceServers}
	pc, err := webrtc.NewPeerConnection(cfg)
	if err != nil {
		return nil, fmt.Errorf("create peer connection: %w", err)
	}
	p := &Peer{
		PC:       pc,
		mode:     mode,
		uid:      uid,
		code:     code,
		dcReady:  make(chan struct{}),
		dcMsg:    make(chan []byte, 64),
		dcClosed: make(chan struct{}),
	}
	pc.OnDataChannel(p.onDataChannel)
	return p, nil
}

// Mode returns the verification posture this Peer was constructed with.
func (p *Peer) Mode() Mode { return p.mode }

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

// HandleOffer parses the offer from signaling, sets up the answer-side
// of the WebRTC handshake, and (in ModeURL) builds the bidirectional-
// verify encrypted_request payload. The caller forwards (answerSDP,
// encryptedRequestB64) to signaling via SendAnswer; encryptedRequestB64
// is empty in ModePair.
//
// After this returns, the caller must call AddICECandidate for each
// inbound candidate (from signaling) and forward locally-gathered
// candidates back via signaling.SendCandidate.
//
// Behavior differs by Mode:
//
//   - ModeURL: requires device_pubkey on the offer, verifies it hashes
//     to the configured UID, encrypts {fingerprint, nonce, code} to the
//     pubkey, returns the base64 ciphertext as encryptedRequestB64.
//   - ModePair: ignores device_pubkey if present, returns "" for
//     encryptedRequestB64. SAS comparison on the listener side is the
//     channel-integrity check.
func (p *Peer) HandleOffer(msg Message) (string, string, error) {
	// offerSDP — avoid the bare name "sdp" because the package import
	// (internal/sdp) shadows it for the fingerprint extraction below.
	offerSDP, _ := msg["sdp"].(string)
	if offerSDP == "" {
		return "", "", fmt.Errorf("offer missing sdp")
	}

	if p.mode == ModeURL {
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
	}

	if err := p.PC.SetRemoteDescription(webrtc.SessionDescription{
		Type: webrtc.SDPTypeOffer,
		SDP:  offerSDP,
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

	if p.mode == ModePair {
		// Pair flow: no encrypted_request, SAS substitutes.
		return localSDP, "", nil
	}

	fp := sdp.ExtractFingerprint(localSDP)
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

	encrypted, err := encryptVerifyPayload(p.devicePubkey, fp, nonce, p.code)
	if err != nil {
		return "", "", fmt.Errorf("encrypt verify payload: %w", err)
	}
	return localSDP, encrypted, nil
}

// SetICEServers updates the peer connection's ICE configuration in place —
// used by the tier-2 fallback to add the relay credentials the signaling
// server pushed. The new servers take effect on the next gathering, which a
// subsequent ICE-restart re-offer (HandleReoffer) triggers. Leaves the DTLS
// certificate (hence fingerprint) untouched, so no re-verify is needed.
func (p *Peer) SetICEServers(servers []webrtc.ICEServer) error {
	cfg := p.PC.GetConfiguration()
	cfg.ICEServers = servers
	return p.PC.SetConfiguration(cfg)
}

// HandleReoffer answers a listener's ICE-restart re-offer. Unlike HandleOffer
// it does no pubkey check and produces no encrypted_request: an ICE restart
// keeps the same DTLS fingerprint, so the original bidirectional-verify
// payload still stands and is never re-sent. Returns the answer SDP, which
// the caller forwards via SendAnswerRestart. Re-gathered candidates (now
// including relay, if SetICEServers added a TURN server) trickle out through
// the existing OnLocalCandidate callback.
func (p *Peer) HandleReoffer(msg Message) (string, error) {
	offerSDP, _ := msg["sdp"].(string)
	if offerSDP == "" {
		return "", fmt.Errorf("re-offer missing sdp")
	}
	if err := p.PC.SetRemoteDescription(webrtc.SessionDescription{
		Type: webrtc.SDPTypeOffer,
		SDP:  offerSDP,
	}); err != nil {
		return "", fmt.Errorf("set remote description (re-offer): %w", err)
	}
	answer, err := p.PC.CreateAnswer(nil)
	if err != nil {
		return "", fmt.Errorf("create answer (re-offer): %w", err)
	}
	if err := p.PC.SetLocalDescription(answer); err != nil {
		return "", fmt.Errorf("set local description (re-offer): %w", err)
	}
	return p.PC.LocalDescription().SDP, nil
}

// AddICECandidate adds an inbound trickle candidate from the device.
func (p *Peer) AddICECandidate(candidateData map[string]interface{}) error {
	c, ok := icehelper.CandidateInit(candidateData)
	if !ok {
		return nil // end-of-candidates marker
	}
	return p.PC.AddICECandidate(c)
}

// OnLocalCandidate registers a callback fired for each locally-gathered
// ICE candidate. The caller forwards each one via signaling.SendCandidate.
// A nil candidate (gathering complete) is not delivered to the callback.
func (p *Peer) OnLocalCandidate(cb func(c map[string]interface{})) {
	p.PC.OnICECandidate(func(candidate *webrtc.ICECandidate) {
		if candidate == nil {
			return
		}
		cb(icehelper.CandidateMap(candidate))
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
// the dependency on RSA-OAEP obvious at the call site. SDP fingerprint
// extraction lives in internal/sdp so both sides use the same parse.
// ---------------------------------------------------------------------------

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
