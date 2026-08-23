package main

import (
	"io"
	"sort"
	"testing"
	"time"

	"github.com/richlegrand/bitbang/internal/auth"
	"github.com/richlegrand/bitbang/internal/fileshare"
	"github.com/richlegrand/bitbang/internal/identity"
	"github.com/richlegrand/bitbang/internal/links"
	"github.com/richlegrand/bitbang/internal/session"
)

// Today a files-only listener cannot become a shell because it has no
// shell handler. With links that is an enforcement check, and a bug in
// it is privilege escalation -- so these gate the merge.

func allCapsConfig(t *testing.T) (serveConfig, *fileshare.FileShare, *identity.Identity) {
	t.Helper()
	cfg := serveConfig{caps: capsOf(links.ScopeShell, links.ScopeForward, links.ScopeFiles, links.ScopeProxy), filesPath: t.TempDir(), shellMaxSessions: 2, server: defaultServer}
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
	terms := links.Terms{Label: "x", Scope: scope}
	granted := terms.GrantSet(offeredScopes(cfg))
	h := buildHandlers(cfg, granted, share, nil, id, "", io.Discard, false)

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
	terms := links.Terms{Label: "s", Scope: []string{links.ScopeShell}}
	h := buildHandlers(cfg, terms.GrantSet(offeredScopes(cfg)), share, nil, id, "", io.Discard, false)
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
	terms := links.Terms{Label: "x", Scope: scope}
	h := buildHandlers(cfg, terms.GrantSet(offeredScopes(cfg)), share, nil, id, "", io.Discard, false)
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
	offered := []string{links.ScopeFiles, links.ScopeShell, links.ScopeForward, links.ScopeProxy}
	table, _, err := links.Build(entries, offered, "IDENTITY")
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
	narrow := links.Terms{Label: "contractor", Code: "C", Scope: []string{links.ScopeFiles}}
	p := peerOn(wide)
	pollPeers([]*servePeer{p}, tableWith(t, []links.Terms{narrow}), time.Now())
	if !p.q.IsClosed() {
		t.Error("a narrowed link kept its old handler set; the set cannot shrink in place")
	}
}

func TestPoll_UnchangedLinkIsLeftAlone(t *testing.T) {
	terms := links.Terms{Label: "contractor", Code: "C", Scope: []string{links.ScopeFiles}}
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
	p := peerOn(links.Terms{Label: "ana-phone", Code: "CODE1", Scope: []string{links.ScopeFiles}})
	renamed := links.Terms{Label: "ana", Code: "CODE1", Scope: []string{links.ScopeFiles}}
	pollPeers([]*servePeer{p}, tableWith(t, []links.Terms{renamed}), time.Now())
	if p.q.IsClosed() {
		t.Error("renaming a link disconnected its holder")
	}
}

// The code is the credential, so reusing a label with a different code
// is a different link and the old session must go.
func TestPoll_ReusingALabelWithANewCodeClosesTheOldSession(t *testing.T) {
	p := peerOn(links.Terms{Label: "ana", Code: "OLD", Scope: []string{links.ScopeFiles}})
	reissued := links.Terms{Label: "ana", Code: "NEW", Scope: []string{links.ScopeFiles}}
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
		x := capContext{cfg: cfg, granted: map[string]bool{
			links.ScopeShell: true, links.ScopeForward: true,
		}}
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
