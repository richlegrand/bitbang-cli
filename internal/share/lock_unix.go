//go:build unix

package share

import (
	"errors"
	"os"
	"strconv"
	"strings"
	"syscall"
)

// ErrLockBusy means another process holds a target's lifecycle lock.
var ErrLockBusy = errors.New("share lifecycle lock is held")

// Lock is an advisory flock over one share target, held for as long as
// a decision about that target is being acted on.
//
// The kernel drops it when the holder exits, so a crash never leaves it
// stale, and it is taken on a path beside the state directory -- see
// LockPathFor -- because cleanup deletes that directory, and a lock file
// that gets deleted excludes nobody.
//
// The PID in the file is only there to name the holder in a message.
// The flock is what excludes; nothing decides anything from the PID.
type Lock struct{ f *os.File }

// TryLock takes the lock without waiting. It returns ErrLockBusy and
// the holder's PID (0 if unreadable) when someone else has it, and any
// other error as itself -- a lock file that cannot be opened is a
// different problem from a lock that is held, and reporting the first
// as the second sends the operator looking for a process that does not
// exist.
func TryLock(path string) (*Lock, int, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, 0, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			pid := readLockHolder(f)
			_ = f.Close()
			return nil, pid, ErrLockBusy
		}
		_ = f.Close()
		return nil, 0, err
	}
	// We hold it: replace any stale PID with our own. Best-effort -- the
	// lock is valid regardless of what the file says.
	if err := f.Truncate(0); err == nil {
		_, _ = f.WriteAt([]byte(strconv.Itoa(os.Getpid())+"\n"), 0)
	}
	return &Lock{f: f}, 0, nil
}

func readLockHolder(f *os.File) int {
	buf := make([]byte, 32)
	n, _ := f.ReadAt(buf, 0)
	pid, _ := strconv.Atoi(strings.TrimSpace(string(buf[:n])))
	return pid
}

// Release drops the lock. Safe on a nil Lock.
func (l *Lock) Release() {
	if l == nil || l.f == nil {
		return
	}
	_ = syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
	_ = l.f.Close()
}
