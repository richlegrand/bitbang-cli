package streamtype

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	neturl "net/url"
	"strings"
	"sync"
	"time"

	"github.com/richlegrand/bitbang/internal/protocol"
)

// HTTPHandler implements StreamHandler for type="http". It dispatches each
// SWSP HTTP request to a backing target. For the proxy mode, the target is
// a local HTTP server (resolved per-session from the connect path or from
// a static --target flag).
//
// One instance per Session. Owns its own per-stream body-pipe state.
type HTTPHandler struct {
	// Target is the fixed local target (e.g. "localhost:8080") set via the
	// --target flag. If empty, the target is extracted from the connect path.
	Target string
	// UID is the device UID, used for the landing page.
	UID string
	// Server is the signaling server hostname (e.g. "bitba.ng"), used for
	// X-Forwarded-Host on proxied requests.
	Server string
	// BrowserIP is the real public IP of the connecting browser, supplied
	// by the signaling server (never client-settable). Stamped onto every
	// proxied request as X-Forwarded-For so the backend sees the true
	// origin instead of our localhost socket peer. Critical for OctoPrint:
	// without it, requests appear to come from 127.0.0.1, which sits in
	// OctoPrint's trusted localNetworks and (for users who enabled
	// autologinLocal) silently auto-authenticates a remote attacker.
	// Empty when the signaling server didn't provide it (older server,
	// local tests) — in which case we strip XFF rather than forge one.
	BrowserIP string
	Verbose   bool

	// Per-session state, set in OnConnect.
	connTarget    string
	targetPrefix  string
	connectPrefix string

	// Per-stream state.
	mu      sync.Mutex
	streams map[uint32]*pendingStream
}

// NewHTTPProxy returns an HTTPHandler configured for HTTP-proxy mode.
// In dynamic mode, target is empty and the destination is extracted from
// the connect-path URL on each session.
func NewHTTPProxy(target, uid, server, browserIP string, verbose bool) *HTTPHandler {
	return &HTTPHandler{
		Target:    target,
		UID:       uid,
		Server:    server,
		BrowserIP: browserIP,
		Verbose:   verbose,
		streams:   make(map[uint32]*pendingStream),
	}
}

type pendingStream struct {
	req protocol.Request
	pw  *io.PipeWriter
}

// Type implements StreamHandler.
func (h *HTTPHandler) Type() string { return "http" }

// OnConnect runs once per session, after the connect message arrives.
// Resolves the proxy target (fixed --target wins, otherwise parsed from
// the connect path). For dynamic targets, performs an HTTPS probe to
// detect targets that require HTTPS (which we don't support yet).
func (h *HTTPHandler) OnConnect(path string) error {
	if h.Target != "" {
		// Fixed --target: path is passed through to requests as-is.
		h.connTarget = h.Target
		h.targetPrefix = ""
		if h.Verbose {
			log.Printf("Connect: target=%s path=%s", h.connTarget, path)
		}
	} else {
		// Dynamic: extract target from the path.
		target, _ := parseTargetFromPath(path)
		if target == "" {
			h.connTarget = ""
			h.targetPrefix = ""
			if h.Verbose {
				log.Printf("Connect: no target, serving landing page")
			}
		} else {
			h.connTarget = target
			h.targetPrefix = "/" + target
			h.connectPrefix = "/" + target
			if h.Verbose {
				log.Printf("Connect: target=%s (from URL)", h.connTarget)
			}
		}
	}

	// Probe to resolve cross-host redirects and detect HTTPS-only targets.
	if h.connTarget != "" {
		requiresHTTPS := false
		probeURL := fmt.Sprintf("http://%s/", h.connTarget)
		probeClient := &http.Client{
			CheckRedirect: func(r *http.Request, via []*http.Request) error {
				if r.URL.Scheme == "https" {
					requiresHTTPS = true
				}
				if r.URL.Host != "" && r.URL.Host != h.connTarget {
					h.connTarget = r.URL.Host
					h.targetPrefix = "/" + r.URL.Host
					if h.Verbose {
						log.Printf("Target resolved: %s (from probe)", r.URL.Host)
					}
				}
				return http.ErrUseLastResponse
			},
		}
		probeReq, _ := http.NewRequest("HEAD", probeURL, nil)
		if probeResp, err := probeClient.Do(probeReq); err == nil {
			probeResp.Body.Close()
		}
		if requiresHTTPS {
			return fmt.Errorf("%s requires HTTPS, which is not currently supported", h.connTarget)
		}
	}
	return nil
}

