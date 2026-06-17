// Package peer manages WebRTC peer connections with browsers.
//
// For each incoming connection request, it creates a PeerConnection with ICE
// servers from the signaling server, generates an SDP offer with a data channel,
// and handles the answer and trickle ICE candidates.
//
// Bidirectional verify rides on the answer message: the browser delivers an
// RSA-OAEP-encrypted {fingerprint, nonce, code} payload. The device decrypts,
// checks the access code (64-bit secret from the URL fragment), confirms
// the fingerprint matches the SDP, and replies with hash(nonce) on stream 0
// as soon as the data channel opens. Any failure closes the connection
// without ever sending the nonce hash, which is what protects the protocol
// against a rogue signaling server that rewrites SDPs or initiates its own
// connections.
package peer

import (
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"sync"

	"github.com/pion/webrtc/v4"
	"github.com/richlegrand/bitbang/internal/identity"
	"github.com/richlegrand/bitbang/internal/pairing"
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

	// OnClose, if set, is invoked when the data channel transitions to
	// closed. Used by listener wire-up to clean up per-session
	// resources whose lifetime would otherwise outlast the connection
	// — most importantly, kill any shell processes that would
	// otherwise keep holding their max-sessions slot.
	OnClose func()

	// PairingMode is true when this connection was created by
	// HandlePairRequest rather than HandleRequest. In pair mode the
	// access-code check in HandleAnswer is skipped (the connector does not
	// yet have an access code — that's what pairing delivers) and the
	// data-channel OnOpen runs the SAS confirmation flow instead of
	// sending verify_nonce_hash. The connector reads its computed SAS
	// aloud; the listener types it; BitBang compares the typed value to
	// its own SAS. Match → pair_approved with uid+access_code. Mismatch →
	// pair_rejected with reason.
	PairingMode bool

	// identity holds the device's private key, used to decrypt the
	// browser's encrypted_request payload riding on the SDP answer.
	identity *identity.Identity

	// browserIP is the connecting browser's IP as reported by the
	// signaling server. Empty when the server didn't supply one (e.g.
	// in tests). Surfaced in log lines for bad-code attempts.
	browserIP string

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

// connSetup carries the per-flow customization for setupConnection:
// what to log on first sight, what to do when the data channel opens,
// and which flow-specific fields to populate on the Connection. The
// shared boilerplate (ParseICEServers, PC creation, DC creation, trickle
// ICE plumbing, offer-send) lives in setupConnection.
type connSetup struct {
	clientID string
	msg      signaling.Message
	sig      *signaling.Client
	id       *identity.Identity
	verbose  bool

	// logTag distinguishes log prefixes between flows — "Connection" for
	// the regular request path, "Pair" for the pair_request path. Used
	// in the data-channel open/close lines and the state-change log.
	logTag string

	// onMessage is the per-data-channel-message callback. Nil for pair
	// flow (no application traffic flows over the pair-mode PC).
	onMessage OnMessageFunc

	// onOpen is the post-DTLS body — verify_nonce_hash for the regular
	// flow, SAS prompt + approve/reject for the pair flow. It runs
	// inside dc.OnOpen; the helper provides conn and dc as arguments.
	onOpen func(conn *Connection, dc *webrtc.DataChannel)

	// Flow-specific Connection-struct fields. browserIP is populated
	// only for the regular flow; pairingMode only for the pair flow.
	browserIP   string
	pairingMode bool
}

// setupConnection builds the PC + DC + signaling wiring shared by both
// flow entry points (HandleRequest and HandlePairRequest). The caller's
// only obligation is to fill in connSetup.onOpen with the post-DTLS body
// that distinguishes the flow.
//
// On success the Connection is returned with the offer already sent;
// answer + ICE-candidate handling proceeds normally from the caller's
// signaling read loop. On failure the partial PC is cleaned up.
func setupConnection(s connSetup) (*Connection, error) {
	iceServers := ParseICEServers(s.msg)
	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{ICEServers: iceServers})
	if err != nil {
		return nil, fmt.Errorf("create peer connection: %w", err)
	}

	conn := &Connection{
		ClientID:    s.clientID,
		PC:          pc,
		sig:         s.sig,
		OnMessage:   s.onMessage,
		identity:    s.id,
		browserIP:   s.browserIP,
		PairingMode: s.pairingMode,
	}

	dc, err := pc.CreateDataChannel("http", nil)
	if err != nil {
		pc.Close()
		return nil, fmt.Errorf("create data channel: %w", err)
	}
	conn.DC = dc

	dc.OnOpen(func() {
		log.Printf("%s data channel opened for %s", s.logTag, s.clientID)
		s.onOpen(conn, dc)
	})

	dcClosed := false
	dc.OnClose(func() {
		dcClosed = true
		log.Printf("%s data channel closed for %s", s.logTag, s.clientID)
		// Run caller-supplied cleanup BEFORE closing the PC, so any
		// resources tied to the data channel (e.g. spawned shell
		// processes) get torn down while the connection state is
		// still observable.
		if conn.OnClose != nil {
			conn.OnClose()
		}
		pc.Close()
	})

	dc.OnMessage(func(m webrtc.DataChannelMessage) {
		if conn.OnMessage != nil {
			conn.OnMessage(m.Data)
		}
	})

	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		if dcClosed {
			return
		}
		if s.verbose {
			log.Printf("%s state for %s: %s", s.logTag, s.clientID, state.String())
		}
		if state == webrtc.PeerConnectionStateFailed {
			pc.Close()
		}
	})

	// Trickle ICE candidates to the connector via signaling.
	pc.OnICECandidate(func(candidate *webrtc.ICECandidate) {
		if candidate == nil {
			return // gathering complete
		}
		cj := candidate.ToJSON()
		s.sig.Send(signaling.Message{
			"type":      "candidate",
			"client_id": s.clientID,
			"candidate": map[string]interface{}{
				"candidate":     cj.Candidate,
				"sdpMid":        cj.SDPMid,
				"sdpMLineIndex": cj.SDPMLineIndex,
			},
		})
	})

	offer, err := pc.CreateOffer(nil)
	if err != nil {
		pc.Close()
		return nil, fmt.Errorf("create offer: %w", err)
	}
	if err := pc.SetLocalDescription(offer); err != nil {
		pc.Close()
		return nil, fmt.Errorf("set local description: %w", err)
	}

	// Trickle ICE: send the offer immediately and let candidates trickle
	// via the OnICECandidate callback above. Waiting for gather to
	// complete here would add up to pion's default 5 s ICE-gather
	// timeout to every connection setup — even though we're already
	// trickling those candidates out separately and bundling them in
	// the SDP would just be redundant. Both connectors (bootstrap.js and
	// internal/client) buffer remote candidates that arrive before
	// setRemoteDescription finishes, so the candidate stream is safe to
	// start before the offer has even been processed.
	s.sig.Send(signaling.Message{
		"type":      "offer",
		"client_id": s.clientID,
		"sdp":       pc.LocalDescription().SDP,
		"streams":   map[string]interface{}{},
	})

	return conn, nil
}

