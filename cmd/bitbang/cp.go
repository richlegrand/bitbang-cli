package main

import (
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/term"

	"github.com/richlegrand/bitbang-cli/internal/client"
	"github.com/richlegrand/bitbang-cli/internal/signaling"
)

// runCp implements `bitbang cp <src> <dst>`.
//
// Source / destination syntax:
//
//	./local.txt                            (local path)
//	-                                      (stdin if src, stdout if dst)
//	https://bitba.ng/<UID>#<CODE>:/remote  (remote path, full URL)
//	bitba.ng/<UID>#<CODE>:/remote          (remote path, scheme-less)
//	<UID>#<CODE>:/remote                   (remote path, defaults to bitba.ng)
//	<name>:/remote                         (remote path, saved device name)
//
// Exactly one of src and dst must be remote. The `#CODE` portion of the
// URL is the access code (URL fragment); without it bidirectional verify
// fails. The remote path follows the colon AFTER the URL part, so
// quoting is rarely required even if the URL itself contains a colon
// (the "://" of the scheme).
func runCp(args []string) {
	fs := flag.NewFlagSet("cp", flag.ExitOnError)
	verbose := fs.Bool("v", false, "Verbose logging")
	timeout := fs.Duration("timeout", 30*time.Second, "Dial timeout")
	pin := fs.String("pin", "", "PIN (skips the interactive prompt)")
	relay := fs.Bool("relay", false, "Request a TURN relay up front instead of only on fallback (ICE still prefers direct if it succeeds)")
	fs.Parse(reorderArgs(fs, args))

	if fs.NArg() != 2 {
		fmt.Fprintln(os.Stderr, "Usage: bitbang cp <src> <dst>")
		fmt.Fprintln(os.Stderr, "  src and dst can be a local path, '-' (stdin/stdout), <URL>:/remote, or <saved-name>:/remote")
		os.Exit(2)
	}
	srcArg, dstArg := fs.Arg(0), fs.Arg(1)

	src, srcRemote := parseRemoteSpec(srcArg)
	dst, dstRemote := parseRemoteSpec(dstArg)

	switch {
	case srcRemote && dstRemote:
		fail("cp: both sides cannot be remote (file-to-file routing across two listeners is not supported)")
	case !srcRemote && !dstRemote:
		fail("cp: at least one of src/dst must be a remote spec (URL:/path, or a saved device name as NAME:/path)")
	case srcRemote:
		runCpGet(src, dstArg, *verbose, *timeout, *pin, *relay)
	case dstRemote:
		runCpPut(srcArg, dst, *verbose, *timeout, *pin, *relay)
	}
}

func runCpGet(remote remoteSpec, dstLocal string, verbose bool, timeout time.Duration, suppliedPIN string, relay bool) {
	sess := dial(remote, verbose, timeout, suppliedPIN, relay)
	defer sess.Close()

	// Pipe mode: `bitbang cp <url>:/file -` streams to stdout, no progress
	// chatter so the output can be piped cleanly (e.g. into grep, less).
	if dstLocal == "-" {
		if _, err := sess.Get(remote.Path, os.Stdout); err != nil {
			fail("cp: %v", err)
		}
		return
	}

	// Allow `bitbang cp <url>:/file .` to land the file alongside its
	// remote basename in the current directory. Same shortcut as scp.
	if dstLocal == "." || strings.HasSuffix(dstLocal, "/") {
		base := filepath.Base(remote.Path)
		if base == "" || base == "/" {
			base = "download"
		}
		if dstLocal == "." {
			dstLocal = base
		} else {
			dstLocal = filepath.Join(dstLocal, base)
		}
	}

	f, err := os.Create(dstLocal)
	if err != nil {
		fail("cp: %v", err)
	}
	defer f.Close()

	fmt.Fprintf(os.Stderr, "Downloading %s → %s\n", remote.Path, dstLocal)
	start := time.Now()
	info, err := sess.Get(remote.Path, f)
	if err != nil {
		fail("cp: %v", err)
	}
	elapsed := time.Since(start)
	fmt.Fprintf(os.Stderr, "Done (%s in %.1fs)\n", humanBytes(info.Size), elapsed.Seconds())
}

