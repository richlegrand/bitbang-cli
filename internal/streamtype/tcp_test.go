package streamtype

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/richlegrand/bitbang/internal/protocol"
)

type tcpTestStream struct {
	id     uint32
	frames chan protocol.Frame
}

func newTCPTestStream() *tcpTestStream {
	return newTCPTestStreamID(7)
}

func newTCPTestStreamID(id uint32) *tcpTestStream {
	return &tcpTestStream{id: id, frames: make(chan protocol.Frame, 32)}
}

func (s *tcpTestStream) ID() uint32          { return s.id }
func (s *tcpTestStream) ConnectPath() string { return "/" }
func (s *tcpTestStream) WriteSYN(p []byte) error {
	s.frames <- protocol.Frame{StreamID: s.id, Flags: protocol.FlagSYN, Payload: append([]byte(nil), p...)}
	return nil
}
func (s *tcpTestStream) WriteDAT(p []byte) error {
	s.frames <- protocol.Frame{StreamID: s.id, Flags: protocol.FlagDAT, Payload: append([]byte(nil), p...)}
	return nil
}
func (s *tcpTestStream) WriteFIN(p []byte) error {
	s.frames <- protocol.Frame{StreamID: s.id, Flags: protocol.FlagFIN, Payload: append([]byte(nil), p...)}
	return nil
}
func (s *tcpTestStream) SendRaw(flags uint16, p []byte) error {
	s.frames <- protocol.Frame{StreamID: s.id, Flags: flags, Payload: append([]byte(nil), p...)}
	return nil
}
func (s *tcpTestStream) BufferedAmount() uint64 { return 0 }

type shortConn struct {
	reader *bytes.Reader
	limit  int

	mu          sync.Mutex
	written     bytes.Buffer
	writeClosed bool
	closed      bool
}

func newShortConn(readData []byte, limit int) *shortConn {
	return &shortConn{reader: bytes.NewReader(readData), limit: limit}
}

