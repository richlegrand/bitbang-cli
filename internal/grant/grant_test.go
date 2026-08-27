package grant

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want Spec
	}{
		{
			// Unspecified, not empty: the caller decides. A listener reads
			// it as Everything, a link as whatever is offered.
			name: "no words is unspecified",
			in:   "",
			want: Spec{},
		},
		{
			name: "a word with no argument",
			in:   "shell",
			want: Spec{Caps: map[string]bool{ScopeShell: true}},
		},
		{
			name: "a command may be several words",
			in:   "shell tmux attach",
			want: Spec{Caps: map[string]bool{ScopeShell: true},
				ShellArgv: []string{"tmux", "attach"}},
		},
		{
			name: "a command stops at the next capability",
			in:   "shell tmux attach files /srv",
			want: Spec{Caps: map[string]bool{ScopeShell: true, ScopeFiles: true},
				ShellArgv: []string{"tmux", "attach"}, FilesPath: "/srv"},
		},
		{
			// The rule that keeps the grammar unambiguous.
			name: "a capability word is never another word's argument",
			in:   "files proxy",
			want: Spec{Caps: map[string]bool{ScopeFiles: true, ScopeProxy: true}},
		},
		{
			name: "comma lists",
			in:   "proxy a:80,b:80 forward g:22,i:5432",
			want: Spec{Caps: map[string]bool{ScopeProxy: true, ScopeForward: true},
				ProxyTargets: []string{"a:80", "b:80"}, ForwardTargets: []string{"g:22", "i:5432"}},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := ParseString(c.in)
			if err != nil {
				t.Fatalf("ParseString(%q): %v", c.in, err)
			}
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("ParseString(%q) =\n  %+v\nwant\n  %+v", c.in, got, c.want)
			}
		})
	}
}

func TestParseRejections(t *testing.T) {
	for in, want := range map[string]string{
		"wat":                 "not something to serve",
		"proxy a:1 proxy b:2": "named twice",
	} {
		if _, err := ParseString(in); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("ParseString(%q) = %v, want an error mentioning %q", in, err, want)
		}
	}
}

// String round-trips, so a grant can be shown the way it would be typed.
func TestStringRoundTrips(t *testing.T) {
	for _, in := range []string{
		"files /srv",
		"proxy a:80,b:80",
		"forward g:22",
		"files /srv proxy a:80 forward g:22 shell tmux attach",
	} {
		spec, err := ParseString(in)
		if err != nil {
			t.Fatalf("ParseString(%q): %v", in, err)
		}
		again, err := ParseString(spec.String())
		if err != nil {
			t.Fatalf("reparse %q: %v", spec.String(), err)
		}
		if !reflect.DeepEqual(spec, again) {
			t.Errorf("%q round-tripped to %q", in, spec.String())
		}
	}
}

// Narrowing is the whole point: a link may only take away.
func TestNarrow(t *testing.T) {
	listener := func(s string) Spec {
		spec, err := ParseString(s)
		if err != nil {
			t.Fatal(err)
		}
		return spec
	}
	cases := []struct {
		name     string
		listener string
		link     string
		want     string
	}{
		{"a subset of capabilities", "shell files proxy forward", "files", "files"},
		{"a subset of targets", "forward a:22,b:80", "forward a:22", "forward a:22"},
		{"targets on an unrestricted listener", "forward", "forward a:22", "forward a:22"},
		{"no targets keeps the listener's", "forward a:22,b:80", "forward", "forward a:22,b:80"},
		{"a link may pin a shell the listener left open", "shell", "shell /bin/login", "shell /bin/login"},
		{"the listener's command carries through", "shell tmux attach", "shell", "shell tmux attach"},
		{"empty grant takes everything offered", "files /srv proxy a:80", "", "files /srv proxy a:80"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := listener(c.listener).Narrow(listener(c.link))
			if err != nil {
				t.Fatalf("Narrow: %v", err)
			}
			if got.String() != c.want {
				t.Errorf("Narrow = %q, want %q", got.String(), c.want)
			}
		})
	}
}

// Widening is refused at mint time rather than issued as a link that
// silently reaches nothing.
func TestNarrowRefusesWidening(t *testing.T) {
	cases := []struct {
		name     string
		listener string
		link     string
		want     string
	}{
		{"a capability the listener does not serve", "files", "shell", "does not serve shell"},
		{"a target outside the listener's", "forward a:22", "forward b:80", "outside what this listener reaches"},
		{"a proxy target outside the listener's", "proxy a:80", "proxy c:80", "outside what this listener reaches"},
		{"a different shell command", "shell /usr/bin/id", "shell /bin/bash", "cannot change it"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			l, _ := ParseString(c.listener)
			k, _ := ParseString(c.link)
			_, err := l.Narrow(k)
			if err == nil {
				t.Fatalf("Narrow(%q, %q) was allowed", c.listener, c.link)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error = %q, want it to mention %q", err, c.want)
			}
		})
	}
}

// A link may hand out a subdirectory, never a sibling -- and not via "..".
func TestNarrowFilesPath(t *testing.T) {
	base := t.TempDir()
	inside := filepath.Join(base, "public")
	l, _ := ParseString("files " + base)

	k, _ := ParseString("files " + inside)
	got, err := l.Narrow(k)
	if err != nil {
		t.Fatalf("a subdirectory was refused: %v", err)
	}
	if got.FilesPath != inside {
		t.Errorf("FilesPath = %q, want %q", got.FilesPath, inside)
	}

	for _, outside := range []string{
		filepath.Join(base, "..", "elsewhere"),
		"/etc",
	} {
		k, _ := ParseString("files " + outside)
		if _, err := l.Narrow(k); err == nil {
			t.Errorf("%q was accepted as inside %q", outside, base)
		}
	}
}
