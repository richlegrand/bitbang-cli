package client

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"

	"github.com/pion/webrtc/v4"

	"github.com/richlegrand/bitbang/internal/protocol"
)

// stderr is the package-wide log sink for connection progress + debug
// chatter. cp prints user-facing data to stdout, so all diagnostic noise
// from this package routes here instead.
var stderr = os.Stderr

// Session wraps the data channel after bidirectional verify has succeeded
// and the control-stream `ready` message has been received. From here on
// the caller drives SWSP file (or future shell/tcp/etc.) streams.
//
// Streams are multiplexed by ID inside the channel. The client picks odd
// numbered IDs starting at 1, leaving the device free to pick even IDs
// for future device-initiated streams; for v1 only the client opens
// streams, so the parity doesn't strictly matter.
type Session struct {
	DC      *webrtc.DataChannel
	Verbose bool

	// ServerCaps and ServerVersion come from the listener's `ready` and
	// let callers gate behavior (e.g. don't try `file` ops if the server
	// doesn't advertise it).
	ServerCaps    []string
	ServerVersion int

	nextStreamID uint32
	mu           sync.Mutex
	streams      map[uint32]*stream
	closed       atomic.Bool
}

// stream is the per-stream state held by the session: an inbound frame
// queue the caller drains via the public Inbox method. SYN/DAT/FIN
// frames all arrive on the same channel — the caller inspects flags.
type stream struct {
	id    uint32
	inbox chan protocol.Frame
	once  sync.Once
}

// newSession constructs a Session bound to a data channel. The peer is
// passed in so the session layer can read off the bidirectional-verify
// nonce expected on the first stream-0 frame; once verify completes the
// session takes over the DC message stream.
func newSession(p *Peer) *Session {
	return &Session{
		DC:           p.DC,
		nextStreamID: 1,
		streams:      make(map[uint32]*stream),
	}
}

