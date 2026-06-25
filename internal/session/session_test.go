package session

import (
	"sync"
	"sync/atomic"
	"testing"

	"github.com/richlegrand/bitbang/internal/auth"
	"github.com/richlegrand/bitbang/internal/protocol"
	"github.com/richlegrand/bitbang/internal/streamtype"
)

// countingHandler is a minimal StreamHandler stub used to detect whether
// the session dispatched a SYN/DAT/FIN to a real handler. If any of these
// counters increments without the session being marked ready, the auth
// gate is broken.
type countingHandler struct {
	mu       sync.Mutex
	onSYN    int
	onDAT    int
	onFIN    int
	connects int
}

func (h *countingHandler) Type() string { return "http" }
func (h *countingHandler) OnConnect(_ string) error {
	h.mu.Lock()
	h.connects++
	h.mu.Unlock()
	return nil
}
func (h *countingHandler) OnSYN(_ streamtype.Stream, _ []byte, _ bool) error {
	h.mu.Lock()
	h.onSYN++
	h.mu.Unlock()
	return nil
}
func (h *countingHandler) OnDAT(_ streamtype.Stream, _ []byte) error {
	h.mu.Lock()
	h.onDAT++
	h.mu.Unlock()
	return nil
}
func (h *countingHandler) OnFIN(_ streamtype.Stream, _ []byte) error {
	h.mu.Lock()
	h.onFIN++
	h.mu.Unlock()
	return nil
}

// newTestSession constructs a Session without touching webrtc — the
// sendFrame field is replaced with a capturing function. handler is
// the type="http" stub; tests inspect it after sending frames.
func newTestSession(t *testing.T, pin string, handler streamtype.StreamHandler) (
	sess *Session,
	captured *atomic.Int64,
) {
	t.Helper()
	captured = &atomic.Int64{}
	sess = &Session{
		PIN:           auth.New(pin),
		handlers:      map[string]streamtype.StreamHandler{handler.Type(): handler},
		streamHandler: make(map[uint32]streamtype.StreamHandler),
		reasm:         make(map[uint32][]byte),
	}
	sess.sendFrame = func(streamID uint32, flags uint16, payload []byte) error {
		captured.Add(1)
		return nil
	}
	return sess, captured
}

// TestSYNBeforeAuth_Rejected is the regression test for the
// jacopotediosi PIN-bypass exploit (PR #1443 review, 2026-06-22):
// a non-zero-stream SYN must not reach the registered handler until
// the stream-0 connect+auth handshake has completed.
//
// Exploit shape: open WebRTC, skip stream-0 entirely, send a SYN
// with {"type":"http"} on stream 1. Before the fix, this dispatched
// directly to the HTTP handler — full OctoPrint access with no PIN.
func TestSYNBeforeAuth_Rejected(t *testing.T) {
	h := &countingHandler{}
	sess, sent := newTestSession(t, "1234", h)

	// Skip connect/auth entirely. Send a SYN on stream 1.
	syn := protocol.BuildFrame(1, protocol.FlagSYN, []byte(`{"type":"http"}`))
	sess.HandleMessage(syn)

	if h.onSYN != 0 {
		t.Errorf("handler.OnSYN called %d times before auth — gate is broken", h.onSYN)
	}
	if sent.Load() == 0 {
		t.Errorf("session did not emit an error frame for unauthenticated SYN")
	}

	// Belt-and-suspenders: a DAT on a stream never opened must also
	// be dropped silently. (handler-nil short-circuit already handles
	// it, but ready=false is the explicit gate.)
	dat := protocol.BuildFrame(1, protocol.FlagDAT, []byte("hello"))
	sess.HandleMessage(dat)
	if h.onDAT != 0 {
		t.Errorf("handler.OnDAT called %d times before auth", h.onDAT)
	}
}

// TestPoCExactSequence_Rejected replays jacopotediosi's published PoC
// (bitbang_pin_bypass.py) frame-for-frame: it sends the stream-0
// `connect` (which pins the proxy target via handleConnect), then —
// crucially — never answers the auth_required challenge, and instead
// rides an HTTP SYN on stream 1. This is the precise shape the exploit
// uses; it differs from TestSYNBeforeAuth_Rejected in that connect DID
// run (connectPath is set, OnConnect fired), so only the ready gate
// stands between the attacker and the handler.
func TestPoCExactSequence_Rejected(t *testing.T) {
	h := &countingHandler{}
	sess, _ := newTestSession(t, "1234", h) // PIN is set

	// PoC line 102: stream-0 connect — "pins the proxy target".
	connect := protocol.BuildFrame(0, protocol.FlagSYN,
		[]byte(`{"type":"connect","path":"/","version":3}`))
	sess.HandleMessage(connect)

	// Connect must have reached OnConnect (target pinned) but the
	// session must NOT be ready — PIN was required and never supplied.
	if h.connects != 1 {
		t.Fatalf("OnConnect calls = %d, want 1 (connect should pin target)", h.connects)
	}
	sess.mu.Lock()
	ready := sess.ready
	sess.mu.Unlock()
	if ready {
		t.Fatalf("session ready after connect with PIN set and no auth — gate is broken")
	}

	// PoC lines 116-121: the actual request rides stream 1. The PoC
	// never sends a stream-0 `auth`, so this must be rejected.
	syn := protocol.BuildFrame(1, protocol.FlagSYN,
		[]byte(`{"type":"http","method":"GET","pathname":"/"}`))
	sess.HandleMessage(syn)

	if h.onSYN != 0 {
		t.Errorf("PIN BYPASS: handler.OnSYN called %d times via the PoC sequence", h.onSYN)
	}
}

