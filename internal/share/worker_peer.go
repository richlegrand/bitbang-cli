package share

import (
	"sync"
	"time"

	"github.com/richlegrand/bitbang/internal/peer"
	"github.com/richlegrand/bitbang/internal/session"
	"github.com/richlegrand/bitbang/internal/streamtype"
)

// sharePeer owns one connection's role reservation, session, and tmux client.
// teardown releases them exactly once regardless of which close path wins.
type sharePeer struct {
	conn *peer.Connection

	mu      sync.Mutex
	closed  bool
	shell   *streamtype.ShellHandler
	session *session.Session
	// deliver hands one frame to the session. A field rather than a
	// direct call so a test can substitute a delivery that blocks or
	// counts, the same seam Session.sendFrame uses.
	deliver func([]byte)
	// dispatching marks the drain goroutine as running, so frames
	// arriving mid-drain queue behind the backlog and stay in order.
	dispatching   bool
	pending       [][]byte
	pendingBytes  int
	releases      []func()
	establishment *time.Timer
	refusal       *time.Timer
}

// handleMessage routes an inbound data-channel frame to the session,
// queueing it while there is none. Frames start arriving the moment the
// channel opens, which can precede the session: the browser sends its
// stream-0 connect immediately, and losing that frame would hang the
// handshake it belongs to. A peer that fills the queue past
// maxPendingPeerBytes before its session exists is disconnected.
func (p *sharePeer) handleMessage(data []byte) {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	if p.session == nil || p.dispatching {
		if p.pendingBytes+len(data) > maxPendingPeerBytes {
			conn := p.conn
			p.mu.Unlock()
			if conn != nil {
				conn.Close()
			}
			return
		}
		p.pending = append(p.pending, append([]byte(nil), data...))
		p.pendingBytes += len(data)
		p.mu.Unlock()
		return
	}
	deliver := p.deliver
	p.mu.Unlock()
	deliver(data)
}

// hold transfers a reservation to teardown. A false return leaves ownership
// with the caller because teardown has already run.
func (p *sharePeer) hold(release func()) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return false
	}
	p.releases = append(p.releases, release)
	return true
}

// publish installs the session atomically with respect to teardown and drains
// early frames on a per-peer goroutine. It returns false if teardown won.
//
// Delivery must not run on the signaling read loop: a congested PTY write can
// block and would otherwise stop signaling for every peer.
func (p *sharePeer) publish(sh *streamtype.ShellHandler, sess *session.Session) bool {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return false
	}
	p.shell = sh
	p.session = sess
	if p.deliver == nil {
		p.deliver = sess.HandleMessage
	}
	p.dispatching = true
	batch := p.takePendingLocked()
	p.mu.Unlock()

	go p.drain(batch)
	return true
}

// drain delivers the backlog, then whatever arrived while it was
// working, until the queue comes up empty or the peer goes away.
// Clearing dispatching under the lock is what hands routing back to
// handleMessage's direct path with no frame overtaking another.
func (p *sharePeer) drain(batch [][]byte) {
	defer func() {
		p.mu.Lock()
		p.dispatching = false
		p.mu.Unlock()
	}()

	for {
		for _, data := range batch {
			// Stop between frames so teardown cannot be followed by delivery
			// of the rest of the backlog.
			p.mu.Lock()
			deliver, closed := p.deliver, p.closed
			p.mu.Unlock()
			if closed {
				return
			}
			deliver(data)
		}

		p.mu.Lock()
		if p.closed {
			p.mu.Unlock()
			return
		}
		batch = p.takePendingLocked()
		if len(batch) == 0 {
			p.mu.Unlock()
			return
		}
		p.mu.Unlock()
	}
}

func (p *sharePeer) takePendingLocked() [][]byte {
	batch := p.pending
	p.pending = nil
	p.pendingBytes = 0
	return batch
}

// isClosed reports whether teardown has run. Stream admission checks it
// so a frame already in flight when the peer died cannot open a terminal
// on the way out.
func (p *sharePeer) isClosed() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.closed
}

// armEstablishment bounds the time from request to a completed stream-0
// handshake. This also covers peers that authorize but never finish ICE.
func (p *sharePeer) armEstablishment(after time.Duration, expire func()) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.closed {
		p.establishment = time.AfterFunc(after, expire)
	}
}

// markEstablished cancels the establishment deadline. Wired to the
// session's readiness callback, which fires once the stream-0 handshake
// completes over an open data channel.
func (p *sharePeer) markEstablished() {
	p.mu.Lock()
	if p.establishment != nil {
		p.establishment.Stop()
		p.establishment = nil
	}
	p.mu.Unlock()
}

// armRefusal starts the grace period for a peer turned away because its
// role was full. It stays connected long enough to load the page and
// read why, then goes, so a full share cannot be pinned open by peers
// that will never be admitted.
func (p *sharePeer) armRefusal(after time.Duration, expire func()) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.closed {
		p.refusal = time.AfterFunc(after, expire)
	}
}

// teardown hands back every reservation, kills the peer's tmux client,
// and closes the connection. It reports whether this call did the work,
// so callers log and drop map entries exactly once.
//
// The handler is read under the same lock publish writes it under, which
// keeps the two ordered: either teardown sees a live peer and closes its
// handler, or publish finds the peer closed and declines.
func (p *sharePeer) teardown() bool {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return false
	}
	p.closed = true
	if p.establishment != nil {
		p.establishment.Stop()
	}
	if p.refusal != nil {
		p.refusal.Stop()
	}
	sh := p.shell
	conn := p.conn
	releases := p.releases
	p.releases = nil
	p.pending = nil
	p.pendingBytes = 0
	p.mu.Unlock()

	if sh != nil {
		sh.Close()
	}
	for _, release := range releases {
		release()
	}
	if conn != nil {
		conn.Close()
	}
	return true
}

// setConn attaches the peer connection. It is written after
// peer.HandleRequest returns but read from pion's callbacks, so it
// takes the same lock as every other late-bound field.
func (p *sharePeer) setConn(conn *peer.Connection) {
	p.mu.Lock()
	p.conn = conn
	p.mu.Unlock()
}

// roleSlots is the admission counter behind the single-controller rule
// and --max-viewers. A slot is taken when a peer is authorized and held
// for as long as it stays connected. Taking one only when a terminal
// opens would leave both limits unenforced, since a peer can finish the
// handshake and never open one.
type roleSlots struct {
	mu   sync.Mutex
	used int
	max  int
	busy string
}

// newRoleSlots returns a pool of max slots. busy is what a refused peer
// is told. A read-only share sizes its control pool at zero, though
// that is a backstop: authorize never grants control on such a share,
// so nothing reaches the pool to be refused.
func newRoleSlots(max int, busy string) *roleSlots {
	return &roleSlots{max: max, busy: busy}
}

// acquire takes a slot and returns the func that gives it back, or nil
// and the busy message when the pool is full.
//
// The returned release function is idempotent so competing cleanup paths
// cannot decrement the pool twice.
func (r *roleSlots) acquire() (func(), string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.used >= r.max {
		return nil, r.busy
	}
	r.used++
	var once sync.Once
	return func() {
		once.Do(func() {
			r.mu.Lock()
			r.used--
			r.mu.Unlock()
		})
	}, ""
}
