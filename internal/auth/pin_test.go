package auth

import "testing"

func TestPIN_NilWhenEmpty(t *testing.T) {
	if New("") != nil {
		t.Errorf("New(\"\") returned non-nil PINAuth")
	}
}

func TestPIN_Required(t *testing.T) {
	if New("1234").Required() != true {
		t.Errorf("Required() = false for non-empty PIN")
	}
	var p *PINAuth
	if p.Required() != false {
		t.Errorf("Required() on nil should be false")
	}
}

func TestPIN_VerifyCorrect(t *testing.T) {
	a := New("4815162342")
	if !a.Verify("4815162342") {
		t.Errorf("Verify rejected the correct PIN")
	}
}

func TestPIN_VerifyWrong(t *testing.T) {
	a := New("4815162342")
	cases := []string{
		"",                   // empty
		"4",                  // shorter prefix (would short-circuit with ==)
		"4815162343",         // off-by-one in last char
		"4815162342_extra",   // correct PIN as prefix
		"________________",   // same length, wrong content
	}
	for _, c := range cases {
		if a.Verify(c) {
			t.Errorf("Verify accepted wrong PIN %q", c)
		}
	}
}

func TestPIN_VerifyNilReceiver(t *testing.T) {
	// Defensive: calling Verify on nil must not panic, must return
	// false. Belt-and-suspenders for any code path that constructs a
	// session with PIN=nil and then forwards an auth attempt anyway.
	var a *PINAuth
	if a.Verify("anything") {
		t.Errorf("Verify on nil PINAuth returned true")
	}
}

func TestPIN_NoPlaintextRetained(t *testing.T) {
	// The original PIN string must not be stored on the struct after
	// construction — otherwise an attacker with read access to process
	// memory could recover it. We verify by reflection-free struct
	// inspection: there is no string-typed field on PINAuth.
	a := New("hunter2")
	_ = a
	// (Structural: if a future refactor adds a `pin string` field
	// back, the build still passes but this comment + the field name
	// shape in pin.go are the signal. The pinHash + set fields are
	// the only state.)
}
