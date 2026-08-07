package streamtype

import (
	"context"
	"encoding/json"
	"log"
	"net"
	"strconv"
	"sync"

	"github.com/richlegrand/bitbang/internal/bytestream"
	"github.com/richlegrand/bitbang/internal/localdns"
	"github.com/richlegrand/bitbang/internal/protocol"
	"github.com/richlegrand/bitbang/internal/tcpforward"
)

// DefaultTCPMaxConcurrent bounds active TCP connections per WebRTC session.
const DefaultTCPMaxConcurrent = 64

// TCPHandler implements StreamHandler for type="tcp". Each stream dials one
// target from the listener's network and preserves directional EOF in both
// directions.
type TCPHandler struct {
	Verbose bool

	// MaxConcurrent caps active TCP streams for this WebRTC session,
	// including pending dials. 0 disables the limit.
	MaxConcurrent int

	// DialContext is injectable for focused tests. Production uses the same
	// mDNS-aware resolver as the HTTP and WebSocket proxy paths.
	DialContext func(context.Context, string, string) (net.Conn, error)

	ctx     context.Context
	cancel  context.CancelFunc
	mu      sync.Mutex
	streams map[uint32]*tcpStream
	active  int
}

type tcpStream struct {
	ctx    context.Context
	cancel context.CancelFunc
	stream Stream
	ready  chan struct{}

	mu      sync.Mutex
	conn    net.Conn
	dialErr error
	writeMu sync.Mutex

	writeDone     chan struct{}
	writeDoneOnce sync.Once
}

// NewTCP returns a per-WebRTC-session TCP handler.
func NewTCP(verbose bool) *TCPHandler {
	ctx, cancel := context.WithCancel(context.Background())
	return &TCPHandler{
		Verbose:       verbose,
		MaxConcurrent: DefaultTCPMaxConcurrent,
		DialContext:   localdns.Default.DialContext,
		ctx:           ctx,
		cancel:        cancel,
		streams:       make(map[uint32]*tcpStream),
	}
}

func (h *TCPHandler) Type() string             { return "tcp" }
func (h *TCPHandler) OnConnect(_ string) error { return nil }

func (h *TCPHandler) OnSYN(s Stream, payload []byte, final bool) error {
	var open protocol.TCPOpen
	if err := json.Unmarshal(payload, &open); err != nil {
		h.sendError(s, "bad tcp request: "+err.Error())
		return nil
	}
	if err := tcpforward.ValidateTarget(open.Host, open.Port); err != nil {
		h.sendError(s, err.Error())
		return nil
	}

	h.mu.Lock()
	old := h.streams[s.ID()]
	if h.MaxConcurrent > 0 && h.active >= h.MaxConcurrent {
		h.mu.Unlock()
		log.Printf("TCP rejected: at max-streams=%d", h.MaxConcurrent)
		h.sendError(s, "listener is busy (max "+strconv.Itoa(h.MaxConcurrent)+" concurrent TCP connections)")
		return nil
	}
	ctx, cancel := context.WithCancel(h.ctx)
	ts := &tcpStream{
		ctx:       ctx,
		cancel:    cancel,
		stream:    s,
		ready:     make(chan struct{}),
		writeDone: make(chan struct{}),
	}
	h.streams[s.ID()] = ts
	h.active++
	h.mu.Unlock()
	if old != nil {
		old.close()
	}

	go h.runStream(ts, open, final)
	return nil
}

func (h *TCPHandler) OnDAT(s Stream, payload []byte) error {
	ts := h.lookup(s.ID())
	if ts == nil {
		return nil
	}
	if err := ts.write(payload); err != nil {
		ts.close()
		return err
	}
	return nil
}

func (h *TCPHandler) OnFIN(s Stream, payload []byte) error {
	ts := h.lookup(s.ID())
	if ts == nil {
		return nil
	}
	if len(payload) > 0 {
		if err := h.OnDAT(s, payload); err != nil {
			return err
		}
	}
	if err := ts.closeWrite(); err != nil {
		ts.close()
		return err
	}
	return nil
}

