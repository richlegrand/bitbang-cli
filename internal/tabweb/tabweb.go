// Package tabweb builds the tab-container HTML page served at the root
// of a multi-cap listener (e.g. `bitbang serve --files X --shell`).
//
// The page is a small templated shell: a tab strip across the top, one
// iframe per cap underneath, JS to switch which iframe is visible. Each
// cap's actual UI is mounted under a subpath (/files/, /shell/, …) by
// the calling code; tabweb just navigates between them.
//
// Single-cap listeners skip the tab UI entirely — they serve the cap's
// own HTTPHandler directly at the root, same as before.
package tabweb

import (
	"embed"
	"html/template"
	"io/fs"
	"net/http"
)

//go:embed templates static
var assetFS embed.FS

// Cap describes one capability tab to render.
type Cap struct {
	// Name is the URL slug used in the tab nav (matches the path the
	// cap is mounted under, e.g. "files", "shell").
	Name string
	// Label is the human-readable tab title.
	Label string
	// URL is the iframe src — typically "/<Name>/", but kept explicit
	// so callers can override (e.g. point at an external URL).
	URL string
}

// New returns an http.Handler that serves the tab page at "/" and the
// embedded static assets (tabs.css, tabs.js) at their respective paths.
// Unknown paths return 404 so the caller's mux can still route subpaths
// to the actual cap handlers.
func New(caps []Cap) http.Handler {
	tmpl, err := template.ParseFS(assetFS, "templates/tab.html")
	if err != nil {
		panic("tabweb: parse template: " + err.Error())
	}
	staticSub, err := fs.Sub(assetFS, "static")
	if err != nil {
		panic("tabweb: static missing: " + err.Error())
	}
	staticHandler := http.FileServer(http.FS(staticSub))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_ = tmpl.Execute(w, struct{ Caps []Cap }{Caps: caps})
		case "/tabs.css", "/tabs.js":
			staticHandler.ServeHTTP(w, r)
		default:
			http.NotFound(w, r)
		}
	})
}
