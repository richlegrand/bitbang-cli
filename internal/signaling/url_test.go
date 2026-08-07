package signaling

import (
	"strings"
	"testing"

	"github.com/richlegrand/bitbang/internal/identity"
)

func testClient(t *testing.T) *Client {
	t.Helper()
	id, err := identity.Load("bitbang-url-test", true)
	if err != nil {
		t.Fatalf("identity.Load: %v", err)
	}
	return NewClient("bitba.ng", id)
}

// TestCodeURLGrammar pins URL composition to the fragment grammar in
// CONVENTIONS.md, which bootstrap.js implements:
//
//	<code> [ '!' <flag> [ ',' <flag> ]* ] [ '/' <device-URL> ]
//
// One "!" opens the flag list and COMMAS separate the flags. Emitting
// "!a!b" instead would leave the browser -- and ParseFragment on the CLI
// side -- reading a single flag named "a!b".
func TestCodeURLGrammar(t *testing.T) {
	c := testClient(t)

	tests := []struct {
		flags []string
		want  string
	}{
		{nil, "#" + c.ID.Code},
		{[]string{"ephemeral"}, "#" + c.ID.Code + "!ephemeral"},
		{[]string{"ephemeral", "debug"}, "#" + c.ID.Code + "!ephemeral,debug"},
		{[]string{"a", "b", "c"}, "#" + c.ID.Code + "!a,b,c"},
	}
	for _, tc := range tests {
		got := c.CodeURL(c.ID.Code, tc.flags...)
		if !strings.HasSuffix(got, tc.want) {
			t.Errorf("CodeURL(%v) = %q, want fragment %q", tc.flags, got, tc.want)
		}
		if !strings.HasPrefix(got, "https://bitba.ng/"+c.ID.UID+"#") {
			t.Errorf("CodeURL(%v) = %q, want the canonical https://<server>/<uid>#... shape", tc.flags, got)
		}
		// Exactly one "!" -- the flag list opener, never a separator.
		if n := strings.Count(got, "!"); n > 1 {
			t.Errorf("CodeURL(%v) = %q has %d '!' separators, want at most 1", tc.flags, got, n)
		}
	}
}

// TestCodeURLCarriesGivenCode: a share's view URL must carry the view
// code, not the identity's own -- that distinction is the whole
// two-credential model.
func TestCodeURLCarriesGivenCode(t *testing.T) {
	c := testClient(t)
	const viewCode = "VIEWCODE123"

	got := c.CodeURL(viewCode, "ephemeral")
	if !strings.HasSuffix(got, "#"+viewCode+"!ephemeral") {
		t.Errorf("CodeURL = %q, want it to carry %q", got, viewCode)
	}
	if strings.Contains(got, c.ID.Code) {
		t.Errorf("CodeURL = %q leaked the identity's own access code", got)
	}
}

// TestURLUsesCodeURL keeps the single-code path on the same composer,
// so the grammar can only ever be defined in one place.
func TestURLUsesCodeURL(t *testing.T) {
	c := testClient(t)
	if got, want := c.URL(false), c.CodeURL(c.ID.Code); got != want {
		t.Errorf("URL(false) = %q, want %q", got, want)
	}
	if got, want := c.URL(true), c.CodeURL(c.ID.Code, "debug"); got != want {
		t.Errorf("URL(true) = %q, want %q", got, want)
	}
}
