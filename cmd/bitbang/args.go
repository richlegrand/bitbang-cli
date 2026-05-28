package main

import (
	"flag"
	"strings"
)

// reorderArgs returns args with all `-flag` / `--flag` tokens (and
// their values, where applicable) hoisted to the front, preserving the
// relative order of positional arguments. Go's `flag` package stops
// parsing at the first non-flag, so without this a command like
//
//	bitbang connect URL --pin 1234
//
// silently treats `--pin` as a positional, never sets the flag, and
// the user's intent is lost. With this rearrangement the flag is
// parsed regardless of order — matching what most non-Go CLIs allow.
//
// The literal `--` token ends reordering: everything after it stays
// positional, in original order, so `bitbang connect URL -- ls -l`
// correctly hands `-l` to the remote command instead of trying to
// parse it as a bitbang flag.
//
// Flags that take a value (anything not declared as a bool flag in
// fs) consume the next arg as their value, so a flag-and-value pair
// moves as a unit.
func reorderArgs(fs *flag.FlagSet, args []string) []string {
	isBool := func(name string) bool {
		f := fs.Lookup(name)
		if f == nil {
			return false
		}
		bf, ok := f.Value.(boolFlag)
		return ok && bf.IsBoolFlag()
	}

	var flagsOut, posOut []string
	i := 0
	for i < len(args) {
		a := args[i]
		if a == "--" {
			// End-of-flags sentinel — preserve it and stop reordering.
			posOut = append(posOut, args[i:]...)
			break
		}
		// A bare "-" is the conventional stdin/stdout sentinel, not a
		// flag.
		if !strings.HasPrefix(a, "-") || a == "-" {
			posOut = append(posOut, a)
			i++
			continue
		}
		// It's a flag token.
		flagsOut = append(flagsOut, a)
		if strings.Contains(a, "=") {
			// `--flag=value` — value embedded, no extra token to grab.
			i++
			continue
		}
		name := strings.TrimLeft(a, "-")
		if isBool(name) {
			i++
			continue
		}
		// Non-bool flag — the next arg, if any, is the value.
		if i+1 < len(args) {
			flagsOut = append(flagsOut, args[i+1])
			i += 2
		} else {
			i++
		}
	}
	return append(flagsOut, posOut...)
}

// boolFlag matches the interface stdlib `flag` uses to identify bool
// flags. We define it locally so callers don't have to import
// anything beyond `flag` to use reorderArgs.
type boolFlag interface {
	IsBoolFlag() bool
}
