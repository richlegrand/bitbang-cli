package client

// e2e tests for command piping and PTY shells (Session.Shell), backed by a real
// shell-stream listener.

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/richlegrand/bitbang-cli/internal/streamtype"
	"golang.org/x/term"
)

const shellHelperEnv = "GO_BITBANG_SHELL_HELPER"

func shellHelperArgv(t *testing.T, action string) []string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	return []string{exe, "-test.run=TestShellHelperProcess", "--", action}
}

// TestShellHelperProcess makes the shell E2E tests independent of Unix tools.
// The test binary re-execs itself as a tiny echo/cat/exit/TTY probe command.
func TestShellHelperProcess(t *testing.T) {
	if os.Getenv(shellHelperEnv) != "1" {
		return
	}
	action := os.Args[len(os.Args)-1]
	switch action {
	case "echo":
		fmt.Print("piped-hello-42")
	case "cat":
		_, _ = io.Copy(os.Stdout, os.Stdin)
	case "exit3":
		os.Exit(3)
	case "tty":
		if term.IsTerminal(int(os.Stdin.Fd())) {
			fmt.Print("IS_TTY")
		} else {
			fmt.Print("NOT_TTY")
		}
	case "line":
		line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		fmt.Print("RECEIVED:" + line)
	}
	os.Exit(0)
}

// shellSession wires a shell-stream listener and returns a connected connector
// Session. Listener + session teardown is registered via t.Cleanup.
func shellSession(t *testing.T) *Session {
	t.Helper()
	id := ephemeralID(t)
	relay := newFakeSignaling()
	t.Cleanup(relay.Close)
	startListener(relay.host(), id, streamtype.NewShell(nil, false))
	waitRegistered(t, relay)
	sess := mustDial(t, relay.host(), id, "shell")
	t.Cleanup(func() { sess.Close() })
	return sess
}

// TestSession_ShellCommand_Piping exercises command piping end-to-end: the
// connector's Session.Shell — the same call cmd/bitbang/connect.go makes for
// `bitbang connect <url> -- <argv>` and `echo ... | bitbang connect <url> -- cat`.
func TestSession_ShellCommand_Piping(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: spins up real pion peer connections and shell processes")
	}
	sess := shellSession(t)

	// 1. Command stdout capture: connect <url> -- echo piped-hello-42
	var out bytes.Buffer
	res, err := sess.Shell(ShellOptions{
		Argv: shellHelperArgv(t, "echo"), Env: map[string]string{shellHelperEnv: "1"}, Stdout: &out,
	})
	if err != nil {
		t.Fatalf("Shell echo: %v", err)
	}
	if res.ExitCode != 0 {
		t.Errorf("echo exit code = %d, want 0", res.ExitCode)
	}
	if !strings.Contains(out.String(), "piped-hello-42") {
		t.Errorf("echo stdout = %q, want to contain piped-hello-42", out.String())
	}

	// 2. True stdin→remote→stdout piping: echo from-stdin-7e | connect <url> -- cat
	var catOut bytes.Buffer
	res2, err := sess.Shell(ShellOptions{
		Argv:   shellHelperArgv(t, "cat"),
		Env:    map[string]string{shellHelperEnv: "1"},
		Stdin:  strings.NewReader("from-stdin-7e\n"),
		Stdout: &catOut,
	})
	if err != nil {
		t.Fatalf("Shell cat: %v", err)
	}
	if res2.ExitCode != 0 {
		t.Errorf("cat exit code = %d, want 0", res2.ExitCode)
	}
	if catOut.String() != "from-stdin-7e\n" {
		t.Errorf("cat stdout = %q, want %q", catOut.String(), "from-stdin-7e\n")
	}

	// 3. Non-zero exit propagation: connect <url> -- sh -c 'exit 3'
	res3, err := sess.Shell(ShellOptions{
		Argv: shellHelperArgv(t, "exit3"), Env: map[string]string{shellHelperEnv: "1"},
	})
	if err != nil {
		t.Fatalf("Shell exit-code: %v", err)
	}
	if res3.ExitCode != 3 {
		t.Errorf("sh -c 'exit 3' exit code = %d, want 3", res3.ExitCode)
	}
}

// TestSession_ShellCommand_PTY exercises PTY mode: the listener allocates a
// pseudo-terminal and runs the command with its stdin on the pty slave, so
// `test -t 0` succeeds — the same path as an interactive `bitbang connect <url>`.
// (In PTY mode the listener merges stderr into stdout and the terminal may add
// CRLF, so this asserts a substring rather than exact output.)
func TestSession_ShellCommand_PTY(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: spins up real pion peer connections and a PTY shell")
	}
	sess := shellSession(t)

	var out bytes.Buffer
	res, err := sess.Shell(ShellOptions{
		Argv:   shellHelperArgv(t, "tty"),
		Env:    map[string]string{shellHelperEnv: "1"},
		PTY:    true,
		Cols:   80,
		Rows:   24,
		Stdout: &out,
	})
	if err != nil {
		t.Fatalf("Shell PTY: %v", err)
	}
	if res.ExitCode != 0 {
		t.Errorf("PTY exit code = %d, want 0", res.ExitCode)
	}
	want := "IS_TTY"
	if !strings.Contains(out.String(), want) {
		t.Errorf("PTY stdout = %q, want to contain %s", out.String(), want)
	}
}
