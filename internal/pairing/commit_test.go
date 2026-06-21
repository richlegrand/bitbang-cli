package pairing

import (
	"strings"
	"testing"
)

const (
	fpA = "AA:BB:CC:DD:EE:FF:00:11:22:33:44:55:66:77:88:99:AA:BB:CC:DD:EE:FF:00:11:22:33:44:55:66:77:88:99"
	fpB = "11:22:33:44:55:66:77:88:99:AA:BB:CC:DD:EE:FF:00:11:22:33:44:55:66:77:88:99:AA:BB:CC:DD:EE:FF:00"
)

func mustNonce(t *testing.T) []byte {
	t.Helper()
	n, err := NewNonce()
	if err != nil {
		t.Fatal(err)
	}
	if len(n) != NonceLen {
		t.Fatalf("nonce len = %d, want %d", len(n), NonceLen)
	}
	return n
}

// Both sides must compute the same SAS from the same nonces + fingerprints,
// regardless of which fingerprint each calls "local" vs "remote".
func TestComputeSAS_Symmetric(t *testing.T) {
	rc, rd := mustNonce(t), mustNonce(t)
	connector := ComputeSAS(rc, rd, fpA, fpB) // connector: local=A, remote=B
	listener := ComputeSAS(rc, rd, fpB, fpA)  // listener: local=B, remote=A
	if connector != listener {
		t.Fatalf("SAS not symmetric: %q vs %q", connector, listener)
	}
	if len(connector) != 6 {
		t.Fatalf("SAS = %q, want 6 digits", connector)
	}
	for _, c := range connector {
		if c < '0' || c > '9' {
			t.Fatalf("SAS = %q, want all digits", connector)
		}
	}
}

// Any change to a nonce or a fingerprint must change the SAS — this is what
// makes a relay's substituted fingerprints (and any nonce tampering) detectable.
func TestComputeSAS_InputsMatter(t *testing.T) {
	rc, rd := mustNonce(t), mustNonce(t)
	base := ComputeSAS(rc, rd, fpA, fpB)

	rc2 := mustNonce(t)
	if ComputeSAS(rc2, rd, fpA, fpB) == base {
		// 1-in-10^6 false collision; astronomically unlikely with random nonces.
		t.Error("changing rc did not change SAS")
	}
	rd2 := mustNonce(t)
	if ComputeSAS(rc, rd2, fpA, fpB) == base {
		t.Error("changing rd did not change SAS")
	}
	fpC := strings.ReplaceAll(fpA, "AA", "BB")
	if ComputeSAS(rc, rd, fpC, fpB) == base {
		t.Error("changing a fingerprint did not change SAS")
	}
}

func TestComputeSAS_CaseInsensitiveFingerprints(t *testing.T) {
	rc, rd := mustNonce(t), mustNonce(t)
	upper := ComputeSAS(rc, rd, fpA, fpB)
	lower := ComputeSAS(rc, rd, strings.ToLower(fpA), strings.ToLower(fpB))
	if upper != lower {
		t.Fatalf("SAS case-sensitive: %q vs %q", upper, lower)
	}
}

func TestCommitment_RoundTrip(t *testing.T) {
	rc := mustNonce(t)
	commit := Commitment(rc)
	if !VerifyCommitment(commit, rc) {
		t.Fatal("VerifyCommitment rejected the matching nonce")
	}
	if VerifyCommitment(commit, mustNonce(t)) {
		t.Fatal("VerifyCommitment accepted a different nonce")
	}
	if VerifyCommitment("not-base64-or-wrong", rc) {
		t.Fatal("VerifyCommitment accepted a wrong commitment")
	}
}

// Full message round-trip: build → parse for each data-channel message.
func TestPairMessages_RoundTrip(t *testing.T) {
	rc, rd := mustNonce(t), mustNonce(t)

	commit := Commitment(rc)
	if m := BuildPairCommit(commit); PairMessageType(m) != MsgPairCommit {
		t.Fatalf("commit type = %q", PairMessageType(m))
	} else if got, ok := ParsePairCommit(m); !ok || got != commit {
		t.Fatalf("ParsePairCommit = %q,%v", got, ok)
	}

	if m := BuildPairChallenge(rd); PairMessageType(m) != MsgPairChallenge {
		t.Fatalf("challenge type = %q", PairMessageType(m))
	} else if got, ok := ParsePairChallenge(m); !ok || string(got) != string(rd) {
		t.Fatalf("ParsePairChallenge mismatch ok=%v", ok)
	}

	if m := BuildPairReveal(rc); PairMessageType(m) != MsgPairReveal {
		t.Fatalf("reveal type = %q", PairMessageType(m))
	} else if got, ok := ParsePairReveal(m); !ok || string(got) != string(rc) {
		t.Fatalf("ParsePairReveal mismatch ok=%v", ok)
	}

	m := BuildPairCredentials("uid-x", "pubkey-b64", "code-y")
	if PairMessageType(m) != MsgPairCredentials {
		t.Fatalf("creds type = %q", PairMessageType(m))
	}
	uid, pk, ac, ok := ParsePairCredentials(m)
	if !ok || uid != "uid-x" || pk != "pubkey-b64" || ac != "code-y" {
		t.Fatalf("ParsePairCredentials = %q,%q,%q,%v", uid, pk, ac, ok)
	}
}

// A short / malformed nonce must be rejected, not silently truncated.
func TestParseChallenge_RejectsBadNonce(t *testing.T) {
	if _, ok := ParsePairChallenge([]byte(`{"nonce_d":"AAAA"}`)); ok {
		t.Fatal("accepted a too-short nonce")
	}
	if _, ok := ParsePairChallenge([]byte(`not json`)); ok {
		t.Fatal("accepted non-JSON")
	}
}
