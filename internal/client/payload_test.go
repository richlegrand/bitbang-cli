package client

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

// The payload a connector sends answers the device's challenge when the offer
// carries one, in the compact form the listener and bitba.ng's page share
// (bitbang-docs/device-origins.md, "The device challenge"), and keeps the
// older form for a device that sent none.
func TestEncryptVerifyPayloadForms(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	fp := strings.TrimSuffix(strings.Repeat("AB:", 32), ":")
	nonce := []byte("0123456789abcdef")
	const code = "ABCDEFGHIJK"
	const challenge = "BwcHBwcHBwcHBwcHBwcHBw=="

	decrypt := func(b64 string) map[string]string {
		t.Helper()
		ct, err := base64.StdEncoding.DecodeString(b64)
		if err != nil {
			t.Fatal(err)
		}
		plain, err := rsa.DecryptOAEP(sha256.New(), nil, key, ct, nil)
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]string
		if err := json.Unmarshal(plain, &m); err != nil {
			t.Fatal(err)
		}
		return m
	}

	enc, err := encryptVerifyPayload(&key.PublicKey, fp, nonce, code, challenge)
	if err != nil {
		t.Fatalf("with a challenge: %v", err)
	}
	m := decrypt(enc)
	fpBytes, _ := base64.StdEncoding.DecodeString(m["fp"])
	if len(fpBytes) != 32 || fpBytes[0] != 0xAB || m["challenge"] != challenge || m["code"] != code {
		t.Errorf("challenge form: %v", m)
	}
	if _, old := m["fingerprint"]; old {
		t.Errorf("challenge form carries the old fingerprint too: %v", m)
	}

	enc, err = encryptVerifyPayload(&key.PublicKey, fp, nonce, code, "")
	if err != nil {
		t.Fatalf("without: %v", err)
	}
	m = decrypt(enc)
	if m["fingerprint"] != fp || m["code"] != code {
		t.Errorf("older form: %v", m)
	}
	if _, ok := m["challenge"]; ok {
		t.Errorf("older form names a challenge: %v", m)
	}

	if _, err := encryptVerifyPayload(&key.PublicKey, "AB:CD", nonce, code, challenge); err == nil {
		t.Error("accepted a fingerprint that is not sha-256")
	}
}
