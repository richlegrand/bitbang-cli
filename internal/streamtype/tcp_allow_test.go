package streamtype

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"strings"
	"testing"

	"github.com/richlegrand/bitbang/internal/allowlist"
)

// tcpError drives one SYN and returns the error the handler sent back, or
// "" if it accepted the target.
func tcpError(t *testing.T, h *TCPHandler, host string, port int) string {
	t.Helper()
	s := newTCPTestStream()
	tcpSYN(t, h, s, host, port)
	frame := nextTCPFrame(t, s)
	var status struct {
		Status string `json:"status"`
		Error  string `json:"error"`
	}
	if err := json.Unmarshal(frame.Payload, &status); err != nil {
		return ""
	}
	if status.Status != "error" {
		return ""
	}
	return status.Error
}

// The allowlist is the only thing between a link scoped to one service and
// the whole network, so a target outside it must be refused without dialing.
func TestForwardAllowlistRefusesOtherTargets(t *testing.T) {
	allow, err := allowlist.Parse([]string{"127.0.0.1:22"})
	if err != nil {
		t.Fatal(err)
	}
	h := NewTCP(false, allow)
	dialed := false
	h.DialContext = func(context.Context, string, string) (net.Conn, error) {
		dialed = true
		return nil, nil
	}

	msg := tcpError(t, h, "10.0.0.5", 80)
	if dialed {
		t.Error("a refused target was dialed anyway")
	}
	if !strings.Contains(msg, "allowed forwards") {
		t.Errorf("refusal = %q, want it to say the target is not allowed", msg)
	}
	if !strings.Contains(msg, "127.0.0.1:22") {
		t.Errorf("refusal = %q, want it to list what is allowed", msg)
	}
}

// Same host, different port is the case worth being explicit about: allowing
// ssh must not also allow whatever else that machine runs.
func TestForwardAllowlistIsPerPort(t *testing.T) {
	allow, _ := allowlist.Parse([]string{"127.0.0.1:22"})
	h := NewTCP(false, allow)
	h.DialContext = func(context.Context, string, string) (net.Conn, error) {
		return nil, nil
	}
	if msg := tcpError(t, h, "127.0.0.1", 8080); !strings.Contains(msg, "allowed forwards") {
		t.Errorf("127.0.0.1:8080 was allowed by an allowlist of 127.0.0.1:22 (%q)", msg)
	}
}

// A listener started without -allow-forward keeps reaching anything.
func TestForwardWithoutAllowlistIsUnrestricted(t *testing.T) {
	h := NewTCP(false, nil)
	h.DialContext = func(context.Context, string, string) (net.Conn, error) {
		return nil, errors.New("connection refused")
	}
	msg := tcpError(t, h, "10.0.0.5", 80)
	if strings.Contains(msg, "allowed forwards") {
		t.Errorf("an unrestricted listener refused a target: %q", msg)
	}
}
