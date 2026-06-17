package client

import (
	"strings"

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
