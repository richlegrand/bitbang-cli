package session

import (
	"encoding/json"
	"log"

	"github.com/richlegrand/bitbang/internal/protocol"
)

// handleControl processes a stream-0 SWSP frame: connect / auth /
// auth_required / ready / auth_result / error.
func (s *Session) handleControl(frame protocol.Frame) {
	if !frame.IsSYN() {
		return
	}

	var msg struct {
		Type    string   `json:"type"`
		Path    string   `json:"path"`
		PIN     string   `json:"pin"`
		Caps    []string `json:"caps"`
		Version int      `json:"version"`
	}
	if err := json.Unmarshal(frame.Payload, &msg); err != nil {
		return
	}

	switch msg.Type {
	case "connect":
		s.handleConnect(msg.Path, msg.Caps, msg.Version)
	case "auth":
		s.handleAuth(msg.PIN)
	}
}

func (s *Session) handleConnect(path string, _ []string, _ int) {
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
		return
	}
	log.Printf("PIN auth failed")
	result, _ := json.Marshal(map[string]interface{}{"type": "auth_result", "success": false})
	_ = s.sendFrame(0, protocol.FlagSYN|protocol.FlagFIN, result)
}

func (s *Session) sendReady() {
	caps := make([]string, 0, len(s.handlers))
	for k := range s.handlers {
		caps = append(caps, k)
	}
	ready, _ := json.Marshal(map[string]interface{}{
		"type":           "ready",
		"caps":           caps,
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
