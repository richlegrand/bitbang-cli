// Package proxy handles forwarding SWSP requests to local HTTP servers.
package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	neturl "net/url"
	"strings"
	"sync"
	"time"

	"github.com/pion/webrtc/v4"
	"github.com/richlegrand/bitbang/bitbangproxy/internal/auth"
	"github.com/richlegrand/bitbang/bitbangproxy/internal/protocol"
)

// Handler processes SWSP frames from the data channel, proxies HTTP requests
// to the target server, and sends responses back as SWSP frames.
type Handler struct {
	Target      string     // e.g. "localhost:8080" (from --target flag)
	UID         string     // device UID for landing page redirect
	Server      string     // signaling server hostname (e.g. "bitba.ng")
	PIN         *auth.PINAuth // PIN authentication (nil = no auth)
	Verbose     bool
	DC          *webrtc.DataChannel
	connTarget    string // resolved target for this connection (from URL or --target)
	targetPrefix  string // path prefix to strip from requests (e.g. "/localhost:8080")
	connectPrefix string // original prefix from connect (doesn't change on redirect)
	authenticated bool   // true after successful PIN auth (or if no PIN required)

	mu      sync.Mutex
	streams map[uint32]*pendingStream // in-flight request bodies
	wsConns map[uint32]*wsStream      // active WebSocket connections
}

// pendingStream tracks a request whose body is still arriving via DAT frames.
type pendingStream struct {
	req protocol.Request
	pw  *io.PipeWriter
}

// HandleMessage processes a raw data channel message (one SWSP frame).
func (h *Handler) HandleMessage(data []byte) {
	frame, err := protocol.ParseFrame(data)
	if err != nil {
		log.Printf("Failed to parse frame: %v", err)
		return
	}

	// StreamId 0 is reserved for control messages (connect/ready/auth)
	if frame.StreamID == 0 {
		h.handleControl(frame)
		return
	}

	if frame.IsSYN() {
		h.handleSYN(frame)
	} else {
		h.handleBodyFrame(frame)
	}
}

func (h *Handler) handleControl(frame protocol.Frame) {
	if !frame.IsSYN() {
		return
	}

	var msg struct {
		Type string `json:"type"`
		Path string `json:"path"`
		PIN  string `json:"pin"`
	}
	if err := json.Unmarshal(frame.Payload, &msg); err != nil {
		return
	}

	if msg.Type == "auth" {
		h.handleAuth(msg.PIN)
		return
	}

	if msg.Type == "connect" {
		path := msg.Path
		if path == "" {
			path = "/"
		}

		if h.Target != "" {
			// --target flag: use fixed target, path is passed through as-is
			h.connTarget = h.Target
			h.targetPrefix = ""
			if h.Verbose {
				log.Printf("Connect: target=%s path=%s", h.connTarget, path)
			}
		} else {
			// Dynamic: extract target from the path
			target, _ := parseTargetFromPath(path)
			if target == "" {
				// No target -- serve landing page, user will enter a URL
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

		// Probe the target to resolve any cross-host redirects (e.g.
		// nas.local -> nas.local:5000) before accepting requests.
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
				log.Printf("Connect: %s requires HTTPS (not supported)", h.connTarget)
				errMsg, _ := json.Marshal(map[string]string{
					"type":    "error",
					"message": h.connTarget + " requires HTTPS, which is not currently supported by BitBangProxy.",
				})
				h.sendFrame(0, protocol.FlagSYN|protocol.FlagFIN, errMsg)
				return
			}
		}


		// If PIN is required, send auth_required instead of ready
		if h.PIN.Required() {
			log.Printf("PIN required for connection")
			authReq, _ := json.Marshal(map[string]string{"type": "auth_required"})
			h.sendFrame(0, protocol.FlagSYN, authReq)
		} else {
			h.authenticated = true
			ready, _ := json.Marshal(map[string]string{"type": "ready"})
			h.sendFrame(0, protocol.FlagSYN|protocol.FlagFIN, ready)
		}
	}
}

