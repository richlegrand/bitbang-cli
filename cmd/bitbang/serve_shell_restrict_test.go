package main

import (
	"strings"
	"testing"

	"github.com/richlegrand/bitbang/internal/links"
	"github.com/richlegrand/bitbang/internal/streamtype"
)

// -shell-cmd is a default: a CLI connector overrides it by supplying argv,
// which is what `connect <url> -- cat /etc/passwd` does. -shell-restrict
// turns it into a pin, so the handler must carry ForcedArgv -- the same
// field `share` uses, which also drops the client's env and cwd.
func TestShellRestrictPinsTheCommand(t *testing.T) {
	argv := []string{"/bin/login"}

	unrestricted := shellHandlerFor(t, serveConfig{
		caps: capsOf(links.ScopeShell), shellCmd: "/bin/login",
	}, argv)
	if len(unrestricted.ForcedArgv) != 0 {
		t.Errorf("ForcedArgv = %v without -shell-restrict; -shell-cmd must stay a default",
			unrestricted.ForcedArgv)
	}
	if got := unrestricted.DefaultArgv; len(got) != 1 || got[0] != "/bin/login" {
		t.Errorf("DefaultArgv = %v, want the -shell-cmd argv", got)
	}

	restricted := shellHandlerFor(t, serveConfig{
		caps: capsOf(links.ScopeShell), shellCmd: "/bin/login", shellRestrict: true,
	}, argv)
	if got := restricted.ForcedArgv; len(got) != 1 || got[0] != "/bin/login" {
		t.Errorf("ForcedArgv = %v with -shell-restrict, want the -shell-cmd argv", got)
	}
}

// The sharing block is where an operator checks what they exposed, so a
// pinned shell has to look different from a default one.
func TestSharingBlockSaysWhenTheShellIsPinned(t *testing.T) {
	var b strings.Builder
	printSharingBlock(&b, serveConfig{
		caps: capsOf(links.ScopeShell), shellCmd: "/bin/login",
		shellRestrict: true, shellMaxSessions: defaultShellMaxSessions,
	}, nil)
	if !strings.Contains(b.String(), "/bin/login only") {
		t.Errorf("sharing block does not mark the shell as pinned:\n%s", b.String())
	}
}

// shellHandlerFor builds the shell capability the way a real connection
// does, so the test exercises the wiring rather than restating it.
func shellHandlerFor(t *testing.T, cfg serveConfig, argv []string) *streamtype.ShellHandler {
	t.Helper()
	for _, c := range capabilities {
		if c.Scope != links.ScopeShell {
			continue
		}
		for _, h := range c.Build(capContext{cfg: cfg, shellArgv: argv}) {
			if sh, ok := h.(*streamtype.ShellHandler); ok {
				return sh
			}
		}
	}
	t.Fatal("no shell handler was built")
	return nil
}
