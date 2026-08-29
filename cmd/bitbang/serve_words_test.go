package main

import (
	"testing"

	"github.com/richlegrand/bitbang/internal/grant"
)

// applySpec is the only grammar logic left in cmd: it folds a parsed grant
// into the config the listener reads. The parser and the narrowing rules are
// tested in internal/grant, so this covers the derivation and nothing else.
func TestApplySpecDerivesFromTheGrant(t *testing.T) {
	var cfg serveConfig
	spec, err := grant.ParseString(`shell "tmux attach" files /srv proxy a:80 forward g:22,i:5432`)
	if err != nil {
		t.Fatal(err)
	}
	if err := applySpec(&cfg, spec); err != nil {
		t.Fatal(err)
	}
	if got := cfg.shellArgv; len(got) != 2 || got[0] != "tmux" {
		t.Errorf("shellArgv = %v", got)
	}
	if cfg.filesPath != "/srv" {
		t.Errorf("filesPath = %q", cfg.filesPath)
	}
	// One proxy target pins; the same target also restricts, so a pin is
	// the degenerate case of a list rather than a separate feature.
	if cfg.target != "a:80" {
		t.Errorf("target = %q, want the single proxy target pinned", cfg.target)
	}
	// The target lists live on the parsed grant, which is what the
	// handlers are built from -- there is no second copy to disagree.
	if allow := allowOf(cfg.offered.ProxyTargets); !allow.Permits("a", 80) || allow.Permits("b", 80) {
		t.Errorf("proxy targets = %v", allow)
	}
	if allow := allowOf(cfg.offered.ForwardTargets); !allow.Permits("i", 5432) || allow.Permits("x", 1) {
		t.Errorf("forward targets = %v", allow)
	}
}

// Bare `serve` means everything. A link with no words means something else
// -- whatever the listener offers -- which is why the parser leaves it
// unspecified and each caller decides.
func TestApplySpecBareServeIsEverything(t *testing.T) {
	var cfg serveConfig
	spec, err := grant.ParseString("")
	if err != nil {
		t.Fatal(err)
	}
	if err := applySpec(&cfg, spec); err != nil {
		t.Fatal(err)
	}
	for _, scope := range []string{grant.ScopeShell, grant.ScopeFiles, grant.ScopeProxy, grant.ScopeForward} {
		if !cfg.caps.has(scope) {
			t.Errorf("bare serve does not offer %s", scope)
		}
	}
}
