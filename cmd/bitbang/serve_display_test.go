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
