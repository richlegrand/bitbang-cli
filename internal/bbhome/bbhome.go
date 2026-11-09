// Package bbhome resolves the one directory BitBang keeps its own files in:
// the identity keypair, the link table, the device table, and share state.
//
// It exists because that directory used to be computed in three places, each
// of which spelled `~/.bitbang` itself, and one of which fell back to "."
// when there was no home directory to spell it against. A service started
// without HOME therefore wrote its keypair relative to whatever directory it
// happened to start in -- `/` for an init script, which is how an OpenWRT
// listener ended up with its identity in /.bitbang (#41).
//
// Resolution order, which the README documents and this file is the authority
// for:
//
//  1. Set() -- the -home flag
//  2. $BITBANG_HOME
//  3. $STATE_DIRECTORY -- systemd sets it from StateDirectory= in the unit,
//     so a unit needs no flag at all
//  4. $XDG_STATE_HOME/bitbang
//  5. $HOME/.bitbang
//  6. an error
//
// The first three name the directory itself; the last two name a parent that
// `bitbang` or `.bitbang` hangs off. That asymmetry is deliberate: an
// operator who says -home /var/lib/bitbang means that directory, while
// XDG_STATE_HOME is a place for many programs' state and HOME is a place for
// many programs' dotfiles.
//
// Nothing here creates the directory. Callers do that when they write, so a
// read-only command does not leave a directory behind.
package bbhome

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// ErrNoHome is returned when every step of the ladder came up empty. It is
// deliberately not a fallback: writing a private key somewhere nobody chose
// is worse than refusing to write it.
var ErrNoHome = errors.New(
	"cannot tell where to keep BitBang's files: no -home, no BITBANG_HOME, " +
		"and no home directory. Pass -home <dir> or set BITBANG_HOME")

var (
	mu       sync.Mutex
	override string
)

// Set fixes the directory explicitly, from the -home flag. Call it after
// flag parsing and before anything reads Root.
func Set(dir string) {
	mu.Lock()
	defer mu.Unlock()
	override = dir
}

// Source names the rung of the ladder an answer came from, so the listener
// can say which one won. SourceHome is the default and the quiet case.
type Source string

const (
	SourceFlag    Source = "-home"
	SourceEnv     Source = "BITBANG_HOME"
	SourceSystemd Source = "STATE_DIRECTORY"
	SourceXDG     Source = "XDG_STATE_HOME"
	SourceHome    Source = "HOME"
)

// Root returns the directory BitBang keeps its files in.
//
// Resolved on every call rather than cached. The cost is a few getenv calls;
// the first draft of this cached the answer and it was wrong. A cached answer
// survives a change to HOME, which is exactly what a test does when it points
// HOME at a temp directory, so the first test to resolve decided for every
// test after it -- and one of them went looking in the developer's real
// device table instead of its own.
func Root() (string, error) {
	dir, _, err := Resolve()
	return dir, err
}

// Resolve is Root plus where the answer came from. With six ways to choose
// the directory, a listener that says which one it used saves the next person
// working out why its files are not where they expected.
func Resolve() (string, Source, error) {
	mu.Lock()
	o := override
	mu.Unlock()
	return resolve(o)
}

// resolve is Resolve's body, split out so a test can pass an override
// without touching the package-level one.
func resolve(override string) (string, Source, error) {
	if override != "" {
		return filepath.Clean(override), SourceFlag, nil
	}
	if v := os.Getenv("BITBANG_HOME"); v != "" {
		return filepath.Clean(v), SourceEnv, nil
	}
	// systemd allows several state directories, colon-separated, in which
	// case the first is the one the unit asked for.
	if v := os.Getenv("STATE_DIRECTORY"); v != "" {
		if i := strings.IndexByte(v, ':'); i >= 0 {
			v = v[:i]
		}
		if v != "" {
			return filepath.Clean(v), SourceSystemd, nil
		}
	}
	// XDG_STATE_HOME is a parent shared with other programs, so we take a
	// subdirectory of it. Undotted, because it is not a dotfile location.
	if v := os.Getenv("XDG_STATE_HOME"); v != "" {
		return filepath.Join(filepath.Clean(v), "bitbang"), SourceXDG, nil
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".bitbang"), SourceHome, nil
	}
	return "", "", ErrNoHome
}
