package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/richlegrand/bitbang/internal/auth"
	"github.com/richlegrand/bitbang/internal/protocol"
	"github.com/richlegrand/bitbang/internal/streamtype"
)

type frameCapture struct {
	mu     sync.Mutex
	frames []protocol.Frame
}

func (c *frameCapture) send(streamID uint32, flags uint16, payload []byte) error {
	c.mu.Lock()
	c.frames = append(c.frames, protocol.Frame{
		StreamID: streamID,
		Flags:    flags,
		Payload:  append([]byte(nil), payload...),
	})
	c.mu.Unlock()
	return nil
}

func (c *frameCapture) countData(streamID uint32) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	count := 0
	for _, frame := range c.frames {
		if frame.StreamID == streamID && !frame.IsSYN() && len(frame.Payload) > 0 {
			count++
		}
	}
	return count
}

func (c *frameCapture) controls(controlType string) []map[string]interface{} {
	c.mu.Lock()
	defer c.mu.Unlock()
	var controls []map[string]interface{}
	for _, frame := range c.frames {
		if frame.StreamID != 0 || !frame.IsSYN() {
			continue
		}
		var control map[string]interface{}
		if json.Unmarshal(frame.Payload, &control) == nil && control["type"] == controlType {
			controls = append(controls, control)
		}
	}
	return controls
}

type testHandler struct {
	onSYN   func(streamtype.Stream, []byte, bool) error
	onDAT   func(streamtype.Stream, []byte) error
	onFIN   func(streamtype.Stream, []byte) error
	onReset func(streamtype.Stream, string, string)
}

func (h *testHandler) Type() string           { return "test" }
func (h *testHandler) OnConnect(string) error { return nil }
func (h *testHandler) OnSYN(s streamtype.Stream, p []byte, final bool) error {
	if h.onSYN != nil {
		return h.onSYN(s, p, final)
	}
	return nil
}
func (h *testHandler) OnDAT(s streamtype.Stream, p []byte) error {
	if h.onDAT != nil {
		return h.onDAT(s, p)
	}
	return nil
}
func (h *testHandler) OnFIN(s streamtype.Stream, p []byte) error {
	if h.onFIN != nil {
		return h.onFIN(s, p)
	}
	return nil
}
func (h *testHandler) OnReset(s streamtype.Stream, code, message string) {
	if h.onReset != nil {
		h.onReset(s, code, message)
	}
}

func newFlowTestSession(t *testing.T, version int, handler streamtype.StreamHandler) (*Session, *frameCapture) {
	t.Helper()
	capture := &frameCapture{}
	sess := New(nil, auth.New(""), false, handler)
	sess.sendFrame = capture.send
	t.Cleanup(sess.Close)
	connect, _ := json.Marshal(map[string]interface{}{
		"type":    "connect",
		"path":    "/",
		"version": version,
	})
	sess.HandleMessage(protocol.BuildFrame(0, protocol.FlagSYN, connect))
	return sess, capture
}

func openTestStream(sess *Session, id uint32, final bool) {
	flags := uint16(protocol.FlagSYN)
	if final {
		flags |= protocol.FlagFIN
	}
	sess.HandleMessage(protocol.BuildFrame(id, flags, []byte(`{"type":"test"}`)))
}

func sendControl(sess *Session, control interface{}) {
	payload, _ := json.Marshal(control)
	sess.HandleMessage(protocol.BuildFrame(0, protocol.FlagSYN, payload))
}

func TestNegotiatedVersionLockedByFirstConnect(t *testing.T) {
	tests := []struct {
		name    string
		version int
		want    int
	}{
		{name: "missing defaults to v2", want: 2},
		{name: "v3", version: 3, want: 3},
		{name: "v4", version: 4, want: 4},
		{name: "future version", version: 99, want: protocol.SWSPVersion},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sess, capture := newFlowTestSession(t, tt.version, &testHandler{})
			sess.mu.Lock()
			got := sess.negotiatedVersion
			sess.mu.Unlock()
			if got != tt.want {
				t.Fatalf("negotiated version = %d, want %d", got, tt.want)
			}
			ready := capture.controls("ready")
			if len(ready) != 1 || int(ready[0]["negotiated_version"].(float64)) != tt.want {
				t.Fatalf("ready controls = %#v, want negotiated_version %d", ready, tt.want)
			}

			// A navigation connect may update routing, but not wire semantics.
			sess.handleConnect("/new-path", 2)
			sess.mu.Lock()
			got = sess.negotiatedVersion
			sess.mu.Unlock()
			if got != tt.want {
				t.Fatalf("renegotiated version = %d, want locked %d", got, tt.want)
			}
		})
	}
}

