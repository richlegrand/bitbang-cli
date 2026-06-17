// Package pairing implements the listener-side of BitBang's code-exchange
// pairing flow. The connector reaches the listener via a short numeric "code A"
// (resolved by the signaling server); a Short Authentication String "code B"
// is then derived independently on both peers from the negotiated DTLS
// fingerprints, and the listener operator types it after hearing it verbally
// from the connector.
//
// The listener never displays code B. There is therefore nothing to blindly
// approve — the prompt is unanswerable without coordinating with the
// connector. That is the property the design buys: failure mode is "no
// connection" (empty prompt, timeout) rather than "wrong connection"
// (autopilot y/n).
package pairing

import (
	"errors"
	"fmt"
	"strings"
)

// MaxSASAttempts is how many times the operator may try to enter the SAS
// before BitBang gives up and rejects the pairing. The SAS is only 4 digits
// — 10 000 possible values — so unbounded retries would brute-force the
// verify in seconds. Three honest attempts is plenty of headroom for fat
// fingers without changing the order of magnitude.
const MaxSASAttempts = 3

// PromptStatus is the outcome reported by a PromptFunc for one attempt.
type PromptStatus string

const (
	// PromptOK means the operator entered a value (which may or may not
	// match — PromptForSAS does the compare).
	PromptOK PromptStatus = "ok"

	// PromptAbort means the operator cancelled (Ctrl-C, EOF, "no" click,
	// closed the modal, etc.). Treated as user_declined.
	PromptAbort PromptStatus = "abort"

	// PromptTimeout means the outer prompt deadline elapsed without input.
	// Treated as timeout.
	PromptTimeout PromptStatus = "timeout"
)

// PromptFunc reads one SAS attempt from the operator. attempt is 1-indexed
// up to MaxSASAttempts. Implementations return (typed, PromptOK) on success,
// ("", PromptAbort) on cancellation, or ("", PromptTimeout) on deadline.
type PromptFunc func(attempt int) (typed string, status PromptStatus)

// ErrAbort and ErrTimeout are convenience sentinel errors callers can wrap
// around the rejection reason — most callers just propagate the string
// reason from PromptForSAS verbatim, but having explicit errors keeps the
// internal API typeable.
var (
	ErrSASMismatch  = errors.New("sas_mismatch")
	ErrUserDeclined = errors.New("user_declined")
	ErrTimeout      = errors.New("timeout")
)

// PromptForSAS asks the listener operator to type the 4-digit SAS that the
// connector is reading aloud. expected is the SAS BitBang independently
// computed from the negotiated DTLS fingerprints; it is never written to the
// terminal — the whole point is that the operator must hear it from the
// connector.
//
// Returns ("", true) on a typed value matching expected within MaxSASAttempts
// retries. Returns (reason, false) otherwise: "sas_mismatch" on exhausted
// retries, "timeout" or "user_declined" propagated from the prompt.
//
// The reason string is wire-stable and is intended to be sent verbatim as
// the reason field of a pair_rejected message.
func PromptForSAS(expected string, prompt PromptFunc) (reason string, ok bool) {
	fmt.Println()
	fmt.Println("Incoming pair request.")
	fmt.Println("Ask the other party to read the 4-digit code shown on their screen.")

	for attempt := 1; attempt <= MaxSASAttempts; attempt++ {
		typed, status := prompt(attempt)
		switch status {
		case PromptAbort:
			return string(ErrUserDeclined.Error()), false
		case PromptTimeout:
			return string(ErrTimeout.Error()), false
		}
		if strings.TrimSpace(typed) == expected {
			return "", true
		}
		if attempt < MaxSASAttempts {
			fmt.Println("Code did not match. Try again.")
		}
	}
	return string(ErrSASMismatch.Error()), false
}

// DefaultTTYPrompt reads one SAS attempt from stdin. Use this as the
// PromptFunc when running interactively. There is no per-attempt timeout —
// a TTY operator is assumed present; the outer pair_request timeout is the
// only deadline.
//
// Returns PromptAbort if Scanln fails for any reason (EOF on stdin, Ctrl-D,
// piped input ending, etc.). Treating any input failure as abort is correct
// because the only safe response to "I can't read input" is "don't approve."
func DefaultTTYPrompt(attempt int) (string, PromptStatus) {
	fmt.Printf("Enter code (attempt %d/%d): ", attempt, MaxSASAttempts)
	var typed string
	if _, err := fmt.Scanln(&typed); err != nil {
		return "", PromptAbort
	}
	return typed, PromptOK
}
