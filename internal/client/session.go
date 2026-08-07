package client

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"

	"github.com/pion/webrtc/v4"

	"github.com/richlegrand/bitbang/internal/protocol"
)

// stderr is the package-wide log sink for connection progress + debug
// chatter. cp prints user-facing data to stdout, so all diagnostic noise
// from this package routes here instead.
var stderr = os.Stderr

var (
	errStreamQueueFull       = errors.New("stream receive queue full")
	errReceiveWindowExceeded = errors.New("stream receive window exceeded")
	errStreamClosed          = errors.New("stream closed")
	errStreamFinished        = errors.New("stream direction already finished")
	errStreamNotStarted      = errors.New("stream has not sent SYN")
)

// Session wraps the data channel after bidirectional verify has succeeded
// and the control-stream `ready` message has been received. From here on
// the caller drives SWSP file (or future shell/tcp/etc.) streams.
//
// Streams are multiplexed by ID inside the channel. The client picks odd
// numbered IDs starting at 1, leaving the device free to pick even IDs
// for future device-initiated streams; for v1 only the client opens
// streams, so the parity doesn't strictly matter.
type Session struct {
	DC      *webrtc.DataChannel
	Verbose bool

	// ServerCaps and ServerVersion come from the listener's `ready` and
	// let callers gate behavior (e.g. don't try `file` ops if the server
	// doesn't advertise it).
	ServerCaps        []string
	ServerVersion     int
	NegotiatedVersion int

	nextStreamID uint32
	mu           sync.Mutex
	streams      map[uint32]*stream
	closed       atomic.Bool
	done         chan struct{}
	closeOnce    sync.Once

	// sendFrameOverride lets unit tests capture wire output without creating a
	// real WebRTC data channel. Production sessions leave it nil.
	sendFrameOverride func(streamID uint32, flags uint16, payload []byte) error
}

// stream is the per-stream state held by the session. The dispatcher writes
// into pending without waiting for the caller; a dedicated worker preserves
// frame order while handing frames to the public inbox.
type stream struct {
	id        uint32
	inbox     chan protocol.Frame
	pending   chan protocol.Frame
	abandoned chan struct{}
	abandon   sync.Once
	closeOnce sync.Once

	queueMu       sync.Mutex
	queuedBytes   int
	queuedFrames  int
	receivedBytes uint64
	consumedBytes uint64
	advertisedMax uint64
	lastUpdateAt  uint64
	windowActive  bool

	sendMu           sync.Mutex
	writeMu          sync.Mutex
	sentBytes        uint64
	sendLimit        uint64
	sendWake         chan struct{}
	streamErr        error
	inboundFIN       atomic.Bool
	inboundDelivered atomic.Bool
	outboundFIN      atomic.Bool
}

// newSession constructs a Session bound to a data channel. The peer is
// passed in so the session layer can read off the bidirectional-verify
// nonce expected on the first stream-0 frame; once verify completes the
// session takes over the DC message stream.
func newSession(p *Peer) *Session {
	return &Session{
		DC:           p.DC,
		nextStreamID: 1,
		streams:      make(map[uint32]*stream),
		done:         make(chan struct{}),
	}
}

