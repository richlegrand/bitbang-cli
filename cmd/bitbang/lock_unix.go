//go:build unix

package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// identityLock is an advisory flock held for the process lifetime on
// ~/.bitbang/<program>/lock. It stops two local processes from claiming the
// same identity (UID) — which would otherwise make them preempt each other at
// the signaling server (one active device connection per UID). The kernel
// releases the lock when the process exits, so a crash never leaves it stale,
// and a legitimate reconnect (same process, dropped WS) still works because the
// lock-holder never let go.
//
// The holder writes its PID into the file purely so the busy path can name it
// in the error message. The flock — not the file contents — is authoritative;
// the PID is never used for a locking decision (that would reintroduce the
// PID-reuse staleness bug flock avoids).
type identityLock struct{ f *os.File }

// acquireIdentityLock takes the per-identity lock. On success it returns the
// lock. If another process holds it, it returns errIdentityBusy and that
// process's PID (0 if it can't be read).
func acquireIdentityLock(dir string) (*identityLock, int, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, 0, err
	}
	f, err := os.OpenFile(filepath.Join(dir, "lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, 0, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		pid := readLockPID(f) // EWOULDBLOCK → held; read the holder's PID for the message
		f.Close()
		return nil, pid, errIdentityBusy
	}
	// We hold it: replace any stale PID with our own. flock guarantees we're
	// the sole writer. Best-effort — the lock is valid regardless.
	if err := f.Truncate(0); err == nil {
		_, _ = f.WriteAt([]byte(strconv.Itoa(os.Getpid())+"\n"), 0)
		_ = f.Sync()
	}
	return &identityLock{f: f}, 0, nil
}

func readLockPID(f *os.File) int {
	buf := make([]byte, 32)
	n, _ := f.ReadAt(buf, 0)
	pid, _ := strconv.Atoi(strings.TrimSpace(string(buf[:n])))
	return pid
}

func (l *identityLock) release() {
	if l == nil || l.f == nil {
		return
	}
	_ = syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
	_ = l.f.Close()
}
