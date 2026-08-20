package main

import (
	"log"
	"time"

	"github.com/richlegrand/bitbang/internal/framequeue"
	"github.com/richlegrand/bitbang/internal/links"
	"github.com/richlegrand/bitbang/internal/peer"
	"github.com/richlegrand/bitbang/internal/peerset"
	"github.com/richlegrand/bitbang/internal/session"
	"github.com/richlegrand/bitbang/internal/streamtype"
	"github.com/richlegrand/bitbang/internal/videohelper"
)

// servePeer is one connector's lifecycle on a `serve` listener.
//
// The session cannot be built when the connection is: the handler set
// depends on what the presented access code grants, and that code does
// not arrive until the SDP answer. So frames queue from the moment the
// data channel opens -- the browser sends its stream-0 connect
// immediately -- and are delivered once admit has chosen the handlers.
// framequeue owns that sequencing and the lock guarding everything
// installed late.
type servePeer struct {
	clientID string
	q        *framequeue.Queue

	conn     *peer.Connection
	deadline *peerset.Deadline
	// browserIP is the connecting browser's IP as reported by the
	// signaling server, needed when the handler set is built later.
	browserIP string
	release   func()
	bridge    *videohelper.Bridge

	// label and terms are recorded when the code resolves, and re-read by
	// the poll to decide whether this session may still run.
	label string
	terms links.Terms

	sess  *session.Session
	shell *streamtype.ShellHandler
	tcp   *streamtype.TCPHandler
}

func newServePeer(clientID string) *servePeer {
	return &servePeer{clientID: clientID, q: framequeue.New(nil, 0)}
}

// grant records the terms a presented code resolved to. It runs on the
// signaling read loop from inside Authorize, before the answer is
// applied and so before any session exists.
func (p *servePeer) grant(terms links.Terms) {
	p.q.Locked(func(bool) {
		p.label = terms.Label
		p.terms = terms
	})
}

// granted reports the label and terms this peer connected under.
func (p *servePeer) granted() (string, links.Terms) {
	var label string
	var terms links.Terms
	p.q.Locked(func(bool) {
		label, terms = p.label, p.terms
	})
	return label, terms
}

// admit installs the session and its handlers, then releases the queued
// frames. False means teardown got there first.
func (p *servePeer) admit(sess *session.Session, h sessionHandlers) bool {
	return p.q.Publish(sess.HandleMessage, func() {
		p.sess = sess
		p.shell = h.shell
		p.tcp = h.tcp
	})
}

// close tears the peer down once, whichever path gets there first: the
// data channel closing, the handshake deadline, a failed answer, or the
// poll deciding the link no longer permits this session.
func (p *servePeer) close() bool {
	return p.q.Close(func() func() {
		sess, shell, tcp, bridge := p.sess, p.shell, p.tcp, p.bridge
		deadline, release := p.deadline, p.release
		return func() {
			if deadline != nil {
				deadline.Done()
			}
			if sess != nil {
				sess.Close()
			}
			// Kill any shell processes -- without this they outlive the
			// browser tab and keep holding their max-sessions slot.
			if shell != nil {
				shell.Close()
			}
			if tcp != nil {
				tcp.CloseAll()
			}
			if bridge != nil {
				bridge.Close()
			}
			if release != nil {
				release()
			}
		}
	})
}

// revoke ends a live session because its link no longer permits it,
// telling the connector why before dropping the channel. session.Goodbye
// owns the ordering that makes the message actually arrive.
func (p *servePeer) revoke(why string) {
	var sess *session.Session
	p.q.Locked(func(bool) { sess = p.sess })
	if sess == nil {
		p.close()
		return
	}
	sess.Goodbye(why) // told, and serving nothing further, right now

	// Off the caller's goroutine: the poll walks every peer, and none of
	// them should wait on another's socket draining.
	go func() {
		sess.WaitDrained()
		p.close()
	}()
}

// whyGone words the message for a session whose code is no longer in the
// table. Looks the label up to tell the three cases apart; the decision
// to close was already made on the code.
func whyGone(table *links.Table, label string, now time.Time) (peerMsg, logLine string) {
	entry, ok := table.ByLabel(label)
	switch {
	case !ok:
		return "this link was revoked", "was deleted"
	case entry.Code == "" && entry.Check(now) != nil:
		return "this link has expired", "expired"
	default:
		return "this link was reissued; ask for the new URL", "was reissued"
	}
}

// pollPeers closes every live session whose link no longer permits it,
// re-resolving each one against the table as it stands now rather than
// re-checking the terms it captured when it connected -- a snapshot
// cannot show that someone deleted the entry.
//
// Deletion and expiry are the same check, which is why one function has
// two triggers: a ticker for clock-driven expiry, where nothing touches
// the file, and a direct call after reload so a deletion takes effect at
// once rather than up to a tick later.
func pollPeers(peers []*servePeer, table *links.Table, now time.Time) {
	offered := table.Offered()
	for _, p := range peers {
		label, had := p.granted()
		if label == "" {
			continue // still handshaking; nothing granted yet
		}
		// By code, not by label. The code is the credential this session
		// presented; the label is a name for the row, and renaming a link
		// must not disconnect the person holding it. The label is still
		// worth carrying for the log line -- it reads far better than a
		// code fragment -- but it decides nothing.
		current, ok := table.ByCode(had.Code)
		if !ok {
			// The credential is gone, and that is the whole decision. The
			// label is consulted only to say which way it went, because
			// "revoked", "expired", and "reissued" call for different
			// things from whoever is reading it.
			why, logLine := whyGone(table, label, now)
			log.Printf("Closing %s: link %q %s", p.clientID, label, logLine)
			p.revoke(why)
			continue
		}
		switch {
		case current.Check(now) != nil:
			log.Printf("Closing %s: link %q expired", p.clientID, label)
			p.revoke("this link has expired")
		case !links.SameGrants(had, current, offered):
			// The handler set was fixed when the session was built and
			// cannot shrink in place, so a narrowed link has to reconnect.
			log.Printf("Closing %s: link %q narrowed its scope", p.clientID, label)
			p.revoke("this link's permissions changed; reconnect")
		}
	}
}
