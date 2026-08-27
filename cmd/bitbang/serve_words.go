package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/richlegrand/bitbang/internal/allowlist"
	"github.com/richlegrand/bitbang/internal/links"
)

// The `serve` grammar: capability words, each optionally followed by the one
// thing it serves.
//
//	bitbang serve shell proxy a:b,c:d files /home/rich forward g:h,i:j
//
// One rule holds it together: a positional says *what* is being served, a flag
// says *how*. `proxy a:b` and `files /srv` are what; -files-upload, -shell-cmd
// and -proxy-client-ip are how. Before this there was no rule -- the files path
// was positional on one subcommand and a flag on another, the proxy target was
// a positional whose neighbouring flag meant something else, and forwarding's
// allowlist was both at once. Every question anyone asked about these flags
// came from having nothing to appeal to.
//
// Bare `bitbang serve` still means all four, and every single-capability form
// (`serve files ~/share`, `serve proxy host:port`) parses the same as it always
// did -- they are just this grammar with one word.
var capWords = map[string]string{
	"shell":   links.ScopeShell,
	"files":   links.ScopeFiles,
	"proxy":   links.ScopeProxy,
	"forward": links.ScopeForward,
}

// capWordOrder is the order the sharing block and the caret present things,
// least powerful first, independent of the order they were typed.
var capWordOrder = []string{"files", "proxy", "forward", "shell"}

// servePlan is what the words asked for, before defaults are applied.
type servePlan struct {
	caps         capSet
	filesPath    string // "" means cwd
	proxyTargets []string
	forwardAllow []string
}

// parseServeWords reads capability words and their arguments from the
// positionals left after flag parsing.
//
// A word's argument is the next positional, unless that positional is itself a
// capability word -- so `serve files proxy` shares the working directory and
// serves a proxy, rather than sharing a directory called "proxy". A directory
// genuinely named `proxy` needs `./proxy`, which is the one sharp edge here and
// is documented.
func parseServeWords(args []string) (servePlan, error) {
	plan := servePlan{caps: capsOf()}
	if len(args) == 0 {
		// Bare `serve`: everything, as it has always meant.
		return servePlan{caps: capsOf(
			links.ScopeShell, links.ScopeForward, links.ScopeFiles, links.ScopeProxy)}, nil
	}

	seen := map[string]bool{}
	for i := 0; i < len(args); i++ {
		word := args[i]
		scope, ok := capWords[word]
		if !ok {
			return servePlan{}, fmt.Errorf(
				"%q is not something to serve (expected %s)", word, strings.Join(capWordOrder, ", "))
		}
		if seen[word] {
			// Naming one twice is a typo, not a merge: the comma list is
			// how you say several.
			return servePlan{}, fmt.Errorf("%s named twice; separate several with commas", word)
		}
		seen[word] = true
		plan.caps[scope] = true

		// Take the next positional as this word's argument, unless it is
		// another capability word.
		var arg string
		if i+1 < len(args) {
			if _, isWord := capWords[args[i+1]]; !isWord {
				arg = args[i+1]
				i++
			}
		}

		switch word {
		case "shell":
			if arg != "" {
				return servePlan{}, fmt.Errorf(
					"shell takes no argument (%q); the command to run is -shell-cmd", arg)
			}
		case "files":
			plan.filesPath = arg
		case "proxy":
			plan.proxyTargets = splitList(arg)
		case "forward":
			plan.forwardAllow = splitList(arg)
		}
	}
	return plan, nil
}

func splitList(arg string) []string {
	if arg == "" {
		return nil
	}
	var out []string
	for _, p := range strings.Split(arg, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// applyPlan folds the parsed words into the config, and settles the one
// behavior that depends on how many targets a proxy was given.
func applyPlan(cfg *serveConfig, plan servePlan) error {
	cfg.caps = plan.caps
	cfg.filesPath = plan.filesPath

	// A single proxy target pins: with nothing else served, the bare device
	// URL is that app, no landing page. Several are a set the browser picks
	// from. Both are "which targets this proxy may reach", so both also
	// restrict it -- one target is the degenerate case of a list, not a
	// separate feature.
	if len(plan.proxyTargets) == 1 {
		cfg.target = plan.proxyTargets[0]
	}
	if len(plan.proxyTargets) > 0 {
		allowed, err := allowlist.Parse(plan.proxyTargets)
		if err != nil {
			return fmt.Errorf("proxy: %w", err)
		}
		cfg.allowProxy = append(cfg.allowProxy, allowed...)
		cfg.proxyTargets = plan.proxyTargets
	}

	if len(plan.forwardAllow) > 0 {
		allowed, err := allowlist.Parse(plan.forwardAllow)
		if err != nil {
			return fmt.Errorf("forward: %w", err)
		}
		cfg.allowForward = append(cfg.allowForward, allowed...)
	}
	return nil
}

// rejectFlagsWithoutCapability turns "that flag does nothing here" into an
// error naming what is missing. With one `serve` command every capability flag
// is registered, so the check that used to be per-subcommand registration has
// to happen after the words are known.
func rejectFlagsWithoutCapability(set map[string]bool, cfg serveConfig) {
	needs := map[string]string{
		"shell-cmd": links.ScopeShell, "shell-max-sessions": links.ScopeShell,
		"disable-shell-mirror": links.ScopeShell, "shell-restrict": links.ScopeShell,
		"files-upload":    links.ScopeFiles,
		"proxy-client-ip": links.ScopeProxy, "allow-proxy": links.ScopeProxy,
		"allow-forward": links.ScopeForward,
	}
	for name, scope := range needs {
		if set[name] && !cfg.caps.has(scope) {
			fmt.Fprintf(os.Stderr, "bitbang serve: -%s needs %s, which this listener does not serve\n", name, scope)
			os.Exit(2)
		}
	}
}
