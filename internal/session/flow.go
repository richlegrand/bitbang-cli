package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/richlegrand/bitbang/internal/protocol"
	"github.com/richlegrand/bitbang/internal/streamtype"
)

var (
	errStreamClosed   = errors.New("stream closed")
	errStreamFinished = errors.New("stream direction already finished")
)

type streamReceiveError struct {
	code    string
	message string
}

func (e *streamReceiveError) Error() string { return e.message }

func receiveErrorCode(err error) string {
	var receiveErr *streamReceiveError
	if errors.As(err, &receiveErr) {
		return receiveErr.code
	}
	return "receive_overflow"
}

// streamState owns one stream's ordering, receive bounds, and directional
// flow-control counters. Its queue is never written with a blocking send.
type streamState struct {
	id      uint32
	handler streamtype.StreamHandler
	flow    bool
	queue   chan protocol.Frame
	done    chan struct{}
	stop    sync.Once

	mu               sync.Mutex
	closed           bool
	queuedBytes      int
	queuedFrames     int
	received         uint64
	consumed         uint64
	advertised       uint64
	lastUpdate       uint64
	sent             uint64
	sendMax          uint64
	sendWake         chan struct{}
	receiveFIN       bool
	receiveQueuedFIN bool
	sendFIN          bool

	// fragmenting is accessed only by the stream worker.
	fragmenting bool
	writeMu     sync.Mutex
}

func newStreamState(id uint32, handler streamtype.StreamHandler, flow bool) *streamState {
	st := &streamState{
		id:       id,
		handler:  handler,
		flow:     flow,
		queue:    make(chan protocol.Frame, protocol.MaxQueuedStreamFrames),
		done:     make(chan struct{}),
		sendWake: make(chan struct{}),
	}
	if flow {
		// The v4 SYN opens both directions with the protocol's implicit
		// initial window; no window_update round trip is required.
		st.advertised = protocol.InitialStreamWindow
		st.sendMax = protocol.InitialStreamWindow
	}
	return st
}

// enqueue accepts a frame only if both the negotiated receive window and the
// local queue bounds allow it. It never waits for the stream worker.
func (st *streamState) enqueue(frame protocol.Frame) error {
	bytes := len(frame.Payload)
	flowBytes := frame.FlowBytes()

	st.mu.Lock()
	defer st.mu.Unlock()
	if st.closed {
		return errStreamClosed
	}
	if st.receiveQueuedFIN {
		return &streamReceiveError{
			code:    "protocol_error",
			message: fmt.Sprintf("stream %d received data after FIN", st.id),
		}
	}
	if st.flow && flowBytes > 0 {
		if st.received > st.advertised || flowBytes > st.advertised-st.received {
			return &streamReceiveError{
				code:    "flow_control_violation",
				message: fmt.Sprintf("stream %d exceeded receive window", st.id),
			}
		}
	}
	if st.queuedFrames >= protocol.MaxQueuedStreamFrames ||
		bytes > protocol.InitialStreamWindow-st.queuedBytes {
		return &streamReceiveError{
			code:    "receive_overflow",
			message: fmt.Sprintf("stream %d receive queue is full", st.id),
		}
	}

	st.received += flowBytes
	if frame.IsFIN() {
		st.receiveQueuedFIN = true
	}
	st.queuedFrames++
	st.queuedBytes += bytes
	select {
	case st.queue <- frame:
		return nil
	default:
		// queuedFrames and the channel capacity use the same limit, so this
		// is only reachable if an invariant changes. Roll back conservatively.
		st.received -= flowBytes
		if frame.IsFIN() {
			st.receiveQueuedFIN = false
		}
		st.queuedFrames--
		st.queuedBytes -= bytes
		return &streamReceiveError{code: "receive_overflow", message: "stream receive queue is full"}
	}
}

func (st *streamState) dequeued(frame protocol.Frame) {
	st.mu.Lock()
	st.queuedFrames--
	st.queuedBytes -= len(frame.Payload)
	st.mu.Unlock()
}

func (st *streamState) isClosed() bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.closed
}

