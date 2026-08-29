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

// errPreempted means a read gave up its turn to a prompt that could not
// wait. Only the command loop sees it, and only ever resumes.
var errPreempted = errors.New("preempted")

// console is the listener's interactive surface: modal, opened by Enter,
// and opened by itself when something needs an answer.
//
// Modal because the grant questions are multi-step, and a set of hotkeys
// does not extend to them. While it is open the listener's output is
// held, so a question cannot be scrolled away by a mirroring shell.
//
// Prompts read and write the terminal directly -- /dev/tty on unix,
// CONIN$/CONOUT$ on Windows (see openTTY) -- rather than stdin and
// stdout, so `bitbang serve > log 2>&1` leaves a clean interactive
// surface with everything else diverted. Without a controlling terminal
// there is no console at all and the listener behaves as it did before.
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
	// yield, when non-nil, is closed to tell the read in progress to give
	// up its turn. Only the command loop registers one: it is the reader
	// that can be resumed, so it is the one that steps aside.
	yield chan struct{}
	// pending counts prompts that must not queue behind the command loop
	// -- a pairing question, where the operator is being told to type
	// something and somebody else is waiting on the answer. idle is
	// broadcast when the count reaches zero.
	pending int
	idle    *sync.Cond
	// looping guards against stacking command loops when lines arrive
	// faster than one is set up.
	looping bool
}

// newConsole attaches to the controlling terminal. A nil console means
// there is none -- a daemon, a pipe, a container without a tty, a
// Windows service with no console attached -- and every method on it is
// a no-op, so callers need no special case.
func newConsole(held ...*holdWriter) *console {
	out, in, err := openTTY()
	if err != nil {
		return nil
	}
	c := &console{out: out, tty: in, held: held}
	c.idle = sync.NewCond(&c.mu)
	go c.read(bufio.NewReader(in))
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

// Writer is where console output goes, or nil when there is no console
// -- callers pass it on and the default (stdout) applies.
func (c *console) Writer() io.Writer {
	if !c.Available() {
		return nil
	}
	return c.out
}

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

// AskNow is Ask for a question somebody else is waiting on -- a pairing
// SAS, and the grant questions behind it. It interrupts the command loop
// rather than queueing behind it.
//
// Queueing was the bug: the loop holds its turn until the operator
// finishes a line, so a pairing prompt sat behind it while the banner
// telling the operator to type a code had already printed. The code they
// typed went to the loop and came back as `unknown command "472663"`.
func (c *console) AskNow(prompt, def string, limit time.Duration) (string, error) {
	if !c.Available() {
		return "", errConsoleClosed
	}
	defer c.Hold()()
	return c.ask(prompt, def, limit)
}

// Hold keeps the command loop off the terminal until the returned
// function is called, and interrupts it if it is waiting. Take one
// around a flow of several questions -- a pairing is a SAS and then the
// grant questions -- so the loop does not win a turn between them and
// flash a prompt that could swallow the next answer.
//
// Safe on a nil console, which is the no-terminal case.
func (c *console) Hold() func() {
	if !c.Available() {
		return func() {}
	}
	c.mu.Lock()
	c.pending++
	if c.yield != nil {
		close(c.yield)
		c.yield = nil
	}
	c.mu.Unlock()
	return func() {
		c.mu.Lock()
		c.pending--
		if c.pending == 0 {
			c.idle.Broadcast()
		}
		c.mu.Unlock()
	}
}

// waitIdle blocks while any priority prompt is outstanding, so the
// command loop does not race one for the terminal the moment it is
// interrupted.
func (c *console) waitIdle() {
	c.mu.Lock()
	for c.pending > 0 {
		c.idle.Wait()
	}
	c.mu.Unlock()
}

// ask shows a prompt with a default that Enter accepts. It opens the
// console if it is not already open, so a caller with a question does not
// have to know whether one is in progress. A zero limit waits forever.
func (c *console) ask(prompt, def string, limit time.Duration) (string, error) {
	return c.askImpl(prompt, def, limit, false)
}

// askYielding is the command loop's read: it steps aside for a prompt
// that cannot wait, and its caller re-prompts.
func (c *console) askYielding(prompt, def string) (string, error) {
	return c.askImpl(prompt, def, 0, true)
}

func (c *console) askImpl(prompt, def string, limit time.Duration, yields bool) (string, error) {
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

	line, err := c.readLineYielding(limit, yields)
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
	return c.readLineYielding(limit, false)
}

// readLineYielding is readLine, optionally registering to be interrupted
// by a priority prompt. Returns errPreempted when it gives up its turn.
func (c *console) readLineYielding(limit time.Duration, yields bool) (string, error) {
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
	// Checked here rather than only before the read: closing yield is an
	// edge, and a priority prompt that arrived while this caller was on
	// its way in would find no yield to close and then wait on askMu for
	// a turn nothing was going to give up.
	if yields && c.pending > 0 {
		c.mu.Unlock()
		return "", errPreempted
	}
	mine := make(chan string, 1)
	c.waiter = mine
	var yield chan struct{}
	if yields {
		yield = make(chan struct{})
		c.yield = yield
	}
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		c.waiter = nil
		if c.yield == yield {
			c.yield = nil
		}
		c.mu.Unlock()
	}()

	select {
	case <-yield:
		// A pairing prompt needs the terminal. Say nothing: it is about
		// to print its own question, and two prompts explaining
		// themselves is worse than one appearing.
		return "", errPreempted
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
		// Stand aside while a pairing question is up, then take the
		// terminal back and re-prompt.
		c.waitIdle()
		line, err := c.askYielding("bitbang", "")
		if errors.Is(err, errPreempted) {
			continue
		}
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
// greet, when set, runs once each time the console is opened by Enter,
// before the first prompt. It exists so the URLs can be reprinted: they
// are the reason to look at a listener at all, and on a busy one they
// have scrolled away long ago.
func (c *console) Watch(prompt string, greet func(), run func(line string) error) {
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
			c.enter()
			if greet != nil {
				greet()
			}
			if strings.TrimSpace(line) != "" {
				// Typed something before Enter: treat it as the first
				// command rather than discarding it.
				if err := run(strings.TrimSpace(line)); err != nil {
					return
				}
			}
			c.Loop(prompt, run)
		}()
	}
	c.mu.Unlock()
}
