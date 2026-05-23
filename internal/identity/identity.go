// Package identity handles RSA key pair generation, persistence, and UID derivation.
//
// The identity scheme matches the Python BitBang implementation:
//   - RSA 2048-bit key pair
//   - UID = hex(sha256(publicKeyDER))[:32]
//   - Stored as PEM (PKCS#8) in ~/.bitbang/<program>/identity.pem
package identity

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
)

// AuthDomain is the domain separation tag prepended to challenge nonces before
// signing. Must match the signaling server's AUTH_DOMAIN.
//
// Prevents cross-protocol attacks: without this prefix, a malicious server
// could send nonce = SHA256(arbitrary_payload) and reuse the device's
// signature in another context (e.g. firmware verification) that uses the
// same RSA key. Binding every signature to its purpose makes a signature
// from one context structurally invalid in any other.
//
// Bumped only if the signing scheme itself changes (padding/hash/structure),
// not when the surrounding protocol version changes.
var AuthDomain = []byte("bitbang-auth-v1:")

// Identity holds an RSA key pair and the derived UID.
type Identity struct {
	PrivateKey *rsa.PrivateKey
	UID        string        // 32 hex chars
	PublicB64  string        // base64-encoded DER public key
}

// Load loads an identity from disk, or creates a new one if it doesn't exist.
// If ephemeral is true, a new identity is created in memory without saving.
func Load(programName string, ephemeral bool) (*Identity, error) {
	if ephemeral {
		return generate()
	}

	dir := identityDir(programName)
	path := filepath.Join(dir, "identity.pem")

	// Try to load existing
	data, err := os.ReadFile(path)
	if err == nil {
		return fromPEM(data)
	}

	// Generate new
	id, err := generate()
	if err != nil {
		return nil, err
	}

	// Save to disk
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, fmt.Errorf("create identity dir: %w", err)
	}
	pemData, err := toPEM(id.PrivateKey)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, pemData, 0600); err != nil {
		return nil, fmt.Errorf("save identity: %w", err)
	}

	return id, nil
}

// Sign signs a domain-separated challenge nonce using RSASSA-PKCS1-v1_5 with
// SHA-256. The domain tag prevents the auth signature from being interchangeable
// with signatures produced for any other purpose.
//
// Retained for future use (e.g. signing network-membership tokens). The
// signaling server no longer issues a challenge, so this is not called on the
// connection path today.
func (id *Identity) Sign(nonce []byte) ([]byte, error) {
	h := sha256.New()
	h.Write(AuthDomain)
	h.Write(nonce)
	hash := h.Sum(nil)
	return rsa.SignPKCS1v15(rand.Reader, id.PrivateKey, crypto.SHA256, hash)
}

// Decrypt unwraps a ciphertext that was produced by RSA-OAEP/SHA-256
// encryption to this identity's public key. Used to decrypt the browser's
// bidirectional-verify payload — {fingerprint, nonce} — that rides on the
// WebRTC answer message.
func (id *Identity) Decrypt(ciphertext []byte) ([]byte, error) {
	return rsa.DecryptOAEP(sha256.New(), rand.Reader, id.PrivateKey, ciphertext, nil)
}

func generate() (*Identity, error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, fmt.Errorf("generate RSA key: %w", err)
	}
	return fromPrivateKey(key)
}

func fromPrivateKey(key *rsa.PrivateKey) (*Identity, error) {
	pubDER, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		return nil, fmt.Errorf("marshal public key: %w", err)
	}

	hash := sha256.Sum256(pubDER)
	uid := hex.EncodeToString(hash[:])[:32]

	return &Identity{
		PrivateKey: key,
		UID:        uid,
		PublicB64:  base64.StdEncoding.EncodeToString(pubDER),
	}, nil
}

func fromPEM(data []byte) (*Identity, error) {
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, fmt.Errorf("no PEM block found")
	}

	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse private key: %w", err)
	}

	rsaKey, ok := key.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("not an RSA key")
	}

	return fromPrivateKey(rsaKey)
}

func toPEM(key *rsa.PrivateKey) ([]byte, error) {
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("marshal private key: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{
		Type:  "PRIVATE KEY",
		Bytes: der,
	}), nil
}

func identityDir(programName string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return filepath.Join(home, ".bitbang", programName)
}
