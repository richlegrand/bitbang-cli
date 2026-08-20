package links

import (
	"crypto/subtle"
	"fmt"
	"sort"
	"time"
)

// Table is the resolved link table: the entries from links.json plus the
// synthesized `me` row for the identity's own code. Lookup is by label
// (the poll) or by code (the resolver).
//
// Built once per load and replaced wholesale on reload. Nothing mutates
// an existing Table, so a poll reading one cannot see a half-applied
// edit.
type Table struct {
	entries []Terms
	// offered is the scope vocabulary this listener supports, needed to
	// resolve a nil scope and to intersect a requested one.
	offered []string
}

// Build assembles a Table from parsed entries. The identity's own code
// becomes a real row labeled `me` -- no expiry, everything served --
// rather than a special case in the checker, so the poll finds a row for
// every live session and the table is the whole story.
//
// Duplicate labels are rejected here, after synthesis, so a hand-written
// entry labeled `me` collides instead of shadowing.
//
// Warnings name links whose scope asks for something this listener does
// not serve. That grants nothing, which is expensive to debug silently.
func Build(entries []Terms, offered []string, identityCode string) (*Table, []string, error) {
	all := make([]Terms, 0, len(entries)+1)
	all = append(all, Terms{Label: MeLabel, Code: identityCode})
	all = append(all, entries...)

	seen := make(map[string]bool, len(all))
	for _, e := range all {
		if seen[e.Label] {
			if e.Label == MeLabel {
				return nil, nil, fmt.Errorf("link %q is reserved for the identity's own code", MeLabel)
			}
			return nil, nil, fmt.Errorf("duplicate link label %q", e.Label)
		}
		seen[e.Label] = true
	}

	offeredSet := make(map[string]bool, len(offered))
	for _, name := range offered {
		offeredSet[name] = true
	}
	var warnings []string
	for _, e := range entries {
		var missing []string
		for _, name := range e.Scope {
			if !offeredSet[name] {
				missing = append(missing, name)
			}
		}
		if len(missing) > 0 {
			sort.Strings(missing)
			warnings = append(warnings, fmt.Sprintf(
				"link %q asks for %v, which this listener does not offer", e.Label, missing))
		}
	}

	sorted := append([]string(nil), offered...)
	sort.Strings(sorted)
	return &Table{entries: all, offered: sorted}, warnings, nil
}

// Entries returns the rows in table order: `me` first, then the file's
// own, unsorted, so the listing matches what you wrote.
func (t *Table) Entries() []Terms { return append([]Terms(nil), t.entries...) }

// Offered returns the scopes this listener supports.
func (t *Table) Offered() []string { return append([]string(nil), t.offered...) }

// ByLabel finds a row. The poll uses it to re-resolve a live session
// against the table as it stands now; a missing label means the entry was
// deleted.
func (t *Table) ByLabel(label string) (Terms, bool) {
	for _, e := range t.entries {
		if e.Label == label {
			return e, true
		}
	}
	return Terms{}, false
}

// Authorize resolves a presented code to its terms.
//
// The comparison runs against every entry with no early exit, so timing
// does not reveal which link a guess missed -- the contract Connection.
// Authorize documents. An entry whose Check fails is refused exactly as
// an unknown code is: the caller learns nothing beyond yes or no.
func (t *Table) Authorize(code string, now time.Time) (Terms, bool) {
	matched := -1
	for i, e := range t.entries {
		if e.Code == "" {
			continue // unminted; cannot be presented
		}
		if subtle.ConstantTimeCompare([]byte(code), []byte(e.Code)) == 1 {
			matched = i
		}
	}
	if matched < 0 {
		return Terms{}, false
	}
	e := t.entries[matched]
	if err := e.Check(now); err != nil {
		return Terms{}, false
	}
	return e, true
}

// RetireExpired clears the code of every entry whose expiry has passed,
// reporting the labels it retired.
//
// An expired code has to die rather than sleep. Left in place it comes
// back the moment someone extends the entry -- the same fragment, still
// in the URL the original holder kept -- so renewing a link for one
// person silently readmits everyone it had already been sent to, which
// is the opposite of what "expires" led them to expect.
//
// Clearing is written back rather than only applied in memory, because
// the gap this closes includes renewing while the listener is stopped:
// on the next start the entry would simply be live again, code and all.
// Editing someone's file is not something to do lightly, and removing a
// credential that is already dead is the one case that earns it.
func RetireExpired(entries []Terms, now time.Time) ([]Terms, []string) {
	out := append([]Terms(nil), entries...)
	var retired []string
	for i := range out {
		if out[i].Code == "" || out[i].Check(now) == nil {
			continue
		}
		out[i].Code = ""
		retired = append(retired, out[i].Label)
	}
	return out, retired
}

// Mint fills in a code for every live entry that has none and reports the
// labels it touched. An entry with no code is a mint request -- editing
// the file is how you create a link, so there is no `link new`.
//
// Expired entries are skipped: they are exactly what RetireExpired just
// emptied, and minting them a fresh code would hand a dead link a live
// credential and rewrite the file on every reload.
func Mint(entries []Terms, now time.Time, gen func() (string, error)) ([]Terms, []string, error) {
	out := append([]Terms(nil), entries...)
	var minted []string
	for i := range out {
		if out[i].Code != "" || out[i].Check(now) != nil {
			continue
		}
		code, err := gen()
		if err != nil {
			return nil, nil, err
		}
		out[i].Code = code
		minted = append(minted, out[i].Label)
	}
	return out, minted, nil
}
