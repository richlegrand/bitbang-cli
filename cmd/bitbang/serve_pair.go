package main

import (
	"fmt"
	"log"
	"strings"
	"time"

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
func grantForPairing(c *console, ls *linkState, remoteIP string) (string, bool) {
	if !c.Available() {
		// No terminal to ask on. Refusing here would make pairing
		// impossible on a listener nobody is watching, which is worse
		// than today; granting the default preserves today's behavior
		// while still minting a revocable link rather than handing over
		// the identity's own code.
		return grantDefault(ls, remoteIP)
	}

	var code string
	err := c.Session(func() error {
		c.Say("")
		answer, err := c.Ask("  Grant everything, no expiry?  [Y/n]", "Y")
		if err != nil {
			return err
		}

		terms := links.Terms{Label: pairLabel(ls, time.Now())}
		if !strings.EqualFold(strings.TrimSpace(answer), "y") {
			c.Say("")
			terms, err = grantQuestions(c, terms, ls.offeredScopes(), time.Now())
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
		c.Say("  Paired. %s -- %s", terms.Label, describeGrant(terms, ls.offeredScopes()))
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
	terms := links.Terms{Label: pairLabel(ls, time.Now())}
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
func pairLabel(ls *linkState, now time.Time) string {
	base := "paired-" + strings.ToLower(now.Format("Jan2"))
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
func describeGrant(t links.Terms, offered []string) string {
	scopes := strings.Join(t.Grants(offered), " ")
	if scopes == "" {
		scopes = "(nothing this listener serves)"
	}
	if t.Expires == nil {
		return scopes + ", no expiry"
	}
	return fmt.Sprintf("%s, %s", scopes, relativeTo(*t.Expires, time.Now()))
}
