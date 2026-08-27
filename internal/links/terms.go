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

	"github.com/richlegrand/bitbang/internal/grant"
)

// Terms is one link's grant. The field set is deliberately also the
// payload a signed token would carry later, so there is one encoding
// rather than two.
type Terms struct {
	Label string `json:"label"`
	// Code is the secret in the URL fragment. Empty means "mint me one":
	// the listener generates it, writes it back, and prints the URL.
	Code string `json:"code,omitempty"`
	// Grant is what this link reaches, in the same words `serve` takes:
	// "files ~/share/public", "forward 127.0.0.1:22", "shell". Empty means
	// everything the listener offers.
	//
	// One sentence rather than a scope list plus a targets object: the
	// listener's own command line is written this way, so there is one
	// grammar to learn, one parser, and no way for two representations to
	// disagree.
	Grant   string     `json:"grant,omitempty"`
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

// The scope vocabulary lives in the grant package with the grammar that
// names it, so there is one definition rather than a constant here and a
// parser there. Re-exported because config files and error messages in this
// package still speak it.
const (
	ScopeFiles   = grant.ScopeFiles
	ScopeShell   = grant.ScopeShell
	ScopeForward = grant.ScopeForward
	ScopeProxy   = grant.ScopeProxy
)

// ScopeNames lists the vocabulary, sorted, for error messages.
func ScopeNames() []string {
	names := append([]string(nil), grant.Order...)
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

// Spec parses what this link asks for. An empty Grant is unspecified,
// which Narrow reads as everything the listener offers.
func (t Terms) Spec() (grant.Spec, error) {
	spec, err := grant.ParseString(t.Grant)
	if err != nil {
		return grant.Spec{}, fmt.Errorf("link %q: %w", t.Label, err)
	}
	return spec, nil
}

// SameGrants reports whether two terms reach the same thing on the same
// listener. The poll uses it to notice that an entry narrowed while a
// session was open: the handler set was fixed when the session was built and
// cannot shrink in place.
//
// Compares the effective grants, so narrowing `forward a:22,b:80` to
// `forward a:22` counts -- under a scope-list comparison it would not have,
// and the session would have kept the wider reach until it ended.
func SameGrants(a, b Terms, offered grant.Spec) bool {
	ea, erra := a.Effective(offered)
	eb, errb := b.Effective(offered)
	if erra != nil || errb != nil {
		return erra == nil && errb == nil
	}
	return ea.String() == eb.String()
}

// Effective is what a holder of this link actually reaches on a listener
// offering `offered`. The narrowing rules live in grant, so a link and the
// command line are held to one definition of "narrower".
func (t Terms) Effective(offered grant.Spec) (grant.Spec, error) {
	spec, err := t.Spec()
	if err != nil {
		return grant.Spec{}, err
	}
	// Lenient about capabilities, strict about what they point at: see
	// grant.Spec.Intersect.
	eff, err := offered.Narrow(spec.Intersect(offered))
	if err != nil {
		return grant.Spec{}, fmt.Errorf("link %q: %w", t.Label, err)
	}
	return eff, nil
}
