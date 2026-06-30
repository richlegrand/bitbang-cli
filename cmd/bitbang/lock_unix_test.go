//go:build unix

package main

import (
	"os"
	"testing"
)

func TestIdentityLock(t *testing.T) {
	dir := t.TempDir()

	l1, _, err := acquireIdentityLock(dir)
	if err != nil {
		t.Fatalf("first acquire failed: %v", err)
	}

	// A second acquire on the same dir (separate flock'd fd) must report busy,
	// and surface the holder's PID — which, in-process, is our own.
	l2, pid, err := acquireIdentityLock(dir)
	if err != errIdentityBusy {
		t.Fatalf("second acquire: err=%v, want errIdentityBusy", err)
	}
	if l2 != nil {
		t.Fatal("second acquire returned a non-nil lock while busy")
	}
	if pid != os.Getpid() {
		t.Errorf("busy PID = %d, want %d", pid, os.Getpid())
	}

	// After release, the lock is free again.
	l1.release()
	l3, _, err := acquireIdentityLock(dir)
	if err != nil {
		t.Fatalf("re-acquire after release failed: %v", err)
	}
	l3.release()
}
