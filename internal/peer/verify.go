// Bidirectional-verify helpers for the listener side: build the
// "verify_nonce_hash" control frame sent as the first stream-0 message
// after the data channel opens. DTLS fingerprint parsing lives in
// internal/sdp so connector and listener parses are guaranteed identical.
package peer

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"

	"github.com/richlegrand/bitbang/internal/protocol"
	"github.com/richlegrand/bitbang/internal/sdp"
)

// extractFingerprint is a local alias for sdp.ExtractFingerprint kept so
// the existing call sites in connection.go don't need to change.
// New code should call sdp.ExtractFingerprint directly.
func extractFingerprint(s string) string { return sdp.ExtractFingerprint(s) }

// fingerprintsEqual is a local alias for sdp.FingerprintsEqual, same
// rationale as extractFingerprint above.
func fingerprintsEqual(a, b string) bool { return sdp.FingerprintsEqual(a, b) }

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