// TestSYNAfterAuth_Dispatched confirms the gate doesn't break the
// happy path: once ready=true, SYNs on application streams reach the
// handler normally.
func TestSYNAfterAuth_Dispatched(t *testing.T) {
	h := &countingHandler{}
	sess, _ := newTestSession(t, "", h) // no PIN required

	// Walk through the legitimate connect path on stream 0. Without
	// PIN, handleConnect sets ready=true and calls sendReady.
	connect := protocol.BuildFrame(0, protocol.FlagSYN,
		[]byte(`{"type":"connect","path":"/","version":3}`))
	sess.HandleMessage(connect)

	if h.connects != 1 {
		t.Errorf("handler.OnConnect calls = %d, want 1", h.connects)
	}

	// Now an application SYN should land.
	syn := protocol.BuildFrame(1, protocol.FlagSYN, []byte(`{"type":"http"}`))
	sess.HandleMessage(syn)

	if h.onSYN != 1 {
		t.Errorf("handler.OnSYN calls = %d, want 1 after ready=true", h.onSYN)
	}
}

// TestOnReady_FiresOnceOnAuth confirms the OnReady hook (used by the
// listener to release an unauthenticated-session slot) fires exactly once
// when a correct PIN completes the handshake.
func TestOnReady_FiresOnceOnAuth(t *testing.T) {
	h := &countingHandler{}
	sess, _ := newTestSession(t, "1234", h)
	var ready int
	sess.OnReady = func() { ready++ }

	sess.HandleMessage(protocol.BuildFrame(0, protocol.FlagSYN,
		[]byte(`{"type":"connect","path":"/","version":3}`)))
	sess.HandleMessage(protocol.BuildFrame(0, protocol.FlagSYN,
		[]byte(`{"type":"auth","pin":"1234"}`)))

	if ready != 1 {
		t.Errorf("OnReady fired %d times, want 1", ready)
	}
}

// TestThreeStrikes_CountsAndStaysUnready confirms repeated wrong PINs
// accumulate toward maxAuthFails, never flip the session ready, and the
// close path (nil-guarded in tests) doesn't panic.
func TestThreeStrikes_CountsAndStaysUnready(t *testing.T) {
	h := &countingHandler{}
	sess, _ := newTestSession(t, "1234", h)
	var ready int
	sess.OnReady = func() { ready++ }

	sess.HandleMessage(protocol.BuildFrame(0, protocol.FlagSYN,
		[]byte(`{"type":"connect","path":"/","version":3}`)))
	for i := 0; i < maxAuthFails; i++ {
		sess.HandleMessage(protocol.BuildFrame(0, protocol.FlagSYN,
			[]byte(`{"type":"auth","pin":"0000"}`)))
	}

	sess.mu.Lock()
	fails, isReady := sess.authFails, sess.ready
	sess.mu.Unlock()
	if fails != maxAuthFails {
		t.Errorf("authFails = %d, want %d", fails, maxAuthFails)
	}
	if isReady {
		t.Error("session became ready after only wrong PINs")
	}
	if ready != 0 {
		t.Errorf("OnReady fired %d times after failures, want 0", ready)
	}
}

// TestSYNAfterFailedPIN_StillRejected ensures that a wrong PIN does
// not flip ready=true: subsequent application SYNs still bounce.
// Without this, an attacker could "auth" with any wrong PIN, ignore
// the failure response, and still slip a SYN through.
func TestSYNAfterFailedPIN_StillRejected(t *testing.T) {
	h := &countingHandler{}
	sess, _ := newTestSession(t, "1234", h)

	// connect → device asks for PIN
	connect := protocol.BuildFrame(0, protocol.FlagSYN,
		[]byte(`{"type":"connect","path":"/","version":3}`))
	sess.HandleMessage(connect)

	// Send the wrong PIN.
	wrong := protocol.BuildFrame(0, protocol.FlagSYN,
		[]byte(`{"type":"auth","pin":"0000"}`))
	sess.HandleMessage(wrong)

	// Now try the bypass SYN.
	syn := protocol.BuildFrame(1, protocol.FlagSYN, []byte(`{"type":"http"}`))
	sess.HandleMessage(syn)

	if h.onSYN != 0 {
		t.Errorf("handler.OnSYN called %d times after failed PIN", h.onSYN)
	}
}
