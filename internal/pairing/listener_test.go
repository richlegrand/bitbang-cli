package pairing

import "testing"

// scriptedPrompt returns a PromptFunc that yields successive (typed, status)
// pairs from the slice. Used to drive PromptForSAS without a real TTY.
func scriptedPrompt(steps []struct {
	typed  string
	status PromptStatus
}) PromptFunc {
	idx := 0
	return func(attempt int) (string, PromptStatus) {
		if idx >= len(steps) {
			return "", PromptAbort
		}
		s := steps[idx]
		idx++
		return s.typed, s.status
	}
}

func TestPromptForSAS_FirstTryMatches(t *testing.T) {
	reason, ok := PromptForSAS("1234", scriptedPrompt([]struct {
		typed  string
		status PromptStatus
	}{
		{"1234", PromptOK},
	}))
	if !ok {
		t.Fatalf("expected ok=true, got reason=%q", reason)
	}
	if reason != "" {
		t.Errorf("expected empty reason on success, got %q", reason)
	}
}

func TestPromptForSAS_RetryThenMatch(t *testing.T) {
	reason, ok := PromptForSAS("1234", scriptedPrompt([]struct {
		typed  string
		status PromptStatus
	}{
		{"1233", PromptOK},
		{"1234", PromptOK},
	}))
	if !ok {
		t.Fatalf("expected ok after retry, got reason=%q", reason)
	}
	if reason != "" {
		t.Errorf("expected empty reason on success, got %q", reason)
	}
}

func TestPromptForSAS_ExhaustedRetries(t *testing.T) {
	reason, ok := PromptForSAS("1234", scriptedPrompt([]struct {
		typed  string
		status PromptStatus
	}{
		{"1111", PromptOK},
		{"2222", PromptOK},
		{"3333", PromptOK},
	}))
	if ok {
		t.Fatalf("expected ok=false after exhausting attempts")
	}
	if reason != "sas_mismatch" {
		t.Errorf("expected reason sas_mismatch, got %q", reason)
	}
}

func TestPromptForSAS_AbortFirstAttempt(t *testing.T) {
	reason, ok := PromptForSAS("1234", scriptedPrompt([]struct {
		typed  string
		status PromptStatus
	}{
		{"", PromptAbort},
	}))
	if ok {
		t.Fatalf("expected ok=false on abort")
	}
	if reason != "user_declined" {
		t.Errorf("expected reason user_declined, got %q", reason)
	}
}

func TestPromptForSAS_TimeoutMidAttempt(t *testing.T) {
	reason, ok := PromptForSAS("1234", scriptedPrompt([]struct {
		typed  string
		status PromptStatus
	}{
		{"1111", PromptOK},
		{"", PromptTimeout},
	}))
	if ok {
		t.Fatalf("expected ok=false on timeout")
	}
	if reason != "timeout" {
		t.Errorf("expected reason timeout, got %q", reason)
	}
}

// TestPromptForSAS_WhitespaceTolerated ensures that a stray leading/trailing
// space from clumsy Scanln behavior doesn't bounce a correct entry. The
// digits themselves must still match exactly.
func TestPromptForSAS_WhitespaceTolerated(t *testing.T) {
	reason, ok := PromptForSAS("1234", scriptedPrompt([]struct {
		typed  string
		status PromptStatus
	}{
		{"  1234  ", PromptOK},
	}))
	if !ok {
		t.Fatalf("expected ok=true with whitespace, got reason=%q", reason)
	}
}