// OnSYN handles the start of a new HTTP stream — parses the request, kicks
// off a goroutine to proxy it to the local target (or serves the landing
// page if no target is set).
//
// final=true (SYN|FIN) means no body: spawn the goroutine with nil body.
// final=false means DAT/FIN body frames will follow: set up a pipe and
// hand the read end to the proxy goroutine.
func (h *HTTPHandler) OnSYN(s Stream, payload []byte, final bool) error {
	req, err := protocol.ParseRequest(payload)
	if err != nil {
		log.Printf("Failed to parse request: %v", err)
		h.sendError(s, 400, "Bad request")
		return nil
	}

	if final {
		go h.proxyRequest(s, req, nil)
		return nil
	}

	pr, pw := io.Pipe()
	h.mu.Lock()
	h.streams[s.ID()] = &pendingStream{req: req, pw: pw}
	h.mu.Unlock()

	go h.proxyRequest(s, req, pr)
	return nil
}

// OnDAT routes body bytes into the in-flight request's pipe.
func (h *HTTPHandler) OnDAT(s Stream, payload []byte) error {
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

// OnFIN closes the body pipe, signaling end-of-body to the in-flight
// HTTP request goroutine.
func (h *HTTPHandler) OnFIN(s Stream, payload []byte) error {
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

func (h *HTTPHandler) proxyRequest(s Stream, req protocol.Request, body io.Reader) {
	if h.connTarget == "" {
		h.serveLandingPage(s, req)
		return
	}

	target, pathname := h.resolveTarget(req.Pathname)
	url := fmt.Sprintf("http://%s%s", target, pathname)

	// Buffer the body so Content-Length is explicit (many embedded
	// servers don't support chunked encoding).
	var reqBody io.Reader
	var reqLen int64
	if body != nil {
		bodyBytes, err := io.ReadAll(body)
		if err != nil {
			log.Printf("Failed to read request body: %v", err)
			h.sendError(s, 500, "Internal error")
			return
		}
		reqBody = bytes.NewReader(bodyBytes)
		reqLen = int64(len(bodyBytes))
	}

	httpReq, err := http.NewRequest(req.Method, url, reqBody)
	if err != nil {
		log.Printf("Failed to create HTTP request: %v", err)
		h.sendError(s, 500, "Internal error")
		return
	}

	skipHeaders := map[string]bool{
		"host": true, "origin": true, "referer": true, "content-length": true,
		// Strip any client-supplied forwarding/origin-spoofing headers. The
		// browser sends an arbitrary header map (see the PoC), so without
		// this an attacker could send X-Forwarded-For: 127.0.0.1 to forge a
		// localhost origin and re-trigger OctoPrint's autologinLocal. We
		// overwrite X-Forwarded-* below with server-known values.
		"x-forwarded-for": true, "x-real-ip": true, "x-forwarded-host": true,
		"x-forwarded-proto": true, "x-forwarded-port": true, "forwarded": true,
	}
	if req.Headers != nil {
		for key, value := range req.Headers {
			if !skipHeaders[strings.ToLower(key)] {
				httpReq.Header.Set(key, value)
			}
		}
	} else {
		if req.ContentType != "" {
			httpReq.Header.Set("Content-Type", req.ContentType)
		}
	}
	if reqLen > 0 {
		httpReq.ContentLength = reqLen
	}
	httpReq.Host = target
	httpReq.Header.Set("X-Forwarded-Host", h.Server)
	httpReq.Header.Set("X-Forwarded-Proto", "https")
	// Stamp the real browser origin so the backend doesn't see our localhost
	// socket peer. BrowserIP is only populated in fixed-target mode (the
	// OctoPrint plugin) — the wiring in serve.go withholds it in dynamic mode,
	// where BitBang proxies arbitrary LAN apps that may grant access based on
	// appearing local and would break if we injected XFF. Validate as a real
	// IP first: a malformed value (port suffix, garbage) could be rejected by
	// a strict backend, and forging is worse than omitting. The Set (not Add)
	// plus the skipHeaders strip means a client cannot influence this.
	if ip := net.ParseIP(h.BrowserIP); ip != nil {
		httpReq.Header.Set("X-Forwarded-For", ip.String())
	} else {
		httpReq.Header.Del("X-Forwarded-For")
	}
	httpReq.Header.Set("Referer", fmt.Sprintf("http://%s/", target))

	client := &http.Client{
		CheckRedirect: func(r *http.Request, via []*http.Request) error {
			if r.URL.Host != "" && r.URL.Host != target {
				h.connTarget = r.URL.Host
				h.targetPrefix = "/" + r.URL.Host
				if h.Verbose {
					log.Printf("Target updated: %s (from redirect)", r.URL.Host)
				}
				return nil
			}
			return http.ErrUseLastResponse
		},
	}

	resp, err := client.Do(httpReq)
	if err != nil {
		log.Printf("Proxy request failed: %s %s -> %v", req.Method, req.Pathname, err)
		h.sendError(s, 502, "Target unreachable")
		return
	}
	defer resp.Body.Close()

	headers := make(map[string]interface{})
	for key, values := range resp.Header {
		// Drop headers that don't apply once the response is being
		// delivered through BitBang's proxy:
		//   - X-Frame-Options: the response is rendered inside our
		//     bootstrap iframe; the app's anti-framing rule would
		//     prevent that from working.
		//   - Content-Security-Policy / -Report-Only: the SW injects
		//     an inline <script> with session id + cookie sync + the
		//     XHR / WebSocket shims. Any app with a strict
		//     script-src (Synology DSM, many enterprise UIs) would
		//     refuse to execute the shim and the proxy would lose
		//     XHR/WS routing. The app's CSP was designed for direct
		//     access at its own origin; once it's being deliberately
		//     tunneled through us, that policy no longer fits.
		switch key {
		case "X-Frame-Options",
			"Content-Security-Policy",
			"Content-Security-Policy-Report-Only":
			continue
		}
		if len(values) > 1 && key == "Set-Cookie" {
			headers[key] = values
		} else if len(values) > 0 {
			headers[key] = values[0]
		}
	}

	if loc, ok := headers["Location"].(string); ok && loc != "" {
		if parsed, err := neturl.Parse(loc); err == nil {
			pathOnly := parsed.RequestURI()
			if pathOnly != loc {
				headers["Location"] = pathOnly
				if h.Verbose {
					log.Printf("Redirect rewritten: %s -> %s", loc, pathOnly)
				}
			}
		}
	}

	respMeta := map[string]interface{}{
		"status":  resp.StatusCode,
		"headers": headers,
	}
	respJSON, _ := json.Marshal(respMeta)
	if err := s.WriteSYN(respJSON); err != nil {
		return
	}

	const maxBuffered = 8 << 20
	buf := make([]byte, protocol.MaxChunkSize)
	totalBytes := 0
	startTime := time.Now()
	nextLogMB := 50
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			for s.BufferedAmount() > maxBuffered {
				time.Sleep(1 * time.Millisecond)
			}
			if err := s.WriteDAT(buf[:n]); err != nil {
				log.Printf("WriteDAT failed (stream %d, %d bytes sent so far): %v", s.ID(), totalBytes, err)
				return
			}
			totalBytes += n
			if h.Verbose {
				mb := totalBytes / (1024 * 1024)
				if mb >= nextLogMB {
					elapsed := time.Since(startTime).Seconds()
					speed := float64(mb) / elapsed
					log.Printf("Upload (stream %d): %d MB (%.1f MB/s)", s.ID(), mb, speed)
					nextLogMB += 50
				}
			}
		}
		if readErr != nil {
			break
		}
	}

	_ = s.WriteFIN(nil)

	if h.Verbose || resp.StatusCode < 200 || resp.StatusCode >= 300 {
		log.Printf("%s %s -> %d (%d bytes)", req.Method, pathname, resp.StatusCode, totalBytes)
	}
}

