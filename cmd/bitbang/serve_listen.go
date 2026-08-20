package main

import (
	"fmt"
	"io"
	"log"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/richlegrand/bitbang/internal/auth"
	"github.com/richlegrand/bitbang/internal/fileshare"
	"github.com/richlegrand/bitbang/internal/identity"
	"github.com/richlegrand/bitbang/internal/links"
	"github.com/richlegrand/bitbang/internal/pairing"
	"github.com/richlegrand/bitbang/internal/peer"
	"github.com/richlegrand/bitbang/internal/peerset"
	"github.com/richlegrand/bitbang/internal/protocol"
	"github.com/richlegrand/bitbang/internal/session"
	"github.com/richlegrand/bitbang/internal/signaling"
	"github.com/richlegrand/bitbang/internal/videohelper"
)

// listener accepts peers for one `serve` process: everything the signaling
// loop needs, and one method per message type it can be handed.
//
// The message types arrive in a fixed order for any one peer -- request,
// then answer, then candidates -- but interleaved freely across peers, so
// each handler looks its peer up rather than closing over one.
type listener struct {
	cfg       serveConfig
	id        *identity.Identity
	share     *fileshare.FileShare
	shellArgv []string
	pinAuth   *auth.PINAuth
	signaling *signaling.Client
	links     *linkState
	video     *videohelper.Client

	peers *peerset.Set[*servePeer]

	// console is the operator's interactive surface, nil when there is no
	// controlling terminal. Pairing asks it what to grant.
	console *console

	// mirror is where a shell session's output goes, held while a prompt
	// is up. Handlers write to it rather than to os.Stdout directly.
	mirror io.Writer

	// unauthSessions counts peers that have not completed the PIN
	// handshake. Bounds parallel brute-force; released on auth or close.
	unauthSessions atomic.Int32
}

// watch starts the two things that run on their own schedule: the triggers
// that replace the link table, and the timer that retires expired links.
func (l *listener) watch(bold, reset string) {
	poll := func(now time.Time) { pollPeers(l.peers.All(), l.links.current(), now) }
	watchReload(func() {
		if err := l.links.reload(); err != nil {
			// The previous table stays in force: an unreadable file must
			// never degrade to "no links", which grants everything.
			fmt.Fprintf(os.Stderr, "Reload failed, keeping the previous links: %v\n", err)
			return
		}
		if listing := l.links.listing(bold, reset); listing != "" {
			fmt.Print(listing)
			fmt.Print(consoleHint())
		}
		poll(time.Now())
	})
	watchExpiry(linkPoll, poll)
}

// pollNow re-checks live sessions against the table as it stands.
// Called after anything that changes the table, so a revocation reaches
// sessions already open rather than only the next connection.
func (l *listener) pollNow() {
	pollPeers(l.peers.All(), l.links.current(), time.Now())
}

func (l *listener) handleSignal(msg signaling.Message) {
	switch t, _ := msg["type"].(string); t {
	case "request":
		l.handleRequest(msg)
	case "answer":
		l.handleAnswer(msg)
	case "pair_request":
		l.handlePairRequest(msg)
	case "candidate":
		l.handleCandidate(msg)
	case "error":
		log.Printf("Signaling error: %v", msg["message"])
	}
}

// handleRequest builds the connection and installs the code resolver. It
// deliberately does not build a session: the access code arrives with the
// answer, and the handler set depends on what it grants. Frames that show
// up in between are queued.
func (l *listener) handleRequest(msg signaling.Message) {
	clientID, _ := msg["client_id"].(string)
	if clientID == "" || l.peers.Has(clientID) {
		log.Printf("Rejecting duplicate or empty client ID %q", clientID)
		return
	}
	// Real browser IP from the signaling server (never client-set);
	// stamped as X-Forwarded-For on proxied requests so the backend sees
	// the true origin instead of our localhost socket peer.
	browserIP, _ := msg["browser_ip"].(string)

	// Cap concurrent un-authenticated sessions to blunt parallel PIN
	// brute-forcing. A connector must already hold the access code to get
	// this far, but without a cap they could open many sessions at once
	// and guess PINs in parallel. Authenticated sessions release their
	// slot, so legit users never hit this.
	if l.unauthSessions.Load() >= maxUnauthSessions {
		log.Printf("Rejecting connection from %s: too many pending sessions (%d)", clientID, maxUnauthSessions)
		return
	}

	p := newServePeer(clientID)
	p.browserIP = browserIP

	conn, err := peer.HandleRequest(msg, l.signaling, l.id, p.q.Enqueue, l.cfg.verbose)
	if err != nil {
		log.Printf("Failed to create peer connection: %v", err)
		return
	}
	p.conn = conn
	p.q.SetConn(conn)

	// Resolve the presented code against the table as it stands now. Set
	// here, after HandleRequest and before the answer arrives, which is
	// the only window there is: the code rides on the answer, so nothing
	// before this point knows what the connector may reach.
	conn.Authorize = func(code string) (protocol.Access, bool) {
		terms, ok := l.links.current().Authorize(code, time.Now())
		if !ok {
			return protocol.AccessDefault, false
		}
		p.grant(terms)
		return protocol.AccessDefault, true
	}

	// Release the unauth slot exactly once, whichever comes first: the
	// peer authenticates, or it closes.
	l.unauthSessions.Add(1)
	var releaseOnce sync.Once
	p.release = func() { releaseOnce.Do(func() { l.unauthSessions.Add(-1) }) }

	p.deadline = peerset.NewDeadline(unreadyPeerTimeout, func() {
		log.Printf("Dropping %s: handshake not completed within %s", clientID, unreadyPeerTimeout)
		p.close()
	})

	// The video bridge does not depend on scope, so it is built with the
	// request and attached when the session appears.
	if l.video != nil {
		// Forward the data PC's ICE servers so the video PC can use the
		// same STUN/TURN (needed for peers with no direct path).
		var iceServers []map[string]interface{}
		if raw, ok := msg["ice_servers"].([]interface{}); ok {
			for _, srv := range raw {
				if m, ok := srv.(map[string]interface{}); ok {
					iceServers = append(iceServers, m)
				}
			}
		}
		p.bridge = l.video.Bridge(clientID, iceServers)
	}

	if !l.peers.Add(clientID, p) {
		p.close()
		return
	}
	conn.SetOnClose(func() {
		p.close()
		l.peers.Forget(clientID, p)
	})
}

