package session

import (
	"encoding/json"
	"testing"

	"github.com/richlegrand/bitbang/internal/auth"
	"github.com/richlegrand/bitbang/internal/protocol"
	"github.com/richlegrand/bitbang/internal/streamtype"
)

// readySession drives a session through the stream-0 connect handshake
// and returns the decoded `ready` message the listener emitted.
func readySession(t *testing.T, access protocol.Access) map[string]interface{} {
	t.Helper()
	h := &countingHandler{}
	sess := &Session{
		PIN:           auth.New(""),
		Access:        access,
		handlers:      map[string]streamtype.StreamHandler{h.Type(): h},
		streamHandler: make(map[uint32]streamtype.StreamHandler),
		reasm:         make(map[uint32][]byte),
	}

	var readyPayload []byte
	sess.sendFrame = func(streamID uint32, flags uint16, payload []byte) error {
		if streamID == 0 {
			var peek struct {
				Type string `json:"type"`
			}
			_ = json.Unmarshal(payload, &peek)
			if peek.Type == "ready" {
				readyPayload = append([]byte(nil), payload...)
			}
		}
		return nil
	}

	sess.HandleMessage(protocol.BuildFrame(0, protocol.FlagSYN,
		[]byte(`{"type":"connect","path":"/","version":3}`)))

	if readyPayload == nil {
		t.Fatal("listener never sent a ready message")
	}
	var msg map[string]interface{}
	if err := json.Unmarshal(readyPayload, &msg); err != nil {
		t.Fatalf("ready is not valid JSON: %v", err)
	}
	return msg
}

func TestReadyCarriesAccessWhenSet(t *testing.T) {
	for _, access := range []protocol.Access{protocol.AccessControl, protocol.AccessView} {
		msg := readySession(t, access)
		if got, _ := msg["access"].(string); got != string(access) {
			t.Errorf("Access=%q: ready carried access=%q", access, got)
		}
		// The additive field must not disturb the existing contract.
		if msg["type"] != "ready" || msg["routing"] != "target-prefix" {
			t.Errorf("Access=%q: ready lost existing fields: %v", access, msg)
		}
		if _, ok := msg["caps"]; !ok {
			t.Errorf("Access=%q: ready lost caps", access)
		}
	}
}

func TestReadyOmitsAccessWhenUnset(t *testing.T) {
	msg := readySession(t, protocol.AccessDefault)
	// Listeners without per-peer roles (serve shell/files/proxy) must
	// emit exactly the pre-existing message -- an empty access string on
	// the wire would be a new, meaningless value for clients to handle.
	if _, present := msg["access"]; present {
		t.Errorf("ready included an access field with no role set: %v", msg)
	}
}
