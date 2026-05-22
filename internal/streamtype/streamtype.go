// Package streamtype defines the contract for handling individual stream
// types (http, websocket, file, shell, etc.) over a SWSP data channel.
//
// One StreamHandler instance is created per Session per type. The handler
// owns its own per-stream state. The Session dispatches frames to the
// handler based on the `type` field in the SYN payload.
package streamtype

// Stream is the per-stream context handed to a StreamHandler. It exposes
// the stream ID, session-level info (e.g. the original connect path), and
// helpers for writing back SWSP frames on the same stream.
type Stream interface {
	ID() uint32
	// ConnectPath is the URL path from the original `connect` message on
	// stream 0. Useful for handlers (like HTTP proxy) whose behavior
	// depends on it.
	ConnectPath() string
	// WriteSYN sends a SYN frame on this stream with the given payload.
	WriteSYN(payload []byte) error
	// WriteDAT sends a DAT (no flags) frame on this stream.
	WriteDAT(payload []byte) error
	// WriteFIN sends a FIN frame on this stream with the given payload
	// (may be nil).
	WriteFIN(payload []byte) error
	// SendRaw sends arbitrary flag bits with payload. Use for SYN|FIN
	// combined frames.
	SendRaw(flags uint16, payload []byte) error
	// BufferedAmount returns the data channel's current send buffer size.
	// Handlers use this for backpressure when streaming large responses.
	BufferedAmount() uint64
}

// StreamHandler handles a single stream type. Implementations register
// with a Session via Session.Register and receive callbacks for SYN /
// DAT / FIN frames on streams claimed for their type.
type StreamHandler interface {
	// Type returns the SWSP stream `type` this handler claims, e.g. "http".
	Type() string
	// OnConnect is called once per session, after the stream-0 `connect`
	// message is received and (if PIN was set) PIN auth succeeded. The
	// path is the URL path from the connect message. Returning an error
	// causes the session to reject the connection.
	OnConnect(path string) error
	// OnSYN is called for the first frame on a new stream whose `type`
	// matches this handler. final=true when the SYN frame had the FIN
	// flag set as well (i.e. single-frame stream, no DAT/FIN to follow).
	// For HTTP, final=true is a body-less request (GET, HEAD).
	OnSYN(s Stream, payload []byte, final bool) error
	// OnDAT is called for each non-SYN, non-FIN frame on a stream this
	// handler is handling.
	OnDAT(s Stream, payload []byte) error
	// OnFIN is called for the final frame on a stream this handler is
	// handling. The handler should clean up any per-stream state.
	OnFIN(s Stream, payload []byte) error
}