func (st *streamState) stopNow() bool {
	st.mu.Lock()
	if st.closed {
		st.mu.Unlock()
		return false
	}
	st.closed = true
	st.mu.Unlock()
	st.stop.Do(func() { close(st.done) })
	return true
}

func (st *streamState) updateSendMax(max uint64) {
	st.mu.Lock()
	if !st.closed && max > st.sendMax {
		st.sendMax = max
		close(st.sendWake)
		st.sendWake = make(chan struct{})
	}
	st.mu.Unlock()
}

func (st *streamState) reserveSend(bytes uint64, sessionDone <-chan struct{}) error {
	if !st.flow || bytes == 0 {
		st.mu.Lock()
		closed := st.closed
		st.mu.Unlock()
		if closed {
			return errStreamClosed
		}
		return nil
	}

	for {
		st.mu.Lock()
		if st.closed {
			st.mu.Unlock()
			return errStreamClosed
		}
		if st.sent <= st.sendMax && bytes <= st.sendMax-st.sent {
			st.sent += bytes
			st.mu.Unlock()
			return nil
		}
		wake := st.sendWake
		st.mu.Unlock()

		select {
		case <-wake:
		case <-st.done:
			return errStreamClosed
		case <-sessionDone:
			return errors.New("session closed")
		}
	}
}

func (s *Session) sendWindowUpdate(st *streamState, max uint64) error {
	st.mu.Lock()
	if st.closed {
		st.mu.Unlock()
		return errStreamClosed
	}
	if max > st.advertised {
		st.advertised = max
	}
	st.mu.Unlock()

	update := protocol.WindowUpdate{
		Type:     protocol.ControlWindowUpdate,
		StreamID: st.id,
		MaxBytes: max,
	}
	payload, _ := json.Marshal(update)
	return s.sendFrame(0, protocol.FlagSYN, payload)
}

func (s *Session) consume(st *streamState, bytes uint64) error {
	if !st.flow || bytes == 0 {
		return nil
	}

	var update uint64
	st.mu.Lock()
	if !st.closed {
		st.consumed += bytes
		if st.consumed-st.lastUpdate >= protocol.StreamWindowUpdateThreshold {
			st.lastUpdate = st.consumed
			st.advertised = st.consumed + protocol.InitialStreamWindow
			update = st.advertised
		}
	}
	st.mu.Unlock()
	if update != 0 {
		return s.sendWindowUpdate(st, update)
	}
	return nil
}

func (s *Session) markReceiveClosed(st *streamState) {
	st.mu.Lock()
	st.receiveFIN = true
	complete := st.sendFIN
	st.mu.Unlock()
	if complete {
		s.finishState(st)
	}
}

func (s *Session) markSendClosed(st *streamState) {
	st.mu.Lock()
	st.sendFIN = true
	complete := st.receiveFIN
	st.mu.Unlock()
	if complete {
		s.finishState(st)
	}
}

func (s *Session) finishState(st *streamState) {
	s.mu.Lock()
	if s.streams[st.id] == st {
		delete(s.streams, st.id)
	}
	s.mu.Unlock()
	st.stopNow()
}

func (s *Session) failStream(st *streamState, code, message string, notifyPeer bool) {
	s.mu.Lock()
	if s.streams[st.id] != st {
		s.mu.Unlock()
		return
	}
	delete(s.streams, st.id)
	s.mu.Unlock()
	if !st.stopNow() {
		return
	}

	stream := newStreamCtx(s, st.id)
	go func() {
		// Tear down the local sink first so a blocked handler is released even
		// if the best-effort reset notification cannot be sent promptly.
		if resetHandler, ok := st.handler.(streamtype.ResetHandler); ok {
			resetHandler.OnReset(stream, code, message)
		}
		// A writer may already have reserved credit when the stream fails.
		// Wait for that in-flight frame to finish before putting the terminal
		// reset on the ordered data channel. Writers still waiting for credit
		// were woken by stopNow and will release this lock without sending.
		st.writeMu.Lock()
		defer st.writeMu.Unlock()
		if notifyPeer {
			if st.flow {
				s.sendReset(st.id, code, message)
			} else {
				// Legacy peers have no full-stream reset. An empty FIN is the
				// closest best-effort signal and remains stream-local.
				_ = s.sendFrame(st.id, protocol.FlagFIN, nil)
			}
		}
	}()
}

