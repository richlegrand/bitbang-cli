package main

import (
	"github.com/richlegrand/bitbang/internal/capbar"
	"strings"

	"fmt"
	"io"
	"net/http"

	"github.com/richlegrand/bitbang/internal/allowlist"
	"github.com/richlegrand/bitbang/internal/fileshare"
	"github.com/richlegrand/bitbang/internal/grant"
	"github.com/richlegrand/bitbang/internal/identity"
	"github.com/richlegrand/bitbang/internal/links"
	"github.com/richlegrand/bitbang/internal/proxyweb"
	"github.com/richlegrand/bitbang/internal/shellweb"
	"github.com/richlegrand/bitbang/internal/streamtype"
)

// capSet is what a listener offers, named in the scope vocabulary.
type capSet map[string]bool

func capsOf(names ...string) capSet {
	s := make(capSet, len(names))
	for _, n := range names {
		s[n] = true
	}
	return s
}

func (c capSet) has(name string) bool { return c[name] }

// capContext is everything a capability needs to make itself real for one
// peer: the config it was started with, and the per-connection facts.
type capContext struct {
	cfg       serveConfig
	share     *fileshare.FileShare
	id        *identity.Identity
	browserIP string
	// mirror is where shell output is echoed for the operator, held while
	// a prompt is on screen.
	mirror io.Writer
	// eff is what this peer's link actually reaches: the listener's grant
	// narrowed by the link's. It decides which capabilities contribute and
	// what each of them is pointed at, so a link can hand out one forward
	// target, one proxy, or a subdirectory without a second mechanism.
	eff grant.Spec
	// owner is true when this peer presented the device's own code
	// rather than a link. Only shell displacement reads it.
	owner bool
	// capBar is the strip every cap page shows when this link grants
	// more than one of them. Empty for a single-capability link, which
	// has nowhere to move between.
	capBar []capbar.Item
}

func (x capContext) offers(scope string) bool { return x.cfg.caps.has(scope) }

// A capability is one thing a link can grant. Scope is the permanent part
// -- those names live in config files people keep -- and the rest is how
// this listener implements it today.
//
// Before this table the same set was spelled out six times: config
// booleans, handler Type() strings, scope names, mux mounts, cap-bar
// items, and the Sharing block. They did not agree on cardinality, and
// adding per-link scope meant hand-writing the mapping between all of
// them. Everything now derives from one list.
//
// Two things are deliberately not capabilities. The listener's own browser
// UI is the frame the others render in, not something a link is scoped to,
// so it rides on every link and shows only what that link grants. And
// websocket is not its own entry: it belongs to proxy's Build, which is
// what stops anyone granting http without it and producing a proxy that
// half works.
type capability struct {
	Scope string

	// Build contributes this capability's stream handlers, or nothing when
	// the link does not reach it.
	Build func(capContext) []streamtype.StreamHandler

	// Mount is where this capability's UI hangs in the all-mode mux, empty
	// for a capability with no browser side. Web builds that UI.
	Mount string
	Web   func(capContext) http.Handler

	// Menu is the cap-bar label, empty for no entry, and MenuPath is where
	// it points -- shell's is "/" because the launcher tab is the shell.
	// MenuWhen, when set, suppresses the entry in configurations where it
	// would be useless.
	Menu     string
	MenuPath string
	MenuWhen func(capContext) bool

	// Describe writes this capability's line (or lines) in the Sharing
	// block, given a listener that offers it.
	Describe func(io.Writer, capContext)
}

// defaultShellMaxSessions caps concurrent shells where real use will
// never reach it, which is the job a limit like this should do. sshd's
// MaxSessions defaults to 10 for the same reason.
//
// It was 1, which people met constantly: with displacement, opening a
// second tab silently killed the shell in the first. Worse once an
// owner's shell became undisplaceable -- a single forgotten session
// locked every guest out entirely. Unlimited is not the answer either;
// there is no auth throttle here, so a reconnect loop or a hostile URL
// holder could spawn PTYs without bound on a small device.
const defaultShellMaxSessions = 10

