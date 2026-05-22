package streamtype

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/richlegrand/bitbang/internal/protocol"
)

// HTTPLocalHandler implements StreamHandler for type="http" by dispatching
// each request to an in-process http.Handler. Used by the fileshare mode
// (and any future mode that wants a Go http.Handler exposed over SWSP).
//
// Contrast with HTTPHandler which makes outbound HTTP calls — the bridge
// logic is the same (parse Request SYN → run handler → frame response
// back as SYN/DAT/FIN) but the "run handler" step differs.
type HTTPLocalHandler struct {
	Handler http.Handler
	Verbose bool

	mu      sync.Mutex
	streams map[uint32]*localPending
}

// NewHTTPLocal wraps an in-process http.Handler as a SWSP StreamHandler.
func NewHTTPLocal(h http.Handler, verbose bool) *HTTPLocalHandler {
	return &HTTPLocalHandler{
		Handler: h,
		Verbose: verbose,
		streams: make(map[uint32]*localPending),
	}
}

type localPending struct {
	pw *io.PipeWriter
}

func (h *HTTPLocalHandler) Type() string             { return "http" }
func (h *HTTPLocalHandler) OnConnect(_ string) error { return nil }

func (h *HTTPLocalHandler) OnSYN(s Stream, payload []byte, final bool) error {
	req, err := protocol.ParseRequest(payload)
	if err != nil {
		sendStreamError(s, 400, "Bad request")
		return nil
	}
	if final {
		go h.dispatch(s, req, nil)
		return nil
	}
	pr, pw := io.Pipe()
	h.mu.Lock()
	h.streams[s.ID()] = &localPending{pw: pw}
	h.mu.Unlock()
	go h.dispatch(s, req, pr)
	return nil
}

func (h *HTTPLocalHandler) OnDAT(s Stream, payload []byte) error {
	h.mu.Lock()
	ps := h.streams[s.ID()]
	h.mu.Unlock()
	if ps == nil {
		return nil
	}
	if len(payload) > 0 {
		_, _ = ps.pw.Write(payload)
	}
	return nil
}

func (h *HTTPLocalHandler) OnFIN(s Stream, payload []byte) error {
	h.mu.Lock()
	ps := h.streams[s.ID()]
	delete(h.streams, s.ID())
	h.mu.Unlock()
	if ps == nil {
		return nil
	}
	if len(payload) > 0 {
		_, _ = ps.pw.Write(payload)
	}
	_ = ps.pw.Close()
	return nil
}

func (h *HTTPLocalHandler) dispatch(s Stream, req protocol.Request, body io.Reader) {
	if body == nil {
		body = http.NoBody
	}
	httpReq, err := http.NewRequest(req.Method, req.Pathname, body)
	if err != nil {
		sendStreamError(s, 500, err.Error())
		return
	}
	for k, v := range req.Headers {
		httpReq.Header.Set(k, v)
	}
	if req.ContentLength > 0 {
		httpReq.ContentLength = int64(req.ContentLength)
	}

	rw := &swspResponseWriter{stream: s, status: 200, headers: http.Header{}}
	h.Handler.ServeHTTP(rw, httpReq)
	rw.Close()

	if h.Verbose {
		log.Printf("local %s %s -> %d", req.Method, req.Pathname, rw.status)
	}
}

// swspResponseWriter implements http.ResponseWriter by streaming bytes out
// as SWSP frames: status + headers in the SYN, body in DAT frames, FIN at
// the end. This means large file downloads stream incrementally instead
// of being buffered in memory.
type swspResponseWriter struct {
	stream     Stream
	status     int
	headers    http.Header
	headerSent bool
	closed     bool
}

func (w *swspResponseWriter) Header() http.Header { return w.headers }

func (w *swspResponseWriter) WriteHeader(status int) {
	if w.headerSent {
		return
	}
	w.status = status
	w.sendHeader()
}

func (w *swspResponseWriter) Write(p []byte) (int, error) {
	if !w.headerSent {
		w.sendHeader()
	}
	if len(p) == 0 {
		return 0, nil
	}
	// Backpressure: mirror what HTTPHandler does — cap the data channel
	// send buffer at 8 MB so a slow consumer doesn't blow up memory.
	const maxBuffered = 8 << 20
	for w.stream.BufferedAmount() > maxBuffered {
		time.Sleep(1 * time.Millisecond)
	}
	// Chunk the write to fit within SWSP's max frame payload.
	written := 0
	for off := 0; off < len(p); off += protocol.MaxChunkSize {
		end := off + protocol.MaxChunkSize
		if end > len(p) {
			end = len(p)
		}
		if err := w.stream.WriteDAT(p[off:end]); err != nil {
			return written, err
		}
		written += end - off
	}
	return written, nil
}

func (w *swspResponseWriter) sendHeader() {
	w.headerSent = true
	headers := make(map[string]interface{}, len(w.headers))
	for k, v := range w.headers {
		if len(v) > 1 && k == "Set-Cookie" {
			headers[k] = v
		} else if len(v) > 0 {
			headers[k] = v[0]
		}
	}
	meta, _ := json.Marshal(map[string]interface{}{
		"status":  w.status,
		"headers": headers,
	})
	_ = w.stream.WriteSYN(meta)
}

// Close finalizes the response: sends headers if not yet sent, then FIN.
func (w *swspResponseWriter) Close() {
	if !w.headerSent {
		w.sendHeader()
	}
	if w.closed {
		return
	}
	w.closed = true
	_ = w.stream.WriteFIN(nil)
}

// sendStreamError is a shared helper for emitting a single-frame error
// response. Used by both HTTPHandler (proxy mode) and HTTPLocalHandler.
func sendStreamError(s Stream, status int, message string) {
	meta, _ := json.Marshal(map[string]interface{}{
		"status":  status,
		"headers": map[string]string{"Content-Type": "text/plain"},
	})
	_ = s.WriteSYN(meta)
	if message != "" {
		_ = s.WriteDAT([]byte(message))
	}
	_ = s.WriteFIN(nil)
}
