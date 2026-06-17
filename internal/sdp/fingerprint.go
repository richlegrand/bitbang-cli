// Package sdp holds small helpers for parsing SDP — the Session Description
// Protocol that WebRTC peers exchange during offer/answer. Only the bits
// BitBang actually needs live here; this is not a general-purpose SDP parser.
//
// The package exists so that connector-side and listener-side code can
// agree on the exact same parse of the same SDP field — the bidirectional-
// verify protocol requires that both peers extract identical DTLS
// fingerprints from their respective SDPs, byte-for-byte. Duplicating the
// regex across both sides would risk silent drift.
package sdp

import (
	"regexp"
	"strings"
)

// fingerprintLine matches lines like "a=fingerprint:sha-256 AA:BB:..."
// (case-insensitive). Only sha-256 is accepted — every modern browser and
// pion emit this; SDPs with only legacy hash families would be rejected
// outright by the bidirectional-verify protocol.
var fingerprintLine = regexp.MustCompile(`(?i)^a=fingerprint:sha-256\s+([0-9A-F:]+)\s*$`)

// ExtractFingerprint pulls the sha-256 DTLS fingerprint out of an SDP and
// returns it in normalized (uppercase, colon-separated) form. Returns ""
// if no sha-256 fingerprint is present.
//
// The normalization step is the contract that makes bidirectional verify
// reliable: connector-side and listener-side parses of the same SDP must
// produce the exact same string, because the verify protocol compares
// the string the connector encrypted in the {fingerprint, nonce, code}
// payload against the string the listener parses from the same SDP it
// received. Any divergence in case or formatting would silently break
// the protocol.
func ExtractFingerprint(sdp string) string {
	for _, line := range strings.Split(sdp, "\n") {
		line = strings.TrimRight(line, "\r")
		m := fingerprintLine.FindStringSubmatch(line)
		if m != nil {
			return strings.ToUpper(m[1])
		}
	}
	return ""
}

// FingerprintsEqual compares two DTLS fingerprints case-insensitively.
// Browsers and pion both emit uppercase colon-separated hex, but the
// case-insensitive compare keeps this resilient to formatting drift
// from sources we don't control (e.g., third-party SDP munging).
func FingerprintsEqual(a, b string) bool {
	return strings.EqualFold(a, b)
}
