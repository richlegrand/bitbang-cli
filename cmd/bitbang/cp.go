package main

import (
	"flag"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/richlegrand/bitbang/internal/client"
)

// runCp implements `bitbang cp <src> <dst>`.
//
// Source / destination syntax:
//
//	./local.txt                            (local path)
//	https://bitba.ng/<UID>#<CODE>:/remote  (remote path, full URL)
//	bitba.ng/<UID>#<CODE>:/remote          (remote path, scheme-less)
//	<UID>#<CODE>:/remote                   (remote path, defaults to bitba.ng)
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
	fs.Parse(args)

	if fs.NArg() != 2 {
		fmt.Fprintln(os.Stderr, "Usage: bitbang cp <src> <dst>")
		fmt.Fprintln(os.Stderr, "  src and dst can be a local path or <URL>:/remote")
		os.Exit(2)
	}
	srcArg, dstArg := fs.Arg(0), fs.Arg(1)

	src, srcRemote := parseRemoteSpec(srcArg)
	dst, dstRemote := parseRemoteSpec(dstArg)

	switch {
	case srcRemote && dstRemote:
		fail("cp: both sides cannot be remote (file-to-file routing across two listeners is not supported)")
	case !srcRemote && !dstRemote:
		fail("cp: at least one of src/dst must be a remote spec (URL:/path)")
	case srcRemote:
		runCpGet(src, dstArg, *verbose, *timeout, *pin)
	case dstRemote:
		runCpPut(srcArg, dst, *verbose, *timeout, *pin)
	}
}

func runCpGet(remote remoteSpec, dstLocal string, verbose bool, timeout time.Duration, suppliedPIN string) {
	sess := dial(remote, verbose, timeout, suppliedPIN)
	defer sess.Close()

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

func runCpPut(srcLocal string, remote remoteSpec, verbose bool, timeout time.Duration, suppliedPIN string) {
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

	// `bitbang cp ./foo URL:/` lands at URL:/foo. Same convention as scp.
	if remote.Path == "" || strings.HasSuffix(remote.Path, "/") {
		remote.Path = remote.Path + filepath.Base(srcLocal)
	}

	sess := dial(remote, verbose, timeout, suppliedPIN)
	defer sess.Close()

	fmt.Fprintf(os.Stderr, "Uploading %s → %s (%s)\n", srcLocal, remote.Path, humanBytes(info.Size()))
	start := time.Now()
	if err := sess.Put(remote.Path, f, false); err != nil {
		fail("cp: %v", err)
	}
	fmt.Fprintf(os.Stderr, "Done (%s in %.1fs)\n", humanBytes(info.Size()), time.Since(start).Seconds())
}

// dial composes the connection options and runs client.Dial. Exits on
// error so callers don't have to re-check.
func dial(r remoteSpec, verbose bool, timeout time.Duration, suppliedPIN string) *client.Session {
	opts := client.DialOptions{
		Server:      r.Server,
		UID:         r.UID,
		Code:        r.Code,
		Caps:        []string{"file"},
		DialTimeout: timeout,
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
		b, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(os.Stderr) // term.ReadPassword swallows the newline
		if err != nil {
			return "", err
		}
		return string(b), nil
	}
}

// remoteSpec is the parsed form of an "URL:/path" cp arg.
type remoteSpec struct {
	Server string // hostname, e.g. "bitba.ng" (no scheme)
	UID    string
	Code   string // access code, may be empty
	Path   string // path on the remote, leading "/"
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
	return remoteSpec{
		Server: u.Host,
		UID:    uid,
		Code:   u.Fragment,
		Path:   rest,
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
