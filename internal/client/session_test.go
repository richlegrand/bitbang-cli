package client

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/richlegrand/bitbang/internal/protocol"
)

func TestSessionNegotiatesFlowControlVersion(t *testing.T) {
	tests := []struct {
		name       string
		server     int
		negotiated int
		wantServer int
		want       int
		wantErr    bool
	}{
		{name: "legacy missing version", wantServer: 2, want: 2},
		{name: "v3 fallback", server: 3, wantServer: 3, want: 3},
		{name: "v4 explicit", server: 4, negotiated: 4, wantServer: 4, want: 4},
		{name: "future server clamps locally", server: 99, negotiated: 4, wantServer: 99, want: 4},
		{name: "lower negotiated version", server: 4, negotiated: 3, wantServer: 4, want: 3},
		{name: "negotiated above server", server: 3, negotiated: 4, wantErr: true},
		{name: "unsupported server", server: 1, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sess := &Session{}
			err := sess.setNegotiatedVersion(tt.server, tt.negotiated)
			if tt.wantErr {
				if err == nil {
					t.Fatal("setNegotiatedVersion returned nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("setNegotiatedVersion: %v", err)
			}
			if sess.ServerVersion != tt.wantServer || sess.NegotiatedVersion != tt.want {
				t.Fatalf("versions = server %d negotiated %d, want server %d negotiated %d",
					sess.ServerVersion, sess.NegotiatedVersion, tt.wantServer, tt.want)
			}
		})
	}
}

func TestSessionV3DataPathDoesNotUseCredit(t *testing.T) {
	p, sess := newDispatchTestSession(t, 2)
	sess.NegotiatedVersion = 3
	sent := make(chan protocol.Frame, 3)
	sess.sendFrameOverride = func(streamID uint32, flags uint16, payload []byte) error {
		sent <- protocol.Frame{StreamID: streamID, Flags: flags, Payload: append([]byte(nil), payload...)}
		return nil
	}
	stream := sess.OpenStream()
	sess.startDispatcher(p)
	if err := stream.WriteSYN([]byte(`{"type":"test"}`)); err != nil {
		t.Fatalf("v3 WriteSYN: %v", err)
	}
	if err := stream.WriteDAT([]byte("request")); err != nil {
		t.Fatalf("v3 WriteDAT: %v", err)
	}
	for i := 0; i < 2; i++ {
		frame := <-sent
		if frame.StreamID != stream.ID() {
			t.Fatalf("v3 sent control frame: %#v", frame)
		}
	}

	p.dcMsg <- protocol.BuildFrame(stream.ID(), protocol.FlagDAT, []byte("response"))
	select {
	case frame := <-stream.Inbox():
		if got := string(frame.Payload); got != "response" {
			t.Fatalf("v3 inbound payload = %q", got)
		}
	case <-time.After(time.Second):
		t.Fatal("v3 inbound data waited for flow-control credit")
	}
}

func TestSessionV4DataUsesImplicitInitialWindow(t *testing.T) {
	p, sess := newDispatchTestSession(t, 1)
	sess.NegotiatedVersion = 4
	sent := make(chan protocol.Frame, 3)
	sess.sendFrameOverride = func(streamID uint32, flags uint16, payload []byte) error {
		sent <- protocol.Frame{StreamID: streamID, Flags: flags, Payload: append([]byte(nil), payload...)}
		return nil
	}
	stream := sess.OpenStream()
	sess.startDispatcher(p)
	writeDone := make(chan error, 1)
	go func() { writeDone <- stream.WriteSYN([]byte(`{"type":"test"}`)) }()
	if err := waitResult(t, writeDone); err != nil {
		t.Fatalf("v4 WriteSYN: %v", err)
	}
	go func() { writeDone <- stream.WriteDAT([]byte("request")) }()
	if err := waitResult(t, writeDone); err != nil {
		t.Fatalf("v4 first WriteDAT: %v", err)
	}

	for i := 0; i < 2; i++ {
		select {
		case frame := <-sent:
			if frame.StreamID != stream.ID() {
				t.Fatalf("v4 sent initial control frame: %#v", frame)
			}
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for v4 stream frame")
		}
	}
	select {
	case frame := <-sent:
		t.Fatalf("v4 sent redundant initial control frame: %#v", frame)
	case <-time.After(20 * time.Millisecond):
	}

	p.dcMsg <- protocol.BuildFrame(stream.ID(), protocol.FlagDAT, []byte("response"))
	select {
	case frame := <-stream.Inbox():
		if got := string(frame.Payload); got != "response" {
			t.Fatalf("v4 immediate response = %q, want response", got)
		}
	case <-time.After(time.Second):
		t.Fatal("v4 immediate response was not accepted")
	}
}

func TestSessionV4RejectsDataBeforeSYN(t *testing.T) {
	_, sess := newDispatchTestSession(t, 1)
	sess.NegotiatedVersion = 4
	sent := make(chan protocol.Frame, 1)
	sess.sendFrameOverride = func(streamID uint32, flags uint16, payload []byte) error {
		sent <- protocol.Frame{StreamID: streamID, Flags: flags, Payload: append([]byte(nil), payload...)}
		return nil
	}
	stream := sess.OpenStream()

	if err := stream.WriteDAT([]byte("early")); !errors.Is(err, errStreamNotStarted) {
		t.Fatalf("pre-SYN WriteDAT error = %v, want %v", err, errStreamNotStarted)
	}
	frame := protocol.Frame{StreamID: stream.ID(), Payload: []byte("early")}
	if err := stream.st.enqueue(frame, true); !errors.Is(err, errStreamNotStarted) {
		t.Fatalf("pre-SYN inbound DAT error = %v, want %v", err, errStreamNotStarted)
	}
	select {
	case frame := <-sent:
		t.Fatalf("pre-SYN WriteDAT sent frame %#v", frame)
	case <-time.After(20 * time.Millisecond):
	}
}

func TestSessionFailedV4SYNClosesStream(t *testing.T) {
	_, sess := newDispatchTestSession(t, 1)
	sess.NegotiatedVersion = 4
	sess.sendFrameOverride = func(uint32, uint16, []byte) error {
		return errors.New("send failed")
	}
	stream := sess.OpenStream()

	if err := stream.WriteSYN([]byte(`{"type":"test"}`)); err == nil || err.Error() != "send failed" {
		t.Fatalf("failed SYN error = %v, want send failed", err)
	}
	if got := sess.findStream(stream.ID()); got != nil {
		t.Fatal("failed SYN left stream registered")
	}
	waitInboxClosed(t, stream.Inbox())
}

func TestSessionOversizedWriteFailsBeforeCreditWait(t *testing.T) {
	_, sess := newDispatchTestSession(t, 1)
	sess.NegotiatedVersion = 4
	stream := sess.OpenStream()
	if err := stream.WriteDAT(make([]byte, protocol.MaxChunkSize+1)); err == nil {
		t.Fatal("oversized v4 write did not fail")
	}
}

func TestSessionOpenStreamAfterCloseIsNotRegistered(t *testing.T) {
	_, sess := newDispatchTestSession(t, 1)
	sess.Close()
	stream := sess.OpenStream()
	if got := sess.findStream(stream.ID()); got != nil {
		t.Fatal("OpenStream registered a stream after session close")
	}
	if _, ok := <-stream.Inbox(); ok {
		t.Fatal("post-close stream inbox remained open")
	}
	if err := stream.WriteSYN([]byte(`{"type":"test"}`)); err == nil {
		t.Fatal("post-close stream write succeeded")
	}
}

func TestSessionSlowStreamDoesNotBlockAnotherStream(t *testing.T) {
	p, sess := newDispatchTestSession(t, protocol.MaxQueuedStreamFrames+2)
	slow := sess.OpenStream()
	fast := sess.OpenStream()
	sess.startDispatcher(p)

	for i := 0; i < protocol.MaxQueuedStreamFrames; i++ {
		p.dcMsg <- protocol.BuildFrame(slow.ID(), protocol.FlagDAT, []byte{byte(i)})
	}
	p.dcMsg <- protocol.BuildFrame(fast.ID(), protocol.FlagDAT, []byte("ready"))

	select {
	case frame := <-fast.Inbox():
		if got := string(frame.Payload); got != "ready" {
			t.Fatalf("fast stream payload = %q, want ready", got)
		}
	case <-time.After(time.Second):
		t.Fatal("fast stream was blocked behind a full slow stream")
	}
}

func TestSessionQueueOverflowClosesOnlyOffendingStream(t *testing.T) {
	p, sess := newDispatchTestSession(t, protocol.MaxQueuedStreamFrames+3)
	slow := sess.OpenStream()
	fast := sess.OpenStream()
	sess.startDispatcher(p)

	for i := 0; i <= protocol.MaxQueuedStreamFrames; i++ {
		p.dcMsg <- protocol.BuildFrame(slow.ID(), protocol.FlagDAT, []byte{byte(i)})
	}
	p.dcMsg <- protocol.BuildFrame(fast.ID(), protocol.FlagDAT, []byte("still-live"))

	select {
	case frame := <-fast.Inbox():
		if got := string(frame.Payload); got != "still-live" {
			t.Fatalf("fast stream payload = %q, want still-live", got)
		}
	case <-time.After(time.Second):
		t.Fatal("session stopped dispatching after another stream overflowed")
	}

	waitInboxClosed(t, slow.Inbox())
	select {
	case <-sess.Done():
		t.Fatal("stream overflow closed the whole session")
	default:
	}
}

func TestSessionReceiveQueueHasByteLimit(t *testing.T) {
	_, sess := newDispatchTestSession(t, 1)
	sess.NegotiatedVersion = 4
	st := sess.OpenStream().st
	st.activateInitialWindow()

	frame := protocol.Frame{StreamID: st.id, Payload: make([]byte, protocol.MaxChunkSize)}
	for i := 0; i < protocol.InitialStreamWindow/protocol.MaxChunkSize; i++ {
		if err := st.enqueue(frame, true); err != nil {
			t.Fatalf("enqueue frame %d: %v", i, err)
		}
	}
	if err := st.enqueue(protocol.Frame{StreamID: st.id, Payload: []byte{1}}, true); !errors.Is(err, errReceiveWindowExceeded) {
		t.Fatalf("over-window enqueue error = %v, want %v", err, errReceiveWindowExceeded)
	}
}

func TestSessionDataChannelCloseWakesFullStream(t *testing.T) {
	p, sess := newDispatchTestSession(t, protocol.MaxQueuedStreamFrames)
	stream := sess.OpenStream()
	sess.startDispatcher(p)

	for i := 0; i < protocol.MaxQueuedStreamFrames; i++ {
		p.dcMsg <- protocol.BuildFrame(stream.ID(), protocol.FlagDAT, []byte{byte(i)})
	}
	waitFor(t, func() bool { return len(p.dcMsg) == 0 }, "dispatcher to drain its input queue")

	close(p.dcClosed)
	select {
	case <-sess.Done():
	case <-time.After(time.Second):
		t.Fatal("session remained open after data channel closure")
	}
	waitInboxClosed(t, stream.Inbox())
}

func TestSessionFINKeepsOppositeDirectionRoutable(t *testing.T) {
	p, sess := newDispatchTestSession(t, 1)
	sess.NegotiatedVersion = 4
	stream := sess.OpenStream()
	stream.st.activateInitialWindow()
	sess.startDispatcher(p)

	p.dcMsg <- protocol.BuildFrame(stream.ID(), protocol.FlagFIN, nil)
	select {
	case frame, ok := <-stream.Inbox():
		if !ok || !frame.IsFIN() {
			t.Fatalf("inbound frame = %#v, open = %v; want FIN", frame, ok)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for FIN")
	}
	if _, ok := <-stream.Inbox(); ok {
		t.Fatal("inbox remained open after FIN")
	}
	if got := sess.findStream(stream.ID()); got != stream.st {
		t.Fatal("inbound FIN removed state for the still-open outbound direction")
	}

	stream.st.outboundFIN.Store(true)
	sess.detachFinishedStream(stream.st)
	if got := sess.findStream(stream.ID()); got != nil {
		t.Fatal("stream state remained after both directions finished")
	}
}

func TestSessionDataAfterFINResetsOnlyOffendingStream(t *testing.T) {
	p, sess := newDispatchTestSession(t, 2)
	sess.NegotiatedVersion = 4
	sent := make(chan protocol.Frame, 1)
	sess.sendFrameOverride = func(streamID uint32, flags uint16, payload []byte) error {
		sent <- protocol.Frame{StreamID: streamID, Flags: flags, Payload: append([]byte(nil), payload...)}
		return nil
	}
	stream := sess.OpenStream()
	stream.st.activateInitialWindow()
	sess.startDispatcher(p)

	if err := stream.st.reserveSend(protocol.InitialStreamWindow, sess.Done()); err != nil {
		t.Fatalf("reserve implicit initial window: %v", err)
	}
	blockedSend := make(chan error, 1)
	go func() { blockedSend <- stream.st.reserveSend(1, sess.Done()) }()
	assertBlocked(t, blockedSend)

	p.dcMsg <- protocol.BuildFrame(stream.ID(), protocol.FlagFIN, nil)
	p.dcMsg <- protocol.BuildFrame(stream.ID(), protocol.FlagDAT, []byte("late"))
	if err := waitResult(t, blockedSend); err == nil {
		t.Fatal("post-FIN protocol error did not wake blocked sender")
	}

	select {
	case frame := <-sent:
		var reset protocol.StreamReset
		if frame.StreamID != 0 || json.Unmarshal(frame.Payload, &reset) != nil ||
			reset.Type != protocol.ControlStreamReset || reset.StreamID != stream.ID() ||
			reset.Code != "protocol_error" {
			t.Fatalf("post-FIN reset = frame %#v reset %#v", frame, reset)
		}
	case <-time.After(time.Second):
		t.Fatal("post-FIN data did not notify peer")
	}
	waitInboxClosed(t, stream.Inbox())
	select {
	case <-sess.Done():
		t.Fatal("post-FIN data closed the whole session")
	default:
	}
}

func TestSessionOwnsStreamUntilQueuedFINIsDelivered(t *testing.T) {
	p, sess := newDispatchTestSession(t, 2)
	sess.NegotiatedVersion = 4
	sess.sendFrameOverride = func(uint32, uint16, []byte) error { return nil }
	stream := sess.OpenStream()
	stream.st.activateInitialWindow()
	sess.startDispatcher(p)

	// Leave the public inbox unread so the delivery worker is blocked on the
	// data frame while the dispatcher queues FIN behind it.
	p.dcMsg <- protocol.BuildFrame(stream.ID(), protocol.FlagDAT, []byte("queued"))
	p.dcMsg <- protocol.BuildFrame(stream.ID(), protocol.FlagFIN, nil)
	waitFor(t, func() bool { return stream.st.inboundFIN.Load() }, "dispatcher to queue FIN")

	if err := stream.WriteFIN(nil); err != nil {
		t.Fatalf("WriteFIN: %v", err)
	}
	if got := sess.findStream(stream.ID()); got != stream.st {
		t.Fatal("stream detached before queued FIN reached its consumer")
	}

	sess.Close()
	select {
	case <-stream.st.abandoned:
	case <-time.After(time.Second):
		t.Fatal("session close could not reach stream with queued FIN")
	}
	waitInboxClosed(t, stream.Inbox())
}

func TestStreamSendCreditIsCumulativeAndTeardownSafe(t *testing.T) {
	_, sess := newDispatchTestSession(t, 1)
	sess.NegotiatedVersion = 4
	st := sess.OpenStream().st
	st.activateInitialWindow()

	if err := st.reserveSend(protocol.InitialStreamWindow, sess.Done()); err != nil {
		t.Fatalf("reserve implicit initial credit: %v", err)
	}

	result := make(chan error, 1)
	go func() { result <- st.reserveSend(1, sess.Done()) }()
	st.updateSendLimit(protocol.InitialStreamWindow - 1) // stale update must not reduce credit
	assertBlocked(t, result)
	st.updateSendLimit(protocol.InitialStreamWindow + 1)
	if err := waitResult(t, result); err != nil {
		t.Fatalf("reserve replenished credit: %v", err)
	}

	go func() { result <- st.reserveSend(1, sess.Done()) }()
	assertBlocked(t, result)
	sess.Close()
	if err := waitResult(t, result); err == nil {
		t.Fatal("blocked send returned nil after session close")
	}
}

func TestSessionHandlesWindowUpdateAndStreamReset(t *testing.T) {
	_, sess := newDispatchTestSession(t, 1)
	sess.NegotiatedVersion = 4
	stream := sess.OpenStream()
	stream.st.activateInitialWindow()

	update, _ := json.Marshal(protocol.WindowUpdate{
		Type:     protocol.ControlWindowUpdate,
		StreamID: stream.ID(),
		MaxBytes: protocol.InitialStreamWindow + 123,
	})
	if !sess.handleControl(protocol.Frame{StreamID: 0, Flags: protocol.FlagSYN, Payload: update}) {
		t.Fatal("window update was not handled")
	}
	if err := stream.st.reserveSend(protocol.InitialStreamWindow+123, sess.Done()); err != nil {
		t.Fatalf("reserve updated window: %v", err)
	}

	reset, _ := json.Marshal(protocol.StreamReset{
		Type:     protocol.ControlStreamReset,
		StreamID: stream.ID(),
		Code:     "peer_error",
		Message:  "target closed",
	})
	if !sess.handleControl(protocol.Frame{StreamID: 0, Flags: protocol.FlagSYN, Payload: reset}) {
		t.Fatal("stream reset was not handled")
	}
	waitInboxClosed(t, stream.Inbox())
	if err := stream.st.reserveSend(1, sess.Done()); err == nil || err.Error() != "target closed" {
		t.Fatalf("send after reset error = %v, want target closed", err)
	}
	select {
	case <-sess.Done():
		t.Fatal("stream reset closed the whole session")
	default:
	}
}

func TestSessionMalformedControlResetsKnownStream(t *testing.T) {
	_, sess := newDispatchTestSession(t, 1)
	sess.NegotiatedVersion = 4
	stream := sess.OpenStream()
	payload := []byte(`{"type":"window_update","stream_id":1,"max_bytes":"invalid"}`)

	if !sess.handleControl(protocol.Frame{StreamID: 0, Flags: protocol.FlagSYN, Payload: payload}) {
		t.Fatal("malformed window update was not handled")
	}
	waitInboxClosed(t, stream.Inbox())
	if got := sess.findStream(stream.ID()); got != nil {
		t.Fatal("malformed control left its target stream active")
	}
	select {
	case <-sess.Done():
		t.Fatal("malformed stream control closed the whole session")
	default:
	}
}

func TestStreamCloseNotifiesPeerWhenOutboundDirectionIsOpen(t *testing.T) {
	for _, tt := range []struct {
		name    string
		version int
	}{
		{name: "legacy FIN", version: 3},
		{name: "v4 reset", version: 4},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, sess := newDispatchTestSession(t, 1)
			sess.NegotiatedVersion = tt.version
			sent := make(chan protocol.Frame, 2)
			sess.sendFrameOverride = func(streamID uint32, flags uint16, payload []byte) error {
				sent <- protocol.Frame{StreamID: streamID, Flags: flags, Payload: append([]byte(nil), payload...)}
				return nil
			}
			stream := sess.OpenStream()

			stream.Close()
			var frame protocol.Frame
			select {
			case frame = <-sent:
			case <-time.After(time.Second):
				t.Fatal("Stream.Close did not notify peer")
			}
			if tt.version < 4 {
				if frame.StreamID != stream.ID() || !frame.IsFIN() || len(frame.Payload) != 0 {
					t.Fatalf("legacy close frame = %#v, want empty stream FIN", frame)
				}
			} else {
				if frame.StreamID != 0 || !frame.IsSYN() {
					t.Fatalf("v4 close frame = %#v, want stream-0 SYN", frame)
				}
				var reset protocol.StreamReset
				if err := json.Unmarshal(frame.Payload, &reset); err != nil {
					t.Fatalf("parse reset: %v", err)
				}
				if reset.Type != protocol.ControlStreamReset || reset.StreamID != stream.ID() {
					t.Fatalf("reset = %#v, want stream %d reset", reset, stream.ID())
				}
			}

			// Close is idempotent and must not emit duplicate terminal controls.
			stream.Close()
			select {
			case duplicate := <-sent:
				t.Fatalf("second Stream.Close sent %#v", duplicate)
			case <-time.After(20 * time.Millisecond):
			}
			if err := stream.WriteDAT([]byte("late")); err == nil {
				t.Fatal("detached stream accepted a later write")
			}
		})
	}
}

