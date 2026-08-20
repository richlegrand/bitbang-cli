package main

import (
	"bytes"
	"strings"
	"sync"
	"testing"
)

func TestHoldWriter_PassesThroughUntilHeld(t *testing.T) {
	var out bytes.Buffer
	h := newHoldWriter(&out)

	h.Write([]byte("before\n"))
	if out.String() != "before\n" {
		t.Fatalf("not passed through: %q", out.String())
	}

	h.Hold()
	h.Write([]byte("during-1\n"))
	h.Write([]byte("during-2\n"))
	if out.String() != "before\n" {
		t.Errorf("wrote while holding: %q", out.String())
	}

	h.Release()
	if out.String() != "before\nduring-1\nduring-2\n" {
		t.Errorf("held output came back wrong or out of order: %q", out.String())
	}

	h.Write([]byte("after\n"))
	if !strings.HasSuffix(out.String(), "after\n") {
		t.Errorf("did not resume: %q", out.String())
	}
}

// Past the cap the oldest go, and the gap is announced. Unbounded memory
// and silent loss are both worse.
func TestHoldWriter_DropsOldestAndSaysSo(t *testing.T) {
	var out bytes.Buffer
	h := newHoldWriter(&out)
	h.Hold()

	line := bytes.Repeat([]byte("x"), 8<<10)
	for i := 0; i < 64; i++ { // 512KB into a 256KB cap
		h.Write(line)
	}
	h.Release()

	got := out.String()
	if !strings.Contains(got, "earlier writes not shown") {
		t.Error("dropped output without saying so")
	}
	if len(got) > maxHeldBytes+1024 {
		t.Errorf("held %d bytes, cap is %d", len(got), maxHeldBytes)
	}
}

func TestHoldWriter_Concurrent(t *testing.T) {
	var out bytes.Buffer
	h := newHoldWriter(&out)
	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if i%7 == 0 {
				h.Hold()
			}
			h.Write([]byte("line\n"))
			if i%7 == 0 {
				h.Release()
			}
		}(i)
	}
	wg.Wait()
	h.Release()
	if h.Holding() {
		t.Error("left holding")
	}
	if n := strings.Count(out.String(), "line\n"); n != 40 {
		t.Errorf("got %d lines, want all 40 eventually written", n)
	}
}
