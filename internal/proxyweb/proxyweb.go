// Package proxyweb implements the proxy cap's landing page — a small
// HTML form at /proxy/ where the user types a target URL (e.g.
// "localhost:3000") and the page opens that target in a new browser
// tab via the dynamic reverse proxy.
//
// The actual reverse proxying lives at the SWSP layer in
// streamtype.HTTPHandler, which pins the target per session (set from
// the connect path) and does a HEAD probe to resolve port redirects
// (e.g. nas.local → nas.local:5000). Putting it at the SWSP layer
// instead of the HTTP layer means upstream Location headers and
// absolute paths flow through naturally without leaking the iframe's
// /__device__/<sessionId>/ prefix.
package proxyweb

import (
	"embed"
	"net/http"
)

//go:embed proxy.html
var staticFS embed.FS

// LandingHandler serves the proxy-target form. The form is the entry
// point users see when they pick "Proxy" from the hamburger menu — it
// asks for a target URL and opens that target in a new browser tab,
// where the listener's SWSP HTTP handler dispatches it to the
// streamtype.HTTPHandler dynamic-target proxy.
func LandingHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Accept either /proxy/ (mounted form) or / (from inside the
		// listener after StripPrefix) — both land here.
		switch r.URL.Path {
		case "/", "/proxy/":
		default:
			http.NotFound(w, r)
			return
		}
		b, err := staticFS.ReadFile("proxy.html")
		if err != nil {
			http.Error(w, "missing landing template", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(b)
	})
}
