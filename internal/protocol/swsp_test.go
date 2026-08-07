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

func TestParseFrameRejectsOversizedAndMismatchedPayloads(t *testing.T) {
	// Accept a legacy peer's valid 16-bit frame even though new senders use
	// the smaller MaxChunkSize bound.
	legacy := BuildFrame(1, FlagDAT, make([]byte, MaxChunkSize+1))
	if _, err := ParseFrame(legacy); err != nil {
		t.Fatalf("ParseFrame rejected a legacy-sized payload: %v", err)
	}

	valid := BuildFrame(1, FlagDAT, []byte("ok"))
	if _, err := ParseFrame(append(valid, 0)); err == nil {
		t.Fatal("ParseFrame accepted trailing bytes")
	}
	if _, err := ParseFrame(valid[:len(valid)-1]); err == nil {
		t.Fatal("ParseFrame accepted a truncated payload")
	}
	if err := ValidateFramePayload(make([]byte, MaxChunkSize+1)); err == nil {
		t.Fatal("ValidateFramePayload accepted an oversized payload")
	}
}

func TestFlowBytes(t *testing.T) {
	tests := []struct {
		name  string
		frame Frame
		want  uint64
	}{
		{name: "syn", frame: Frame{Flags: FlagSYN, Payload: []byte("metadata")}},
		{name: "syn fin", frame: Frame{Flags: FlagSYN | FlagFIN, Payload: []byte("metadata")}},
		{name: "dat", frame: Frame{Flags: FlagDAT, Payload: []byte("data")}, want: 4},
		{name: "more", frame: Frame{Flags: FlagMORE, Payload: []byte("part")}, want: 4},
		{name: "fin payload", frame: Frame{Flags: FlagFIN, Payload: []byte("tail")}, want: 4},
		{name: "empty fin", frame: Frame{Flags: FlagFIN}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.frame.FlowBytes(); got != tt.want {
				t.Fatalf("FlowBytes() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestNegotiateSWSPVersion(t *testing.T) {
	tests := []struct {
		peer int
		want int
	}{
		{peer: -1, want: 2},
		{peer: 0, want: 2},
		{peer: 2, want: 2},
		{peer: 3, want: 3},
		{peer: 4, want: 4},
		{peer: 99, want: SWSPVersion},
	}

	for _, tt := range tests {
		if got := NegotiateSWSPVersion(tt.peer); got != tt.want {
			t.Errorf("NegotiateSWSPVersion(%d) = %d, want %d", tt.peer, got, tt.want)
		}
	}
}
