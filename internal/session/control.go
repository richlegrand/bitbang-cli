package session

import (
	"encoding/json"
	"log"
	"sort"
	"time"

	"github.com/richlegrand/bitbang-cli/internal/protocol"
)

// pinFailDelay is the artificial pause before responding to a wrong
// PIN. Slows brute-force attempts without meaningfully inconveniencing
// a human who mistyped: a single typo costs an extra two seconds, an
// attacker testing 4-digit PINs is capped at ~30 attempts/minute per
// session (and the client closes the session after 3 misses anyway).
const pinFailDelay = 2 * time.Second

// maxAuthFails is how many wrong PINs a single session tolerates before
// its data channel is torn down. Combined with pinFailDelay and the
// per-listener concurrent-session cap, this bounds brute-force: an
// attacker gets 3 tries (each paced 2s) per WebRTC handshake.
const maxAuthFails = 3

// handleControl processes a stream-0 SWSP frame: connect / auth /
// auth_required / ready / auth_result / error.
func (s *Session) handleControl(frame protocol.Frame) {
	if !frame.IsSYN() {
		return
	}

	var envelope struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(frame.Payload, &envelope); err != nil {
		return
	}

	switch envelope.Type {
	case protocol.ControlWindowUpdate:
		var update protocol.WindowUpdate
		if err := json.Unmarshal(frame.Payload, &update); err != nil {
			s.resetMalformedControl(frame.Payload, err)
			return
		}
		s.applyWindowUpdate(update)
		return
	case protocol.ControlStreamReset:
		var reset protocol.StreamReset
		if err := json.Unmarshal(frame.Payload, &reset); err != nil {
			s.resetMalformedControl(frame.Payload, err)
			return
		}
		s.applyStreamReset(reset)
		return
	}

	var msg struct {
		Path      string                 `json:"path"`
		PIN       string                 `json:"pin"`
		Version   int                    `json:"version"`
		SDP       string                 `json:"sdp"`
		Candidate map[string]interface{} `json:"candidate"`
	}
	if err := json.Unmarshal(frame.Payload, &msg); err != nil {
		return
	}

	switch envelope.Type {
	case "connect":
		s.handleConnect(msg.Path, msg.Version)
	case "auth":
		s.handleAuth(msg.PIN)
	case "video_answer":
		s.mu.Lock()
		v := s.video
		s.mu.Unlock()
		if v != nil {
			v.Answer(msg.SDP)
		}
	case "video_candidate":
		s.mu.Lock()
		v := s.video
		s.mu.Unlock()
		if v != nil {
			v.Candidate(msg.Candidate)
		}
	}
}

func (s *Session) handleConnect(path string, peerVersion int) {
	if path == "" {
		path = "/"
	}

	s.mu.Lock()
	s.connectPath = path
	s.mu.Unlock()

	// Lock the transport semantics after the first accepted connect. Browser
	// navigation can send another connect to update routing, but changing flow
	// control while streams are live would make byte accounting ambiguous.
	s.mu.Lock()
	if s.negotiatedVersion == 0 {
		s.negotiatedVersion = protocol.NegotiateSWSPVersion(peerVersion)
	}
	s.mu.Unlock()

	// The PIN gate comes before any handler setup, because handler setup is
	// not passive: the HTTP proxy's OnConnect dials the target and probes it.
	// Running that first let a code-holder who does not know the PIN make the
	// listener connect to any host:port and tell open from closed by whether
	// auth_required or error came back -- an arbitrary port scan of the
	// listener's network in dynamic-target mode, before authenticating.
	//
	// Handler setup moves to notifyHandlers, called after a successful auth
	// instead.
	if s.PIN.Required() {
		log.Printf("PIN required for connection")
		authReq, _ := json.Marshal(map[string]string{"type": "auth_required"})
		_ = s.sendFrame(0, protocol.FlagSYN, authReq)
		return
	}

	if !s.notifyHandlers(path) {
		return
	}

	s.mu.Lock()
	s.authenticated = true
	s.ready = true
	s.mu.Unlock()
	s.markReady()
	s.sendReady()
}

// notifyHandlers lets every registered handler set up per-session state
// (e.g. the HTTP proxy resolves and probes its target from the connect path).
// Reports whether the session may proceed; on failure it has already sent the
// control error.
//
// Called after authentication, never before -- see the PIN gate in
// handleConnect.
func (s *Session) notifyHandlers(path string) bool {
	for _, h := range s.handlers {
		if err := h.OnConnect(path); err != nil {
			log.Printf("Handler %q OnConnect rejected connect: %v", h.Type(), err)
			s.sendControlError(err.Error())
			return false
		}
	}
	return true
}

