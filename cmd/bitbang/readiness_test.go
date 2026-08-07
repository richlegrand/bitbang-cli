package main

import (
	"testing"
	"time"
)

func TestDeadlineGuard(t *testing.T) {
	dropped := make(chan struct{})
	newDeadlineGuard(20*time.Millisecond, func() { close(dropped) })
	select {
	case <-dropped:
	case <-time.After(2 * time.Second):
		t.Fatal("unready peer was not dropped")
	}

	stillThere := make(chan struct{})
	guard := newDeadlineGuard(20*time.Millisecond, func() { close(stillThere) })
	guard.Done()
	select {
	case <-stillThere:
		t.Fatal("completed peer was dropped")
	case <-time.After(100 * time.Millisecond):
	}
}
