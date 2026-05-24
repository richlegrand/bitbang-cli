package identity

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGenerateIdentityShape(t *testing.T) {
	id, err := generate()
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if len(id.UID) != 22 {
		t.Errorf("UID length = %d, want 22", len(id.UID))
	}
	if len(id.Code) != 11 {
		t.Errorf("Code length = %d, want 11", len(id.Code))
	}
	if id.PublicB64 == "" {
		t.Error("PublicB64 empty")
	}
	if id.PrivateKey == nil {
		t.Error("PrivateKey nil")
	}
}

func TestPEMRoundTrip(t *testing.T) {
	id, err := generate()
	if err != nil {
		t.Fatalf("generate: %v", err)
	}

	pemBytes, err := toPEM(id.PrivateKey, id.Code)
	if err != nil {
		t.Fatalf("toPEM: %v", err)
	}

	// Sanity: file contains both PEM block types
	s := string(pemBytes)
	if !strings.Contains(s, "BEGIN PRIVATE KEY") {
		t.Error("missing PRIVATE KEY block")
	}
	if !strings.Contains(s, "BEGIN BITBANG ACCESS CODE") {
		t.Error("missing BITBANG ACCESS CODE block")
	}

	got, err := fromPEM(pemBytes)
	if err != nil {
		t.Fatalf("fromPEM: %v", err)
	}
	if got.UID != id.UID {
		t.Errorf("UID mismatch: %q vs %q", got.UID, id.UID)
	}
	if got.Code != id.Code {
		t.Errorf("Code mismatch: %q vs %q", got.Code, id.Code)
	}
}

func TestLegacyPEMRejected(t *testing.T) {
	// Build a v2-style single-block PEM (no access code).
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa.GenerateKey: %v", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("MarshalPKCS8PrivateKey: %v", err)
	}
	legacy := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})

	if _, err := fromPEM(legacy); err == nil {
		t.Fatal("fromPEM accepted legacy single-block file; should have rejected")
	} else if !strings.Contains(err.Error(), "legacy") {
		t.Errorf("legacy error should mention 'legacy', got: %v", err)
	}
}

func TestLoadCreatesAndReloads(t *testing.T) {
	// Redirect HOME so we don't touch the user's real ~/.bitbang.
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	first, err := Load("bitbangproxy-test", false)
	if err != nil {
		t.Fatalf("Load (create): %v", err)
	}

	// File should now exist with both blocks.
	path := filepath.Join(tmp, ".bitbang", "bitbangproxy-test", "identity.pem")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read identity file: %v", err)
	}
	if !strings.Contains(string(data), "BITBANG ACCESS CODE") {
		t.Fatal("written identity is missing access-code block")
	}

	second, err := Load("bitbangproxy-test", false)
	if err != nil {
		t.Fatalf("Load (reload): %v", err)
	}
	if second.UID != first.UID || second.Code != first.Code {
		t.Errorf("reload mismatch: uid=%q/%q code=%q/%q",
			first.UID, second.UID, first.Code, second.Code)
	}
}