// HandleRequest creates a new peer connection in response to a connector's
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

	// browser_ip is supplied by the signaling server; connectors can't
	// set it themselves. Empty when the server didn't provide it (e.g.
	// older signaling server, local tests).
	browserIP, _ := msg["browser_ip"].(string)
	logIP := browserIP
	if logIP == "" {
		logIP = "?"
	}
	log.Printf("Connection request from %s (browser_ip=%s)", clientID, logIP)

	return setupConnection(connSetup{
		clientID:  clientID,
		msg:       msg,
		sig:       sig,
		id:        id,
		verbose:   verbose,
		logTag:    "Connection",
		onMessage: onMessage,
		browserIP: browserIP,
		onOpen:    handleRequestOnOpen,
	})
}

// handleRequestOnOpen is the post-DTLS body for the regular flow. Sends
// verify_nonce_hash on stream 0, proving to the connector that the
// listener decrypted the bidirectional-verify payload. Verify failure
// (set by HandleAnswer) closes without sending the frame, which is the
// signal the connector watches for to detect a rogue relay.
func handleRequestOnOpen(conn *Connection, dc *webrtc.DataChannel) {
	conn.mu.Lock()
	failed := conn.verifyFailed
	nonce := conn.nonce
	conn.mu.Unlock()
	if failed {
		log.Printf("Verify failed for %s — closing without nonce reply", conn.ClientID)
		conn.PC.Close()
		return
	}
	if nonce == nil {
		// Connector did not include the bidirectional-verify payload —
		// reject. Connecting clients must use a bootstrap that supports
		// the encrypted_request field; older browsers won't be served.
		log.Printf("No bidirectional-verify nonce for %s — closing", conn.ClientID)
		conn.PC.Close()
		return
	}
	frame, err := buildVerifyNonceHashFrame(nonce)
	if err != nil {
		log.Printf("Build verify_nonce_hash for %s: %v", conn.ClientID, err)
		conn.PC.Close()
		return
	}
	if err := dc.Send(frame); err != nil {
		log.Printf("Send verify_nonce_hash for %s: %v", conn.ClientID, err)
		conn.PC.Close()
		return
	}
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
		Code        string `json:"code"`
	}
	if err := json.Unmarshal(plaintext, &req); err != nil {
		c.markVerifyFailed()
		return fmt.Errorf("parse encrypted_request: %w", err)
	}

	// Check the access code before anything else — a wrong code should
	// never reveal whether the fingerprint matched. Constant-time compare
	// to avoid leaking the code via timing.
	if subtle.ConstantTimeCompare([]byte(req.Code), []byte(c.identity.Code)) != 1 {
		c.markVerifyFailed()
		ip := c.browserIP
		if ip == "" {
			ip = "?"
		}
		return fmt.Errorf("bad access code from %s (browser_ip=%s)", c.ClientID, ip)
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

// HandlePairRequest creates a peer connection in response to a "pair_request"
// from the signaling server. Wire-shape is the same as a regular request
// (offer / answer / candidate); the difference is in what happens after the
// data channel opens: instead of sending verify_nonce_hash, the listener
// computes the Short Authentication String (SAS) from the two negotiated DTLS
// fingerprints, prompts its operator to enter the SAS that the connector is
// reading aloud, and sends pair_approved (with the listener's UID and access
// code) or pair_rejected (with the reason) through signaling.
//
// prompt is the PromptFunc used to read each SAS attempt; pass
// pairing.DefaultTTYPrompt for interactive listeners. The SAS itself is
// never written to the terminal — the whole design point is that there is
// nothing to blindly approve.
//
// The connection closes itself in all post-DTLS outcomes (approved, rejected,
// timeout, abort). The connector never sends application traffic over this
// connection; once it receives pair_approved on signaling, it reconnects via
// the standard /ws/client/<uid> direct flow using the delivered access code.
//
// pair_request currently carries no ice_servers — pairing assumes
// reachable peers (typically interactive co-location). If the server
// later attaches TURN here, ParseICEServers picks it up unchanged.
func HandlePairRequest(msg signaling.Message, sig *signaling.Client, id *identity.Identity, prompt pairing.PromptFunc, verbose bool) (*Connection, error) {
	clientID, _ := msg["client_id"].(string)
	if clientID == "" {
		return nil, fmt.Errorf("missing client_id")
	}

	// pair_request carries the connector's IP under remote_ip — distinct
	// from the regular request flow's browser_ip, since the connector
	// isn't necessarily a browser.
	if remoteIP, _ := msg["remote_ip"].(string); remoteIP == "" {
		log.Printf("Pair request received")
	} else {
		log.Printf("Pair request received from %s", remoteIP)
	}

	return setupConnection(connSetup{
		clientID:    clientID,
		msg:         msg,
		sig:         sig,
		id:          id,
		verbose:     verbose,
		logTag:      "Pair",
		pairingMode: true,
		onOpen: func(conn *Connection, dc *webrtc.DataChannel) {
			handlePairRequestOnOpen(conn, prompt, id)
		},
	})
}

// handlePairRequestOnOpen is the post-DTLS body for the pair flow. Extracts
// both DTLS fingerprints from the negotiated SDPs, computes the SAS, prompts
// the listener operator to type the value the connector is reading aloud,
// and signals pair_approved / pair_rejected accordingly. Closes the PC in
// all outcomes — the connector reconnects via the standard direct flow.
func handlePairRequestOnOpen(conn *Connection, prompt pairing.PromptFunc, id *identity.Identity) {
	localFp := extractFingerprint(conn.PC.LocalDescription().SDP)
	remoteFp := extractFingerprint(conn.PC.RemoteDescription().SDP)
	if localFp == "" || remoteFp == "" {
		// SDP without sha-256 fingerprints shouldn't happen with any
		// modern peer; if it does, refusing is the only safe move.
		log.Printf("Pair %s: missing SDP fingerprints (local=%q remote=%q)", conn.ClientID, localFp, remoteFp)
		conn.sendPairRejected("user_declined")
		conn.PC.Close()
		return
	}
	sas := ComputeSAS(localFp, remoteFp)
	// Don't log the SAS — even verbose mode shouldn't leak it to the
	// operator (who would then have nothing to type, since the design
	// assumes they hear it from the connector).

	reason, ok := pairing.PromptForSAS(sas, prompt)
	if !ok {
		log.Printf("Pair rejected for %s: %s", conn.ClientID, reason)
		conn.sendPairRejected(reason)
		conn.PC.Close()
		return
	}

	log.Printf("Pair approved for %s", conn.ClientID)
	conn.sendPairApproved(id.UID, id.Code)
	// Close the pair-flow PC: the connector now has uid+access_code and
	// will reconnect via the standard direct request flow.
	conn.PC.Close()
}

// HandlePairAnswer applies the connector's SDP answer to a pair-flow peer
// connection. Unlike HandleAnswer it does not require (or expect) an
// encrypted_request payload — the connector does not yet hold an access code,
// and the bidirectional-verify fingerprint check is supplanted in pair mode
// by the SAS comparison the OnOpen callback runs after the data channel
// opens.
func (c *Connection) HandlePairAnswer(sdp string) error {
	answer := webrtc.SessionDescription{
		Type: webrtc.SDPTypeAnswer,
		SDP:  sdp,
	}
	if err := c.PC.SetRemoteDescription(answer); err != nil {
		return fmt.Errorf("set remote description (pair): %w", err)
	}
	return nil
}

// sendPairApproved emits pair_approved through signaling. uid and accessCode
// are the listener's own — the connector saves them after receiving this and
// reaches the device directly on subsequent connects.
func (c *Connection) sendPairApproved(uid, accessCode string) {
	if err := c.sig.Send(signaling.Message{
		"type":        "pair_approved",
		"client_id":   c.ClientID,
		"uid":         uid,
		"access_code": accessCode,
	}); err != nil {
		log.Printf("Pair %s: send pair_approved: %v", c.ClientID, err)
	}
}

// sendPairRejected emits pair_rejected with the given reason. Reason is wire-
// stable: "sas_mismatch", "timeout", or "user_declined".
func (c *Connection) sendPairRejected(reason string) {
	if err := c.sig.Send(signaling.Message{
		"type":      "pair_rejected",
		"client_id": c.ClientID,
		"reason":    reason,
	}); err != nil {
		log.Printf("Pair %s: send pair_rejected: %v", c.ClientID, err)
	}
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

// RestartICE reconfigures the connection with the given STUN/TURN servers,
// regenerates the ICE credentials, re-gathers candidates, and re-offers to the
// browser. The browser's TURN-withhold fallback triggers this (via the signaling
// server's request_ice handler) when its direct-only attempt stalls.
//
// The iceServers are the same creds the server just pushed to the browser. We
// need them: without a STUN/TURN server we gather only host candidates, which
// are unreachable across NATs — and the browser can't even open a TURN
// permission for our real public address since it never learns it. With them we
// gather srflx + relay, so our relay ↔ the browser's relay (double relay)
// connects. DTLS is untouched by an ICE restart, so the verified fingerprint
// stands and no re-verify runs. New candidates trickle out via OnICECandidate.
func (c *Connection) RestartICE(iceServers []webrtc.ICEServer) error {
	if len(iceServers) > 0 {
		cfg := c.PC.GetConfiguration()
		cfg.ICEServers = iceServers
		if err := c.PC.SetConfiguration(cfg); err != nil {
			return fmt.Errorf("set configuration (ice restart): %w", err)
		}
	}
	offer, err := c.PC.CreateOffer(&webrtc.OfferOptions{ICERestart: true})
	if err != nil {
		return fmt.Errorf("create ice-restart offer: %w", err)
	}
	if err := c.PC.SetLocalDescription(offer); err != nil {
		return fmt.Errorf("set local description (ice restart): %w", err)
	}
	c.sig.Send(signaling.Message{
		"type":      "offer",
		"client_id": c.ClientID,
		"sdp":       c.PC.LocalDescription().SDP,
		"streams":   map[string]interface{}{},
	})
	return nil
}

// HandleRenegotiationAnswer applies the browser's answer to an ICE-restart
// re-offer. Unlike HandleAnswer it skips the bidirectional-verify decrypt: an
// ICE restart leaves DTLS (hence the verified fingerprint) unchanged, so there
// is nothing new to verify — only the fresh ICE ufrag/pwd and relay candidates
// matter.
func (c *Connection) HandleRenegotiationAnswer(sdp string) error {
	answer := webrtc.SessionDescription{
		Type: webrtc.SDPTypeAnswer,
		SDP:  sdp,
	}
	if err := c.PC.SetRemoteDescription(answer); err != nil {
		return fmt.Errorf("set remote description (ice restart): %w", err)
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
func ParseICEServers(msg signaling.Message) []webrtc.ICEServer {
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
