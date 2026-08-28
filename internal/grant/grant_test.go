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
			in:   `shell "tmux attach"`,
			want: Spec{Caps: map[string]bool{ScopeShell: true},
				ShellArgv: []string{"tmux", "attach"}},
		},
		{
			name: "a command stops at the next capability",
			in:   `shell "tmux attach" files /srv`,
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
		`files /srv proxy a:80 forward g:22 shell "tmux attach"`,
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
		{"the listener's command carries through", `shell "tmux attach"`, "shell", `shell "tmux attach"`},
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

// A command with arguments reaches us as one token, because the shell
// stripped the quotes: `serve shell "ssh -p 2222 host"` is one argv
// element. Treating it as a filename is what produced
// `fork/exec /usr/bin/ssh 127.0.0.1: no such file or directory`.
func TestParse_QuotedShellCommandBecomesArgv(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want []string
	}{
		{"quoted, with a flag of its own",
			[]string{"shell", "ssh -p 2222 host"},
			[]string{"ssh", "-p", "2222", "host"}},
		{"quoted, two words",
			[]string{"shell", "tmux attach"},
			[]string{"tmux", "attach"}},
		{"one word stays one word",
			[]string{"shell", "/bin/login"},
			[]string{"/bin/login"}},
		// The escape hatch for a path with a space: quote again inside.
		{"inner quotes protect a spaced path",
			[]string{"shell", "'/opt/my app/bin' --login"},
			[]string{"/opt/my app/bin", "--login"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec, err := Parse(tc.args)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if strings.Join(spec.ShellArgv, "\x00") != strings.Join(tc.want, "\x00") {
				t.Errorf("ShellArgv = %q, want %q", spec.ShellArgv, tc.want)
			}
		})
	}
}

// A grant in a file goes through both splits: the line into words, then the
// command field into argv. Quoting has to survive each, which is why the
// command is quoted as a whole and its spaced argument quoted again inside.
func TestParseString_QuotesSurviveBothSplits(t *testing.T) {
	spec, err := ParseString(`shell "'/opt/my app/bin' --login"`)
	if err != nil {
		t.Fatalf("ParseString: %v", err)
	}
	want := []string{"/opt/my app/bin", "--login"}
	if strings.Join(spec.ShellArgv, "\x00") != strings.Join(want, "\x00") {
		t.Errorf("ShellArgv = %q, want %q", spec.ShellArgv, want)
	}
}

// Whatever String writes, ParseString has to read back -- links.json stores
// exactly this, and a grant that changed meaning on reload would hand out
// something nobody wrote.
func TestSpec_RoundTripsArgumentsWithSpaces(t *testing.T) {
	for _, argv := range [][]string{
		{"ssh", "-p", "2222", "host"},
		{"/opt/my app/bin", "--login"},
		{"sh", "-c", "echo one two"},
	} {
		orig := Spec{Caps: map[string]bool{ScopeShell: true}, ShellArgv: argv}
		back, err := ParseString(orig.String())
		if err != nil {
			t.Fatalf("ParseString(%q): %v", orig.String(), err)
		}
		if strings.Join(back.ShellArgv, "\x00") != strings.Join(argv, "\x00") {
			t.Errorf("%q -> %q -> %q", argv, orig.String(), back.ShellArgv)
		}
	}
}

func TestSplitFields_UnbalancedQuoteIsAnError(t *testing.T) {
	if _, err := ParseString(`shell "ssh -p 2222`); err == nil {
		t.Error("an unbalanced quote parsed cleanly; it would silently drop the rest")
	}
}

// A command of several words has to be quoted. Unquoted, the second word is
// read as a capability -- which is the whole reason the grammar gives shell
// exactly one argument like every other word, rather than guessing where a
// command ends.
func TestParse_UnquotedMultiWordCommandIsAnError(t *testing.T) {
	for _, args := range [][]string{
		{"shell", "tmux", "attach"},
		{"shell", "ssh", "-l", "alice", "host"},
	} {
		if _, err := Parse(args); err == nil {
			t.Errorf("Parse(%q) succeeded; an unquoted command must not parse", args)
		}
	}
	// And the word really is a capability when that is what was meant.
	spec, err := Parse([]string{"shell", "echo hi", "forward"})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !spec.Has(ScopeForward) || strings.Join(spec.ShellArgv, " ") != "echo hi" {
		t.Errorf("argv=%q forward=%v", spec.ShellArgv, spec.Has(ScopeForward))
	}
}
