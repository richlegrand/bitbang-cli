package main

import (
	"testing"

	"github.com/richlegrand/bitbang/internal/links"
)

// One device, one identity. Every mode lands on the same UID, and what a
// given URL reaches is decided by the code presented, not by which
// identity it addresses.
func TestDeriveProgram(t *testing.T) {
	cases := []struct {
		name string
		cfg  serveConfig
	}{
		{"all caps", serveConfig{caps: capsOf(links.ScopeShell, links.ScopeForward, links.ScopeFiles, links.ScopeProxy)}},
		{"shell only", serveConfig{caps: capsOf(links.ScopeShell, links.ScopeForward)}},
		{"files only", serveConfig{caps: capsOf(links.ScopeFiles)}},
		{"proxy only", serveConfig{caps: capsOf(links.ScopeProxy)}},
		// The instance used to fork the identity. It no longer does: two
		// paths, or two targets, are the same device.
		{"files with a path", serveConfig{caps: capsOf(links.ScopeFiles), filesPath: "/srv/share"}},
		{"files with another path", serveConfig{caps: capsOf(links.ScopeFiles), filesPath: "/srv/other"}},
		{"proxy with a target", serveConfig{caps: capsOf(links.ScopeProxy), target: "localhost:8096"}},
		{"proxy with another target", serveConfig{caps: capsOf(links.ScopeProxy), target: "localhost:3000"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := deriveProgram(c.cfg); got != defaultProgram {
				t.Errorf("got %q, want %q", got, defaultProgram)
			}
		})
	}
}

// The explicit flag is the only way to get a second identity now, so it
// has to keep winning.
func TestDeriveProgramExplicitOverride(t *testing.T) {
	cfg := serveConfig{caps: capsOf(links.ScopeProxy), target: "localhost:8096", program: "octoprint"}
	if got := deriveProgram(cfg); got != "octoprint" {
		t.Errorf("got %q, want the pinned name", got)
	}
}
