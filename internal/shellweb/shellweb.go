// Package shellweb bundles the in-browser UI for `bitbang shell`.
//
// When a browser opens a shell-mode listener's URL, the existing
// service-worker tunnel proxies HTTP requests to this handler. We
// serve a small page that boots xterm.js and opens a magic WebSocket
// (path `/__bitbang/shell`). The bootstrap.js bridge in
// ~/bitbang-server/web/ recognizes that path and routes the WebSocket
// to a SWSP shell stream over the existing data channel.
//
// xterm.js itself is loaded from a CDN rather than vendored, to keep
// the binary small. The page degrades gracefully if the CDN is
// unreachable — the user gets a visible error in the iframe rather
// than a silent hang.
//
// Launcher mode: when constructed with capbar.Item entries, the shared
// strip (internal/capbar) is spliced into index.html at its CAP_BAR
// marker. Anchor clicks in the dropdown postMessage `{type:
// 'bb-open-cap', path: '<path>'}` up to bootstrap.js, which composes
// the full URL (including the secret access code from the fragment)
// and opens a new browser tab. Bootstrap.js never has to know about
// caps, labels, or dropdown rendering -- the device controls all of it.
package shellweb

import (
	"github.com/richlegrand/bitbang/internal/capbar"

	"embed"
	"io/fs"
	"net/http"
	"strings"
)

//go:embed static
var staticFS embed.FS

// ShellWeb serves the shell-cap browser UI. Construct with New() for
// plain shell, or New(WithCapBar(items)) to inject a hamburger strip
// at the top of the page (for the launcher tab in serve-all mode).
type ShellWeb struct {
	capBar   []capbar.Item
	viewOnly bool
}

// Option configures a ShellWeb at construction time.
type Option func(*ShellWeb)

// WithViewOnly serves the watch-only variant of the terminal page:
// shell.js shows a VIEW ONLY badge, disables stdin, and never
// transmits keystrokes or signals. This is a UX affordance; the
// listener drops input from view peers regardless (ShellHandler.
// ViewOnly), and the tmux client is attached read-only behind that.
func WithViewOnly() Option {
	return func(s *ShellWeb) {
		s.viewOnly = true
	}
}

// WithCapBar enables the launcher hamburger strip with the given
// dropdown entries. The strip has no current-cap label next to the
// hamburger — the visible iframe content (a terminal) makes it
// obvious which cap you're on, and naming it explicitly is just noise.
func WithCapBar(items []capbar.Item) Option {
	return func(s *ShellWeb) {
		s.capBar = items
	}
}

// New constructs a ShellWeb. With no options, serves plain shell.html.
// With WithCapBar, serves shell.html with a strip injected.
func New(opts ...Option) *ShellWeb {
	s := &ShellWeb{}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// HTTPHandler serves the shell page (index.html) and its companion
// static assets. When CapBar is configured, the root page gets the
// strip HTML injected at the <!-- CAP_BAR --> placeholder; other
// requests fall through to the embedded file server unchanged.
func (s *ShellWeb) HTTPHandler() http.Handler {
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		// embed.FS errors here only happen if the //go:embed directive
		// is malformed at build time — fail loud.
		panic("shellweb: embedded static dir missing: " + err.Error())
	}
	files := http.FileServer(http.FS(sub))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" || r.URL.Path == "/index.html" {
			s.serveIndex(w, r)
			return
		}
		files.ServeHTTP(w, r)
	})
}

// serveIndex renders index.html with the cap bar HTML substituted in.
// When no cap bar is configured, the placeholder line is stripped (so
// the served HTML doesn't carry an empty comment).
func (s *ShellWeb) serveIndex(w http.ResponseWriter, r *http.Request) {
	raw, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		http.Error(w, "missing index.html", http.StatusInternalServerError)
		return
	}

	out := string(raw)
	if s.viewOnly {
		// Flag script must land before shell.js so the terminal boots
		// straight into view mode (no input hooks to un-register).
		out = strings.Replace(out, `<script src="shell.js"></script>`,
			"<script>window.BB_VIEW_ONLY = true;</script>\n<script src=\"shell.js\"></script>", 1)
	}
	// The terminal is absolutely sized, so index.html carries its own
	// `body.with-cap-bar #terminal` rule on top of the shared padding.
	out = capbar.Inject(out, s.capBar)

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(out))
}
