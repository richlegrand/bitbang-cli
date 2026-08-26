package main

import (
	"io"
	"os"
	"reflect"
	"strings"
	"syscall"
	"testing"

	"github.com/richlegrand/bitbang/internal/tcpforward"
)

func TestParseConnectOptionsRepeatedForwardsAndReorderedFlags(t *testing.T) {
	got, err := parseConnectOptions([]string{
		"device1",
		"-L", "15432:db.internal:5432",
		"-g",
		"-L", "14450:nas.local:445",
	}, io.Discard)
	if err != nil {
		t.Fatalf("parseConnectOptions: %v", err)
	}
	wantForwards := forwardFlags{
		{LocalPort: 15432, Host: "db.internal", Port: 5432},
		{LocalPort: 14450, Host: "nas.local", Port: 445},
	}
	if !reflect.DeepEqual(got.forwards, wantForwards) {
		t.Fatalf("forwards = %#v, want %#v", got.forwards, wantForwards)
	}
	if !got.gateway {
		t.Fatal("gateway = false, want true")
	}
}

func TestParseConnectOptionsRemoteCommand(t *testing.T) {
	got, err := parseConnectOptions([]string{"device1", "--", "sh", "-c", "echo ok"}, io.Discard)
	if err != nil {
		t.Fatalf("parseConnectOptions: %v", err)
	}
	if want := []string{"sh", "-c", "echo ok"}; !reflect.DeepEqual(got.argv, want) {
		t.Fatalf("argv = %q, want %q", got.argv, want)
	}
}

func TestParseConnectOptionsRejectsForwardWithCommand(t *testing.T) {
	_, err := parseConnectOptions([]string{
		"device1", "-L", "15432:db.internal:5432", "--", "echo", "ok",
	}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "cannot be combined") {
		t.Fatalf("error = %v, want -L/command conflict", err)
	}
}

func TestParseLocalForwardValidTargetsAndBoundaryPorts(t *testing.T) {
	cases := []struct {
		input string
		want  tcpforward.Forward
	}{
		{"1:db.internal:65535", tcpforward.Forward{LocalPort: 1, Host: "db.internal", Port: 65535}},
		{"443:192.0.2.10:1", tcpforward.Forward{LocalPort: 443, Host: "192.0.2.10", Port: 1}},
		{"65535:[2001:db8::5]:443", tcpforward.Forward{LocalPort: 65535, Host: "2001:db8::5", Port: 443}},
	}
	for _, tc := range cases {
		t.Run(tc.input, func(t *testing.T) {
			got, err := parseLocalForward(tc.input)
			if err != nil {
				t.Fatalf("parseLocalForward: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got %#v, want %#v", got, tc.want)
			}
		})
	}
}

func TestParseLocalForwardRejectsMalformedMappings(t *testing.T) {
	for _, input := range []string{
		"", "1234", ":host:80", "1234::80", "1234:host:",
		"0:host:80", "65536:host:80", "1234:host:0", "1234:host:65536",
		"abc:host:80", "1234:2001:db8::5:80", "1234:[not-ipv6]:80",
		"1234:[192.0.2.1]:80",
		"1234:bad host:80", "1234:host/path:80",
	} {
		t.Run(strings.ReplaceAll(input, "/", "_"), func(t *testing.T) {
			if _, err := parseLocalForward(input); err == nil {
				t.Fatalf("parseLocalForward(%q) succeeded, want error", input)
			}
		})
	}
}

func TestParseConnectOptionsRejectsGatewayWithoutForward(t *testing.T) {
	if _, err := parseConnectOptions([]string{"device1", "-g"}, io.Discard); err == nil || !strings.Contains(err.Error(), "requires") {
		t.Fatalf("error = %v, want -g requires -L", err)
	}
}

func TestWaitForForwardExit(t *testing.T) {
	t.Run("session closes", func(t *testing.T) {
		done := make(chan struct{})
		close(done)
		if !waitForForwardExit(done, make(chan os.Signal)) {
			t.Fatal("sessionClosed = false, want true")
		}
	})

	for _, sig := range []os.Signal{syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP} {
		t.Run(sig.String(), func(t *testing.T) {
			signals := make(chan os.Signal, 1)
			signals <- sig
			if waitForForwardExit(make(chan struct{}), signals) {
				t.Fatal("sessionClosed = true, want false")
			}
		})
	}
}

// The device table stores the access code, which is a working credential.
// -nosave is for a machine that is not yours, so it has to actually suppress
// the write rather than only the "Saved as" line.
func TestNoSaveRejectsAName(t *testing.T) {
	_, err := parseConnectOptions([]string{"-nosave", "-name", "laptop", "https://x/y#z"}, io.Discard)
	if err == nil {
		t.Fatal("-nosave with -name was accepted; they ask for opposite things")
	}
	if !strings.Contains(err.Error(), "-nosave") {
		t.Errorf("error = %q, want it to name the flag", err)
	}
}

func TestNoSaveParses(t *testing.T) {
	opts, err := parseConnectOptions([]string{"-nosave", "https://x/y#z"}, io.Discard)
	if err != nil {
		t.Fatalf("parseConnectOptions: %v", err)
	}
	if !opts.nosave {
		t.Error("-nosave did not set the flag")
	}
	if opts.name != "" {
		t.Errorf("name = %q, want empty", opts.name)
	}
}

// -relay and -norelay are the other pair that cancel out.
func TestRelayAndNoRelayAreRefused(t *testing.T) {
	if _, err := parseConnectOptions([]string{"-relay", "-norelay", "https://x/y#z"}, io.Discard); err == nil {
		t.Fatal("-relay with -norelay was accepted")
	}
}
