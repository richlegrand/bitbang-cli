// Package links implements the per-link access table: the terms a code
// grants, when they lapse, and which capabilities they reach.
//
// One Terms record per link. `serve` reads them from links.json next to
// the identity; other callers may build them in memory. Check is the only
// place a link's validity is decided, so every future term (uses,
// first-use TTL) becomes a case there rather than a second check
// somewhere else.
package links

import (
	"fmt"
	"sort"
	"time"
)

// Terms is one link's grant. The field set is deliberately also the
// payload a signed token would carry later, so there is one encoding
// rather than two.
type Terms struct {
	Label string `json:"label"`
	// Code is the secret in the URL fragment. Empty means "mint me one":
	// the listener generates it, writes it back, and prints the URL.
	Code string `json:"code,omitempty"`
	// Scope is the user-facing vocabulary below, not wire stream types.
	// Nil means everything the listener offers; an empty non-nil slice is
	// rejected at parse time as a typo.
	Scope   []string   `json:"scope,omitempty"`
	Expires *time.Time `json:"expires,omitempty"`
}

// OwnerLabel is the label of the implicit entry standing for the
// identity's own access code -- the operator's own link, which grants
// everything the listener serves and never expires. It is synthesized
// into the table at load rather than special-cased in the checker, so
// the table is the whole story and the poll finds a row for every live
// session.
//
// Named for the reader rather than the writer: a listener started at
// boot is read by whoever is looking at the log, and "me" has no
// referent there.
const OwnerLabel = "owner"

// The scope vocabulary. These names are permanent in a way flags are
// not: they live in config files people keep, so a stream type can be
// renamed or split later without invalidating anything they wrote.
//
// What each one reaches is the listener's business, not this package's,
// which is why there is no name-to-stream-type table here. Two of them
// need saying out loud:
//
//   - Proxy is one name for both http and websocket streams. Granting
//     one without the other yields a proxy that half works, and nobody
//     would predict that.
//   - Neither Proxy nor any other scope gates the listener's own browser
//     UI. That UI is the shell the other scopes act through -- a
//     files-only link still has to render a file browser -- so it rides
//     on every link and shows only what the link actually grants.
const (
	ScopeFiles   = "files"
	ScopeShell   = "shell"
	ScopeForward = "forward"
	ScopeProxy   = "proxy"
)

var knownScopes = map[string]bool{
	ScopeFiles:   true,
	ScopeShell:   true,
	ScopeForward: true,
	ScopeProxy:   true,
}

// ScopeNames lists the vocabulary, sorted, for error messages.
func ScopeNames() []string {
	names := make([]string, 0, len(knownScopes))
	for n := range knownScopes {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Check reports whether the terms still hold at the given instant. A nil
// error means the link is good.
func (t Terms) Check(now time.Time) error {
	if t.Expires != nil && !now.Before(*t.Expires) {
		return fmt.Errorf("link %q expired at %s", t.Label, t.Expires.Format(time.RFC3339))
	}
	return nil
}

// Grants resolves the terms against what this listener offers, returning
// the scopes actually in force, sorted.
//
// Effective is always requested and offered: a link scoped [shell] on a
// files-only listener conjures no shell. Absent scope means everything
// offered, which is what keeps a pre-existing single-code setup working
// unchanged after upgrade.
func (t Terms) Grants(offered []string) []string {
	if t.Scope == nil {
		out := append([]string(nil), offered...)
		sort.Strings(out)
		return out
	}
	want := make(map[string]bool, len(t.Scope))
	for _, name := range t.Scope {
		want[name] = true
	}
	var out []string
	for _, name := range offered {
		if want[name] {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// GrantSet is Grants as a lookup, for a listener assembling its handlers.
func (t Terms) GrantSet(offered []string) map[string]bool {
	set := make(map[string]bool)
	for _, name := range t.Grants(offered) {
		set[name] = true
	}
	return set
}

// SameGrants reports whether two terms grant the same scopes against the
// same listener. The poll uses it to notice that an entry narrowed while
// a session was open: the handler set was fixed when the session was
// built and cannot shrink in place.
func SameGrants(a, b Terms, offered []string) bool {
	ga, gb := a.Grants(offered), b.Grants(offered)
	if len(ga) != len(gb) {
		return false
	}
	for i := range ga {
		if ga[i] != gb[i] {
			return false
		}
	}
	return true
}
