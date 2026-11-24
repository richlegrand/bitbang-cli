package peer

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"testing"

	"github.com/richlegrand/bitbang-cli/internal/identity"
)

// The device challenge (bitbang-docs/device-origins.md, "The device
// challenge"): an offer carries 16 random bytes, and the payload that names
// them -- with the fingerprint as its 32 bytes, so it fits RSA-2048 OAEP -- is
// good for that connection only.

var testNonce = []byte("0123456789abcdef")

func challengeConn(t *testing.T, name string) (*Connection, string, *identity.Identity) {
	t.Helper()
	id, err := identity.Load(name, true)
	if err != nil {
		t.Fatalf("identity: %v", err)
	}
	conn, answerSDP := answeringPair(t, id)
	conn.challenge = bytes.Repeat([]byte{7}, ChallengeLen)
	return conn, answerSDP, id
}

// encryptPayload encrypts any payload shape to the device's key.
func encryptPayload(t *testing.T, id *identity.Identity, payload map[string]string) string {
	t.Helper()
	plain, _ := json.Marshal(payload)
	if len(plain) > 190 {
		t.Fatalf("payload is %d bytes; RSA-2048 OAEP/SHA-256 takes at most 190", len(plain))
	}
	ct, err := encryptToPubkey(t, id, plain)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	return base64.StdEncoding.EncodeToString(ct)
}

func fpB64(t *testing.T, sdp string) string {
	t.Helper()
	b, err := fingerprintBytes(extractFingerprint(sdp))
	if err != nil {
		t.Fatalf("fingerprint: %v", err)
	}
	return base64.StdEncoding.EncodeToString(b)
}

func challengePayload(t *testing.T, conn *Connection, sdp, code string) map[string]string {
	return map[string]string{
		"fp":        fpB64(t, sdp),
		"nonce":     base64.StdEncoding.EncodeToString(testNonce),
		"code":      code,
		"challenge": base64.StdEncoding.EncodeToString(conn.challenge),
	}
}

func verifyFailed(conn *Connection) bool {
	conn.mu.Lock()
	defer conn.mu.Unlock()
	return conn.verifyFailed
}

func TestChallengePayloadAccepted(t *testing.T) {
	conn, sdp, id := challengeConn(t, "bitbang-challenge-ok")
	p := challengePayload(t, conn, sdp, id.Code)
	if err := conn.HandleAnswer(sdp, encryptPayload(t, id, p)); err != nil {
		t.Fatalf("HandleAnswer: %v", err)
	}
	conn.mu.Lock()
	defer conn.mu.Unlock()
	if conn.verifyFailed || !bytes.Equal(conn.nonce, testNonce) {
		t.Errorf("verifyFailed=%v nonce=%q", conn.verifyFailed, conn.nonce)
	}
	if conn.challenge != nil {
		t.Error("challenge still held after the answer: it could be used again")
	}
}

func TestChallengePayloadRejected(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mangle func(p map[string]string, conn *Connection)
	}{
		{"another challenge", func(p map[string]string, _ *Connection) {
			p["challenge"] = base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{8}, ChallengeLen))
		}},
		{"challenge not base64", func(p map[string]string, _ *Connection) { p["challenge"] = "!!" }},
		{"another fingerprint", func(p map[string]string, _ *Connection) {
			p["fp"] = base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32))
		}},
		{"short fingerprint", func(p map[string]string, _ *Connection) {
			p["fp"] = base64.StdEncoding.EncodeToString([]byte{1, 2, 3})
		}},
		{"fp without a challenge", func(p map[string]string, _ *Connection) { delete(p, "challenge") }},
		{"challenge with the old fingerprint", func(p map[string]string, _ *Connection) {
			delete(p, "fp")
			p["fingerprint"] = "AA"
		}},
		{"both fingerprints", func(p map[string]string, _ *Connection) { p["fingerprint"] = "AA" }},
		{"no challenge issued", func(_ map[string]string, conn *Connection) { conn.challenge = nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn, sdp, id := challengeConn(t, "bitbang-challenge-bad")
			p := challengePayload(t, conn, sdp, id.Code)
			tc.mangle(p, conn)
			if err := conn.HandleAnswer(sdp, encryptPayload(t, id, p)); err == nil {
				t.Fatal("HandleAnswer accepted it")
			}
			if !verifyFailed(conn) {
				t.Error("verifyFailed not set: the data channel would carry on")
			}
		})
	}
}

// A wrong code fails as a wrong code, whatever the challenge says -- the code
// is checked first, so a bad guess learns nothing about the rest.
func TestChallengePayloadWrongCode(t *testing.T) {
	conn, sdp, id := challengeConn(t, "bitbang-challenge-code")
	p := challengePayload(t, conn, sdp, "WRONGCODE00")
	err := conn.HandleAnswer(sdp, encryptPayload(t, id, p))
	if err == nil || !verifyFailed(conn) {
		t.Fatalf("HandleAnswer: %v", err)
	}
}

// The older payload is still accepted from connectors that predate the
// challenge (bitbang-docs/device-origins.md, "Compatibility"), even when the
// offer carried one.
func TestOldPayloadStillAccepted(t *testing.T) {
	conn, sdp, id := challengeConn(t, "bitbang-challenge-old")
	if err := conn.HandleAnswer(sdp, encryptedRequest(t, id, sdp, id.Code)); err != nil {
		t.Fatalf("HandleAnswer: %v", err)
	}
	if verifyFailed(conn) {
		t.Error("verifyFailed set for an older connector")
	}
}

// What a browser sends, at its largest -- an 11-character code, as every
// access code is -- has to fit the key. (Go's json escapes nothing in these
// fields, so its length is a browser's JSON.stringify length.)
func TestChallengePayloadFits(t *testing.T) {
	conn, sdp, _ := challengeConn(t, "bitbang-challenge-size")
	plain, _ := json.Marshal(challengePayload(t, conn, sdp, "ABCDEFGHIJK"))
	if len(plain) > 190 {
		t.Fatalf("payload is %d bytes, over RSA-2048 OAEP's 190", len(plain))
	}
	t.Logf("payload: %d bytes", len(plain))
}
