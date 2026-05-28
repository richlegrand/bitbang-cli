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
package shellweb

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed static
var staticFS embed.FS

// ShellWeb is a thin wrapper that satisfies the same "give me an
// http.Handler" shape as fileshare.FileShare. There's no per-instance
// state today; New() exists so the API mirrors fileshare for
// consistency.
type ShellWeb struct{}

// New constructs a ShellWeb.
func New() *ShellWeb { return &ShellWeb{} }

// HTTPHandler serves the embedded static files (shell.html, shell.js)
// rooted at "/". The file server picks up `index.html` for the root
// path automatically.
func (s *ShellWeb) HTTPHandler() http.Handler {
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		// embed.FS errors here only happen if the //go:embed directive
		// is malformed at build time — fail loud.
		panic("shellweb: embedded static dir missing: " + err.Error())
	}
	return http.FileServer(http.FS(sub))
}
