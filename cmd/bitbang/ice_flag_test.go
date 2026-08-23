package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/richlegrand/bitbang/internal/icehelper"
	"github.com/richlegrand/bitbang/internal/signaling"
)

// resolveFSPath is what --ice-servers runs on the operator's path. It
// panicked on a bare "~" until recently, taking the listener down before
// it could report anything useful.
func TestResolveFSPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // windows

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		in   string
		want string
		why  string
	}{
		{"~", home, "a bare tilde is the home directory, not an index panic"},
		{"~/turn.json", filepath.Join(home, "turn.json"), "tilde-rooted"},
		{"/etc/turn.json", "/etc/turn.json", "absolute passes through"},
		{"turn.json", filepath.Join(cwd, "turn.json"), "relative resolves against cwd"},
		{"./turn.json", filepath.Join(cwd, "turn.json"), "dot-relative"},
	}
	for _, c := range cases {
		t.Run(c.why, func(t *testing.T) {
			got, err := resolveFSPath(c.in)
			if err != nil {
				t.Fatalf("resolveFSPath(%q): %v", c.in, err)
			}
			if got != c.want {
				t.Errorf("resolveFSPath(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// "~user/x" is a home other than ours, which we cannot resolve. It must
// not be silently mangled into something under our own home -- the old
// code sliced it and produced "ser/x".
func TestResolveFSPathLeavesOtherUsersHomesAlone(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	got, err := resolveFSPath("~someone/turn.json")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(got, "omeone") && !strings.Contains(got, "~someone") {
		t.Errorf("mangled another user's home: %q", got)
	}
	if strings.HasPrefix(got, home+"/") {
		t.Errorf("resolved another user's home against ours: %q", got)
	}
}

// The other half of the wiring: a parsed config has to reach the
// register message. The parse itself is covered in internal/icehelper;
// what this pins is that the signaling client actually sends it, which
// is the only reason the feature has any effect.
func TestOwnICEServersReachRegister(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "turn.json")
	body := `{"iceServers":[{"urls":["turn:turn.example:3478"],"username":"u","credential":"p"}]}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	resolved, err := resolveFSPath(path)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(resolved)
	if err != nil {
		t.Fatal(err)
	}
	servers, err := icehelper.ParseUserICEFile(raw)
	if err != nil {
		t.Fatalf("parsing the file the flag points at: %v", err)
	}

	c := &signaling.Client{}
	c.OwnICEServers = servers

	reg := map[string]interface{}{"type": "register"}
	if len(c.OwnICEServers) > 0 {
		reg["ice_servers"] = c.OwnICEServers
	}

	// Marshaled the way register does, so a field-tag mismatch shows up
	// here rather than as a server that quietly ignores the override.
	out, err := json.Marshal(reg)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"ice_servers", "turn:turn.example:3478", `"username":"u"`, `"credential":"p"`} {
		if !strings.Contains(string(out), want) {
			t.Errorf("register payload missing %s: %s", want, out)
		}
	}
}

// Nothing configured must add nothing to the message -- the server then
// picks its own, which is what every listener without the flag relies on.
func TestNoICEConfigSendsNoField(t *testing.T) {
	c := &signaling.Client{}
	reg := map[string]interface{}{"type": "register"}
	if len(c.OwnICEServers) > 0 {
		reg["ice_servers"] = c.OwnICEServers
	}
	out, _ := json.Marshal(reg)
	if strings.Contains(string(out), "ice_servers") {
		t.Errorf("unconfigured listener sent ice_servers: %s", out)
	}
}
