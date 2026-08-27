package main

import (
	"fmt"
	"os"

	"github.com/richlegrand/bitbang/internal/allowlist"
	"github.com/richlegrand/bitbang/internal/grant"
)

// The `serve` grammar lives in internal/grant, because a link's grant is
// written in it too:
//
//	bitbang serve shell proxy a:80,b:80 files ~/share forward db:5432
//	{"label": "ana", "grant": "forward db:5432"}
//
// One rule holds it together: a positional says *what* is being served, a
// flag says *how*. `proxy a:b` and `files /srv` are what; -files-upload and
// -proxy-client-ip are how. A target therefore has exactly one spelling --
// the argument to its word -- and no flag repeats it.

// applySpec folds a parsed listener grant into the config the rest of the
// listener reads. The derived fields are conveniences over the spec, not a
// second source of truth: everything here is computed from it.
func applySpec(cfg *serveConfig, spec grant.Spec) error {
	if spec.Caps == nil {
		// Bare `serve`: everything. A *link* with no words means something
		// different -- whatever the listener offers -- which is why the
		// parser leaves it unspecified rather than choosing here.
		spec = grant.Everything()
	}
	cfg.offered = spec
	cfg.caps = capSet(spec.Caps)
	cfg.shellArgv = spec.ShellArgv
	cfg.filesPath = spec.FilesPath

	// A single proxy target pins: with nothing else served, the bare device
	// URL is that app, no landing page. Several are a set the browser picks
	// from. Both restrict it, so one target is the degenerate case of a
	// list rather than a separate feature.
	if len(spec.ProxyTargets) == 1 {
		cfg.target = spec.ProxyTargets[0]
	}
	// Parsed here and thrown away, so a bad target is refused when the
	// command is read rather than when someone first dials it.
	if _, err := allowlist.Parse(spec.ProxyTargets); err != nil {
		return fmt.Errorf("proxy: %w", err)
	}
	if _, err := allowlist.Parse(spec.ForwardTargets); err != nil {
		return fmt.Errorf("forward: %w", err)
	}
	return nil
}

// rejectFlagsWithoutCapability turns "that flag does nothing here" into an
// error naming what is missing. One `serve` command registers every
// capability flag, so which ones apply is only known once the words are
// parsed.
func rejectFlagsWithoutCapability(set map[string]bool, cfg serveConfig) {
	needs := map[string]string{
		"shell-max-sessions":   grant.ScopeShell,
		"disable-shell-mirror": grant.ScopeShell,
		"files-upload":         grant.ScopeFiles,
		"proxy-client-ip":      grant.ScopeProxy,
	}
	for name, scope := range needs {
		if set[name] && !cfg.caps.has(scope) {
			fmt.Fprintf(os.Stderr,
				"bitbang serve: -%s needs %s, which this listener does not serve\n", name, scope)
			os.Exit(2)
		}
	}
}
