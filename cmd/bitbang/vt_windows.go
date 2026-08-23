//go:build windows

package main

import (
	"os"

	"golang.org/x/sys/windows"
)

// enableVT turns on ANSI escape handling for the console, and reports
// whether it worked.
//
// Windows consoles do not interpret escape sequences unless a process
// asks. Under ConPTY -- Windows Terminal, or anything hosting a pty --
// this is already on; a legacy conhost session is where it is not, and
// there our bold would arrive as a literal ESC[1m in front of every
// label. Ask, and skip the styling if the answer is no.
func enableVT() bool {
	h := windows.Handle(os.Stdout.Fd())
	var mode uint32
	if err := windows.GetConsoleMode(h, &mode); err != nil {
		return false // not a console at all
	}
	if mode&windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING != 0 {
		return true
	}
	err := windows.SetConsoleMode(h, mode|windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING)
	return err == nil
}