// handshake runs the client-side control protocol after the data channel
// opens: verify_nonce_hash → connect → (auth_required + auth)* → ready.
// Returns once `ready` arrives or the channel dies.
//
// pinPrompt is called when the listener replies auth_required; it returns
// the PIN to send (and an error to abort). cp passes a stdin-based
// implementation that uses golang.org/x/term to hide the input.
func (s *Session) handshake(p *Peer, path string, caps []string, pinPrompt func(retry int) (string, error)) error {
	// 1. verify_nonce_hash must be the *first* control frame.
	first, ok := <-p.DCMessages()
	if !ok {
		return errors.New("data channel closed before verify")
	}
	frame, err := protocol.ParseFrame(first)
	if err != nil {
		return fmt.Errorf("parse verify frame: %w", err)
	}
	if frame.StreamID != 0 || !frame.IsSYN() {
		return fmt.Errorf("expected verify_nonce_hash on stream 0, got stream %d flags %#x", frame.StreamID, frame.Flags)
	}
	var verify struct {
		Type string `json:"type"`
		Hash string `json:"hash"`
	}
	if err := json.Unmarshal(frame.Payload, &verify); err != nil {
		return fmt.Errorf("parse verify_nonce_hash: %w", err)
	}
	if verify.Type != "verify_nonce_hash" {
		return fmt.Errorf("expected verify_nonce_hash, got %q", verify.Type)
	}
	want := expectedNonceHash(p.Nonce())
	if verify.Hash != want {
		return errors.New("bidirectional verify failed: device did not prove possession of private key")
	}
	if s.Verbose {
		fmt.Fprintln(stderr, "[client] bidirectional verify OK")
	}

	// 2. Send `connect` with caps + version.
	connectMsg, _ := json.Marshal(map[string]interface{}{
		"type":    "connect",
		"path":    path,
		"caps":    caps,
		"version": protocol.SWSPVersion,
	})
	if err := s.sendControlSYN(connectMsg); err != nil {
		return fmt.Errorf("send connect: %w", err)
	}

	// 3. Drain stream-0 SYN frames until we see `ready` (success) or
	// `error` (give up). `auth_required` triggers a PIN prompt and
	// retries; `auth_result.success=false` triggers another retry up to
	// a small cap.
	retry := 0
	for {
		msg, ok := <-p.DCMessages()
		if !ok {
			return errors.New("data channel closed during handshake")
		}
		f, err := protocol.ParseFrame(msg)
		if err != nil {
			return fmt.Errorf("parse control frame: %w", err)
		}
		// During handshake everything should be on stream 0. If a SYN
		// arrives on a non-zero stream the listener is misbehaving;
		// surface as a protocol error.
		if f.StreamID != 0 {
			return fmt.Errorf("unexpected non-control frame during handshake (stream %d)", f.StreamID)
		}
		if !f.IsSYN() {
			continue
		}
		var ctl struct {
			Type          string   `json:"type"`
			Message       string   `json:"message"`
			Success       bool     `json:"success"`
			Caps          []string `json:"caps"`
			ServerVersion int      `json:"server_version"`
		}
		_ = json.Unmarshal(f.Payload, &ctl)

		switch ctl.Type {
		case "ready":
			s.ServerCaps = ctl.Caps
			s.ServerVersion = ctl.ServerVersion
			if s.ServerVersion == 0 {
				// v2 listeners send {type:"ready"} with no version.
				s.ServerVersion = 2
			}
			if s.Verbose {
				fmt.Fprintf(stderr, "[client] device ready (server v%d, caps: %v)\n",
					s.ServerVersion, s.ServerCaps)
			}
			return nil
		case "auth_required":
			if pinPrompt == nil {
				return errors.New("listener requires PIN but no prompt provided")
			}
			pin, err := pinPrompt(retry)
			if err != nil {
				return fmt.Errorf("PIN prompt: %w", err)
			}
			retry++
			authMsg, _ := json.Marshal(map[string]string{"type": "auth", "pin": pin})
			if err := s.sendControlSYN(authMsg); err != nil {
				return fmt.Errorf("send auth: %w", err)
			}
		case "auth_result":
			if ctl.Success {
				// Listener follows up with `ready` immediately; loop.
				continue
			}
			if retry >= 3 {
				return errors.New("PIN authentication failed (3 attempts)")
			}
			fmt.Fprintln(stderr, "Incorrect PIN, try again.")
			pin, err := pinPrompt(retry)
			if err != nil {
				return fmt.Errorf("PIN prompt: %w", err)
			}
			retry++
			authMsg, _ := json.Marshal(map[string]string{"type": "auth", "pin": pin})
			if err := s.sendControlSYN(authMsg); err != nil {
				return fmt.Errorf("send auth: %w", err)
			}
		case "error":
			return fmt.Errorf("listener error: %s", ctl.Message)
		default:
			if s.Verbose {
				fmt.Fprintf(stderr, "[client] ignoring control message type=%q\n", ctl.Type)
			}
		}
	}
}

// startDispatcher takes over the DC message queue from the session layer
// and routes inbound frames to their owning stream's inbox. Run as a
// goroutine after handshake completes; exits when the DC closes.
func (s *Session) startDispatcher(p *Peer) {
	go func() {
		for msg := range p.DCMessages() {
			frame, err := protocol.ParseFrame(msg)
			if err != nil {
				if s.Verbose {
					fmt.Fprintf(stderr, "[client] dropped malformed frame: %v\n", err)
				}
				continue
			}
			if frame.StreamID == 0 {
				// Late stream-0 messages (e.g. a server-initiated error)
				// are uncommon for v3; log + drop.
				if s.Verbose {
					fmt.Fprintf(stderr, "[client] ignoring late stream-0 frame flags=%#x\n", frame.Flags)
				}
				continue
			}
			s.mu.Lock()
			st := s.streams[frame.StreamID]
			s.mu.Unlock()
			if st == nil {
				if s.Verbose {
					fmt.Fprintf(stderr, "[client] frame for unknown stream %d (flags %#x) — dropping\n", frame.StreamID, frame.Flags)
				}
				continue
			}
			// Non-blocking send keeps the dispatcher responsive even if
			// the consumer is slow; in practice callers consume eagerly.
			st.inbox <- frame
			if frame.IsFIN() {
				// FIN closes the stream's inbox so callers ranging over
				// it see the channel close instead of blocking forever.
				s.closeStream(st.id)
			}
		}
	}()
	go func() {
		<-p.DCClosed()
		s.closed.Store(true)
		s.mu.Lock()
		defer s.mu.Unlock()
		for id, st := range s.streams {
			st.once.Do(func() { close(st.inbox) })
			delete(s.streams, id)
		}
	}()
}

