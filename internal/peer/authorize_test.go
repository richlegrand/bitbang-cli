package peer

import (
	"encoding/base64"
	"encoding/json"
	"testing"

	"github.com/pion/webrtc/v4"

	"github.com/richlegrand/bitbang/internal/identity"
	"github.com/richlegrand/bitbang/internal/protocol"
)

// answeringPair builds a real offerer/answerer SDP exchange so
// HandleAnswer runs against a genuine PeerConnection (SetRemoteDescription
// rejects an answer that doesn't match the local offer). No ICE
// connectivity is needed -- the test stops at SDP + verify.
func answeringPair(t *testing.T, id *identity.Identity) (*Connection, string) {
	t.Helper()

	offerPC, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatalf("offer PC: %v", err)
	}
	t.Cleanup(func() { _ = offerPC.Close() })
	if _, err := offerPC.CreateDataChannel("http", nil); err != nil {
		t.Fatalf("data channel: %v", err)
	}
	offer, err := offerPC.CreateOffer(nil)
	if err != nil {
		t.Fatalf("create offer: %v", err)
	}
	if err := offerPC.SetLocalDescription(offer); err != nil {
		t.Fatalf("set local description: %v", err)
	}

	answerPC, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatalf("answer PC: %v", err)
	}
	t.Cleanup(func() { _ = answerPC.Close() })
	if err := answerPC.SetRemoteDescription(*offerPC.LocalDescription()); err != nil {
		t.Fatalf("answerer set remote: %v", err)
	}
	answer, err := answerPC.CreateAnswer(nil)
	if err != nil {
		t.Fatalf("create answer: %v", err)
	}
	if err := answerPC.SetLocalDescription(answer); err != nil {
		t.Fatalf("answerer set local: %v", err)
	}

	conn := &Connection{ClientID: "client-1", PC: offerPC, identity: id}
	return conn, answerPC.LocalDescription().SDP
}

// encryptedRequest builds the browser-side payload HandleAnswer expects.
func encryptedRequest(t *testing.T, id *identity.Identity, sdp, code string) string {
	t.Helper()
	payload, _ := json.Marshal(map[string]string{
		"fingerprint": extractFingerprint(sdp),
		"nonce":       base64.StdEncoding.EncodeToString([]byte("0123456789abcdef")),
		"code":        code,
	})
	ct, err := encryptToPubkey(t, id, payload)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	return base64.StdEncoding.EncodeToString(ct)
}

// TestAuthorizeGrantsAccessLevel is the core of `bitbang share`'s
// two-credential model: one UID, two codes, each mapping to a role
// that HandleAnswer records before any session exists.
func TestAuthorizeGrantsAccessLevel(t *testing.T) {
	id, err := identity.Load("bitbang-authorize-test", true)
	if err != nil {
		t.Fatalf("identity: %v", err)
	}
	const viewCode = "VIEWCODE123"

	for _, tc := range []struct {
		name, code string
		wantAccess protocol.Access
	}{
		{"control code", id.Code, protocol.AccessControl},
		{"view code", viewCode, protocol.AccessView},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn, answerSDP := answeringPair(t, id)
			conn.Authorize = func(code string) (protocol.Access, bool) {
				switch code {
				case id.Code:
					return protocol.AccessControl, true
				case viewCode:
					return protocol.AccessView, true
				}
				return "", false
			}

			if err := conn.HandleAnswer(answerSDP, encryptedRequest(t, id, answerSDP, tc.code)); err != nil {
				t.Fatalf("HandleAnswer: %v", err)
			}
			if got := conn.Access(); got != tc.wantAccess {
				t.Errorf("Access() = %q, want %q", got, tc.wantAccess)
			}
			conn.mu.Lock()
			failed, nonce := conn.verifyFailed, conn.nonce
			conn.mu.Unlock()
			if failed {
				t.Error("verifyFailed set for an authorized peer")
			}
			if nonce == nil {
				t.Error("nonce not recorded -- the data channel would close on open")
			}
		})
	}
}

// TestAuthorizeRejectsUnknownCode: a guess that matches neither role
// must fail exactly like a bad single-code connection does.
func TestAuthorizeRejectsUnknownCode(t *testing.T) {
	id, err := identity.Load("bitbang-authorize-reject", true)
	if err != nil {
		t.Fatalf("identity: %v", err)
	}
	conn, answerSDP := answeringPair(t, id)
	called := 0
	conn.Authorize = func(string) (protocol.Access, bool) {
		called++
		return "", false
	}

	err = conn.HandleAnswer(answerSDP, encryptedRequest(t, id, answerSDP, "WRONGCODE00"))
	if err == nil {
		t.Fatal("HandleAnswer accepted an unauthorized code")
	}
	if called != 1 {
		t.Errorf("Authorize called %d times, want 1", called)
	}
	if got := conn.Access(); got != "" {
		t.Errorf("Access() = %q after rejection, want empty", got)
	}
	conn.mu.Lock()
	failed, nonce := conn.verifyFailed, conn.nonce
	conn.mu.Unlock()
	if !failed {
		t.Error("verifyFailed not set -- a rejected peer's channel would stay open")
	}
	if nonce != nil {
		t.Error("nonce recorded for a rejected peer")
	}
}

// TestReadOnlyShareRefusesControlCode models `share --read-only`: the
// identity's own code exists (identities always carry one) but the
// policy never grants control, so it is simply not a valid credential.
func TestReadOnlyShareRefusesControlCode(t *testing.T) {
	id, err := identity.Load("bitbang-authorize-readonly", true)
	if err != nil {
		t.Fatalf("identity: %v", err)
	}
	const viewCode = "VIEWONLY0001"
	policy := func(code string) (protocol.Access, bool) {
		if code == viewCode {
			return protocol.AccessView, true
		}
		return "", false
	}

	conn, answerSDP := answeringPair(t, id)
	conn.Authorize = policy
	if err := conn.HandleAnswer(answerSDP, encryptedRequest(t, id, answerSDP, id.Code)); err == nil {
		t.Fatal("read-only share accepted the identity's control code")
	}

	conn2, answerSDP2 := answeringPair(t, id)
	conn2.Authorize = policy
	if err := conn2.HandleAnswer(answerSDP2, encryptedRequest(t, id, answerSDP2, viewCode)); err != nil {
		t.Fatalf("read-only share rejected its view code: %v", err)
	}
	if got := conn2.Access(); got != protocol.AccessView {
		t.Errorf("Access() = %q, want view", got)
	}
}

// TestNoAuthorizeKeepsDefaultCheck guards the unchanged path: without
// a policy, HandleAnswer still compares against identity.Code.
func TestNoAuthorizeKeepsDefaultCheck(t *testing.T) {
	id, err := identity.Load("bitbang-authorize-default", true)
	if err != nil {
		t.Fatalf("identity: %v", err)
	}

	conn, answerSDP := answeringPair(t, id)
	if err := conn.HandleAnswer(answerSDP, encryptedRequest(t, id, answerSDP, id.Code)); err != nil {
		t.Fatalf("default path rejected the correct code: %v", err)
	}
	if got := conn.Access(); got != "" {
		t.Errorf("Access() = %q with no policy, want empty", got)
	}

	conn2, answerSDP2 := answeringPair(t, id)
	if err := conn2.HandleAnswer(answerSDP2, encryptedRequest(t, id, answerSDP2, "BADCODE0000")); err == nil {
		t.Fatal("default path accepted a wrong code")
	}
}
