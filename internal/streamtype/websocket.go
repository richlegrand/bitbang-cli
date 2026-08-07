package streamtype

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"sync"

	"github.com/gorilla/websocket"
	"github.com/richlegrand/bitbang/internal/bytestream"
	"github.com/richlegrand/bitbang/internal/localdns"
	"github.com/richlegrand/bitbang/internal/protocol"
)

// wsDialer mirrors websocket.DefaultDialer but resolves .local targets over
// mDNS, which a CGO_ENABLED=0 build cannot do through the system resolver.
var wsDialer = &websocket.Dialer{
	Proxy:            websocket.DefaultDialer.Proxy,
	HandshakeTimeout: websocket.DefaultDialer.HandshakeTimeout,
	NetDialContext:   localdns.Default.DialContext,
}

// wss:// variants. Split so the trust decision mirrors the HTTP side
// exactly: local targets skip verification (home devices self-sign),
// public ones verify normally.
var wsDialerSecure = &websocket.Dialer{
	Proxy:            websocket.DefaultDialer.Proxy,
	HandshakeTimeout: websocket.DefaultDialer.HandshakeTimeout,
	NetDialContext:   localdns.Default.DialContext,
}

var wsDialerInsecure = &websocket.Dialer{
	Proxy:            websocket.DefaultDialer.Proxy,
	HandshakeTimeout: websocket.DefaultDialer.HandshakeTimeout,
	NetDialContext:   localdns.Default.DialContext,
	TLSClientConfig:  &tls.Config{InsecureSkipVerify: true},
}

// WSHandler implements StreamHandler for type="websocket". Bridges a SWSP
// WebSocket-on-stream-N to a real ws:// connection to a local server.
//
// Resolves the target the same way HTTPHandler does — via the HTTPHandler
// it's paired with. (Both are per-session, share session state.)
type WSHandler struct {
	// Resolver supplies the current target + path-rewriting logic. In
	// proxy mode it's the paired HTTPHandler; other modes can substitute.
	Resolver TargetResolver
	// BrowserIP is the real client IP, stamped as X-Forwarded-For on the
	// upstream WS handshake for the same reason as HTTPHandler.BrowserIP —
	// the SockJS upgrade is an HTTP request subject to the same autologin.
	BrowserIP string
	Verbose   bool

	mu      sync.Mutex
	streams map[uint32]*wsStream
}

// TargetResolver maps a SWSP request path to a (target host, ws path) pair.
type TargetResolver interface {
	ResolveTarget(requestPath string) (target, path string)
}

// NewWebSocket constructs a WSHandler. resolver is typically the paired
// HTTPHandler so that WS streams use the same dynamic-target logic.
func NewWebSocket(resolver TargetResolver, browserIP string, verbose bool) *WSHandler {
	return &WSHandler{
		Resolver:  resolver,
		BrowserIP: browserIP,
		Verbose:   verbose,
		streams:   make(map[uint32]*wsStream),
	}
}

type wsStream struct {
	ctx    context.Context
	cancel context.CancelFunc

	mu   sync.Mutex
	conn *websocket.Conn

	writeMu sync.Mutex
	writer  io.WriteCloser
}

func (h *WSHandler) Type() string { return "websocket" }

func (h *WSHandler) OnConnect(_ string) error { return nil }

