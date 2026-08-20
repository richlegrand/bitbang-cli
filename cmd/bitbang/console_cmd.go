package main

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/richlegrand/bitbang/internal/links"
)

// commands is what the console accepts, in the order help lists them.
// Grouped by what you are doing: the table, then pairing, then the
// listener itself.
type command struct {
	name string
	args string
	help string
	run  func(*listener, *console, []string) error
}

// Assigned in init rather than inline: cmdHelp reads this table, and a
// literal that contains a function reading it is an initialization cycle.
var commands []command

func init() {
	commands = []command{
		{"list", "", "the links you have handed out", cmdList},
		{"add", "", "create a link", cmdAdd},
		{"edit", "<label>", "change one, seeded with its current values", cmdEdit},
		{"rm", "<label>", "delete one, closing any session using it", cmdRemove},
		{"qr", "<label>", "its URL as a QR code", cmdQR},
		{"code", "", "the pairing code, or a fresh one if it has lapsed", cmdCode},
		{"url", "", "this device's own URL", cmdURL},
		{"status", "", "who is connected", cmdStatus},
		{"reload", "", "re-read links.json after editing it outside", cmdReload},
		{"help", "", "", cmdHelp},
	}
}

// runCommand dispatches one line. An unknown word says so rather than
// failing silently, since the whole surface is discoverable only by
// asking.
func (l *listener) runCommand(c *console, line string) error {
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return nil
	}
	name, args := fields[0], fields[1:]
	for _, cmd := range commands {
		if cmd.name == name {
			return cmd.run(l, c, args)
		}
	}
	c.Say("  unknown command %q -- try help", name)
	return nil
}

func cmdHelp(l *listener, c *console, _ []string) error {
	c.Say("")
	for _, cmd := range commands {
		if cmd.help == "" {
			continue
		}
		c.Say("  %-14s %s", strings.TrimSpace(cmd.name+" "+cmd.args), cmd.help)
	}
	c.Say("  %-14s %s", "exit", "leave the console; output resumes")
	c.Say("")
	return nil
}

func cmdList(l *listener, c *console, _ []string) error {
	listing := l.links.listing("", "")
	if listing == "" {
		c.Say("  Only this device's own code. `add` makes another.")
		return nil
	}
	c.Say("%s", strings.TrimRight(listing, "\n"))
	return nil
}

func cmdAdd(l *listener, c *console, _ []string) error {
	terms, err := grantQuestions(c, links.Terms{}, l.links.offeredScopes(), time.Now())
	if err != nil {
		return err
	}
	code, err := l.links.add(terms)
	if err != nil {
		c.Say("  %v", err)
		return nil
	}
	c.Say("")
	c.Say("  %s -- %s", terms.Label, describeGrant(terms, l.links.offeredScopes()))
	c.Say("  %s", l.links.url(code))
	return nil
}

// cmdEdit re-asks the same questions seeded with the current values, so
// pressing Enter through changes nothing. Renaming is allowed: the label
// is a name for the row, not the credential, and the poll keys on the
// code -- so a rename does not disconnect whoever holds it.
func cmdEdit(l *listener, c *console, args []string) error {
	if len(args) != 1 {
		c.Say("  edit <label>")
		return nil
	}
	current, ok := l.links.current().ByLabel(args[0])
	if !ok || current.Label == links.OwnerLabel {
		c.Say("  no link called %q", args[0])
		return nil
	}
	edited, err := grantQuestions(c, current, l.links.offeredScopes(), time.Now())
	if err != nil {
		return err
	}
	if err := l.links.replace(current.Label, edited); err != nil {
		c.Say("  %v", err)
		return nil
	}
	l.pollNow()
	after, _ := l.links.current().ByLabel(edited.Label)
	c.Say("")
	c.Say("  %s -- %s", edited.Label, describeGrant(edited, l.links.offeredScopes()))
	if after.Code != current.Code {
		c.Say("  code changed, so the old URL is dead: %s", l.links.url(after.Code))
	}
	return nil
}

func cmdRemove(l *listener, c *console, args []string) error {
	if len(args) != 1 {
		c.Say("  rm <label>")
		return nil
	}
	if err := l.links.remove(args[0]); err != nil {
		c.Say("  %v", err)
		return nil
	}
	// Revocation reaches sessions already open, not just the next
	// connection, which is the whole point of rm over editing the file.
	l.pollNow()
	c.Say("  removed %q", args[0])
	return nil
}

func cmdQR(l *listener, c *console, args []string) error {
	if len(args) != 1 {
		c.Say("  qr <label>")
		return nil
	}
	entry, ok := l.links.current().ByLabel(args[0])
	if !ok {
		c.Say("  no link called %q", args[0])
		return nil
	}
	if entry.Code == "" {
		c.Say("  %q has no code until it is renewed", args[0])
		return nil
	}
	url := l.links.url(entry.Code)
	c.Say("%s", strings.TrimRight(smallQR(url), "\n"))
	c.Say("  %s", url)
	return nil
}

func cmdURL(l *listener, c *console, _ []string) error {
	owner, _ := l.links.current().ByLabel(links.OwnerLabel)
	url := l.links.url(owner.Code)
	c.Say("%s", strings.TrimRight(smallQR(url), "\n"))
	c.Say("  %s", url)
	return nil
}

func cmdStatus(l *listener, c *console, _ []string) error {
	peers := l.peers.All()
	live := make([]string, 0, len(peers))
	for _, p := range peers {
		label, terms := p.granted()
		switch {
		case label == "":
			live = append(live, "  (handshaking)")
		default:
			live = append(live, fmt.Sprintf("  %-14s %s",
				label, strings.Join(terms.Grants(l.links.offeredScopes()), " ")))
		}
	}
	if len(live) == 0 {
		c.Say("  nobody connected")
		return nil
	}
	sort.Strings(live)
	for _, line := range live {
		c.Say("%s", line)
	}
	return nil
}

func cmdReload(l *listener, c *console, _ []string) error {
	if err := l.links.reload(); err != nil {
		// The previous table stays in force: an unreadable file must not
		// degrade to "no links", which grants everything.
		c.Say("  reload failed, keeping the previous links: %v", err)
		return nil
	}
	l.pollNow()
	return cmdList(l, c, nil)
}

// cmdCode shows the pairing code, or asks for a fresh one once the old
// has lapsed.
//
// No `code new` variant: the server's Issue is already idempotent inside
// the code's lifetime and mints fresh past it, so one command gives both
// behaviors and both are the one you want -- read the live code out
// again, or get another once it has gone.
func cmdCode(l *listener, c *console, _ []string) error {
	// Always ask, never report the cached value. Issue is idempotent
	// inside the code's lifetime, so asking returns the live code when
	// there is one and a fresh code once it has lapsed -- which is why
	// there is no separate `code new`.
	//
	// The cache cannot answer this: nothing clears PairingCode when a
	// code expires, so trusting it would print a dead code indefinitely.
	// It is only a fallback for a server too old to answer.
	code, err := l.signaling.RenewPairingCode(3 * time.Second)
	if err != nil {
		if cached := l.signaling.PairingCode; cached != "" {
			c.Say("  %v", err)
			c.Say("  Last code issued was %s, which may have lapsed.", cached)
			return nil
		}
		c.Say("  %v", err)
		return nil
	}
	c.Say("  Pairing code: %s  (valid 5 minutes)", code)
	c.Say("  They run: bitbang connect %s", code)
	return nil
}
