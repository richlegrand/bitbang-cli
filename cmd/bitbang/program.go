package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"path/filepath"
	"strings"
)

// errIdentityBusy is returned by acquireIdentityLock when another local process
// already holds the per-identity lock (see lock_unix.go).
var errIdentityBusy = errors.New("identity already in use by another process")

// deriveProgram maps a serve configuration to the identity "program" name — the
// directory under ~/.bitbang/<program>/ whose keypair fixes the UID (and so the
// shareable URL). Identity is keyed by *access scope*:
//
//   - Any config that includes the shell cap (`serve`, `serve shell`) collapses
//     to the master "bitbang" identity. Shell is the most permissive cap, so a
//     combined listener is no less dangerous than shell alone — one URL — and
//     this preserves the pre-existing URL + the legacy-alias migration.
//   - A single non-shell cap gets its own identity, per instance: a proxy to a
//     fixed target, a shared file path (and later a serial port / forwarded
//     socket) each derive a distinct, stable UID — so they coexist on one
//     machine without preempting each other, and each URL grants only that task.
//   - The generic (instance-less) form of a cap is its own identity too:
//     `proxy` ("proxy anything") is more powerful than a fixed-target proxy.
//
// An explicit --program always wins (used by embedders, e.g. the OctoPrint
// plugin, to pin a shared identity).
func deriveProgram(cfg serveConfig) string {
	if cfg.program != "" {
		return cfg.program
	}
	switch {
	case cfg.shellEnabled:
		return "bitbang"
	case cfg.proxyEnabled:
		if cfg.target != "" {
			return "proxy-" + slug(normalizeTarget(cfg.target))
		}
		return "proxy"
	case cfg.filesEnabled:
		if cfg.filesPath != "" {
			return "files-" + slug(normalizePath(cfg.filesPath))
		}
		return "files"
	default:
		return "bitbang"
	}
}

// slug turns an instance parameter into a filesystem- and Windows-safe,
// collision-resistant identity component: a readable sanitized form plus a
// short hash of the *normalized* value. The hash makes equivalent inputs map to
// one identity and guarantees sanitization can never alias two distinct inputs.
func slug(normalized string) string {
	sum := sha256.Sum256([]byte(normalized))
	short := hex.EncodeToString(sum[:])[:6]

	var b strings.Builder
	for _, r := range normalized {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	san := strings.Trim(b.String(), "-")
	if len(san) > 32 {
		san = strings.Trim(san[:32], "-")
	}
	if san == "" {
		return short
	}
	return san + "-" + short
}

// normalizeTarget canonicalises a proxy target (host:port) so trivially
// equivalent forms share one identity. Host/scheme are case-insensitive and a
// trailing slash is irrelevant.
func normalizeTarget(t string) string {
	return strings.ToLower(strings.TrimRight(strings.TrimSpace(t), "/"))
}

// normalizePath canonicalises a shared filesystem path to its absolute, cleaned
// form so e.g. `serve files .` and `serve files /abs/path` for the same dir
// share one identity. Case is preserved (paths are case-sensitive on the
// device OS).
func normalizePath(p string) string {
	p = strings.TrimSpace(p)
	if abs, err := filepath.Abs(p); err == nil {
		return filepath.Clean(abs)
	}
	return filepath.Clean(p)
}
