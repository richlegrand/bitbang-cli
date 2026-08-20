package main

import (
	"os"
	"path/filepath"
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

// Identities the old derivation left behind are reported, not migrated:
// a machine can hold several and picking one would be a guess.
func TestStrandedIdentities(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // windows

	root := filepath.Join(home, ".bitbang")
	for _, name := range []string{
		"files-srv-share-9263ba", // derived, has a key
		"proxy",                  // derived, has a key
		"bitbang",                // the current identity, not stranded
		"octoprint",              // explicitly pinned, not ours to report
		"files-no-key",           // derived-looking but empty
	} {
		if err := os.MkdirAll(filepath.Join(root, name), 0o700); err != nil {
			t.Fatal(err)
		}
		if name == "files-no-key" {
			continue
		}
		if err := os.WriteFile(filepath.Join(root, name, "identity.pem"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	got := strandedIdentities()
	want := []string{"files-srv-share-9263ba", "proxy"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func TestLooksDerived(t *testing.T) {
	for _, name := range []string{"files", "proxy", "files-srv-share-abc123", "proxy-localhost-8096-def456"} {
		if !looksDerived(name) {
			t.Errorf("%q should look derived", name)
		}
	}
	for _, name := range []string{"bitbang", "octoprint", "myfiles", "filesystem-notes"} {
		if looksDerived(name) {
			t.Errorf("%q should not look derived", name)
		}
	}
}
