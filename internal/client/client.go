package client

import (
	"errors"
	"fmt"
	"time"

	"github.com/richlegrand/bitbang/internal/icehelper"
)

// DialOptions configures a Dial call.
type DialOptions struct {
	// Server is the signaling host (e.g. "bitba.ng"). Empty defaults to
	// "bitba.ng" via signaling.New.
	Server string

	// UID and Code identify the listener — typically parsed out of a URL
	// of the form https://server/UID#CODE. Code is required when the
	// listener was started with one (the default for new identities);
	// supplying it incorrectly fails bidirectional verify.
	UID  string
	Code string

	// Path is the SWSP `connect` path. cp uses "/" since the file-stream
	// type doesn't care about HTTP-style routing.
	Path string

	// Caps is the list of SWSP stream types the client knows how to
	// drive. For cp this is just ["file"]; future shell/cp combos can
	// pass more.
	Caps []string

	// PINPrompt is called if the listener requires PIN auth. The
	// retry argument is 0 on first prompt, 1 on second, etc. Return
	// the PIN string; return an error to abort the dial.
	PINPrompt func(retry int) (string, error)

	// DialTimeout caps how long Dial waits for the data channel to
	// open + verify + ready to land. Zero means no timeout.
	DialTimeout time.Duration

	// ForceRelay requests tier-2 TURN credentials on the initial offer
	// instead of trying direct (STUN-only) first. Skips the lazy fallback
	// for networks known to need a relay. Wired to the `--relay` CLI flag.
	ForceRelay bool

	// Verbose toggles progress logging to stderr.
	Verbose bool

	// trickleDelay overrides how long a relay candidate is deferred before
	// being trickled (single-phase direct-bias). Zero uses the production
	// default (messageTimeoutMs). Test-only seam — kept unexported so it is
	// not part of the public dial surface.
	trickleDelay time.Duration
}

// messageTimeoutMs mirrors MESSAGE_TIMEOUT_MS in web/bootstrap.js
// (bitbang-server): the single-phase relay-candidate trickle delay, in
// milliseconds. TURN creds are stamped on the offer up front; the connector
// gathers a relay candidate immediately but defers trickling it by this long
// so a direct pair can win the race. Keep the two implementations in sync.
const messageTimeoutMs = 3000

