package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/richlegrand/bitbang/internal/auth"
	"github.com/richlegrand/bitbang/internal/fileshare"
	"github.com/richlegrand/bitbang/internal/grant"
	"github.com/richlegrand/bitbang/internal/identity"
	"github.com/richlegrand/bitbang/internal/links"
	"github.com/richlegrand/bitbang/internal/session"
	"github.com/richlegrand/bitbang/internal/streamtype"
)

// Today a files-only listener cannot become a shell because it has no
// shell handler. With links that is an enforcement check, and a bug in
// it is privilege escalation -- so these gate the merge.

func allCapsConfig(t *testing.T) (serveConfig, *fileshare.FileShare, *identity.Identity) {
	t.Helper()
	// Built the way the CLI builds it, from a parsed grant, so the config
	// and what it offers cannot disagree.
	var cfg serveConfig
	if err := applySpec(&cfg, grant.Everything()); err != nil {
		t.Fatal(err)
	}
	cfg.filesPath = t.TempDir()
	cfg.offered.FilesPath = cfg.filesPath
	cfg.shellMaxSessions = 2
	cfg.server = defaultServer
	share, err := fileshare.New(cfg.filesPath)
	if err != nil {
		t.Fatal(err)
	}
	id, err := identity.Load("", true) // ephemeral: no files touched
	if err != nil {
		t.Fatal(err)
	}
	return cfg, share, id
}

// capsFor builds the handler set a link with the given scope would get,
// and reports the stream types it can reach. Types come from the same
// place `ready` gets its caps, which is the point of building the set
// rather than checking at stream-open time.
func capsFor(t *testing.T, scope []string) []string {
	t.Helper()
	cfg, share, id := allCapsConfig(t)
	terms := links.Terms{Label: "x", Grant: strings.Join(scope, " ")}
	granted := mustEffective(t, terms, cfg.offered)
	h := buildHandlers(cfg, granted, share, id, "", io.Discard, false)

	var caps []string
	for _, handler := range h.all {
		caps = append(caps, handler.Type())
	}
	sort.Strings(caps)
	return caps
}

func TestScope_FilesCannotReachShellTCPOrProxy(t *testing.T) {
	caps := capsFor(t, []string{links.ScopeFiles})
	for _, forbidden := range []string{"shell", "tcp", "websocket"} {
		for _, got := range caps {
			if got == forbidden {
				t.Errorf("a files-scoped link reached %q; caps=%v", forbidden, caps)
			}
		}
	}
	if !contains(caps, "file") {
		t.Errorf("a files-scoped link cannot reach files; caps=%v", caps)
	}
	// http is present because the browser UI rides on every link, but it
	// must be the local branch with no proxy behind it.
	assertNoProxyBranch(t, []string{links.ScopeFiles})
}

func TestScope_ProxyGetsBothHTTPAndWebSocket(t *testing.T) {
	caps := capsFor(t, []string{links.ScopeProxy})
	if !contains(caps, "http") || !contains(caps, "websocket") {
		t.Errorf("proxy must never be half of itself; caps=%v", caps)
	}
}

func TestScope_ShellAndForwardAreSeparable(t *testing.T) {
	shellOnly := capsFor(t, []string{links.ScopeShell})
	if !contains(shellOnly, "shell") || contains(shellOnly, "tcp") {
		t.Errorf("shell-scoped link caps=%v, want shell without tcp", shellOnly)
	}
	forwardOnly := capsFor(t, []string{links.ScopeForward})
	if !contains(forwardOnly, "tcp") || contains(forwardOnly, "shell") {
		t.Errorf("forward-scoped link caps=%v, want tcp without shell", forwardOnly)
	}
}

func TestScope_AbsentScopeIsUnchangedBehavior(t *testing.T) {
	caps := capsFor(t, nil)
	for _, want := range []string{"file", "shell", "tcp", "http", "websocket"} {
		if !contains(caps, want) {
			t.Errorf("an unscoped link lost %q; caps=%v", want, caps)
		}
	}
}

func TestScope_NotServedIsDroppedNotGranted(t *testing.T) {
	// A files-only listener with a shell-scoped link must conjure nothing.
	cfg := serveConfig{caps: capsOf(links.ScopeFiles), filesPath: t.TempDir(), server: defaultServer}
	share, err := fileshare.New(cfg.filesPath)
	if err != nil {
		t.Fatal(err)
	}
	id, err := identity.Load("", true)
	if err != nil {
		t.Fatal(err)
	}
	terms := links.Terms{Label: "s", Grant: "shell"}
	h := buildHandlers(cfg, mustEffective(t, terms, cfg.offered), share, id, "", io.Discard, false)
	for _, handler := range h.all {
		if handler.Type() == "shell" {
			t.Fatal("a shell-scoped link conjured a shell on a files-only listener")
		}
	}
	if h.shell != nil {
		t.Fatal("shell handler built despite not being served")
	}
}

