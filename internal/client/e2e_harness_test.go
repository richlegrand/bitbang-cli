package client

// Shared end-to-end test harness for the connector. It stands up an in-process
// signaling server (httptest TLS, matching the wss:// + InsecureSkipVerify
// dialer the real connector/listener use) that relays offer/answer/candidate
// between a real internal/peer listener and client.Dial. pion gathers host
// candidates on loopback, so no real network is touched — this exercises the
// whole handshake: request → offer (+ device_pubkey stamp) → answer → trickle
// ICE → DTLS → bidirectional verify → SWSP control handshake.
//
// The per-feature e2e tests live in dial_e2e_test.go, cp_e2e_test.go, and
// shell_e2e_test.go; they all build on the helpers here.

import (
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pion/turn/v4"

	"github.com/richlegrand/bitbang/internal/auth"
	"github.com/richlegrand/bitbang/internal/identity"
	"github.com/richlegrand/bitbang/internal/peer"
	"github.com/richlegrand/bitbang/internal/session"
	"github.com/richlegrand/bitbang/internal/signaling"
	"github.com/richlegrand/bitbang/internal/streamtype"
)

// fakeSignaling is a minimal one-device/one-connector relay that mirrors what
// the production signaling server stamps: device_pubkey onto the offer (from
// the device's register) and client_id onto messages bound for the device.
// It deliberately omits ice_servers — loopback host candidates suffice.
type fakeSignaling struct {
	srv      *httptest.Server
	upgrader websocket.Upgrader

	mu           sync.Mutex
	deviceConn   *websocket.Conn
	clientConn   *websocket.Conn
	devicePubkey string
	clientID     string

	deviceWrite sync.Mutex
	clientWrite sync.Mutex

	deviceReady chan struct{} // closed once the device has registered
	readyOnce   sync.Once

	// Timing-test knobs (zero-valued for the happy-path test):
	//   offerICEServers — stamped onto the offer so the connector gathers a
	//     relay candidate (mirrors single-phase creds-up-front).
	//   blockICE — drop all trickle candidates (both directions) so ICE never
	//     completes; the connector's WS stays open, letting us observe the
	//     deferred relay candidate instead of Dial closing on a fast direct hit.
	offerICEServers []interface{}
	blockICE        bool

	candMu      sync.Mutex
	clientCands []candEvent // connector→server candidates, in arrival order
}

// candEvent records when a candidate of a given ICE type arrived at the relay.
type candEvent struct {
	typ string // "host", "srflx", "relay", ...
	at  time.Time
}

// clientCandidateEvents returns a snapshot of the connector's candidate
// arrivals in order.
func (f *fakeSignaling) clientCandidateEvents() []candEvent {
	f.candMu.Lock()
	defer f.candMu.Unlock()
	return append([]candEvent(nil), f.clientCands...)
}

// candidateType extracts the "typ <x>" token from an SDP candidate line inside
// a trickle message ({"candidate": {"candidate": "...typ host..."}}).
func candidateType(msg map[string]interface{}) string {
	cd, _ := msg["candidate"].(map[string]interface{})
	line, _ := cd["candidate"].(string)
	if m := candTypeRe.FindStringSubmatch(line); m != nil {
		return m[1]
	}
	return ""
}

var candTypeRe = regexp.MustCompile(`(?:^| )typ (\S+)`)

