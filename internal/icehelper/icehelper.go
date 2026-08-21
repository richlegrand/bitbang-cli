// Package icehelper bridges browser-native ICE wire formats (as delivered
// over signaling) and pion's typed representations. Both the listener side
// (internal/peer) and the connector side (internal/client) reach for the
// same three conversions — parsing the server's ice_servers offer, parsing
// an inbound trickle candidate from the peer, and serializing a locally-
// gathered candidate for transmission — so they live here once.
package icehelper

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/pion/webrtc/v4"
)

// FromMessage reads the "ice_servers" field of a signaling message and
// returns it as pion's []webrtc.ICEServer. The input is the full message
// (map[string]interface{}); a missing or malformed ice_servers returns
// nil — callers that need the empty/missing distinction should check
// msg["ice_servers"] themselves.
//
// Missing is a normal state, not an error: the server drops the field
// when it has no STUN to stamp, and omits it from an offer when TURN is
// unavailable. Both mean "gather host candidates only".
func FromMessage(msg map[string]interface{}) []webrtc.ICEServer {
	raw, _ := msg["ice_servers"].([]any)
	if raw == nil {
		return nil
	}

	data, err := json.Marshal(raw)
	if err != nil {
		return nil
	}
	return parseICEServers(data)
}

// parseICEServers decodes a JSON ice_servers array.
//
// The browser-native wire format allows urls to be either a string or
// a []string; both are accepted. A Username triggers password-credential
// type (the only one pion supports for trickle ICE).
func parseICEServers(raw []byte) []webrtc.ICEServer {
	var entries []struct {
		URLs       interface{} `json:"urls"`
		Username   string      `json:"username"`
		Credential string      `json:"credential"`
	}
	if err := json.Unmarshal(raw, &entries); err != nil {
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

// ParseUserICEFile reads an operator-supplied --ice-servers file. Three
// shapes are accepted, so a config lifted from a TURN provider's docs or
// from browser code works without editing: a bare array, an object with
// "ice_servers" (our wire spelling), or one with "iceServers" (the
// RTCConfiguration spelling).
func ParseUserICEFile(data []byte) ([]webrtc.ICEServer, error) {
	// Check the root structural token, ignoring leading whitespace.
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" {
		return nil, fmt.Errorf("empty ICE config file")
	}

	if trimmed[0] == '[' {
		parsed := parseICEServers(data)
		if parsed == nil {
			return nil, fmt.Errorf("not a valid ICE server array")
		}
		return parsed, nil
	}

	if trimmed[0] == '{' {
		// json.RawMessage holds the keys without decoding their bodies yet.
		var wrapper map[string]json.RawMessage
		if err := json.Unmarshal(data, &wrapper); err != nil {
			return nil, err
		}
		for _, key := range []string{"ice_servers", "iceServers"} {
			rawArray, exists := wrapper[key]
			if !exists {
				continue
			}
			parsed := parseICEServers(rawArray)
			if parsed == nil {
				return nil, fmt.Errorf("%s did not parse to an array of ICE servers", key)
			}
			return parsed, nil
		}
	}

	return nil, fmt.Errorf("unexpected JSON format: want an array of ICE servers, " +
		"or an object with an ice_servers or iceServers key")
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
