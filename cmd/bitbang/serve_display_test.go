package main

import (
	"strings"
	"testing"
)

// -noqr leaves the banner and the URL and drops the QR block. A listener
// run as a daemon reprints this on every reconnect, so eighteen lines of
// block characters are the thing an operator wants gone from the log.
func TestReadyBlockNoQR(t *testing.T) {
	const url = "https://bitba.ng/UID#CODE"

	// On a terminal, where a QR is printed at all -- off one there is never
	// one now, so newDisplay under `go test` would prove nothing.
	with := display{url: url, isTTY: true, width: 100}.readyBlock()
	without := display{url: url, isTTY: true, noqr: true, width: 100}.readyBlock()

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
	for _, want := range []string{"URL: https://bitba.ng/UID", "bitbang-cli v" + version} {
		if !strings.Contains(piped, want) {
			t.Errorf("piped output dropped %q along with the banner", want)
		}
	}

	// A daemon: no terminal and no QR. Version, then the URL, and nothing
	// else -- no stray separator left over from the QR that is not there.
	daemon := display{url: url, isTTY: false, noqr: true, width: 100}.readyBlock()
	want := "bitbang-cli v" + version + "\nURL: https://bitba.ng/UID\n"
	if daemon != want {
		t.Errorf("daemon block = %q, want %q", daemon, want)
	}
}

// The access code is a credential, and off a terminal the startup block goes
// into a journal. It has to leave by both doors: the URL text, and the QR,
// which encodes the same URL and would otherwise carry the code as pixels.
func TestReadyBlockKeepsTheCodeOffNonTerminals(t *testing.T) {
	const url = "https://bitba.ng/UID#CODE"

	for _, tc := range []struct {
		name string
		d    display
	}{
		{"piped", display{url: url, isTTY: false, width: 100}},
		{"daemon", display{url: url, isTTY: false, noqr: true, width: 100}},
	} {
		got := tc.d.readyBlock()
		if strings.Contains(got, "CODE") {
			t.Errorf("%s: the access code reached a non-terminal: %q", tc.name, got)
		}
		if strings.Contains(got, "#") {
			t.Errorf("%s: the fragment reached a non-terminal: %q", tc.name, got)
		}
		if strings.Contains(got, "█") {
			t.Errorf("%s: a QR of the full URL reached a non-terminal", tc.name)
		}
		if !strings.Contains(got, "URL: https://bitba.ng/UID\n") {
			t.Errorf("%s: lost the URL along with the code: %q", tc.name, got)
		}
	}

	// On a terminal it stays whole -- that is where it is read in order to
	// be used, and the QR is the point of printing one.
	tty := display{url: url, isTTY: true, width: 100}.readyBlock()
	if !strings.Contains(tty, "URL: "+url) {
		t.Errorf("a terminal lost the access code: %q", tty)
	}
	if !strings.Contains(tty, "█") {
		t.Error("a terminal lost the QR")
	}
}