func runCpPut(srcLocal string, remote remoteSpec, verbose bool, timeout time.Duration, suppliedPIN string, relay bool) {
	var (
		src      io.Reader
		size     int64 = -1 // -1 means unknown (stdin)
		basename string
	)
	if srcLocal == "-" {
		// Pipe mode: read from stdin. Size unknown; remote path must be
		// concrete (no auto-basename-from-stdin guessing).
		src = os.Stdin
		basename = "stdin"
	} else {
		f, err := os.Open(srcLocal)
		if err != nil {
			fail("cp: %v", err)
		}
		defer f.Close()
		info, err := f.Stat()
		if err != nil {
			fail("cp: %v", err)
		}
		if info.IsDir() {
			fail("cp: %s is a directory (recursive upload not implemented yet)", srcLocal)
		}
		src = f
		size = info.Size()
		basename = filepath.Base(srcLocal)
	}

	// `bitbang cp ./foo URL:/` lands at URL:/foo. Same convention as scp.
	// For stdin, the user really should pick a filename, but we land at
	// URL:/stdin if they don't — predictable, not a guess.
	if remote.Path == "" || strings.HasSuffix(remote.Path, "/") {
		remote.Path = remote.Path + basename
	}

	sess := dial(remote, verbose, timeout, suppliedPIN, relay)
	defer sess.Close()

	if size >= 0 {
		fmt.Fprintf(os.Stderr, "Uploading %s → %s (%s)\n", srcLocal, remote.Path, humanBytes(size))
	} else {
		fmt.Fprintf(os.Stderr, "Uploading stdin → %s\n", remote.Path)
	}
	start := time.Now()
	if err := sess.Put(remote.Path, src, false); err != nil {
		fail("cp: %v", err)
	}
	if size >= 0 {
		fmt.Fprintf(os.Stderr, "Done (%s in %.1fs)\n", humanBytes(size), time.Since(start).Seconds())
	} else {
		fmt.Fprintf(os.Stderr, "Done in %.1fs\n", time.Since(start).Seconds())
	}
}

// dial composes the connection options and runs client.Dial. Exits on
// error so callers don't have to re-check.
func dial(r remoteSpec, verbose bool, timeout time.Duration, suppliedPIN string, relay bool) *client.Session {
	opts := client.DialOptions{
		Server:      r.Server,
		UID:         r.UID,
		Code:        r.Code,
		Caps:        []string{"file"},
		DialTimeout: timeout,
		ForceRelay:  relay,
		Verbose:     verbose,
		PINPrompt:   makePINPrompt(suppliedPIN),
	}
	if verbose {
		fmt.Fprintf(os.Stderr, "[cp] dialing %s\n", r.Server)
	}
	sess, err := client.Dial(opts)
	if err != nil {
		fail("cp: %v", err)
	}
	reportUpdate(os.Stderr, sess.LatestVersions, r.Server)
	// The listener must advertise the `file` cap for cp to work. If it
	// doesn't, exit before issuing a SYN the listener would just 500.
	if !hasCap(sess.ServerCaps, "file") {
		sess.Close()
		fail("cp: listener does not advertise the `file` capability (caps: %v)", sess.ServerCaps)
	}
	return sess
}

// makePINPrompt returns a prompt function. If suppliedPIN is non-empty,
// use it on the first attempt and fall back to interactive on retry.
func makePINPrompt(suppliedPIN string) func(retry int) (string, error) {
	return func(retry int) (string, error) {
		if retry == 0 && suppliedPIN != "" {
			return suppliedPIN, nil
		}
		fmt.Fprint(os.Stderr, "PIN: ")
		fd := int(os.Stdin.Fd())
		unguard := guardTerminal(fd)
		b, err := term.ReadPassword(fd)
		unguard()
		fmt.Fprintln(os.Stderr) // term.ReadPassword swallows the newline
		if err != nil {
			return "", err
		}
		return string(b), nil
	}
}

// guardTerminal keeps a signal from leaving the terminal with echo off
// while we are prompting for a PIN.
//
// term.ReadPassword clears ECHO and puts the old termios back from a
// deferred call. A process killed by a signal never runs its defers, and
// at the PIN prompt nothing has registered for SIGINT yet -- connect does
// that later, around raw mode -- so Ctrl-C took the default disposition
// and killed us inside read(2). The user got back a shell that no longer
// echoed what they typed. ISIG is still on during the prompt, which is
// what makes Ctrl-C a signal here rather than an input byte.
//
// Same arrangement as setupInteractive, for the same reason: the signal
// path has to call os.Exit, which skips defers, so restoring has to
// happen before it.
//
// The returned function stops the guard and must be called on every path
// out of the prompt.
func guardTerminal(fd int) func() {
	oldState, err := term.GetState(fd)
	if err != nil {
		return func() {} // not a terminal, nothing to put back
	}
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	stop := make(chan struct{})
	go func() {
		select {
		case sig := <-sigCh:
			_ = term.Restore(fd, oldState)
			fmt.Fprintln(os.Stderr) // finish the "PIN: " line
			code := 130             // 128 + SIGINT, if we can't tell
			if s, ok := sig.(syscall.Signal); ok {
				code = 128 + int(s)
			}
			os.Exit(code)
		case <-stop:
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() {
			signal.Stop(sigCh)
			close(stop)
		})
	}
}