// handshake runs the client-side control protocol after the data channel
// opens: verify_nonce_hash → connect → (auth_required + auth)* → ready.
// Returns once `ready` arrives or the channel dies.
//
// pinPrompt is called when the listener replies auth_required; it returns
// the PIN to send (and an error to abort). cp passes a stdin-based
// implementation that uses golang.org/x/term to hide the input.
func (s *Session) handshake(p *Peer, path string, caps []string, pinPrompt func(retry int) (string, error)) error {
	// 1. verify_nonce_hash must be the *first* control frame.
	first, ok := <-p.DCMessages()
	if !ok {
		return errors.New("data channel closed before verify")
	}
	frame, err := protocol.ParseFrame(first)
	if err != nil {
		return fmt.Errorf("parse verify frame: %w", err)
	}
	if frame.StreamID != 0 || !frame.IsSYN() {
		return fmt.Errorf("expected verify_nonce_hash on stream 0, got stream %d flags %#x", frame.StreamID, frame.Flags)
	}
	var verify struct {
		Type string `json:"type"`
		Hash string `json:"hash"`
	}
	if err := json.Unmarshal(frame.Payload, &verify); err != nil {
		return fmt.Errorf("parse verify_nonce_hash: %w", err)
	}
	if verify.Type != "verify_nonce_hash" {
		return fmt.Errorf("expected verify_nonce_hash, got %q", verify.Type)
	}
	want := expectedNonceHash(p.Nonce())
	if verify.Hash != want {
		return errors.New("bidirectional verify failed: device did not prove possession of private key")
	}
	if s.Verbose {
		fmt.Fprintln(stderr, "[client] bidirectional verify OK")
	}

	// 2. Send `connect` with caps + version.
	connectMsg, _ := json.Marshal(map[string]interface{}{
		"type":    "connect",
		"path":    path,
		"caps":    caps,
		"version": protocol.SWSPVersion,
	})
	if err := s.sendControlSYN(connectMsg); err != nil {
		return fmt.Errorf("send connect: %w", err)
	}

	// 3. Drain stream-0 SYN frames until we see `ready` (success) or
	// `error` (give up). `auth_required` triggers a PIN prompt and
	// retries; `auth_result.success=false` triggers another retry up to
	// a small cap.
	retry := 0
	for {
		msg, ok := <-p.DCMessages()
		if !ok {
			return errors.New("data channel closed during handshake")
		}
		f, err := protocol.ParseFrame(msg)
		if err != nil {
			return fmt.Errorf("parse control frame: %w", err)
		}
		// During handshake everything should be on stream 0. If a SYN
		// arrives on a non-zero stream the listener is misbehaving;
		// surface as a protocol error.
		if f.StreamID != 0 {
			return fmt.Errorf("unexpected non-control frame during handshake (stream %d)", f.StreamID)
		}
		if !f.IsSYN() {
			continue
		}
		var ctl struct {
			Type              string   `json:"type"`
			Message           string   `json:"message"`
			Success           bool     `json:"success"`
			Caps              []string `json:"caps"`
			ServerVersion     int      `json:"server_version"`
			NegotiatedVersion int      `json:"negotiated_version"`
		}
		_ = json.Unmarshal(f.Payload, &ctl)

		switch ctl.Type {
		case "ready":
			s.ServerCaps = ctl.Caps
			if err := s.setNegotiatedVersion(ctl.ServerVersion, ctl.NegotiatedVersion); err != nil {
				return err
			}
			if s.Verbose {
				fmt.Fprintf(stderr, "[client] device ready (server v%d, negotiated v%d, caps: %v)\n",
					s.ServerVersion, s.NegotiatedVersion, s.ServerCaps)
			}
			return nil
		case "auth_required":
			if pinPrompt == nil {
				return errors.New("listener requires PIN but no prompt provided")
			}
			pin, err := pinPrompt(retry)
			if err != nil {
				return fmt.Errorf("PIN prompt: %w", err)
			}
			retry++
			authMsg, _ := json.Marshal(map[string]string{"type": "auth", "pin": pin})
			if err := s.sendControlSYN(authMsg); err != nil {
				return fmt.Errorf("send auth: %w", err)
			}
		case "auth_result":
			if ctl.Success {
				// Listener follows up with `ready` immediately; loop.
				continue
			}
			if retry >= 3 {
				return errors.New("PIN authentication failed (3 attempts)")
			}
			fmt.Fprintln(stderr, "Incorrect PIN, try again.")
			pin, err := pinPrompt(retry)
			if err != nil {
				return fmt.Errorf("PIN prompt: %w", err)
			}
			retry++
			authMsg, _ := json.Marshal(map[string]string{"type": "auth", "pin": pin})
			if err := s.sendControlSYN(authMsg); err != nil {
				return fmt.Errorf("send auth: %w", err)
			}
		case "error":
			return fmt.Errorf("listener error: %s", ctl.Message)
		default:
			if s.Verbose {
				fmt.Fprintf(stderr, "[client] ignoring control message type=%q\n", ctl.Type)
			}
		}
	}
}

func (s *Session) setNegotiatedVersion(serverVersion, negotiatedVersion int) error {
	if serverVersion == 0 {
		// v2 listeners send {type:"ready"} with no version.
		serverVersion = 2
	}
	maxVersion := protocol.NegotiateSWSPVersion(serverVersion)
	if maxVersion < 2 {
		return fmt.Errorf("listener returned unsupported server version %d", serverVersion)
	}
	if negotiatedVersion == 0 {
		negotiatedVersion = maxVersion
	} else if negotiatedVersion < 2 || negotiatedVersion > maxVersion {
		return fmt.Errorf("listener returned invalid negotiated version %d", negotiatedVersion)
	}
	s.ServerVersion = serverVersion
	s.NegotiatedVersion = negotiatedVersion
	return nil
}

