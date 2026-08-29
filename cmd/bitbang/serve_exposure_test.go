package main

import (
	"strings"
	"testing"

	"github.com/richlegrand/bitbang/internal/grant"
	"github.com/richlegrand/bitbang/internal/links"
)

// The URL is the whole credential, so what it reaches has to be said out
// loud before anyone sends it. The sharing block describes capabilities;
// these notices are for the two that reach past what their name suggests.
func TestExposureNotice(t *testing.T) {
	cfgFor := func(t *testing.T, words string) serveConfig {
		t.Helper()
		spec, err := grant.ParseString(words)
		if err != nil {
			t.Fatal(err)
		}
		var cfg serveConfig
		if err := applySpec(&cfg, spec); err != nil {
			t.Fatal(err)
		}
		return cfg
	}

	for _, tc := range []struct {
		name    string
		words   string
		pinned  bool
		want    string
		warning bool
	}{
		{name: "a shell warns", words: "shell", want: "gets a shell", warning: true},
		{
			// The case this whole notice was added for. Before `forward`
			// became its own capability it could not occur: forwarding
			// only ever shipped beside a shell, which already reached
			// everything it did.
			name:  "unrestricted forwarding warns on its own",
			words: "forward", want: "any host this machine can reach", warning: true,
		},
		{
			// Naming targets is the bound the warning asks for, so having
			// named them must clear it.
			name:  "named targets need no warning",
			words: "forward 127.0.0.1:22", want: "",
		},
		{
			// Not silence by accident: files alone reaches one directory.
			name:  "files alone says nothing",
			words: "files", want: "",
		},
		{
			// A shell outranks it -- one warning, the more serious one.
			name:  "shell and open forwarding warn once",
			words: "shell forward", want: "gets a shell", warning: true,
		},
		{
			// The notices point at --pin, so taking the advice has to
			// clear them rather than leaving a warning that reads as
			// though nothing was done.
			name:  "a PIN replaces the warning",
			words: "forward", pinned: true, want: "PIN protection enabled",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, warning := exposureNotice(cfgFor(t, tc.words), tc.pinned, "", "")
			if tc.want == "" {
				if got != "" {
					t.Errorf("want no notice, got %q", got)
				}
				return
			}
			if !strings.Contains(got, tc.want) {
				t.Errorf("notice = %q, want it to mention %q", got, tc.want)
			}
			if warning != tc.warning {
				t.Errorf("warning = %v, want %v (it picks the stream)", warning, tc.warning)
			}
		})
	}
}

// A bare `bitbang serve` is every capability, so it warns for the shell.
func TestExposureNoticeBareServeWarns(t *testing.T) {
	var cfg serveConfig
	if err := applySpec(&cfg, grant.Spec{}); err != nil {
		t.Fatal(err)
	}
	if !cfg.caps.has(links.ScopeForward) || !cfg.caps.has(links.ScopeShell) {
		t.Fatal("bare serve should offer both")
	}
	got, warning := exposureNotice(cfg, false, "", "")
	if !warning || !strings.Contains(got, "gets a shell") {
		t.Errorf("bare serve notice = %q, warning=%v", got, warning)
	}
}
