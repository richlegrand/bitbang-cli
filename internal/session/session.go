// Package session manages the per-WebRTC-peer lifecycle of a SWSP session.
//
// One Session per accepted peer. Owns the data channel. Handles stream-0
// control messages (connect, auth_required, auth, auth_result, ready,
// error) directly. Dispatches stream-1+ SYN to a registered StreamHandler
// based on the SYN payload's `type` field; routes subsequent DAT/FIN
// frames to the same handler.
package session

import (
	"encoding/json"
	"fmt"
	"log"
	"sync"

	"github.com/pion/webrtc/v4"

	"github.com/richlegrand/bitbang/internal/auth"
	"github.com/richlegrand/bitbang/internal/protocol"
	"github.com/richlegrand/bitbang/internal/streamtype"
)

// Session owns a single peer's data channel and routes SWSP frames to the
// appropriate StreamHandler.
type Session struct {
	DC      *webrtc.DataChannel
	PIN     *auth.PINAuth
	Verbose bool

	// handlers is the set of registered StreamHandlers, keyed by their
	// Type() string. Populated once at session creation; not modified
	// after the data channel opens.
	handlers map[string]streamtype.StreamHandler

	// State set during the stream-0 connect handshake.
	mu            sync.Mutex
	connectPath   string
	authenticated bool
	ready         bool

	// Per-stream routing: once a SYN dispatches a stream to a handler,
	// subsequent DAT/FIN frames on that stream go to the same handler.
	streamHandler map[uint32]streamtype.StreamHandler
}

// New creates a Session bound to the given data channel. The handlers
// list is the set of StreamHandlers to dispatch to based on SYN type.
// Each handler's Type() must be unique within the session.
func New(dc *webrtc.DataChannel, pin *auth.PINAuth, verbose bool, handlers ...streamtype.StreamHandler) *Session {
	s := &Session{
		DC:            dc,
		PIN:           pin,
		Verbose:       verbose,
		handlers:      make(map[string]streamtype.StreamHandler, len(handlers)),
		streamHandler: make(map[uint32]streamtype.StreamHandler),
	}
	for _, h := range handlers {
		s.handlers[h.Type()] = h
	}
	return s
}

// HandleMessage parses a SWSP frame and routes it. Wire this to
// dc.OnMessage at peer setup time.
func (s *Session) HandleMessage(data []byte) {
	frame, err := protocol.ParseFrame(data)
	if err != nil {
		log.Printf("Failed to parse frame: %v", err)
		return
	}

	if frame.StreamID == 0 {
		s.handleControl(frame)
		return
	}

	if frame.IsSYN() {
		s.handleSYN(frame)
	} else {
		s.handleBody(frame)
	}
}

func (s *Session) handleSYN(frame protocol.Frame) {
	// Peek at the type. SYNs without an explicit type default to "http"
	// for backwards-compatibility with v2 wire format during transition.
	var peek struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal(frame.Payload, &peek)
	if peek.Type == "" {
		peek.Type = "http"
	}

	handler, ok := s.handlers[peek.Type]
	if !ok {
		log.Printf("No handler for stream type %q (stream %d)", peek.Type, frame.StreamID)
		s.sendStreamError(frame.StreamID, fmt.Sprintf("unsupported stream type: %s", peek.Type))
		return
	}

	s.mu.Lock()
	s.streamHandler[frame.StreamID] = handler
	s.mu.Unlock()

	stream := newStreamCtx(s, frame.StreamID)
	if err := handler.OnSYN(stream, frame.Payload, frame.IsFIN()); err != nil {
		log.Printf("Handler OnSYN error (stream %d, type %q): %v", frame.StreamID, peek.Type, err)
	}
	if frame.IsFIN() {
		// SYN|FIN: stream is complete in one frame. Clean up the routing
		// table; handler's OnSYN already handled the final state.
		s.mu.Lock()
		delete(s.streamHandler, frame.StreamID)
		s.mu.Unlock()
	}
}

func (s *Session) handleBody(frame protocol.Frame) {
	s.mu.Lock()
	handler := s.streamHandler[frame.StreamID]
	s.mu.Unlock()
	if handler == nil {
		return
	}

	stream := newStreamCtx(s, frame.StreamID)
	if frame.IsFIN() {
		if err := handler.OnFIN(stream, frame.Payload); err != nil {
			log.Printf("Handler OnFIN error (stream %d): %v", frame.StreamID, err)
		}
		s.mu.Lock()
		delete(s.streamHandler, frame.StreamID)
		s.mu.Unlock()
		return
	}

	if err := handler.OnDAT(stream, frame.Payload); err != nil {
		log.Printf("Handler OnDAT error (stream %d): %v", frame.StreamID, err)
	}
}

// sendFrame is used by streamCtx (and by control.go).
func (s *Session) sendFrame(streamID uint32, flags uint16, payload []byte) error {
	if s.DC.ReadyState() != webrtc.DataChannelStateOpen {
		return fmt.Errorf("data channel closed")
	}
	return s.DC.Send(protocol.BuildFrame(streamID, flags, payload))
}

// sendStreamError sends a single-frame error response (SYN+FIN) on the
// given stream, with a JSON body that looks like a 500 HTTP response.
// Used when no handler claims the stream.
func (s *Session) sendStreamError(streamID uint32, message string) {
	errBody := map[string]interface{}{
		"status": 500,
		"headers": map[string]string{
			"Content-Type": "text/plain",
		},
	}
	meta, _ := json.Marshal(errBody)
	_ = s.sendFrame(streamID, protocol.FlagSYN, meta)
	_ = s.sendFrame(streamID, protocol.FlagDAT, []byte(message))
	_ = s.sendFrame(streamID, protocol.FlagFIN, nil)
}

// streamCtx is the per-stream context handed to handlers.
type streamCtx struct {
	id      uint32
	session *Session
}

func newStreamCtx(s *Session, id uint32) *streamCtx {
	return &streamCtx{id: id, session: s}
}

func (s *streamCtx) ID() uint32          { return s.id }
func (s *streamCtx) ConnectPath() string { return s.session.connectPath }

func (s *streamCtx) WriteSYN(payload []byte) error {
	return s.session.sendFrame(s.id, protocol.FlagSYN, payload)
}
func (s *streamCtx) WriteDAT(payload []byte) error {
	return s.session.sendFrame(s.id, protocol.FlagDAT, payload)
}
func (s *streamCtx) WriteFIN(payload []byte) error {
	return s.session.sendFrame(s.id, protocol.FlagFIN, payload)
}
func (s *streamCtx) SendRaw(flags uint16, payload []byte) error {
	return s.session.sendFrame(s.id, flags, payload)
}
func (s *streamCtx) BufferedAmount() uint64 {
	return s.session.DC.BufferedAmount()
}

// Compile-time check.
var _ streamtype.Stream = (*streamCtx)(nil)