func TestSlowStreamDoesNotBlockAnotherStream(t *testing.T) {
	slowStarted := make(chan struct{})
	fastDelivered := make(chan struct{})
	releaseSlow := make(chan struct{})
	var startOnce, fastOnce, releaseOnce sync.Once
	handler := &testHandler{
		onDAT: func(s streamtype.Stream, _ []byte) error {
			switch s.ID() {
			case 1:
				startOnce.Do(func() { close(slowStarted) })
				<-releaseSlow
			case 3:
				fastOnce.Do(func() { close(fastDelivered) })
			}
			return nil
		},
		onReset: func(s streamtype.Stream, _, _ string) {
			if s.ID() == 1 {
				releaseOnce.Do(func() { close(releaseSlow) })
			}
		},
	}
	sess, _ := newFlowTestSession(t, 3, handler)
	openTestStream(sess, 1, false)
	openTestStream(sess, 3, false)

	sess.HandleMessage(protocol.BuildFrame(1, protocol.FlagDAT, []byte("block")))
	select {
	case <-slowStarted:
	case <-time.After(time.Second):
		t.Fatal("slow stream handler did not start")
	}
	sess.HandleMessage(protocol.BuildFrame(3, protocol.FlagDAT, []byte("fast")))
	select {
	case <-fastDelivered:
	case <-time.After(time.Second):
		t.Fatal("healthy stream was blocked by slow stream")
	}

	// Saturating the slow stream's frame bound resets only that stream and
	// must not make HandleMessage wait for its blocked handler.
	started := time.Now()
	for i := 0; i <= protocol.MaxQueuedStreamFrames; i++ {
		sess.HandleMessage(protocol.BuildFrame(1, protocol.FlagDAT, nil))
	}
	if elapsed := time.Since(started); elapsed > 250*time.Millisecond {
		t.Fatalf("queue saturation blocked HandleMessage for %v", elapsed)
	}
	waitFor(t, func() bool {
		sess.mu.Lock()
		defer sess.mu.Unlock()
		return sess.streams[1] == nil && sess.streams[3] != nil
	})
}

