package client

import (
	"strings"
	"testing"
	"time"

	"github.com/richlegrand/bitbang/internal/identity"
)

// TestDial_WrongAccessCode_FailsFast is a regression test for a hang:
// when a listener rejects the connection it closes the data channel
// without ever sending verify_nonce_hash, and the client used to block
// forever waiting for that frame (the dial timeout only covers channel
// *open*, which succeeds -- DTLS completes before verification runs).
//
// It matters most for `bitbang share`, whose URLs get pasted around
// and expire, so a stale or mistyped code is the common case rather
// than the exotic one.
func TestDial_WrongAccessCode_FailsFast(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: spins up real pion peer connections")
	}
	relay := newFakeSignaling()
	defer relay.Close()

	id := ephemeralID(t)
	startListener(relay.host(), id)
	waitRegistered(t, relay)

	// Same UID, wrong code -- exactly what a stale share link looks like.
	wrong, err := identity.Load("bitbang-e2e-wrongcode", true)
	if err != nil {
		t.Fatalf("identity.Load: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		_, dialErr := Dial(DialOptions{
			Server:      relay.host(),
			UID:         id.UID,
			Code:        wrong.Code,
			Path:        "/",
			Caps:        []string{"shell"},
			DialTimeout: 15 * time.Second,
		})
		done <- dialErr
	}()

	select {
	case dialErr := <-done:
		if dialErr == nil {
			t.Fatal("Dial succeeded with a wrong access code")
		}
		// The message has to point at the credential -- "timeout" alone
		// would send users chasing a network problem they don't have.
		if !strings.Contains(dialErr.Error(), "access code") {
			t.Errorf("error %q does not name the access code as the cause", dialErr)
		}
	case <-time.After(handshakeTimeout + 20*time.Second):
		t.Fatal("Dial hung after the listener rejected the access code")
	}
}
