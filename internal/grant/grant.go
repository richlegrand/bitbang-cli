// Package grant defines what a listener offers and what a link may reach,
// in one vocabulary with one parser.
//
// The same sentence describes both:
//
//	bitbang serve shell proxy a:80,b:80 files ~/share forward db:5432
//	{"label": "ana", "grant": "forward db:5432", ...}
//
// The listener's words say what exists; a link's words say which part of it
// this holder gets. Sharing the grammar is not tidiness -- it means one
// parser, one set of error messages, and one definition of what "narrower"
// means, rather than a second syntax for the console and a third for the
// file.
package grant

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/richlegrand/bitbang/internal/allowlist"
)

// The scope vocabulary. These names are permanent in a way flags are not:
// they live in config files people keep, so a stream type can be renamed or
// split later without invalidating anything anyone wrote.
//
// What each one reaches is the listener's business. Two need saying:
//
//   - Proxy is one name for both http and websocket streams. Granting one
//     without the other yields a proxy that half works, and nobody would
//     predict that.
//   - No scope gates the listener's own browser UI. That UI is the shell the
//     other scopes act through -- a files-only link still has to render a
//     file browser -- so it rides on every link and shows only what the link
//     actually grants.
const (
	ScopeFiles   = "files"
	ScopeShell   = "shell"
	ScopeForward = "forward"
	ScopeProxy   = "proxy"
)

// Word is a capability name as typed. The vocabulary is links' scope
// vocabulary, which is permanent: these names live in files people keep.
var words = map[string]string{
	"shell":   ScopeShell,
	"files":   ScopeFiles,
	"proxy":   ScopeProxy,
	"forward": ScopeForward,
}

// Order is how capabilities are listed back to a human, least powerful
// first, whatever order they were typed in.
var Order = []string{"files", "proxy", "forward", "shell"}

// Spec is a grant: which capabilities, and the one thing each of them
// serves. The zero value grants nothing.
type Spec struct {
	Caps map[string]bool

	// ShellArgv is the command a shell runs. Empty means the platform shell.
	ShellArgv []string
	// FilesPath is the directory or file shared. Empty means the caller's
	// default, which for a listener is the working directory.
	FilesPath string
	// ProxyTargets are the web apps offered. Empty means the browser names
	// its own.
	ProxyTargets []string
	// ForwardTargets are what `connect -L` may reach. Empty means anything
	// the listener can route to.
	ForwardTargets []string
}

// Has reports whether the spec grants a scope.
func (s Spec) Has(scope string) bool { return s.Caps[scope] }

// Everything is the grant a listener has when no words were typed.
//
// A *link* with no words is a different thing -- it means "whatever this
// listener offers", which is not the same as naming all four, since a
// listener serving two of them would then be asked for capabilities it does
// not have. That case is an unspecified Spec (nil Caps), which Narrow reads
// as "all of the listener's".
func Everything() Spec {
	return Spec{Caps: map[string]bool{
		ScopeShell: true, ScopeFiles: true,
		ScopeProxy: true, ScopeForward: true,
	}}
}

// Parse reads capability words and their arguments.
//
// A word's argument is the following token unless that token is itself a
// capability word, so `files proxy` shares the working directory and serves
// a proxy rather than sharing a directory called "proxy". A directory
// genuinely named `proxy` needs `./proxy`.
func Parse(args []string) (Spec, error) { return parse(args) }

// ParseString reads a grant written as one line, the form links.json holds.
//
// Quotes are honored here rather than by a shell, so an argument containing
// a space survives: `shell "/opt/my app/bin" --login`.
func ParseString(s string) (Spec, error) {
	fields, err := splitFields(s)
	if err != nil {
		return Spec{}, err
	}
	return parse(fields)
}

