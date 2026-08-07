package streamtype

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/richlegrand/bitbang/internal/protocol"
)

type wsTestStream struct {
	id uint32

	mu     sync.Mutex
	frames []protocol.Frame
}

func (s *wsTestStream) ID() uint32          { return s.id }
func (s *wsTestStream) ConnectPath() string { return "/" }
func (s *wsTestStream) WriteSYN(payload []byte) error {
	return s.capture(protocol.FlagSYN, payload)
}
func (s *wsTestStream) WriteDAT(payload []byte) error {
	return s.capture(protocol.FlagDAT, payload)
}
func (s *wsTestStream) WriteFIN(payload []byte) error {
	return s.capture(protocol.FlagFIN, payload)
}
func (s *wsTestStream) SendRaw(flags uint16, payload []byte) error {
	return s.capture(flags, payload)
}
func (s *wsTestStream) BufferedAmount() uint64 { return 0 }

func (s *wsTestStream) capture(flags uint16, payload []byte) error {
	s.mu.Lock()
	s.frames = append(s.frames, protocol.Frame{
		StreamID: s.id,
		Flags:    flags,
		Payload:  append([]byte(nil), payload...),
	})
	s.mu.Unlock()
	return nil
}

func (s *wsTestStream) snapshot() []protocol.Frame {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]protocol.Frame(nil), s.frames...)
}

func TestWriteWSReaderStreamsLargeMessageInOrder(t *testing.T) {
	want := make([]byte, 1<<20+137)
	for i := range want {
		want[i] = byte(i * 31)
	}
	s := &wsTestStream{id: 11}
	if err := writeWSReader(s, websocket.BinaryMessage, bytes.NewReader(want)); err != nil {
		t.Fatalf("writeWSReader: %v", err)
	}

	frames := s.snapshot()
	if len(frames) < 2 {
		t.Fatalf("frame count = %d, want a fragmented message", len(frames))
	}
	var got bytes.Buffer
	for i, frame := range frames {
		if len(frame.Payload) > protocol.MaxChunkSize {
			t.Fatalf("frame %d payload = %d bytes, max %d", i, len(frame.Payload), protocol.MaxChunkSize)
		}
		if i < len(frames)-1 && !frame.IsMORE() {
			t.Fatalf("frame %d flags = %#x, want MORE", i, frame.Flags)
		}
		if i == len(frames)-1 && frame.IsMORE() {
			t.Fatalf("final frame flags = %#x, want DAT", frame.Flags)
		}
		payload := frame.Payload
		if i == 0 {
			if len(payload) == 0 || payload[0] != 1 {
				t.Fatalf("first payload = %v, want binary type byte", payload)
			}
			payload = payload[1:]
		}
		got.Write(payload)
	}
	if !bytes.Equal(got.Bytes(), want) {
		t.Fatalf("reassembled %d bytes with matching=%v, want %d", got.Len(), bytes.Equal(got.Bytes(), want), len(want))
	}
}

type fixedWSResolver struct {
	target string
}

func (r fixedWSResolver) ResolveTarget(string) (string, string) {
	return r.target, "/ws"
}

func TestWSHandlerForwardsLargeFragmentedMessage(t *testing.T) {
	received := make(chan []byte, 1)
	readErr := make(chan error, 1)
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			readErr <- err
			return
		}
		defer conn.Close()
		msgType, reader, err := conn.NextReader()
		if err != nil {
			readErr <- err
			return
		}
		body, err := io.ReadAll(reader)
		if err != nil {
			readErr <- err
			return
		}
		received <- append([]byte{byte(msgType)}, body...)
	}))
	defer server.Close()

	h := NewWebSocket(fixedWSResolver{target: strings.TrimPrefix(server.URL, "http://")}, "", false)
	s := newTCPTestStreamID(13)
	open, _ := json.Marshal(map[string]string{"pathname": "/ws"})
	if err := h.OnSYN(s, open, false); err != nil {
		t.Fatalf("OnSYN: %v", err)
	}
	if ack := nextTCPFrame(t, s); !ack.IsSYN() || ack.IsFIN() {
		t.Fatalf("ack flags = %#x, want SYN", ack.Flags)
	}

	want := make([]byte, 1<<20+333)
	for i := range want {
		want[i] = byte(i * 17)
	}
	firstLen := protocol.MaxChunkSize - 1
	first := make([]byte, firstLen+1)
	first[0] = 1
	copy(first[1:], want[:firstLen])
	if err := h.OnFragment(s, first, true); err != nil {
		t.Fatalf("first fragment: %v", err)
	}
	for off := firstLen; off < len(want); {
		end := off + protocol.MaxChunkSize
		if end > len(want) {
			end = len(want)
		}
		if err := h.OnFragment(s, want[off:end], end < len(want)); err != nil {
			t.Fatalf("fragment at %d: %v", off, err)
		}
		off = end
	}

	select {
	case got := <-received:
		if got[0] != byte(websocket.BinaryMessage) || !bytes.Equal(got[1:], want) {
			t.Fatalf("upstream got type=%d bytes=%d matching=%v", got[0], len(got)-1, bytes.Equal(got[1:], want))
		}
	case err := <-readErr:
		t.Fatalf("upstream read: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for upstream WebSocket message")
	}

	_ = h.OnFIN(s, nil)
}

var _ FragmentHandler = (*WSHandler)(nil)
var _ ResetHandler = (*WSHandler)(nil)
