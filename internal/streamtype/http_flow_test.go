package streamtype

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/richlegrand/bitbang/internal/protocol"
)

type httpRecordingStream struct {
	mu     sync.Mutex
	frames []protocol.Frame
}

func (*httpRecordingStream) ID() uint32          { return 1 }
func (*httpRecordingStream) ConnectPath() string { return "/" }
func (s *httpRecordingStream) WriteSYN(payload []byte) error {
	return s.record(protocol.FlagSYN, payload)
}
func (s *httpRecordingStream) WriteDAT(payload []byte) error {
	return s.record(protocol.FlagDAT, payload)
}
func (s *httpRecordingStream) WriteFIN(payload []byte) error {
	return s.record(protocol.FlagFIN, payload)
}
func (s *httpRecordingStream) SendRaw(flags uint16, payload []byte) error {
	return s.record(flags, payload)
}
func (*httpRecordingStream) BufferedAmount() uint64 { return 0 }

func (s *httpRecordingStream) record(flags uint16, payload []byte) error {
	if err := protocol.ValidateFramePayload(payload); err != nil {
		return err
	}
	s.mu.Lock()
	s.frames = append(s.frames, protocol.Frame{
		StreamID: 1,
		Flags:    flags,
		Payload:  append([]byte(nil), payload...),
	})
	s.mu.Unlock()
	return nil
}

func (s *httpRecordingStream) snapshot() []protocol.Frame {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]protocol.Frame(nil), s.frames...)
}

func assertHeaderLimitResponse(t *testing.T, frames []protocol.Frame) {
	t.Helper()
	if len(frames) != 3 {
		t.Fatalf("response frame count = %d, want 3: %#v", len(frames), frames)
	}
	var meta struct {
		Status int `json:"status"`
	}
	if !frames[0].IsSYN() || json.Unmarshal(frames[0].Payload, &meta) != nil || meta.Status != 502 {
		t.Fatalf("response SYN = %#v, metadata = %#v; want status 502", frames[0], meta)
	}
	if got := string(frames[1].Payload); got != "Response headers are too large" {
		t.Fatalf("response body = %q", got)
	}
	if !frames[2].IsFIN() {
		t.Fatalf("terminal frame flags = %#x, want FIN", frames[2].Flags)
	}
}

func TestHTTPProxyRejectsOversizedResponseHeadersCoherently(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Oversized", strings.Repeat("x", protocol.MaxChunkSize))
		_, _ = w.Write([]byte("must not be forwarded"))
	}))
	defer target.Close()

	host := strings.TrimPrefix(target.URL, "http://")
	h := &HTTPHandler{
		Target:     host,
		connTarget: host,
		Server:     "bitba.ng",
		streams:    make(map[uint32]*pendingStream),
	}
	stream := &httpRecordingStream{}
	h.proxyRequest(stream, protocol.Request{Method: http.MethodGet, Pathname: "/"}, nil)

	assertHeaderLimitResponse(t, stream.snapshot())
}

func TestHTTPLocalRejectsOversizedResponseHeadersBeforeBody(t *testing.T) {
	stream := &httpRecordingStream{}
	w := &swspResponseWriter{
		stream:  stream,
		status:  http.StatusOK,
		headers: http.Header{"X-Oversized": []string{strings.Repeat("x", protocol.MaxChunkSize)}},
		done:    make(chan struct{}),
	}

	if n, err := w.Write([]byte("must not be forwarded")); err == nil || n != 0 {
		t.Fatalf("oversized-header Write = %d, %v; want 0, error", n, err)
	}
	w.Close()
	assertHeaderLimitResponse(t, stream.snapshot())
}
