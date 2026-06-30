package main

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestDeriveProgram(t *testing.T) {
	// Shell-bearing configs collapse to the master "bitbang" identity.
	all := serveConfig{shellEnabled: true, filesEnabled: true, proxyEnabled: true}
	if got := deriveProgram(all); got != "bitbang" {
		t.Errorf("all-caps serve: got %q, want \"bitbang\"", got)
	}
	if got := deriveProgram(serveConfig{shellEnabled: true}); got != "bitbang" {
		t.Errorf("shell-only: got %q, want \"bitbang\"", got)
	}

	// Generic single caps get their own stable identity.
	if got := deriveProgram(serveConfig{proxyEnabled: true}); got != "proxy" {
		t.Errorf("generic proxy: got %q, want \"proxy\"", got)
	}
	if got := deriveProgram(serveConfig{filesEnabled: true}); got != "files" {
		t.Errorf("generic files: got %q, want \"files\"", got)
	}

	// An explicit --program always wins.
	if got := deriveProgram(serveConfig{shellEnabled: true, program: "custom"}); got != "custom" {
		t.Errorf("explicit program override: got %q, want \"custom\"", got)
	}

	// Fixed proxy target → per-target identity, prefixed and readable.
	p1 := deriveProgram(serveConfig{proxyEnabled: true, target: "localhost:8096"})
	p2 := deriveProgram(serveConfig{proxyEnabled: true, target: "localhost:3000"})
	if !strings.HasPrefix(p1, "proxy-localhost-8096-") {
		t.Errorf("proxy target slug not readable: %q", p1)
	}
	if p1 == p2 {
		t.Errorf("different targets must yield different identities: %q == %q", p1, p2)
	}

	// Trivially-equivalent targets normalize to the SAME identity.
	if a, b := deriveProgram(serveConfig{proxyEnabled: true, target: "LocalHost:8096/"}),
		deriveProgram(serveConfig{proxyEnabled: true, target: "localhost:8096"}); a != b {
		t.Errorf("equivalent targets must share identity: %q != %q", a, b)
	}

	// Equivalent file paths (relative vs absolute) share one identity.
	abs, _ := filepath.Abs(".")
	if a, b := deriveProgram(serveConfig{filesEnabled: true, filesPath: "."}),
		deriveProgram(serveConfig{filesEnabled: true, filesPath: abs}); a != b {
		t.Errorf("equivalent paths must share identity: %q != %q", a, b)
	}
}

func TestSlug(t *testing.T) {
	// Stable.
	if slug("localhost:8096") != slug("localhost:8096") {
		t.Error("slug not stable")
	}
	// Filesystem/Windows-safe: only [A-Za-z0-9._-].
	for _, in := range []string{"localhost:8096", "/dev/ttyUSB0", "a b\tc", "weird/../path"} {
		s := slug(in)
		for _, r := range s {
			ok := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
				(r >= '0' && r <= '9') || r == '.' || r == '_' || r == '-'
			if !ok {
				t.Errorf("slug(%q)=%q contains unsafe rune %q", in, s, r)
			}
		}
	}
	// The hash suffix prevents sanitization from aliasing distinct inputs:
	// "a:b" and "a-b" sanitize identically but must not collide.
	if slug("a:b") == slug("a-b") {
		t.Error("slug aliased two distinct inputs that sanitize the same")
	}
}
