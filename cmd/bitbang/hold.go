package main

import (
	"fmt"
	"io"
	"sync"
)

// maxHeldBytes bounds what a holdWriter keeps while output is paused.
// Past it the oldest lines go, and the count is reported on release --
// better than unbounded memory, and better than losing lines silently.
const maxHeldBytes = 256 << 10

// holdWriter passes writes through until it is told to hold, then keeps
// them until released.
//
// The listener writes three streams to the terminal: log lines, the shell
// mirror, and its own banner. All three have to stop while a prompt is on
// screen, or a mirroring shell session scrolls the question away before
// it can be read. Holding is a stopgap: once the daemon exists, logs go
// to a file and the console owns the terminal, at which point there is
// nothing to interleave.
type holdWriter struct {
	out io.Writer

	mu      sync.Mutex
	holding bool
	held    [][]byte
	bytes   int
	dropped int
}

func newHoldWriter(out io.Writer) *holdWriter { return &holdWriter{out: out} }

// The lock covers the write to the underlying writer as well as the
// state, because two goroutines calling out.Write concurrently interleave
// their bytes. The std logger does the same, and the cost is the same:
// a blocked terminal blocks every writer, which is already true of two
// streams sharing one fd.
func (h *holdWriter) Write(p []byte) (int, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.holding {
		return h.out.Write(p)
	}

	// Copy: callers reuse their buffers, and this one outlives the call.
	h.held = append(h.held, append([]byte(nil), p...))
	h.bytes += len(p)
	for h.bytes > maxHeldBytes && len(h.held) > 1 {
		h.bytes -= len(h.held[0])
		h.held = h.held[1:]
		h.dropped++
	}
	return len(p), nil
}

// Hold starts buffering. Safe to call when already holding.
func (h *holdWriter) Hold() {
	h.mu.Lock()
	h.holding = true
	h.mu.Unlock()
}

// Release writes everything held and resumes passing through, reporting
// how many writes were dropped past the cap so a gap is never silent.
func (h *holdWriter) Release() {
	h.mu.Lock()
	defer h.mu.Unlock()
	held, dropped := h.held, h.dropped
	h.held, h.bytes, h.dropped, h.holding = nil, 0, 0, false

	if dropped > 0 {
		fmt.Fprintf(h.out, "-- %d earlier writes not shown --\n", dropped)
	}
	for _, b := range held {
		_, _ = h.out.Write(b)
	}
}

// Holding reports whether output is currently paused.
func (h *holdWriter) Holding() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.holding
}
