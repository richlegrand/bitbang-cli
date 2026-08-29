package main

import (
	"strings"
	"testing"

	"github.com/richlegrand/bitbang/internal/grant"
	"github.com/richlegrand/bitbang/internal/links"
	"github.com/richlegrand/bitbang/internal/streamtype"
)

// A command named after `shell` is the command that runs. There is no flag
// to turn that on, and no mode where it is merely a suggestion the far end
// may replace: `serve shell htop` serves htop, and a connector asking for
// something else is refused rather than quietly given htop's output under
// another name.
func TestNamedCommandIsPinned(t *testing.T) {
	sh := shellHandlerFor(t, serveConfig{caps: capsOf(links.ScopeShell)},
		[]string{"tmux", "attach"})
	if got := sh.ForcedArgv; len(got) != 2 || got[0] != "tmux" || got[1] != "attach" {
		t.Errorf("ForcedArgv = %v, want the command named after `shell`", got)
	}
}

// The other half: naming nothing pins nothing, so a connector still gets
// its own command and, failing that, $SHELL.
func TestNoCommandLeavesTheShellOpen(t *testing.T) {
	sh := shellHandlerFor(t, serveConfig{caps: capsOf(links.ScopeShell)}, nil)
	if len(sh.ForcedArgv) != 0 {
		t.Errorf("ForcedArgv = %v on a listener that named no command", sh.ForcedArgv)
	}
}

// A link narrowing an open listener to one command pins it the same way.
// The holder of the link is who the narrowing is against, so there is no
// reading under which they get to pass argv.
func TestLinkNarrowedCommandIsPinned(t *testing.T) {
	sh := shellHandlerFor(t, serveConfig{caps: capsOf(links.ScopeShell)},
		[]string{"/usr/bin/htop"})
	if got := sh.ForcedArgv; len(got) != 1 || got[0] != "/usr/bin/htop" {
		t.Errorf("ForcedArgv = %v, want the command the link named", got)
	}
}

// The sharing block is where an operator checks what they exposed, so a
// pinned shell has to look different from an open one.
func TestSharingBlockSaysWhenTheShellIsPinned(t *testing.T) {
	var b strings.Builder
	printSharingBlock(&b, serveConfig{
		caps: capsOf(links.ScopeShell), shellArgv: []string{"/bin/login"},
		shellMaxSessions: defaultShellMaxSessions,
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
		for _, h := range c.Build(capContext{cfg: cfg, eff: grant.Spec{ShellArgv: argv}}) {
			if sh, ok := h.(*streamtype.ShellHandler); ok {
				return sh
			}
		}
	}
	t.Fatal("no shell handler was built")
	return nil
}
