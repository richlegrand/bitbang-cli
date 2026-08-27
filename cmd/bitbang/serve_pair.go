package main

import (
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/richlegrand/bitbang/internal/grant"
	"github.com/richlegrand/bitbang/internal/links"
)

// grantForPairing asks what a completed pairing should hand over, mints a
// link for it, and returns its code.
//
// Runs after the SAS has matched, which is the right moment for two
// reasons. The security one: the listener never displays the SAS, so the
// operator has already had to coordinate with the connector to get this
// far, and there is nothing here that can be answered on autopilot. The
// practical one: by now who they are is settled, so the question is only
// what they get.
//
// Returning ok=false declines the pairing, and the connector is told the
// same thing it would have been told by an operator who refused.
func grantForPairing(c *console, ls *linkState, remoteIP string, offered grant.Spec) (string, bool) {
	if !c.Available() {
		// No terminal to ask on. Refusing here would make pairing
		// impossible on a listener nobody is watching, which is worse
		// than today; granting the default preserves today's behavior
		// while still minting a revocable link rather than handing over
		// the identity's own code.
		return grantDefault(ls, remoteIP)
	}

	var code string
	// A verified connector is holding the line through these questions,
	// so they are bounded the way the SAS prompt is.
	waiting := &boundedAsker{c: c, limit: peerWaitLimit}
	err := c.Session(func() error {
		c.Say("")
		answer, err := waiting.Ask("  Grant everything, no expiry?  [Y/n]", "Y")
		if err != nil {
			return err
		}

		terms := links.Terms{Label: datedLabel(ls, "paired", time.Now())}
		if !strings.EqualFold(strings.TrimSpace(answer), "y") {
			c.Say("")
			terms, err = grantQuestions(waiting, terms, offered, ls.takenLabels(""), time.Now())
			if err != nil {
				return err
			}
		}

		minted, err := ls.add(terms)
		if err != nil {
			return err
		}
		code = minted
		c.Say("")
		c.Say("  Paired. %s -- %s", terms.Label, describeGrant(terms, ls.offered))
		c.Say("  %s", ls.url(minted))
		c.Say("")
		return nil
	})
	if err != nil {
		log.Printf("Pair grant abandoned: %v", err)
		return "", false
	}
	return code, true
}

// grantDefault mints the everything-no-expiry link a pairing gets when
// there is no terminal to ask on.
func grantDefault(ls *linkState, remoteIP string) (string, bool) {
	terms := links.Terms{Label: datedLabel(ls, "paired", time.Now())}
	code, err := ls.add(terms)
	if err != nil {
		log.Printf("Pair grant failed: %v", err)
		return "", false
	}
	log.Printf("Paired %s as %q (everything, no expiry)", remoteIP, terms.Label)
	return code, true
}

// pairLabel proposes a dated label, because paired-1 tells you nothing six
// weeks later. Numbered only when the same day already has one.
func datedLabel(ls *linkState, prefix string, now time.Time) string {
	base := prefix + "-" + strings.ToLower(now.Format("Jan2"))
	taken := make(map[string]bool)
	for _, l := range ls.labels() {
		taken[l] = true
	}
	if !taken[base] {
		return base
	}
	for n := 2; ; n++ {
		candidate := fmt.Sprintf("%s-%d", base, n)
		if !taken[candidate] {
			return candidate
		}
	}
}

// describeGrant renders what a link reaches, for the confirmation line.
func describeGrant(t links.Terms, offered grant.Spec) string {
	reach := effectiveWords(t, offered)
	if reach == "" {
		reach = "(nothing this listener serves)"
	}
	if t.Expires == nil {
		return reach + ", no expiry"
	}
	return fmt.Sprintf("%s, %s", reach, relativeTo(*t.Expires, time.Now()))
}

// boundedAsker is the console with a deadline on every question, for
// flows something else is blocked on.
type boundedAsker struct {
	c     *console
	limit time.Duration
}

func (b *boundedAsker) Ask(prompt, def string) (string, error) {
	// AskNow, not AskWithin: the connector is waiting on these answers
	// too, so they interrupt the command loop the same way the SAS did
	// rather than queueing behind it.
	return b.c.AskNow(prompt, def, b.limit)
}

func (b *boundedAsker) Say(format string, args ...interface{}) { b.c.Say(format, args...) }
