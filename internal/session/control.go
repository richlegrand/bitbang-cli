package session

import (
	"encoding/json"
	"log"
	"time"

	"github.com/richlegrand/bitbang/internal/protocol"
)

// pinFailDelay is the artificial pause before responding to a wrong
// PIN. Slows brute-force attempts without meaningfully inconveniencing
// a human who mistyped: a single typo costs an extra two seconds, an
// attacker testing 4-digit PINs is capped at ~30 attempts/minute per
// session (and the client closes the session after 3 misses anyway).
const pinFailDelay = 2 * time.Second

// handleControl processes a stream-0 SWSP frame: connect / auth /
// auth_required / ready / auth_result / error.
func (s *Session) handleControl(frame protocol.Frame) {
	if !frame.IsSYN() {
		return
	}

	var msg struct {
		Type    string `json:"type"`
		Path    string `json:"path"`
		PIN     string `json:"pin"`
		Version int    `json:"version"`
	}
	if err := json.Unmarshal(frame.Payload, &msg); err != nil {
		return
	}

	switch msg.Type {
	case "connect":
		s.handleConnect(msg.Path, msg.Version)
	case "auth":
		s.handleAuth(msg.PIN)
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
		result, _ := json.Marshal(map[string]interface{}{"type": "auth_result", "success": true})
		_ = s.sendFrame(0, protocol.FlagSYN|protocol.FlagFIN, result)
		// The client's handshake loop sits waiting for `ready` after a
		// successful auth_result — without this it would hang.
		s.sendReady()
		return
	}
	log.Printf("PIN auth failed")
	time.Sleep(pinFailDelay)
	result, _ := json.Marshal(map[string]interface{}{"type": "auth_result", "success": false})
	_ = s.sendFrame(0, protocol.FlagSYN|protocol.FlagFIN, result)
}

func (s *Session) sendReady() {
	ready, _ := json.Marshal(map[string]interface{}{
		"type":           "ready",
		"server_version": protocol.SWSPVersion,
	})
	_ = s.sendFrame(0, protocol.FlagSYN|protocol.FlagFIN, ready)
}

func (s *Session) sendControlError(message string) {
	errMsg, _ := json.Marshal(map[string]string{
		"type":    "error",
		"message": message,
	})
	_ = s.sendFrame(0, protocol.FlagSYN|protocol.FlagFIN, errMsg)
}
