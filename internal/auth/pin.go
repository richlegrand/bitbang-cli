// Package auth handles PIN-based authentication for BitBangProxy.
//
// PIN verification happens over the DTLS-encrypted data channel, so the
// signaling server never sees the PIN.
package auth

import (
	"crypto/sha256"
	"crypto/subtle"
)

// PINAuth manages PIN verification.
type PINAuth struct {
	// pinHash is sha256(pin) — fixed 32 bytes, so the constant-time
	// compare in Verify can't leak either timing OR length. The
	// original pin string is never kept after construction.
	pinHash [32]byte
	set     bool
}

// New creates a PINAuth with the given PIN.
// Returns nil if pin is empty (no auth required).
func New(pin string) *PINAuth {
	if pin == "" {
		return nil
	}
	return &PINAuth{pinHash: sha256.Sum256([]byte(pin)), set: true}
}

// Required returns true if PIN authentication is configured.
func (a *PINAuth) Required() bool {
	return a != nil && a.set
}

// Verify checks the PIN. Returns true if correct.
//
// Hashes the attempt to the same fixed 32-byte width as the stored
// PIN hash, then uses crypto/subtle.ConstantTimeCompare so neither the
// character-position timing (Go's plain `==` short-circuits on first
// mismatch) nor the length-vs-correct-length distinction leaks to a
// remote observer. The combination of this with the 2s pinFailDelay in
// session/control.go bounds an attacker's effective rate at ~30
// attempts/minute, sequentially, with no timing side channel.
func (a *PINAuth) Verify(attempt string) bool {
	if a == nil {
		return false
	}
	got := sha256.Sum256([]byte(attempt))
	return subtle.ConstantTimeCompare(a.pinHash[:], got[:]) == 1
}
