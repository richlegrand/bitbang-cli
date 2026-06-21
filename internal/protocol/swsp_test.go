package protocol

import (
	"testing"
)

func TestBuildAndParseFrame(t *testing.T) {
	payload := []byte("hello world")
	raw := BuildFrame(42, FlagSYN, payload)

	frame, err := ParseFrame(raw)
	if err != nil {
		t.Fatalf("ParseFrame: %v", err)
	}

	if frame.StreamID != 42 {
		t.Errorf("StreamID = %d, want 42", frame.StreamID)
	}
	if frame.Flags != FlagSYN {
		t.Errorf("Flags = %d, want %d", frame.Flags, FlagSYN)
	}
	if string(frame.Payload) != "hello world" {
		t.Errorf("Payload = %q, want %q", frame.Payload, "hello world")
	}
}

func TestBuildAndParseEmptyPayload(t *testing.T) {
	raw := BuildFrame(1, FlagFIN, nil)

	frame, err := ParseFrame(raw)
	if err != nil {
		t.Fatalf("ParseFrame: %v", err)
	}

	if frame.StreamID != 1 {
		t.Errorf("StreamID = %d, want 1", frame.StreamID)
	}
	if !frame.IsFIN() {
		t.Error("expected FIN flag")
	}
	if len(frame.Payload) != 0 {
		t.Errorf("Payload length = %d, want 0", len(frame.Payload))
	}
}

func TestSYNFINFrame(t *testing.T) {
	raw := BuildFrame(5, FlagSYN|FlagFIN, []byte("{}"))

	frame, err := ParseFrame(raw)
	if err != nil {
		t.Fatalf("ParseFrame: %v", err)
	}

	if !frame.IsSYN() {
		t.Error("expected SYN flag")
	}
	if !frame.IsFIN() {
		t.Error("expected FIN flag")
	}
}

func TestParseRequest(t *testing.T) {
	payload := []byte(`{"method":"GET","pathname":"/api/status"}`)
	raw := BuildFrame(1, FlagSYN|FlagFIN, payload)

	frame, err := ParseFrame(raw)
	if err != nil {
		t.Fatalf("ParseFrame: %v", err)
	}

	req, err := frame.ParseRequest()
	if err != nil {
		t.Fatalf("ParseRequest: %v", err)
	}

	if req.Method != "GET" {
		t.Errorf("Method = %q, want %q", req.Method, "GET")
	}
	if req.Pathname != "/api/status" {
		t.Errorf("Pathname = %q, want %q", req.Pathname, "/api/status")
	}
}

func TestParseFrameTooShort(t *testing.T) {
	_, err := ParseFrame([]byte{1, 2, 3})
	if err == nil {
		t.Error("expected error for short frame")
	}
}
