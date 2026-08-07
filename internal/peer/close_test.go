package peer

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"
)

// A peer abandoned before its data channel opens must still reach a terminal
// PeerConnection state so listener cleanup has a reliable trigger.
func TestDataChannelCloseDoesNotFireBeforeOpen(t *testing.T) {
	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatalf("new peer connection: %v", err)
	}
	dc, err := pc.CreateDataChannel("http", nil)
	if err != nil {
		t.Fatalf("create data channel: %v", err)
	}

	dcClosed := make(chan struct{})
	dc.OnClose(func() { close(dcClosed) })
	pcClosed := make(chan struct{})
	var once atomic.Bool
	pc.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		if s == webrtc.PeerConnectionStateClosed && once.CompareAndSwap(false, true) {
			close(pcClosed)
		}
	})

	offer, err := pc.CreateOffer(nil)
	if err != nil {
		t.Fatalf("create offer: %v", err)
	}
	if err := pc.SetLocalDescription(offer); err != nil {
		t.Fatalf("set local description: %v", err)
	}
	// The connector never answers; we give up on it.
	_ = pc.Close()

	select {
	case <-pcClosed:
	case <-time.After(5 * time.Second):
		t.Fatal("peer connection never reported a terminal state -- nothing left to hang teardown on")
	}
	select {
	case <-dcClosed:
		t.Log("note: pion now fires DataChannel.OnClose for a never-opened channel")
	case <-time.After(500 * time.Millisecond):
		// Expected: this is the gap the PC-state teardown covers.
	}
}

func TestFireCloseRunsExactlyOnce(t *testing.T) {
	var calls atomic.Int32
	c := &Connection{}
	c.SetOnClose(func() { calls.Add(1) })

	c.fireClose()
	c.fireClose()
	c.fireClose()

	if got := calls.Load(); got != 1 {
		t.Errorf("OnClose ran %d times, want exactly 1", got)
	}
}

func TestSetOnCloseAfterCloseRunsImmediately(t *testing.T) {
	var calls atomic.Int32
	c := &Connection{}
	c.fireClose()
	c.SetOnClose(func() { calls.Add(1) })
	c.SetOnClose(func() { calls.Add(1) })

	if got := calls.Load(); got != 1 {
		t.Errorf("late OnClose ran %d times, want exactly 1", got)
	}
}

func TestFireCloseWithoutCallback(t *testing.T) {
	c := &Connection{}
	c.fireClose()
}
