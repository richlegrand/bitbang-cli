package main

import (
	"strings"
	"testing"
	"time"

	"github.com/richlegrand/bitbang/internal/links"
)

// `link ls` renders a link's grant as written in the file, arguments and
// all. It runs without a listener, so it is often where someone checks
// what they wrote -- and the whole reason a grant is one string is that it
// reads the same everywhere it is shown.
func TestGrantOf(t *testing.T) {
	for _, tc := range []struct {
		name  string
		terms links.Terms
		want  string
	}{
		{"a capability alone", links.Terms{Grant: "files"}, "files"},
		{"an argument survives", links.Terms{Grant: "files /srv/photos"}, "files /srv/photos"},
		{"a target list survives", links.Terms{Grant: "forward a:22,b:80"}, "forward a:22,b:80"},
		{"a pinned command survives", links.Terms{Grant: "shell tmux attach"}, "shell tmux attach"},
		// An empty grant is not an empty link: nothing here knows what the
		// listener serves, so saying "everything served" is the honest
		// rendering, and printing nothing would read as "reaches nothing".
		{"empty says everything", links.Terms{}, "(everything served)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := grantOf(tc.terms); got != tc.want {
				t.Errorf("grantOf = %q, want %q", got, tc.want)
			}
		})
	}
}

// The listing is columnar, so a long grant must widen the column rather
// than run into the expiry beside it.
func TestLinkListingAlignsAroundTheWidestGrant(t *testing.T) {
	entries := []links.Terms{
		{Label: "ana", Grant: "files", Code: "aaaa"},
		{Label: "dev", Grant: "forward 127.0.0.1:5432 shell", Code: "bbbb"},
	}
	out := renderLinkListing(entries, "https://bitba.ng", "8ach_I7oQk2vBb9xYzT0Lw", time.Now())
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("want a line per entry, got:\n%s", out)
	}
	// Both URLs start at the same column, which is what makes the listing
	// scannable when one entry narrows a target and another does not.
	if a, b := strings.Index(lines[0], "https://"), strings.Index(lines[1], "https://"); a != b {
		t.Errorf("URL columns disagree (%d vs %d):\n%s", a, b, out)
	}
	if !strings.Contains(out, "forward 127.0.0.1:5432 shell") {
		t.Errorf("the wide grant was truncated:\n%s", out)
	}
}
