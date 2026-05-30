// Package identity handles RSA key pair generation, persistence, UID
// derivation, and access-code generation/persistence.
//
// The identity scheme matches the Python BitBang implementation:
//   - RSA 2048-bit key pair
//   - UID  = base64url(sha256(publicKeyDER)[:16])    128 bits, 22 chars
//   - Code = base64url(8 random bytes)                64 bits, 11 chars
//   - Stored together in ~/.bitbang/<program>/identity.pem as a
//     two-block PEM file (PKCS#8 private key + BITBANG ACCESS CODE).
//     The PEM body for the code block is standard PEM base64 of the raw
//     8 bytes; the URL-facing string form is the same bytes encoded as
//     11 base64url-no-padding chars.
//
// A legacy file (single PEM block, no access code) is rejected on load —
// the v3 signaling server doesn't accept the legacy UID anyway, so the
// only useful path is to ``--ephemeral`` or delete the file and let the
// new format be created.
package identity

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
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

// accessCodePEMType is the PEM block name used to store the access code
// alongside the private key.
const accessCodePEMType = "BITBANG ACCESS CODE"

// accessCodeBytes is the raw length of the access code; 8 bytes = 64 bits,
// displayed as 11 base64url chars (no padding) in the URL fragment.
const accessCodeBytes = 8

// Identity holds an RSA key pair, the derived UID, and the access code.
type Identity struct {
	PrivateKey *rsa.PrivateKey
	UID        string // 22 base64url chars (128 bits)
	Code       string // 11 base64url chars (64 bits) — lives in the URL fragment
	PublicB64  string // base64-encoded DER public key
}

// legacyPrograms is the list of subcommand-specific identity directories
// the old per-cap subcommands wrote to. When loading the unified
// "bitbang" identity, we check these as fallbacks so users who ran any
// of the legacy aliases (bitbang fileshare / bitbang shell /
// bitbangproxy) keep the same URL after upgrading. First-found wins;
// the order reflects which alias was most likely to be the user's
// primary listener.
var legacyPrograms = []string{
	"bitbang-shell",
	"bitbang-fileshare",
	"bitbangproxy",
}

// Load loads an identity from disk, or creates a new one if it doesn't exist.
// If ephemeral is true, a new identity is created in memory without saving.
//
// For programName == "bitbang", a one-shot migration tries the legacy
// alias directories (bitbang-shell, bitbang-fileshare, bitbangproxy)
// and copies the first one found into the new location. This preserves
// the user's URL across the alias-removal transition. The legacy file
// is left in place so any old binary still around keeps working.
//
// A legacy v2 file (no access-code block) is rejected with a clear error;
// the caller should surface it to the user along with a regenerate hint.
func Load(programName string, ephemeral bool) (*Identity, error) {
	if ephemeral {
		return generate()
	}

	dir := identityDir(programName)
	path := filepath.Join(dir, "identity.pem")

	// Try to load existing
	data, err := os.ReadFile(path)
	if err == nil {
		id, loadErr := fromPEM(data)
		if loadErr != nil {
			return nil, fmt.Errorf("%w (at %s)", loadErr, path)
		}
		return id, nil
	}

	// Migration: when loading the unified "bitbang" identity for the
	// first time, try to inherit from a legacy alias directory before
	// generating a new keypair. Same URL across the upgrade.
	if programName == "bitbang" {
		for _, legacy := range legacyPrograms {
			legacyPath := filepath.Join(identityDir(legacy), "identity.pem")
			legacyData, err := os.ReadFile(legacyPath)
			if err != nil {
				continue
			}
			id, parseErr := fromPEM(legacyData)
			if parseErr != nil {
				continue
			}
			// Migrate: write into the new location with 600 perms.
			if mkErr := os.MkdirAll(dir, 0700); mkErr != nil {
				return nil, fmt.Errorf("create identity dir: %w", mkErr)
			}
			if wErr := os.WriteFile(path, legacyData, 0600); wErr != nil {
				return nil, fmt.Errorf("migrate legacy identity: %w", wErr)
			}
			return id, nil
		}
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
	pemData, err := toPEM(id.PrivateKey, id.Code)
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
// bidirectional-verify payload — {fingerprint, nonce, code} — that rides on
// the WebRTC answer message.
func (id *Identity) Decrypt(ciphertext []byte) ([]byte, error) {
	return rsa.DecryptOAEP(sha256.New(), rand.Reader, id.PrivateKey, ciphertext, nil)
}

func generate() (*Identity, error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, fmt.Errorf("generate RSA key: %w", err)
	}
	code, err := generateAccessCode()
	if err != nil {
		return nil, err
	}
	return fromPrivateKeyAndCode(key, code)
}

func generateAccessCode() (string, error) {
	buf := make([]byte, accessCodeBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate access code: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

func fromPrivateKeyAndCode(key *rsa.PrivateKey, code string) (*Identity, error) {
	pubDER, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		return nil, fmt.Errorf("marshal public key: %w", err)
	}

	hash := sha256.Sum256(pubDER)
	uid := base64.RawURLEncoding.EncodeToString(hash[:16])

	return &Identity{
		PrivateKey: key,
		UID:        uid,
		Code:       code,
		PublicB64:  base64.StdEncoding.EncodeToString(pubDER),
	}, nil
}

func fromPEM(data []byte) (*Identity, error) {
	var keyBlock, codeBlock *pem.Block
	rest := data
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		switch block.Type {
		case "PRIVATE KEY":
			if keyBlock == nil {
				keyBlock = block
			}
		case accessCodePEMType:
			if codeBlock == nil {
				codeBlock = block
			}
		}
	}

	if keyBlock == nil {
		return nil, fmt.Errorf("no PRIVATE KEY block in identity file")
	}
	if codeBlock == nil {
		return nil, fmt.Errorf("identity file is legacy v2 format (no access-code block); " +
			"run with --ephemeral or delete the file to generate a v3 identity")
	}

	key, err := x509.ParsePKCS8PrivateKey(keyBlock.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse private key: %w", err)
	}
	rsaKey, ok := key.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("not an RSA key")
	}

	if len(codeBlock.Bytes) != accessCodeBytes {
		return nil, fmt.Errorf("access-code block has wrong length: %d (want %d)",
			len(codeBlock.Bytes), accessCodeBytes)
	}
	code := base64.RawURLEncoding.EncodeToString(codeBlock.Bytes)

	return fromPrivateKeyAndCode(rsaKey, code)
}

func toPEM(key *rsa.PrivateKey, code string) ([]byte, error) {
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("marshal private key: %w", err)
	}
	codeBytes, err := base64.RawURLEncoding.DecodeString(code)
	if err != nil {
		return nil, fmt.Errorf("decode access code: %w", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "PRIVATE KEY",
		Bytes: der,
	})
	codePEM := pem.EncodeToMemory(&pem.Block{
		Type:  accessCodePEMType,
		Bytes: codeBytes,
	})
	out := make([]byte, 0, len(keyPEM)+len(codePEM))
	out = append(out, keyPEM...)
	out = append(out, codePEM...)
	return out, nil
}

func identityDir(programName string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return filepath.Join(home, ".bitbang", programName)
}
