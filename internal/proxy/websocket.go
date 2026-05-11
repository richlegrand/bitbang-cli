package proxy

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"

	"github.com/gorilla/websocket"
	"github.com/richlegrand/bitbang/bitbangproxy/internal/protocol"
)

// wsStream tracks an active WebSocket connection to a local server.
type wsStream struct {
	conn     *websocket.Conn
	streamID uint32
}

// handleWSOpen processes a SYN frame with type "websocket".
// Opens a WebSocket to the local server and bridges messages bidirectionally.
func (h *Handler) handleWSOpen(frame protocol.Frame) {
	var msg struct {
		Type     string `json:"type"`
		Pathname string `json:"pathname"`
		Cookies  string `json:"cookies"`
	}
	if err := json.Unmarshal(frame.Payload, &msg); err != nil {
		log.Printf("Failed to parse WS open: %v", err)
		return
	}

	go h.bridgeWebSocket(frame.StreamID, msg.Pathname, msg.Cookies)
}

func (h *Handler) bridgeWebSocket(streamID uint32, pathname string, cookies string) {
	// Resolve target and path (same logic as HTTP)
	target, wsPath := h.resolveTarget(pathname)
	// Connect to local WebSocket server with cookies for session auth
	wsURL := fmt.Sprintf("ws://%s%s", target, wsPath)
	header := http.Header{}
	if cookies != "" {
		header.Set("Cookie", cookies)
	}
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, header)
	if err != nil {
		log.Printf("WS connect failed: %s -> %v", pathname, err)
		// Send FIN to close the browser-side WebSocket
		h.sendFrame(streamID, protocol.FlagFIN, nil)
		return
	}

	log.Printf("WS opened: %s (stream %d)", pathname, streamID)

	// Track this WebSocket stream
	h.mu.Lock()
	if h.wsConns == nil {
		h.wsConns = make(map[uint32]*wsStream)
	}
	h.wsConns[streamID] = &wsStream{conn: conn, streamID: streamID}
	h.mu.Unlock()

	// Send SYN acknowledgement to browser
	h.sendFrame(streamID, protocol.FlagSYN, nil)

	// Read from local WS server, send to browser via SWSP DAT frames
	defer func() {
		conn.Close()
		h.mu.Lock()
		delete(h.wsConns, streamID)
		h.mu.Unlock()
		// Send FIN to browser
		h.sendFrame(streamID, protocol.FlagFIN, nil)
		log.Printf("WS closed: %s (stream %d)", pathname, streamID)
	}()

	for {
		msgType, data, err := conn.ReadMessage()
		if err != nil {
			return // connection closed or error
		}

		// DAT frame: type byte (0=text, 1=binary) + message
		var typeByte byte
		if msgType == websocket.TextMessage {
			typeByte = 0
		} else {
			typeByte = 1
		}

		payload := make([]byte, 1+len(data))
		payload[0] = typeByte
		copy(payload[1:], data)

		if err := h.sendFrame(streamID, protocol.FlagDAT, payload); err != nil {
			return
		}
	}
}

// handleWSMessage forwards a DAT frame from the browser to the local WS server.
func (h *Handler) handleWSMessage(frame protocol.Frame) {
	h.mu.Lock()
	ws := h.wsConns[frame.StreamID]
	h.mu.Unlock()

	if ws == nil {
		return
	}

	if frame.IsFIN() {
		// Browser closed the WebSocket
		ws.conn.Close()
		return
	}

	if len(frame.Payload) < 1 {
		return
	}

	// Parse type byte + message
	typeByte := frame.Payload[0]
	data := frame.Payload[1:]

	var msgType int
	if typeByte == 0 {
		msgType = websocket.TextMessage
	} else {
		msgType = websocket.BinaryMessage
	}

	if err := ws.conn.WriteMessage(msgType, data); err != nil {
		log.Printf("WS write failed (stream %d): %v", frame.StreamID, err)
		ws.conn.Close()
	}
}
