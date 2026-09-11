package client

import (
	"net"
	"strconv"
	"strings"
	"testing"

	"github.com/richlegrand/bitbang-cli/internal/tcpforward"
)

func TestStartLocalForwardingDuplicateBindIsAtomic(t *testing.T) {
	probe, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("probe listen: %v", err)
	}
	port := probe.Addr().(*net.TCPAddr).Port
	_ = probe.Close()

	sess := &Session{done: make(chan struct{})}
	forward := tcpforward.Forward{LocalPort: port, Host: "example.test", Port: 80}
	if _, err := sess.StartLocalForwarding([]tcpforward.Forward{forward, forward}, false); err == nil || !strings.Contains(err.Error(), "bind") {
		t.Fatalf("duplicate bind error = %v, want bind failure", err)
	}

	ln, err := net.Listen("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		t.Fatalf("first listener was not released after atomic bind failure: %v", err)
	}
	_ = ln.Close()
}

func TestStartLocalForwardingBindScope(t *testing.T) {
	for _, tc := range []struct {
		name    string
		gateway bool
		wantIP  string
	}{
		{name: "loopback by default", wantIP: "127.0.0.1"},
		{name: "gateway is explicit", gateway: true, wantIP: "0.0.0.0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			probe, err := net.Listen("tcp4", "127.0.0.1:0")
			if err != nil {
				t.Fatalf("probe listen: %v", err)
			}
			port := probe.Addr().(*net.TCPAddr).Port
			_ = probe.Close()

			sess := &Session{done: make(chan struct{})}
			forwarder, err := sess.StartLocalForwarding([]tcpforward.Forward{{
				LocalPort: port,
				Host:      "example.test",
				Port:      80,
			}}, tc.gateway)
			if err != nil {
				t.Fatalf("StartLocalForwarding: %v", err)
			}
			if got := forwarder.listeners[0].Addr().(*net.TCPAddr).IP.String(); got != tc.wantIP {
				t.Errorf("listener IP = %q, want %q", got, tc.wantIP)
			}
			forwarder.Close()
		})
	}
}