func (c *shortConn) Read(p []byte) (int, error) { return c.reader.Read(p) }
func (c *shortConn) Write(p []byte) (int, error) {
	if len(p) > c.limit {
		p = p[:c.limit]
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.written.Write(p)
}
func (c *shortConn) Close() error {
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
	return nil
}
func (c *shortConn) CloseWrite() error {
	c.mu.Lock()
	c.writeClosed = true
	c.mu.Unlock()
	return nil
}
func (c *shortConn) LocalAddr() net.Addr              { return &net.TCPAddr{} }
func (c *shortConn) RemoteAddr() net.Addr             { return &net.TCPAddr{} }
func (c *shortConn) SetDeadline(time.Time) error      { return nil }
func (c *shortConn) SetReadDeadline(time.Time) error  { return nil }
func (c *shortConn) SetWriteDeadline(time.Time) error { return nil }

func tcpSYN(t *testing.T, h *TCPHandler, s Stream, host string, port int) {
	t.Helper()
	payload, _ := json.Marshal(protocol.TCPOpen{Type: "tcp", Host: host, Port: port})
	if err := h.OnSYN(s, payload, false); err != nil {
		t.Fatalf("OnSYN: %v", err)
	}
}

func nextTCPFrame(t *testing.T, s *tcpTestStream) protocol.Frame {
	t.Helper()
	select {
	case frame := <-s.frames:
		return frame
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for TCP frame")
		return protocol.Frame{}
	}
}

func TestTCPHandlerDialErrorUsesSYNFIN(t *testing.T) {
	h := NewTCP(false)
	h.DialContext = func(context.Context, string, string) (net.Conn, error) {
		return nil, errors.New("connection refused")
	}
	s := newTCPTestStream()
	tcpSYN(t, h, s, "db.internal", 5432)

	frame := nextTCPFrame(t, s)
	if !frame.IsSYN() || !frame.IsFIN() {
		t.Fatalf("flags = %#x, want SYN|FIN", frame.Flags)
	}
	var status struct {
		Status string `json:"status"`
		Error  string `json:"error"`
	}
	if err := json.Unmarshal(frame.Payload, &status); err != nil {
		t.Fatalf("response JSON: %v", err)
	}
	if status.Status != "error" || status.Error == "" {
		t.Fatalf("status = %#v, want an error", status)
	}
}

func TestTCPHandlerBinaryDataPartialWritesHalfCloseAndTargetEOF(t *testing.T) {
	fromTarget := []byte{0, 1, 2, 0xff, 'o', 'k'}
	toTarget := []byte{0xff, 0, 'a', 'b', 'c', 0, 'z'}
	conn := newShortConn(fromTarget, 2)
	h := NewTCP(false)
	h.DialContext = func(context.Context, string, string) (net.Conn, error) { return conn, nil }
	s := newTCPTestStream()
	tcpSYN(t, h, s, "127.0.0.1", 9000)

	ack := nextTCPFrame(t, s)
	if !ack.IsSYN() || ack.IsFIN() {
		t.Fatalf("ack flags = %#x, want SYN", ack.Flags)
	}
	if err := h.OnDAT(s, toTarget[:3]); err != nil {
		t.Fatalf("OnDAT 1: %v", err)
	}
	if err := h.OnDAT(s, toTarget[3:]); err != nil {
		t.Fatalf("OnDAT 2: %v", err)
	}
	if err := h.OnFIN(s, nil); err != nil {
		t.Fatalf("OnFIN: %v", err)
	}

	dat := nextTCPFrame(t, s)
	fin := nextTCPFrame(t, s)
	if !bytes.Equal(dat.Payload, fromTarget) || dat.IsSYN() || dat.IsFIN() {
		t.Fatalf("target DAT = %v flags=%#x, want %v", dat.Payload, dat.Flags, fromTarget)
	}
	if !fin.IsFIN() {
		t.Fatalf("target EOF flags = %#x, want FIN", fin.Flags)
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		conn.mu.Lock()
		got := append([]byte(nil), conn.written.Bytes()...)
		halfClosed := conn.writeClosed
		closed := conn.closed
		conn.mu.Unlock()
		if bytes.Equal(got, toTarget) && halfClosed && closed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("target got %v halfClosed=%v closed=%v, want %v/true/true", got, halfClosed, closed, toTarget)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestTCPHandlerCloseAllCancelsPendingDial(t *testing.T) {
	started := make(chan struct{})
	finished := make(chan error, 1)
	h := NewTCP(false)
	h.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
		close(started)
		<-ctx.Done()
		finished <- ctx.Err()
		return nil, ctx.Err()
	}
	s := newTCPTestStream()
	tcpSYN(t, h, s, "slow.internal", 80)
	<-started
	h.CloseAll()
	select {
	case err := <-finished:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("dial error = %v, want context canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("pending dial was not canceled")
	}
	select {
	case frame := <-s.frames:
		t.Fatalf("session cleanup emitted unexpected frame: %#v", frame)
	case <-time.After(20 * time.Millisecond):
	}
}

func TestTCPHandlerResetUnblocksStalledTargetWrite(t *testing.T) {
	conn, target := net.Pipe()
	defer target.Close()

	h := NewTCP(false)
	h.DialContext = func(context.Context, string, string) (net.Conn, error) {
		return conn, nil
	}
	s := newTCPTestStream()
	tcpSYN(t, h, s, "127.0.0.1", 9000)
	if ack := nextTCPFrame(t, s); !ack.IsSYN() || ack.IsFIN() {
		t.Fatalf("ack flags = %#x, want SYN", ack.Flags)
	}

	done := make(chan error, 1)
	go func() {
		done <- h.OnDAT(s, bytes.Repeat([]byte("x"), protocol.MaxChunkSize))
	}()
	select {
	case err := <-done:
		t.Fatalf("stalled target write returned early: %v", err)
	case <-time.After(20 * time.Millisecond):
	}

	h.OnReset(s, "canceled", "test reset")
	h.OnReset(s, "canceled", "duplicate reset")
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("stalled target write returned nil after reset")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("reset did not unblock stalled target write")
	}
}

func TestTCPHandlerRejectsStreamsOverLimit(t *testing.T) {
	started := make(chan struct{}, 2)
	finished := make(chan struct{}, 2)
	h := NewTCP(false)
	h.MaxConcurrent = 1
	h.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
		started <- struct{}{}
		<-ctx.Done()
		finished <- struct{}{}
		return nil, ctx.Err()
	}

	first := newTCPTestStreamID(7)
	tcpSYN(t, h, first, "first.internal", 80)
	<-started

	second := newTCPTestStreamID(9)
	tcpSYN(t, h, second, "second.internal", 80)
	frame := nextTCPFrame(t, second)
	if !frame.IsSYN() || !frame.IsFIN() {
		t.Fatalf("flags = %#x, want SYN|FIN", frame.Flags)
	}
	var status struct {
		Status string `json:"status"`
		Error  string `json:"error"`
	}
	if err := json.Unmarshal(frame.Payload, &status); err != nil {
		t.Fatalf("response JSON: %v", err)
	}
	if status.Status != "error" || !strings.Contains(status.Error, "max 1") {
		t.Fatalf("status = %#v, want an error", status)
	}
	select {
	case <-started:
		t.Fatal("over-limit stream reached DialContext")
	default:
	}

	h.CloseAll()
	select {
	case <-finished:
	case <-time.After(2 * time.Second):
		t.Fatal("accepted stream was not canceled")
	}
}

var _ net.Conn = (*shortConn)(nil)
var _ io.Writer = (*shortConn)(nil)
var _ ResetHandler = (*TCPHandler)(nil)
