package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"sync"
	"time"
)

// consoleIdle closes the console after this long without a keystroke.
// Walking away mid-prompt should not hold the listener's output
// indefinitely, and the transition back is announced so it is not
// mysterious.
const consoleIdle = 30 * time.Second

// errConsoleClosed ends a prompt because the operator left: Ctrl-C, EOF,
// or the idle timeout. Callers treat it the way they treat a decline.
var errConsoleClosed = errors.New("console closed")

// console is the listener's interactive surface: modal, opened by Enter,
// and opened by itself when something needs an answer.
//
// Modal because the grant questions are multi-step, and a set of hotkeys
// does not extend to them. While it is open the listener's output is
// held, so a question cannot be scrolled away by a mirroring shell.
//
// Prompts read and write /dev/tty rather than stdin and stdout, so
// `bitbang serve > log 2>&1` leaves a clean interactive surface with
// everything else diverted. Without a controlling terminal there is no
// console at all and the listener behaves as it did before.
type console struct {
	in   *bufio.Reader
	out  io.Writer
	tty  *os.File
	held []*holdWriter

	mu   sync.Mutex
	open bool
}

// newConsole attaches to the controlling terminal. A nil console means
// there is none -- a daemon, a pipe, a container without a tty -- and
// every method on it is a no-op, so callers need no special case.
func newConsole(held ...*holdWriter) *console {
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return nil
	}
	return &console{in: bufio.NewReader(tty), out: tty, tty: tty, held: held}
}

// Available reports whether there is a terminal to prompt on.
func (c *console) Available() bool { return c != nil && c.tty != nil }

// Say writes a line to the terminal, bypassing the hold -- console output
// is the thing the hold exists to protect, so it must not be held itself.
func (c *console) Say(format string, args ...interface{}) {
	if !c.Available() {
		return
	}
	fmt.Fprintf(c.out, format+"\n", args...)
}

// Ask shows a prompt with a default that Enter accepts. It opens the
// console if it is not already open, so a caller with a question does not
// have to know whether one is in progress.
func (c *console) Ask(prompt, def string) (string, error) {
	if !c.Available() {
		return "", errConsoleClosed
	}
	c.enter()

	shown := prompt
	if def != "" {
		shown += " [" + def + "]"
	}
	fmt.Fprintf(c.out, "%s: ", shown)

	line, err := c.readLine()
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(line) == "" {
		return def, nil
	}
	return line, nil
}

// readLine reads one line, giving up on the idle deadline or on the
// operator interrupting. The read runs on its own goroutine because a
// bufio read cannot be cancelled; the goroutine ends when the line
// finally arrives, or when the process does.
func (c *console) readLine() (string, error) {
	type result struct {
		line string
		err  error
	}
	lines := make(chan result, 1)
	go func() {
		line, err := c.in.ReadString('\n')
		lines <- result{strings.TrimRight(line, "\r\n"), err}
	}()

	// Ctrl-C leaves the console rather than killing the listener. The
	// handler is installed only while a prompt is up, so outside the
	// console the key still stops everything.
	sigint := make(chan os.Signal, 1)
	signal.Notify(sigint, os.Interrupt)
	defer signal.Stop(sigint)

	idle := time.NewTimer(consoleIdle)
	defer idle.Stop()

	select {
	case r := <-lines:
		if r.err != nil {
			c.leave("")
			return "", errConsoleClosed
		}
		return r.line, nil
	case <-sigint:
		c.Say("")
		c.leave("")
		return "", errConsoleClosed
	case <-idle.C:
		c.leave("idle")
		return "", errConsoleClosed
	}
}

// enter holds the listener's output and marks the console open.
func (c *console) enter() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.open {
		return
	}
	c.open = true
	for _, h := range c.held {
		h.Hold()
	}
}

// leave releases the held output and says why, so the transition is
// visible rather than output simply resuming.
func (c *console) leave(why string) {
	c.mu.Lock()
	if !c.open {
		c.mu.Unlock()
		return
	}
	c.open = false
	c.mu.Unlock()

	switch why {
	case "":
		fmt.Fprintln(c.out, "-- resuming output --")
	default:
		fmt.Fprintf(c.out, "-- resuming output (%s) --\n", why)
	}
	for _, h := range c.held {
		h.Release()
	}
}

// Session runs fn with the console open and the output held, releasing
// afterwards whichever way fn ends. Every caller that asks more than one
// question should use it, so a mid-flow abort still resumes output.
func (c *console) Session(fn func() error) error {
	if !c.Available() {
		return errConsoleClosed
	}
	c.enter()
	defer c.leave("")
	return fn()
}
