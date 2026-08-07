// Package protocol implements SWSP (Simple WebRTC Streaming Protocol) over
// WebRTC data channels.
//
// Frame format (8-byte header + payload):
//
//	+-----------+-----------+-----------+-----------+
//	| StreamID  | Flags     | Length    | Payload   |
//	| 4 bytes   | 2 bytes   | 2 bytes   | variable  |
//	| (LE)      | (LE)      | (LE)      |           |
//	+-----------+-----------+-----------+-----------+
package protocol

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
)

const (
	FlagSYN  = 0x0001 // Start of stream, payload is JSON metadata
	FlagFIN  = 0x0004 // End of stream
	FlagDAT  = 0x0000 // Data chunk (no flags set)
	FlagMORE = 0x0002 // Non-final fragment of a chunked WebSocket message

	MaxChunkSize = 32768 // 32KB max payload per frame (frame stays under 64KB SCTP limit)
	HeaderSize   = 8

	// ProtocolVersion is the registration-protocol version sent to the
	// signaling server on device register. Independent of SWSPVersion
	// below (which is the data-channel wire protocol).
	//
	// v3: split-identity URLs (128-bit UID + 64-bit access code in the URL
	// fragment, both base64url-encoded), browser includes the code in the
	// encrypted_request payload, signaling server forwards browser_ip on
	// connection requests.
	ProtocolVersion = 3

	// SWSPVersion is the data-channel wire protocol version. Sent in
	// stream-0 `connect` (client → device) and `ready` (device → client).
	// v3 adds typed SYN payloads and capability negotiation while keeping
	// the byte-level frame format unchanged from v2.
	// v4 adds negotiated per-stream flow control and stream-local resets.
	SWSPVersion = 4

	// Flow-control defaults are deliberately per stream. A v4 SYN implicitly
	// grants the initial window in both directions; later window updates raise
	// that cumulative limit. Keeping the update threshold below the window lets
	// a sender continue while the next grant is in flight. The frame limit
	// separately bounds queue work for tiny or empty frames that consume little
	// or no byte credit.
	InitialStreamWindow         = 1 << 20
	StreamWindowUpdateThreshold = InitialStreamWindow / 2
	MaxQueuedStreamFrames       = 256

	ControlWindowUpdate = "window_update"
	ControlStreamReset  = "stream_reset"
)

// WindowUpdate raises the cumulative number of payload bytes the peer may
// send in one direction of a stream. Updates are monotonic, which makes stale
// or duplicate controls harmless.
type WindowUpdate struct {
	Type     string `json:"type"`
	StreamID uint32 `json:"stream_id"`
	MaxBytes uint64 `json:"max_bytes"`
}

// StreamReset terminates both directions of one stream without affecting the
// other streams multiplexed over the data channel.
type StreamReset struct {
	Type     string `json:"type"`
	StreamID uint32 `json:"stream_id"`
	Code     string `json:"code,omitempty"`
	Message  string `json:"message,omitempty"`
}

// Frame represents a single SWSP frame.
type Frame struct {
	StreamID uint32
	Flags    uint16
	Payload  []byte
}

// Request is the JSON metadata for an HTTP-stream SYN frame. The optional
// `Type` field is SWSP v3 — it's "http" for new clients; v2 clients omit
// it and the listener treats missing-type as "http" by default.
type Request struct {
	Type          string            `json:"type,omitempty"`
	Method        string            `json:"method"`
	Pathname      string            `json:"pathname"`
	ContentType   string            `json:"contentType,omitempty"`
	ContentLength int               `json:"contentLength,omitempty"`
	Headers       map[string]string `json:"headers,omitempty"`
}

// Response is the JSON metadata for an HTTP-stream response SYN frame.
type Response struct {
	Status  int               `json:"status"`
	Headers map[string]string `json:"headers"`
}

