// Package peer manages WebRTC peer connections with browsers.
//
// For each incoming connection request, it creates a PeerConnection with ICE
// servers from the signaling server, generates an SDP offer with a data channel,
// and handles the answer and trickle ICE candidates.
package peer

import (
	"encoding/json"
	"fmt"
	"log"

	"github.com/pion/webrtc/v4"
	"github.com/richlegrand/bitbang/internal/signaling"
)

// OnMessageFunc is called for each data channel message.
type OnMessageFunc func(data []byte)

// Connection represents a WebRTC peer connection with a browser client.
type Connection struct {
	ClientID  string
	PC        *webrtc.PeerConnection
	DC        *webrtc.DataChannel
	sig       *signaling.Client
	OnMessage OnMessageFunc
}

// HandleRequest creates a new peer connection in response to a browser's
// connection request. It configures ICE servers, creates the data channel,
// generates an SDP offer, and sends it back via the signaling client.
// The onMessage callback is called for each data channel message.
func HandleRequest(msg signaling.Message, sig *signaling.Client, onMessage OnMessageFunc, verbose bool) (*Connection, error) {
	clientID, _ := msg["client_id"].(string)
	if clientID == "" {
		return nil, fmt.Errorf("missing client_id")
	}

	// Parse ICE servers from the signaling server's request message
	iceServers := parseICEServers(msg)

	config := webrtc.Configuration{
		ICEServers: iceServers,
	}

	pc, err := webrtc.NewPeerConnection(config)
	if err != nil {
		return nil, fmt.Errorf("create peer connection: %w", err)
	}

	conn := &Connection{
		ClientID:  clientID,
		PC:        pc,
		sig:       sig,
		OnMessage: onMessage,
	}

	// Create data channel (we are the offerer)
	dc, err := pc.CreateDataChannel("http", nil)
	if err != nil {
		pc.Close()
		return nil, fmt.Errorf("create data channel: %w", err)
	}
	conn.DC = dc

	dc.OnOpen(func() {
		log.Printf("Data channel opened for %s", clientID)
	})

	dcClosed := false

	dc.OnClose(func() {
		dcClosed = true
		log.Printf("Data channel closed for %s", clientID)
		pc.Close()
	})

	dc.OnMessage(func(msg webrtc.DataChannelMessage) {
		if conn.OnMessage != nil {
			conn.OnMessage(msg.Data)
		}
	})

	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		if dcClosed {
			return
		}
		if verbose {
			log.Printf("Connection state for %s: %s", clientID, state.String())
		}
		if state == webrtc.PeerConnectionStateFailed {
			pc.Close()
		}
	})

	// Send trickle ICE candidates to browser via signaling
	pc.OnICECandidate(func(candidate *webrtc.ICECandidate) {
		if candidate == nil {
			return // gathering complete
		}
		candidateJSON := candidate.ToJSON()
		sig.Send(signaling.Message{
			"type":      "candidate",
			"client_id": clientID,
			"candidate": map[string]interface{}{
				"candidate":     candidateJSON.Candidate,
				"sdpMid":        candidateJSON.SDPMid,
				"sdpMLineIndex": candidateJSON.SDPMLineIndex,
			},
		})
	})

	// Create and send offer
	offer, err := pc.CreateOffer(nil)
	if err != nil {
		pc.Close()
		return nil, fmt.Errorf("create offer: %w", err)
	}
	if err := pc.SetLocalDescription(offer); err != nil {
		pc.Close()
		return nil, fmt.Errorf("set local description: %w", err)
	}

	// Wait for ICE gathering to complete so all candidates are in the SDP
	gatherComplete := webrtc.GatheringCompletePromise(pc)
	<-gatherComplete

	// Send the offer with all candidates bundled
	sig.Send(signaling.Message{
		"type":      "offer",
		"client_id": clientID,
		"sdp":       pc.LocalDescription().SDP,
		"streams":   map[string]interface{}{},
	})

	return conn, nil
}

// HandleAnswer sets the remote description from the browser's SDP answer.
func (c *Connection) HandleAnswer(sdp string) error {
	answer := webrtc.SessionDescription{
		Type: webrtc.SDPTypeAnswer,
		SDP:  sdp,
	}
	if err := c.PC.SetRemoteDescription(answer); err != nil {
		return fmt.Errorf("set remote description: %w", err)
	}
	return nil
}

// AddICECandidate adds a trickle ICE candidate from the browser.
func (c *Connection) AddICECandidate(candidateData map[string]interface{}) error {
	candidateStr, _ := candidateData["candidate"].(string)
	if candidateStr == "" {
		return nil // empty candidate = end of candidates
	}

	sdpMid, _ := candidateData["sdpMid"].(string)
	sdpMLineIndexFloat, _ := candidateData["sdpMLineIndex"].(float64)
	sdpMLineIndex := uint16(sdpMLineIndexFloat)

	candidate := webrtc.ICECandidateInit{
		Candidate:     candidateStr,
		SDPMid:        &sdpMid,
		SDPMLineIndex: &sdpMLineIndex,
	}

	if err := c.PC.AddICECandidate(candidate); err != nil {
		return fmt.Errorf("add ICE candidate: %w", err)
	}
	return nil
}

// Close closes the peer connection.
func (c *Connection) Close() {
	if c.PC != nil {
		c.PC.Close()
	}
}

// parseICEServers extracts ICE server configuration from the signaling
// server's request message and converts to Pion's format.
func parseICEServers(msg signaling.Message) []webrtc.ICEServer {
	raw, ok := msg["ice_servers"]
	if !ok {
		return nil
	}

	// ice_servers arrives as []interface{} from JSON unmarshaling
	data, err := json.Marshal(raw)
	if err != nil {
		return nil
	}

	// Parse the browser-native iceServers format
	var servers []struct {
		URLs       interface{} `json:"urls"`
		Username   string      `json:"username"`
		Credential string      `json:"credential"`
	}
	if err := json.Unmarshal(data, &servers); err != nil {
		return nil
	}

	var iceServers []webrtc.ICEServer
	for _, s := range servers {
		// urls can be string or []string
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

		server := webrtc.ICEServer{URLs: urls}
		if s.Username != "" {
			server.Username = s.Username
			server.Credential = s.Credential
			server.CredentialType = webrtc.ICECredentialTypePassword
		}
		iceServers = append(iceServers, server)
	}

	return iceServers
}
