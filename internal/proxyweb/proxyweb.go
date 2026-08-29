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
	"io"
	"strings"

	"github.com/richlegrand/bitbang/internal/capbar"

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
//
// When the listener named its targets, the form is disabled rather than
// replaced by a list of them. The caret is where choosing happens
// everywhere else in the product, and the targets are already entries in
// it -- a second chooser on the page would be a second mechanism for one
// job. Left visible because a disabled control still says what is normally
// here and that this listener does not offer it.
func LandingHandler(bar []capbar.Item, targets []string) http.Handler {
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
		page := string(b)
		if len(targets) > 0 {
			var ok bool
			if page, ok = disableForm(page); !ok {
				// The markup moved. Serving a live form would invite
				// someone to type a target that is then refused, so fail
				// loudly rather than quietly offering a dead end.
				http.Error(w, "landing template no longer matches", http.StatusInternalServerError)
				return
			}
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, capbar.Inject(page, bar, capbar.Caret))
	})
}

// disableForm greys out the target box and its button, and points at the
// caret instead. Reports false if the template no longer contains what it
// expects, so a markup change cannot silently leave the form live.
func disableForm(page string) (string, bool) {
	const input = `<input type="text" id="target" placeholder="localhost:3000" autofocus`
	const button = `<button onclick="go()">Go</button>`
	const hint = `<div class="hint">e.g. <code>localhost:3000</code>, <code>192.168.1.50:8080</code>.</div>`
	if !strings.Contains(page, input) || !strings.Contains(page, button) || !strings.Contains(page, hint) {
		return page, false
	}
	page = strings.Replace(page, input,
		`<input type="text" id="target" placeholder="set by the listener" disabled`, 1)
	page = strings.Replace(page, button, `<button onclick="go()" disabled>Go</button>`, 1)
	page = strings.Replace(page, hint,
		`<div class="hint">This listener chose its targets — pick one from the menu above.</div>`, 1)
	return page, true
}
