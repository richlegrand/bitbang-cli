package pairing

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// This file holds the SAS (code B) and its anti-grind commitment — the crypto
// shared by both the listener (internal/peer) and the connector (cmd/bitbang).
//
// The pairing channel is verified by a 6-digit SAS that both peers compute
// independently from two fresh nonces and the negotiated DTLS fingerprints. A
// commit→challenge→reveal exchange runs first so a man-in-the-middle can't
// grind its substituted fingerprints to force the two sides' SAS values to
// collide: the connector commits its nonce (hidden) before the device reveals
// its challenge nonce, so neither side can choose its contribution after seeing
// what it would need to match. The fingerprints (which differ per leg under a
// relay) are what make the SAS detect a MITM at all; the committed nonces are
// what make that detection non-grindable. See ~/bitbang/code_exchange.md.

// NonceLen is the byte length of the commitment nonces r_c (connector) and
// r_d (device).
const NonceLen = 32

// DC message types exchanged directly over the pairing data channel
// (peer-to-peer, DTLS-encrypted — the signaling server never sees these).
const (
	MsgPairCommit      = "pair_commit"      // connector → device: { commit }
	MsgPairChallenge   = "pair_challenge"   // device → connector: { nonce_d }
	MsgPairReveal      = "pair_reveal"      // connector → device: { nonce_c }
	MsgPairCredentials = "pair_credentials" // device → connector: { uid, public_key, access_code }
)

// NewNonce returns NonceLen cryptographically-random bytes.
func NewNonce() ([]byte, error) {
	n := make([]byte, NonceLen)
	if _, err := rand.Read(n); err != nil {
		return nil, fmt.Errorf("generate nonce: %w", err)
	}
	return n, nil
}

// Commitment returns base64(SHA-256(nonce)) — the hiding+binding commitment the
// connector sends before the device reveals its challenge nonce.
func Commitment(nonce []byte) string {
	h := sha256.Sum256(nonce)
	return base64.StdEncoding.EncodeToString(h[:])
}

// VerifyCommitment reports whether commit == Commitment(nonce), in constant
// time. The device calls this on the connector's revealed nonce to confirm it
// matches the commitment it sent earlier (binding).
func VerifyCommitment(commit string, nonce []byte) bool {
	want := Commitment(nonce)
	return subtle.ConstantTimeCompare([]byte(commit), []byte(want)) == 1
}

// ComputeSAS returns the 6-digit Short Authentication String. Both peers
// compute the same value if and only if they agree on all four inputs:
//
//	rc, rd            the committed connector nonce and the device challenge nonce
//	localFp, remoteFp the two negotiated DTLS fingerprints
//
// Hash = SHA-256( rc ‖ rd ‖ sort(upper(localFp), upper(remoteFp)) ); the SAS is
// the first 32 bits read **big-endian** mod 1_000_000, zero-padded to 6 digits.
// rc and rd are fixed-length (NonceLen) so the concatenation is unambiguous; the
// fingerprints are joined with "|" after sorting so the result is symmetric.
//
// Browser and Python mirrors must reproduce this byte-for-byte (big-endian).
func ComputeSAS(rc, rd []byte, localFp, remoteFp string) string {
	fps := []string{strings.ToUpper(localFp), strings.ToUpper(remoteFp)}
	sort.Strings(fps)

	h := sha256.New()
	h.Write(rc)
	h.Write(rd)
	h.Write([]byte(fps[0] + "|" + fps[1]))
	sum := h.Sum(nil)

	code := binary.BigEndian.Uint32(sum[:4]) % 1_000_000
	return fmt.Sprintf("%06d", code)
}

// PairMessageType returns the "type" field of a data-channel pairing message,
// or "" if the bytes aren't a JSON object with a string type.
func PairMessageType(data []byte) string {
	var m struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal(data, &m)
	return m.Type
}

// --- builders / parsers for the data-channel messages ---------------------

func BuildPairCommit(commit string) []byte {
	b, _ := json.Marshal(map[string]string{"type": MsgPairCommit, "commit": commit})
	return b
}

func ParsePairCommit(data []byte) (commit string, ok bool) {
	var m struct {
		Commit string `json:"commit"`
	}
	if json.Unmarshal(data, &m) != nil || m.Commit == "" {
		return "", false
	}
	return m.Commit, true
}

func BuildPairChallenge(nonceD []byte) []byte {
	b, _ := json.Marshal(map[string]string{
		"type":    MsgPairChallenge,
		"nonce_d": base64.StdEncoding.EncodeToString(nonceD),
	})
	return b
}

func ParsePairChallenge(data []byte) (nonceD []byte, ok bool) {
	var m struct {
		NonceD string `json:"nonce_d"`
	}
	if json.Unmarshal(data, &m) != nil {
		return nil, false
	}
	return decodeNonce(m.NonceD)
}

func BuildPairReveal(nonceC []byte) []byte {
	b, _ := json.Marshal(map[string]string{
		"type":    MsgPairReveal,
		"nonce_c": base64.StdEncoding.EncodeToString(nonceC),
	})
	return b
}

func ParsePairReveal(data []byte) (nonceC []byte, ok bool) {
	var m struct {
		NonceC string `json:"nonce_c"`
	}
	if json.Unmarshal(data, &m) != nil {
		return nil, false
	}
	return decodeNonce(m.NonceC)
}

func BuildPairCredentials(uid, publicKey, accessCode string) []byte {
	b, _ := json.Marshal(map[string]string{
		"type":        MsgPairCredentials,
		"uid":         uid,
		"public_key":  publicKey,
		"access_code": accessCode,
	})
	return b
}

func ParsePairCredentials(data []byte) (uid, publicKey, accessCode string, ok bool) {
	var m struct {
		UID        string `json:"uid"`
		PublicKey  string `json:"public_key"`
		AccessCode string `json:"access_code"`
	}
	if json.Unmarshal(data, &m) != nil || m.UID == "" {
		return "", "", "", false
	}
	return m.UID, m.PublicKey, m.AccessCode, true
}

func decodeNonce(s string) ([]byte, bool) {
	n, err := base64.StdEncoding.DecodeString(s)
	if err != nil || len(n) != NonceLen {
		return nil, false
	}
	return n, true
}
