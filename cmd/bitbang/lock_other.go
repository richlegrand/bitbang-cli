//go:build !unix

package main

// On non-unix platforms (Windows) there is no cross-process identity lock yet,
// so concurrent same-identity listeners there can still preempt each other at
// the signaling server. This stub keeps the build portable; revisit with
// LockFileEx if Windows listeners become common.
type identityLock struct{}

func acquireIdentityLock(dir string) (*identityLock, int, error) { return &identityLock{}, 0, nil }

func (l *identityLock) release() {}
