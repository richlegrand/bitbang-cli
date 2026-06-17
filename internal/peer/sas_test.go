package peer

import (
	"strings"
	"testing"
)

// TestComputeSAS_Symmetric ensures both peers compute the same SAS regardless
// of which side passes its fingerprint as "local" vs "remote". This is the
// core invariant — the sort step exists for exactly this reason.
func TestComputeSAS_Symmetric(t *testing.T) {
	fpA := "AA:BB:CC:DD:EE:FF:00:11:22:33:44:55:66:77:88:99:AA:BB:CC:DD:EE:FF:00:11:22:33:44:55:66:77:88:99"
	fpB := "11:22:33:44:55:66:77:88:99:AA:BB:CC:DD:EE:FF:00:11:22:33:44:55:66:77:88:99:AA:BB:CC:DD:EE:FF:00"

	sideA := ComputeSAS(fpA, fpB)
	sideB := ComputeSAS(fpB, fpA)

	if sideA != sideB {
		t.Fatalf("SAS asymmetric: sideA=%q sideB=%q (sort step is broken)", sideA, sideB)
	}
	if len(sideA) != 4 {
		t.Errorf("SAS length = %d, want 4 (zero-padded)", len(sideA))
	}
	for _, c := range sideA {
		if c < '0' || c > '9' {
			t.Errorf("SAS contains non-digit %q in %q", c, sideA)
		}
	}
}

// TestComputeSAS_CaseInsensitive verifies upper/lower fingerprint forms
// produce the same SAS — both pion and browsers emit uppercase, but we
// don't want a stray lowercase pasted into a test to silently diverge.
func TestComputeSAS_CaseInsensitive(t *testing.T) {
	fpA := "AA:BB:CC:DD:EE:FF:00:11:22:33:44:55:66:77:88:99:AA:BB:CC:DD:EE:FF:00:11:22:33:44:55:66:77:88:99"
	fpB := "11:22:33:44:55:66:77:88:99:AA:BB:CC:DD:EE:FF:00:11:22:33:44:55:66:77:88:99:AA:BB:CC:DD:EE:FF:00"

	upper := ComputeSAS(fpA, fpB)
	mixed := ComputeSAS(strings.ToLower(fpA), fpB)
	if upper != mixed {
		t.Errorf("SAS case-sensitive: upper=%q mixed=%q", upper, mixed)
	}
}

// TestComputeSAS_Differs verifies that distinct fingerprint pairs produce
// distinct SAS values most of the time. This is the property that catches
// MITM: a rogue relay creates two different fingerprint sets, and the SAS
// values on each side diverge.
func TestComputeSAS_Differs(t *testing.T) {
	fpA := "AA:BB:CC:DD:EE:FF:00:11:22:33:44:55:66:77:88:99:AA:BB:CC:DD:EE:FF:00:11:22:33:44:55:66:77:88:99"
	fpB := "11:22:33:44:55:66:77:88:99:AA:BB:CC:DD:EE:FF:00:11:22:33:44:55:66:77:88:99:AA:BB:CC:DD:EE:FF:00"
	fpC := "DE:AD:BE:EF:CA:FE:BA:BE:DE:AD:BE:EF:CA:FE:BA:BE:DE:AD:BE:EF:CA:FE:BA:BE:DE:AD:BE:EF:CA:FE:BA:BE"

	honest := ComputeSAS(fpA, fpB)
	mitm := ComputeSAS(fpA, fpC) // listener-side: A sees rogue (C) instead of B
	if honest == mitm {
		// Random 4-digit collision is 1/10,000; flaky once in a blue moon.
		// Real production use would catch the divergence via the human
		// compare. Reporting helps diagnose if this ever does fire.
		t.Errorf("honest SAS (%s) == MITM SAS (%s) — 1/10000 coincidence", honest, mitm)
	}
}