func (h *Handler) handleAuth(pin string) {
	if !h.PIN.Required() {
		return
	}

	if h.PIN.Verify(pin) {
		log.Printf("PIN auth succeeded")
		h.authenticated = true
		result, _ := json.Marshal(map[string]interface{}{"type": "auth_result", "success": true})
		h.sendFrame(0, protocol.FlagSYN|protocol.FlagFIN, result)
	} else {
		log.Printf("PIN auth failed")
		result, _ := json.Marshal(map[string]interface{}{"type": "auth_result", "success": false})
		h.sendFrame(0, protocol.FlagSYN|protocol.FlagFIN, result)
	}
}

func (h *Handler) handleSYN(frame protocol.Frame) {
	// Check if this is a WebSocket stream
	var peek struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(frame.Payload, &peek) == nil && peek.Type == "websocket" {
		h.handleWSOpen(frame)
		return
	}

	req, err := frame.ParseRequest()
	if err != nil {
		log.Printf("Failed to parse request: %v", err)
		h.sendError(frame.StreamID, 400, "Bad request")
		return
	}

	if frame.IsFIN() {
		// SYN|FIN: complete request in one frame (no body)
		go h.proxyRequest(frame.StreamID, req, nil)
	} else {
		// SYN only: body frames will follow. Create a pipe so the HTTP
		// request can read the body as DAT frames arrive.
		pr, pw := io.Pipe()

		h.mu.Lock()
		if h.streams == nil {
			h.streams = make(map[uint32]*pendingStream)
		}
		h.streams[frame.StreamID] = &pendingStream{req: req, pw: pw}
		h.mu.Unlock()

		go h.proxyRequest(frame.StreamID, req, pr)
	}
}

func (h *Handler) handleBodyFrame(frame protocol.Frame) {
	// Check if this is a WebSocket stream
	h.mu.Lock()
	_, isWS := h.wsConns[frame.StreamID]
	h.mu.Unlock()
	if isWS {
		h.handleWSMessage(frame)
		return
	}

	h.mu.Lock()
	ps := h.streams[frame.StreamID]
	h.mu.Unlock()

	if ps == nil {
		return // unknown stream, ignore
	}

	// Write body data into the pipe
	if len(frame.Payload) > 0 {
		ps.pw.Write(frame.Payload)
	}

	// FIN closes the pipe, signaling end of body to the HTTP request
	if frame.IsFIN() {
		ps.pw.Close()
		h.mu.Lock()
		delete(h.streams, frame.StreamID)
		h.mu.Unlock()
	}
}

