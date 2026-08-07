package signaling

import (
	"reflect"
	"testing"
)

func TestParseFragment(t *testing.T) {
	tests := []struct {
		fragment  string
		wantCode  string
		wantFlags []string
	}{
		{"ABC123", "ABC123", nil},
		{"ABC123!ephemeral", "ABC123", []string{"ephemeral"}},
		{"ABC123!ephemeral,debug", "ABC123", []string{"ephemeral", "debug"}},
		{"ABC123!msg_timeout=5,ephemeral", "ABC123", []string{"msg_timeout=5", "ephemeral"}},
		{"ABC123!ephemeral,debug/https://nas.local", "ABC123", []string{"ephemeral", "debug"}},
		{"ABC123/https://nas.local", "ABC123", nil},
		{"ABC123!,,", "ABC123", nil},
		{"", "", nil},
	}
	for _, tc := range tests {
		code, flags := ParseFragment(tc.fragment)
		if code != tc.wantCode || !reflect.DeepEqual(flags, tc.wantFlags) {
			t.Errorf("ParseFragment(%q) = (%q, %v), want (%q, %v)", tc.fragment, code, flags, tc.wantCode, tc.wantFlags)
		}
	}
}

func TestHasFlagIgnoresValue(t *testing.T) {
	flags := []string{"debug", "msg_timeout=5", "ephemeral"}
	if !HasFlag(flags, "msg_timeout") || !HasFlag(flags, "ephemeral") || HasFlag(flags, "relay") {
		t.Fatalf("HasFlag returned unexpected results for %v", flags)
	}
}
