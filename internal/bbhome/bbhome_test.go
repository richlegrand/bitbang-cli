package bbhome

import (
	"errors"
	"path/filepath"
	"testing"
)

// The ladder, rung by rung, each with the ones above it cleared. resolve is
// called directly rather than Root so the package-level override stays out of
// it.
func TestResolveOrder(t *testing.T) {
	for _, tc := range []struct {
		name     string
		override string
		env      map[string]string
		want     string
		wantSrc  Source
	}{
		{
			name:     "the flag beats everything",
			override: "/flag",
			env: map[string]string{
				"BITBANG_HOME": "/env", "STATE_DIRECTORY": "/state",
				"XDG_STATE_HOME": "/xdg", "HOME": "/home/u",
			},
			want: filepath.FromSlash("/flag"), wantSrc: SourceFlag,
		},
		{
			name: "BITBANG_HOME beats the rest",
			env: map[string]string{
				"BITBANG_HOME": "/env", "STATE_DIRECTORY": "/state",
				"XDG_STATE_HOME": "/xdg", "HOME": "/home/u",
			},
			want: filepath.FromSlash("/env"), wantSrc: SourceEnv,
		},
		{
			name: "systemd's STATE_DIRECTORY is used as given",
			env: map[string]string{
				"STATE_DIRECTORY": "/var/lib/bitbang",
				"XDG_STATE_HOME":  "/xdg", "HOME": "/home/u",
			},
			want: filepath.FromSlash("/var/lib/bitbang"), wantSrc: SourceSystemd,
		},
		{
			name: "several state directories: the first is the unit's own",
			env: map[string]string{
				"STATE_DIRECTORY": "/var/lib/bitbang:/var/lib/other",
				"HOME":            "/home/u",
			},
			want: filepath.FromSlash("/var/lib/bitbang"), wantSrc: SourceSystemd,
		},
		{
			name: "XDG_STATE_HOME is a parent, so we take a subdirectory",
			env:  map[string]string{"XDG_STATE_HOME": "/xdg/state", "HOME": "/home/u"},
			want: filepath.Join("/xdg/state", "bitbang"), wantSrc: SourceXDG,
		},
		{
			name: "HOME is a parent too, and keeps the dot",
			env:  map[string]string{"HOME": "/home/u"},
			want: filepath.Join("/home/u", ".bitbang"), wantSrc: SourceHome,
		},
		{
			name: "a trailing slash does not survive",
			env:  map[string]string{"BITBANG_HOME": "/env/dir/"},
			want: filepath.FromSlash("/env/dir"), wantSrc: SourceEnv,
		},
		{
			name: "an empty STATE_DIRECTORY is skipped, not obeyed",
			env:  map[string]string{"STATE_DIRECTORY": "", "HOME": "/home/u"},
			want: filepath.Join("/home/u", ".bitbang"), wantSrc: SourceHome,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearEnv(t)
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			// os.UserHomeDir reads USERPROFILE on Windows, so a case that
			// expects the home rung has to set both or it only tests unix.
			if v, ok := tc.env["HOME"]; ok {
				t.Setenv("USERPROFILE", v)
			}
			got, src, err := resolve(tc.override)
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			if got != tc.want {
				t.Errorf("resolve = %q, want %q", got, tc.want)
			}
			if src != tc.wantSrc {
				t.Errorf("source = %q, want %q", src, tc.wantSrc)
			}
		})
	}
}

// The bug this package was written for: with nowhere to put its files the
// listener used to fall back to ".", so a service started without HOME wrote
// its keypair relative to whatever directory it happened to start in. It has
// to refuse instead.
func TestResolveRefusesRatherThanGuessing(t *testing.T) {
	clearEnv(t)
	got, _, err := resolve("")
	if !errors.Is(err, ErrNoHome) {
		t.Fatalf("resolve err = %v, want ErrNoHome", err)
	}
	if got != "" {
		t.Errorf("resolve returned %q alongside an error; a caller that "+
			"ignores the error would write there", got)
	}
}

func TestSetOverridesTheEnvironment(t *testing.T) {
	clearEnv(t)
	t.Setenv("HOME", "/home/u")
	t.Setenv("USERPROFILE", "/home/u")
	t.Cleanup(func() { Set("") })

	Set("/explicit")
	got, err := Root()
	if err != nil {
		t.Fatalf("Root: %v", err)
	}
	if want := filepath.FromSlash("/explicit"); got != want {
		t.Errorf("Root = %q, want %q", got, want)
	}
}

// Root reads the environment every time. A cached answer would survive a
// test pointing HOME at a temp directory, which is how the first draft of
// this ended up reading the real device table.
func TestRootFollowsAChangedHome(t *testing.T) {
	clearEnv(t)
	t.Cleanup(func() { Set("") })
	Set("")

	t.Setenv("HOME", "/home/first")
	t.Setenv("USERPROFILE", "/home/first")
	first, err := Root()
	if err != nil {
		t.Fatalf("Root: %v", err)
	}
	t.Setenv("HOME", "/home/second")
	t.Setenv("USERPROFILE", "/home/second")
	second, err := Root()
	if err != nil {
		t.Fatalf("Root: %v", err)
	}
	if first == second {
		t.Fatalf("Root returned %q both times; it is caching", first)
	}
	if second != filepath.Join("/home/second", ".bitbang") {
		t.Errorf("Root = %q, want /home/second/.bitbang", second)
	}
}

// clearEnv blanks every variable the ladder consults, so a case only sees
// what it sets. HOME blank makes os.UserHomeDir fail, which is the point.
func clearEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"BITBANG_HOME", "STATE_DIRECTORY", "XDG_STATE_HOME", "HOME", "USERPROFILE",
	} {
		t.Setenv(k, "")
	}
}
