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
	"time"

	"github.com/pion/webrtc/v4"
	"github.com/richlegrand/bitbang/internal/icehelper"
	"github.com/richlegrand/bitbang/internal/identity"
	"github.com/richlegrand/bitbang/internal/pairing"
	"github.com/richlegrand/bitbang/internal/signaling"
)

// relayAcceptanceMinWait is how long the device's (ICE-controlling) pion agent
// withholds nominating a relay candidate pair, giving a direct (host/srflx)
// pair time to win even on a slow device. Generous on purpose: TURN bandwidth
// is the scarce resource and we don't mind waiting for a direct path. See the
// "Favoring direct on slow & embedded devices" note in bitbang/CONVENTIONS.md.
const relayAcceptanceMinWait = 8 * time.Second

// relayWaitFor returns how long the device (the ICE-controlling agent)
// withholds nominating a relay candidate pair. Normally the full grace, so a
// direct (host/srflx) pair can win the race; but when the connector forced
// relay (--relay/?relay, surfaced as force_relay on the request) it gathers
// relay-only — there is no direct path to wait for, so the grace would be dead
// time and we skip it (0). A missing or non-bool force_relay defaults to the
// full grace.
func relayWaitFor(msg signaling.Message) time.Duration {
	if forceRelay, _ := msg["force_relay"].(bool); forceRelay {
		return 0
	}
	return relayAcceptanceMinWait
}

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
	// data-channel OnOpen runs the commitment + SAS confirmation flow
	// (handlePairRequestOnOpen) instead of sending verify_nonce_hash.
	PairingMode bool

	// dcInbox carries inbound data-channel frames to the pairing handshake
	// goroutine. Non-nil only in pair mode; the regular flow routes inbound
	// frames to OnMessage instead. The pairing handshake (commit/challenge/
	// reveal) needs to *read* from the data channel, which OnMessage (a
	// fire-and-forget callback) can't model.
	dcInbox chan []byte

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
// shared boilerplate (icehelper.ParseICEServers, PC creation, DC creation, trickle
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
	connStart := time.Now()

	iceServers := icehelper.ParseICEServers(s.msg)

	// Single-phase ICE bias toward direct: the device is the offerer and thus
	// the ICE-controlling agent, so the candidate-pair nomination decision is
	// made here. pion's controlling selector won't nominate a relay pair until
	// relayAcceptanceMinWait has elapsed since checks began, while a direct
	// (host/srflx) pair becomes nominatable almost immediately — so a direct
	// pair wins whenever it can validate within the window, even on a slow
	// device where the always-reachable relay would otherwise win the race.
	// The relay candidate is the connector's (the device is STUN-only), but
	// isNominatable keys on candidate *type*, so this gates it correctly.
	//
	// Exception: a forced-relay connect (--relay/?relay) gathers relay-only,
	// so there is no direct path to wait for — relayWaitFor skips the grace.
	se := webrtc.SettingEngine{}
	se.SetRelayAcceptanceMinWait(relayWaitFor(s.msg))
	api := webrtc.NewAPI(webrtc.WithSettingEngine(se))

	pc, err := api.NewPeerConnection(webrtc.Configuration{ICEServers: iceServers})
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
	if s.pairingMode {
		conn.dcInbox = make(chan []byte, 8)
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
		// Pair mode: hand frames to the handshake goroutine via dcInbox.
		// Regular mode: fire the session's OnMessage callback.
		if conn.dcInbox != nil {
			select {
			case conn.dcInbox <- m.Data:
			default: // handshake not reading / done — drop
			}
			return
		}
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
		if state == webrtc.PeerConnectionStateConnected {
			logSelectedPair(s.logTag, s.clientID, pc, time.Since(connStart))
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
		s.sig.Send(signaling.Message{
			"type":      "candidate",
			"client_id": s.clientID,
			"candidate": icehelper.CandidateMap(candidate),
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

// logSelectedPair logs which ICE candidate pair won and how long it took, so
// direct-vs-relay outcomes (and the effect of relayAcceptanceMinWait) can be
// measured on slow devices. Best-effort: a not-yet-ready transport or any
// lookup error is silently ignored.
func logSelectedPair(logTag, clientID string, pc *webrtc.PeerConnection, elapsed time.Duration) {
	sctp := pc.SCTP()
	if sctp == nil {
		return
	}
	dtls := sctp.Transport()
	if dtls == nil {
		return
	}
	ice := dtls.ICETransport()
	if ice == nil {
		return
	}
	pair, err := ice.GetSelectedCandidatePair()
	if err != nil || pair == nil || pair.Local == nil || pair.Remote == nil {
		return
	}
	kind := "DIRECT"
	if pair.Local.Typ == webrtc.ICECandidateTypeRelay || pair.Remote.Typ == webrtc.ICECandidateTypeRelay {
		kind = "RELAY"
	}
	log.Printf("%s connected for %s via %s in %v (local=%s remote=%s)",
		logTag, clientID, kind, elapsed.Round(time.Millisecond), pair.Local.Typ, pair.Remote.Typ)
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
// (offer / answer / candidate); the difference is what happens once the data
// channel opens (handlePairRequestOnOpen): instead of sending verify_nonce_hash,
// the listener runs the commit→challenge→reveal exchange, computes the 6-digit
// SAS, prompts its operator to type what the connector reads aloud, and on a
// match delivers {uid, public_key, access_code} over the SAS-verified *data
// channel* (plus a bare pair_approved over signaling). On any failure it sends
// pair_rejected over signaling.
//
// prompt is the PromptFunc used to read each SAS attempt; pass
// pairing.DefaultTTYPrompt for interactive listeners. The SAS itself is
// never written to the terminal — there is nothing to blindly approve.
//
// The connection closes itself in all post-DTLS outcomes. The connector never
// sends application traffic over this connection; once it has the credentials
// it reconnects via the standard /ws/client/<uid> direct flow.
//
// pair_request carries phase-1 STUN ice_servers (stamped by the signaling
// server, mirroring a regular request); icehelper.ParseICEServers picks them up.
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
			// Run the handshake off the OnOpen goroutine so dc.OnMessage can
			// keep delivering the connector's commit/reveal frames into
			// dcInbox while the handshake (and the blocking SAS prompt) runs.
			go handlePairRequestOnOpen(conn, prompt, id)
		},
	})
}

// pairStepTimeout bounds each wait for a connector data-channel frame during
// the commitment handshake. The connector sends commit/reveal back-to-back, so
// this only needs to cover network + the connector's own SAS display; generous.
const pairStepTimeout = 30 * time.Second

// recvDC blocks for the next inbound data-channel frame on a pairing
// connection, or returns ok=false on timeout / channel teardown.
func (c *Connection) recvDC(timeout time.Duration) ([]byte, bool) {
	select {
	case data, ok := <-c.dcInbox:
		return data, ok
	case <-time.After(timeout):
		return nil, false
	}
}

// handlePairRequestOnOpen is the post-DTLS body for the pair flow, run on its
// own goroutine. It:
//
//  1. runs the commit→challenge→reveal exchange over the data channel (the
//     connector commits its nonce r_c; the listener challenges with a fresh
//     r_d; the connector reveals r_c, which the listener verifies against the
//     commitment) — this is what makes the short SAS non-grindable,
//  2. computes the 6-digit SAS from (r_c, r_d, both fingerprints),
//  3. prompts the listener operator to type the value the connector is reading
//     aloud (blind entry — the SAS is never displayed here),
//  4. on a match, delivers {uid, public_key, access_code} over the *data
//     channel* (DTLS-encrypted, now SAS-verified — the server can't read it)
//     and a bare pair_approved over signaling; on any failure, pair_rejected.
//
// Closes the PC in all outcomes — the connector reconnects via the standard
// direct flow with the delivered access code.
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

	// 1a. Receive the connector's commitment.
	data, ok := conn.recvDC(pairStepTimeout)
	if !ok || pairing.PairMessageType(data) != pairing.MsgPairCommit {
		log.Printf("Pair %s: no pair_commit", conn.ClientID)
		conn.sendPairRejected("timeout")
		conn.PC.Close()
		return
	}
	commit, ok := pairing.ParsePairCommit(data)
	if !ok {
		conn.sendPairRejected("user_declined")
		conn.PC.Close()
		return
	}

	// 1b. Challenge with a fresh device nonce.
	rd, err := pairing.NewNonce()
	if err != nil {
		log.Printf("Pair %s: nonce: %v", conn.ClientID, err)
		conn.sendPairRejected("user_declined")
		conn.PC.Close()
		return
	}
	if err := conn.DC.Send(pairing.BuildPairChallenge(rd)); err != nil {
		conn.PC.Close()
		return
	}

	// 1c. Receive the reveal and verify it opens the commitment.
	data, ok = conn.recvDC(pairStepTimeout)
	if !ok || pairing.PairMessageType(data) != pairing.MsgPairReveal {
		log.Printf("Pair %s: no pair_reveal", conn.ClientID)
		conn.sendPairRejected("timeout")
		conn.PC.Close()
		return
	}
	rc, ok := pairing.ParsePairReveal(data)
	if !ok || !pairing.VerifyCommitment(commit, rc) {
		log.Printf("Pair %s: commitment did not open", conn.ClientID)
		conn.sendPairRejected("sas_mismatch")
		conn.PC.Close()
		return
	}

	// 2. Compute the SAS. Never logged — the operator must hear it from the
	//    connector, not read it here.
	sas := pairing.ComputeSAS(rc, rd, localFp, remoteFp)

	// 3. Blind operator entry.
	reason, ok := pairing.PromptForSAS(sas, prompt)
	if !ok {
		log.Printf("Pair rejected for %s: %s", conn.ClientID, reason)
		conn.sendPairRejected(reason)
		conn.PC.Close()
		return
	}

	// 4. Deliver credentials over the verified data channel, plus a bare
	//    approval over signaling.
	log.Printf("Pair approved for %s", conn.ClientID)
	if err := conn.DC.Send(pairing.BuildPairCredentials(id.UID, id.PublicB64, id.Code)); err != nil {
		log.Printf("Pair %s: send credentials: %v", conn.ClientID, err)
	}
	conn.sendPairApproved()
	// Let the credentials frame flush before tearing down, so the connector
	// reliably receives it (it reads creds, then closes its side).
	for i := 0; i < 200 && conn.DC.BufferedAmount() > 0; i++ {
		time.Sleep(5 * time.Millisecond)
	}
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

// sendPairApproved emits a bare pair_approved over signaling — a non-secret
// "approved" ack. The actual credentials (uid, public_key, access_code) are
// delivered over the SAS-verified data channel, never over signaling, so the
// server can't read them.
func (c *Connection) sendPairApproved() {
	if err := c.sig.Send(signaling.Message{
		"type":      "pair_approved",
		"client_id": c.ClientID,
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
	candidate, ok := icehelper.CandidateInit(candidateData)
	if !ok {
		return nil // empty candidate = end of candidates
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
