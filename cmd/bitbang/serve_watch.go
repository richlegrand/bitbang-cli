package main

import (
	"os"
	"time"

	"golang.org/x/term"
)

// watchReload wires SIGHUP to a reload, which reprints the listing and
// then polls the live sessions so a deletion takes effect at once rather
// than waiting for the next tick.
//
// Enter used to do this too, reading stdin directly. It cannot any more:
// the console reads the terminal, and two readers race for every line --
// which showed up as a pairing SAS being swallowed and the pairing then
// timing out. Reload is a console command now, which is better anyway,
// since the console has a dozen of them and there are not a dozen keys.
func watchReload(reload func()) {
	sighup := make(chan os.Signal, 1)
	notifyReload(sighup)
	go func() {
		for range sighup {
			reload()
		}
	}()
}

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
