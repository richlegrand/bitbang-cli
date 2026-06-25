package session

import (
	"encoding/json"
	"log"
	"sort"
	"time"

	"github.com/richlegrand/bitbang/internal/protocol"
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

	var msg struct {
		Type      string                 `json:"type"`
		Path      string                 `json:"path"`
		PIN       string                 `json:"pin"`
		Version   int                    `json:"version"`
		SDP       string                 `json:"sdp"`
		Candidate map[string]interface{} `json:"candidate"`
	}
	if err := json.Unmarshal(frame.Payload, &msg); err != nil {
		return
	}

	switch msg.Type {
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

func (s *Session) handleConnect(path string, _ int) {
	if path == "" {
		path = "/"
	}

	s.mu.Lock()
	s.connectPath = path
	s.mu.Unlock()

	// Notify all registered handlers so they can set up per-session state
	// (e.g. the HTTP proxy resolves its target from the connect path).
	for _, h := range s.handlers {
		if err := h.OnConnect(path); err != nil {
			log.Printf("Handler %q OnConnect rejected connect: %v", h.Type(), err)
			s.sendControlError(err.Error())
			return
		}
	}

	if s.PIN.Required() {
		log.Printf("PIN required for connection")
		authReq, _ := json.Marshal(map[string]string{"type": "auth_required"})
		_ = s.sendFrame(0, protocol.FlagSYN, authReq)
		return
	}

	s.mu.Lock()
	s.authenticated = true
	s.ready = true
	s.mu.Unlock()
	s.markReady()
	s.sendReady()
}

func (s *Session) handleAuth(pin string) {
	if !s.PIN.Required() {
		return
	}
	if s.PIN.Verify(pin) {
		log.Printf("PIN auth succeeded")
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

	ready, _ := json.Marshal(map[string]interface{}{
		"type":           "ready",
		"server_version": protocol.SWSPVersion,
		"caps":           caps,
	})
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

func (s *Session) sendControlError(message string) {
	errMsg, _ := json.Marshal(map[string]string{
		"type":    "error",
		"message": message,
	})
	_ = s.sendFrame(0, protocol.FlagSYN|protocol.FlagFIN, errMsg)
}