// assertNoProxyBranch checks that the dispatcher for this scope has no
// proxy behind it, so a path that looks like a LAN target resolves to
// the local UI rather than reaching 192.168.x.x.
func assertNoProxyBranch(t *testing.T, scope []string) {
	t.Helper()
	cfg, share, id := allCapsConfig(t)
	terms := links.Terms{Label: "x", Grant: strings.Join(scope, " ")}
	h := buildHandlers(cfg, mustEffective(t, terms, cfg.offered), share, id, "", io.Discard, false)
	for _, handler := range h.all {
		d, ok := handler.(*httpDispatcher)
		if !ok {
			continue
		}
		if d.proxy != nil {
			t.Errorf("scope %v left the dispatcher's proxy branch wired; a LAN host is reachable", scope)
		}
		return
	}
	t.Errorf("scope %v produced no http dispatcher, so the browser UI is unreachable", scope)
}

// -- The poll: revocation reaches live sessions --

func tableWith(t *testing.T, entries []links.Terms) *links.Table {
	t.Helper()
	table, _, err := links.Build(entries, grant.Everything(), "IDENTITY")
	if err != nil {
		t.Fatal(err)
	}
	return table
}

// peerOn returns a peer that has already been granted the given terms,
// with a queue that records whether it was closed.
func peerOn(terms links.Terms) *servePeer {
	p := newServePeer("client-1")
	p.grant(terms)
	return p
}

func TestPoll_DeletedLinkClosesLiveSession(t *testing.T) {
	p := peerOn(links.Terms{Label: "contractor", Code: "C"})
	pollPeers([]*servePeer{p}, tableWith(t, nil), time.Now())
	if !p.q.IsClosed() {
		t.Error("deleting a link left its session running; revocation only blocked reconnects")
	}
}

func TestPoll_ExpiredLinkClosesLiveSession(t *testing.T) {
	past := time.Now().Add(-time.Hour)
	terms := links.Terms{Label: "contractor", Code: "C", Expires: &past}
	p := peerOn(terms)
	pollPeers([]*servePeer{p}, tableWith(t, []links.Terms{terms}), time.Now())
	if !p.q.IsClosed() {
		t.Error("an expired link's session stayed open with no file change to trigger the check")
	}
}

func TestPoll_NarrowedScopeClosesLiveSession(t *testing.T) {
	wide := links.Terms{Label: "contractor", Code: "C"}
	narrow := links.Terms{Label: "contractor", Code: "C", Grant: "files"}
	p := peerOn(wide)
	pollPeers([]*servePeer{p}, tableWith(t, []links.Terms{narrow}), time.Now())
	if !p.q.IsClosed() {
		t.Error("a narrowed link kept its old handler set; the set cannot shrink in place")
	}
}

func TestPoll_UnchangedLinkIsLeftAlone(t *testing.T) {
	terms := links.Terms{Label: "contractor", Code: "C", Grant: "files"}
	p := peerOn(terms)
	pollPeers([]*servePeer{p}, tableWith(t, []links.Terms{terms}), time.Now())
	if p.q.IsClosed() {
		t.Error("the poll closed a session whose link had not changed")
	}
}

func TestPoll_MeSurvives(t *testing.T) {
	p := peerOn(links.Terms{Label: links.OwnerLabel, Code: "IDENTITY"})
	pollPeers([]*servePeer{p}, tableWith(t, nil), time.Now())
	if p.q.IsClosed() {
		t.Error("the poll closed the operator's own session; owner must be a real row")
	}
}