func (s *Session) handleAuth(pin string) {
	if !s.PIN.Required() {
		return
	}
	if s.PIN.Verify(pin) {
		log.Printf("PIN auth succeeded")
		s.mu.Lock()
		path := s.connectPath
		s.mu.Unlock()
		if path == "" {
			path = "/"
		}
		// Deferred from handleConnect so an unauthenticated peer cannot make
		// the listener dial anything.
		if !s.notifyHandlers(path) {
			return
		}
		s.mu.Lock()
		s.authenticated = true
		s.ready = true
		s.mu.Unlock()
		s.markReady()
		result, _ := json.Marshal(map[string]interface{}{"type": "auth_result", "success": true})
		_ = s.sendFrame(0, protocol.FlagSYN|protocol.FlagFIN, result)
		// The client's handshake loop sits waiting for `ready` after a
		// successful auth_result — without this it would hang.
		s.sendReady()
		return
	}
	s.mu.Lock()
	s.authFails++
	fails := s.authFails
	s.mu.Unlock()
	log.Printf("PIN auth failed (%d/%d)", fails, maxAuthFails)
	time.Sleep(pinFailDelay)
	result, _ := json.Marshal(map[string]interface{}{"type": "auth_result", "success": false})
	_ = s.sendFrame(0, protocol.FlagSYN|protocol.FlagFIN, result)
	if fails >= maxAuthFails {
		log.Printf("Too many failed PIN attempts — closing data channel")
		// Closing the DC triggers the listener's OnClose teardown (which
		// also releases the unauth-session slot). Further guesses require a
		// brand-new WebRTC handshake.
		if s.DC != nil {
			_ = s.DC.Close()
		}
	}
}

// markReady fires the one-shot OnReady hook (if set) outside the lock.
func (s *Session) markReady() {
	if s.OnReady != nil {
		s.OnReady()
	}
}

func (s *Session) sendReady() {
	// Caps from the registered handler types — what stream kinds this
	// listener is willing to serve. Empty when no handlers (test paths);
	// sorted for stable wire output (otherwise map iteration order would
	// jitter and complicate snapshot tests / log diffing). The client's
	// hasCap() check is the consumer.
	caps := make([]string, 0, len(s.handlers))
	for t := range s.handlers {
		caps = append(caps, t)
	}
	sort.Strings(caps)

	// routing = "target-prefix" tells the browser bootstrap that the
	// first path segment in the URL fragment is a LAN target (proxied
	// hostname), not part of the app's own path. This is what isolates
	// cookies between different LAN hosts reached through the same UID;
	// direct adapters (bitbang-python's WSGI/ASGI) declare "direct"
	// instead, and everything under one UID shares a cookie jar.
	s.mu.Lock()
	negotiatedVersion := s.negotiatedVersion
	s.mu.Unlock()
	readyMsg := map[string]interface{}{
		"type":               "ready",
		"server_version":     protocol.SWSPVersion,
		"negotiated_version": negotiatedVersion,
		"caps":               caps,
		"routing":            "target-prefix",
	}
	// access is additive (like want_code on register): only present when
	// the listener granted a per-peer role, and old clients ignore it.
	if s.Access != "" {
		readyMsg["access"] = s.Access
	}
	ready, _ := json.Marshal(readyMsg)
	_ = s.sendFrame(0, protocol.FlagSYN|protocol.FlagFIN, ready)

	// Channel is verified and ready — kick off the video PC handshake once.
	s.mu.Lock()
	v := s.video
	start := v != nil && !s.videoStarted
	if start {
		s.videoStarted = true
	}
	s.mu.Unlock()
	if start {
		go s.startVideo(v)
	}
}

// SendError puts a stream-0 error on the wire, naming why the listener
// is about to drop this session. Callers outside the handshake use it to
// say goodbye before closing: the browser bootstrap renders a stream-0
// error at any point in a session, and the CLI client surfaces one from
// its post-handshake control handler.
func (s *Session) SendError(message string) { s.sendControlError(message) }

// goodbyeGrace bounds how long a parting message may take to leave the
// data channel. Long enough for a frame to clear an ordinary link, short
// enough that a peer which has already vanished is not waited on.
const goodbyeGrace = 2 * time.Second

// Goodbye tells the peer why its session is ending and stops serving it,
// both at once. Pair it with WaitDrained before closing the connection:
//
//	sess.Goodbye("this link was revoked")
//	go func() { sess.WaitDrained(); conn.Close() }()
//
// Split in two because the halves have opposite requirements. Ending the
// session has to be immediate -- it is what stops a peer whose access was
// just withdrawn from opening anything else -- while the message needs
// time to leave, and waiting for that on the caller's goroutine would
// stall whatever loop is walking the peers.
func (s *Session) Goodbye(reason string) {
	s.SendError(reason)
	s.Close()
}

// WaitDrained blocks until the data channel's send buffer empties, or
// goodbyeGrace elapses.
//
// This is the half that is silent when it is missing. Closing the
// connection straight after a send discards whatever is still queued: the
// peer sees only a dead channel, treats it as a network blip, and
// reconnects, so the reason never arrives and the user is left watching a
// reconnect that cannot succeed. An empty buffer means pion handed the
// frame to SCTP rather than that it arrived, and there is no delivery
// signal to wait on, hence the short settle rather than a race against
// the wire.
func (s *Session) WaitDrained() {
	deadline := time.Now().Add(goodbyeGrace)
	for time.Now().Before(deadline) {
		if s.DC == nil || s.DC.BufferedAmount() == 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(250 * time.Millisecond)
}

func (s *Session) sendControlError(message string) {
	errMsg, _ := json.Marshal(map[string]string{
		"type":    "error",
		"message": message,
	})
	_ = s.sendFrame(0, protocol.FlagSYN|protocol.FlagFIN, errMsg)
}