// handleAnswer verifies the answer, which is where the access code is
// checked, and then admits the peer with the handler set its link grants.
func (l *listener) handleAnswer(msg signaling.Message) {
	clientID, _ := msg["client_id"].(string)
	p, ok := l.peers.Get(clientID)
	if !ok {
		return
	}
	sdp, _ := msg["sdp"].(string)

	// Pair-flow answers skip the bidirectional-verify decrypt -- SAS
	// comparison (run from the data-channel OnOpen) is the substitute,
	// since the connector doesn't hold an access code yet to feed
	// encrypted_request with.
	if p.conn.PairingMode {
		if err := p.conn.HandlePairAnswer(sdp); err != nil {
			log.Printf("Failed to handle pair answer: %v", err)
			p.close()
		}
		return
	}

	encrypted, _ := msg["encrypted_request"].(string)
	if err := p.conn.HandleAnswer(sdp, encrypted); err != nil {
		log.Printf("Failed to handle answer: %v", err)
		p.close()
		return
	}

	// Authorize ran inside HandleAnswer, so the terms are known. Build the
	// handler set from what they grant, rather than building everything
	// and checking later: sendReady derives advertised caps from the set,
	// and OnConnect never runs for a handler that was never built.
	label, terms := p.granted()
	if label == "" {
		log.Printf("Dropping %s: answer accepted with no terms resolved", clientID)
		p.close()
		return
	}
	granted := terms.GrantSet(offeredScopes(l.cfg))
	h := buildHandlers(l.cfg, granted, l.share, l.shellArgv, l.id, p.browserIP, l.mirror)

	sess := session.New(p.conn.DC, l.pinAuth, l.cfg.verbose, h.all...)
	sess.OnReady = func() {
		p.deadline.Done()
		p.release()
	}
	if p.bridge != nil {
		sess.SetVideoBridge(p.bridge)
	}
	if !p.admit(sess, h) {
		// Teardown won the race; nothing published, so close the session
		// we just built rather than leaking it.
		sess.Close()
		return
	}
	if label != links.MeLabel {
		log.Printf("Peer %s authorized on link %q (%v)", clientID, label, terms.Grants(offeredScopes(l.cfg)))
	}
}

// handlePairRequest admits a connector that has no access code yet. Pairing
// is unscoped: PairingMode skips the code check entirely, so no terms are
// ever resolved for this peer and the poll leaves it alone. It never gets a
// session here either -- the handshake delivers a code the connector
// reconnects with, and that connection is scoped like any other.
func (l *listener) handlePairRequest(msg signaling.Message) {
	clientID, _ := msg["client_id"].(string)
	if clientID == "" || l.peers.Has(clientID) {
		log.Printf("Rejecting duplicate pair client ID %q", clientID)
		return
	}
	// The SAS prompt runs on the console so its question is not scrolled
	// away by a mirroring shell session -- it is the one prompt that
	// cannot be answered if you miss it.
	conn, err := peer.HandlePairRequest(msg, l.signaling, l.id,
		l.sasPrompt(), l.cfg.verbose)
	if err != nil {
		log.Printf("Failed to handle pair request: %v", err)
		return
	}

	// Hand over a minted link rather than the identity's own code, so the
	// pairing is visible in the table, revocable on its own, and can be
	// limited or made to lapse.
	remoteIP, _ := msg["remote_ip"].(string)
	conn.GrantPairCredential = func() (string, bool) {
		return grantForPairing(l.console, l.links, remoteIP)
	}
	p := newServePeer(clientID)
	p.conn = conn
	p.q.SetConn(conn)
	p.deadline = peerset.NewDeadline(unreadyPeerTimeout, func() {
		log.Printf("Dropping pair %s: handshake not completed within %s", clientID, unreadyPeerTimeout)
		p.close()
	})
	if !l.peers.Add(clientID, p) {
		p.close()
		return
	}
	conn.SetOnClose(func() {
		p.close()
		l.peers.Forget(clientID, p)
	})
}

func (l *listener) handleCandidate(msg signaling.Message) {
	clientID, _ := msg["client_id"].(string)
	p, ok := l.peers.Get(clientID)
	if !ok {
		return
	}
	candidate, _ := msg["candidate"].(map[string]interface{})
	_ = p.conn.AddICECandidate(candidate)
}

// sasPrompt reads the SAS on the console, falling back to the package
// default when there is no terminal.
//
// The listener never displays the code -- the operator hears it from the
// connector and types it -- so this stays a free-text prompt rather than
// becoming a confirmation. A y/n here would be exactly the autopilot
// approval the pairing design exists to prevent.
func (l *listener) sasPrompt() pairing.PromptFunc {
	if !l.console.Available() {
		return pairing.DefaultTTYPrompt
	}
	return func(attempt int) (string, pairing.PromptStatus) {
		typed, err := l.console.Ask(
			fmt.Sprintf("Enter code (attempt %d/%d)", attempt, pairing.MaxSASAttempts), "")
		if err != nil {
			return "", pairing.PromptAbort
		}
		return typed, pairing.PromptOK
	}
}
