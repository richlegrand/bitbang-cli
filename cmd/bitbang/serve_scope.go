package main

import (
	"fmt"
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
	shellArgv []string, id *identity.Identity, browserIP string, mirror io.Writer) sessionHandlers {

	x := capContext{cfg: cfg, share: share, shellArgv: shellArgv, id: id,
		browserIP: browserIP, granted: granted, mirror: mirror}

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
		return http.HandlerFunc(http.NotFound)
	}
	if len(live) == 1 {
		return live[0].handler
	}

	// The strip's dropdown anchors postMessage parent to open new tabs
	// (bootstrap.js handles the URL composition).
	launcher := shellweb.New(shellweb.WithCapBar(capBarItems(x))).HTTPHandler()

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

// capBarItems composes the launcher dropdown from the capabilities this
// link reaches. Items render in table order.
func capBarItems(x capContext) []shellweb.CapBarItem {
	var items []shellweb.CapBarItem
	for _, c := range capabilities {
		if c.Menu == "" || !x.reaches(c.Scope) {
			continue
		}
		if c.MenuWhen != nil && !c.MenuWhen(x) {
			continue
		}
		items = append(items, shellweb.CapBarItem{Label: c.Menu, Path: c.MenuPath})
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
