//go:build unix

package streamtype

import (
	"os"
	"syscall"
	"time"

	ptylib "github.com/aymanbagabas/go-pty"
)

func defaultShellArgv() []string {
	if shell := os.Getenv("SHELL"); shell != "" {
		return []string{shell}
	}
	return []string{"/bin/sh"}
}

const platformSupportsPTY = true

func terminateShellProcess(process *os.Process) error {
	return process.Signal(syscall.SIGHUP)
}

// finishPTY is called once the shell process has exited. It gets the
// output pumps to EOF, then closes the terminal.
//
// The slave close is what makes the wait finishable. go-pty's unixPty
// holds the slave open in this process for the pty's whole life, so a
// read on the master returns neither EOF nor EIO however long ago the
// child died -- the kernel still sees an open slave, which is us. Left
// as it was, the wait below could not succeed in PTY mode and every
// interactive session paid the full timeout on exit before being
// cancelled.
//
// Closing it costs no output: the master still delivers what the child
// wrote before exiting and ends with EIO after (verified on Linux), and
// the pumps were reading those same bytes during the timeout anyway.
func finishPTY(terminal ptylib.Pty, output *shellOutput, timeout time.Duration) bool {
	if unixTerminal, ok := terminal.(ptylib.UnixPty); ok {
		_ = unixTerminal.Slave().Close()
	}
	if output.wait(timeout) {
		_ = terminal.Close()
		return true
	}
	output.cancel()
	_ = terminal.Close()
	_ = output.wait(shellOutputCloseGrace)
	return false
}

func signalFromName(name string) os.Signal {
	switch name {
	case "INT", "SIGINT":
		return syscall.SIGINT
	case "TERM", "SIGTERM":
		return syscall.SIGTERM
	case "QUIT", "SIGQUIT":
		return syscall.SIGQUIT
	case "HUP", "SIGHUP":
		return syscall.SIGHUP
	case "USR1", "SIGUSR1":
		return syscall.SIGUSR1
	case "USR2", "SIGUSR2":
		return syscall.SIGUSR2
	case "KILL", "SIGKILL":
		return syscall.SIGKILL
	}
	return nil
}
