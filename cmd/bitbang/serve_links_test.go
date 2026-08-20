package main

import (
	"strings"
	"testing"
	"time"

	"github.com/richlegrand/bitbang/internal/links"
)

func TestExpiryNote(t *testing.T) {
	now := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	at := func(d time.Duration) links.Terms {
		e := now.Add(d)
		return links.Terms{Label: "x", Expires: &e}
	}
	cases := []struct {
		name  string
		terms links.Terms
		want  string
	}{
		{"no expiry", links.Terms{Label: "x"}, ""},
		// Rounding up: a link minted with a six-day expiry is a few
		// milliseconds under six days for its whole life, and truncating
		// would show 5d from the moment it was created.
		{"six days", at(6*24*time.Hour - time.Second), "expires in 6d"},
		{"just over a day", at(25 * time.Hour), "expires in 2d"},
		{"hours", at(90 * time.Minute), "expires in 2h"},
		{"minutes", at(90 * time.Second), "expires in 2m"},
		{"seconds", at(30 * time.Second), "expires shortly"},
		{"already gone", at(-time.Hour), "expired"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := expiryNote(c.terms, now); got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

// A listener with no links.json must print nothing extra: the URL banner
// above has already said everything, and today's behavior is one code
// granting everything served.
func TestListing_SilentWithOnlyTheImplicitRow(t *testing.T) {
	ls := &linkState{
		offered: []string{links.ScopeFiles},
		code:    "CODE",
		codeURL: func(code string, flags ...string) string { return "https://x/uid#" + code },
	}
	table, _, err := links.Build(nil, ls.offered, ls.code)
	if err != nil {
		t.Fatal(err)
	}
	ls.table = table
	if got := ls.listing("", ""); got != "" {
		t.Errorf("listing = %q, want nothing when there is no table", got)
	}
}

func TestListing_ShowsEveryRowWithItsURL(t *testing.T) {
	past := time.Now().Add(-time.Hour)
	ls := &linkState{
		offered: []string{links.ScopeFiles, links.ScopeShell},
		code:    "OWNERCODE",
		codeURL: func(code string, flags ...string) string { return "https://x/uid#" + code },
	}
	table, _, err := links.Build([]links.Terms{
		{Label: "contractor", Code: "CONTRACTOR", Scope: []string{links.ScopeFiles}},
		{Label: "photographer", Code: "PHOTO", Scope: []string{links.ScopeFiles}, Expires: &past},
	}, ls.offered, ls.code)
	if err != nil {
		t.Fatal(err)
	}
	ls.table = table

	got := ls.listing("", "")
	for _, want := range []string{
		"owner", "#OWNERCODE",
		"contractor", "#CONTRACTOR",
		// An expired row prints, marked: dropping it looks like the file
		// failed to load.
		"photographer", "expired", "#PHOTO",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("listing missing %q:\n%s", want, got)
		}
	}
}