// parse is the grammar.
func parse(args []string) (Spec, error) {
	if len(args) == 0 {
		// Unspecified, not empty. The caller decides what that means: a
		// listener takes Everything, a link takes whatever is offered.
		return Spec{}, nil
	}
	spec := Spec{Caps: map[string]bool{}}
	seen := map[string]bool{}

	for i := 0; i < len(args); i++ {
		word := args[i]
		scope, ok := words[word]
		if !ok {
			return Spec{}, fmt.Errorf("%q is not something to serve (expected %s)",
				word, strings.Join(Order, ", "))
		}
		if seen[word] {
			// Naming one twice is a typo, not a merge: a comma list is how
			// you say several.
			return Spec{}, fmt.Errorf("%s named twice; separate several with commas", word)
		}
		seen[word] = true
		spec.Caps[scope] = true

		var arg string
		if i+1 < len(args) {
			if _, isWord := words[args[i+1]]; !isWord {
				arg = args[i+1]
				i++
			}
		}

		switch word {
		case "shell":
			// One argument, like every other word. It is a command line, so
			// it is split into argv here -- `shell "ssh -p 2222 host"`.
			// Quoting is the only way to spell a command of several words,
			// which is what keeps `shell tmux attach forward` from having
			// to guess where the command ends and the next word begins.
			if arg != "" {
				argv, err := splitFields(arg)
				if err != nil {
					return Spec{}, fmt.Errorf("shell: %w", err)
				}
				spec.ShellArgv = argv
			}
		case "files":
			spec.FilesPath = arg
		case "proxy":
			spec.ProxyTargets = splitList(arg)
		case "forward":
			spec.ForwardTargets = splitList(arg)
		}
	}
	return spec, nil
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

// String renders a spec back into the grammar, so a grant can be shown the
// way it would be typed.
func (s Spec) String() string {
	var b strings.Builder
	for _, w := range Order {
		if !s.Caps[words[w]] {
			continue
		}
		if b.Len() > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(w)
		switch w {
		case "shell":
			if len(s.ShellArgv) > 0 {
				b.WriteByte(' ')
				b.WriteString(quoteCommand(s.ShellArgv))
			}
		case "files":
			if s.FilesPath != "" {
				b.WriteByte(' ')
				b.WriteString(quoteField(s.FilesPath))
			}
		case "proxy":
			if len(s.ProxyTargets) > 0 {
				b.WriteByte(' ')
				b.WriteString(strings.Join(s.ProxyTargets, ","))
			}
		case "forward":
			if len(s.ForwardTargets) > 0 {
				b.WriteByte(' ')
				b.WriteString(strings.Join(s.ForwardTargets, ","))
			}
		}
	}
	return b.String()
}

// Intersect drops capabilities the listener does not serve, leaving each
// remaining one's argument alone.
//
// Resolving a session is lenient about capabilities on purpose: a
// links.json written for a fuller listener keeps working for whatever
// overlaps, and Build already warns at load about the rest. Targets, paths
// and commands are a different matter -- silently narrowing one of those
// would hand out something the operator did not write -- so Narrow stays
// strict about them.
func (s Spec) Intersect(offered Spec) Spec {
	if s.Caps == nil {
		return s
	}
	out := s
	out.Caps = map[string]bool{}
	for scope := range s.Caps {
		if offered.Caps[scope] {
			out.Caps[scope] = true
		}
	}
	return out
}

// Narrow returns what a holder of `link` actually reaches on a listener
// offering `s`.
//
// A link may only take away. Every rule below is the same rule wearing a
// different coat: the listener's grant is the ceiling, and a link that
// reaches past it is refused at mint time rather than issued as something
// that silently reaches nothing.
func (s Spec) Narrow(link Spec) (Spec, error) {
	out := Spec{Caps: map[string]bool{}}

	// An unspecified link takes everything the listener offers.
	if link.Caps == nil {
		link.Caps = s.Caps
	}

	for _, w := range Order {
		scope := words[w]
		if !link.Caps[scope] {
			continue
		}
		if !s.Caps[scope] {
			return Spec{}, fmt.Errorf("this listener does not serve %s (it serves %s)",
				w, strings.Join(s.Words(), ", "))
		}
		out.Caps[scope] = true
	}

	// The shell command: a link may pin one where the listener left it open.
	// It may not choose a different one than the listener already pinned --
	// that would be a link running something the operator did not offer.
	switch {
	case len(link.ShellArgv) == 0:
		out.ShellArgv = s.ShellArgv
	case len(s.ShellArgv) == 0:
		out.ShellArgv = link.ShellArgv
	case strings.Join(link.ShellArgv, " ") != strings.Join(s.ShellArgv, " "):
		return Spec{}, fmt.Errorf("this listener runs %q; a link cannot change it",
			strings.Join(s.ShellArgv, " "))
	default:
		out.ShellArgv = s.ShellArgv
	}

	// The files path: a link may hand out a subdirectory of what the
	// listener shares, never a sibling.
	switch {
	case link.FilesPath == "":
		out.FilesPath = s.FilesPath
	case s.FilesPath == "":
		out.FilesPath = link.FilesPath
	default:
		within, err := isWithin(s.FilesPath, link.FilesPath)
		if err != nil {
			return Spec{}, err
		}
		if !within {
			return Spec{}, fmt.Errorf("%s is not inside %s, which is what this listener shares",
				link.FilesPath, s.FilesPath)
		}
		out.FilesPath = link.FilesPath
	}

	var err error
	if out.ProxyTargets, err = narrowTargets("proxy", s.ProxyTargets, link.ProxyTargets); err != nil {
		return Spec{}, err
	}
	if out.ForwardTargets, err = narrowTargets("forward", s.ForwardTargets, link.ForwardTargets); err != nil {
		return Spec{}, err
	}
	return out, nil
}

// narrowTargets keeps the link's targets when the listener allows them. An
// unrestricted listener accepts any, which is how a link narrows something
// that was open.
func narrowTargets(what string, listener, link []string) ([]string, error) {
	if len(link) == 0 {
		return listener, nil
	}
	if len(listener) == 0 {
		return link, nil
	}
	allowed, err := allowlist.Parse(listener)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	for _, t := range link {
		if !allowed.PermitsTarget(t) {
			return nil, fmt.Errorf("%s %s is outside what this listener reaches (%s)",
				what, t, allowed)
		}
	}
	return link, nil
}

// isWithin reports whether child is path-contained by parent. Both are
// cleaned and made absolute first, so `~/share/../etc` cannot masquerade as
// a subdirectory.
func isWithin(parent, child string) (bool, error) {
	p, err := filepath.Abs(parent)
	if err != nil {
		return false, err
	}
	c, err := filepath.Abs(child)
	if err != nil {
		return false, err
	}
	rel, err := filepath.Rel(p, c)
	if err != nil {
		return false, nil
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)), nil
}

