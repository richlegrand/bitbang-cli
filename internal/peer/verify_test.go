package peer

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"testing"

	"github.com/richlegrand/bitbang/internal/identity"
	"github.com/richlegrand/bitbang/internal/protocol"
)

// encryptToPubkey is the test-side mirror of what bootstrap.js does in the
// browser: import the device's PublicB64 (base64 DER), then RSA-OAEP/SHA-256
// encrypt a plaintext payload. Kept in the test file because production
// device code never encrypts, only decrypts.
func encryptToPubkey(t *testing.T, id *identity.Identity, plain []byte) ([]byte, error) {
	t.Helper()
	der, err := base64.StdEncoding.DecodeString(id.PublicB64)
	if err != nil {
		return nil, err
	}
	parsed, err := x509.ParsePKIXPublicKey(der)
	if err != nil {
		return nil, err
	}
	pub, ok := parsed.(*rsa.PublicKey)
	if !ok {
		t.Fatalf("expected *rsa.PublicKey, got %T", parsed)
	}
	return rsa.EncryptOAEP(sha256.New(), rand.Reader, pub, plain, nil)
}

const sampleSDP = `v=0
o=- 0 0 IN IP4 0.0.0.0
s=-
c=IN IP4 0.0.0.0
t=0 0
m=application 9 UDP/DTLS/SCTP webrtc-datachannel
a=ice-ufrag:abcd
a=ice-pwd:zzzzzzzzzzzzzzzzzzzzzz
a=fingerprint:sha-256 AA:BB:CC:DD:EE:FF:00:11:22:33:44:55:66:77:88:99:AA:BB:CC:DD:EE:FF:00:11:22:33:44:55:66:77:88:99
a=setup:active
a=mid:0
a=sctp-port:5000
`

func TestExtractFingerprintLowercaseHash(t *testing.T) {
	got := extractFingerprint(sampleSDP)
	want := "AA:BB:CC:DD:EE:FF:00:11:22:33:44:55:66:77:88:99:AA:BB:CC:DD:EE:FF:00:11:22:33:44:55:66:77:88:99"
	if got != want {
		t.Fatalf("extractFingerprint = %q, want %q", got, want)
	}
}

func TestExtractFingerprintMissing(t *testing.T) {
	if got := extractFingerprint("v=0\nm=application 9 UDP/DTLS/SCTP webrtc-datachannel\n"); got != "" {
		t.Fatalf("expected empty for SDP without fingerprint, got %q", got)
	}
}

func TestFingerprintsEqual(t *testing.T) {
	if !fingerprintsEqual("aa:bb:cc", "AA:BB:CC") {
		t.Fatal("case-insensitive compare failed")
	}
	if fingerprintsEqual("aa:bb", "cc:dd") {
		t.Fatal("distinct fingerprints reported equal")
	}
}

func TestBuildVerifyNonceHashFrameRoundTrip(t *testing.T) {
	nonce := []byte("0123456789ABCDEF")
	frame, err := buildVerifyNonceHashFrame(nonce)
	if err != nil {
		t.Fatalf("buildVerifyNonceHashFrame: %v", err)
	}

	parsed, err := protocol.ParseFrame(frame)
	if err != nil {
		t.Fatalf("ParseFrame: %v", err)
	}
	if parsed.StreamID != 0 {
		t.Fatalf("expected stream 0, got %d", parsed.StreamID)
	}
	if parsed.Flags&protocol.FlagSYN == 0 {
		t.Fatal("expected SYN flag")
	}

	var msg struct {
		Type string `json:"type"`
		Hash string `json:"hash"`
	}
	if err := json.Unmarshal(parsed.Payload, &msg); err != nil {
		t.Fatalf("payload not JSON: %v", err)
	}
	if msg.Type != "verify_nonce_hash" {
		t.Fatalf("type = %q", msg.Type)
	}
	expected := sha256.Sum256(nonce)
	if msg.Hash != base64.StdEncoding.EncodeToString(expected[:]) {
		t.Fatalf("hash mismatch")
	}
}

// TestDecryptRoundTrip verifies that identity.Decrypt unwraps a payload
// encrypted with RSA-OAEP/SHA-256 to the identity's public key. Mirrors the
// browser-side encryption that will live in bootstrap.js.
func TestDecryptRoundTrip(t *testing.T) {
	id, err := identity.Load("bitbang-peer-test", true) // ephemeral
	if err != nil {
		t.Fatalf("identity.Load ephemeral: %v", err)
	}

	plain := []byte(`{"fingerprint":"AA:BB","nonce":"YWFh"}`)

	ct, err := encryptToPubkey(t, id, plain)
	if err != nil {
		t.Fatalf("encryptToPubkey: %v", err)
	}

	got, err := id.Decrypt(ct)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if string(got) != string(plain) {
		t.Fatalf("roundtrip mismatch: got %q, want %q", got, plain)
	}
}
