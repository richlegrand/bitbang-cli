package client

import (
	"fmt"
	"strings"
	"time"

	"github.com/pion/webrtc/v4"
)

// detectConnectionPath classifies the established ICE path from pion's
// stats report. Returns one of "direct", "relay", or "tcp-relay" — the
// same vocabulary the browser side (bootstrap.js _detectConnectionPath)
// reports, so server-side counters aggregate cleanly across clients.
//
// The classifier scans every transport (data + media, if multiple),
// looks at each transport's selected candidate pair, and picks the
// "worst" outcome across them. A session with data direct + video
// relay should report relay, not direct — otherwise we'd undercount
// relay use.
//
// Returns "direct" when no relay candidate is in the selected pair set,
// when stats aren't yet populated, or when the report parses
// unexpectedly. Failure to classify is never an error — telemetry is
// strictly best-effort and must not break the connection path.
func detectConnectionPath(pc *webrtc.PeerConnection) string {
	report := pc.GetStats()
	path := "direct"
	for _, s := range report {
		ts, ok := s.(webrtc.TransportStats)
		if !ok || ts.SelectedCandidatePairID == "" {
			continue
		}
		pair, ok := report[ts.SelectedCandidatePairID].(webrtc.ICECandidatePairStats)
		if !ok {
			continue
		}
		local, _ := report[pair.LocalCandidateID].(webrtc.ICECandidateStats)
		remote, _ := report[pair.RemoteCandidateID].(webrtc.ICECandidateStats)

		var relay *webrtc.ICECandidateStats
		switch {
		case local.CandidateType == webrtc.ICECandidateTypeRelay:
			relay = &local
		case remote.CandidateType == webrtc.ICECandidateTypeRelay:
			relay = &remote
		}
		if relay == nil {
			continue
		}
		// RelayProtocol is the TURN allocation's transport (set when
		// libwebrtc has it); Protocol is the candidate's wire transport
		// (always set). Either one telling us TCP means tcp-relay.
		proto := relay.RelayProtocol
		if proto == "" {
			proto = relay.Protocol
		}
		if strings.EqualFold(proto, "tcp") {
			path = "tcp-relay"
			// Worst classification possible — no point continuing.
			break
		}
		path = "relay"
	}
	return path
}

// describeConnectionPath renders the selected candidate pair the way the
// listener logs it, for `-v`. The listener has always printed this and the
// connector never did, so the person who asked for verbose output got the
// half that says least about how the session is actually routed.
//
// Reads the pair off the ICE transport rather than GetStats: the stats
// report is not populated yet at the moment the data channel opens, which
// is exactly when this runs.
//
// Says more than the listener's line on purpose -- the wire protocol of the
// selected pair is the difference between "relayed" and "relayed over TCP",
// and only one of those explains the latency.
func describeConnectionPath(pc *webrtc.PeerConnection, elapsed time.Duration) string {
	sctp := pc.SCTP()
	if sctp == nil {
		return ""
	}
	dtls := sctp.Transport()
	if dtls == nil {
		return ""
	}
	ice := dtls.ICETransport()
	if ice == nil {
		return ""
	}
	pair, err := ice.GetSelectedCandidatePair()
	if err != nil || pair == nil || pair.Local == nil || pair.Remote == nil {
		return ""
	}
	kind := "DIRECT"
	if pair.Local.Typ == webrtc.ICECandidateTypeRelay ||
		pair.Remote.Typ == webrtc.ICECandidateTypeRelay {
		kind = "RELAY"
	}
	return fmt.Sprintf("connected via %s over %s in %v (local=%s remote=%s)",
		kind, strings.ToUpper(pair.Local.Protocol.String()),
		elapsed.Round(time.Millisecond), pair.Local.Typ, pair.Remote.Typ)
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