func TestV4SenderUsesImplicitInitialCreditAndWaitsForUpdate(t *testing.T) {
	streamReady := make(chan streamtype.Stream, 1)
	handler := &testHandler{onSYN: func(s streamtype.Stream, _ []byte, _ bool) error {
		streamReady <- s
		return nil
	}}
	sess, capture := newFlowTestSession(t, 4, handler)
	openTestStream(sess, 1, false)
	stream := <-streamReady
	if updates := capture.controls(protocol.ControlWindowUpdate); len(updates) != 0 {
		t.Fatalf("v4 SYN sent redundant initial window update: %#v", updates)
	}

	payload := make([]byte, protocol.MaxChunkSize)
	frames := protocol.InitialStreamWindow / protocol.MaxChunkSize
	fillDone := make(chan error, 1)
	go func() {
		for i := 0; i < frames; i++ {
			if err := stream.WriteDAT(payload); err != nil {
				fillDone <- fmt.Errorf("write implicit window frame %d: %w", i, err)
				return
			}
		}
		fillDone <- nil
	}()
	select {
	case err := <-fillDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("implicit initial window did not become sendable")
	}

	writeDone := make(chan error, 1)
	go func() { writeDone <- stream.WriteDAT([]byte("x")) }()
	select {
	case err := <-writeDone:
		t.Fatalf("write beyond implicit window completed early: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	if got := capture.countData(1); got != frames {
		t.Fatalf("sent %d data frames before update, want %d", got, frames)
	}

	sendControl(sess, protocol.WindowUpdate{
		Type:     protocol.ControlWindowUpdate,
		StreamID: 1,
		MaxBytes: protocol.InitialStreamWindow + 1,
	})
	select {
	case err := <-writeDone:
		if err != nil {
			t.Fatalf("credited write: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("write did not resume after credit")
	}
	if got := capture.countData(1); got != frames+1 {
		t.Fatalf("sent %d data frames after update, want %d", got, frames+1)
	}
}

func TestStreamResetFollowsAlreadyReservedData(t *testing.T) {
	streamReady := make(chan streamtype.Stream, 1)
	handler := &testHandler{onSYN: func(s streamtype.Stream, _ []byte, _ bool) error {
		streamReady <- s
		return nil
	}}
	sess, _ := newFlowTestSession(t, 4, handler)
	openTestStream(sess, 1, false)
	stream := <-streamReady

	sess.mu.Lock()
	st := sess.streams[1]
	sess.mu.Unlock()
	dataStarted := make(chan struct{})
	releaseData := make(chan struct{})
	events := make(chan string, 2)
	sess.sendFrame = func(streamID uint32, _ uint16, _ []byte) error {
		if streamID == 1 {
			close(dataStarted)
			<-releaseData
			events <- "data"
			return nil
		}
		events <- "reset"
		return nil
	}

	writeDone := make(chan error, 1)
	go func() { writeDone <- stream.WriteDAT([]byte("x")) }()
	select {
	case <-dataStarted:
	case <-time.After(time.Second):
		t.Fatal("data send did not start")
	}
	sess.failStream(st, "test_reset", "reset", true)
	select {
	case event := <-events:
		t.Fatalf("terminal event %q overtook blocked data", event)
	case <-time.After(20 * time.Millisecond):
	}

	close(releaseData)
	select {
	case err := <-writeDone:
		if err != nil {
			t.Fatalf("in-flight data send: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for data send")
	}
	for _, want := range []string{"data", "reset"} {
		select {
		case got := <-events:
			if got != want {
				t.Fatalf("send order = %q before %q", got, want)
			}
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for %s send", want)
		}
	}
}

func TestV3DataPathDoesNotWaitForOrSendCredit(t *testing.T) {
	received := make(chan string, 1)
	handler := &testHandler{
		onSYN: func(s streamtype.Stream, _ []byte, _ bool) error {
			return s.WriteDAT([]byte("response"))
		},
		onDAT: func(_ streamtype.Stream, payload []byte) error {
			received <- string(payload)
			return nil
		},
	}
	sess, capture := newFlowTestSession(t, 3, handler)
	openTestStream(sess, 1, false)
	waitFor(t, func() bool { return capture.countData(1) == 1 })
	if controls := capture.controls(protocol.ControlWindowUpdate); len(controls) != 0 {
		t.Fatalf("v3 session sent window controls: %#v", controls)
	}

	sess.HandleMessage(protocol.BuildFrame(1, protocol.FlagDAT, []byte("request")))
	select {
	case got := <-received:
		if got != "request" {
			t.Fatalf("v3 inbound payload = %q, want request", got)
		}
	case <-time.After(time.Second):
		t.Fatal("v3 inbound data waited for flow-control credit")
	}
}

func TestV4OversizedWriteFailsBeforeWaitingForCredit(t *testing.T) {
	streamReady := make(chan streamtype.Stream, 1)
	handler := &testHandler{onSYN: func(s streamtype.Stream, _ []byte, _ bool) error {
		streamReady <- s
		return nil
	}}
	sess, _ := newFlowTestSession(t, 4, handler)
	openTestStream(sess, 1, false)
	stream := <-streamReady
	if err := stream.WriteDAT(make([]byte, protocol.MaxChunkSize+1)); err == nil {
		t.Fatal("oversized v4 write did not fail")
	}
}

func TestV4ReceiveWindowViolationResetsOnlyOffendingStream(t *testing.T) {
	slowStarted := make(chan struct{})
	releaseSlow := make(chan struct{})
	var startOnce, releaseOnce sync.Once
	handler := &testHandler{
		onDAT: func(s streamtype.Stream, _ []byte) error {
			if s.ID() == 1 {
				startOnce.Do(func() { close(slowStarted) })
				<-releaseSlow
			}
			return nil
		},
		onReset: func(s streamtype.Stream, _, _ string) {
			if s.ID() == 1 {
				releaseOnce.Do(func() { close(releaseSlow) })
			}
		},
	}
	sess, capture := newFlowTestSession(t, 4, handler)
	openTestStream(sess, 1, false)
	openTestStream(sess, 3, false)
	if updates := capture.controls(protocol.ControlWindowUpdate); len(updates) != 0 {
		t.Fatalf("v4 SYNs sent redundant initial window updates: %#v", updates)
	}

	payload := make([]byte, protocol.MaxChunkSize)
	sess.HandleMessage(protocol.BuildFrame(1, protocol.FlagDAT, payload))
	select {
	case <-slowStarted:
	case <-time.After(time.Second):
		t.Fatal("slow handler did not start")
	}
	for i := 1; i < protocol.InitialStreamWindow/protocol.MaxChunkSize; i++ {
		sess.HandleMessage(protocol.BuildFrame(1, protocol.FlagDAT, payload))
	}
	// The next byte exceeds the exact cumulative grant.
	sess.HandleMessage(protocol.BuildFrame(1, protocol.FlagDAT, []byte{1}))

	waitFor(t, func() bool { return len(capture.controls(protocol.ControlStreamReset)) == 1 })
	sess.mu.Lock()
	offending, healthy := sess.streams[1], sess.streams[3]
	sess.mu.Unlock()
	if offending != nil || healthy == nil {
		t.Fatalf("stream states after violation: offending=%v healthy=%v", offending != nil, healthy != nil)
	}
}

func TestV4ReceiverQueuesImplicitWindowDataBehindSYN(t *testing.T) {
	synStarted := make(chan struct{})
	releaseSYN := make(chan struct{})
	dataDelivered := make(chan struct{})
	var startOnce, releaseOnce sync.Once
	handler := &testHandler{
		onSYN: func(streamtype.Stream, []byte, bool) error {
			startOnce.Do(func() { close(synStarted) })
			<-releaseSYN
			return nil
		},
		onReset: func(streamtype.Stream, string, string) {
			releaseOnce.Do(func() { close(releaseSYN) })
		},
		onDAT: func(streamtype.Stream, []byte) error {
			close(dataDelivered)
			return nil
		},
	}
	sess, capture := newFlowTestSession(t, 4, handler)
	openTestStream(sess, 1, false)
	select {
	case <-synStarted:
	case <-time.After(time.Second):
		t.Fatal("SYN handler did not start")
	}

	// The initial window is implicit, so ordered DAT can queue while OnSYN is
	// still running. The worker must not deliver it ahead of SYN.
	sess.HandleMessage(protocol.BuildFrame(1, protocol.FlagDAT, []byte{1}))
	select {
	case <-dataDelivered:
		t.Fatal("DAT overtook the blocked SYN handler")
	case <-time.After(30 * time.Millisecond):
	}
	if resets := capture.controls(protocol.ControlStreamReset); len(resets) != 0 {
		t.Fatalf("implicit-window DAT reset the stream: %#v", resets)
	}
	if updates := capture.controls(protocol.ControlWindowUpdate); len(updates) != 0 {
		t.Fatalf("v4 SYN sent redundant initial window update: %#v", updates)
	}
	releaseOnce.Do(func() { close(releaseSYN) })
	select {
	case <-dataDelivered:
	case <-time.After(time.Second):
		t.Fatal("queued DAT was not delivered after SYN completed")
	}
}

func TestConsumedBytesReplenishWindow(t *testing.T) {
	var consumed atomic.Int64
	handler := &testHandler{onDAT: func(streamtype.Stream, []byte) error {
		consumed.Add(protocol.MaxChunkSize)
		return nil
	}}
	sess, capture := newFlowTestSession(t, 4, handler)
	openTestStream(sess, 1, false)
	if updates := capture.controls(protocol.ControlWindowUpdate); len(updates) != 0 {
		t.Fatalf("v4 SYN sent redundant initial window update: %#v", updates)
	}

	payload := make([]byte, protocol.MaxChunkSize)
	for i := 0; i < protocol.StreamWindowUpdateThreshold/protocol.MaxChunkSize; i++ {
		sess.HandleMessage(protocol.BuildFrame(1, protocol.FlagDAT, payload))
	}
	waitFor(t, func() bool { return len(capture.controls(protocol.ControlWindowUpdate)) == 1 })
	updates := capture.controls(protocol.ControlWindowUpdate)
	want := float64(protocol.InitialStreamWindow + protocol.StreamWindowUpdateThreshold)
	if got := updates[0]["max_bytes"]; got != want {
		t.Fatalf("replenished max_bytes = %v, want %.0f", got, want)
	}
	if got := consumed.Load(); got != protocol.StreamWindowUpdateThreshold {
		t.Fatalf("consumed bytes = %d, want %d", got, protocol.StreamWindowUpdateThreshold)
	}
}

func TestResetAndSessionCloseWakeBlockedWriters(t *testing.T) {
	for _, tt := range []struct {
		name  string
		close func(*Session)
	}{
		{
			name: "stream reset",
			close: func(sess *Session) {
				sendControl(sess, protocol.StreamReset{
					Type:     protocol.ControlStreamReset,
					StreamID: 1,
					Code:     "cancelled",
				})
			},
		},
		{name: "session close", close: func(sess *Session) { sess.Close() }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			streamReady := make(chan streamtype.Stream, 1)
			handler := &testHandler{onSYN: func(s streamtype.Stream, _ []byte, _ bool) error {
				streamReady <- s
				return nil
			}}
			sess, _ := newFlowTestSession(t, 4, handler)
			openTestStream(sess, 1, false)
			stream := <-streamReady
			sess.mu.Lock()
			st := sess.streams[1]
			sess.mu.Unlock()
			if err := st.reserveSend(protocol.InitialStreamWindow, sess.done); err != nil {
				t.Fatalf("reserve implicit initial window: %v", err)
			}
			writeDone := make(chan error, 1)
			go func() { writeDone <- stream.WriteDAT([]byte("blocked")) }()
			select {
			case <-writeDone:
				t.Fatal("write completed before reset")
			case <-time.After(30 * time.Millisecond):
			}

			tt.close(sess)
			select {
			case err := <-writeDone:
				if !errors.Is(err, errStreamClosed) && err.Error() != "session closed" {
					t.Fatalf("blocked write error = %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("blocked write did not wake")
			}
		})
	}
}