func (h *Handler) proxyRequest(streamID uint32, req protocol.Request, body io.Reader) {
	// If no target is set, serve the landing page
	if h.connTarget == "" {
		h.serveLandingPage(streamID, req)
		return
	}

	// Resolve the target and path for this request.
	// The path may contain a different target than the original connect
	// (e.g. after a redirect from nas.local to nas.local:5000).
	target, pathname := h.resolveTarget(req.Pathname)
	url := fmt.Sprintf("http://%s%s", target, pathname)

	// Buffer the body so we can set Content-Length explicitly.
	// Many embedded web servers (e.g. TP-Link routers) don't support
	// Transfer-Encoding: chunked, which Go uses for unbuffered pipe bodies.
	var reqBody io.Reader
	var reqLen int64
	if body != nil {
		bodyBytes, err := io.ReadAll(body)
		if err != nil {
			log.Printf("Failed to read request body: %v", err)
			h.sendError(streamID, 500, "Internal error")
			return
		}
		reqBody = bytes.NewReader(bodyBytes)
		reqLen = int64(len(bodyBytes))
	}

	httpReq, err := http.NewRequest(req.Method, url, reqBody)
	if err != nil {
		log.Printf("Failed to create HTTP request: %v", err)
		h.sendError(streamID, 500, "Internal error")
		return
	}

	// Forward request headers from the browser/SW, skipping headers
	// that must be rewritten to match the target (not bitba.ng).
	skipHeaders := map[string]bool{
		"host": true, "origin": true, "referer": true, "content-length": true,
	}
	if req.Headers != nil {
		for key, value := range req.Headers {
			if !skipHeaders[strings.ToLower(key)] {
				httpReq.Header.Set(key, value)
			}
		}
	} else {
		// Fallback for older SWSP without headers field
		if req.ContentType != "" {
			httpReq.Header.Set("Content-Type", req.ContentType)
		}
	}
	// Set content length from buffered body (avoids chunked encoding)
	if reqLen > 0 {
		httpReq.ContentLength = reqLen
	}
	// Set Host, Referer, and Origin to match the target, not bitba.ng.
	// Forward the original host so reverse-proxy-aware apps (e.g. OctoPrint)
	// generate URLs and cookies matching the external hostname.
	httpReq.Host = target
	httpReq.Header.Set("X-Forwarded-Host", h.Server)
	httpReq.Header.Set("X-Forwarded-Proto", "https")
	httpReq.Header.Set("Referer", fmt.Sprintf("http://%s/", target))

	// Only follow redirects that change the target host (e.g. nas.local ->
	// nas.local:5000). Same-host redirects are passed back to the browser
	// so the iframe URL stays correct for relative path resolution.
	client := &http.Client{
		CheckRedirect: func(r *http.Request, via []*http.Request) error {
			if r.URL.Host != "" && r.URL.Host != target {
				h.connTarget = r.URL.Host
				h.targetPrefix = "/" + r.URL.Host
				if h.Verbose {
					log.Printf("Target updated: %s (from redirect)", r.URL.Host)
				}
				return nil // follow cross-host redirect
			}
			return http.ErrUseLastResponse // pass same-host redirect to browser
		},
	}

	resp, err := client.Do(httpReq)
	if err != nil {
		log.Printf("Proxy request failed: %s %s -> %v", req.Method, req.Pathname, err)
		h.sendError(streamID, 502, "Target unreachable")
		return
	}
	defer resp.Body.Close()

	// Build response headers. Set-Cookie may have multiple values which
	// must be preserved as an array for the SW cookie jar to process.
	// X-Frame-Options is stripped because all proxied content loads in
	// an iframe — the target's framing policy doesn't apply here.
	headers := make(map[string]interface{})
	for key, values := range resp.Header {
		if key == "X-Frame-Options" {
			continue
		}
		if len(values) > 1 && key == "Set-Cookie" {
			headers[key] = values
		} else if len(values) > 0 {
			headers[key] = values[0]
		}
	}

	// Rewrite redirect Location to just the path so the browser stays
	// within the /__device__/ scope. The SW will catch the absolute path
	// and redirect it to /__device__/<sessionId>/path.
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

	// Send SYN frame with status and headers immediately
	respMeta := map[string]interface{}{
		"status":  resp.StatusCode,
		"headers": headers,
	}
	respJSON, _ := json.Marshal(respMeta)
	if err := h.sendFrame(streamID, protocol.FlagSYN, respJSON); err != nil {
		return
	}

	// Stream DAT frames as chunks arrive from the local server.
	// Backpressure: cap data channel send buffer to prevent unbounded RAM
	// growth and head-of-line blocking between concurrent streams. The cap
	// also bounds bandwidth-delay product -- too low throttles WAN throughput.
	const maxBuffered = 8 << 20 // 8MB
	buf := make([]byte, protocol.MaxChunkSize)
	totalBytes := 0
	startTime := time.Now()
	nextLogMB := 50
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			for h.DC.BufferedAmount() > maxBuffered {
				time.Sleep(1 * time.Millisecond)
			}
			if err := h.sendFrame(streamID, protocol.FlagDAT, buf[:n]); err != nil {
				log.Printf("sendFrame failed (stream %d, %d bytes sent so far): %v", streamID, totalBytes, err)
				return
			}
			totalBytes += n
			if h.Verbose {
				mb := totalBytes / (1024 * 1024)
				if mb >= nextLogMB {
					elapsed := time.Since(startTime).Seconds()
					speed := float64(mb) / elapsed
					log.Printf("Upload (stream %d): %d MB (%.1f MB/s)", streamID, mb, speed)
					nextLogMB += 50
				}
			}
		}
		if readErr != nil {
			break // EOF or error
		}
	}

	// Send FIN frame
	h.sendFrame(streamID, protocol.FlagFIN, nil)

	if h.Verbose || resp.StatusCode < 200 || resp.StatusCode >= 300 {
		log.Printf("%s %s -> %d (%d bytes)", req.Method, pathname, resp.StatusCode, totalBytes)
	}
}

