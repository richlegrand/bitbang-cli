package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/richlegrand/bitbang/internal/grant"
	"github.com/richlegrand/bitbang/internal/identity"
	"github.com/richlegrand/bitbang/internal/links"
)

// linkPoll is how often live sessions are re-checked against the link
// table. Deletion is applied the moment the table is replaced; this
// ticker exists for expiry, where the clock moves with nobody touching
// the file. Expiry is measured in days, so a minute is ample.
const linkPoll = time.Minute

// linkState owns the listener's link table and every path that replaces
// it. Readers take a snapshot pointer; nothing mutates a table in place,
// so a poll cannot observe a half-applied edit.
type linkState struct {
	path     string
	offered  grant.Spec
	code     string
	codeURL  func(code string, flags ...string) string
	readOnly bool // ephemeral identity: no file, just the implicit row

	mu    sync.RWMutex
	table *links.Table
	// mod is the file's modtime as loaded, checked before write-back so a
	// mint cannot clobber an edit made while the listener was running.
	mod time.Time
}

func newLinkState(program string, offered grant.Spec, code string, ephemeral bool,
	codeURL func(string, ...string) string) (*linkState, error) {

	ls := &linkState{
		path:     "",
		offered:  offered,
		code:     code,
		codeURL:  codeURL,
		readOnly: ephemeral,
	}
	if !ephemeral {
		ls.path = filepath.Join(identity.Dir(program), links.Filename)
	}
	if err := ls.reload(); err != nil {
		return nil, err
	}
	return ls, nil
}

// current returns the table as it stands now.
func (ls *linkState) current() *links.Table {
	ls.mu.RLock()
	defer ls.mu.RUnlock()
	return ls.table
}

// reload reads the file, mints codes for entries that have none, writes
// those back, and replaces the table. An unreadable or invalid file is
// an error and leaves the previous table in place: an empty table means
// "no links", which is defined as one code granting everything, so
// degrading to it would silently widen access.
func (ls *linkState) reload() error {
	var entries []links.Terms
	var mod time.Time
	if !ls.readOnly {
		var err error
		entries, mod, err = links.Load(ls.path)
		if err != nil {
			return err
		}
	}

	// Two passes over the file, and the order matters. Retire first, so a
	// code whose entry has lapsed is gone before anything can mint over
	// it; then mint, which fills the entries that are live and codeless
	// -- including one just renewed, which is how a renewed link gets a
	// fresh URL rather than reviving the one already handed out.
	now := time.Now()
	var changed []string
	if len(entries) > 0 {
		var retired, minted []string

		// Before retiring, because the two interact. If an expired row
		// and a live row share a code and retirement runs first, the
		// expired row's code is cleared, dedup sees no duplicate, and the
		// live row keeps a code that was handed out as the expired one's.
		// Clearing every sharing row makes the passes commute, but the
		// order is kept anyway so the reasoning does not have to be
		// rediscovered.
		var conflicts []links.CodeConflict
		entries, conflicts = links.DedupeCodes(entries, ls.code)
		for _, c := range conflicts {
			if c.Reserved {
				fmt.Fprintf(os.Stderr,
					"Link %s used this device's own code; it has been given its own.\n",
					strings.Join(quoteAll(c.Labels), " and "))
				continue
			}
			fmt.Fprintf(os.Stderr,
				"Links %s shared a code; none kept it and each has a new one. "+
					"Anyone holding the old URL needs the new one.\n",
				strings.Join(quoteAll(c.Labels), " and "))
			changed = append(changed, c.Labels...)
		}

		entries, retired = links.RetireExpired(entries, now)
		var err error
		entries, minted, err = links.Mint(entries, now, identity.NewAccessCode)
		if err != nil {
			return err
		}
		for _, label := range retired {
			fmt.Fprintf(os.Stderr, "Link %q expired; its code is retired and a renewal will get a new one.\n", label)
		}
		changed = append(append(changed, retired...), minted...)
	}
	if len(changed) > 0 {
		if err := links.Save(ls.path, entries, mod); err != nil {
			return fmt.Errorf("writing link codes: %w", err)
		}
		// Re-stat so the next mint compares against what we just wrote.
		if _, m, err := links.Load(ls.path); err == nil {
			mod = m
		}
	}

	table, warnings, err := links.Build(entries, ls.offered, ls.code)
	if err != nil {
		return err
	}
	for _, w := range warnings {
		fmt.Fprintf(os.Stderr, "Warning: %s\n", w)
	}

	ls.mu.Lock()
	ls.table, ls.mod = table, mod
	ls.mu.Unlock()
	return nil
}

