package streamtype

import (
	"testing"
)

// Tests use RFC 5737 documentation IPs (192.0.2.x) and RFC 2606 reserved
// hostnames (*.example) so they never collide with real network targets.

// -- parseTargetFromPath tests -----------------------------------------------

func TestParseTargetHostPort(t *testing.T) {
	target, path := parseTargetFromPath("/nas.example:8080/admin")
	if target != "nas.example:8080" {
		t.Errorf("target = %q, want %q", target, "nas.example:8080")
	}
	if path != "/admin" {
		t.Errorf("path = %q, want %q", path, "/admin")
	}
}

func TestParseTargetDottedIP(t *testing.T) {
	target, path := parseTargetFromPath("/192.0.2.10/path")
	if target != "192.0.2.10" {
		t.Errorf("target = %q, want %q", target, "192.0.2.10")
	}
	if path != "/path" {
		t.Errorf("path = %q, want %q", path, "/path")
	}
}

func TestParseTargetIPWithPort(t *testing.T) {
	target, path := parseTargetFromPath("/192.0.2.1:3000/api/data")
	if target != "192.0.2.1:3000" {
		t.Errorf("target = %q, want %q", target, "192.0.2.1:3000")
	}
	if path != "/api/data" {
		t.Errorf("path = %q, want %q", path, "/api/data")
	}
}

func TestParseTargetLocalhost(t *testing.T) {
	target, path := parseTargetFromPath("/localhost/path")
	if target != "localhost" {
		t.Errorf("target = %q, want %q", target, "localhost")
	}
	if path != "/path" {
		t.Errorf("path = %q, want %q", path, "/path")
	}
}

func TestParseTargetNoTarget(t *testing.T) {
	target, path := parseTargetFromPath("/just-a-path")
	if target != "" {
		t.Errorf("target = %q, want empty", target)
	}
	if path != "/just-a-path" {
		t.Errorf("path = %q, want %q", path, "/just-a-path")
	}
}

func TestParseTargetBareHostPort(t *testing.T) {
	target, path := parseTargetFromPath("/nas.example:8080")
	if target != "nas.example:8080" {
		t.Errorf("target = %q, want %q", target, "nas.example:8080")
	}
	if path != "/" {
		t.Errorf("path = %q, want %q", path, "/")
	}
}

func TestParseTargetEmpty(t *testing.T) {
	target, path := parseTargetFromPath("/")
	if target != "" {
		t.Errorf("target = %q, want empty", target)
	}
	if path != "/" {
		t.Errorf("path = %q, want %q", path, "/")
	}
}

func TestParseTargetDottedHostname(t *testing.T) {
	target, path := parseTargetFromPath("/nas.example/admin")
	if target != "nas.example" {
		t.Errorf("target = %q, want %q", target, "nas.example")
	}
	if path != "/admin" {
		t.Errorf("path = %q, want %q", path, "/admin")
	}
}

func TestParseTargetStripHTTP(t *testing.T) {
	target, path := parseTargetFromPath("/http://192.0.2.11/main.html")
	if target != "192.0.2.11" {
		t.Errorf("target = %q, want %q", target, "192.0.2.11")
	}
	if path != "/main.html" {
		t.Errorf("path = %q, want %q", path, "/main.html")
	}
}

func TestParseTargetStripHTTPS(t *testing.T) {
	target, path := parseTargetFromPath("/https://nas.example:8080/admin")
	if target != "nas.example:8080" {
		t.Errorf("target = %q, want %q", target, "nas.example:8080")
	}
	if path != "/admin" {
		t.Errorf("path = %q, want %q", path, "/admin")
	}
}

func TestParseTargetStripHTTPBare(t *testing.T) {
	target, path := parseTargetFromPath("/http://192.0.2.11")
	if target != "192.0.2.11" {
		t.Errorf("target = %q, want %q", target, "192.0.2.11")
	}
	if path != "/" {
		t.Errorf("path = %q, want %q", path, "/")
	}
}