func (h *Handler) serveLandingPage(streamID uint32, req protocol.Request) {
	if req.Pathname == "/favicon.ico" {
		h.sendError(streamID, 404, "Not found")
		return
	}

	headers := map[string]string{"Content-Type": "text/html"}
	html := strings.Replace(landingPageHTML, "{{UID}}", h.UID, 1)
	body := []byte(html)
	frames := protocol.BuildResponseFrames(streamID, 200, headers, body)
	for _, f := range frames {
		if h.DC.ReadyState() != webrtc.DataChannelStateOpen {
			return
		}
		h.DC.Send(f)
	}
}

const landingPageHTML = `<!DOCTYPE html>
<html>
<head>
    <meta charset="utf-8">
    <title>BitBangProxy</title>
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

func (h *Handler) sendFrame(streamID uint32, flags uint16, payload []byte) error {
	if h.DC.ReadyState() != webrtc.DataChannelStateOpen {
		return fmt.Errorf("data channel closed")
	}
	return h.DC.Send(protocol.BuildFrame(streamID, flags, payload))
}

// resolveTarget determines the target host and path for a request.
// Strips the target prefix from the path if present, handling the case
// where a redirect changed the target (e.g. nas.local -> nas.local:5000).
func (h *Handler) resolveTarget(requestPath string) (string, string) {
	if h.Target != "" {
		// Fixed target from --target flag -- path is passed through
		return h.connTarget, requestPath
	}

	// Try to strip the current target prefix
	if h.targetPrefix != "" && strings.HasPrefix(requestPath, h.targetPrefix) {
		remainder := requestPath[len(h.targetPrefix):]
		if remainder == "" {
			remainder = "/"
		}
		return h.connTarget, remainder
	}

	// Target prefix doesn't match -- check if a redirect changed the target.
	// Only re-parse if the first path segment contains a colon (host:port),
	// which is unambiguous. Bare hostnames (nas.local) can't be distinguished
	// from file paths (favicon.ico) so we don't re-parse those.
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
		// Path is just "/<host:port>" with no trailing content
		h.connTarget = trimmed
		h.targetPrefix = "/" + trimmed
		if h.Verbose {
			log.Printf("Target updated: %s (from redirect)", trimmed)
		}
		return trimmed, "/"
	}


	// Check the original connect prefix (iframe URL uses this even after
	// a cross-host redirect changed the target, e.g. nas.local -> nas.local:5000)
	if h.connectPrefix != "" && h.connectPrefix != h.targetPrefix && strings.HasPrefix(requestPath, h.connectPrefix) {
		remainder := requestPath[len(h.connectPrefix):]
		if remainder == "" {
			remainder = "/"
		}
		return h.connTarget, remainder
	}
	// No new target found -- use current connect target with full path
	return h.connTarget, requestPath
}

// parseTargetFromPath extracts a host:port target from the first segment of
// the connect path. Returns (target, remainingPath).
// e.g. "/nas.local:8080/admin" -> ("nas.local:8080", "/admin")
// e.g. "/192.168.1.10:3000"   -> ("192.168.1.10:3000", "/")
// e.g. "/just-a-path"         -> ("", "/just-a-path") -- no port, not a target
func parseTargetFromPath(path string) (string, string) {
	// Strip leading slash and any http(s):// scheme that users sometimes
	// paste into the landing page URL input.
	trimmed := strings.TrimPrefix(path, "/")
	trimmed = strings.TrimPrefix(trimmed, "http://")
	trimmed = strings.TrimPrefix(trimmed, "https://")
	if trimmed == "" {
		return "", "/"
	}

	// Split on first slash: "nas.local:8080/admin" -> ["nas.local:8080", "admin"]
	var target, remainder string
	if idx := strings.Index(trimmed, "/"); idx >= 0 {
		target = trimmed[:idx]
		remainder = trimmed[idx:] // keeps the leading /
	} else {
		target = trimmed
		remainder = "/"
	}

	// Accept host:port, dotted hostname (nas.local, 192.168.1.10), or localhost
	if strings.Contains(target, ":") || strings.Contains(target, ".") || target == "localhost" {
		return target, remainder
	}

	return "", path // not a recognizable target
}

func (h *Handler) sendError(streamID uint32, status int, message string) {
	headers := map[string]string{"Content-Type": "text/plain"}
	body := []byte(message)
	frames := protocol.BuildResponseFrames(streamID, status, headers, body)
	for _, f := range frames {
		if h.DC.ReadyState() != webrtc.DataChannelStateOpen {
			return
		}
		h.DC.Send(f)
	}
}