// capabilities is the whole vocabulary, in the order things are presented.
var capabilities = []capability{
	{
		Scope: links.ScopeShell,
		Build: func(x capContext) []streamtype.StreamHandler {
			// A command named after `shell` -- by the listener or by the
			// link narrowing it -- is the command that runs. NewShell pins
			// it, which also ignores the client's env and cwd, since any of
			// those can steer a pinned command as surely as argv can.
			sh := streamtype.NewShell(x.eff.ShellArgv, x.cfg.verbose)
			sh.MaxConcurrent = x.cfg.shellMaxSessions
			sh.OwnerCredential = x.owner
			if !x.cfg.disableShellMirror {
				// Through the hold, not straight to the terminal: the
				// mirror is the loudest thing on the console and would
				// otherwise scroll a prompt away mid-question.
				sh.StdoutMirror = x.mirror
				sh.StderrMirror = x.mirror
			}
			return []streamtype.StreamHandler{sh}
		},
		Mount:    "/shell/",
		Web:      func(capContext) http.Handler { return shellweb.New().HTTPHandler() },
		Menu:     "Shell",
		MenuPath: "/",
		// With one session allowed, the launcher tab IS the only shell and
		// offering another would just hit the limit. Only reachable now by
		// setting -shell-max-sessions 1 explicitly.
		MenuWhen: func(x capContext) bool { return x.cfg.shellMaxSessions != 1 },
		Describe: describeShell,
	},
	{
		Scope: links.ScopeForward,
		Build: func(x capContext) []streamtype.StreamHandler {
			return []streamtype.StreamHandler{streamtype.NewTCP(x.cfg.verbose, allowOf(x.eff.ForwardTargets))}
		},
		// No Mount and no Menu: forwarding is driven by `connect -L`, and
		// there is nothing for a browser to show.
		Describe: describeForward,
	},
	{
		Scope: links.ScopeFiles,
		Build: func(x capContext) []streamtype.StreamHandler {
			if x.share == nil {
				return nil
			}
			return []streamtype.StreamHandler{streamtype.NewFile(x.share, x.cfg.verbose)}
		},
		Mount: "/files/",
		Web: func(x capContext) http.Handler {
			if x.share == nil {
				return nil
			}
			x.share.CapBar(x.capBar)
			return x.share.HTTPHandler()
		},
		Menu:     "Files",
		MenuPath: "/files/",
		MenuWhen: func(x capContext) bool { return x.share != nil },
		Describe: describeFiles,
	},
	{
		Scope: links.ScopeProxy,
		Build: buildProxyHandlers,
		Mount: "/proxy/",
		// eff, not cfg, so the page is built from what this link reaches.
		// The two agree on emptiness today, which is all the landing page
		// asks of the list -- but reading the listener's grant on a
		// per-link page is how that stops being true.
		Web:      func(x capContext) http.Handler { return proxyweb.LandingHandler(x.capBar, x.eff.ProxyTargets) },
		Menu:     "Proxy",
		MenuPath: "/proxy/",
		Describe: describeProxy,
	},
}

// buildProxyHandlers is the one Build that branches, because proxy means
// two different things depending on how the listener was started.
//
// With a fixed target there is no dispatcher and no local UI: `http` IS
// the proxy, so this returns it directly along with the websocket handler
// that has to resolve to the same target. In dispatcher mode it returns
// only websocket; the http half is the dispatcher's proxy branch, wired by
// buildHandlers because the dispatcher is not a capability.
func buildProxyHandlers(x capContext) []streamtype.StreamHandler {
	if !fixedTargetMode(x.cfg) {
		p := dynamicProxy(x)
		return []streamtype.StreamHandler{streamtype.NewWebSocket(p, "", x.cfg.verbose)}
	}
	// Only forward the client IP when explicitly enabled (the backend
	// trusts localhost for auth); otherwise withhold it so requests look
	// local and don't trip an external-access warning.
	xffIP := ""
	if x.cfg.proxyClientIP {
		xffIP = x.browserIP
	}
	p := streamtype.NewHTTPProxy(x.cfg.target, x.id.UID, x.cfg.server, xffIP, x.cfg.verbose)
	p.Allow = allowOf(x.eff.ProxyTargets)
	return []streamtype.StreamHandler{p, streamtype.NewWebSocket(p, xffIP, x.cfg.verbose)}
}

