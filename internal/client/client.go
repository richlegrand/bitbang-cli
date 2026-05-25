package client

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/pion/webrtc/v4"
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

	// Verbose toggles progress logging to stderr.
	Verbose bool
}

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

	// Request the offer.
	if err := sig.SendRequest(opts.Caps, 3); err != nil {
		sig.Close()
		return nil, fmt.Errorf("send request: %w", err)
	}

	// Wait for the offer (or terminal error / timeout).
	var offer Message
	select {
	case offer = <-offerCh:
	case err := <-errCh:
		sig.Close()
		return nil, err
	case <-time.After(opts.DialTimeout):
		sig.Close()
		return nil, errors.New("timeout waiting for offer")
	}

	// Build the Peer with the ICE servers the signaling server included
	// in the offer (relay creds, STUN URLs, or empty for direct-only).
	iceServers := parseICEServers(offer)
	peer, err := NewPeer(opts.UID, opts.Code, iceServers)
	if err != nil {
		sig.Close()
		return nil, err
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

	// Wait for the data channel to open within the dial timeout.
	select {
	case <-peer.DCReady():
	case err := <-errCh:
		close(candDone)
		peer.Close()
		sig.Close()
		return nil, err
	case <-time.After(opts.DialTimeout):
		close(candDone)
		peer.Close()
		sig.Close()
		return nil, errors.New("timeout waiting for data channel")
	}

	// Data channel is up — signaling is done with us. Stop the candidate
	// drain and close the WS; from here on traffic flows over WebRTC.
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

// parseICEServers converts the "ice_servers" field of the offer message
// into pion's []webrtc.ICEServer. Mirrors internal/peer/connection.go
// (parseICEServers) so the client side sees TURN exactly the way the
// listener side does.
func parseICEServers(msg Message) []webrtc.ICEServer {
	raw, ok := msg["ice_servers"]
	if !ok {
		return nil
	}
	data, err := json.Marshal(raw)
	if err != nil {
		return nil
	}
	var servers []struct {
		URLs       interface{} `json:"urls"`
		Username   string      `json:"username"`
		Credential string      `json:"credential"`
	}
	if err := json.Unmarshal(data, &servers); err != nil {
		return nil
	}
	var out []webrtc.ICEServer
	for _, s := range servers {
		var urls []string
		switch v := s.URLs.(type) {
		case string:
			urls = []string{v}
		case []interface{}:
			for _, u := range v {
				if str, ok := u.(string); ok {
					urls = append(urls, str)
				}
			}
		}
		entry := webrtc.ICEServer{URLs: urls}
		if s.Username != "" {
			entry.Username = s.Username
			entry.Credential = s.Credential
			entry.CredentialType = webrtc.ICECredentialTypePassword
		}
		out = append(out, entry)
	}
	return out
}