func newFakeSignaling() *fakeSignaling {
	f := &fakeSignaling{
		deviceReady: make(chan struct{}),
		clientID:    "e2e-client-1",
		upgrader:    websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/ws/device/", f.handleDevice)
	mux.HandleFunc("/ws/client/", f.handleClient)
	f.srv = httptest.NewTLSServer(mux)
	return f
}

// host returns the "127.0.0.1:port" the connector/listener dial (the dialers
// prepend wss:// and skip cert verification).
func (f *fakeSignaling) host() string {
	return strings.TrimPrefix(f.srv.URL, "https://")
}

func (f *fakeSignaling) Close() {
	// Ask any attached listener to stop reconnecting before we tear down the
	// server: signaling.Client.Connect returns on a "preempted" error, so this
	// avoids a goroutine that otherwise reconnect-loops forever against the
	// closed server. Best-effort; the brief pause lets the device read it.
	f.writeDevice(map[string]interface{}{"type": "error", "message": "preempted"})
	time.Sleep(100 * time.Millisecond)
	f.srv.Close()
}

func (f *fakeSignaling) writeDevice(m map[string]interface{}) {
	f.mu.Lock()
	c := f.deviceConn
	f.mu.Unlock()
	if c == nil {
		return
	}
	f.deviceWrite.Lock()
	defer f.deviceWrite.Unlock()
	_ = c.WriteJSON(m)
}

func (f *fakeSignaling) writeClient(m map[string]interface{}) {
	f.mu.Lock()
	c := f.clientConn
	f.mu.Unlock()
	if c == nil {
		return
	}
	f.clientWrite.Lock()
	defer f.clientWrite.Unlock()
	_ = c.WriteJSON(m)
}

// handleDevice processes messages the listener SENDS: register (capture
// pubkey, ack), and offer/candidate (forward to the connector — offers get the
// device_pubkey stamped, as the real server does).
func (f *fakeSignaling) handleDevice(w http.ResponseWriter, r *http.Request) {
	c, err := f.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	f.mu.Lock()
	f.deviceConn = c
	f.mu.Unlock()
	for {
		var msg map[string]interface{}
		if err := c.ReadJSON(&msg); err != nil {
			return
		}
		switch msg["type"] {
		case "register":
			pk, _ := msg["public_key"].(string)
			f.mu.Lock()
			f.devicePubkey = pk
			f.mu.Unlock()
			f.writeDevice(map[string]interface{}{"type": "registered"})
			f.readyOnce.Do(func() { close(f.deviceReady) })
		case "offer":
			f.mu.Lock()
			msg["device_pubkey"] = f.devicePubkey
			if f.offerICEServers != nil {
				msg["ice_servers"] = f.offerICEServers
			}
			f.mu.Unlock()
			f.writeClient(msg)
		case "candidate":
			if f.blockICE {
				continue // keep ICE from completing (device→client dropped)
			}
			f.writeClient(msg)
		}
	}
}

// handleClient processes messages the connector SENDS: request/answer/candidate
// (forward to the device with client_id stamped). connection_path telemetry is
// dropped.
func (f *fakeSignaling) handleClient(w http.ResponseWriter, r *http.Request) {
	c, err := f.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	f.mu.Lock()
	f.clientConn = c
	f.mu.Unlock()
	for {
		var msg map[string]interface{}
		if err := c.ReadJSON(&msg); err != nil {
			return
		}
		switch msg["type"] {
		case "candidate":
			// Record arrival time + type before deciding whether to forward.
			f.candMu.Lock()
			f.clientCands = append(f.clientCands, candEvent{typ: candidateType(msg), at: time.Now()})
			f.candMu.Unlock()
			if f.blockICE {
				continue // client→device dropped too; ICE never completes
			}
			f.mu.Lock()
			msg["client_id"] = f.clientID
			f.mu.Unlock()
			f.writeDevice(msg)
		case "request", "answer":
			f.mu.Lock()
			msg["client_id"] = f.clientID
			f.mu.Unlock()
			f.writeDevice(msg)
		}
	}
}

// startListener runs a real internal/peer listener against the fake signaling
// server, mirroring cmd/bitbang/serve.go's request/answer/candidate wiring.
// Pass stream handlers (file, shell, …) to give the listener capabilities; with
// none, only the control handshake works. It runs in a goroutine and is stopped
// cleanly by fakeSignaling.Close (which sends a "preempted" error).
func startListener(host string, id *identity.Identity, handlers ...streamtype.StreamHandler) {
	sig := signaling.NewClient(host, id)
	var mu sync.Mutex
	var conn *peer.Connection
	var sess *session.Session

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
				log.Printf("[e2e listener] HandleRequest: %v", err)
				return
			}
			mu.Lock()
			conn = c
			sess = session.New(c.DC, auth.New(""), false, handlers...)
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
		}
	})
}

// startTURNServer stands up an in-process pion TURN server on loopback and
// returns the ice_servers entry (browser-native shape) the connector should be
// handed, plus a stop func. Relayed traffic and allocations all stay on
// 127.0.0.1, so no real network is touched.
func startTURNServer(t *testing.T) (iceServer map[string]interface{}, stop func()) {
	t.Helper()
	const (
		user  = "bbtest"
		pass  = "bbpass"
		realm = "bitbang.test"
	)

	udpConn, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("turn listen: %v", err)
	}

	srv, err := turn.NewServer(turn.ServerConfig{
		Realm: realm,
		AuthHandler: func(u, r string, _ net.Addr) ([]byte, bool) {
			if u == user && r == realm {
				return turn.GenerateAuthKey(user, realm, pass), true
			}
			return nil, false
		},
		PacketConnConfigs: []turn.PacketConnConfig{{
			PacketConn: udpConn,
			RelayAddressGenerator: &turn.RelayAddressGeneratorStatic{
				RelayAddress: net.ParseIP("127.0.0.1"),
				Address:      "127.0.0.1",
			},
		}},
	})
	if err != nil {
		udpConn.Close()
		t.Fatalf("turn.NewServer: %v", err)
	}

	port := udpConn.LocalAddr().(*net.UDPAddr).Port
	iceServer = map[string]interface{}{
		"urls":       fmt.Sprintf("turn:127.0.0.1:%d?transport=udp", port),
		"username":   user,
		"credential": pass,
	}
	return iceServer, func() { _ = srv.Close() }
}

// --- Shared helpers for the e2e tests ------------------------------------

func ephemeralID(t *testing.T) *identity.Identity {
	t.Helper()
	id, err := identity.Load("bitbang-e2e-test", true)
	if err != nil {
		t.Fatalf("identity.Load: %v", err)
	}
	return id
}

func waitRegistered(t *testing.T, relay *fakeSignaling) {
	t.Helper()
	select {
	case <-relay.deviceReady:
	case <-time.After(5 * time.Second):
		t.Fatal("listener did not register within 5s")
	}
}

func mustDial(t *testing.T, host string, id *identity.Identity, caps ...string) *Session {
	t.Helper()
	if len(caps) == 0 {
		caps = []string{"file"}
	}
	sess, err := Dial(DialOptions{
		Server:      host,
		UID:         id.UID,
		Code:        id.Code,
		Path:        "/",
		Caps:        caps,
		DialTimeout: 15 * time.Second,
	})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	return sess
}

func typesOf(events []candEvent) []string {
	out := make([]string, len(events))
	for i, e := range events {
		out[i] = e.typ
	}
	return out
}