// OnSYN opens a real WebSocket to the local target and starts the read loop.
func (h *WSHandler) OnSYN(s Stream, payload []byte, _ bool) error {
	var msg struct {
		Pathname string `json:"pathname"`
		Cookies  string `json:"cookies"`
	}
	if err := json.Unmarshal(payload, &msg); err != nil {
		log.Printf("Failed to parse WS open: %v", err)
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	ws := &wsStream{ctx: ctx, cancel: cancel}
	h.mu.Lock()
	old := h.streams[s.ID()]
	h.streams[s.ID()] = ws
	h.mu.Unlock()
	if old != nil {
		old.close()
	}
	go h.bridge(s, ws, msg.Pathname, msg.Cookies)
	return nil
}

// OnDAT forwards a message from the browser to the local WS server.
func (h *WSHandler) OnDAT(s Stream, payload []byte) error {
	return h.OnFragment(s, payload, false)
}

// OnFragment streams one browser message into a single upstream WebSocket
// writer without assembling it in the session receive loop.
func (h *WSHandler) OnFragment(s Stream, payload []byte, more bool) error {
	h.mu.Lock()
	ws := h.streams[s.ID()]
	h.mu.Unlock()
	if ws == nil {
		return nil
	}
	if err := ws.writeFragment(payload, more); err != nil {
		ws.close()
		return err
	}
	return nil
}

// OnFIN closes the upstream WebSocket.
func (h *WSHandler) OnFIN(s Stream, _ []byte) error {
	h.mu.Lock()
	ws := h.streams[s.ID()]
	delete(h.streams, s.ID())
	h.mu.Unlock()
	if ws != nil {
		ws.close()
	}
	return nil
}

func (h *WSHandler) OnReset(s Stream, _, _ string) {
	h.mu.Lock()
	ws := h.streams[s.ID()]
	delete(h.streams, s.ID())
	h.mu.Unlock()
	if ws != nil {
		ws.close()
	}
}

func (h *WSHandler) bridge(s Stream, ws *wsStream, pathname, cookies string) {
	target, wsPath := h.Resolver.ResolveTarget(pathname)

	// An HTTPS session must dial wss://, or the handshake hits a TLS port
	// in plaintext and fails. The scheme and the trust decision both come
	// from the paired HTTPHandler via optional interfaces, so other
	// TargetResolver implementations stay unaffected.
	scheme, dialer := "ws", wsDialer
	if sp, ok := h.Resolver.(interface{ Scheme() string }); ok && sp.Scheme() == "https" {
		scheme = "wss"
		dialer = wsDialerSecure
		if sv, ok := h.Resolver.(interface{ SkipVerify() bool }); ok && sv.SkipVerify() {
			dialer = wsDialerInsecure
		}
	}
	wsURL := fmt.Sprintf("%s://%s%s", scheme, target, wsPath)
	header := http.Header{}
	if cookies != "" {
		header.Set("Cookie", cookies)
	}
	// Only stamped in fixed-target mode (serve.go withholds it otherwise);
	// validated as a real IP so we never emit a malformed XFF.
	if ip := net.ParseIP(h.BrowserIP); ip != nil {
		header.Set("X-Forwarded-For", ip.String())
	}
	conn, _, err := dialer.DialContext(ws.ctx, wsURL, header)
	if err != nil {
		if ws.ctx.Err() == nil {
			log.Printf("WS connect failed: %s -> %v", pathname, err)
			_ = s.WriteFIN(nil)
		}
		h.remove(s.ID(), ws)
		return
	}
	if !ws.setConn(conn) {
		_ = conn.Close()
		h.remove(s.ID(), ws)
		return
	}
	log.Printf("WS opened: %s (stream %d)", pathname, s.ID())

	_ = s.WriteSYN(nil)

	defer func() {
		ws.close()
		h.remove(s.ID(), ws)
		_ = s.WriteFIN(nil)
		log.Printf("WS closed: %s (stream %d)", pathname, s.ID())
	}()

	for {
		msgType, reader, err := conn.NextReader()
		if err != nil {
			return
		}
		if err := writeWSReader(s, msgType, reader); err != nil {
			return
		}
	}
}

// writeWSReader sends one WebSocket message as bounded SWSP fragments. The
// one-byte lookahead determines MORE without reading the whole message.
func writeWSReader(s Stream, msgType int, src io.Reader) error {
	reader := bufio.NewReaderSize(src, protocol.MaxChunkSize)
	buf := make([]byte, protocol.MaxChunkSize)
	first := true
	for {
		start := 0
		if first {
			if msgType == websocket.BinaryMessage {
				buf[0] = 1
			} else {
				buf[0] = 0
			}
			start = 1
		}

		n, err := io.ReadFull(reader, buf[start:])
		if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
			return err
		}
		payload := buf[:start+n]
		more := false
		if err == nil {
			if _, peekErr := reader.Peek(1); peekErr == nil {
				more = true
			} else if peekErr != io.EOF {
				return peekErr
			}
		}
		if !more {
			return s.WriteDAT(payload)
		}
		if err := s.SendRaw(protocol.FlagDAT|protocol.FlagMORE, payload); err != nil {
			return err
		}
		first = false
	}
}

func (h *WSHandler) remove(id uint32, want *wsStream) {
	h.mu.Lock()
	if h.streams[id] == want {
		delete(h.streams, id)
	}
	h.mu.Unlock()
}

func (ws *wsStream) setConn(conn *websocket.Conn) bool {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	if ws.ctx.Err() != nil {
		return false
	}
	ws.conn = conn
	return true
}

func (ws *wsStream) writeFragment(payload []byte, more bool) error {
	ws.writeMu.Lock()
	defer ws.writeMu.Unlock()

	ws.mu.Lock()
	conn := ws.conn
	ws.mu.Unlock()
	if conn == nil {
		return fmt.Errorf("websocket is not connected")
	}

	data := payload
	if ws.writer == nil {
		if len(payload) < 1 {
			return fmt.Errorf("websocket fragment is missing its message type")
		}
		msgType := websocket.TextMessage
		switch payload[0] {
		case 0:
		case 1:
			msgType = websocket.BinaryMessage
		default:
			return fmt.Errorf("invalid websocket message type %d", payload[0])
		}
		writer, err := conn.NextWriter(msgType)
		if err != nil {
			return err
		}
		ws.writer = writer
		data = payload[1:]
	}
	if err := bytestream.WriteFull(ws.writer, data); err != nil {
		return err
	}
	if !more {
		err := ws.writer.Close()
		ws.writer = nil
		return err
	}
	return nil
}

func (ws *wsStream) close() {
	ws.cancel()
	ws.mu.Lock()
	conn := ws.conn
	ws.mu.Unlock()
	if conn != nil {
		_ = conn.Close()
	}
	ws.writeMu.Lock()
	if ws.writer != nil {
		_ = ws.writer.Close()
		ws.writer = nil
	}
	ws.writeMu.Unlock()
}
