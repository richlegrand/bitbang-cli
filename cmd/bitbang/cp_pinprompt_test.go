//go:build unix

package main

import (
	"os"
	"os/exec"
	"reflect"
	"testing"
	"time"

	"github.com/creack/pty"
	"golang.org/x/term"
)

// Ctrl-C at the PIN prompt used to leave the terminal with echo off,
// because term.ReadPassword restores the old termios from a defer and a
// signal-killed process never runs defers. Reported as issue #48, where
// the symptom was a shell that stopped echoing anything typed into it.
//
// The guard it checks exits via os.Exit, so this has to be a real process
// on a real terminal. The test re-execs itself as the child, which sits in
// the prompt until signalled.
//
// What it asserts is the whole of what matters: the terminal is bit-for-bit
// the way we found it. Checking ECHO alone would miss a future flag.
func TestPINPromptRestoresTerminalOnSignal(t *testing.T) {
	if os.Getenv("BITBANG_TEST_PIN_PROMPT") == "1" {
		_, _ = makePINPrompt("")(0)
		return
	}

	ptmx, tty, err := pty.Open()
	if err != nil {
		t.Skipf("no pty available: %v", err)
	}
	defer ptmx.Close()

	master := int(ptmx.Fd())
	before, err := term.GetState(master)
	if err != nil {
		t.Fatalf("read initial terminal state: %v", err)
	}

	cmd := exec.Command(os.Args[0], "-test.run=TestPINPromptRestoresTerminalOnSignal")
	cmd.Env = append(os.Environ(), "BITBANG_TEST_PIN_PROMPT=1")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = tty, tty, tty
	if err := cmd.Start(); err != nil {
		t.Fatalf("start child: %v", err)
	}
	tty.Close() // the child owns it now

	// Wait for the prompt to actually take hold. ReadPassword clearing
	// ECHO is the signal that we are inside it; sending SIGINT before
	// that would test nothing.
	if !waitForStateChange(t, master, before) {
		cmd.Process.Kill()
		cmd.Wait()
		t.Fatal("child never entered the PIN prompt")
	}

	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatalf("signal child: %v", err)
	}
	if err := cmd.Wait(); err == nil {
		t.Fatal("child exited cleanly, expected death by signal")
	}

	after, err := term.GetState(master)
	if err != nil {
		t.Fatalf("read final terminal state: %v", err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Errorf("terminal not restored after signal at the PIN prompt:\n before %+v\n after  %+v", before, after)
	}
}

// waitForStateChange reports whether the terminal state moved away from
// want before the deadline.
func waitForStateChange(t *testing.T, fd int, want *term.State) bool {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		got, err := term.GetState(fd)
		if err == nil && !reflect.DeepEqual(want, got) {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}
