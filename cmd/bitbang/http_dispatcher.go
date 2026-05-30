package main

import (
	"strings"

	"github.com/richlegrand/bitbang/internal/streamtype"
)

// httpDispatcher picks between two type="http" stream handlers based on
// the connect path:
//
//   - paths under known cap mounts (/, /shell, /files, /proxy) →
//     the in-process mux (HTTPLocalHandler wrapping shellweb/fileshare/
//     proxyweb landing page).
//   - paths whose first segment looks like a host:port target
//     (contains ":" or ".", or is "localhost") → the existing
//     streamtype.HTTPHandler in dynamic-target mode, which pins the
//     target at OnConnect, does a HEAD probe that resolves port
//     redirects (so e.g. nas.local → nas.local:5000), and proxies all
//     subsequent requests in that session to the resolved host.
//
// The choice is made once per session at OnConnect; subsequent
// OnSYN/OnDAT/OnFIN all route to the same chosen handler. This keeps
// each session purpose-built — every browser tab opened from the
// hamburger dropdown is its own session with its own target — and
// avoids the redirect-leak problem the previous per-request dynamic
// proxy had (upstream Location headers pointing at the real host:port
// would mixed-content-block in the iframe).
type httpDispatcher struct {
	local      streamtype.StreamHandler // mux of cap handlers
	proxy      streamtype.StreamHandler // dynamic-target HTTPHandler; nil if proxy cap disabled
	chosen     streamtype.StreamHandler // set at OnConnect, routes everything after
}

// Compile-time check.
var _ streamtype.StreamHandler = (*httpDispatcher)(nil)

func newHTTPDispatcher(local, proxy streamtype.StreamHandler) *httpDispatcher {
	return &httpDispatcher{local: local, proxy: proxy}
}

func (d *httpDispatcher) Type() string { return "http" }

func (d *httpDispatcher) OnConnect(path string) error {
	if d.proxy != nil && looksLikeProxyTarget(path) {
		d.chosen = d.proxy
	} else {
		d.chosen = d.local
	}
	return d.chosen.OnConnect(path)
}

func (d *httpDispatcher) OnSYN(s streamtype.Stream, payload []byte, final bool) error {
	return d.chosen.OnSYN(s, payload, final)
}

func (d *httpDispatcher) OnDAT(s streamtype.Stream, payload []byte) error {
	return d.chosen.OnDAT(s, payload)
}

func (d *httpDispatcher) OnFIN(s streamtype.Stream, payload []byte) error {
	return d.chosen.OnFIN(s, payload)
}

// looksLikeProxyTarget mirrors the heuristic in streamtype.HTTPHandler
// (parseTargetFromPath): a path segment is a proxy host if it contains
// ":" or ".", or equals "localhost". Reserved cap names (shell, files,
// proxy) never trigger this — they're short ascii-letter strings.
func looksLikeProxyTarget(path string) bool {
	trimmed := strings.TrimPrefix(path, "/")
	trimmed = strings.TrimPrefix(trimmed, "http://")
	trimmed = strings.TrimPrefix(trimmed, "https://")
	if trimmed == "" {
		return false
	}
	var first string
	if idx := strings.Index(trimmed, "/"); idx >= 0 {
		first = trimmed[:idx]
	} else {
		first = trimmed
	}
	return strings.Contains(first, ":") || strings.Contains(first, ".") || first == "localhost"
}
