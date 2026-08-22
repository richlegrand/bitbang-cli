package main

import (
	"fmt"
	"os"
	"strings"
	"unicode/utf8"

	"golang.org/x/term"
)

// display is the listener's startup presentation: the logo, the QR code,
// the URL, and the pairing code. Split out of startListener because it is
// the half of that function that decides how things look rather than what
// runs, and it is reprinted on reconnect.
type display struct {
	url   string
	isTTY bool
	width int
	// bold and reset are empty on a pipe, so log scrapers and tests are not
	// confused by escape sequences.
	bold  string
	reset string
}

func newDisplay(url string) display {
	b := display{url: url, isTTY: term.IsTerminal(int(os.Stdout.Fd()))}
	if w, _, err := term.GetSize(int(os.Stdout.Fd())); err == nil {
		b.width = w
	}
	if b.isTTY {
		b.bold, b.reset = "\033[1m", "\033[0m"
	}
	return b
}

// printReady renders the banner, QR code, and URL. On a wide TTY the
// banner sits to the right of the QR (vertically centered) so the whole
// startup block stays short enough to fit on one screen — handy for a
// screen recording. On a narrow or non-TTY output it falls back to the
// banner stacked above the QR so pipes, logs, and tests stay readable.
func (b display) ready() {
	qr := smallQR(b.url)
	bannerLines := strings.Split(strings.TrimRight(banner, "\n"), "\n")
	bannerLines = append(bannerLines, "bitbang-cli v"+version)
	var qrLines []string
	if qr != "" {
		qrLines = strings.Split(strings.TrimRight(qr, "\n"), "\n")
	}

	bannerWidth := 0
	for _, l := range bannerLines {
		if w := utf8.RuneCountInString(l); w > bannerWidth {
			bannerWidth = w
		}
	}
	const gap = "   "
	qrWidth := 0
	if len(qrLines) > 0 {
		qrWidth = utf8.RuneCountInString(qrLines[0])
	}

	if len(qrLines) > 0 && b.isTTY && b.width >= qrWidth+len(gap)+bannerWidth {
		// QR flush left keeps its quiet zone against the terminal margin;
		// the banner is centered vertically against the taller QR block.
		off := (len(qrLines) - len(bannerLines)) / 2
		if off < 0 {
			off = 0
		}
		for i, ql := range qrLines {
			if bi := i - off; bi >= 0 && bi < len(bannerLines) {
				fmt.Println(ql + gap + bannerLines[bi])
			} else {
				fmt.Println(ql)
			}
		}
	} else {
		for _, l := range bannerLines {
			fmt.Println(l)
		}
		fmt.Println()
		fmt.Print(qr)
	}
	fmt.Printf("URL: %s%s%s\n", b.bold, b.url, b.reset)
}

// printPairCode renders the issued pairing code on its own line —
// the operator shares this verbally so the connector can pair
// without the full UID URL. Code may be empty when (a) --nocode is
// set, or (b) the server lacks pairing support. In either case the
// URL flow still works; we just don't surface a code. Bolded on a
// TTY so it's easy to spot in the startup block; plain on pipes
// so log scrapers/tests aren't confused by escape sequences.
func (b display) pairCode(code string) {
	if code != "" {
		fmt.Printf("%sPairing code: %s%s (valid 5 minutes)\n", b.bold, code, b.reset)
	}
}

// updateAvailable prints the one-line update notice. Informational, not
// a warning: nothing is wrong with the running version, and BitBang does
// not update itself.
func (b display) updateAvailable(notice string) {
	fmt.Printf("%s  %s\n", notice, "https://bitba.ng/install")
}
