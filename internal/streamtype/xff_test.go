package streamtype

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/richlegrand/bitbang/internal/protocol"
)

// captureStream is a no-op Stream — proxyRequest writes the response back
// through it, but these tests only care about what the upstream target
// received, so the writes are discarded.
type captureStream struct{}

func (captureStream) ID() uint32                       { return 1 }
func (captureStream) ConnectPath() string              { return "/" }
func (captureStream) WriteSYN(_ []byte) error          { return nil }
func (captureStream) WriteDAT(_ []byte) error          { return nil }
func (captureStream) WriteFIN(_ []byte) error          { return nil }
func (captureStream) SendRaw(_ uint16, _ []byte) error { return nil }
func (captureStream) BufferedAmount() uint64           { return 0 }

// newXFFTarget spins up an HTTP server that records the X-Forwarded-For
// header it receives, and returns a fixed-target HTTPHandler pointed at it.
func newXFFTarget(t *testing.T, browserIP string) (*HTTPHandler, *string, *sync.Mutex) {
	t.Helper()
	var mu sync.Mutex
	var gotXFF string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotXFF = r.Header.Get("X-Forwarded-For")
		mu.Unlock()
		w.WriteHeader(200)
	}))
	t.Cleanup(srv.Close)

	host := strings.TrimPrefix(srv.URL, "http://")
	h := &HTTPHandler{
		Target:     host,
		connTarget: host,
		BrowserIP:  browserIP,
		Server:     "bitba.ng",
		streams:    make(map[uint32]*pendingStream),
	}
	return h, &gotXFF, &mu
}

// TestXFF_StampsBrowserIP confirms fixed-target mode stamps the real
// browser IP as X-Forwarded-For so the backend sees the true origin.
func TestXFF_StampsBrowserIP(t *testing.T) {
	h, got, mu := newXFFTarget(t, "203.0.113.7")
	h.proxyRequest(captureStream{}, protocol.Request{Method: "GET", Pathname: "/"}, nil)
	mu.Lock()
	defer mu.Unlock()
	if *got != "203.0.113.7" {
		t.Errorf("X-Forwarded-For = %q, want %q", *got, "203.0.113.7")
	}
}

// TestXFF_StripsClientSpoof confirms a client-supplied X-Forwarded-For is
// dropped and replaced with the server-known IP — otherwise an attacker
// could forge 127.0.0.1 to re-trigger OctoPrint's autologinLocal.
func TestXFF_StripsClientSpoof(t *testing.T) {
	h, got, mu := newXFFTarget(t, "203.0.113.7")
	req := protocol.Request{
		Method:   "GET",
		Pathname: "/",
		Headers:  map[string]string{"X-Forwarded-For": "127.0.0.1", "X-Real-IP": "127.0.0.1"},
	}
	h.proxyRequest(captureStream{}, req, nil)
	mu.Lock()
	defer mu.Unlock()
	if *got != "203.0.113.7" {
		t.Errorf("spoofed XFF survived: got %q, want %q", *got, "203.0.113.7")
	}
}

// TestXFF_DynamicModeOmits confirms that with no BrowserIP (dynamic-target
// mode), no XFF is injected — and a client spoof is still stripped, never
// forwarded.
func TestXFF_DynamicModeOmits(t *testing.T) {
	h, got, mu := newXFFTarget(t, "") // dynamic mode: serve.go withholds browser_ip
	req := protocol.Request{
		Method:   "GET",
		Pathname: "/",
		Headers:  map[string]string{"X-Forwarded-For": "10.1.2.3"},
	}
	h.proxyRequest(captureStream{}, req, nil)
	mu.Lock()
	defer mu.Unlock()
	if *got != "" {
		t.Errorf("X-Forwarded-For = %q, want empty (dynamic mode, spoof stripped)", *got)
	}
}

// TestXFF_MalformedIPOmitted confirms a non-IP browser value is never
// emitted as XFF (a strict backend could reject a malformed header).
func TestXFF_MalformedIPOmitted(t *testing.T) {
	h, got, mu := newXFFTarget(t, "not-an-ip:8080")
	h.proxyRequest(captureStream{}, protocol.Request{Method: "GET", Pathname: "/"}, nil)
	mu.Lock()
	defer mu.Unlock()
	if *got != "" {
		t.Errorf("X-Forwarded-For = %q, want empty (malformed IP)", *got)
	}
}
