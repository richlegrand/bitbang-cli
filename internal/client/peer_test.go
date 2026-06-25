package client

import (
	"testing"
	"time"

	"github.com/pion/webrtc/v4"
)

// TestRelayTrickleDelay locks in the single-phase ICE policy: a relay
// candidate is deferred (so a direct pair can win the race) unless --relay
// forced relay-only; every other candidate type trickles immediately.
func TestRelayTrickleDelay(t *testing.T) {
	const base = 3 * time.Second
	cases := []struct {
		name       string
		typ        webrtc.ICECandidateType
		forceRelay bool
		want       time.Duration
	}{
		{"relay deferred by default", webrtc.ICECandidateTypeRelay, false, base},
		{"relay immediate when forced", webrtc.ICECandidateTypeRelay, true, 0},
		{"host immediate", webrtc.ICECandidateTypeHost, false, 0},
		{"host immediate when forced", webrtc.ICECandidateTypeHost, true, 0},
		{"srflx immediate", webrtc.ICECandidateTypeSrflx, false, 0},
		{"prflx immediate", webrtc.ICECandidateTypePrflx, false, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := relayTrickleDelay(tc.typ, tc.forceRelay, base); got != tc.want {
				t.Errorf("relayTrickleDelay(%v, forceRelay=%v) = %v, want %v",
					tc.typ, tc.forceRelay, got, tc.want)
			}
		})
	}
}

// TestRelayTrickleDelay_BaseIsConfigurable confirms the returned delay tracks
// the injected base (the seam tests use to keep the e2e timing test fast).
func TestRelayTrickleDelay_BaseIsConfigurable(t *testing.T) {
	if got := relayTrickleDelay(webrtc.ICECandidateTypeRelay, false, 25*time.Millisecond); got != 25*time.Millisecond {
		t.Errorf("got %v, want 25ms", got)
	}
}

// TestNewPeer_DefaultsTrickleDelay verifies a freshly constructed Peer carries
// the production delay (messageTimeoutMs) rather than a zero value, which would
// silently disable the direct-bias.
func TestNewPeer_DefaultsTrickleDelay(t *testing.T) {
	p, err := NewPeer("uid", "code", nil)
	if err != nil {
		t.Fatalf("NewPeer: %v", err)
	}
	defer p.Close()
	if want := messageTimeoutMs * time.Millisecond; p.trickleDelay != want {
		t.Errorf("trickleDelay = %v, want %v", p.trickleDelay, want)
	}
}
