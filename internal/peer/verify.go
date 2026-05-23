// Bidirectional-verify helpers for the device side: pull the DTLS fingerprint
// out of an SDP, build the "verify_nonce_hash" control frame sent as the
// first stream-0 message after the data channel opens.
package peer

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/richlegrand/bitbang/internal/protocol"
)

// fingerprintLine matches lines like "a=fingerprint:sha-256 AA:BB:..."
// (case-insensitive). Only sha-256 is accepted — every modern browser and
// pion emit this; SDPs with only legacy hashes would be rejected outright.
var fingerprintLine = regexp.MustCompile(`(?i)^a=fingerprint:sha-256\s+([0-9A-F:]+)\s*$`)

// extractFingerprint pulls the sha-256 DTLS fingerprint out of an SDP and
// returns it in normalized (uppercase, colon-separated) form. Returns ""
// if no sha-256 fingerprint is present.
func extractFingerprint(sdp string) string {
	for _, line := range strings.Split(sdp, "\n") {
		line = strings.TrimRight(line, "\r")
		m := fingerprintLine.FindStringSubmatch(line)
		if m != nil {
			return strings.ToUpper(m[1])
		}
	}
	return ""
}

// fingerprintsEqual compares two DTLS fingerprints case-insensitively.
// Browsers and pion both emit uppercase colon-separated hex, but normalizing
// here keeps this resilient to minor formatting differences.
func fingerprintsEqual(a, b string) bool {
	return strings.EqualFold(a, b)
}

// buildVerifyNonceHashFrame returns the SWSP stream-0 SYN frame the device
// sends right after the data channel opens, proving to the browser that it
// successfully decrypted the bidirectional-verify payload. Hash is base64
// sha-256 of the original nonce.
//
// Wire shape: { "type": "verify_nonce_hash", "hash": "<base64>" } as the
// payload of a stream-0 SYN frame. The browser must receive this before any
// proxy traffic flows; if it doesn't match sha256(its own nonce) the
// connection is rejected.
func buildVerifyNonceHashFrame(nonce []byte) ([]byte, error) {
	hash := sha256.Sum256(nonce)
	payload, err := json.Marshal(struct {
		Type string `json:"type"`
		Hash string `json:"hash"`
	}{
		Type: "verify_nonce_hash",
		Hash: base64.StdEncoding.EncodeToString(hash[:]),
	})
	if err != nil {
		return nil, fmt.Errorf("marshal verify_nonce_hash: %w", err)
	}
	return protocol.BuildFrame(0, protocol.FlagSYN, payload), nil
}
