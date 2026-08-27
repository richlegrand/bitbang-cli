package main

import (
	"github.com/richlegrand/bitbang/internal/capbar"

	"fmt"
	"html"
	"io"
	"net/http"
	"strings"

	"github.com/richlegrand/bitbang/internal/fileshare"
	"github.com/richlegrand/bitbang/internal/identity"
	"github.com/richlegrand/bitbang/internal/links"
	"github.com/richlegrand/bitbang/internal/shellweb"
	"github.com/richlegrand/bitbang/internal/streamtype"
)

// offeredScopes lists what this listener supports, which is what an absent
// scope on a link resolves to and what a requested one is intersected with.
func offeredScopes(cfg serveConfig) []string {
	var out []string
	for _, c := range capabilities {
		if cfg.caps.has(c.Scope) {
			out = append(out, c.Scope)
		}
	}
	return out
}

// sessionHandlers is the stream-handler set for one peer, plus the two
// handlers whose resources outlive a stream and must be closed when the
// connection goes.
type sessionHandlers struct {
	all   []streamtype.StreamHandler
	shell *streamtype.ShellHandler
	tcp   *streamtype.TCPHandler
}

// buildHandlers assembles the handler set a single peer gets, given the
// scopes its link grants.
//
// Building the set rather than filtering a complete one is what makes
// scope enforcement free: sendReady derives advertised caps from the
// registered handlers, so a files-scoped link advertises caps ["file"]
// with no extra code; `bitbang connect` already fails with "listener does
// not advertise the 'shell' capability"; and OnConnect never runs for a
// handler that was never built, which matters because the HTTP proxy's
// OnConnect resolves and probes its target.
func buildHandlers(cfg serveConfig, granted map[string]bool, share *fileshare.FileShare,
	shellArgv []string, id *identity.Identity, browserIP string, mirror io.Writer,
	owner bool) sessionHandlers {

	x := capContext{cfg: cfg, share: share, shellArgv: shellArgv, id: id,
		browserIP: browserIP, granted: granted, mirror: mirror, owner: owner}

	var out sessionHandlers
	for _, c := range capabilities {
		if !x.reaches(c.Scope) {
			continue
		}
		for _, h := range c.Build(x) {
			// Two handlers own resources that outlive a stream, so the
			// connection's teardown needs them by name.
			switch t := h.(type) {
			case *streamtype.ShellHandler:
				out.shell = t
			case *streamtype.TCPHandler:
				out.tcp = t
			}
			out.all = append(out.all, h)
		}
	}

	// With a fixed target `http` is the proxy itself, already contributed
	// above, and there is no local UI to dispatch to.
	if fixedTargetMode(cfg) {
		return out
	}

	// The dispatcher is not a capability: the listener's browser UI is the
	// frame the capabilities render in, so it rides on every link and shows
	// only what that link grants. Its proxy branch is handed over only when
	// the link reaches proxy, and a nil proxy is already how the dispatcher
	// is told there is none.
	var proxyHTTP streamtype.StreamHandler
	if x.reaches(links.ScopeProxy) {
		proxyHTTP = dynamicProxy(x)
	}
	local := streamtype.NewHTTPLocal(buildServeHTTPHandler(x), cfg.verbose)
	out.all = append(out.all, newHTTPDispatcher(local, proxyHTTP))
	return out
}

// buildServeHTTPHandler composes the in-process HTTP front-end from the
// capabilities this link reaches.
//
// One granted cap is served at "/" directly, so relative URLs in its HTML
// resolve cleanly. More than one gets the launcher: shell at "/" with the
// cap bar injected, and each cap at its own mount.
//
// Dynamic-target reverse proxying lives at the SWSP layer in
// streamtype.HTTPHandler, dispatched by httpDispatcher based on the connect
// path -- those paths never reach this handler.
func buildServeHTTPHandler(x capContext) http.Handler {
	// Resolved before the handlers are built, because each cap page
	// splices the strip at construction. Only when there is more than one
	// destination: a link granting a single thing has nowhere to move
	// between, and a dropdown of one is furniture.
	if items := capBarItems(x); len(items) > 1 {
		x.capBar = items
	}

	type mounted struct {
		cap     capability
		handler http.Handler
	}
	var live []mounted
	for _, c := range capabilities {
		if c.Web == nil || !x.reaches(c.Scope) {
			continue
		}
		if h := c.Web(x); h != nil {
			live = append(live, mounted{c, h})
		}
	}

	if len(live) == 0 {
		// Everything this link grants is command-line only -- today that
		// means a forward-only link. It authorizes fine and then has
		// nothing to render, and a bare 404 reads as a broken link
		// rather than as "this one is for the CLI".
		return noBrowserPage(x)
	}
	if len(live) == 1 {
		return live[0].handler
	}

	// The strip's dropdown anchors postMessage parent to open new tabs
	// (bootstrap.js handles the URL composition).
	launcher := shellweb.New(shellweb.WithCapBar(x.capBar)).HTTPHandler()

	mux := http.NewServeMux()
	capRoots := map[string]bool{}
	for _, m := range live {
		root := strings.TrimSuffix(m.cap.Mount, "/")
		mux.Handle(m.cap.Mount, http.StripPrefix(root, m.handler))
		capRoots[root] = true
	}

	// "/" is the launcher tab: shell plus the strip. Without shell, the
	// first granted cap takes the root instead.
	if x.reaches(links.ScopeShell) {
		mux.Handle("/", launcher)
	} else {
		mux.Handle("/", live[0].handler)
	}

	// Trailing-slash normalizer for the cap subpath roots. Without this,
	// "/proxy" -> 301 -> "/proxy/", and the redirect's server-relative
	// Location loses the browser's /__device__/<sessionId>/ prefix.
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if capRoots[r.URL.Path] {
			r.URL.Path += "/"
		}
		mux.ServeHTTP(w, r)
	})
}

