package main

import (
	"sync/atomic"
	"time"
)

// unreadyPeerTimeout allows time for an interactive PIN prompt while bounding
// peers that request a connection and never answer.
const unreadyPeerTimeout = 2 * time.Minute

type deadlineGuard struct {
	done  atomic.Bool
	timer *time.Timer
}

// newDeadlineGuard runs expire unless Done wins the race first.
func newDeadlineGuard(after time.Duration, expire func()) *deadlineGuard {
	g := &deadlineGuard{}
	g.timer = time.AfterFunc(after, func() {
		if g.done.CompareAndSwap(false, true) {
			expire()
		}
	})
	return g
}

func (g *deadlineGuard) Done() {
	if g != nil && g.done.CompareAndSwap(false, true) {
		g.timer.Stop()
	}
}
