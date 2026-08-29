package main

import (
	"os"
	"time"

	"golang.org/x/term"
)

// watchExpiry re-checks live sessions on a timer. Deletion is applied
// when the table is replaced; this covers expiry, where the clock moves
// with nobody touching the file.
func watchExpiry(every time.Duration, poll func(time.Time)) {
	go func() {
		t := time.NewTicker(every)
		defer t.Stop()
		for now := range t.C {
			poll(now)
		}
	}()
}

// consoleHint is the footer under the link listing, shown only when
// there is a terminal to open a console on.
func consoleHint() string {
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return ""
	}
	return "  Enter: console (help, list, add, rm, reload, code, url)\n"
}