func (h *HTTPHandler) serveLandingPage(s Stream, req protocol.Request) {
	if req.Pathname == "/favicon.ico" {
		h.sendError(s, 404, "Not found")
		return
	}
	headers := map[string]string{"Content-Type": "text/html"}
	body := []byte(strings.Replace(landingPageHTML, "{{UID}}", h.UID, 1))
	respMeta := map[string]interface{}{
		"status":  200,
		"headers": headers,
	}
	respJSON, _ := json.Marshal(respMeta)
	_ = s.WriteSYN(respJSON)
	if len(body) > 0 {
		_ = s.WriteDAT(body)
	}
	_ = s.WriteFIN(nil)
}

// ResolveTarget exposes the per-session target resolution to the paired
// WebSocket handler. Satisfies streamtype.TargetResolver.
func (h *HTTPHandler) ResolveTarget(requestPath string) (string, string) {
	return h.resolveTarget(requestPath)
}

// resolveTarget determines the target host and path for a request, handling
// dynamic-target redirects (e.g. nas.local -> nas.local:5000).
func (h *HTTPHandler) resolveTarget(requestPath string) (string, string) {
	if h.Target != "" {
		return h.connTarget, requestPath
	}
	if h.targetPrefix != "" && strings.HasPrefix(requestPath, h.targetPrefix) {
		remainder := requestPath[len(h.targetPrefix):]
		if remainder == "" {
			remainder = "/"
		}
		return h.connTarget, remainder
	}
	trimmed := strings.TrimPrefix(requestPath, "/")
	if slashIdx := strings.Index(trimmed, "/"); slashIdx > 0 {
		firstSeg := trimmed[:slashIdx]
		if strings.Contains(firstSeg, ":") {
			h.connTarget = firstSeg
			h.targetPrefix = "/" + firstSeg
			remainder := trimmed[slashIdx:]
			if h.Verbose {
				log.Printf("Target updated: %s (from redirect)", firstSeg)
			}
			return firstSeg, remainder
		}
	} else if strings.Contains(trimmed, ":") {
		h.connTarget = trimmed
		h.targetPrefix = "/" + trimmed
		if h.Verbose {
			log.Printf("Target updated: %s (from redirect)", trimmed)
		}
		return trimmed, "/"
	}
	if h.connectPrefix != "" && h.connectPrefix != h.targetPrefix && strings.HasPrefix(requestPath, h.connectPrefix) {
		remainder := requestPath[len(h.connectPrefix):]
		if remainder == "" {
			remainder = "/"
		}
		return h.connTarget, remainder
	}
	return h.connTarget, requestPath
}