func TestPoll_HandshakingPeerIsLeftAlone(t *testing.T) {
	p := newServePeer("client-2") // nothing granted yet
	pollPeers([]*servePeer{p}, tableWith(t, nil), time.Now())
	if p.q.IsClosed() {
		t.Error("the poll closed a peer that had not presented a code yet")
	}
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

// The ordering inside revoke is what an e2e run caught: sending the
// goodbye and closing the connection in one breath discards the frame
// pion still has queued, and the connector reconnects instead of being
// told why. The session must be dead at once -- that is the security
// half -- while the connection lingers just long enough for the message
// to leave.
func TestRevoke_ClosesSessionAtOnceAndConnectionAfter(t *testing.T) {
	p := peerOn(links.Terms{Label: "contractor", Code: "C"})
	sess := session.New(nil, auth.New(""), false)
	if !p.admit(sess, sessionHandlers{}) {
		t.Fatal("admit refused a live peer")
	}

	pollPeers([]*servePeer{p}, tableWith(t, nil), time.Now())

	// Immediately: the session serves nothing further.
	if !sess.Closed() {
		t.Error("session still live right after revocation; it could still open streams")
	}
	// The connection follows once the goodbye has had its moment.
	if p.q.IsClosed() {
		t.Error("connection closed synchronously, before the goodbye could leave")
	}
	// session.Goodbye waits for the frame to flush before we close.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if p.q.IsClosed() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Error("connection was never torn down after revocation")
}

// Renaming a link is a cosmetic edit and must not disconnect the person
// holding it. It used to: the poll re-resolved the label, so a rename
// looked exactly like a deletion.
func TestPoll_RenamingALinkDoesNotCloseItsSession(t *testing.T) {
	p := peerOn(links.Terms{Label: "ana-phone", Code: "CODE1", Grant: "files"})
	renamed := links.Terms{Label: "ana", Code: "CODE1", Grant: "files"}
	pollPeers([]*servePeer{p}, tableWith(t, []links.Terms{renamed}), time.Now())
	if p.q.IsClosed() {
		t.Error("renaming a link disconnected its holder")
	}
}

// The code is the credential, so reusing a label with a different code
// is a different link and the old session must go.
func TestPoll_ReusingALabelWithANewCodeClosesTheOldSession(t *testing.T) {
	p := peerOn(links.Terms{Label: "ana", Code: "OLD", Grant: "files"})
	reissued := links.Terms{Label: "ana", Code: "NEW", Grant: "files"}
	pollPeers([]*servePeer{p}, tableWith(t, []links.Terms{reissued}), time.Now())
	if !p.q.IsClosed() {
		t.Error("a session holding a retired code was left running")
	}
}

// The decision to close is made on the code; the label only chooses the
// wording. Revoked, expired, and reissued need different things from the
// person reading them.
func TestWhyGone(t *testing.T) {
	past := time.Now().Add(-time.Hour)
	cases := []struct {
		name    string
		entries []links.Terms
		want    string
	}{
		{"deleted", nil, "this link was revoked"},
		{
			"expired, code retired",
			[]links.Terms{{Label: "ana", Expires: &past}},
			"this link has expired",
		},
		{
			"reissued under the same label",
			[]links.Terms{{Label: "ana", Code: "NEW"}},
			"this link was reissued; ask for the new URL",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, _ := whyGone(tableWith(t, c.entries), "ana", time.Now())
			if got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

// The shell menu entry is suppressed only when exactly one session is
// allowed, where the launcher tab is already the only shell there can
// be. At the default it must appear -- the old default of 1 meant the
// product shipped with no way to open a second shell from the browser.
func TestCapBarShellEntryFollowsTheSessionLimit(t *testing.T) {
	cases := []struct {
		max  int
		want bool
	}{
		{defaultShellMaxSessions, true},
		{1, false},
		{2, true},
		{0, true}, // unlimited
	}
	for _, c := range cases {
		cfg := serveConfig{
			caps:             capsOf(links.ScopeShell, links.ScopeForward),
			shellMaxSessions: c.max,
		}
		x := capContext{cfg: cfg, eff: mustSpec(t, "shell forward")}
		var found bool
		for _, item := range capBarItems(x) {
			if item.Label == "Shell" {
				found = true
			}
		}
		if found != c.want {
			t.Errorf("max=%d: Shell in cap bar = %v, want %v", c.max, found, c.want)
		}
	}
}

// mustEffective resolves what a link reaches on a listener, failing the test
// rather than the session if the link asks for more than is served.
func mustEffective(t *testing.T, terms links.Terms, offered grant.Spec) grant.Spec {
	t.Helper()
	eff, err := terms.Effective(offered)
	if err != nil {
		t.Fatalf("Effective: %v", err)
	}
	return eff
}

// -- Narrowing past the capability word --
//
// A grant is written in the words `serve` takes, so a link can hand out
// one of several forward targets, one of several proxy targets, or a
// subdirectory of the share. These assert the narrowed spec reaches the
// handlers, not just the decision to build them: the earlier gate is
// which capabilities exist, and it would pass while every one of these
// still handed over the listener's full reach.

// narrowedHandlers builds what a link with the given grant gets on a
// listener started with the given one.
func narrowedHandlers(t *testing.T, listener, link string) sessionHandlers {
	t.Helper()
	var cfg serveConfig
	if err := applySpec(&cfg, mustSpec(t, listener)); err != nil {
		t.Fatal(err)
	}
	cfg.shellMaxSessions = defaultShellMaxSessions
	cfg.server = defaultServer

	var share *fileshare.FileShare
	eff := mustEffective(t, links.Terms{Label: "x", Grant: link}, cfg.offered)
	if eff.FilesPath != "" {
		s, err := fileshare.New(eff.FilesPath)
		if err != nil {
			t.Fatal(err)
		}
		share = s
	}
	id, err := identity.Load("", true)
	if err != nil {
		t.Fatal(err)
	}
	return buildHandlers(cfg, eff, share, id, "", io.Discard, false)
}

func TestNarrow_ForwardTargetsReachTheTCPHandler(t *testing.T) {
	h := narrowedHandlers(t, "forward 127.0.0.1:22,db.internal:5432", "forward db.internal:5432")
	if h.tcp == nil {
		t.Fatal("no tcp handler")
	}
	if h.tcp.Allow.PermitsTarget("127.0.0.1:22") {
		t.Error("a link narrowed to db.internal still dials 127.0.0.1:22")
	}
	if !h.tcp.Allow.PermitsTarget("db.internal:5432") {
		t.Error("a link narrowed to db.internal cannot dial it")
	}
}

func TestNarrow_AnUnnarrowedLinkKeepsTheListenersTargets(t *testing.T) {
	h := narrowedHandlers(t, "forward 127.0.0.1:22,db.internal:5432", "forward")
	for _, target := range []string{"127.0.0.1:22", "db.internal:5432"} {
		if !h.tcp.Allow.PermitsTarget(target) {
			t.Errorf("naming no target dropped %s; a link that narrows nothing must narrow nothing", target)
		}
	}
	// Still the listener's list and not the whole network.
	if h.tcp.Allow.Empty() {
		t.Error("the link inherited an empty allowlist, which reaches every host the listener can")
	}
}

func TestNarrow_ProxyTargetsReachTheHandlerAndTheCaret(t *testing.T) {
	h := narrowedHandlers(t, "files proxy nas.lan:8096,pi.lan:80", "files proxy pi.lan:80")
	var proxied *streamtype.HTTPHandler
	for _, handler := range h.all {
		d, ok := handler.(*httpDispatcher)
		if !ok || d.proxy == nil {
			continue
		}
		if proxied, ok = d.proxy.(*streamtype.HTTPHandler); !ok {
			t.Fatalf("dispatcher proxy is %T, not an HTTP proxy", d.proxy)
		}
	}
	if proxied == nil {
		t.Fatal("no proxy behind the dispatcher")
	}
	if proxied.Allow.PermitsTarget("nas.lan:8096") {
		t.Error("a link narrowed to pi.lan still proxies to nas.lan")
	}
	if !proxied.Allow.PermitsTarget("pi.lan:80") {
		t.Error("a link narrowed to pi.lan cannot reach it")
	}

	// The caret has to agree with the gate. Offering nas.lan in a menu
	// whose every request to it is refused is worse than not offering it.
	page := renderProxyPage(t, "files proxy nas.lan:8096,pi.lan:80", "files proxy pi.lan:80")
	if strings.Contains(page, "nas.lan") {
		t.Error("the caret still offers nas.lan to a link narrowed away from it")
	}
	if !strings.Contains(page, "/proxy/pi.lan:80/") {
		t.Errorf("the caret does not offer pi.lan, the one target the link has:\n%s", page)
	}
}

// renderProxyPage fetches the proxy page a link would see, cap bar and all.
func renderProxyPage(t *testing.T, listener, link string) string {
	t.Helper()
	var cfg serveConfig
	if err := applySpec(&cfg, mustSpec(t, listener)); err != nil {
		t.Fatal(err)
	}
	cfg.shellMaxSessions = defaultShellMaxSessions
	cfg.server = defaultServer
	eff := mustEffective(t, links.Terms{Label: "x", Grant: link}, cfg.offered)
	share, err := fileshare.New(eff.FilesPath)
	if err != nil {
		t.Fatal(err)
	}
	h := buildServeHTTPHandler(capContext{cfg: cfg, share: share, eff: eff})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/proxy/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /proxy/ = %d", rec.Code)
	}
	return rec.Body.String()
}

func TestNarrow_FilesPathIsTheSubdirectory(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "public")
	if err := os.MkdirAll(filepath.Join(sub, "inner"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "secret"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	eff := mustEffective(t, links.Terms{Label: "x", Grant: "files " + sub},
		mustSpec(t, "files "+root))
	if eff.FilesPath != sub {
		t.Fatalf("FilesPath = %q, want the subdirectory %q", eff.FilesPath, sub)
	}
	// The share is rooted at the subdirectory, so the sibling file is not
	// merely hidden from the listing -- it is outside the root.
	share, err := fileshare.New(eff.FilesPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, escape := range []string{"secret", "../secret"} {
		if _, err := share.StatPath(escape); err == nil {
			t.Errorf("a link narrowed to a subdirectory still reads %q beside it", escape)
		}
	}
	if _, err := share.StatPath("inner"); err != nil {
		t.Errorf("the narrowed share cannot see its own contents: %v", err)
	}
}
