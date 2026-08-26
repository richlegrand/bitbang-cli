package main

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/richlegrand/bitbang/internal/allowlist"
	"github.com/richlegrand/bitbang/internal/links"
)

// scriptedAsker answers questions from a list, so the flow can be
// exercised without a terminal. An empty answer means "press Enter",
// which takes the default.
type scriptedAsker struct {
	answers []string
	asked   []string
	said    []string
	t       *testing.T
}

func (s *scriptedAsker) Ask(prompt, def string) (string, error) {
	s.asked = append(s.asked, prompt+" ["+def+"]")
	if len(s.answers) == 0 {
		s.t.Fatalf("ran out of scripted answers at %q; asked so far: %v", prompt, s.asked)
	}
	a := s.answers[0]
	s.answers = s.answers[1:]
	if a == "" {
		return def, nil
	}
	return a, nil
}

func (s *scriptedAsker) Say(format string, args ...interface{}) {
	s.said = append(s.said, fmt.Sprintf(format, args...))
}

var allScopes = []string{links.ScopeFiles, links.ScopeShell, links.ScopeForward, links.ScopeProxy}

func TestGrantQuestions_PickingScopesAndAnExpiry(t *testing.T) {
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	a := &scriptedAsker{t: t, answers: []string{
		"1,3", // files and forward
		"3",   // 24 hours; the menu is never, 1h, 24h, 7d, other
		"ana-phone",
	}}
	got, err := grantQuestions(a, links.Terms{}, allScopes, nil, now, scopeReach{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got.Scope, ",") != "files,forward" {
		t.Errorf("scope = %v, want files,forward", got.Scope)
	}
	if got.Expires == nil || !got.Expires.Equal(now.Add(24*time.Hour)) {
		t.Errorf("expires = %v, want 24h out", got.Expires)
	}
	if got.Label != "ana-phone" {
		t.Errorf("label = %q", got.Label)
	}
}

// Enter through everything: the seed comes back unchanged. This is what
// makes `edit` reuse the flow rather than needing its own.
func TestGrantQuestions_EnterThroughKeepsTheSeed(t *testing.T) {
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	at := now.Add(22 * time.Hour)
	seed := links.Terms{Label: "ana", Code: "KEEPME", Scope: []string{links.ScopeFiles}, Expires: &at}

	a := &scriptedAsker{t: t, answers: []string{"", "", ""}}
	got, err := grantQuestions(a, seed, allScopes, nil, now, scopeReach{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got.Scope, ",") != "files" {
		t.Errorf("scope changed to %v", got.Scope)
	}
	if got.Expires == nil || !got.Expires.Equal(at) {
		t.Errorf("expiry changed to %v", got.Expires)
	}
	if got.Label != "ana" || got.Code != "KEEPME" {
		t.Errorf("label or code changed: %q %q", got.Label, got.Code)
	}
}

// The default grant is everything and never, and `a` is how the flow
// expresses "everything" -- a nil scope, same as an unscoped link.
func TestGrantQuestions_AllAndNever(t *testing.T) {
	now := time.Now()
	a := &scriptedAsker{t: t, answers: []string{"a", "1", "kiosk"}}
	got, err := grantQuestions(a, links.Terms{}, allScopes, nil, now, scopeReach{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Scope != nil {
		t.Errorf("scope = %v, want nil meaning everything", got.Scope)
	}
	if got.Expires != nil {
		t.Errorf("expires = %v, want never", got.Expires)
	}
}

func TestGrantQuestions_RejectsBadInputAndAsksAgain(t *testing.T) {
	now := time.Now()
	a := &scriptedAsker{t: t, answers: []string{
		"9",     // out of range
		"1",     // files
		"99",    // out of range
		"1",     // never
		"owner", // reserved label
		"   ",   // whitespace; "" would mean Enter and take the default
		"ok",    // fine
	}}
	got, err := grantQuestions(a, links.Terms{}, allScopes, nil, now, scopeReach{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Label != "ok" {
		t.Errorf("label = %q", got.Label)
	}
	joined := strings.Join(a.said, "\n")
	for _, want := range []string{"not one of 1-4", "pick 1-5", "reserved", "how you refer to this link"} {
		if !strings.Contains(joined, want) {
			t.Errorf("no message containing %q; got:\n%s", want, joined)
		}
	}
}

// The scope menu only offers what the listener serves, so it can never
// present a choice that would grant nothing.
func TestGrantQuestions_OnlyOffersWhatIsServed(t *testing.T) {
	now := time.Now()
	a := &scriptedAsker{t: t, answers: []string{"1", "1", "x"}}
	got, err := grantQuestions(a, links.Terms{}, []string{links.ScopeFiles}, nil, now, scopeReach{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got.Scope, ",") != "files" {
		t.Errorf("scope = %v", got.Scope)
	}
	if strings.Contains(strings.Join(a.said, "\n"), "shell") {
		t.Error("offered a scope this listener does not serve")
	}
}

func TestParseWhen(t *testing.T) {
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		in   string
		want time.Duration
		bad  bool
	}{
		{in: "90m", want: 90 * time.Minute},
		{in: "1h30m", want: 90 * time.Minute},
		// d and w are the extensions time.ParseDuration lacks, and the
		// docs use them.
		{in: "3d", want: 72 * time.Hour},
		{in: "2w", want: 14 * 24 * time.Hour},
		{in: "1w3d", want: 10 * 24 * time.Hour},
		{in: "3d12h", want: 84 * time.Hour},
		{in: "", bad: true},
		{in: "soon", bad: true},
		{in: "-3d", bad: true},
		{in: "2020-01-01", bad: true}, // already past
	}
	for _, c := range cases {
		got, err := parseWhen(c.in, now)
		if c.bad {
			if err == nil {
				t.Errorf("parseWhen(%q) = %v, want an error", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseWhen(%q): %v", c.in, err)
			continue
		}
		if !got.Equal(now.Add(c.want)) {
			t.Errorf("parseWhen(%q) = %v, want %v out", c.in, got, c.want)
		}
	}

	if got, err := parseWhen("2026-09-15", now); err != nil || got.Day() != 15 {
		t.Errorf("a bare date did not resolve: %v %v", got, err)
	}
}

// Free text is confirmed by showing the resolved date, because 2w is easy
// to misread. Saying no returns to the menu rather than accepting it.
func TestAskOtherExpiry_ConfirmsAndCanBeDeclined(t *testing.T) {
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)

	a := &scriptedAsker{t: t, answers: []string{"3d", "y"}}
	at, err := askOtherExpiry(a, now)
	if err != nil || at == nil || !at.Equal(now.Add(72*time.Hour)) {
		t.Fatalf("accepted path: got %v %v", at, err)
	}
	if !strings.Contains(strings.Join(a.asked, "\n"), "in 3 days") {
		t.Errorf("the resolved date was not shown back: %v", a.asked)
	}

	a = &scriptedAsker{t: t, answers: []string{"3d", "n"}}
	at, err = askOtherExpiry(a, now)
	if err != nil || at != nil {
		t.Errorf("declining should return to the menu, got %v %v", at, err)
	}
}

// The menu is ordered least powerful first, so a mis-keyed 1 grants a
// file browser rather than a terminal.
func TestGrantQuestions_MenuPutsShellLast(t *testing.T) {
	a := &scriptedAsker{t: t, answers: []string{"1", "1", "x"}}
	got, err := grantQuestions(a, links.Terms{}, allScopes, nil, time.Now(), scopeReach{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got.Scope, ",") != "files" {
		t.Errorf("option 1 granted %v, want files", got.Scope)
	}
	menu := strings.Join(a.said, "\n")
	files, shell := strings.Index(menu, "files"), strings.Index(menu, "shell")
	if files < 0 || shell < 0 || files > shell {
		t.Errorf("shell is not last in the menu:\n%s", menu)
	}
}

// A label already in use is refused inside the question, not after the
// whole flow. Being told at the write means answering scopes and expiry
// again for a name you could have been warned about immediately.
func TestGrantQuestionsRefusesATakenLabel(t *testing.T) {
	now := time.Now()
	a := &scriptedAsker{t: t, answers: []string{
		"a",          // scopes: all
		"1",          // expires: never
		"contractor", // taken
		"ana",        // free
	}}
	taken := map[string]bool{"contractor": true}
	got, err := grantQuestions(a, links.Terms{}, allScopes, taken, now, scopeReach{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Label != "ana" {
		t.Errorf("label = %q, want the free one", got.Label)
	}
}

// Editing an entry without renaming it must stay legal, so the entry
// being edited is excluded from the taken set by its caller.
func TestGrantQuestionsAcceptsTheSeedsOwnLabel(t *testing.T) {
	now := time.Now()
	a := &scriptedAsker{t: t, answers: []string{"a", "1", ""}} // Enter through
	seed := links.Terms{Label: "contractor"}
	got, err := grantQuestions(a, seed, allScopes, map[string]bool{"ana": true}, now, scopeReach{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Label != "contractor" {
		t.Errorf("label = %q, want the seed kept", got.Label)
	}
}

// The menu is where an operator decides what to hand out, so `forward` has to
// say what it reaches there. It used to say only "without a shell", which
// reads as the narrow option while being the widest one on an unrestricted
// listener.
func TestScopeMenuSaysWhatForwardReaches(t *testing.T) {
	a := &scriptedAsker{t: t, answers: []string{"3", "1", "label-a"}}
	_, _ = grantQuestions(a, links.Terms{}, allScopes, nil, time.Now(), scopeReach{})
	menu := strings.Join(a.said, "\n")
	if !strings.Contains(menu, "any host on this network") {
		t.Errorf("unrestricted forward does not say what it reaches:\n%s", menu)
	}
}

// And when the listener is pinned, the menu should say so -- that is what
// makes -allow-forward visible at the moment it matters.
func TestScopeMenuNamesTheAllowlist(t *testing.T) {
	a := &scriptedAsker{t: t, answers: []string{"3", "1", "label-b"}}
	_, _ = grantQuestions(a, links.Terms{}, allScopes, nil, time.Now(),
		scopeReach{forward: "127.0.0.1:22", proxy: "nas.lan:8080"})
	menu := strings.Join(a.said, "\n")
	if !strings.Contains(menu, "forward TCP to 127.0.0.1:22") {
		t.Errorf("menu does not name the forward allowlist:\n%s", menu)
	}
	if !strings.Contains(menu, "reach nas.lan:8080") {
		t.Errorf("menu does not name the proxy target:\n%s", menu)
	}
	if strings.Contains(menu, "any host on this network") {
		t.Errorf("menu still claims unrestricted reach on a pinned listener:\n%s", menu)
	}
}

// reachOf reads the listener config, and a pinned -target is a restriction
// even though it is not an allowlist.
func TestReachOfPrefersPinnedTarget(t *testing.T) {
	allow, err := allowlist.Parse([]string{"a.lan:1", "b.lan"})
	if err != nil {
		t.Fatal(err)
	}
	r := reachOf(serveConfig{allowForward: allow, target: "127.0.0.1:8096"})
	if r.forward != "a.lan:1, b.lan:*" {
		t.Errorf("forward reach = %q", r.forward)
	}
	if r.proxy != "127.0.0.1:8096" {
		t.Errorf("proxy reach = %q, want the pinned target", r.proxy)
	}
	if got := reachOf(serveConfig{}).forward; got != "" {
		t.Errorf("unrestricted forward reach = %q, want empty", got)
	}
}