// startDispatcher takes over the DC message queue from the session layer and
// routes inbound frames to bounded per-stream queues. It never waits for an
// individual stream consumer, so one stalled stream cannot delay the rest of
// the session. Run as a goroutine after handshake completes; exits when the DC
// closes.
func (s *Session) startDispatcher(p *Peer) {
	go func() {
		defer s.finishStreams()
		for {
			var msg []byte
			select {
			case <-s.done:
				return
			case <-p.DCClosed():
				s.markClosed()
				return
			case msg = <-p.DCMessages():
			}
			frame, err := protocol.ParseFrame(msg)
			if err != nil {
				if s.Verbose {
					fmt.Fprintf(stderr, "[client] dropped malformed frame: %v\n", err)
				}
				continue
			}
			if frame.StreamID == 0 {
				if s.flowControlEnabled() && s.handleControl(frame) {
					continue
				}
				// Other late stream-0 messages are informational in the
				// established session and can be ignored.
				if s.Verbose {
					fmt.Fprintf(stderr, "[client] ignoring late stream-0 frame flags=%#x\n", frame.Flags)
				}
				continue
			}
			s.mu.Lock()
			st := s.streams[frame.StreamID]
			s.mu.Unlock()
			if st == nil {
				if s.Verbose {
					fmt.Fprintf(stderr, "[client] frame for unknown stream %d (flags %#x) — dropping\n", frame.StreamID, frame.Flags)
				}
				continue
			}
			if err := st.enqueue(frame, s.flowControlEnabled()); err != nil {
				// The stream has exceeded its private queue or was abandoned.
				// Detach only this stream; the dispatcher must remain available
				// to every other stream sharing the session.
				if !errors.Is(err, errStreamClosed) {
					code := "receive_overflow"
					switch {
					case errors.Is(err, errReceiveWindowExceeded):
						code = "flow_control_violation"
					case errors.Is(err, errStreamFinished), errors.Is(err, errStreamNotStarted):
						code = "protocol_error"
					}
					s.failStream(st, code, err.Error(), true)
				}
				continue
			}
		}
	}()
}

// OpenStream allocates a new outbound stream and returns its handle.
// The caller writes the SYN/DAT/FIN frames and reads inbound frames via
// the returned Stream's Inbox method.
func (s *Session) OpenStream() *Stream {
	s.mu.Lock()
	id := s.nextStreamID
	// Client uses odd IDs (1,3,5...) so any future device-initiated
	// stream can use even IDs without an ID-allocation negotiation.
	s.nextStreamID += 2
	st := &stream{
		id:        id,
		inbox:     make(chan protocol.Frame),
		pending:   make(chan protocol.Frame, protocol.MaxQueuedStreamFrames),
		abandoned: make(chan struct{}),
		sendWake:  make(chan struct{}),
	}
	if s.closed.Load() {
		s.mu.Unlock()
		st.fail(errors.New("data channel closed"))
		st.closeOnce.Do(func() { close(st.inbox) })
		return &Stream{s: s, st: st}
	}
	s.streams[id] = st
	s.mu.Unlock()
	go s.deliverStream(st)
	return &Stream{s: s, st: st}
}

func (s *Session) detachStream(id uint32, st *stream) bool {
	s.mu.Lock()
	removed := false
	if s.streams[id] == st {
		delete(s.streams, id)
		removed = true
	}
	s.mu.Unlock()
	return removed
}

func (s *Session) detachFinishedStream(st *stream) {
	if st.inboundDelivered.Load() && st.outboundFIN.Load() {
		s.detachStream(st.id, st)
	}
}

func (s *Session) findStream(id uint32) *stream {
	s.mu.Lock()
	st := s.streams[id]
	s.mu.Unlock()
	return st
}

