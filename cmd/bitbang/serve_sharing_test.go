package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/richlegrand/bitbang/internal/allowlist"
	"github.com/richlegrand/bitbang/internal/fileshare"
	"github.com/richlegrand/bitbang/internal/links"
)

// The Sharing block is the listener's answer to "what did I just expose",
// printed once at startup and never asserted anywhere until now. Pin the
// exact wording across the mode matrix, so a refactor that reorganizes how
// capabilities are described cannot quietly reword or drop a line.
func TestSharingBlock(t *testing.T) {
	dir := t.TempDir()
	single := filepath.Join(dir, "one.bin")
	if err := os.WriteFile(single, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	shareDir, err := fileshare.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	shareFile, err := fileshare.New(single)
	if err != nil {
		t.Fatal(err)
	}
	uploads, err := fileshare.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	uploads.UploadEnabled = true

	cases := []struct {
		name  string
		cfg   serveConfig
		share *fileshare.FileShare
		want  []string
	}{
		{
			name: "shell only, defaults",
			cfg:  serveConfig{caps: capsOf(links.ScopeShell), shellMaxSessions: defaultShellMaxSessions},
			want: []string{
				"Sharing:",
				"  • shell  (" + defaultShellLabel() + ", mirroring to console)",
				"",
			},
		},
		{
			name: "shell with a command, session cap, and mirroring",
			cfg: serveConfig{caps: capsOf(links.ScopeShell), shellCmd: "/bin/zsh",
				shellMaxSessions: 3},
			want: []string{
				"Sharing:",
				"  • shell  (/bin/zsh, max 3 concurrent sessions, mirroring to console)",
				"",
			},
		},
		{
			name: "unlimited shell sessions",
			cfg:  serveConfig{caps: capsOf(links.ScopeShell), shellMaxSessions: 0},
			want: []string{
				"Sharing:",
				"  • shell  (" + defaultShellLabel() + ", unlimited concurrent sessions, mirroring to console)",
				"",
			},
		},
		{
			name:  "files, a directory",
			cfg:   serveConfig{caps: capsOf(links.ScopeFiles)},
			share: shareDir,
			want:  []string{"Sharing:", "  • files  (" + dir + ")", ""},
		},
		{
			name:  "files, a directory with uploads",
			cfg:   serveConfig{caps: capsOf(links.ScopeFiles)},
			share: uploads,
			want:  []string{"Sharing:", "  • files  (" + dir + ", uploads enabled)", ""},
		},
		{
			name:  "files, a single file",
			cfg:   serveConfig{caps: capsOf(links.ScopeFiles)},
			share: shareFile,
			want:  []string{"Sharing:", "  • files  (one.bin — single file)", ""},
		},
		{
			name: "proxy, target chosen in the browser",
			cfg:  serveConfig{caps: capsOf(links.ScopeProxy)},
			want: []string{"Sharing:", "  • proxy  (target chosen in browser)", ""},
		},
		{
			name: "proxy, fixed target",
			cfg:  serveConfig{caps: capsOf(links.ScopeProxy), target: "localhost:5000"},
			want: []string{"Sharing:", "  • proxy  (localhost:5000)", ""},
		},
		{
			name:  "all caps, in order",
			cfg:   serveConfig{caps: capsOf(links.ScopeShell, links.ScopeForward, links.ScopeFiles, links.ScopeProxy), shellMaxSessions: defaultShellMaxSessions},
			share: shareDir,
			want: []string{
				"Sharing:",
				"  • shell  (" + defaultShellLabel() + ", mirroring to console)",
				"  • forward (unrestricted targets, chosen by connect -L; max 64 concurrent connections per session; loopback-bound on connector by default)",
				"  • files  (" + dir + ")",
				"  • proxy  (target chosen in browser)",
				"",
			},
		},
		{
			name: "files enabled but no share is silent",
			cfg:  serveConfig{caps: capsOf(links.ScopeFiles)},
			want: []string{"Sharing:", ""},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var buf bytes.Buffer
			printSharingBlock(&buf, c.cfg, c.share)
			got := strings.Split(strings.TrimSuffix(buf.String(), "\n"), "\n")
			if len(got) != len(c.want) {
				t.Fatalf("got %d lines, want %d:\n%q", len(got), len(c.want), buf.String())
			}
			for i := range c.want {
				if got[i] != c.want[i] {
					t.Errorf("line %d:\n got %q\nwant %q", i+1, got[i], c.want[i])
				}
			}
		})
	}
}

