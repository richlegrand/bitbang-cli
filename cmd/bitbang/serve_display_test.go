package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/richlegrand/bitbang-cli/internal/bbhome"
)

// The directory is always named, default or not -- a first-time user has the
// same question as an operator who configured it. Only the reason is
// conditional: "(HOME)" after a path under ~ adds nothing.
func TestHomeNoticeAlwaysNamesTheDirectory(t *testing.T) {
	d := display{url: "https://bitba.ng/UID#CODE", isTTY: false, width: 100}
	for _, k := range []string{"BITBANG_HOME", "STATE_DIRECTORY", "XDG_STATE_HOME"} {
		t.Setenv(k, "")
	}
	// Both, because os.UserHomeDir reads USERPROFILE on Windows.
	t.Setenv("HOME", "/home/u")
	t.Setenv("USERPROFILE", "/home/u")
	bbhome.Set("")
	t.Cleanup(func() { bbhome.Set("") })

	got := d.homeNotice()
	if !strings.Contains(got, filepath.Join("/home/u", ".bitbang")) {
		t.Errorf("notice = %q, want the default directory named", got)
	}
	if strings.Contains(got, "HOME") {
		t.Errorf("notice = %q, want no source for the plain default", got)
	}

	t.Setenv("STATE_DIRECTORY", "/var/lib/bitbang")
	got = d.homeNotice()
	if !strings.Contains(got, filepath.FromSlash("/var/lib/bitbang")) {
		t.Errorf("notice = %q, want the directory in it", got)
	}
	if !strings.Contains(got, "STATE_DIRECTORY") {
		t.Errorf("notice = %q, want the source that won in it", got)
	}
}

// -noqr leaves the banner and the URL and drops the QR block. A listener
// run as a daemon reprints this on every reconnect, so eighteen lines of
// block characters are the thing an operator wants gone from the log.
func TestReadyBlockNoQR(t *testing.T) {
	const url = "https://bitba.ng/UID#CODE"

	with := newDisplay(url, false).readyBlock()
	without := newDisplay(url, true).readyBlock()

	if !strings.Contains(with, "█") {
		t.Fatal("the default block has no QR in it, so this test proves nothing")
	}
	if strings.Contains(without, "█") {
		t.Error("-noqr still printed the QR")
	}
	for _, want := range []string{"URL: " + url, "bitbang-cli v" + version} {
		if !strings.Contains(without, want) {
			t.Errorf("-noqr dropped %q along with the QR", want)
		}
	}
}

// The logo is for a person looking at a screen. Off a terminal the startup
// block is reprinted on every reconnect into a journal nobody is reading
// art in, which is what #34 asked for and -noqr only half delivered.
func TestReadyBlockBannerIsForTerminalsOnly(t *testing.T) {
	const url = "https://bitba.ng/UID#CODE"
	logoLine := strings.Split(strings.TrimLeft(banner, "\n"), "\n")[0]

	tty := display{url: url, isTTY: true, width: 100}.readyBlock()
	if !strings.Contains(tty, logoLine) {
		t.Error("a terminal lost the banner")
	}

	piped := display{url: url, isTTY: false, width: 100}.readyBlock()
	if strings.Contains(piped, logoLine) {
		t.Error("the banner reached a non-terminal")
	}
	for _, want := range []string{"URL: " + url, "bitbang-cli v" + version} {
		if !strings.Contains(piped, want) {
			t.Errorf("piped output dropped %q along with the banner", want)
		}
	}

	// A daemon: no terminal and no QR. Version, then the URL, and nothing
	// else -- no stray separator left over from the QR that is not there.
	daemon := display{url: url, isTTY: false, noqr: true, width: 100}.readyBlock()
	want := "bitbang-cli v" + version + "\nURL: " + url + "\n"
	if daemon != want {
		t.Errorf("daemon block = %q, want %q", daemon, want)
	}
}
