package main

import (
	"reflect"
	"strings"
	"testing"

	"github.com/richlegrand/bitbang/internal/links"
)

// The grammar is the CLI's whole shape now, so pin what each form means.
func TestParseServeWords(t *testing.T) {
	cases := []struct {
		name  string
		args  []string
		caps  []string
		files string
		proxy []string
		fwd   []string
	}{
		{
			name: "bare serve is everything, as it always was",
			args: nil,
			caps: []string{links.ScopeShell, links.ScopeForward, links.ScopeFiles, links.ScopeProxy},
		},
		{
			name: "one word, no argument",
			args: []string{"shell"},
			caps: []string{links.ScopeShell},
		},
		{
			name:  "the single-capability forms parse as they did before",
			args:  []string{"files", "/srv/x"},
			caps:  []string{links.ScopeFiles},
			files: "/srv/x",
		},
		{
			name:  "one proxy target",
			args:  []string{"proxy", "nas.lan:8096"},
			caps:  []string{links.ScopeProxy},
			proxy: []string{"nas.lan:8096"},
		},
		{
			name:  "several, comma separated",
			args:  []string{"proxy", "a:80,b:80,c:80"},
			caps:  []string{links.ScopeProxy},
			proxy: []string{"a:80", "b:80", "c:80"},
		},
		{
			name:  "the whole grammar at once",
			args:  []string{"shell", "proxy", "a:80,b:80", "files", "/home/rich", "forward", "g:22,i:5432"},
			caps:  []string{links.ScopeShell, links.ScopeProxy, links.ScopeFiles, links.ScopeForward},
			files: "/home/rich",
			proxy: []string{"a:80", "b:80"},
			fwd:   []string{"g:22", "i:5432"},
		},
		{
			// The rule that keeps this unambiguous: a capability word is
			// never eaten as another word's argument.
			name: "a word is not the previous word's argument",
			args: []string{"files", "proxy"},
			caps: []string{links.ScopeFiles, links.ScopeProxy},
		},
		{
			name: "forward with no targets is unrestricted",
			args: []string{"forward"},
			caps: []string{links.ScopeForward},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			plan, err := parseServeWords(c.args)
			if err != nil {
				t.Fatalf("parseServeWords(%q): %v", c.args, err)
			}
			want := capsOf(c.caps...)
			if !reflect.DeepEqual(map[string]bool(plan.caps), map[string]bool(want)) {
				t.Errorf("caps = %v, want %v", plan.caps, want)
			}
			if plan.filesPath != c.files {
				t.Errorf("filesPath = %q, want %q", plan.filesPath, c.files)
			}
			if !reflect.DeepEqual(plan.proxyTargets, c.proxy) {
				t.Errorf("proxyTargets = %v, want %v", plan.proxyTargets, c.proxy)
			}
			if !reflect.DeepEqual(plan.forwardAllow, c.fwd) {
				t.Errorf("forwardAllow = %v, want %v", plan.forwardAllow, c.fwd)
			}
		})
	}
}

func TestParseServeWordsRejections(t *testing.T) {
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"wat"}, "not something to serve"},
		{[]string{"proxy", "a:1", "proxy", "b:2"}, "named twice"},
		{[]string{"shell", "/bin/bash"}, "shell takes no argument"},
	}
	for _, c := range cases {
		_, err := parseServeWords(c.args)
		if err == nil {
			t.Errorf("parseServeWords(%q) was accepted", c.args)
			continue
		}
		if got := err.Error(); !strings.Contains(got, c.want) {
			t.Errorf("parseServeWords(%q) = %q, want it to mention %q", c.args, got, c.want)
		}
	}
}

// One target pins, several are a set. Both restrict what the proxy may reach,
// which is the property that makes them one feature rather than two.
func TestApplyPlanPinsOneAndRestrictsMany(t *testing.T) {
	var one serveConfig
	if err := applyPlan(&one, servePlan{caps: capsOf(links.ScopeProxy), proxyTargets: []string{"a:80"}}); err != nil {
		t.Fatal(err)
	}
	if one.target != "a:80" {
		t.Errorf("target = %q, want the single target pinned", one.target)
	}
	if one.allowProxy.Empty() || !one.allowProxy.Permits("a", 80) {
		t.Error("a pinned target must also restrict the proxy to it")
	}

	var many serveConfig
	if err := applyPlan(&many, servePlan{caps: capsOf(links.ScopeProxy), proxyTargets: []string{"a:80", "b:80"}}); err != nil {
		t.Fatal(err)
	}
	if many.target != "" {
		t.Errorf("target = %q, want no pin when several are named", many.target)
	}
	if !many.allowProxy.Permits("b", 80) || many.allowProxy.Permits("c", 80) {
		t.Errorf("allowProxy = %v, want exactly the named targets", many.allowProxy)
	}
}
