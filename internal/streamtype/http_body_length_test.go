package streamtype

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/richlegrand/bitbang-cli/internal/protocol"
)

// A browser's service worker cannot read Content-Length, so a POST from a
// proxied page arrives with contentLength 0 and a streaming body. Sending
// that on as Transfer-Encoding: chunked loses the body for every target that
// doesn't decode chunked requests (Python's http.server, many embedded
// servers), so a body that fits in the sniff buffer must be measured here and
// forwarded with a real Content-Length.

type receivedRequest struct {
	mu               sync.Mutex
	body             string
	contentLength    int64
	transferEncoding []string
}

func echoTarget(t *testing.T) (*httptest.Server, *receivedRequest) {
	t.Helper()
	got := &receivedRequest{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		got.mu.Lock()
		got.body, got.contentLength, got.transferEncoding = string(body), r.ContentLength, r.TransferEncoding
		got.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)
	return srv, got
}

func postThrough(t *testing.T, srv *httptest.Server, req protocol.Request, body io.Reader) {
	t.Helper()
	host := strings.TrimPrefix(srv.URL, "http://")
	h := &HTTPHandler{
		Target:     host,
		connTarget: host,
		Server:     "bitba.ng",
		streams:    make(map[uint32]*pendingStream),
	}
	req.Method = http.MethodPost
	req.Pathname = "/"
	h.proxyRequest(&httpRecordingStream{}, req, body)
}

func TestHTTPPostWithoutLengthGetsContentLength(t *testing.T) {
	srv, got := echoTarget(t)
	payload := `{"aid":"18.1@1789606800"}`
	postThrough(t, srv, protocol.Request{ContentLength: 0}, strings.NewReader(payload))

	got.mu.Lock()
	defer got.mu.Unlock()
	if got.body != payload {
		t.Errorf("target received body %q, want %q", got.body, payload)
	}
	if got.contentLength != int64(len(payload)) {
		t.Errorf("Content-Length = %d, want %d", got.contentLength, len(payload))
	}
	if len(got.transferEncoding) != 0 {
		t.Errorf("Transfer-Encoding = %v, want none", got.transferEncoding)
	}
}

func TestHTTPEmptyPostWithoutLengthGetsZeroContentLength(t *testing.T) {
	srv, got := echoTarget(t)
	postThrough(t, srv, protocol.Request{ContentLength: 0}, strings.NewReader(""))

	got.mu.Lock()
	defer got.mu.Unlock()
	if got.body != "" {
		t.Errorf("target received body %q, want empty", got.body)
	}
	if got.contentLength != 0 {
		t.Errorf("Content-Length = %d, want 0", got.contentLength)
	}
	if len(got.transferEncoding) != 0 {
		t.Errorf("Transfer-Encoding = %v, want none", got.transferEncoding)
	}
}

// Beyond the sniff buffer the body streams, which is what keeps a big upload
// from being held in memory. Chunked is correct there; the body must still
// arrive whole.
func TestHTTPLargePostStreamsChunked(t *testing.T) {
	srv, got := echoTarget(t)
	payload := strings.Repeat("x", sniffBodyBytes+1024)
	postThrough(t, srv, protocol.Request{ContentLength: 0}, strings.NewReader(payload))

	got.mu.Lock()
	defer got.mu.Unlock()
	if got.body != payload {
		t.Errorf("target received %d body bytes, want %d", len(got.body), len(payload))
	}
	if len(got.transferEncoding) == 0 || got.transferEncoding[0] != "chunked" {
		t.Errorf("Transfer-Encoding = %v, want chunked", got.transferEncoding)
	}
}

// A sender that does know the length (the buffered service-worker path, an
// upload that declares x-file-size) is taken at its word, with no sniffing.
func TestHTTPPostWithDeclaredLengthIsPreserved(t *testing.T) {
	srv, got := echoTarget(t)
	payload := strings.Repeat("y", 2048)
	postThrough(t, srv, protocol.Request{ContentLength: len(payload)}, strings.NewReader(payload))

	got.mu.Lock()
	defer got.mu.Unlock()
	if got.body != payload {
		t.Errorf("target received %d body bytes, want %d", len(got.body), len(payload))
	}
	if got.contentLength != int64(len(payload)) {
		t.Errorf("Content-Length = %d, want %d", got.contentLength, len(payload))
	}
	if len(got.transferEncoding) != 0 {
		t.Errorf("Transfer-Encoding = %v, want none", got.transferEncoding)
	}
}