// A forward listener started with -allow-forward must say what it can
// actually reach. "unrestricted targets" on a restricted listener would be
// worse than saying nothing.
func TestSharingBlockNamesAllowedForwards(t *testing.T) {
	allow, err := allowlist.Parse([]string{"127.0.0.1:22", "nas.lan"})
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	printSharingBlock(&buf, serveConfig{
		caps:         capsOf(links.ScopeForward),
		allowForward: allow,
	}, nil)
	got := buf.String()
	if strings.Contains(got, "unrestricted") {
		t.Errorf("sharing block says unrestricted on a restricted listener:\n%s", got)
	}
	for _, want := range []string{"127.0.0.1:22", "nas.lan:*"} {
		if !strings.Contains(got, want) {
			t.Errorf("sharing block does not name %q:\n%s", want, got)
		}
	}
}

// The flag is spelled as a negative so it works bare. The zero value is now
// the shipped default -- mirroring on -- which is what a config straight from
// the flag package holds.
func TestSharingBlockShowsMirrorDisabled(t *testing.T) {
	var on, off strings.Builder
	printSharingBlock(&on, serveConfig{
		caps: capsOf(links.ScopeShell), shellMaxSessions: defaultShellMaxSessions,
	}, nil)
	printSharingBlock(&off, serveConfig{
		caps: capsOf(links.ScopeShell), shellMaxSessions: defaultShellMaxSessions,
		disableShellMirror: true,
	}, nil)

	if !strings.Contains(on.String(), "mirroring to console") {
		t.Errorf("default should mirror:\n%s", on.String())
	}
	if strings.Contains(off.String(), "mirroring to console") {
		t.Errorf("-disable-shell-mirror still claims to mirror:\n%s", off.String())
	}
}

// -target is gone: `serve proxy <target>` was always the same thing. And a
// capability flag without its capability is an error naming what is missing,
// rather than a setting that quietly does nothing -- which is what
// `serve -proxy-client-ip` used to be, on a mode that cannot enter
// fixed-target mode at all.
func TestFlagsNeedTheirCapability(t *testing.T) {
	bin := bitbangBinary(t)

	out, err := exec.Command(bin, "serve", "-target", "x:1").CombinedOutput()
	if err == nil || !strings.Contains(string(out), "not defined") {
		t.Errorf("-target still exists: err=%v out=%q", err, out)
	}

	out, err = exec.Command(bin, "serve", "shell", "-proxy-client-ip").CombinedOutput()
	if err == nil {
		t.Error("-proxy-client-ip was accepted without proxy")
	}
	if !strings.Contains(string(out), "needs proxy") {
		t.Errorf("output = %q, want it to name the missing capability", out)
	}

	// With the capability, the same flag is fine.
	out, err = exec.Command(bin, "serve", "proxy", "x:1", "-proxy-client-ip", "-h").CombinedOutput()
	if strings.Contains(string(out), "needs proxy") {
		t.Errorf("-proxy-client-ip rejected alongside proxy: %q", out)
	}
	_ = err
}

// bitbangBinary builds the CLI once for tests that need to exercise argument
// parsing end to end, which is the only way to see a flag package rejection.
func bitbangBinary(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "bitbang")
	build := exec.Command("go", "build", "-o", bin, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	return bin
}
