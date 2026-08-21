package icehelper

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/pion/webrtc/v4"
)

func TestFromMessage(t *testing.T) {
	// The server drops ice_servers when it has no STUN to stamp, and
	// omits it from an offer when TURN is unavailable. Both are ordinary
	// states meaning "host candidates only", so neither may panic.
	t.Run("missing field returns nil", func(t *testing.T) {
		if got := FromMessage(map[string]interface{}{}); got != nil {
			t.Errorf("got %v, want nil", got)
		}
	})

	t.Run("null field returns nil", func(t *testing.T) {
		var msg map[string]interface{}
		if err := json.Unmarshal([]byte(`{"ice_servers":null}`), &msg); err != nil {
			t.Fatal(err)
		}
		if got := FromMessage(msg); got != nil {
			t.Errorf("got %v, want nil", got)
		}
	})

	t.Run("field of the wrong type returns nil", func(t *testing.T) {
		msg := map[string]interface{}{"ice_servers": "stun:stun.example:3478"}
		if got := FromMessage(msg); got != nil {
			t.Errorf("got %v, want nil", got)
		}
	})

	t.Run("urls as string", func(t *testing.T) {
		msg := map[string]interface{}{
			"ice_servers": []interface{}{
				map[string]interface{}{"urls": "stun:stun.example:3478"},
			},
		}
		got := FromMessage(msg)
		if len(got) != 1 || len(got[0].URLs) != 1 || got[0].URLs[0] != "stun:stun.example:3478" {
			t.Fatalf("got %+v, want single stun url", got)
		}
		// No username → default (unauthenticated) credential type.
		if got[0].Username != "" {
			t.Errorf("unexpected username %q", got[0].Username)
		}
	})

	t.Run("urls as list", func(t *testing.T) {
		msg := map[string]interface{}{
			"ice_servers": []interface{}{
				map[string]interface{}{"urls": []interface{}{"turn:turn.example:3478", "turns:turn.example:5349"}},
			},
		}
		got := FromMessage(msg)
		if len(got) != 1 || len(got[0].URLs) != 2 {
			t.Fatalf("got %+v, want 2 urls", got)
		}
	})

	t.Run("username sets password credential type", func(t *testing.T) {
		msg := map[string]interface{}{
			"ice_servers": []interface{}{
				map[string]interface{}{
					"urls":       "turn:turn.example:3478",
					"username":   "1700000000",
					"credential": "secret",
				},
			},
		}
		got := FromMessage(msg)
		if len(got) != 1 {
			t.Fatalf("got %d servers, want 1", len(got))
		}
		s := got[0]
		if s.Username != "1700000000" || s.Credential != "secret" {
			t.Errorf("creds not parsed: %+v", s)
		}
		if s.CredentialType != webrtc.ICECredentialTypePassword {
			t.Errorf("CredentialType = %v, want password", s.CredentialType)
		}
	})

	t.Run("malformed ice_servers returns nil", func(t *testing.T) {
		msg := map[string]interface{}{"ice_servers": "not-an-array"}
		if got := FromMessage(msg); got != nil {
			t.Errorf("got %v, want nil", got)
		}
	})
}

func TestCandidateInit(t *testing.T) {
	t.Run("empty candidate is end-of-candidates marker", func(t *testing.T) {
		_, ok := CandidateInit(map[string]interface{}{"candidate": ""})
		if ok {
			t.Error("ok = true for empty candidate, want false")
		}
	})

	t.Run("missing candidate key", func(t *testing.T) {
		_, ok := CandidateInit(map[string]interface{}{})
		if ok {
			t.Error("ok = true for missing candidate, want false")
		}
	})

	t.Run("populated candidate", func(t *testing.T) {
		in := map[string]interface{}{
			"candidate":     "candidate:1 1 udp 2113937151 192.0.2.1 54321 typ host",
			"sdpMid":        "0",
			"sdpMLineIndex": float64(0), // JSON numbers decode to float64
		}
		got, ok := CandidateInit(in)
		if !ok {
			t.Fatal("ok = false, want true")
		}
		if got.Candidate != in["candidate"] {
			t.Errorf("Candidate = %q, want %q", got.Candidate, in["candidate"])
		}
		if got.SDPMid == nil || *got.SDPMid != "0" {
			t.Errorf("SDPMid = %v, want 0", got.SDPMid)
		}
		if got.SDPMLineIndex == nil || *got.SDPMLineIndex != 0 {
			t.Errorf("SDPMLineIndex = %v, want 0", got.SDPMLineIndex)
		}
	})
}

func TestCandidateMap(t *testing.T) {
	c := &webrtc.ICECandidate{
		Foundation: "1",
		Priority:   2113937151,
		Address:    "192.0.2.1",
		Protocol:   webrtc.ICEProtocolUDP,
		Port:       54321,
		Typ:        webrtc.ICECandidateTypeHost,
		Component:  1,
		SDPMid:     "0",
	}
	got := CandidateMap(c)

	// Exactly the three browser-native (RTCIceCandidate.toJSON) keys.
	for _, k := range []string{"candidate", "sdpMid", "sdpMLineIndex"} {
		if _, ok := got[k]; !ok {
			t.Errorf("missing key %q in %+v", k, got)
		}
	}
	if len(got) != 3 {
		t.Errorf("got %d keys, want 3: %+v", len(got), got)
	}
	candStr, _ := got["candidate"].(string)
	if !strings.Contains(candStr, "typ host") {
		t.Errorf("candidate string %q missing 'typ host'", candStr)
	}
}

// The --ice-servers file: an operator should be able to paste in what
// their TURN provider handed them, whichever of the three shapes it is.
func TestParseUserICEFile(t *testing.T) {
	want := "turn:turn.example:3478"

	t.Run("bare array", func(t *testing.T) {
		got, err := ParseUserICEFile([]byte(`[{"urls":"` + want + `"}]`))
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0].URLs[0] != want {
			t.Fatalf("got %+v", got)
		}
	})

	t.Run("ice_servers wrapper", func(t *testing.T) {
		got, err := ParseUserICEFile([]byte(`{"ice_servers":[{"urls":"` + want + `"}]}`))
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0].URLs[0] != want {
			t.Fatalf("got %+v", got)
		}
	})

	t.Run("iceServers wrapper", func(t *testing.T) {
		got, err := ParseUserICEFile([]byte(`  {"iceServers":[{"urls":"` + want + `"}]}`))
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0].URLs[0] != want {
			t.Fatalf("got %+v", got)
		}
	})

	t.Run("credentials survive", func(t *testing.T) {
		got, err := ParseUserICEFile([]byte(`[{"urls":["` + want + `"],"username":"u","credential":"p"}]`))
		if err != nil {
			t.Fatal(err)
		}
		if got[0].Username != "u" || got[0].Credential != "p" {
			t.Fatalf("got %+v", got[0])
		}
		if got[0].CredentialType != webrtc.ICECredentialTypePassword {
			t.Errorf("got credential type %v", got[0].CredentialType)
		}
	})

	for _, bad := range []string{"", "   ", "not json", "42", `{"other":[]}`, `{"ice_servers":"nope"}`} {
		t.Run("rejects "+bad, func(t *testing.T) {
			if _, err := ParseUserICEFile([]byte(bad)); err == nil {
				t.Errorf("%q parsed, want an error", bad)
			}
		})
	}
}