// OpenStream allocates a new outbound stream and returns its handle.
// The caller writes the SYN/DAT/FIN frames and reads inbound frames via
// the returned Stream's Inbox method.
func (s *Session) OpenStream() *Stream {
	s.mu.Lock()
	id := s.nextStreamID
	// Client uses odd IDs (1,3,5...) so any future device-initiated
	// stream can use even IDs without an ID-allocation negotiation.
	s.nextStreamID += 2
	st := &stream{
		id:    id,
		inbox: make(chan protocol.Frame, 256),
	}
	s.streams[id] = st
	s.mu.Unlock()
	return &Stream{s: s, st: st}
}

func (s *Session) closeStream(id uint32) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.streams[id]
	if st == nil {
		return
	}
	st.once.Do(func() { close(st.inbox) })
	delete(s.streams, id)
}

func (s *Session) sendFrame(streamID uint32, flags uint16, payload []byte) error {
	if s.closed.Load() || s.DC.ReadyState() != webrtc.DataChannelStateOpen {
		return errors.New("data channel closed")
	}
	return s.DC.Send(protocol.BuildFrame(streamID, flags, payload))
}

func (s *Session) sendControlSYN(payload []byte) error {
	return s.sendFrame(0, protocol.FlagSYN, payload)
}

// Close closes the data channel and returns. Anything left in flight
// (DTLS close_notify, TURN dealloc, ICE relay socket teardown) is left
// to the OS to reap when the process exits — pion's synchronous
// PC.Close() path is slow on relay sessions and isn't worth waiting for.
func (s *Session) Close() {
	if s.DC != nil {
		_ = s.DC.Close()
	}
}

// Stream is the caller-facing handle for an outbound SWSP stream.
type Stream struct {
	s  *Session
	st *stream
}

// ID returns the stream's wire-level ID.
func (s *Stream) ID() uint32 { return s.st.id }

// WriteSYN sends a SYN frame.
func (s *Stream) WriteSYN(payload []byte) error {
	return s.s.sendFrame(s.st.id, protocol.FlagSYN, payload)
}

// WriteDAT sends a DAT frame. Caller is responsible for chunking to
// MaxChunkSize (file ops do this internally).
func (s *Stream) WriteDAT(payload []byte) error {
	return s.s.sendFrame(s.st.id, protocol.FlagDAT, payload)
}

// WriteFIN sends the closing frame for an outbound stream.
func (s *Stream) WriteFIN(payload []byte) error {
	return s.s.sendFrame(s.st.id, protocol.FlagFIN, payload)
}

// Inbox is the channel of inbound frames for this stream. Closes when
// the device sends FIN or the data channel is torn down.
func (s *Stream) Inbox() <-chan protocol.Frame { return s.st.inbox }

// BufferedAmount exposes the underlying DC's send buffer, for backpressure.
func (s *Stream) BufferedAmount() uint64 { return s.s.DC.BufferedAmount() }

// Close drops the stream from the session's routing table. The caller
// is expected to have sent FIN already; this is just cleanup if the
// stream is being abandoned (e.g. on error).
func (s *Stream) Close() { s.s.closeStream(s.st.id) }
