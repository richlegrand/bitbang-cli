package streamtype

import (
	"context"
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

	"github.com/richlegrand/bitbang/internal/localdns"
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
	// connScheme is "http" or "https", resolved once per session in
	// OnConnect and reused for every request and WebSocket on it.
	connScheme string

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
	pw     *io.PipeWriter
	cancel context.CancelFunc
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

	// Resolve cross-host redirects and settle on a scheme for the session.
	//
	// Plaintext is probed FIRST because it is the common case: an HTTPS-only
	// target then pays one failed HTTP probe, rather than every plaintext
	// target paying a failed TLS handshake.
	if h.connTarget != "" {
		h.connScheme = "http"
		sawHTTPSRedirect := false
		probeURL := fmt.Sprintf("http://%s/", h.connTarget)
		probeClient := &http.Client{
			// mDNS-aware: a CGO_ENABLED=0 build cannot resolve .local
			// targets (nas.local and friends) through the system resolver.
			Transport: localdns.Transport(),
			CheckRedirect: func(r *http.Request, via []*http.Request) error {
				if r.URL.Scheme == "https" {
					sawHTTPSRedirect = true
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
		probeResp, probeErr := probeClient.Do(probeReq)
		if probeResp != nil {
			probeResp.Body.Close()
		}

		plaintextOK := probeErr == nil && probeResp != nil && probeResp.StatusCode < 400

		switch {
		case sawHTTPSRedirect:
			// Target redirects http -> https. Follow it rather than refusing.
			h.connScheme = "https"

		case plaintextOK:
			// Clean plaintext answer; nothing more to do.

		default:
			// Either the probe errored, or it "succeeded" with a 4xx/5xx.
			//
			// The error-only test is NOT sufficient: a TLS port does not
			// usually refuse a plaintext request. Go's TLS server and nginx
			// (which Frigate runs) both reply with a perfectly readable
			// HTTP 400 -- "The plain HTTP request was sent to HTTPS port".
			// Treating that as success is what left HTTPS targets silently
			// broken, so any non-2xx/3xx is worth a TLS probe.
			//
			// A plaintext server that genuinely answers 4xx at "/" (auth
			// required, say) just pays one failed handshake on a LAN and
			// stays on http.
			switch {
			case localdns.ProbeTLS(context.Background(), h.connTarget, probeTimeout):
				h.connScheme = "https"
			case probeErr != nil:
				// No HTTP response and no TLS handshake: nothing is there.
				// Previously this was swallowed and surfaced later as a 502
				// on every request with no hint as to why.
				return fmt.Errorf("%s is unreachable (no HTTP or HTTPS response)", h.connTarget)
			}
			// Otherwise: reachable over plaintext, just an error status at
			// "/" -- stay on http.
		}

		if h.Verbose {
			log.Printf("Connect: scheme=%s target=%s%s", h.connScheme, h.connTarget,
				map[bool]string{true: " (cert verification skipped: local target)", false: ""}[h.skipVerify()])
		}
	}
	return nil
}

// probeTimeout bounds the TLS fallback probe. LAN-local, so this is a
// ceiling for a hung target rather than a realistic latency budget --
// without it, a server that accepts the connection and never replies
// would stall the whole session setup.
const probeTimeout = 3 * time.Second

// scheme returns the resolved session scheme, defaulting to http for
// sessions that never ran the probe (fixed-target tests, landing page).
func (h *HTTPHandler) scheme() string {
	if h.connScheme == "" {
		return "http"
	}
	return h.connScheme
}

// Scheme implements the optional interface WSHandler type-asserts for, so
// a WebSocket on an HTTPS session dials wss:// rather than ws://.
func (h *HTTPHandler) Scheme() string { return h.scheme() }

// SkipVerify reports whether TLS certificate verification should be
// skipped for this session's target. Exposed for WSHandler, which needs
// the same trust decision for its own dialer.
func (h *HTTPHandler) SkipVerify() bool { return h.skipVerify() }

func (h *HTTPHandler) skipVerify() bool {
	return h.scheme() == "https" && isLocalHost(h.connTarget)
}

// httpClient returns a client for this session, with the transport chosen
// by the trust decision: local HTTPS targets skip verification, everything
// else verifies normally.
func (h *HTTPHandler) transport() *http.Transport {
	if h.skipVerify() {
		return localdns.InsecureTransport()
	}
	return localdns.Transport()
}

// isLocalHost reports whether a "host[:port]" target is on the local
// network: loopback, RFC1918, CGNAT, link-local, or an mDNS ".local" name.
//
// Only these skip certificate verification. A PUBLIC host failing
// verification is genuinely suspicious and still errors -- the exemption
// exists because home devices universally self-sign, not because
// certificates are inconvenient.
func isLocalHost(target string) bool {
	host := target
	if h, _, err := net.SplitHostPort(target); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	if host == "" {
		return false
	}
	lower := strings.ToLower(host)
	if lower == "localhost" || strings.HasSuffix(lower, ".localhost") {
		return true
	}
	if localdns.IsLocalName(host) {
		return true
	}
	ip := net.ParseIP(host)
	if ip == nil {
		// A bare hostname with no dots is a LAN name in practice
		// ("nas", "octopi"); anything dotted is treated as public.
		return !strings.Contains(host, ".")
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() {
		return true
	}
	// 100.64.0.0/10 -- CGNAT, used by Tailscale and similar overlays.
	if ip4 := ip.To4(); ip4 != nil && ip4[0] == 100 && ip4[1] >= 64 && ip4[1] <= 127 {
		return true
	}
	return false
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

	ctx, cancel := context.WithCancel(context.Background())
	ps := &pendingStream{cancel: cancel}
	var body io.Reader
	if !final {
		pr, pw := io.Pipe()
		ps.pw = pw
		body = pr
	}
	h.mu.Lock()
	old := h.streams[s.ID()]
	h.streams[s.ID()] = ps
	h.mu.Unlock()
	if old != nil {
		old.close()
	}

	go func() {
		defer h.finishStream(s.ID(), ps)
		h.proxyRequestContext(ctx, s, req, body)
	}()
	return nil
}

// OnDAT routes body bytes into the in-flight request's pipe.
func (h *HTTPHandler) OnDAT(s Stream, payload []byte) error {
	h.mu.Lock()
	ps := h.streams[s.ID()]
	h.mu.Unlock()
	if ps == nil || ps.pw == nil {
		return nil
	}
	if len(payload) > 0 {
		_, err := ps.pw.Write(payload)
		return err
	}
	return nil
}

// OnFIN closes the body pipe, signaling end-of-body to the in-flight
// HTTP request goroutine.
func (h *HTTPHandler) OnFIN(s Stream, payload []byte) error {
	h.mu.Lock()
	ps := h.streams[s.ID()]
	h.mu.Unlock()
	if ps == nil || ps.pw == nil {
		return nil
	}
	if len(payload) > 0 {
		if _, err := ps.pw.Write(payload); err != nil {
			return err
		}
	}
	return ps.pw.Close()
}

func (h *HTTPHandler) OnReset(s Stream, _, _ string) {
	h.mu.Lock()
	ps := h.streams[s.ID()]
	delete(h.streams, s.ID())
	h.mu.Unlock()
	if ps != nil {
		ps.close()
	}
}

func (h *HTTPHandler) proxyRequest(s Stream, req protocol.Request, body io.Reader) {
	h.proxyRequestContext(context.Background(), s, req, body)
}

func (h *HTTPHandler) proxyRequestContext(ctx context.Context, s Stream, req protocol.Request, body io.Reader) {
	if h.connTarget == "" {
		h.serveLandingPage(s, req)
		return
	}

	target, pathname := h.resolveTarget(req.Pathname)
	url := fmt.Sprintf("%s://%s%s", h.scheme(), target, pathname)

	// Stream request bytes directly into the upstream transport. Flow-control
	// credit is returned only when this reader advances, so a slow target cannot
	// turn an upload into an unbounded in-memory buffer. Browsers supply the
	// length for ordinary form/file bodies; preserving it avoids chunked
	// encoding for embedded servers that require Content-Length.
	var reqBody io.Reader
	if body != nil {
		reqBody = body
	}

	httpReq, err := http.NewRequestWithContext(ctx, req.Method, url, reqBody)
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
	if body != nil && req.ContentLength > 0 {
		httpReq.ContentLength = int64(req.ContentLength)
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
	httpReq.Header.Set("Referer", fmt.Sprintf("%s://%s/", h.scheme(), target))

	client := &http.Client{
		// Shared mDNS-aware transport -- also preserves the connection pool
		// across these per-request clients. Picks the verifying or
		// non-verifying variant per the session's trust decision.
		Transport: h.transport(),
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
		if ctx.Err() != nil {
			return
		}
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
	if err := protocol.ValidateFramePayload(respJSON); err != nil {
		log.Printf("HTTP response headers exceed SWSP frame limit: %v", err)
		h.sendError(s, 502, "Response headers are too large")
		return
	}
	if err := s.WriteSYN(respJSON); err != nil {
		return
	}

	const maxBuffered = 8 << 20
	buf := make([]byte, protocol.MaxChunkSize)
	totalBytes := 0
	startTime := time.Now()
	nextLogMB := 50
	backpressureTick := time.NewTicker(time.Millisecond)
	defer backpressureTick.Stop()
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			for s.BufferedAmount() > maxBuffered {
				select {
				case <-ctx.Done():
					return
				case <-backpressureTick.C:
				}
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

func (h *HTTPHandler) finishStream(id uint32, want *pendingStream) {
	h.mu.Lock()
	if h.streams[id] == want {
		delete(h.streams, id)
	}
	h.mu.Unlock()
	want.close()
}

func (ps *pendingStream) close() {
	ps.cancel()
	if ps.pw != nil {
		_ = ps.pw.CloseWithError(context.Canceled)
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
	// Strip any scheme the browser kept in the path. OnConnect already
	// stripped it to derive connTarget, but requests arrive carrying the
	// path the user actually typed -- so "/https://nas.local/" reaches here
	// intact, targetPrefix ("/nas.local") does not match it, and the
	// heuristic below would otherwise adopt "https:" as the hostname.
	trimmed := stripScheme(strings.TrimPrefix(requestPath, "/"))
	if slashIdx := strings.Index(trimmed, "/"); slashIdx > 0 {
		firstSeg := trimmed[:slashIdx]
		if looksLikeHostPort(firstSeg) {
			h.connTarget = firstSeg
			h.targetPrefix = "/" + firstSeg
			remainder := trimmed[slashIdx:]
			if h.Verbose {
				log.Printf("Target updated: %s (from redirect)", firstSeg)
			}
			return firstSeg, remainder
		}
	} else if looksLikeHostPort(trimmed) {
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

// stripScheme removes a leading http/https scheme from a target string.
//
// Tolerant on purpose: this string crosses browser fragment parsing,
// bootstrap.js, the service worker's path construction, and Go's URL
// handling, and each can reshape it. Observed and plausible manglings:
//
//	https://host:8971      as typed
//	https:/host:8971       "//" collapsed by a URL normaliser
//	HTTPS://host:8971      case varies
//	https%3A%2F%2Fhost     ":" and "/" percent-encoded
//
// Only the scheme prefix is decoded -- never the whole path, where %20 and
// friends are legitimate and decoding them would corrupt the request.
func stripScheme(s string) string {
	for _, scheme := range []string{"https", "http"} { // longest first
		for _, sep := range []string{"://", ":/", "%3A%2F%2F", "%3a%2f%2f", "%3A%2F", "%3a%2f"} {
			p := scheme + sep
			if len(s) > len(p) && strings.EqualFold(s[:len(p)], p) {
				return s[len(p):]
			}
		}
	}
	return s
}

// looksLikeHostPort reports whether a path segment can plausibly be a
// "host" or "host:port" target.
//
// Replaces a bare strings.Contains(seg, ":") test, which accepted "https:"
// as a hostname: with a scheme left in the path, the proxy adopted "https:"
// as the target, dialled it, and poisoned connTarget for the rest of the
// session. A colon alone is not evidence of a host.
// Deliberately a minimal change from the original strings.Contains(seg, ":")
// test: same answer for every segment except one that ENDS in a colon, which
// is a scheme remnant ("https:") rather than a host. Broadening it further
// would reclassify ordinary path segments as targets -- "webpages",
// "cgi-bin" and friends must keep resolving as paths.
func looksLikeHostPort(seg string) bool {
	if seg == "" || strings.HasSuffix(seg, ":") {
		return false
	}
	return strings.Contains(seg, ":")
}

// parseTargetFromPath extracts a host:port target from the first segment
// of the connect path. Returns (target, remainingPath).
func parseTargetFromPath(path string) (string, string) {
	trimmed := stripScheme(strings.TrimSpace(strings.TrimPrefix(path, "/")))
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
