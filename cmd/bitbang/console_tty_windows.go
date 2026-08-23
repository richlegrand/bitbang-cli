//go:build windows

package main

import (
	"io"
	"os"
)

// openTTY opens the console for reading and writing.
//
// Windows has no /dev/tty. The equivalent is a pair of reserved names,
// CONIN$ and CONOUT$, which resolve to the process's attached console
// regardless of how stdin and stdout have been redirected -- the same
// property /dev/tty is used for on unix.
//
// Both are opened read-write: CONIN$ in particular requires it, because
// the console input handle is not a plain read-only file.
//
// A process with no console -- a service, or one started with
// DETACHED_PROCESS -- fails here, which is the same "no terminal"
// outcome as a unix daemon and leaves the console nil.
func openTTY() (io.Writer, *os.File, error) {
	out, err := os.OpenFile("CONOUT$", os.O_RDWR, 0)
	if err != nil {
		return nil, nil, err
	}
	in, err := os.OpenFile("CONIN$", os.O_RDWR, 0)
	if err != nil {
		out.Close()
		return nil, nil, err
	}
	return out, in, nil
}
