package signaling

import (
	"testing"
	"time"
)

func TestStopInterruptsReconnect(t *testing.T) {
	c := testClient(t)
	c.ServerWS = "ws://127.0.0.1:1"

	done := make(chan struct{})
	go func() {
		c.Connect(func(Message) {})
		close(done)
	}()

	time.Sleep(20 * time.Millisecond)
	c.Stop()
	c.Stop()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Connect did not stop during its reconnect delay")
	}
}