// parseTargetFromPath extracts a host:port target from the first segment
// of the connect path. Returns (target, remainingPath).
func parseTargetFromPath(path string) (string, string) {
	trimmed := strings.TrimPrefix(path, "/")
	trimmed = strings.TrimPrefix(trimmed, "http://")
	trimmed = strings.TrimPrefix(trimmed, "https://")
	if trimmed == "" {
		return "", "/"
	}
	var target, remainder string
	if idx := strings.Index(trimmed, "/"); idx >= 0 {
		target = trimmed[:idx]
		remainder = trimmed[idx:]
	} else {
		target = trimmed
		remainder = "/"
	}
	if strings.Contains(target, ":") || strings.Contains(target, ".") || target == "localhost" {
		return target, remainder
	}
	return "", path
}

func (h *HTTPHandler) sendError(s Stream, status int, message string) {
	headers := map[string]string{"Content-Type": "text/plain"}
	body := []byte(message)
	respMeta := map[string]interface{}{
		"status":  status,
		"headers": headers,
	}
	respJSON, _ := json.Marshal(respMeta)
	_ = s.WriteSYN(respJSON)
	if len(body) > 0 {
		_ = s.WriteDAT(body)
	}
	_ = s.WriteFIN(nil)
}

const landingPageHTML = `<!DOCTYPE html>
<html>
<head>
    <meta charset="utf-8">
    <title>BitBang</title>
    <style>
        * { margin: 0; padding: 0; box-sizing: border-box; }
        body {
            font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, sans-serif;
            background: #fff;
            color: #333;
            padding: 12px 16px;
        }
        input {
            padding: 6px 8px;
            font-size: 14px;
            border: 1px solid #ccc;
            border-radius: 4px;
            width: 220px;
            outline: none;
        }
        input:focus { border-color: #999; }
        .hint {
            margin-top: 6px;
            font-size: 12px;
            color: #999;
        }
    </style>
</head>
<body>
    <input type="text" id="target" placeholder="hostname:port" autofocus
           onkeydown="if(event.key==='Enter')go()">
    <button onclick="go()" style="padding:6px 12px;font-size:14px;border:1px solid #ccc;border-radius:4px;background:#fff;cursor:pointer;margin-left:4px;">Go</button>
    <div class="hint">e.g. localhost:8080, nas.local, 192.168.1.10</div>
    <script>
        function go() {
            let target = document.getElementById('target').value.trim();
            if (!target) return;
            target = target.replace(/^https?:\/\//, '');
            target = target.replace(/\/$/, '');
            window.parent.postMessage({ type: 'bb-navigate', path: '/' + target }, '*');
        }
    </script>
</body>
</html>`