func (h *TCPHandler) OnReset(s Stream, _, _ string) {
	if ts := h.lookup(s.ID()); ts != nil {
		ts.close()
	}
}

func (h *TCPHandler) runStream(ts *tcpStream, open protocol.TCPOpen, final bool) {
	defer h.remove(ts.stream.ID(), ts)

	address := net.JoinHostPort(open.Host, strconv.Itoa(open.Port))
	conn, err := h.DialContext(ts.ctx, "tcp", address)
	if err != nil {
		if ts.ctx.Err() == nil {
			h.sendError(ts.stream, "dial "+address+": "+err.Error())
		}
		ts.finishDial(nil, err)
		ts.cancel()
		return
	}
	if !ts.finishDial(conn, nil) {
		_ = conn.Close()
		return
	}

	ack, _ := json.Marshal(map[string]string{"status": "ok"})
	if err := ts.stream.WriteSYN(ack); err != nil {
		ts.cancel()
		_ = conn.Close()
		return
	}
	if h.Verbose {
		log.Printf("TCP stream %d connected to %s", ts.stream.ID(), address)
	}

	if final {
		if err := ts.closeWrite(); err != nil {
			ts.close()
			return
		}
	}

	_, pumpErr := bytestream.Pump(ts.ctx, conn, ts.stream)
	if pumpErr == nil {
		_ = bytestream.CloseRead(conn)
		select {
		case <-ts.writeDone:
		case <-ts.ctx.Done():
		}
	}
	ts.close()
	if h.Verbose {
		log.Printf("TCP stream %d closed (%s)", ts.stream.ID(), address)
	}
}

func (h *TCPHandler) lookup(id uint32) *tcpStream {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.streams[id]
}

func (h *TCPHandler) remove(id uint32, want *tcpStream) {
	h.mu.Lock()
	if h.streams[id] == want {
		delete(h.streams, id)
	}
	h.active--
	h.mu.Unlock()
}

func (ts *tcpStream) finishDial(conn net.Conn, err error) bool {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if ts.ctx.Err() != nil {
		ts.dialErr = ts.ctx.Err()
		close(ts.ready)
		return false
	}
	ts.conn = conn
	ts.dialErr = err
	close(ts.ready)
	return true
}

func (ts *tcpStream) waitConn() (net.Conn, error) {
	select {
	case <-ts.ready:
		ts.mu.Lock()
		defer ts.mu.Unlock()
		if ts.conn != nil {
			return ts.conn, nil
		}
		if ts.dialErr != nil {
			return nil, ts.dialErr
		}
		return nil, net.ErrClosed
	case <-ts.ctx.Done():
		return nil, ts.ctx.Err()
	}
}

func (ts *tcpStream) write(payload []byte) error {
	conn, err := ts.waitConn()
	if err != nil {
		return err
	}
	ts.writeMu.Lock()
	defer ts.writeMu.Unlock()
	return bytestream.WriteFull(conn, payload)
}

func (ts *tcpStream) closeWrite() error {
	conn, err := ts.waitConn()
	if err != nil {
		return err
	}
	ts.writeMu.Lock()
	err = bytestream.CloseWrite(conn)
	ts.writeMu.Unlock()
	ts.writeDoneOnce.Do(func() { close(ts.writeDone) })
	return err
}

func (ts *tcpStream) close() {
	ts.cancel()
	ts.mu.Lock()
	conn := ts.conn
	ts.mu.Unlock()
	if conn != nil {
		_ = conn.Close()
	}
}

// CloseAll cancels pending dials and closes every target socket owned by this
// WebRTC session.
func (h *TCPHandler) CloseAll() {
	h.cancel()
	h.mu.Lock()
	streams := make([]*tcpStream, 0, len(h.streams))
	for _, ts := range h.streams {
		streams = append(streams, ts)
	}
	h.mu.Unlock()
	for _, ts := range streams {
		ts.close()
	}
}

func (h *TCPHandler) sendError(s Stream, message string) {
	payload, _ := json.Marshal(map[string]string{"status": "error", "error": message})
	_ = s.SendRaw(protocol.FlagSYN|protocol.FlagFIN, payload)
}
