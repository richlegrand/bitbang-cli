package main

import (
	"fmt"
	"io"
	"strconv"
	"strings"
)

// productKey is the row this binary looks itself up under in the
// versions table the signaling server stamps on the registered reply.
// Other BitBang clients (the OctoPrint plugin, and whatever comes next)
// use their own keys against the same table.
const productKey = "cli"

// updateNotice returns the line to print when the server reports a
// release newer than this build, or "" when there is nothing to say.
//
// The comparison happens here rather than server-side on purpose: the
// client sends neither its version nor its product name, so nothing
// about this installation is disclosed by asking. The server states the
// same table to everyone and we decide locally whether we care.
func updateNotice(versions map[string]string, current string) string {
	latest := versions[productKey]
	if latest == "" || !isNewer(latest, current) {
		return ""
	}
	return fmt.Sprintf("A newer bitbang is available: %s (this is %s)", latest, current)
}

// updateBlock renders the notice together with the command that installs
// it. The server named is the one this process talked to, not a
// compiled-in host: a self-hoster's install endpoint ships the binary
// they built.
func updateBlock(notice, server string) string {
	return fmt.Sprintf("%s\n  curl -fsSL https://%s/install | sh\n", notice, server)
}

// reportUpdate writes the notice to w when the table holds something
// newer, and writes nothing at all otherwise. Informational, not a
// warning: nothing is wrong with the running version, and BitBang does
// not update itself.
//
// Connectors pass stderr. `bitbang cp <url>:/file -` streams the file to
// stdout, so anything printed there would land in the middle of the
// user's data.
func reportUpdate(w io.Writer, versions map[string]string, server string) {
	notice := updateNotice(versions, version)
	if notice == "" {
		return
	}
	fmt.Fprint(w, updateBlock(notice, server))
}

// isNewer reports whether latest is a strictly greater release than
// current, comparing major.minor.patch numerically.
//
// A pre-release suffix on the local build ("0.5.0-dev") is ignored for
// the comparison and then broken in its favor: a dev build of 0.5.0 is
// ahead of released 0.5.0, not behind it, so it must not be nagged to
// "upgrade" to the thing it already contains. Anything unparseable on
// either side returns false -- staying quiet beats a wrong notice.
func isNewer(latest, current string) bool {
	l, lok := parseVersion(latest)
	c, cok := parseVersion(current)
	if !lok || !cok {
		return false
	}
	for i := range l {
		if l[i] != c[i] {
			return l[i] > c[i]
		}
	}
	// Equal numbers. A local pre-release is *behind* the same released
	// version -- 0.5.0-rc1 should take 0.5.0 -- while a plain equal
	// version has nothing to offer.
	return hasPreRelease(current) && !hasPreRelease(latest)
}

// parseVersion reads major.minor.patch, tolerating a leading "v" and
// ignoring any -suffix or +build. Missing components read as zero, so
// "0.5" parses as 0.5.0.
func parseVersion(s string) ([3]int, bool) {
	var out [3]int
	s = strings.TrimPrefix(strings.TrimSpace(s), "v")
	if i := strings.IndexAny(s, "-+"); i >= 0 {
		s = s[:i]
	}
	if s == "" {
		return out, false
	}
	parts := strings.Split(s, ".")
	if len(parts) > 3 {
		return out, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return out, false
		}
		out[i] = n
	}
	return out, true
}

func hasPreRelease(s string) bool {
	return strings.ContainsAny(strings.TrimPrefix(strings.TrimSpace(s), "v"), "-+")
}