func (s *Session) failStream(st *stream, code, message string, notifyPeer bool) {
	if !s.detachStream(st.id, st) {
		return
	}
	st.fail(errors.New(message))
	if notifyPeer {
		go func() {
			// Preserve terminal ordering on the reliable data channel: any
			// frame that already reserved credit completes before the reset,
			// while blocked/future writers observe st.fail and stop.
			st.writeMu.Lock()
			defer st.writeMu.Unlock()
			if s.flowControlEnabled() {
				_ = s.sendStreamReset(st.id, code, message)
				return
			}
			_ = s.sendRawFrame(st.id, protocol.FlagFIN, nil)
		}()
	}
}

func (s *Session) deliverStream(st *stream) {
	defer st.closeOnce.Do(func() { close(st.inbox) })
	for {
		select {
		case frame := <-st.pending:
			select {
			case st.inbox <- frame:
				maxBytes, update := st.consume(frame, s.flowControlEnabled())
				if update && !frame.IsFIN() {
					if err := s.sendWindowUpdate(st.id, maxBytes); err != nil {
						s.failStream(st, "control_send_failed", err.Error(), false)
						return
					}
				}
				if frame.IsFIN() {
					// Keep the state session-owned until FIN reaches the caller.
					// Otherwise an abandoned inbox can strand this worker after
					// both wire directions have already finished.
					st.inboundDelivered.Store(true)
					s.detachFinishedStream(st)
					return
				}
			case <-st.abandoned:
				st.release(frame)
				return
			case <-s.done:
				st.release(frame)
				return
			}
		case <-st.abandoned:
			return
		case <-s.done:
			return
		}
	}
}

func (st *stream) enqueue(frame protocol.Frame, enforceWindow bool) error {
	if st.inboundFIN.Load() {
		return errStreamFinished
	}
	flowBytes := frame.FlowBytes()
	st.queueMu.Lock()
	if enforceWindow && !st.windowActive {
		st.queueMu.Unlock()
		return errStreamNotStarted
	}
	if enforceWindow && (st.receivedBytes > st.advertisedMax || flowBytes > st.advertisedMax-st.receivedBytes) {
		st.queueMu.Unlock()
		return errReceiveWindowExceeded
	}
	if st.queuedFrames >= protocol.MaxQueuedStreamFrames || st.queuedBytes+len(frame.Payload) > protocol.InitialStreamWindow {
		st.queueMu.Unlock()
		return errStreamQueueFull
	}
	st.queuedFrames++
	st.queuedBytes += len(frame.Payload)
	st.receivedBytes += flowBytes
	if frame.IsFIN() {
		st.inboundFIN.Store(true)
	}
	st.queueMu.Unlock()

	select {
	case st.pending <- frame:
		return nil
	case <-st.abandoned:
		st.release(frame)
		return errStreamClosed
	default:
		// queuedFrames includes the frame a worker may currently be handing
		// to the caller, so this should only be reachable during teardown.
		st.release(frame)
		return errStreamQueueFull
	}
}

func (st *stream) release(frame protocol.Frame) {
	st.queueMu.Lock()
	st.queuedFrames--
	st.queuedBytes -= len(frame.Payload)
	st.queueMu.Unlock()
}

func (st *stream) consume(frame protocol.Frame, flowControl bool) (uint64, bool) {
	flowBytes := frame.FlowBytes()
	st.queueMu.Lock()
	st.queuedFrames--
	st.queuedBytes -= len(frame.Payload)
	if !flowControl || flowBytes == 0 {
		st.queueMu.Unlock()
		return 0, false
	}
	st.consumedBytes += flowBytes
	if st.consumedBytes-st.lastUpdateAt < protocol.StreamWindowUpdateThreshold {
		st.queueMu.Unlock()
		return 0, false
	}
	st.lastUpdateAt = st.consumedBytes
	st.advertisedMax = protocol.InitialStreamWindow + st.consumedBytes
	maxBytes := st.advertisedMax
	st.queueMu.Unlock()
	return maxBytes, true
}

func (st *stream) activateInitialWindow() {
	st.queueMu.Lock()
	activate := !st.windowActive
	if activate {
		st.advertisedMax = protocol.InitialStreamWindow
		st.windowActive = true
	}
	st.queueMu.Unlock()
	if activate {
		st.updateSendLimit(protocol.InitialStreamWindow)
	}
}

func (st *stream) initialWindowActive() bool {
	st.queueMu.Lock()
	defer st.queueMu.Unlock()
	return st.windowActive
}

