package main

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/richlegrand/bitbang/internal/links"
)

// asker reads one line from the operator, showing a default that Enter
// accepts. An interface so the question flow can be tested without a
// terminal, and so the same flow can later be driven by a console
// attached over a socket rather than by stdin.
type asker interface {
	// Ask shows prompt with def as the bracketed default and returns what
	// was typed, or def when the line was empty. A non-nil error means
	// the operator gave up: Ctrl-C, EOF, or a deadline.
	Ask(prompt, def string) (string, error)
	// Say writes a line of context between questions.
	Say(format string, args ...interface{})
}

// grantQuestions asks what a link should grant. One flow, three callers:
// a pairing that declined the default, `add`, and `edit`. They differ only
// in what the answers are seeded with, so nothing here knows which is
// running.
//
// Seed carries the current values. For `add` and pairing that is the
// default grant; for `edit` it is the entry as it stands, so pressing
// Enter through the whole flow changes nothing.
func grantQuestions(a asker, seed links.Terms, offered []string, taken map[string]bool, now time.Time, reach scopeReach) (links.Terms, error) {
	out := seed

	scope, err := askScope(a, seed, offered, reach)
	if err != nil {
		return links.Terms{}, err
	}
	out.Scope = scope

	expires, err := askExpiry(a, seed, now)
	if err != nil {
		return links.Terms{}, err
	}
	out.Expires = expires

	label, err := askLabel(a, seed, taken, now)
	if err != nil {
		return links.Terms{}, err
	}
	out.Label = label

	return out, nil
}

// scopeHelp is the one-line description beside each scope. forward's
// earns its place: TCP without a shell is the combination people do not
// expect, and the answer to "can they have the NAS but not the box".
var scopeHelp = map[string]string{
	links.ScopeFiles:   "browse and transfer files",
	links.ScopeShell:   "a terminal on this machine",
	links.ScopeForward: "forward TCP to any host on this network, without a shell",
	links.ScopeProxy:   "reach web apps on this network",
}

// scopeReach carries what the two target-choosing scopes can actually reach,
// rendered from the listener's allowlists. Empty means unrestricted.
//
// The menu is where an operator decides what to hand out, and `forward` read
// as the narrow option there: it said "without a shell" and nothing about
// reach, while `proxy` right above it said "on this network". Unrestricted,
// forward is the wider of the two.
type scopeReach struct {
	forward string
	proxy   string
}

// reachOf renders a listener's allowlists for the menu. A pinned proxy target
// counts as a restriction too, since the browser cannot choose another.
func reachOf(cfg serveConfig) scopeReach {
	r := scopeReach{forward: cfg.allowForward.String()}
	switch {
	case cfg.target != "":
		r.proxy = cfg.target
	default:
		r.proxy = cfg.allowProxy.String()
	}
	return r
}

// helpFor is scopeHelp, but naming the allowlist when the listener has one.
// A pinned listener should say so here -- the operator gets to see that the
// link they are minting is genuinely narrow, which is the reward for having
// set -allow-forward in the first place.
func (r scopeReach) helpFor(scope string) string {
	switch {
	case scope == links.ScopeForward && r.forward != "":
		return "forward TCP to " + r.forward + ", without a shell"
	case scope == links.ScopeProxy && r.proxy != "":
		return "reach " + r.proxy
	}
	return scopeHelp[scope]
}

// menuOrder lists scopes least powerful first, so a mis-keyed 1 grants a
// file browser rather than a terminal. The capability table's order suits
// the Sharing block, where it describes a listener; here the numbers are
// something an operator types in a hurry.
var menuOrder = []string{links.ScopeFiles, links.ScopeProxy, links.ScopeForward, links.ScopeShell}

// orderForMenu sorts what the listener offers into menuOrder, keeping
// anything unrecognized at the end rather than dropping it.
func orderForMenu(offered []string) []string {
	rank := make(map[string]int, len(menuOrder))
	for i, name := range menuOrder {
		rank[name] = i
	}
	out := append([]string(nil), offered...)
	sort.SliceStable(out, func(i, j int) bool {
		ri, ok := rank[out[i]]
		if !ok {
			ri = len(menuOrder)
		}
		rj, ok := rank[out[j]]
		if !ok {
			rj = len(menuOrder)
		}
		return ri < rj
	})
	return out
}

