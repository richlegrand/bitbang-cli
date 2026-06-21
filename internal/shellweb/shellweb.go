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
// Launcher mode: when constructed with CapBarItem entries, shellweb
// injects a 32px top strip with a hamburger dropdown into index.html.
// Anchor clicks in the dropdown postMessage `{type: 'bb-open-cap',
// path: '<path>'}` up to bootstrap.js, which composes the full URL
// (including the secret access code from the fragment) and opens a
// new browser tab. Bootstrap.js never has to know about caps, labels,
// or dropdown rendering — the device controls all of it.
package shellweb

import (
	"embed"
	"fmt"
	"html"
	"io/fs"
	"net/http"
	"strings"
)

//go:embed static
var staticFS embed.FS

// CapBarItem is one entry in the launcher's hamburger dropdown.
// Label is the human-readable text; Path is the listener-side URL the
// new tab should land on (e.g. "/files/").
type CapBarItem struct {
	Label string
	Path  string
}

// ShellWeb serves the shell-cap browser UI. Construct with New() for
// plain shell, or New(WithCapBar(items)) to inject a hamburger strip
// at the top of the page (for the launcher tab in serve-all mode).
type ShellWeb struct {
	capBar []CapBarItem
}

// Option configures a ShellWeb at construction time.
type Option func(*ShellWeb)

// WithCapBar enables the launcher hamburger strip with the given
// dropdown entries. The strip has no current-cap label next to the
// hamburger — the visible iframe content (a terminal) makes it
// obvious which cap you're on, and naming it explicitly is just noise.
func WithCapBar(items []CapBarItem) Option {
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
	if len(s.capBar) > 0 {
		out = strings.Replace(out, "<!-- CAP_BAR -->", renderCapBar(s.capBar), 1)
		// Mark the body so its #terminal shrinks to leave room for the strip.
		out = strings.Replace(out, "<body>", `<body class="with-cap-bar">`, 1)
	} else {
		out = strings.Replace(out, "<!-- CAP_BAR -->\n", "", 1)
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(out))
}

// renderCapBar emits the 32px launcher strip: just a hamburger button
// with a dropdown of openable caps. No current-cap label — the iframe
// content speaks for itself. Each dropdown anchor postMessages
// bb-open-cap to the parent (bootstrap.js) which knows the secret
// access code and composes the new-tab URL. The iframe itself never
// sees the code.
//
// Everything inline (CSS + JS + markup) so the strip is one
// self-contained chunk — bootstrap.js doesn't need to coordinate
// styling or hook event handlers.
func renderCapBar(items []CapBarItem) string {
	var dropdown strings.Builder
	for _, it := range items {
		fmt.Fprintf(&dropdown,
			`<a href="#" data-path="%s">%s</a>`,
			html.EscapeString(it.Path), html.EscapeString(it.Label))
	}
	return fmt.Sprintf(`<style>
#bb-cap-bar {
  position: fixed; top: 0; left: 0; right: 0; height: 22px;
  background: #000;
  display: flex; align-items: center; padding: 0 8px 0 2px;
  font-family: -apple-system, "Segoe UI", Roboto, sans-serif;
  color: #ccc; z-index: 100;
}
#bb-cap-bar button {
  background: transparent; border: none; padding: 2px 6px;
  cursor: pointer; display: flex; align-items: center;
}
#bb-cap-bar button:hover { background: #222; border-radius: 3px; }
#bb-cap-bar svg { display: block; }
#bb-cap-bar nav {
  position: absolute; top: 22px; left: 0;
  min-width: 160px; background: #000;
  border: 1px solid #333;
  box-shadow: 0 2px 6px rgba(0,0,0,0.4);
}
#bb-cap-bar nav[hidden] { display: none; }
#bb-cap-bar nav a {
  display: block; padding: 4px 14px;
  font-size: 14px; color: #ccc; text-decoration: none;
}
#bb-cap-bar nav a:hover { background: #222; }
body.with-cap-bar #terminal { margin-top: 22px; }
</style>
<div id="bb-cap-bar">
  <button id="bb-ham" aria-label="Capabilities menu">
    <svg width="10" height="6" viewBox="0 0 10 6" xmlns="http://www.w3.org/2000/svg">
      <path d="M0 0 L10 0 L5 6 Z" fill="#ccc"/>
    </svg>
  </button>
  <nav id="bb-menu" hidden>%s</nav>
</div>
<script>
(function(){
  var ham = document.getElementById('bb-ham');
  var menu = document.getElementById('bb-menu');
  ham.addEventListener('click', function(e){ e.stopPropagation(); menu.hidden = !menu.hidden; });
  document.addEventListener('click', function(e){
    if (!menu.contains(e.target) && e.target !== ham) menu.hidden = true;
  });
  menu.querySelectorAll('a').forEach(function(a){
    a.addEventListener('click', function(e){
      e.preventDefault();
      parent.postMessage({type:'bb-open-cap', path: a.dataset.path}, '*');
      menu.hidden = true;
    });
  });
})();
</script>`, dropdown.String())
}