func (st *stream) reserveSend(n uint64, sessionDone <-chan struct{}) error {
	for {
		st.sendMu.Lock()
		if st.streamErr != nil {
			err := st.streamErr
			st.sendMu.Unlock()
			return err
		}
		if n <= st.sendLimit-st.sentBytes {
			st.sentBytes += n
			st.sendMu.Unlock()
			return nil
		}
		wake := st.sendWake
		st.sendMu.Unlock()

		select {
		case <-wake:
		case <-st.abandoned:
			return errStreamClosed
		case <-sessionDone:
			return errors.New("data channel closed")
		}
	}
}

func (st *stream) updateSendLimit(maxBytes uint64) {
	st.sendMu.Lock()
	if maxBytes > st.sendLimit {
		st.sendLimit = maxBytes
		close(st.sendWake)
		st.sendWake = make(chan struct{})
	}
	st.sendMu.Unlock()
}

func (st *stream) fail(err error) {
	st.sendMu.Lock()
	if st.streamErr == nil {
		st.streamErr = err
		close(st.sendWake)
		st.sendWake = make(chan struct{})
	}
	st.sendMu.Unlock()
	st.abandon.Do(func() { close(st.abandoned) })
}

func (s *Session) finishStreams() {
	s.markClosed()
	s.mu.Lock()
	streams := make([]*stream, 0, len(s.streams))
	for id, st := range s.streams {
		streams = append(streams, st)
		delete(s.streams, id)
	}
	s.mu.Unlock()
	for _, st := range streams {
		st.abandon.Do(func() { close(st.abandoned) })
	}
}

func (s *Session) markClosed() {
	s.closeOnce.Do(func() {
		s.closed.Store(true)
		close(s.done)
	})
}

func (s *Session) flowControlEnabled() bool {
	return s.NegotiatedVersion >= 4
}

func (s *Session) handleControl(frame protocol.Frame) bool {
	if !frame.IsSYN() {
		return false
	}
	var header struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(frame.Payload, &header); err != nil {
		return false
	}

	switch header.Type {
	case protocol.ControlWindowUpdate:
		var update protocol.WindowUpdate
		if err := json.Unmarshal(frame.Payload, &update); err != nil {
			s.resetMalformedControl(frame.Payload, err)
			return true
		}
		st := s.findStream(update.StreamID)
		if st == nil {
			return true
		}
		if update.StreamID == 0 || update.MaxBytes == 0 {
			s.failStream(st, "invalid_control", "invalid stream window update", true)
			return true
		}
		st.updateSendLimit(update.MaxBytes)
		return true

	case protocol.ControlStreamReset:
		var reset protocol.StreamReset
		if err := json.Unmarshal(frame.Payload, &reset); err != nil {
			s.resetMalformedControl(frame.Payload, err)
			return true
		}
		st := s.findStream(reset.StreamID)
		if st == nil {
			return true
		}
		message := reset.Message
		if message == "" {
			message = reset.Code
		}
		if message == "" {
			message = "stream reset by peer"
		}
		s.failStream(st, reset.Code, message, false)
		return true
	}

	return false
}

func (s *Session) resetMalformedControl(payload []byte, controlErr error) {
	var target struct {
		StreamID uint32 `json:"stream_id"`
	}
	if json.Unmarshal(payload, &target) != nil || target.StreamID == 0 {
		return
	}
	if st := s.findStream(target.StreamID); st != nil {
		s.failStream(st, "invalid_control", controlErr.Error(), true)
	}
}

func (s *Session) sendWindowUpdate(streamID uint32, maxBytes uint64) error {
	payload, err := json.Marshal(protocol.WindowUpdate{
		Type:     protocol.ControlWindowUpdate,
		StreamID: streamID,
		MaxBytes: maxBytes,
	})
	if err != nil {
		return err
	}
	return s.sendControlSYN(payload)
}

func (s *Session) sendStreamReset(streamID uint32, code, message string) error {
	payload, err := json.Marshal(protocol.StreamReset{
		Type:     protocol.ControlStreamReset,
		StreamID: streamID,
		Code:     code,
		Message:  message,
	})
	if err != nil {
		return err
	}
	return s.sendControlSYN(payload)
}

