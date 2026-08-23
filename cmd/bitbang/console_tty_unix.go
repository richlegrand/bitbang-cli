//go:build unix

package main

import (
	"io"
	"os"
)

// openTTY opens the controlling terminal for the console.
//
// /dev/tty rather than stdin/stdout, so the console keeps working when
// either is redirected -- which is the normal case, since the shell
// mirror and the log both write to stdout.
//
// One handle serves both directions here; Windows needs two.
func openTTY() (io.Writer, *os.File, error) {
	f, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return nil, nil, err
	}
	return f, f, nil
}
