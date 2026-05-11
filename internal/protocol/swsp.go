// Package protocol implements SWSP (Simple WebRTC Streaming Protocol) for
// HTTP over WebRTC data channels.
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
	FlagSYN = 0x0001 // Start of stream, payload is JSON metadata
	FlagFIN = 0x0004 // End of stream
	FlagDAT = 0x0000 // Data chunk (no flags set)

	MaxChunkSize = 32768 // 32KB max payload per frame (frame stays under 64KB SCTP limit)
	HeaderSize   = 8

	// ProtocolVersion is sent in the register message. The signaling server
	// rejects devices below its minimum. Bump only for breaking wire changes.
	ProtocolVersion = 2
)

// Frame represents a single SWSP frame.
type Frame struct {
	StreamID uint32
	Flags    uint16
	Payload  []byte
}

// Request is the JSON metadata from a SYN frame (browser -> device).
type Request struct {
	Method        string `json:"method"`
	Pathname      string `json:"pathname"`
	ContentType   string            `json:"contentType,omitempty"`
	ContentLength int               `json:"contentLength,omitempty"`
	Headers       map[string]string `json:"headers,omitempty"`
}

// Response is the JSON metadata for a SYN frame (device -> browser).
type Response struct {
	Status  int               `json:"status"`
	Headers map[string]string `json:"headers"`
}

// ParseFrame parses a raw SWSP frame from bytes.
func ParseFrame(data []byte) (Frame, error) {
	if len(data) < HeaderSize {
		return Frame{}, fmt.Errorf("frame too short: %d bytes", len(data))
	}

	streamID := binary.LittleEndian.Uint32(data[0:4])
	flags := binary.LittleEndian.Uint16(data[4:6])
	length := binary.LittleEndian.Uint16(data[6:8])

	if len(data) < HeaderSize+int(length) {
		return Frame{}, fmt.Errorf("frame truncated: expected %d payload bytes, got %d", length, len(data)-HeaderSize)
	}

	payload := make([]byte, length)
	copy(payload, data[HeaderSize:HeaderSize+int(length)])

	return Frame{
		StreamID: streamID,
		Flags:    flags,
		Payload:  payload,
	}, nil
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

// ParseRequest parses the JSON payload of a SYN frame as a Request.
func (f Frame) ParseRequest() (Request, error) {
	var req Request
	if err := json.Unmarshal(f.Payload, &req); err != nil {
		return req, fmt.Errorf("parse request: %w", err)
	}
	return req, nil
}

// BuildResponseFrames creates the SWSP frames for an HTTP response:
// a SYN frame with status/headers, DAT frames for the body, and a FIN frame.
func BuildResponseFrames(streamID uint32, status int, headers map[string]string, body []byte) [][]byte {
	var frames [][]byte

	// SYN frame with response metadata
	resp := Response{Status: status, Headers: headers}
	respJSON, _ := json.Marshal(resp)
	frames = append(frames, BuildFrame(streamID, FlagSYN, respJSON))

	// DAT frames for body
	for i := 0; i < len(body); i += MaxChunkSize {
		end := i + MaxChunkSize
		if end > len(body) {
			end = len(body)
		}
		frames = append(frames, BuildFrame(streamID, FlagDAT, body[i:end]))
	}

	// FIN frame
	frames = append(frames, BuildFrame(streamID, FlagFIN, nil))

	return frames
}