// Dial opens a signaling session, negotiates WebRTC, runs bidirectional
// verify, completes the SWSP connect handshake, and returns a Session the
// caller drives file ops on. On any failure the partial connection is
// cleaned up before returning.
func Dial(opts DialOptions) (*Session, error) {
	if opts.UID == "" {
		return nil, errors.New("dial: UID is required")
	}
	if opts.Path == "" {
		opts.Path = "/"
	}
	if opts.DialTimeout == 0 {
		opts.DialTimeout = 30 * time.Second
	}

	sig := New(opts.Server, opts.UID)
	sig.Verbose = opts.Verbose
	if err := sig.Connect(); err != nil {
		return nil, err
	}

	// Signaling owns the offer-arrival; pass it to a buffered channel
	// the Dial body reads. err channel is for terminal signaling errors.
	offerCh := make(chan Message, 1)
	candCh := make(chan Message, 16)
	errCh := make(chan error, 1)

	sig.OnOffer = func(m Message) { offerCh <- m }
	sig.OnCandidate = func(m Message) { candCh <- m }
	sig.OnError = func(msg string) {
		select {
		case errCh <- fmt.Errorf("signaling: %s", msg):
		default:
		}
	}

	// Request the offer. force_relay (if set) asks the server to stamp TURN
	// onto the initial offer so we skip the direct attempt entirely.
	if err := sig.SendRequest(opts.Caps, 3, opts.ForceRelay); err != nil {
		sig.Close()
		return nil, fmt.Errorf("send request: %w", err)
	}

	// Wait for the offer (or terminal error / timeout).
	var offer Message
	select {
	case offer = <-offerCh:
	case err := <-errCh:
		sendConnectionPath(sig, "failed", "signaling_error")
		sig.Close()
		return nil, err
	case <-time.After(opts.DialTimeout):
		sendConnectionPath(sig, "failed", "offer_timeout")
		sig.Close()
		return nil, errors.New("timeout waiting for offer")
	}

	// Build the Peer with the ICE servers the signaling server included
	// in the offer (relay creds, STUN URLs, or empty for direct-only).
	iceServers := icehelper.ParseICEServers(offer)
	peer, err := NewPeer(opts.UID, opts.Code, iceServers)
	if err != nil {
		sig.Close()
		return nil, err
	}
	// Single-phase ICE: bias toward direct by delaying the relay candidate
	// (unless --relay forces relay-only). See Peer.OnLocalCandidate.
	peer.ForceRelay = opts.ForceRelay
	if opts.trickleDelay > 0 {
		peer.trickleDelay = opts.trickleDelay
	}

	// Outbound candidates → signaling.
	peer.OnLocalCandidate(func(c map[string]interface{}) {
		_ = sig.SendCandidate(c)
	})

	// Handle offer → answer. Must run before the candidate drain starts
	// so the first AddICECandidate call has a remote description to
	// attach against. Candidates that arrived on the WS while we were
	// waiting for the offer are queued in candCh (16-deep buffer); the
	// drain below pulls them once the listener-side description is set.
	answerSDP, encryptedRequest, err := peer.HandleOffer(offer)
	if err != nil {
		peer.Close()
		sig.Close()
		return nil, fmt.Errorf("handle offer: %w", err)
	}
	if err := sig.SendAnswer(answerSDP, encryptedRequest); err != nil {
		peer.Close()
		sig.Close()
		return nil, fmt.Errorf("send answer: %w", err)
	}

	// Drain inbound candidates as they arrive. The dispatch goroutine
	// exits when the data channel is up + verified; until then candidates
	// are still trickling.
	candDone := make(chan struct{})
	go func() {
		for {
			select {
			case <-candDone:
				return
			case m := <-candCh:
				cdata, _ := m["candidate"].(map[string]interface{})
				if cdata != nil {
					_ = peer.AddICECandidate(cdata)
				}
			}
		}
	}()

	// Wait for the data channel to open within the dial timeout. Single-phase
	// ICE: TURN creds were stamped on the offer up front and the relay
	// candidate is trickled (after a delay) by Peer.OnLocalCandidate, so
	// there is no fallback round trip — the loop just waits for the channel,
	// a terminal signaling error, or timeout.
	deadline := time.After(opts.DialTimeout)
	dialFail := func(reason string, err error) (*Session, error) {
		close(candDone)
		sendConnectionPath(sig, "failed", reason)
		peer.Close()
		sig.Close()
		return nil, err
	}
waitLoop:
	for {
		select {
		case <-peer.DCReady():
			break waitLoop
		case err := <-errCh:
			return dialFail("signaling_error", err)
		case <-deadline:
			return dialFail("ice_timeout", errors.New("timeout waiting for data channel"))
		}
	}

	// Data channel is up — fire one connection_path report with the
	// path that ICE actually settled on (direct / relay / tcp-relay).
	// Must run before sig.Close, since the report rides the same WS.
	sendConnectionPath(sig, detectConnectionPath(peer.PC), "")

	// Signaling is done with us. Stop the candidate drain and close the
	// WS; from here on traffic flows over WebRTC.
	close(candDone)
	sig.Close()

	sess := newSession(peer)
	sess.Verbose = opts.Verbose

	// Run the control-stream handshake. handshake consumes raw DC
	// messages until it sees `ready`; on success we switch the DC reader
	// into stream-routing mode via startDispatcher.
	if err := sess.handshake(peer, opts.Path, opts.Caps, opts.PINPrompt); err != nil {
		peer.Close()
		return nil, err
	}
	sess.startDispatcher(peer)
	return sess, nil
}