func TestV4StreamCloseCancelsResponseAfterOutboundFIN(t *testing.T) {
	_, sess := newDispatchTestSession(t, 1)
	sess.NegotiatedVersion = 4
	sent := make(chan protocol.Frame, 1)
	sess.sendFrameOverride = func(streamID uint32, flags uint16, payload []byte) error {
		sent <- protocol.Frame{StreamID: streamID, Flags: flags, Payload: append([]byte(nil), payload...)}
		return nil
	}
	stream := sess.OpenStream()
	stream.st.outboundFIN.Store(true)

	stream.Close()
	select {
	case frame := <-sent:
		var reset protocol.StreamReset
		if frame.StreamID != 0 || json.Unmarshal(frame.Payload, &reset) != nil ||
			reset.Type != protocol.ControlStreamReset || reset.StreamID != stream.ID() {
			t.Fatalf("close frame = %#v reset = %#v, want v4 reset", frame, reset)
		}
	case <-time.After(time.Second):
		t.Fatal("Stream.Close did not cancel in-flight response")
	}
}

func TestStreamResetFollowsAlreadyReservedData(t *testing.T) {
	_, sess := newDispatchTestSession(t, 1)
	sess.NegotiatedVersion = 4
	stream := sess.OpenStream()
	stream.st.activateInitialWindow()

	dataStarted := make(chan struct{})
	releaseData := make(chan struct{})
	events := make(chan string, 2)
	sess.sendFrameOverride = func(streamID uint32, _ uint16, _ []byte) error {
		if streamID == stream.ID() {
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
	sess.failStream(stream.st, "test_reset", "reset", true)
	select {
	case event := <-events:
		t.Fatalf("terminal event %q overtook blocked data", event)
	case <-time.After(20 * time.Millisecond):
	}

	close(releaseData)
	if err := waitResult(t, writeDone); err != nil {
		t.Fatalf("in-flight data send: %v", err)
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

func newDispatchTestSession(t *testing.T, messageBuffer int) (*Peer, *Session) {
	t.Helper()
	p := &Peer{
		dcMsg:    make(chan []byte, messageBuffer),
		dcClosed: make(chan struct{}),
	}
	sess := newSession(p)
	t.Cleanup(sess.Close)
	return p, sess
}

func waitInboxClosed(t *testing.T, inbox <-chan protocol.Frame) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		for range inbox {
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("stream inbox remained open")
	}
}

func waitFor(t *testing.T, condition func() bool, description string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", description)
		}
		time.Sleep(time.Millisecond)
	}
}

func assertBlocked(t *testing.T, result <-chan error) {
	t.Helper()
	select {
	case err := <-result:
		t.Fatalf("operation returned early: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
}

func waitResult(t *testing.T, result <-chan error) error {
	t.Helper()
	select {
	case err := <-result:
		return err
	case <-time.After(time.Second):
		t.Fatal("operation did not finish")
		return nil
	}
}