func (s *Session) rejectStream(streamID uint32, code, message string) {
	s.mu.Lock()
	flow := s.negotiatedVersion >= 4
	s.mu.Unlock()
	go func() {
		if flow {
			s.sendReset(streamID, code, message)
			return
		}
		s.sendStreamError(streamID, message)
	}()
}

func (s *Session) sendReset(streamID uint32, code, message string) {
	reset := protocol.StreamReset{
		Type:     protocol.ControlStreamReset,
		StreamID: streamID,
		Code:     code,
		Message:  message,
	}
	payload, _ := json.Marshal(reset)
	_ = s.sendFrame(0, protocol.FlagSYN, payload)
}

func (s *Session) applyWindowUpdate(update protocol.WindowUpdate) {
	if update.StreamID == 0 {
		return
	}
	s.mu.Lock()
	flow := s.negotiatedVersion >= 4
	st := s.streams[update.StreamID]
	s.mu.Unlock()
	if !flow || st == nil || !st.flow {
		return
	}
	if update.MaxBytes == 0 {
		s.failStream(st, "invalid_control", "window update must grant a positive maximum", true)
		return
	}
	st.updateSendMax(update.MaxBytes)
}

func (s *Session) applyStreamReset(reset protocol.StreamReset) {
	if reset.StreamID == 0 {
		return
	}
	s.mu.Lock()
	flow := s.negotiatedVersion >= 4
	st := s.streams[reset.StreamID]
	s.mu.Unlock()
	if flow && st != nil && st.flow {
		s.failStream(st, reset.Code, reset.Message, false)
	}
}

func (s *Session) resetMalformedControl(payload []byte, controlErr error) {
	var target struct {
		StreamID uint32 `json:"stream_id"`
	}
	if json.Unmarshal(payload, &target) != nil || target.StreamID == 0 {
		return
	}
	s.mu.Lock()
	flow := s.negotiatedVersion >= 4
	st := s.streams[target.StreamID]
	s.mu.Unlock()
	if flow && st != nil && st.flow {
		s.failStream(st, "invalid_control", controlErr.Error(), true)
	}
}

func (s *Session) sendStreamFrame(streamID uint32, flags uint16, payload []byte) error {
	if err := protocol.ValidateFramePayload(payload); err != nil {
		return err
	}
	s.mu.Lock()
	st := s.streams[streamID]
	closed := s.closed
	s.mu.Unlock()
	if closed || st == nil {
		return errStreamClosed
	}

	st.writeMu.Lock()
	defer st.writeMu.Unlock()
	st.mu.Lock()
	sendFinished := st.sendFIN
	st.mu.Unlock()
	if sendFinished {
		return errStreamFinished
	}
	frame := protocol.Frame{StreamID: streamID, Flags: flags, Payload: payload}
	if err := st.reserveSend(frame.FlowBytes(), s.done); err != nil {
		return err
	}
	if err := s.sendFrame(streamID, flags, payload); err != nil {
		s.failStream(st, "send_error", err.Error(), false)
		return err
	}
	if frame.IsFIN() {
		s.markSendClosed(st)
	}
	return nil
}

// Close stops all per-stream workers and releases writers waiting for credit.
func (s *Session) Close() {
	s.close.Do(func() {
		s.mu.Lock()
		s.closed = true
		close(s.done)
		states := make([]*streamState, 0, len(s.streams))
		for id, st := range s.streams {
			states = append(states, st)
			delete(s.streams, id)
		}
		s.mu.Unlock()

		for _, st := range states {
			if !st.stopNow() {
				continue
			}
			if resetHandler, ok := st.handler.(streamtype.ResetHandler); ok {
				go resetHandler.OnReset(newStreamCtx(s, st.id), "session_closed", "session closed")
			}
		}
	})
}
