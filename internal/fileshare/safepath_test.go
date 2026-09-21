package fileshare

import (
	"os"
	"path/filepath"
	"testing"
)

// resolvedTempDir is t.TempDir() with symlinks already resolved.
//
// SafePath returns a fully-resolved path, so a test comparing against a raw
// t.TempDir() passes only where the temp directory happens to contain no
// symlinks. That is true on most Linux boxes and false elsewhere: macOS puts
// temp dirs under /var, a symlink to /private/var, and Windows hands back an
// 8.3 short name that resolution expands. Resolving here keeps the comparison
// about SafePath rather than about the host's temp layout.
func resolvedTempDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("resolve temp dir: %v", err)
	}
	return real
}

func TestSafePathInsideBase(t *testing.T) {
	base := resolvedTempDir(t)
	sub := filepath.Join(base, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	got := SafePath(base, "sub")
	if got != sub {
		t.Errorf("SafePath(base, 'sub') = %q, want %q", got, sub)
	}
}

func TestSafePathBaseItself(t *testing.T) {
	base := resolvedTempDir(t)
	got := SafePath(base, "")
	if got != base {
		t.Errorf("SafePath(base, '') = %q, want %q", got, base)
	}
	got = SafePath(base, ".")
	if got != base {
		t.Errorf("SafePath(base, '.') = %q, want %q", got, base)
	}
}

func TestSafePathTraversalRejected(t *testing.T) {
	base := t.TempDir()
	// Make a sibling directory the attacker would want to read.
	parent := filepath.Dir(base)
	sibling := filepath.Join(parent, "sibling")
	_ = os.Mkdir(sibling, 0o755)
	defer os.RemoveAll(sibling)

	cases := []string{
		"../sibling",
		"..",
		"../..",
		"sub/../../sibling",
	}
	for _, in := range cases {
		got := SafePath(base, in)
		if got != "" {
			t.Errorf("SafePath(base, %q) = %q, want empty (traversal)", in, got)
		}
	}
}

func TestSafePathNonexistent(t *testing.T) {
	base := t.TempDir()
	got := SafePath(base, "does-not-exist")
	if got != "" {
		t.Errorf("SafePath of nonexistent path = %q, want empty", got)
	}
}

func TestShouldShow(t *testing.T) {
	cases := []struct {
		name       string
		showHidden bool
		want       bool
	}{
		{"foo.txt", false, true},
		{"foo.txt", true, true},
		{".env", false, false},  // SYSTEM_FILES always hidden
		{".env", true, false},   // even when showHidden
		{".git", false, false},
		{".DS_Store", false, false},
		{"__pycache__", false, false},
		{".hidden", false, false},
		{".hidden", true, true},
	}
	for _, c := range cases {
		got := ShouldShow(c.name, c.showHidden)
		if got != c.want {
			t.Errorf("ShouldShow(%q, %v) = %v, want %v", c.name, c.showHidden, got, c.want)
		}
	}
}

// Sharing a filesystem root -- `serve files D:\` on Windows, `serve files /`
// on Unix -- listed the directory and then refused every file in it, because
// withinBase appended a separator to a base that already ended in one.
// Issue #38.
func TestWithinBaseAtRoot(t *testing.T) {
	sep := string(os.PathSeparator)
	cases := []struct {
		name, path, base string
		want             bool
	}{
		{"file under a root base", sep + "etc", sep, true},
		{"nested under a root base", sep + "etc" + sep + "hosts", sep, true},
		{"the root itself", sep, sep, true},
		// The reason the separator is appended at all: a sibling whose name
		// merely starts with the base must not match.
		{"sibling with a shared prefix", sep + "srv" + sep + "photos-old",
			sep + "srv" + sep + "photos", false},
		{"child of an ordinary base", sep + "srv" + sep + "photos" + sep + "a.jpg",
			sep + "srv" + sep + "photos", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := withinBase(c.path, c.base); got != c.want {
				t.Errorf("withinBase(%q, %q) = %v, want %v", c.path, c.base, got, c.want)
			}
		})
	}
}

// The same thing through SafePath and the real filesystem, since the bug was
// only reachable when filepath.Abs returned a path ending in a separator --
// which only a root does.
func TestSafePathUnderFilesystemRoot(t *testing.T) {
	root, err := filepath.Abs(string(os.PathSeparator))
	if err != nil {
		t.Skipf("no absolute root: %v", err)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) == 0 {
		t.Skipf("cannot read %q: %v", root, err)
	}

	var reached int
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if SafePath(root, e.Name()) != "" {
			reached++
		}
	}
	if reached == 0 {
		t.Errorf("SafePath reached nothing under the root share %q, out of %d entries", root, len(entries))
	}
}
