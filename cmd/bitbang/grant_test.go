package main

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/richlegrand/bitbang/internal/grant"
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

// mustSpec parses a listener grant for a test, in the words `serve` takes.
func mustSpec(t *testing.T, s string) grant.Spec {
	t.Helper()
	spec, err := grant.ParseString(s)
	if err != nil {
		t.Fatalf("ParseString(%q): %v", s, err)
	}
	return spec
}

// The prompt takes the same words as the command line. That is the point:
// an operator who has typed a listener command already knows this one, and
// a mistake reads exactly as it would there.
func TestGrantQuestions_TakesTheSameWords(t *testing.T) {
	a := &scriptedAsker{t: t, answers: []string{
		"forward 127.0.0.1:22", // the grant
		"2",                    // never / 1h / 24h / 7d / other
		"ana-laptop",
	}}
	got, err := grantQuestions(a, links.Terms{}, mustSpec(t, "shell forward 127.0.0.1:22,db:5432"),
		nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if got.Grant != "forward 127.0.0.1:22" {
		t.Errorf("Grant = %q, want the words back", got.Grant)
	}
}

// Enter grants everything the listener offers, which is not the same as
// naming all four -- a two-capability listener would reject that.
func TestGrantQuestions_EnterGrantsWhatIsOffered(t *testing.T) {
	a := &scriptedAsker{t: t, answers: []string{"", "1", "kiosk"}}
	got, err := grantQuestions(a, links.Terms{}, mustSpec(t, "files /srv"), nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if got.Grant != "" {
		t.Errorf("Grant = %q, want empty for everything-offered", got.Grant)
	}
}

// A grant reaching past the listener is refused at the prompt rather than
// minted as a link that silently reaches nothing.
func TestGrantQuestions_RefusesWideningAndAsksAgain(t *testing.T) {
	a := &scriptedAsker{t: t, answers: []string{
		"shell",               // not served
		"forward 10.0.0.9:22", // outside the listener's targets
		"forward 127.0.0.1:22",
		"1", "ok",
	}}
	got, err := grantQuestions(a, links.Terms{}, mustSpec(t, "forward 127.0.0.1:22"), nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if got.Grant != "forward 127.0.0.1:22" {
		t.Errorf("Grant = %q", got.Grant)
	}
	said := strings.Join(a.said, "\n")
	if !strings.Contains(said, "does not serve shell") {
		t.Errorf("no complaint about the unserved capability:\n%s", said)
	}
	if !strings.Contains(said, "outside what this listener reaches") {
		t.Errorf("no complaint about the out-of-range target:\n%s", said)
	}
}

// The listener's own grant is shown above the prompt, so the operator can
// see the ceiling they are narrowing -- and a pinned listener gets the
// credit for having been pinned.
func TestGrantQuestions_ShowsWhatTheListenerServes(t *testing.T) {
	a := &scriptedAsker{t: t, answers: []string{"", "1", "x"}}
	_, err := grantQuestions(a, links.Terms{},
		mustSpec(t, "files /srv/share proxy nas.lan:8096 forward 127.0.0.1:22"), nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	said := strings.Join(a.said, "\n")
	for _, want := range []string{"/srv/share", "nas.lan:8096", "127.0.0.1:22"} {
		if !strings.Contains(said, want) {
			t.Errorf("prompt does not show %q:\n%s", want, said)
		}
	}
}
