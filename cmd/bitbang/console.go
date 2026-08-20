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

// peerWaitLimit bounds a prompt that something else is blocked on -- the
// pairing SAS and the grant questions, where a connector is sitting on
// the other end. The command loop has no such limit: holding output is
// what a modal console is for, the buffer is bounded, and timing an
// operator out mid-thought while they read a link table is worse than
// holding a few hundred lines.
const peerWaitLimit = 2 * time.Minute

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
	out  io.Writer
	tty  *os.File
	held []*holdWriter

	// askMu serializes prompts, so a pairing question waits for a
	// half-typed command rather than racing it for the next line. The
	// wait is bounded by the operator finishing their line; phase 2's
	// request queue is what makes it properly interruptible.
	askMu sync.Mutex

	mu   sync.Mutex
	open bool
	// waiter is the prompt currently expecting a line, if any. The reader
	// hands lines to it, and to the idle handler otherwise -- one reader
	// with one destination at a time, rather than two receivers racing
	// for whatever the operator typed. Getting this wrong sent a pairing
	// SAS to the command loop, which ran it as a command.
	waiter chan string
	// onLine takes a line typed when no prompt is waiting.
	onLine func(string)
	// closed is set when the terminal reaches EOF.
	closed bool
	// looping guards against stacking command loops when lines arrive
	// faster than one is set up.
	looping bool
}

// newConsole attaches to the controlling terminal. A nil console means
// there is none -- a daemon, a pipe, a container without a tty -- and
// every method on it is a no-op, so callers need no special case.
func newConsole(held ...*holdWriter) *console {
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return nil
	}
	c := &console{out: tty, tty: tty, held: held}
	go c.read(bufio.NewReader(tty))
	return c
}

// read is the only reader of the terminal, and dispatches each line to
// whoever is waiting: the prompt in progress, or the idle handler.
//
// One destination at a time is the point. Two receivers on a shared
// channel race for whatever was typed, which sent a pairing SAS to the
// command loop and had it run as a command.
func (c *console) read(in *bufio.Reader) {
	for {
		line, err := in.ReadString('\n')
		if err != nil {
			c.mu.Lock()
			c.closed = true
			w := c.waiter
			c.mu.Unlock()
			if w != nil {
				close(w)
			}
			return
		}
		line = strings.TrimRight(line, "\r\n")

		c.mu.Lock()
		w, idle := c.waiter, c.onLine
		c.mu.Unlock()
		switch {
		case w != nil:
			// Buffered by one, and non-blocking: a prompt that gave up
			// between the read and here must not wedge the reader.
			select {
			case w <- line:
			default:
			}
		case idle != nil:
			idle(line)
		}
	}
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
	return c.ask(prompt, def, 0)
}

// AskWithin is Ask with a deadline, for a question something else is
// waiting on.
func (c *console) AskWithin(prompt, def string, limit time.Duration) (string, error) {
	return c.ask(prompt, def, limit)
}

// ask shows a prompt with a default that Enter accepts. It opens the
// console if it is not already open, so a caller with a question does not
// have to know whether one is in progress. A zero limit waits forever.
func (c *console) ask(prompt, def string, limit time.Duration) (string, error) {
	if !c.Available() {
		return "", errConsoleClosed
	}
	c.askMu.Lock()
	defer c.askMu.Unlock()
	c.enter()

	shown := prompt
	if def != "" {
		shown += " [" + def + "]"
	}
	fmt.Fprintf(c.out, "%s: ", shown)

	line, err := c.readLine(limit)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(line) == "" {
		return def, nil
	}
	return line, nil
}

// readLine takes the next line the operator types, giving up if they
// interrupt or if a deadline passes. A zero limit means no deadline:
// there is nothing to time out for, since the console holds output on
// purpose and the buffer behind it is bounded.
func (c *console) readLine(limit time.Duration) (string, error) {
	// Ctrl-C leaves the console rather than killing the listener. The
	// handler is installed only while a prompt is up, so outside the
	// console the key still stops everything.
	sigint := make(chan os.Signal, 1)
	signal.Notify(sigint, os.Interrupt)
	defer signal.Stop(sigint)

	var deadline <-chan time.Time
	if limit > 0 {
		t := time.NewTimer(limit)
		defer t.Stop()
		deadline = t.C
	}

	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return "", errConsoleClosed
	}
	mine := make(chan string, 1)
	c.waiter = mine
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		c.waiter = nil
		c.mu.Unlock()
	}()

	select {
	case line, ok := <-mine:
		if !ok {
			c.leave("")
			return "", errConsoleClosed
		}
		return line, nil
	case <-sigint:
		c.Say("")
		c.leave("")
		return "", errConsoleClosed
	case <-deadline:
		c.leave("no answer")
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

// Loop runs the command loop until the operator leaves. Opened by Enter
// at the terminal, and by anything else that wants a session.
//
// Modal: while it runs, the listener's output is held, so a mirroring
// shell session cannot scroll a half-typed command or a question away.
func (c *console) Loop(prompt string, run func(line string) error) {
	if !c.Available() {
		return
	}
	c.enter()
	defer c.leave("")

	c.Say("")
	c.Say("  %s", prompt)
	for {
		line, err := c.Ask("bitbang", "")
		if err != nil {
			return // Ctrl-C, EOF, or idle; leave() runs on the way out
		}
		switch strings.TrimSpace(line) {
		case "":
			continue
		case "exit", "quit", "q":
			return
		}
		if err := run(line); err != nil {
			return
		}
	}
}

// Watch opens the console when the operator presses Enter. One reader on
// the terminal, so nothing else may read stdin -- two readers race for
// every line, and the loser is whichever prompt needed it.
func (c *console) Watch(prompt string, run func(line string) error) {
	if !c.Available() {
		return
	}
	c.mu.Lock()
	c.onLine = func(line string) {
		c.mu.Lock()
		if c.looping {
			c.mu.Unlock()
			return
		}
		c.looping = true
		c.mu.Unlock()

		// On its own goroutine: onLine is called by the reader, and the
		// loop below waits for lines that same reader has to deliver.
		// Running it inline deadlocks the console on its first command.
		go func() {
			defer func() {
				c.mu.Lock()
				c.looping = false
				c.mu.Unlock()
			}()
			if strings.TrimSpace(line) != "" {
				// Typed something before Enter: treat it as the first
				// command rather than discarding it.
				c.enter()
				if err := run(strings.TrimSpace(line)); err != nil {
					return
				}
			}
			c.Loop(prompt, run)
		}()
	}
	c.mu.Unlock()
}