// noBrowserPage explains a link whose grants have no browser side, and
// says what to do with it instead. Served at every path, because
// somebody following a link will not try a second one.
func noBrowserPage(x capContext) http.Handler {
	var granted []string
	for _, c := range capabilities {
		if x.reaches(c.Scope) {
			granted = append(granted, c.Scope)
		}
	}
	what := strings.Join(granted, ", ")
	if what == "" {
		what = "nothing this listener offers"
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The host the visitor actually used, not a compiled-in one:
		// this listener may be on somebody's own signaling server, and
		// sending them to ours would install a binary from a project
		// they did not choose.
		host := r.Host
		if host == "" {
			host = x.cfg.server
		}
		// The install endpoint is a shell script -- it exists to be
		// piped to sh. Linking a person at it downloads a wall of bash,
		// which is why it reads as broken. Show the command instead.
		body := fmt.Sprintf(`<!doctype html>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>BitBang -- command-line link</title>
<style>
 body { font: 15px/1.55 -apple-system, "Segoe UI", Roboto, sans-serif;
        max-width: 34rem; margin: 12vh auto; padding: 0 1.2rem; color: #222; }
 code { background: #f4f4f4; padding: .1rem .3rem; border-radius: 3px; }
 pre  { background: #f4f4f4; padding: .7rem .9rem; border-radius: 4px;
        overflow-x: auto; }
 .muted { color: #666; }
</style>
<h2>Nothing to show here</h2>
<p>This link grants <strong>%s</strong>, which BitBang drives from the
command line rather than the browser.</p>
<p>Copy the address bar and use it with the CLI:</p>
<pre>bitbang connect &lt;this URL&gt; -L 8080:localhost:80</pre>
<p class="muted">The link is valid -- there is simply no page for what it
allows. No CLI yet?</p>
<pre>curl -fsSL https://%s/install | sh</pre>
`, html.EscapeString(what), html.EscapeString(host))

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, body)
	})
}

// capBarItems composes the launcher dropdown from the capabilities this
// link reaches. Items render in table order.
func capBarItems(x capContext) []capbar.Item {
	var items []capbar.Item
	for _, c := range capabilities {
		if c.Menu == "" || !x.reaches(c.Scope) {
			continue
		}
		if c.MenuWhen != nil && !c.MenuWhen(x) {
			continue
		}
		// A proxy given several targets is several entries: the caret is
		// where someone picks one, so listing them there is what makes a
		// multi-target proxy usable without a landing page in between.
		if c.Scope == links.ScopeProxy && len(x.cfg.proxyTargets) > 1 {
			for _, t := range x.cfg.proxyTargets {
				items = append(items, capbar.Item{
					Label: c.Menu + " " + t,
					Path:  c.MenuPath + t + "/",
				})
			}
			continue
		}
		// A single pinned target needs no landing page either -- go
		// straight there, the way the whole URL would if nothing else
		// were served.
		if c.Scope == links.ScopeProxy && len(x.cfg.proxyTargets) == 1 && !fixedTargetMode(x.cfg) {
			items = append(items, capbar.Item{
				Label: c.Menu + " " + x.cfg.proxyTargets[0],
				Path:  c.MenuPath + x.cfg.proxyTargets[0] + "/",
			})
			continue
		}
		items = append(items, capbar.Item{Label: c.Menu, Path: c.MenuPath})
	}
	return items
}

// printSharingBlock prints the "Sharing:" status block listing each
// offered capability with its salient configuration.
//
// Takes a writer so the exact wording can be pinned by a test: this block
// is the listener's answer to "what did I just expose", and a slip in it
// is user-visible with nothing else to catch it.
func printSharingBlock(w io.Writer, cfg serveConfig, share *fileshare.FileShare) {
	x := capContext{cfg: cfg, share: share}
	fmt.Fprintln(w, "Sharing:")
	for _, c := range capabilities {
		if c.Describe == nil || !cfg.caps.has(c.Scope) {
			continue
		}
		c.Describe(w, x)
	}
	fmt.Fprintln(w)
}
