//go:build !unix

package share

import "errors"

// ErrLockBusy means another process holds a target's lifecycle lock.
var ErrLockBusy = errors.New("share lifecycle lock is held")

var errLockUnsupported = errors.New("share lifecycle locks are unavailable on this platform")

// Lock is the platform-specific share lifecycle lock.
type Lock struct{}

// TryLock reports that share hosting is unavailable on this platform.
func TryLock(path string) (*Lock, int, error) { return nil, 0, errLockUnsupported }

// Release is a no-op because TryLock cannot succeed on this platform.
func (l *Lock) Release() {}
