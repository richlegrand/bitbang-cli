package main

import (
	"fmt"
	"strings"
	"testing"
	"time"

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
	got, err := grantQuestions(a, links.Terms{}, allScopes, now)
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
	got, err := grantQuestions(a, seed, allScopes, now)
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
	got, err := grantQuestions(a, links.Terms{}, allScopes, now)
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
		"9",   // out of range
		"1",   // files
		"99",  // out of range
		"1",   // never
		"me",  // reserved label
		"   ", // whitespace; "" would mean Enter and take the default
		"ok",  // fine
	}}
	got, err := grantQuestions(a, links.Terms{}, allScopes, now)
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
	got, err := grantQuestions(a, links.Terms{}, []string{links.ScopeFiles}, now)
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
	got, err := grantQuestions(a, links.Terms{}, allScopes, time.Now())
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