func askScope(a asker, seed links.Terms, offered []string, reach scopeReach) ([]string, error) {
	if len(offered) == 0 {
		return nil, nil
	}
	offered = orderForMenu(offered)
	a.Say("  Grant which?")
	for i, name := range offered {
		a.Say("    %d) %-8s  %s", i+1, name, reach.helpFor(name))
	}

	// A nil scope means everything, which is what `a` produces, so the
	// default reads the same way whichever the seed holds.
	def := "a"
	if seed.Scope != nil {
		var picks []string
		for i, name := range offered {
			for _, s := range seed.Scope {
				if s == name {
					picks = append(picks, strconv.Itoa(i+1))
				}
			}
		}
		if len(picks) > 0 {
			def = strings.Join(picks, ",")
		}
	}

	for {
		answer, err := a.Ask(fmt.Sprintf("  [1-%d, comma-separated, or a for all]", len(offered)), def)
		if err != nil {
			return nil, err
		}
		scope, err := parseScopeAnswer(answer, offered)
		if err != nil {
			a.Say("  %v", err)
			continue
		}
		return scope, nil
	}
}

// parseScopeAnswer turns "1,3" or "a" into scope names. A nil result
// means everything offered, which is how an unscoped link is stored.
func parseScopeAnswer(answer string, offered []string) ([]string, error) {
	answer = strings.TrimSpace(answer)
	if answer == "" {
		return nil, fmt.Errorf("pick at least one, or a for all")
	}
	if strings.EqualFold(answer, "a") {
		return nil, nil
	}
	seen := make(map[string]bool)
	var out []string
	for _, field := range strings.Split(answer, ",") {
		field = strings.TrimSpace(field)
		n, err := strconv.Atoi(field)
		if err != nil || n < 1 || n > len(offered) {
			return nil, fmt.Errorf("%q is not one of 1-%d", field, len(offered))
		}
		if name := offered[n-1]; !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out, nil
}

// expiryChoice is one row of the expiry menu.
type expiryChoice struct {
	label string
	dur   time.Duration // 0 means never
	keep  bool          // edit only: leave the current value alone
	other bool          // free text
}

func expiryMenu(seed links.Terms, now time.Time) []expiryChoice {
	var out []expiryChoice
	// On edit the current value matches no preset, so keep is offered
	// first and is the default -- Enter changes nothing.
	if seed.Expires != nil {
		out = append(out, expiryChoice{
			label: fmt.Sprintf("keep: %s, %s", seed.Expires.Local().Format("2006-01-02 15:04"),
				relativeTo(*seed.Expires, now)),
			keep: true,
		})
	}
	out = append(out,
		expiryChoice{label: "never"},
		expiryChoice{label: "1 hour", dur: time.Hour},
		expiryChoice{label: "24 hours", dur: 24 * time.Hour},
		expiryChoice{label: "7 days", dur: 7 * 24 * time.Hour},
		expiryChoice{label: "other", other: true},
	)
	return out
}

func askExpiry(a asker, seed links.Terms, now time.Time) (*time.Time, error) {
	menu := expiryMenu(seed, now)
	a.Say("  Expires?")
	for i, c := range menu {
		suffix := ""
		if i == 0 {
			suffix = "  [default]"
		}
		a.Say("    %d) %s%s", i+1, c.label, suffix)
	}

	for {
		answer, err := a.Ask(fmt.Sprintf("  [1-%d]", len(menu)), "1")
		if err != nil {
			return nil, err
		}
		n, convErr := strconv.Atoi(strings.TrimSpace(answer))
		if convErr != nil || n < 1 || n > len(menu) {
			a.Say("  pick 1-%d", len(menu))
			continue
		}
		switch c := menu[n-1]; {
		case c.keep:
			return seed.Expires, nil
		case c.other:
			at, err := askOtherExpiry(a, now)
			if err != nil {
				return nil, err
			}
			if at == nil {
				continue // declined the confirmation; ask again
			}
			return at, nil
		case c.dur == 0:
			return nil, nil
		default:
			at := now.Add(c.dur).UTC().Truncate(time.Second)
			return &at, nil
		}
	}
}

// askOtherExpiry is the only free text in the flow. It resolves to an
// absolute instant and shows it back, because "2w" is unambiguous to a
// parser and easy to misread at a glance -- the confirmation is what
// catches it before someone is locked out on the wrong day. A nil return
// with a nil error means the operator said no to the resolved date.
func askOtherExpiry(a asker, now time.Time) (*time.Time, error) {
	for {
		answer, err := a.Ask("  How long?  (90m, 3d, 2w, or a date: 2026-09-15)", "")
		if err != nil {
			return nil, err
		}
		at, parseErr := parseWhen(strings.TrimSpace(answer), now)
		if parseErr != nil {
			a.Say("  %v", parseErr)
			continue
		}
		ok, err := a.Ask(fmt.Sprintf("  -> %s, %s.  OK?",
			at.Local().Format("2006-01-02 15:04 MST"), relativeTo(at, now)), "Y")
		if err != nil {
			return nil, err
		}
		if strings.EqualFold(strings.TrimSpace(ok), "y") {
			at = at.UTC().Truncate(time.Second)
			return &at, nil
		}
		return nil, nil
	}
}

// askLabel offers seed.Label as the default and refuses a name already
// in use.
//
// taken excludes the entry being edited, so renaming something to itself
// is allowed. Catching a collision here rather than at the write matters
// because the write is the end of a five-question flow: being told the
// name is taken after choosing scopes and an expiry means answering all
// of it again.
func askLabel(a asker, seed links.Terms, taken map[string]bool, now time.Time) (string, error) {
	def := seed.Label
	for {
		answer, err := a.Ask("  Label", def)
		if err != nil {
			return "", err
		}
		answer = strings.TrimSpace(answer)
		switch {
		case answer == "":
			a.Say("  a label is how you refer to this link later")
		case answer == links.OwnerLabel:
			a.Say("  %q is reserved for this device's own code", links.OwnerLabel)
		case taken[answer]:
			a.Say("  a link called %q already exists", answer)
		default:
			return answer, nil
		}
	}
}

// dayWeek expands the d and w units before handing the rest to
// time.ParseDuration, which has neither -- so `7d` fails there, while the
// cookbook and the links docs both use it. Handles them mid-string too,
// so `1w3d` and `3d12h` work.
var dayWeek = regexp.MustCompile(`(\d+)([dw])`)

// whenLayouts are the absolute forms accepted, tried in order.
var whenLayouts = []string{"2006-01-02 15:04", "2006-01-02T15:04", "2006-01-02"}

// parseWhen resolves free text to an instant: a duration from now, or a
// date. Resolution happens once, here, so the stored expiry is absolute
// and does not slide if the listener restarts.
func parseWhen(s string, now time.Time) (time.Time, error) {
	if s == "" {
		return time.Time{}, fmt.Errorf("say how long, e.g. 90m, 3d, 2w, or 2026-09-15")
	}

	for _, layout := range whenLayouts {
		if t, err := time.ParseInLocation(layout, s, now.Location()); err == nil {
			if !t.After(now) {
				return time.Time{}, fmt.Errorf("%s is already past", t.Format("2006-01-02 15:04"))
			}
			return t, nil
		}
	}

	expanded := dayWeek.ReplaceAllStringFunc(s, func(m string) string {
		parts := dayWeek.FindStringSubmatch(m)
		n, _ := strconv.Atoi(parts[1])
		if parts[2] == "w" {
			n *= 7
		}
		return strconv.Itoa(n*24) + "h"
	})
	d, err := time.ParseDuration(expanded)
	if err != nil {
		return time.Time{}, fmt.Errorf("cannot read %q -- try 90m, 3d, 2w, or 2026-09-15", s)
	}
	if d <= 0 {
		return time.Time{}, fmt.Errorf("that is not in the future")
	}
	return now.Add(d), nil
}

// relativeTo renders the gap in the coarsest unit that still says
// something, for showing a resolved date back to the operator.
func relativeTo(at, now time.Time) string {
	d := at.Sub(now)
	if d < 0 {
		return "already past"
	}
	switch {
	case d >= 48*time.Hour:
		return fmt.Sprintf("in %d days", int((d+time.Hour)/(24*time.Hour)))
	case d >= 2*time.Hour:
		return fmt.Sprintf("in %d hours", int((d+time.Minute)/time.Hour))
	case d >= 2*time.Minute:
		return fmt.Sprintf("in %d minutes", int(d/time.Minute))
	default:
		return "very shortly"
	}
}