// listing renders the table: every link, what it grants, when it lapses,
// and the URL that reaches it. The listing is the state of the world, so
// it prints expired rows too -- dropping them looks like the file failed
// to load.
// listing renders the link table.
//
// Two lines per entry, with the URL alone on the second. One line each
// read better in a wide window and was unusable in a normal one: a URL
// is about 58 columns on its own, so the row ran past 100 and wrapped
// mid-fragment on an 80-column terminal -- illegible, and impossible to
// select cleanly, which is the one thing anybody does with it.
//
// No blank line between entries: the number in front of each label is a
// left edge to find the next entry by, so the pairs read as pairs
// without spending a line saying so. The URL sits at the hanging indent
// the number already makes, under the label rather than out past it.
// The label carries the bold, being what you scan for.
func (ls *linkState) listing(bold, reset string) string {
	table := ls.current()
	entries := table.Entries()
	if len(entries) == 1 {
		// Only the implicit row: the caller shows the URL itself.
		return ""
	}

	now := time.Now()
	labelW := 0
	for _, e := range entries {
		if n := len(e.Label); n > labelW {
			labelW = n
		}
	}

	var b strings.Builder
	b.WriteString("\n")
	for i, e := range entries {
		reach := effectiveWords(e, ls.offered)
		if reach == "" {
			reach = "(nothing)"
		}
		head := fmt.Sprintf("  %d) %s%-*s%s  %s", i, bold, labelW, e.Label, reset, reach)
		if note := expiryNote(e, now); note != "" {
			head += "  " + note
		}
		b.WriteString(head + "\n")

		// A retired or not-yet-minted entry has no code, and printing a
		// URL whose fragment is empty offers something that cannot work.
		url := "(no code until renewed)"
		if e.Code != "" {
			url = ls.codeURL(e.Code)
		}
		// Indented to the label, not to the scopes column: aligning under
		// the scopes would push the URL right as labels grow, and a long
		// label would put it back over 80 columns, which is the whole
		// thing this layout exists to avoid.
		fmt.Fprintf(&b, "     %s\n", url)
	}
	return b.String()
}

// takenLabels is the set a new label may not collide with, minus the
// one entry being renamed -- editing something without changing its
// name has to stay legal.
func (ls *linkState) takenLabels(except string) map[string]bool {
	out := make(map[string]bool)
	for _, l := range ls.labels() {
		if l != except {
			out[l] = true
		}
	}
	return out
}

// byRef turns a console argument into a label. A label wins over a
// number, so an entry someone literally named "2" is still reachable by
// its name; failing that the argument is read as the index the listing
// printed beside it.
//
// Resolving here rather than in each command means everything
// downstream -- the owner refusals, the "no link called" message, the
// mutation itself -- keeps working on labels and never learns that
// numbers exist. An argument that resolves to nothing is handed back
// unchanged, so the error names what the operator actually typed.
func (ls *linkState) byRef(arg string) string {
	entries := ls.current().Entries()
	for _, e := range entries {
		if e.Label == arg {
			return arg
		}
	}
	if n, err := strconv.Atoi(arg); err == nil && n >= 0 && n < len(entries) {
		return entries[n].Label
	}
	return arg
}

// expiryNote is the human column: nothing for a link that does not
// expire, a rough countdown for one that does, and `expired` for one
// that already has.
func expiryNote(t links.Terms, now time.Time) string {
	if t.Expires == nil {
		return ""
	}
	if t.Check(now) != nil {
		return "expired"
	}
	// Round up: a link set to expire in six days has six days left for
	// almost all of its life, and truncating shows "5d" the moment it is
	// minted, which reads as an off-by-one.
	left := t.Expires.Sub(now)
	switch {
	case left > 24*time.Hour:
		return fmt.Sprintf("expires in %dd", ceilUnits(left, 24*time.Hour))
	case left > time.Hour:
		return fmt.Sprintf("expires in %dh", ceilUnits(left, time.Hour))
	case left > time.Minute:
		return fmt.Sprintf("expires in %dm", ceilUnits(left, time.Minute))
	default:
		return "expires shortly"
	}
}

func ceilUnits(d, unit time.Duration) int {
	return int((d + unit - 1) / unit)
}

