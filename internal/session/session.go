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
	// negotiatedVersion is fixed by the first accepted connect message.
	// Later connect messages may update the path, but cannot change the
	// transport semantics of an established data channel.
	negotiatedVersion int
	// authFails counts wrong PIN attempts on this session. After
	// maxAuthFails the data channel is closed, forcing a fresh WebRTC
	// handshake to make further guesses (rate-limits brute-force).
	authFails int

	// streams owns the independently queued and flow-controlled state for
	// every application stream. HandleMessage never waits for a worker.
	streams map[uint32]*streamState
	closed  bool
	done    chan struct{}
	close   sync.Once

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
		DC:       dc,
		PIN:      pin,
		Verbose:  verbose,
		handlers: make(map[string]streamtype.StreamHandler, len(handlers)),
		streams:  make(map[uint32]*streamState),
		done:     make(chan struct{}),
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
	if err := protocol.ValidateFramePayload(payload); err != nil {
		return err
	}
	if s.DC == nil || s.DC.ReadyState() != webrtc.DataChannelStateOpen {
		return fmt.Errorf("data channel closed")
	}
	return s.DC.Send(protocol.BuildFrame(streamID, flags, payload))
}

// HandleMessage parses a SWSP frame and routes it. Wire this to
// dc.OnMessage at peer setup time.
func (s *Session) HandleMessage(data []byte) {
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if closed {
		return
	}
	frame, err := protocol.ParseFrame(data)
	if err != nil {
		log.Printf("Failed to parse frame: %v", err)
		return
	}

	if frame.StreamID == 0 {
		s.handleControl(frame)
		return
	}

	s.handleStreamFrame(frame)
}

func (s *Session) handleStreamFrame(frame protocol.Frame) {
	// SECURITY: gate every non-stream-0 SYN on a completed handshake.
	// Without this check, an attacker who has the WebRTC channel up
	// (post bidirectional-verify) but has not sent `connect` / `auth`
	// can open application streams directly — bypassing PIN. Reported
	// by jacopotediosi against OctoPrint-BitBang 0.2.7, PR #1443.
	s.mu.Lock()
	ready := s.ready
	st := s.streams[frame.StreamID]
	s.mu.Unlock()
	if !ready {
		if frame.IsSYN() {
			log.Printf("Rejecting SYN on stream %d: session not ready (auth bypass attempt?)", frame.StreamID)
			s.rejectStream(frame.StreamID, "unauthenticated", "session is not ready")
		}
		return
	}
	if !frame.IsSYN() {
		if st == nil {
			return
		}
		if err := st.enqueue(frame); err != nil {
			s.failStream(st, receiveErrorCode(err), err.Error(), true)
		}
		return
	}
	if st != nil {
		s.failStream(st, "duplicate_syn", "duplicate SYN", true)
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
		s.rejectStream(frame.StreamID, "unsupported_stream", fmt.Sprintf("unsupported stream type: %s", peek.Type))
		return
	}

	s.mu.Lock()
	if s.closed || s.streams[frame.StreamID] != nil {
		s.mu.Unlock()
		return
	}
	st = newStreamState(frame.StreamID, handler, s.negotiatedVersion >= 4)
	s.streams[frame.StreamID] = st
	s.mu.Unlock()
	if err := st.enqueue(frame); err != nil {
		s.failStream(st, receiveErrorCode(err), err.Error(), true)
		return
	}
	go s.runStream(st)
}

func (s *Session) runStream(st *streamState) {
	stream := newStreamCtx(s, st.id)
	for {
		select {
		case <-s.done:
			return
		case <-st.done:
			return
		case frame := <-st.queue:
			if st.isClosed() {
				st.dequeued(frame)
				return
			}
			err := s.processStreamFrame(st, stream, frame)
			st.dequeued(frame)
			if err != nil {
				log.Printf("Handler error (stream %d, type %q): %v", st.id, st.handler.Type(), err)
				s.failStream(st, "handler_error", err.Error(), true)
				return
			}
		}
	}
}

func (s *Session) processStreamFrame(st *streamState, stream *streamCtx, frame protocol.Frame) error {
	if frame.IsSYN() {
		if err := st.handler.OnSYN(stream, frame.Payload, frame.IsFIN()); err != nil {
			return err
		}
		if frame.IsFIN() {
			s.markReceiveClosed(st)
		}
		return nil
	}

	if frame.IsFIN() {
		if frame.IsMORE() || st.fragmenting {
			return fmt.Errorf("FIN during fragmented message")
		}
		if err := st.handler.OnFIN(stream, frame.Payload); err != nil {
			return err
		}
		if err := s.consume(st, frame.FlowBytes()); err != nil {
			return fmt.Errorf("replenish receive window: %w", err)
		}
		s.markReceiveClosed(st)
		return nil
	}

	var err error
	if frame.IsMORE() || st.fragmenting {
		fragmentHandler, ok := st.handler.(streamtype.FragmentHandler)
		if !ok {
			return fmt.Errorf("stream type %q does not support fragmented messages", st.handler.Type())
		}
		err = fragmentHandler.OnFragment(stream, frame.Payload, frame.IsMORE())
		st.fragmenting = frame.IsMORE()
	} else {
		err = st.handler.OnDAT(stream, frame.Payload)
	}
	if err != nil {
		return err
	}
	if err := s.consume(st, frame.FlowBytes()); err != nil {
		return fmt.Errorf("replenish receive window: %w", err)
	}
	return nil
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

func (s *streamCtx) ID() uint32 { return s.id }
func (s *streamCtx) ConnectPath() string {
	s.session.mu.Lock()
	defer s.session.mu.Unlock()
	return s.session.connectPath
}

func (s *streamCtx) WriteSYN(payload []byte) error {
	return s.session.sendStreamFrame(s.id, protocol.FlagSYN, payload)
}
func (s *streamCtx) WriteDAT(payload []byte) error {
	return s.session.sendStreamFrame(s.id, protocol.FlagDAT, payload)
}
func (s *streamCtx) WriteFIN(payload []byte) error {
	return s.session.sendStreamFrame(s.id, protocol.FlagFIN, payload)
}
func (s *streamCtx) SendRaw(flags uint16, payload []byte) error {
	return s.session.sendStreamFrame(s.id, flags, payload)
}
func (s *streamCtx) BufferedAmount() uint64 {
	if s.session.DC == nil {
		return 0
	}
	return s.session.DC.BufferedAmount()
}

// Compile-time check.
var _ streamtype.Stream = (*streamCtx)(nil)