func (s *Session) sendFrame(streamID uint32, flags uint16, payload []byte) error {
	if err := protocol.ValidateFramePayload(payload); err != nil {
		return err
	}
	frame := protocol.Frame{StreamID: streamID, Flags: flags, Payload: payload}
	var st *stream
	if streamID != 0 {
		st = s.findStream(streamID)
		if st == nil {
			return errStreamClosed
		}
	}
	if st != nil {
		st.writeMu.Lock()
		defer st.writeMu.Unlock()
		if st.outboundFIN.Load() {
			return errStreamFinished
		}
	}
	flowControl := s.flowControlEnabled() && streamID != 0
	if flowControl {
		if !frame.IsSYN() && !st.initialWindowActive() {
			return errStreamNotStarted
		}
		if frame.IsSYN() {
			// Activate receive credit before the ordered SYN goes on the wire,
			// so an immediate response cannot race the receive-window check.
			// writeMu keeps later local data behind the SYN itself.
			st.activateInitialWindow()
		}
		if err := st.reserveSend(frame.FlowBytes(), s.done); err != nil {
			return err
		}
	}

	if err := s.sendRawFrame(streamID, flags, payload); err != nil {
		if flowControl && frame.IsSYN() {
			s.failStream(st, "send_error", err.Error(), false)
		}
		return err
	}
	if st != nil && frame.IsFIN() {
		st.outboundFIN.Store(true)
		s.detachFinishedStream(st)
	}
	return nil
}

func (s *Session) sendRawFrame(streamID uint32, flags uint16, payload []byte) error {
	if err := protocol.ValidateFramePayload(payload); err != nil {
		return err
	}
	if s.closed.Load() {
		return errors.New("data channel closed")
	}
	if s.sendFrameOverride != nil {
		return s.sendFrameOverride(streamID, flags, payload)
	}
	if s.DC == nil || s.DC.ReadyState() != webrtc.DataChannelStateOpen {
		return errors.New("data channel closed")
	}
	return s.DC.Send(protocol.BuildFrame(streamID, flags, payload))
}

func (s *Session) sendControlSYN(payload []byte) error {
	return s.sendRawFrame(0, protocol.FlagSYN, payload)
}

// Close closes the data channel and returns. Anything left in flight
// (DTLS close_notify, TURN dealloc, ICE relay socket teardown) is left
// to the OS to reap when the process exits — pion's synchronous
// PC.Close() path is slow on relay sessions and isn't worth waiting for.
func (s *Session) Close() {
	s.finishStreams()
	if s.DC != nil {
		_ = s.DC.Close()
	}
}

// Done closes when the data channel disconnects or Close is called.
func (s *Session) Done() <-chan struct{} { return s.done }

// Stream is the caller-facing handle for an outbound SWSP stream.
type Stream struct {
	s  *Session
	st *stream
}

// ID returns the stream's wire-level ID.
func (s *Stream) ID() uint32 { return s.st.id }

// WriteSYN sends a SYN frame.
func (s *Stream) WriteSYN(payload []byte) error {
	return s.s.sendFrame(s.st.id, protocol.FlagSYN, payload)
}

// WriteDAT sends a DAT frame. Caller is responsible for chunking to
// MaxChunkSize (file ops do this internally).
func (s *Stream) WriteDAT(payload []byte) error {
	return s.s.sendFrame(s.st.id, protocol.FlagDAT, payload)
}

// WriteFIN sends the closing frame for an outbound stream.
func (s *Stream) WriteFIN(payload []byte) error {
	return s.s.sendFrame(s.st.id, protocol.FlagFIN, payload)
}

// Inbox is the channel of inbound frames for this stream. Closes when
// the device sends FIN or the data channel is torn down.
func (s *Stream) Inbox() <-chan protocol.Frame { return s.st.inbox }

// BufferedAmount exposes the underlying DC's send buffer, for backpressure.
func (s *Stream) BufferedAmount() uint64 { return s.s.DC.BufferedAmount() }

// Close drops the stream from the session's routing table. The caller
// is expected to have sent FIN already; this is just cleanup if the
// stream is being abandoned (e.g. on error).
func (s *Stream) Close() {
	// A v4 reset is full-duplex, so it also cancels a response that is still
	// arriving after our FIN (for example, a download whose local writer
	// failed). Legacy peers only understand directional FIN; avoid sending it
	// twice when our outbound direction already ended.
	if s.s.flowControlEnabled() || !s.st.outboundFIN.Load() {
		s.s.failStream(s.st, "cancelled", "stream closed", true)
		return
	}
	if s.s.detachStream(s.st.id, s.st) {
		s.st.fail(errStreamClosed)
	}
}