// remoteSpec is the parsed form of an "URL:/path" cp arg.
type remoteSpec struct {
	Server string // hostname, e.g. "bitba.ng" (no scheme)
	UID    string
	Code   string // access code, may be empty
	Path   string // path on the remote, leading "/"

	// Ephemeral suppresses device persistence for a share URL.
	Ephemeral bool
}

// parseRemoteSpec returns (spec, true) if arg looks like a remote
// reference: "<URL>:/<path>", "<server>/<UID>[#<CODE>]:/<path>", or
// "<UID>[#<CODE>]:/<path>". Returns (_, false) otherwise (treat as local).
//
// The split lives at the first ":/" that's NOT the scheme separator.
// That lets URLs like "https://bitba.ng/UID#CODE:/path" parse cleanly
// without any escaping.
func parseRemoteSpec(arg string) (remoteSpec, bool) {
	// Strip scheme up front so the ":/" that lives between URL and path
	// is the only one our search can find.
	urlPart, rest := arg, ""
	schemeOffset := 0
	if i := strings.Index(arg, "://"); i >= 0 {
		schemeOffset = i + 3
	}
	// Find the colon after the scheme that precedes "/".
	rel := strings.Index(arg[schemeOffset:], ":/")
	if rel < 0 {
		return remoteSpec{}, false
	}
	cutPath := schemeOffset + rel
	urlPart = arg[:cutPath]
	rest = arg[cutPath+1:] // includes the "/"

	// A bare word shaped like a saved device name resolves against the
	// table first, so `cp nas:/photos .` works wherever `connect nas`
	// does. Same precedence connect uses. A name-shaped word that is not
	// in the table falls through and is read as a UID, which is what it
	// was before any of this.
	if looksLikeDeviceName(urlPart) {
		if ent, ok := lookupDeviceByName(urlPart); ok {
			return remoteSpec{
				Server: ent.Server,
				UID:    ent.UID,
				Code:   ent.AccessCode,
				Path:   rest,
			}, true
		}
	}

	// Now parse urlPart. Accept three shapes (without scheme, with
	// scheme, bare UID#CODE) by normalizing to a URL.
	if !strings.Contains(urlPart, "://") {
		// "server/UID#CODE" or just "UID#CODE": prepend https:// and let
		// net/url do the work. A bare UID (no slash) maps to the default
		// server with the UID as path.
		if !strings.Contains(urlPart, "/") {
			urlPart = "bitba.ng/" + urlPart
		}
		urlPart = "https://" + urlPart
	}
	u, err := url.Parse(urlPart)
	if err != nil || u.Host == "" {
		return remoteSpec{}, false
	}
	// UID is the first path segment.
	uid := strings.TrimPrefix(u.Path, "/")
	if i := strings.IndexByte(uid, '/'); i >= 0 {
		uid = uid[:i]
	}
	if uid == "" {
		return remoteSpec{}, false
	}
	// Fragment grammar (see CONVENTIONS.md): `<code>[!<flags>][/<device-URL>]`.
	// For cp the device URL is superseded by the `:/path` after the URL.
	code, flags := signaling.ParseFragment(u.Fragment)
	return remoteSpec{
		Server:    u.Host,
		UID:       uid,
		Code:      code,
		Path:      rest,
		Ephemeral: signaling.HasFlag(flags, "ephemeral"),
	}, true
}

func hasCap(caps []string, want string) bool {
	for _, c := range caps {
		if c == want {
			return true
		}
	}
	return false
}

// humanBytes formats n as a 1-decimal "X.Y unit" string. For status
// lines only, not anything machine-parseable.
func humanBytes(n int64) string {
	const k = 1024
	if n < k {
		return fmt.Sprintf("%d B", n)
	}
	v := float64(n)
	units := []string{"KB", "MB", "GB", "TB"}
	for _, u := range units {
		v /= k
		if v < k {
			return fmt.Sprintf("%.1f %s", v, u)
		}
	}
	return fmt.Sprintf("%.1f PB", v/k)
}

// fail prints to stderr and exits 1. Kept short for use at every error
// site in cp where a stack trace would just be noise.
func fail(format string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