// Words lists the capability words this spec grants, in presentation order.
func (s Spec) Words() []string {
	var out []string
	for _, w := range Order {
		if s.Caps[words[w]] {
			out = append(out, w)
		}
	}
	return out
}

// splitFields splits a line into tokens on whitespace, honoring single and
// double quotes so an argument containing a space stays one token.
//
// A grant lives in a file as well as on a command line, and in the file
// there is no shell to do this. Without it, `shell "/opt/my app/bin"` in
// links.json would parse as two arguments and run neither.
//
// Deliberately not a shell: no variable expansion, no escapes beyond the
// quotes themselves, no operators. A grant says what is reached, and
// anything that turned it into a program someone else could steer would be
// working against the point.
func splitFields(s string) ([]string, error) {
	var (
		out   []string
		cur   strings.Builder
		quote rune // 0 when not inside quotes
		open  bool // cur holds a token, even if it is empty
	)
	for _, r := range s {
		switch {
		case quote != 0 && r == quote:
			quote = 0
		case quote == 0 && (r == '\'' || r == '"'):
			quote, open = r, true
		case quote == 0 && (r == ' ' || r == '\t' || r == '\n' || r == '\r'):
			if open {
				out = append(out, cur.String())
				cur.Reset()
				open = false
			}
		default:
			cur.WriteRune(r)
			open = true
		}
	}
	if quote != 0 {
		return nil, fmt.Errorf("unbalanced %c in %q", quote, s)
	}
	if open {
		out = append(out, cur.String())
	}
	return out, nil
}

// quoteCommand renders argv as the single field the grammar expects: the
// arguments quoted against each other, then the whole quoted again so it
// survives being split off the line.
//
// Two levels because there are two splits. `["ssh","-p","22"]` becomes
// `"ssh -p 22"`, and `["/opt/my app/bin","--login"]` becomes
// `"'/opt/my app/bin' --login"`.
func quoteCommand(argv []string) string {
	parts := make([]string, len(argv))
	for i, a := range argv {
		parts[i] = quoteField(a)
	}
	return quoteField(strings.Join(parts, " "))
}

// quoteField is splitFields' inverse for one token, so String round-trips a
// command whose arguments contain spaces.
func quoteField(s string) string {
	if s == "" {
		return `""`
	}
	if !strings.ContainsAny(s, " \t\n\r'\"") {
		return s
	}
	if !strings.Contains(s, `"`) {
		return `"` + s + `"`
	}
	return `'` + s + `'`
}
