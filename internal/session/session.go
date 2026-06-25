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

	// OnReady, if set, is invoked exactly once when the session completes
	// its handshake (connect with no PIN, or a verified auth). Used by the
	// listener to release the session's unauthenticated-slot reservation.
	OnReady func()

	// State set during the stream-0 connect handshake.
	mu            sync.Mutex
	connectPath   string
	authenticated bool
	ready         bool
	// authFails counts wrong PIN attempts on this session. After
	// maxAuthFails the data channel is closed, forcing a fresh WebRTC
	// handshake to make further guesses (rate-limits brute-force).
	authFails int

	// Per-stream routing: once a SYN dispatches a stream to a handler,
	// subsequent DAT/FIN frames on that stream go to the same handler.
	streamHandler map[uint32]streamtype.StreamHandler

	// reasm buffers WebSocket message fragments per stream. A large WS
	// message arrives as DAT frames with FLAG_MORE on every non-final chunk;
	// we reassemble before delivering to OnDAT so the message boundary is
	// preserved. Only WS streams set FLAG_MORE, so byte-stream handlers
	// (http/file/shell) never populate this.
	reasm map[uint32][]byte

	// video, if set, negotiates a secondary video PeerConnection with the
	// browser over stream-0 control frames (relayed to an external media
	// helper). videoStarted guards the one-shot handshake kickoff. Both
	// guarded by mu.
	video        VideoBridge
	videoStarted bool

	// sendFrame is the function used to write a SWSP frame onto the
	// data channel. Field rather than method so unit tests can swap in
	// a capturing implementation without setting up a real WebRTC
	// peer. Production wiring (New) points it at the default DC-backed
	// implementation; nothing outside this package should reassign it
	// in production code.
	sendFrame func(streamID uint32, flags uint16, payload []byte) error
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
		reasm:         make(map[uint32][]byte),
	}
	s.sendFrame = s.dcSend
	for _, h := range handlers {
		s.handlers[h.Type()] = h
	}
	return s
}

// dcSend is the default sendFrame implementation: writes to the
// underlying data channel if it's open, otherwise reports an error.
// Tests override Session.sendFrame; production never does.
func (s *Session) dcSend(streamID uint32, flags uint16, payload []byte) error {
	if s.DC.ReadyState() != webrtc.DataChannelStateOpen {
		return fmt.Errorf("data channel closed")
	}
	return s.DC.Send(protocol.BuildFrame(streamID, flags, payload))
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
	// SECURITY: gate every non-stream-0 SYN on a completed handshake.
	// Without this check, an attacker who has the WebRTC channel up
	// (post bidirectional-verify) but has not sent `connect` / `auth`
	// can open application streams directly — bypassing PIN. Reported
	// by jacopotediosi against OctoPrint-BitBang 0.2.7, PR #1443.
	s.mu.Lock()
	ready := s.ready
	s.mu.Unlock()
	if !ready {
		log.Printf("Rejecting SYN on stream %d: session not ready (auth bypass attempt?)", frame.StreamID)
		s.sendStreamError(frame.StreamID, "unauthenticated")
		return
	}

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
	// SECURITY: same gate as handleSYN. The handler-nil short-circuit
	// below already catches the common bypass shape (no SYN was ever
	// dispatched, so streamHandler[id] is nil), but checking ready
	// explicitly is belt-and-suspenders — and gives the right log
	// signal if something tries it.
	s.mu.Lock()
	ready := s.ready
	handler := s.streamHandler[frame.StreamID]
	s.mu.Unlock()
	if !ready || handler == nil {
		return
	}

	stream := newStreamCtx(s, frame.StreamID)
	if frame.IsFIN() {
		if err := handler.OnFIN(stream, frame.Payload); err != nil {
			log.Printf("Handler OnFIN error (stream %d): %v", frame.StreamID, err)
		}
		s.mu.Lock()
		delete(s.streamHandler, frame.StreamID)
		delete(s.reasm, frame.StreamID)
		s.mu.Unlock()
		return
	}

	// Reassemble a chunked WebSocket message: FLAG_MORE marks non-final
	// fragments. Buffer until the final chunk, then deliver the whole message.
	// Only WS streams set FLAG_MORE, so byte-stream handlers (http/file/shell)
	// see each DAT delivered as-is.
	payload := frame.Payload
	if frame.IsMORE() {
		s.mu.Lock()
		s.reasm[frame.StreamID] = append(s.reasm[frame.StreamID], payload...)
		s.mu.Unlock()
		return
	}
	s.mu.Lock()
	if buf, ok := s.reasm[frame.StreamID]; ok {
		delete(s.reasm, frame.StreamID)
		payload = append(buf, payload...)
	}
	s.mu.Unlock()

	if err := handler.OnDAT(stream, payload); err != nil {
		log.Printf("Handler OnDAT error (stream %d): %v", frame.StreamID, err)
	}
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
