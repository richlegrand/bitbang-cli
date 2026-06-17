package peer

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"sort"
	"strings"
)

// ComputeSAS returns the 4-digit Short Authentication String derived from the
// two negotiated DTLS fingerprints. Both peers compute SAS the same way: sort
// the (uppercase) fingerprint strings so the operation is symmetric, hash the
// pair with SHA-256, and take the first 32 bits mod 10000 as a zero-padded
// decimal.
//
// The output is identical on both sides if and only if they agree on both
// fingerprints — which is the property a rogue signaling server cannot fake.
// A relay that terminated both DTLS sessions sees different fingerprints on
// each side and the two SAS values diverge; the human comparison (connector
// reads the displayed value, listener types what they hear) detects the
// mismatch.
func ComputeSAS(localFp, remoteFp string) string {
	pair := []string{
		strings.ToUpper(localFp),
		strings.ToUpper(remoteFp),
	}
	sort.Strings(pair)

	h := sha256.Sum256([]byte(pair[0] + "|" + pair[1]))
	code := binary.BigEndian.Uint32(h[:4]) % 10000
	return fmt.Sprintf("%04d", code)
}
