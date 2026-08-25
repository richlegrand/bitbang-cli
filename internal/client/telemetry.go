package client

import (
	"fmt"
	"strings"
	"time"

	"github.com/pion/webrtc/v4"
)

// selectedPair returns the candidate pair ICE settled on, or nil.
//
// Read off the ICE transport rather than GetStats. pion leaves
// TransportStats.SelectedCandidatePairID empty, so anything routed through
// the stats report hits its own "not populated" guard and falls back to a
// default -- which is how a forced-relay session was still classified as
// direct two seconds after connecting.
func selectedPair(pc *webrtc.PeerConnection) *webrtc.ICECandidatePair {
	sctp := pc.SCTP()
	if sctp == nil {
		return nil
	}
	dtls := sctp.Transport()
	if dtls == nil {
		return nil
	}
	ice := dtls.ICETransport()
	if ice == nil {
		return nil
	}
	pair, err := ice.GetSelectedCandidatePair()
	if err != nil || pair == nil || pair.Local == nil || pair.Remote == nil {
		return nil
	}
	return pair
}

func isRelay(pair *webrtc.ICECandidatePair) bool {
	return pair.Local.Typ == webrtc.ICECandidateTypeRelay ||
		pair.Remote.Typ == webrtc.ICECandidateTypeRelay
}

// detectConnectionPath classifies the established ICE path. Returns
// "direct", "relay", or "tcp-relay" -- the same vocabulary the browser side
// (bootstrap.js _detectConnectionPath) reports, so server-side counters
// aggregate cleanly across clients.
//
// Returns "direct" when the pair is unavailable, since telemetry is
// best-effort and must never disturb the connection. That default used to
// be reached on every call: see selectedPair.
func detectConnectionPath(pc *webrtc.PeerConnection) string {
	pair := selectedPair(pc)
	if pair == nil || !isRelay(pair) {
		return "direct"
	}
	if strings.EqualFold(pair.Local.Protocol.String(), "tcp") ||
		strings.EqualFold(pair.Remote.Protocol.String(), "tcp") {
		return "tcp-relay"
	}
	return "relay"
}

// describeConnectionPath renders the selected pair the way the listener logs
// it, for `-v`. The listener has always printed this and the connector never
// did, so the end that asked for verbose output got the half that says least
// about how the session is actually routed.
func describeConnectionPath(pc *webrtc.PeerConnection, elapsed time.Duration) string {
	pair := selectedPair(pc)
	if pair == nil {
		return ""
	}
	kind := "DIRECT"
	if isRelay(pair) {
		kind = "RELAY"
	}
	return fmt.Sprintf("connected via %s over %s in %v (local=%s remote=%s)",
		kind, strings.ToUpper(pair.Local.Protocol.String()),
		elapsed.Round(time.Millisecond), pair.Local.Typ, pair.Remote.Typ)
}

// relayNotice is the one line a connector sees when a session ends up
// relayed without being asked to. The listener has always logged it and the
// connector said nothing, so the end that feels the latency was the end that
// was not told.
//
// Silent when -relay was passed: they already know. Not gated on -v, because
// the point is to reach someone who did not think to ask.
func relayNotice(pc *webrtc.PeerConnection, requested bool) string {
	pair := selectedPair(pc)
	if requested || pair == nil || !isRelay(pair) {
		return ""
	}
	return "Note: relayed -- no direct path was found, so traffic goes through a TURN\n" +
		"      relay. Expect higher latency. Use -ice-servers to relay through your own."
}

// sendConnectionPath fires one telemetry message to the signaling server.
// Fire-and-forget: errors are swallowed because telemetry must never
// disturb the session. Caller is responsible for sending before closing
// the signaling WebSocket — once it's closed this is a no-op.
func sendConnectionPath(sig *Signaling, path, reason string) {
	if sig == nil {
		return
	}
	msg := Message{
		"type": "connection_path",
		"path": path,
	}
	if reason != "" {
		msg["reason"] = reason
	}
	_ = sig.send(msg)
}
