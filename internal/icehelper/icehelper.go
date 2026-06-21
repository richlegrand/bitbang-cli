// Package icehelper bridges browser-native ICE wire formats (as delivered
// over signaling) and pion's typed representations. Both the listener side
// (internal/peer) and the connector side (internal/client) reach for the
// same three conversions — parsing the server's ice_servers offer, parsing
// an inbound trickle candidate from the peer, and serializing a locally-
// gathered candidate for transmission — so they live here once.
package icehelper

import (
	"encoding/json"

	"github.com/pion/webrtc/v4"
)

// ParseICEServers reads the "ice_servers" field of a signaling message
// and returns it as pion's []webrtc.ICEServer. The input is the full
// message (map[string]interface{}); a missing or malformed ice_servers
// returns nil — callers that need the empty/missing distinction should
// check msg["ice_servers"] themselves.
//
// The browser-native wire format allows urls to be either a string or
// a []string; both are accepted. A Username triggers password-credential
// type (the only one pion supports for trickle ICE).
func ParseICEServers(msg map[string]interface{}) []webrtc.ICEServer {
	raw, ok := msg["ice_servers"]
	if !ok {
		return nil
	}
	// ice_servers arrives as []interface{} from JSON unmarshaling; re-marshal
	// + unmarshal into a typed struct is simplest.
	data, err := json.Marshal(raw)
	if err != nil {
		return nil
	}
	var entries []struct {
		URLs       interface{} `json:"urls"`
		Username   string      `json:"username"`
		Credential string      `json:"credential"`
	}
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil
	}
	var out []webrtc.ICEServer
	for _, e := range entries {
		var urls []string
		switch v := e.URLs.(type) {
		case string:
			urls = []string{v}
		case []interface{}:
			for _, u := range v {
				if s, ok := u.(string); ok {
					urls = append(urls, s)
				}
			}
		}
		s := webrtc.ICEServer{URLs: urls}
		if e.Username != "" {
			s.Username = e.Username
			s.Credential = e.Credential
			s.CredentialType = webrtc.ICECredentialTypePassword
		}
		out = append(out, s)
	}
	return out
}

// CandidateInit converts a JSON-decoded RTCIceCandidate-shaped object
// (as sent by browsers via signaling) to pion's init form. Returns
// ok=false for the empty/end-of-candidates marker so callers can no-op
// instead of forwarding it to pion.
func CandidateInit(candidateData map[string]interface{}) (webrtc.ICECandidateInit, bool) {
	candidateStr, _ := candidateData["candidate"].(string)
	if candidateStr == "" {
		return webrtc.ICECandidateInit{}, false
	}
	sdpMid, _ := candidateData["sdpMid"].(string)
	sdpMLineIndexFloat, _ := candidateData["sdpMLineIndex"].(float64)
	sdpMLineIndex := uint16(sdpMLineIndexFloat)
	return webrtc.ICECandidateInit{
		Candidate:     candidateStr,
		SDPMid:        &sdpMid,
		SDPMLineIndex: &sdpMLineIndex,
	}, true
}

// CandidateMap converts a pion locally-gathered candidate to the JSON-
// shaped map the browser expects on the wire (matching
// RTCIceCandidate.toJSON()). The signaling layer ships it verbatim
// inside the candidate field of a "candidate" message.
func CandidateMap(c *webrtc.ICECandidate) map[string]interface{} {
	j := c.ToJSON()
	return map[string]interface{}{
		"candidate":     j.Candidate,
		"sdpMid":        j.SDPMid,
		"sdpMLineIndex": j.SDPMLineIndex,
	}
}