// FileOp is the JSON metadata for a file-stream SYN frame (SWSP v3).
// `Op` is one of "get", "put", "list", "stat", "delete".
type FileOp struct {
	Type      string  `json:"type"`
	Op        string  `json:"op"`
	Path      string  `json:"path"`
	Size      int64   `json:"size,omitempty"`      // for put: total bytes the client will send
	Overwrite bool    `json:"overwrite,omitempty"` // for put
	Range     []int64 `json:"range,omitempty"`     // for get: [start, end] inclusive byte range
}

// TCPOpen is the JSON metadata for a raw TCP-stream SYN frame (SWSP v3).
type TCPOpen struct {
	Type string `json:"type"`
	Host string `json:"host"`
	Port int    `json:"port"`
}

// ParseFrame parses a raw SWSP frame from bytes.
func ParseFrame(data []byte) (Frame, error) {
	if len(data) < HeaderSize {
		return Frame{}, fmt.Errorf("frame too short: %d bytes", len(data))
	}

	streamID := binary.LittleEndian.Uint32(data[0:4])
	flags := binary.LittleEndian.Uint16(data[4:6])
	length := binary.LittleEndian.Uint16(data[6:8])

	if len(data) != HeaderSize+int(length) {
		return Frame{}, fmt.Errorf("frame length mismatch: expected %d payload bytes, got %d", length, len(data)-HeaderSize)
	}

	payload := make([]byte, length)
	copy(payload, data[HeaderSize:HeaderSize+int(length)])

	return Frame{
		StreamID: streamID,
		Flags:    flags,
		Payload:  payload,
	}, nil
}

// ValidateFramePayload rejects payloads that cannot be represented as one
// bounded SWSP data-channel message. Callers must split larger byte streams
// using their stream type's framing rules.
func ValidateFramePayload(payload []byte) error {
	if len(payload) > MaxChunkSize {
		return fmt.Errorf("frame payload too large: %d bytes (max %d)", len(payload), MaxChunkSize)
	}
	return nil
}

// BuildFrame creates raw bytes for an SWSP frame.
func BuildFrame(streamID uint32, flags uint16, payload []byte) []byte {
	buf := make([]byte, HeaderSize+len(payload))
	binary.LittleEndian.PutUint32(buf[0:4], streamID)
	binary.LittleEndian.PutUint16(buf[4:6], flags)
	binary.LittleEndian.PutUint16(buf[6:8], uint16(len(payload)))
	copy(buf[HeaderSize:], payload)
	return buf
}

// IsSYN returns true if the SYN flag is set.
func (f Frame) IsSYN() bool { return f.Flags&FlagSYN != 0 }

// IsFIN returns true if the FIN flag is set.
func (f Frame) IsFIN() bool { return f.Flags&FlagFIN != 0 }

// IsMORE returns true if the MORE flag is set, i.e. this DAT frame is a
// non-final fragment of a chunked WebSocket message.
func (f Frame) IsMORE() bool { return f.Flags&FlagMORE != 0 }

// FlowBytes returns the number of bytes charged against a v4 receive window.
// SYN metadata and empty FIN frames must remain sendable without credit so
// stream setup and teardown cannot deadlock.
func (f Frame) FlowBytes() uint64 {
	if f.IsSYN() {
		return 0
	}
	return uint64(len(f.Payload))
}

// NegotiateSWSPVersion selects the highest wire version supported by both
// peers. A missing version is the legacy v2 wire format.
func NegotiateSWSPVersion(peerVersion int) int {
	if peerVersion <= 0 {
		return 2
	}
	if peerVersion > SWSPVersion {
		return SWSPVersion
	}
	return peerVersion
}

// ParseRequest parses the JSON payload of a SYN frame as a Request.
func (f Frame) ParseRequest() (Request, error) {
	return ParseRequest(f.Payload)
}

// ParseRequest unmarshals a SYN payload as an HTTP-stream Request. Useful
// when the caller has the payload directly (e.g. from a StreamHandler's
// OnSYN callback) without a Frame wrapper.
func ParseRequest(payload []byte) (Request, error) {
	var req Request
	if err := json.Unmarshal(payload, &req); err != nil {
		return req, fmt.Errorf("parse request: %w", err)
	}
	return req, nil
}