// dynamicProxy builds the dynamic-target proxy. It withholds browser_ip so
// we DON'T inject XFF: this mode proxies arbitrary LAN apps that may rely
// on requests appearing local, and silently forwarding the real IP could
// break their access control. Fixed-target mode passes it -- there the
// backend is known.
func dynamicProxy(x capContext) *streamtype.HTTPHandler {
	p := streamtype.NewHTTPProxy("", x.id.UID, x.cfg.server, "", x.cfg.verbose)
	p.Allow = allowOf(x.eff.ProxyTargets)
	return p
}

// fixedTargetMode reports the proxy-only-with-a-target configuration (e.g.
// the OctoPrint plugin): every request goes straight to --target, so the
// plain device URL serves the app directly, with no dispatcher and no
// landing page.
func fixedTargetMode(cfg serveConfig) bool {
	return cfg.caps.has(links.ScopeProxy) && cfg.target != "" &&
		!cfg.caps.has(links.ScopeShell) && !cfg.caps.has(links.ScopeFiles)
}

// -- Sharing block descriptions --

func describeShell(w io.Writer, x capContext) {
	line := "  • shell  ("
	if len(x.cfg.shellArgv) > 0 {
		// "only", always: a named command is the command, so the operator
		// reading this block should not have to wonder whether a connector
		// can ask for something else.
		line += strings.Join(x.cfg.shellArgv, " ") + " only"
	} else {
		line += defaultShellLabel()
	}
	// State the limit only when it is not the default -- otherwise every
	// listener carries a number nobody chose.
	if x.cfg.shellMaxSessions == 0 {
		line += ", unlimited concurrent sessions"
	} else if x.cfg.shellMaxSessions != defaultShellMaxSessions {
		line += fmt.Sprintf(", max %d concurrent sessions", x.cfg.shellMaxSessions)
	}
	if !x.cfg.disableShellMirror {
		line += ", mirroring to console"
	}
	line += ")"
	fmt.Fprintln(w, line)
}

func describeForward(w io.Writer, x capContext) {
	reach := "unrestricted targets"
	if allow := allowOf(x.cfg.offered.ForwardTargets); !allow.Empty() {
		reach = allow.String()
	}
	fmt.Fprintf(w, "  • forward (%s, chosen by connect -L; max %d concurrent connections per session; loopback-bound on connector by default)\n",
		reach, streamtype.DefaultTCPMaxConcurrent)
}

func describeFiles(w io.Writer, x capContext) {
	if x.share == nil {
		return
	}
	if x.share.Mode == fileshare.ModeSend {
		fmt.Fprintf(w, "  • files  (%s — single file)\n", x.share.FileName)
		return
	}
	line := fmt.Sprintf("  • files  (%s", x.share.BasePath)
	if x.share.UploadEnabled {
		line += ", uploads enabled"
	}
	line += ")"
	fmt.Fprintln(w, line)
}

func describeProxy(w io.Writer, x capContext) {
	if x.cfg.target != "" {
		fmt.Fprintf(w, "  • proxy  (%s)\n", x.cfg.target)
		return
	}
	if allow := allowOf(x.cfg.offered.ProxyTargets); !allow.Empty() {
		fmt.Fprintf(w, "  • proxy  (target chosen in browser, from %s)\n", allow)
		return
	}
	fmt.Fprintln(w, "  • proxy  (target chosen in browser)")
}

// allowOf turns a narrowed grant's targets into the allowlist a handler
// enforces. The targets were validated when the grant was parsed, so a
// parse error here cannot happen -- and an empty list means unrestricted,
// which is what an unpinned listener already offered.
func allowOf(targets []string) allowlist.List {
	l, _ := allowlist.Parse(targets)
	return l
}
