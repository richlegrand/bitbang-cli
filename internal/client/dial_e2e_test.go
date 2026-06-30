package client

// e2e tests for the connect/dial path: the happy-path handshake and an
// env-gated integration test against a live signaling server.

import (
	"log"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/richlegrand/bitbang/internal/auth"
	"github.com/richlegrand/bitbang/internal/identity"
	"github.com/richlegrand/bitbang/internal/peer"
	"github.com/richlegrand/bitbang/internal/session"
	"github.com/richlegrand/bitbang/internal/signaling"
)

// TestDial_DirectConnect_Success is the happy-path e2e: a real connector and
// listener complete the full handshake over the fake signaling relay and Dial
// returns a live Session.
func TestDial_DirectConnect_Success(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: spins up real pion peer connections")
	}
	id := ephemeralID(t)

	relay := newFakeSignaling()
	defer relay.Close()
	startListener(relay.host(), id)
	waitRegistered(t, relay)

	sess := mustDial(t, relay.host(), id)
	if sess == nil {
		t.Fatal("Dial returned nil session")
	}
	sess.Close()
}

// TestIntegration_RealServer_Connect drives a real internal/peer listener and a
// real Dial connector against a LIVE signaling server (the deployed test env),
// exercising the full single-phase handshake through actual server code:
// register → request → offer (server stamps device_pubkey + creds up front) →
// answer → trickle ICE → DTLS → bidirectional verify → SWSP control handshake.
//
// Both ends run on this machine, so ICE settles on a direct host pair — this
// proves the deployed server's signaling relay + the connector handshake, not
// NAT traversal. It also asserts the server never sends an ice_restart (the
// removed two-phase trigger), which would mean an old build is deployed.
//
// Gated on BITBANG_TEST_SERVER so it never runs in normal CI:
//
//	BITBANG_TEST_SERVER=test.bitba.ng go test -v -run TestIntegration_RealServer ./internal/client/
func TestIntegration_RealServer_Connect(t *testing.T) {
	host := os.Getenv("BITBANG_TEST_SERVER")
	if host == "" {
		t.Skip("set BITBANG_TEST_SERVER (e.g. test.bitba.ng) to run the live integration test")
	}

	id, err := identity.Load("bitbang-integration-test", true) // ephemeral
	if err != nil {
		t.Fatalf("identity.Load: %v", err)
	}

	var (
		mu            sync.Mutex
		conn          *peer.Connection
		sess          *session.Session
		sawICERestart atomic.Bool
	)
	ready := make(chan struct{})
	var readyOnce sync.Once

	sig := signaling.NewClient(host, id)
	sig.OnReady = func() { readyOnce.Do(func() { close(ready) }) }

	go sig.Connect(func(msg signaling.Message) {
		switch msg["type"] {
		case "request":
			c, err := peer.HandleRequest(msg, sig, id, func(data []byte) {
				mu.Lock()
				s := sess
				mu.Unlock()
				if s != nil {
					s.HandleMessage(data)
				}
			}, false)
			if err != nil {
				log.Printf("[integration listener] HandleRequest: %v", err)
				return
			}
			mu.Lock()
			conn = c
			sess = session.New(c.DC, auth.New(""), false)
			mu.Unlock()
		case "answer":
			sdp, _ := msg["sdp"].(string)
			enc, _ := msg["encrypted_request"].(string)
			mu.Lock()
			c := conn
			mu.Unlock()
			if c != nil {
				_ = c.HandleAnswer(sdp, enc)
			}
		case "candidate":
			cdata, _ := msg["candidate"].(map[string]interface{})
			mu.Lock()
			c := conn
			mu.Unlock()
			if c != nil {
				_ = c.AddICECandidate(cdata)
			}
		case "ice_restart":
			// A single-phase server must never send this.
			sawICERestart.Store(true)
		}
	})

	select {
	case <-ready:
	case <-time.After(15 * time.Second):
		t.Fatal("listener did not register with the live server within 15s")
	}

	dialSess, err := Dial(DialOptions{
		Server:      host,
		UID:         id.UID,
		Code:        id.Code,
		Path:        "/",
		Caps:        []string{"file"},
		DialTimeout: 20 * time.Second,
	})
	if err != nil {
		t.Fatalf("Dial against live server %s failed: %v", host, err)
	}
	if dialSess == nil {
		t.Fatal("Dial returned nil session")
	}
	dialSess.Close()

	if sawICERestart.Load() {
		t.Error("listener received an ice_restart from the live server — single-phase server not deployed?")
	}
	t.Logf("connected through live server %s (no ice_restart) ✓", host)
}
