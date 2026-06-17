package sdp

import "testing"

// sampleSDP is a fragment representative of what pion and browsers emit.
// Only the a=fingerprint line matters for ExtractFingerprint; the rest
// is here to make sure the parser doesn't trip on surrounding content.
const sampleSDP = `v=0
o=- 1 2 IN IP4 127.0.0.1
s=-
t=0 0
m=application 9 UDP/DTLS/SCTP webrtc-datachannel
a=fingerprint:sha-256 AB:CD:EF:01:23:45:67:89:00:11:22:33:44:55:66:77:88:99:AA:BB:CC:DD:EE:FF:00:11:22:33:44:55:66:77
a=setup:actpass
a=mid:0
`

func TestExtractFingerprint(t *testing.T) {
	got := ExtractFingerprint(sampleSDP)
	want := "AB:CD:EF:01:23:45:67:89:00:11:22:33:44:55:66:77:88:99:AA:BB:CC:DD:EE:FF:00:11:22:33:44:55:66:77"
	if got != want {
		t.Fatalf("ExtractFingerprint = %q, want %q", got, want)
	}
}

func TestExtractFingerprint_NoFingerprint(t *testing.T) {
	if got := ExtractFingerprint("v=0\nm=application 9 UDP/DTLS/SCTP webrtc-datachannel\n"); got != "" {
		t.Errorf("ExtractFingerprint = %q, want \"\"", got)
	}
}

// TestExtractFingerprint_Lowercase ensures we normalize uppercase even
// when an SDP source emits lowercase hex. Pion and browsers both emit
// uppercase, but third-party SDP munging could in principle change this
// — the bidirectional-verify protocol relies on the normalized form.
func TestExtractFingerprint_Lowercase(t *testing.T) {
	sdp := "a=fingerprint:sha-256 ab:cd:ef:01:23:45\n"
	got := ExtractFingerprint(sdp)
	want := "AB:CD:EF:01:23:45"
	if got != want {
		t.Errorf("ExtractFingerprint = %q, want %q (normalization broken)", got, want)
	}
}

// TestExtractFingerprint_CRLF guards against the case where an SDP uses
// CR/LF line endings (the spec allows either). Without the TrimRight,
// the regex's `\s*$` could absorb the CR but the captured group would
// include it.
func TestExtractFingerprint_CRLF(t *testing.T) {
	sdp := "a=fingerprint:sha-256 AB:CD:EF:01:23:45\r\n"
	got := ExtractFingerprint(sdp)
	want := "AB:CD:EF:01:23:45"
	if got != want {
		t.Errorf("ExtractFingerprint = %q, want %q (CRLF handling broken)", got, want)
	}
}

func TestFingerprintsEqual(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"AB:CD", "AB:CD", true},
		{"AB:CD", "ab:cd", true},
		{"AB:CD", "AB:CE", false},
		{"", "", true},
	}
	for _, c := range cases {
		if got := FingerprintsEqual(c.a, c.b); got != c.want {
			t.Errorf("FingerprintsEqual(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}