// labels lists the table's labels, sorted, for `link` subcommands.
func (ls *linkState) labels() []string {
	var out []string
	for _, e := range ls.current().Entries() {
		out = append(out, e.Label)
	}
	sort.Strings(out)
	return out
}

// quoteAll wraps each label in quotes for a message.
func quoteAll(labels []string) []string {
	out := make([]string, len(labels))
	for i, l := range labels {
		out[i] = fmt.Sprintf("%q", l)
	}
	return out
}

// mutate applies fn to the table on disk and installs the result: read,
// change, de-duplicate, retire, mint, write, rebuild, swap.
//
// The listener is the writer, which is why the console beats telling
// someone to edit the file. There is no second process, so no modtime
// race and no guard to trip over. The file is re-read first so an edit
// made outside is folded in rather than lost.
func (ls *linkState) mutate(fn func([]links.Terms) ([]links.Terms, error)) error {
	if ls.readOnly {
		return fmt.Errorf("this listener has an ephemeral identity, so it keeps no link table")
	}

	entries, mod, err := links.Load(ls.path)
	if err != nil {
		return err
	}
	entries, err = fn(entries)
	if err != nil {
		return err
	}

	now := time.Now()
	entries, _ = links.DedupeCodes(entries, ls.code)
	entries, _ = links.RetireExpired(entries, now)
	entries, _, err = links.Mint(entries, now, identity.NewAccessCode)
	if err != nil {
		return err
	}
	if err := links.Save(ls.path, entries, mod); err != nil {
		return err
	}

	table, warnings, err := links.Build(entries, ls.offered, ls.code)
	if err != nil {
		return err
	}
	for _, w := range warnings {
		fmt.Fprintf(os.Stderr, "Warning: %s\n", w)
	}
	if _, m, err := links.Load(ls.path); err == nil {
		mod = m
	}

	ls.mu.Lock()
	ls.table, ls.mod = table, mod
	ls.mu.Unlock()
	return nil
}

// add appends a link and returns the code minted for it.
func (ls *linkState) add(entry links.Terms) (string, error) {
	if entry.Label == links.OwnerLabel {
		return "", fmt.Errorf("%q is reserved for this device's own code", links.OwnerLabel)
	}
	entry.Code = "" // minted by mutate, never carried in from the caller

	err := ls.mutate(func(entries []links.Terms) ([]links.Terms, error) {
		for _, e := range entries {
			if e.Label == entry.Label {
				return nil, fmt.Errorf("a link called %q already exists", entry.Label)
			}
		}
		return append(entries, entry), nil
	})
	if err != nil {
		return "", err
	}
	added, _ := ls.current().ByLabel(entry.Label)
	return added.Code, nil
}

// remove deletes a link. Sessions using it are closed by the poll the
// caller runs afterwards, which is also what makes rm a revocation
// rather than only a bookkeeping change.
func (ls *linkState) remove(label string) error {
	if label == links.OwnerLabel {
		return fmt.Errorf("%q is this device's own code, not a table entry", links.OwnerLabel)
	}
	return ls.mutate(func(entries []links.Terms) ([]links.Terms, error) {
		kept := make([]links.Terms, 0, len(entries))
		found := false
		for _, e := range entries {
			if e.Label == label {
				found = true
				continue
			}
			kept = append(kept, e)
		}
		if !found {
			return nil, fmt.Errorf("no link called %q", label)
		}
		return kept, nil
	})
}

// replace swaps an entry for an edited one, keyed on the label it had.
// The code is carried across, so a URL already handed out keeps working
// under the new terms -- unless the entry has lapsed, in which case
// retirement clears it and a fresh one is minted.
func (ls *linkState) replace(oldLabel string, entry links.Terms) error {
	if oldLabel == links.OwnerLabel || entry.Label == links.OwnerLabel {
		return fmt.Errorf("%q is this device's own code, not a table entry", links.OwnerLabel)
	}
	return ls.mutate(func(entries []links.Terms) ([]links.Terms, error) {
		out := make([]links.Terms, 0, len(entries))
		found := false
		for _, e := range entries {
			switch {
			case e.Label == oldLabel:
				found = true
				entry.Code = e.Code
				out = append(out, entry)
			case e.Label == entry.Label:
				return nil, fmt.Errorf("a link called %q already exists", entry.Label)
			default:
				out = append(out, e)
			}
		}
		if !found {
			return nil, fmt.Errorf("no link called %q", oldLabel)
		}
		return out, nil
	})
}

// url composes the URL for a code.
func (ls *linkState) url(code string) string { return ls.codeURL(code) }