// -- resolveTarget tests -----------------------------------------------------

func newHandler(target, connTarget, targetPrefix, connectPrefix string) *HTTPHandler {
	return &HTTPHandler{
		Target:        target,
		connTarget:    connTarget,
		targetPrefix:  targetPrefix,
		connectPrefix: connectPrefix,
	}
}

func TestResolveTargetFixedTarget(t *testing.T) {
	h := newHandler("localhost:8080", "localhost:8080", "", "")
	target, path := h.resolveTarget("/api/data")
	if target != "localhost:8080" {
		t.Errorf("target = %q, want %q", target, "localhost:8080")
	}
	if path != "/api/data" {
		t.Errorf("path = %q, want %q", path, "/api/data")
	}
}

func TestResolveTargetStripPrefix(t *testing.T) {
	h := newHandler("", "192.0.2.1", "/192.0.2.1", "/192.0.2.1")
	target, path := h.resolveTarget("/192.0.2.1/webpages/login.html")
	if target != "192.0.2.1" {
		t.Errorf("target = %q, want %q", target, "192.0.2.1")
	}
	if path != "/webpages/login.html" {
		t.Errorf("path = %q, want %q", path, "/webpages/login.html")
	}
}

func TestResolveTargetEmptyRemainder(t *testing.T) {
	h := newHandler("", "192.0.2.1", "/192.0.2.1", "/192.0.2.1")
	target, path := h.resolveTarget("/192.0.2.1")
	if target != "192.0.2.1" {
		t.Errorf("target = %q, want %q", target, "192.0.2.1")
	}
	if path != "/" {
		t.Errorf("path = %q, want %q", path, "/")
	}
}

func TestResolveTargetCrossHostRedirect(t *testing.T) {
	h := newHandler("", "nas.example:5000", "/nas.example:5000", "/nas.example")
	target, path := h.resolveTarget("/nas.example:5000/admin")
	if target != "nas.example:5000" {
		t.Errorf("target = %q, want %q", target, "nas.example:5000")
	}
	if path != "/admin" {
		t.Errorf("path = %q, want %q", path, "/admin")
	}
}

func TestResolveTargetCrossHostRedirectReparse(t *testing.T) {
	h := newHandler("", "nas.example", "/nas.example", "/nas.example")
	target, _ := h.resolveTarget("/nas.example:5000/admin")
	if target == "" {
		t.Error("target should not be empty")
	}
}

func TestResolveTargetConnectPrefixFallback(t *testing.T) {
	h := newHandler("", "nas.example:5000", "/nas.example:5000", "/nas.example")
	target, path := h.resolveTarget("/nas.example/admin")
	if target != "nas.example:5000" {
		t.Errorf("target = %q, want %q", target, "nas.example:5000")
	}
	if path != "/admin" {
		t.Errorf("path = %q, want %q", path, "/admin")
	}
}

func TestResolveTargetNoPrefixMatch(t *testing.T) {
	h := newHandler("", "192.0.2.1", "/192.0.2.1", "/192.0.2.1")
	target, path := h.resolveTarget("/webpages/css/widget.css")
	if target != "192.0.2.1" {
		t.Errorf("target = %q, want %q", target, "192.0.2.1")
	}
	if path != "/webpages/css/widget.css" {
		t.Errorf("path = %q, want %q", path, "/webpages/css/widget.css")
	}
}

func TestResolveTargetQueryString(t *testing.T) {
	h := newHandler("", "192.0.2.1", "/192.0.2.1", "/192.0.2.1")
	target, path := h.resolveTarget("/cgi-bin/luci/;stok=/locale?form=lang")
	if target != "192.0.2.1" {
		t.Errorf("target = %q, want %q", target, "192.0.2.1")
	}
	if path != "/cgi-bin/luci/;stok=/locale?form=lang" {
		t.Errorf("path = %q, want %q", path, "/cgi-bin/luci/;stok=/locale?form=lang")
	}
}
