package main

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/richlegrand/bitbang/internal/grant"
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
func grantQuestions(a asker, seed links.Terms, offered grant.Spec, taken map[string]bool, now time.Time) (links.Terms, error) {
	out := seed

	g, err := askGrant(a, seed, offered)
	if err != nil {
		return links.Terms{}, err
	}
	out.Grant = g

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

// askGrant asks what a link reaches, in the words `serve` takes.
//
// The same grammar, the same parser, the same errors: an operator who has
// typed a listener command already knows this one, and a mistake here reads
// exactly as it would there. It also replaces two questions with one, since
// naming a target names the capability that carries it.
//
// The listener's own grant is shown above the prompt, because a link can
// only narrow it -- and because a pinned listener should get the credit for
// having been pinned.
func askGrant(a asker, seed links.Terms, offered grant.Spec) (string, error) {
	if len(offered.Caps) == 0 {
		return "", nil
	}
	a.Say("  This listener serves:")
	rows := make([][2]string, 0, 4)
	width := 0
	for _, w := range offered.Words() {
		form := w + argumentForm(w, offered)
		if len(form) > width {
			width = len(form)
		}
		rows = append(rows, [2]string{form, describeOffer(w, offered)})
	}
	for _, r := range rows {
		a.Say("    %-*s  %s", width, r[0], r[1])
	}
	a.Say("  Grant what? The same words `serve` takes, e.g. %s", grantExample(offered))
	a.Say("  Enter grants all of it.")

	def := seed.Grant
	if def == "" {
		def = "all"
	}
	for {
		answer, err := a.Ask("  Grant", def)
		if err != nil {
			return "", err
		}
		answer = strings.TrimSpace(answer)
		if answer == "" || strings.EqualFold(answer, "all") {
			return "", nil
		}
		spec, err := grant.ParseString(answer)
		if err != nil {
			a.Say("  %v", err)
			continue
		}
		// Refuse a widening grant here rather than minting a link that
		// silently reaches nothing.
		if _, err := offered.Narrow(spec); err != nil {
			a.Say("  %v", err)
			continue
		}
		return spec.String(), nil
	}
}

// argumentForm is the placeholder shown beside a capability, so the prompt
// says what may be typed rather than only what is served.
//
// Shown only where narrowing is actually available. A listener that pinned
// its shell command has fixed it -- a link may not choose another -- so
// offering `[COMMAND]` there would invite an answer that gets refused.
func argumentForm(word string, offered grant.Spec) string {
	switch word {
	case "files":
		return " [DIR]"
	case "proxy", "forward":
		return " [HOST:PORT,...]"
	case "shell":
		if len(offered.ShellArgv) > 0 {
			return ""
		}
		return " [COMMAND]"
	}
	return ""
}

// grantExample picks an example from what this listener actually serves, so
// the shape shown is one that would be accepted if typed.
func grantExample(offered grant.Spec) string {
	// A listener offering several targets gives the best example, because
	// picking one of them is narrowing rather than just restating what is
	// already served.
	if len(offered.ForwardTargets) > 1 {
		return "`forward " + offered.ForwardTargets[0] + "`"
	}
	if len(offered.ProxyTargets) > 1 {
		return "`proxy " + offered.ProxyTargets[0] + "`"
	}
	switch {
	case offered.Has(grant.ScopeFiles):
		if offered.FilesPath != "" {
			// Under what is shared, so the example is one that would be
			// accepted -- a generic path would be refused here.
			return "`files " + strings.TrimSuffix(offered.FilesPath, "/") + "/SUBDIR`"
		}
		return "`files ~/Downloads`"
	case offered.Has(grant.ScopeForward):
		if len(offered.ForwardTargets) == 1 {
			return "`forward " + offered.ForwardTargets[0] + "`"
		}
		return "`forward db:5432`"
	case offered.Has(grant.ScopeProxy):
		if len(offered.ProxyTargets) == 1 {
			return "`proxy " + offered.ProxyTargets[0] + "`"
		}
		return "`proxy nas:8096`"
	}
	if len(offered.ShellArgv) == 0 {
		// Pinning a command is the only narrowing a shell-only listener
		// offers, so that is the example worth showing.
		return "`shell /bin/login`"
	}
	return "`shell`"
}

// describeOffer says what one capability of the listener's grant reaches,
// so the prompt above shows the ceiling a link is narrowing.
func describeOffer(word string, offered grant.Spec) string {
	switch word {
	case "shell":
		if len(offered.ShellArgv) > 0 {
			return strings.Join(offered.ShellArgv, " ")
		}
		return "a terminal on this machine"
	case "files":
		if offered.FilesPath != "" {
			return offered.FilesPath
		}
		return "browse and transfer files"
	case "proxy":
		if len(offered.ProxyTargets) > 0 {
			return strings.Join(offered.ProxyTargets, ", ")
		}
		return "web apps on this network, chosen in the browser"
	case "forward":
		if len(offered.ForwardTargets) > 0 {
			return strings.Join(offered.ForwardTargets, ", ")
		}
		return "TCP to any host on this network, without a shell"
	}
	return ""
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

// effectiveWords renders what a link actually reaches on this listener, in
// the grammar it was written in. Every place that shows a link's reach --
// the console listing, the pairing confirmation, the authorization log --
// goes through here, so they cannot drift from each other or from the file.
func effectiveWords(t links.Terms, offered grant.Spec) string {
	eff, err := t.Effective(offered)
	if err != nil {
		// Validate rejects an unparseable grant at load, so this is a link
		// asking for more than the listener serves. Say so rather than
		// printing a reach that is not real. The label is already the row
		// this is printed beside, so drop the copy the error carries.
		return "nothing (" + strings.TrimPrefix(err.Error(),
			fmt.Sprintf("link %q: ", t.Label)) + ")"
	}
	// The whole grant, arguments and all: a link narrowed to one forward
	// target reads identically to the other three rows without them, and
	// the narrowing is the thing worth seeing.
	return eff.String()
}
