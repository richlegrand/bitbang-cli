package session

import (
	"encoding/json"

	"github.com/richlegrand/bitbang/internal/protocol"
)

// VideoBridge negotiates a secondary "video" PeerConnection with the browser,
// relaying its SDP/ICE to an external media helper. The Session carries the
// browser side over stream-0 control frames (video_offer / video_answer /
// video_candidate); the bridge carries the helper side. nil when the listener
// was started without --video-fd.
//
// Satisfied structurally by *videohelper.Bridge.
type VideoBridge interface {
	// Start is invoked once, after the data channel is verified and ready.
	// The bridge produces a video offer via onOffer and trickles ICE via
	// onCandidate; the Session forwards both to the browser over stream 0.
	Start(onOffer func(sdp string), onCandidate func(map[string]interface{}))
	// Answer / Candidate feed the browser's reply back to the helper.
	Answer(sdp string)
	Candidate(cand map[string]interface{})
	// Close tears down the video PC for this session.
	Close()
}

// SetVideoBridge attaches a video bridge to the session. Call before the
// data channel opens. No-op effect until the session reaches `ready`.
func (s *Session) SetVideoBridge(v VideoBridge) {
	s.mu.Lock()
	s.video = v
	s.mu.Unlock()
}

// startVideo kicks off the video-PC handshake once the data channel is
// verified+ready, relaying the helper's offer/candidates to the browser as
// stream-0 control frames.
func (s *Session) startVideo(v VideoBridge) {
	v.Start(
		func(sdp string) {
			m, _ := json.Marshal(map[string]string{"type": "video_offer", "sdp": sdp})
			_ = s.sendFrame(0, protocol.FlagSYN, m)
		},
		func(cand map[string]interface{}) {
			m, _ := json.Marshal(map[string]interface{}{"type": "video_candidate", "candidate": cand})
			_ = s.sendFrame(0, protocol.FlagSYN, m)
		},
	)
}
