// Bidirectional-verify helpers for the listener side: build the
// "verify_nonce_hash" control frame sent as the first stream-0 message
// after the data channel opens. DTLS fingerprint parsing lives in
// internal/sdp so connector and listener parses are guaranteed identical.
package peer

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/richlegrand/bitbang-cli/internal/protocol"
	"github.com/richlegrand/bitbang-cli/internal/sdp"
)

// ChallengeLen is the size of the challenge an offer carries: 16 random
// bytes, sent as standard base64.
const ChallengeLen = 16

// verifyRequest is the decrypted bidirectional-verify payload. Two forms
// (bitbang-docs/device-origins.md, "The device challenge"):
//
//	{fp, nonce, code, challenge}  fp is the answer's DTLS fingerprint as its
//	                              32 bytes in base64, compact so the payload
//	                              fits RSA-2048 OAEP's 190 bytes; challenge
//	                              is the one this connection's offer issued.
//	                              Good for this connection only.
//	{fingerprint, nonce, code}    the older form, fingerprint as SDP writes
//	                              it. Accepted for now, from connectors that
//	                              predate the challenge; it can be replayed
//	                              by whoever captures it.
//
// A payload mixing the two -- a challenge with the old fingerprint, fp with no
// challenge -- is rejected rather than guessed at.
type verifyRequest struct {
	Fingerprint string  `json:"fingerprint"`
	Fp          string  `json:"fp"`
	Nonce       string  `json:"nonce"`
	Code        string  `json:"code"`
	Challenge   *string `json:"challenge"`
}

// errReplayable marks an accepted payload in the older form, so the caller
// can say so.
var errReplayable = errors.New("payload has no challenge")

// checkBinding reports whether the payload belongs to this connection: its
// fingerprint is the answer SDP's, and in the challenge form its challenge is
// the one issued. It returns errReplayable, not nil, for an acceptable
// payload in the older form; any other error rejects.
func (r *verifyRequest) checkBinding(sdpFingerprint string, issued []byte) error {
	if sdpFingerprint == "" {
		return fmt.Errorf("no sha-256 fingerprint in answer SDP")
	}
	if r.Challenge == nil && r.Fp == "" {
		if r.Fingerprint == "" {
			return fmt.Errorf("payload has no fingerprint")
		}
		if !fingerprintsEqual(sdpFingerprint, r.Fingerprint) {
			return fmt.Errorf("DTLS fingerprint mismatch (browser claimed %s, SDP has %s) — possible rogue relay", r.Fingerprint, sdpFingerprint)
		}
		return errReplayable
	}
	if r.Challenge == nil || r.Fp == "" || r.Fingerprint != "" {
		return fmt.Errorf("payload mixes the challenge and older forms")
	}
	if len(issued) != ChallengeLen {
		return fmt.Errorf("payload names a challenge, but this connection issued none")
	}
	got, err := base64.StdEncoding.DecodeString(*r.Challenge)
	if err != nil || subtle.ConstantTimeCompare(got, issued) != 1 {
		return fmt.Errorf("challenge is not this connection's")
	}
	fp, err := base64.StdEncoding.DecodeString(r.Fp)
	want, werr := fingerprintBytes(sdpFingerprint)
	if err != nil || werr != nil || subtle.ConstantTimeCompare(fp, want) != 1 {
		return fmt.Errorf("DTLS fingerprint mismatch (browser claimed %x, SDP has %s) — possible rogue relay", fp, sdpFingerprint)
	}
	return nil
}

// fingerprintBytes turns an SDP fingerprint, "AB:CD:...", into its 32 bytes.
func fingerprintBytes(fp string) ([]byte, error) {
	b, err := hex.DecodeString(strings.ReplaceAll(fp, ":", ""))
	if err != nil {
		return nil, err
	}
	if len(b) != sha256.Size {
		return nil, fmt.Errorf("fingerprint is %d bytes, not %d", len(b), sha256.Size)
	}
	return b, nil
}

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
