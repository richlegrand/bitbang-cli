package streamtype

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sync"

	"github.com/gorilla/websocket"
	"github.com/richlegrand/bitbang/internal/protocol"
)

// WSHandler implements StreamHandler for type="websocket". Bridges a SWSP
// WebSocket-on-stream-N to a real ws:// connection to a local server.
//
// Resolves the target the same way HTTPHandler does — via the HTTPHandler
// it's paired with. (Both are per-session, share session state.)
type WSHandler struct {
	// Resolver supplies the current target + path-rewriting logic. In
	// proxy mode it's the paired HTTPHandler; other modes can substitute.
	Resolver TargetResolver
	Verbose  bool

	mu      sync.Mutex
	streams map[uint32]*wsStream
}

// TargetResolver maps a SWSP request path to a (target host, ws path) pair.
type TargetResolver interface {
	ResolveTarget(requestPath string) (target, path string)
}

// NewWebSocket constructs a WSHandler. resolver is typically the paired
// HTTPHandler so that WS streams use the same dynamic-target logic.
func NewWebSocket(resolver TargetResolver, verbose bool) *WSHandler {
	return &WSHandler{
		Resolver: resolver,
		Verbose:  verbose,
		streams:  make(map[uint32]*wsStream),
	}
}

type wsStream struct {
	conn *websocket.Conn
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
	go h.bridge(s, msg.Pathname, msg.Cookies)
	return nil
}

// OnDAT forwards a message from the browser to the local WS server.
func (h *WSHandler) OnDAT(s Stream, payload []byte) error {
	h.mu.Lock()
	ws := h.streams[s.ID()]
	h.mu.Unlock()
	if ws == nil || len(payload) < 1 {
		return nil
	}
	typeByte := payload[0]
	data := payload[1:]
	msgType := websocket.TextMessage
	if typeByte == 1 {
		msgType = websocket.BinaryMessage
	}
	if err := ws.conn.WriteMessage(msgType, data); err != nil {
		log.Printf("WS write failed (stream %d): %v", s.ID(), err)
		ws.conn.Close()
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
		ws.conn.Close()
	}
	return nil
}

func (h *WSHandler) bridge(s Stream, pathname, cookies string) {
	target, wsPath := h.Resolver.ResolveTarget(pathname)
	wsURL := fmt.Sprintf("ws://%s%s", target, wsPath)
	header := http.Header{}
	if cookies != "" {
		header.Set("Cookie", cookies)
	}
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, header)
	if err != nil {
		log.Printf("WS connect failed: %s -> %v", pathname, err)
		_ = s.WriteFIN(nil)
		return
	}
	log.Printf("WS opened: %s (stream %d)", pathname, s.ID())

	h.mu.Lock()
	h.streams[s.ID()] = &wsStream{conn: conn}
	h.mu.Unlock()

	_ = s.WriteSYN(nil)

	defer func() {
		conn.Close()
		h.mu.Lock()
		delete(h.streams, s.ID())
		h.mu.Unlock()
		_ = s.WriteFIN(nil)
		log.Printf("WS closed: %s (stream %d)", pathname, s.ID())
	}()

	for {
		msgType, data, err := conn.ReadMessage()
		if err != nil {
			return
		}
		var typeByte byte
		if msgType == websocket.TextMessage {
			typeByte = 0
		} else {
			typeByte = 1
		}
		buf := make([]byte, 1+len(data))
		buf[0] = typeByte
		copy(buf[1:], data)
		// The data channel caps messages at MaxChunkSize and the SWSP
		// length field is 16-bit, so a large WS message must be split
		// across frames. Non-final chunks carry FlagMORE; the receiver
		// reassembles them into one WS message (the type byte rides in
		// the first chunk).
		if err := writeWSChunks(s, buf); err != nil {
			return
		}
	}
}

// writeWSChunks sends one WebSocket message as one or more SWSP DAT frames,
// each at most MaxChunkSize bytes. Every chunk but the last carries FlagMORE
// so the receiver reassembles them back into a single WS message.
func writeWSChunks(s Stream, buf []byte) error {
	for off := 0; off < len(buf); off += protocol.MaxChunkSize {
		end := off + protocol.MaxChunkSize
		if end >= len(buf) {
			return s.WriteDAT(buf[off:]) // final chunk: FlagDAT, no MORE
		}
		if err := s.SendRaw(protocol.FlagDAT|protocol.FlagMORE, buf[off:end]); err != nil {
			return err
		}
	}
	return s.WriteDAT(buf) // only reached for an empty message
}
