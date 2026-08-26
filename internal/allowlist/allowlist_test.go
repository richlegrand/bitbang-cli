package allowlist

import "testing"

func TestEmptyListPermitsEverything(t *testing.T) {
	var l List
	if !l.Permits("10.0.0.1", 22) {
		t.Error("the zero value must allow everything, so an unrestricted listener keeps working")
	}
}

func TestExactAndAnyPort(t *testing.T) {
	l, err := Parse([]string{"127.0.0.1:22", "nas.lan"})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	cases := []struct {
		host string
		port int
		want bool
	}{
		{"127.0.0.1", 22, true},
		{"127.0.0.1", 23, false}, // same host, wrong port
		{"nas.lan", 22, true},    // portless entry allows any port
		{"nas.lan", 5432, true},
		{"other.lan", 22, false},
	}
	for _, c := range cases {
		if got := l.Permits(c.host, c.port); got != c.want {
			t.Errorf("Permits(%q, %d) = %v, want %v", c.host, c.port, got, c.want)
		}
	}
}

// The same host written differently must compare equal, or an allowlist is
// trivially sidestepped by changing the spelling.
func TestHostNormalization(t *testing.T) {
	l, _ := Parse([]string{"NAS.Lan:22", "[::1]:22"})
	for _, host := range []string{"nas.lan", "NAS.LAN", "nas.lan."} {
		if !l.Permits(host, 22) {
			t.Errorf("Permits(%q, 22) = false, want true", host)
		}
	}
	for _, host := range []string{"::1", "[::1]"} {
		if !l.Permits(host, 22) {
			t.Errorf("Permits(%q, 22) = false, want true", host)
		}
	}
}

// Names are matched as written and never resolved: localhost and 127.0.0.1
// are the same machine but not the same string, and resolving at check time
// would let the answer change before the dial.
func TestNamesAreNotResolved(t *testing.T) {
	l, _ := Parse([]string{"127.0.0.1:22"})
	if l.Permits("localhost", 22) {
		t.Error("localhost matched an allowlist of 127.0.0.1; entries must not be resolved")
	}
}

func TestPortlessRequestNeedsPortlessEntry(t *testing.T) {
	l, _ := Parse([]string{"nas.lan:8080"})
	if l.PermitsTarget("nas.lan") {
		t.Error("a request naming no port matched an entry that names one")
	}
	l2, _ := Parse([]string{"nas.lan"})
	if !l2.PermitsTarget("nas.lan") {
		t.Error("a request naming no port must match a portless entry")
	}
	if !l2.PermitsTarget("nas.lan:8080") {
		t.Error("a portless entry must allow any port")
	}
}

func TestParseRejectsJunk(t *testing.T) {
	for _, spec := range []string{"", "host:0", "host:70000", "host:ssh", ":22", "::1:22"} {
		if _, err := Parse([]string{spec}); err == nil {
			t.Errorf("Parse(%q) accepted an invalid target", spec)
		}
	}
}

func TestStringRendersForMessages(t *testing.T) {
	l, _ := Parse([]string{"127.0.0.1:22", "nas.lan", "[fd00::20]:5900"})
	want := "127.0.0.1:22, nas.lan:*, [fd00::20]:5900"
	if got := l.String(); got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
}
