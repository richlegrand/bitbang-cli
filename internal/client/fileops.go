package client

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/richlegrand/bitbang/internal/protocol"
)

// FileInfo is the metadata the listener returns at the head of a `get`
// (and `list` entries). Surface enough for cp to display a progress
// indicator and a useful one-line summary.
type FileInfo struct {
	Name     string `json:"name,omitempty"`
	Type     string `json:"type,omitempty"`
	Size     int64  `json:"size,omitempty"`
	Modified int64  `json:"modified,omitempty"`
	Mime     string `json:"mime,omitempty"`
}

// fileStatus is the JSON shape the listener uses for SYN/FIN status
// messages on a `file` stream. Mirrors streamtype/file.go on the
// listener side.
type fileStatus struct {
	Status   string `json:"status"`
	Error    string `json:"error"`
	Size     int64  `json:"size"`
	Modified int64  `json:"modified"`
	Mime     string `json:"mime"`
}

// Get downloads remotePath and streams the bytes to w. Returns the
// listener-reported FileInfo on success.
//
// Wire trace (cf. streamtype/file.go on listener side):
//
//	client → SYN|FIN  {type:"file", op:"get", path:remotePath}
//	server → SYN       {status:"ok", size:N, modified, mime}
//	server → DAT*      raw bytes
//	server → FIN       empty or {error}
func (s *Session) Get(remotePath string, w io.Writer) (FileInfo, error) {
	st := s.OpenStream()
	defer st.Close()

	syn, _ := json.Marshal(protocol.FileOp{Type: "file", Op: "get", Path: remotePath})
	// SYN|FIN: get has no body, signal end-of-request in one frame so the
	// listener doesn't sit waiting for DAT.
	if err := st.s.sendFrame(st.st.id, protocol.FlagSYN|protocol.FlagFIN, syn); err != nil {
		return FileInfo{}, fmt.Errorf("send get: %w", err)
	}

	// First inbound frame must be SYN with status.
	first, ok := <-st.Inbox()
	if !ok {
		return FileInfo{}, errors.New("stream closed before response")
	}
	if !first.IsSYN() {
		return FileInfo{}, fmt.Errorf("expected SYN, got flags %#x", first.Flags)
	}
	var status fileStatus
	if err := json.Unmarshal(first.Payload, &status); err != nil {
		return FileInfo{}, fmt.Errorf("parse get response: %w", err)
	}
	if status.Status != "ok" {
		return FileInfo{}, fmt.Errorf("server: %s", statusErr(status))
	}
	info := FileInfo{Size: status.Size, Modified: status.Modified, Mime: status.Mime}

	// A single-frame SYN|FIN response is valid for empty files.
	if first.IsFIN() {
		return info, nil
	}

	// Drain DAT frames until FIN. If FIN carries a {error} we surface it.
	for frame := range st.Inbox() {
		if frame.IsFIN() {
			if len(frame.Payload) > 0 {
				var fin fileStatus
				if json.Unmarshal(frame.Payload, &fin) == nil && fin.Error != "" {
					return info, fmt.Errorf("server: %s", fin.Error)
				}
			}
			return info, nil
		}
		if len(frame.Payload) == 0 {
			continue
		}
		if _, err := w.Write(frame.Payload); err != nil {
			return info, fmt.Errorf("write local: %w", err)
		}
	}
	return info, errors.New("stream closed without FIN")
}

// Put uploads bytes from r to remotePath. If overwrite is false and the
// path exists, the listener returns ErrExists and Put returns an error.
//
// Wire trace:
//
//	client → SYN       {type:"file", op:"put", path, overwrite}
//	server → SYN       {status:"ok"}                    (ack — ready)
//	client → DAT*      raw bytes
//	client → FIN       empty
//	server → FIN       {status:"ok"} or {error}
func (s *Session) Put(remotePath string, r io.Reader, overwrite bool) error {
	st := s.OpenStream()
	defer st.Close()

	syn, _ := json.Marshal(protocol.FileOp{
		Type: "file", Op: "put", Path: remotePath, Overwrite: overwrite,
	})
	if err := st.WriteSYN(syn); err != nil {
		return fmt.Errorf("send put: %w", err)
	}

	// Wait for the server's ack SYN before sending DAT — without this the
	// server can't return an early error (path traversal, exists, uploads
	// disabled) until after the entire body has streamed.
	first, ok := <-st.Inbox()
	if !ok {
		return errors.New("stream closed before ack")
	}
	if !first.IsSYN() {
		return fmt.Errorf("expected SYN ack, got flags %#x", first.Flags)
	}
	var ack fileStatus
	if err := json.Unmarshal(first.Payload, &ack); err != nil {
		return fmt.Errorf("parse put ack: %w", err)
	}
	if ack.Status != "ok" {
		return fmt.Errorf("server: %s", statusErr(ack))
	}

	// Pump the local file in MaxChunkSize-sized DAT frames, with a soft
	// cap on the DC send buffer so a slow consumer doesn't blow up the
	// SCTP queue. The 8 MB cap mirrors what HTTPLocalHandler uses.
	buf := make([]byte, protocol.MaxChunkSize)
	const maxBuffered uint64 = 8 << 20
	for {
		n, readErr := r.Read(buf)
		if n > 0 {
			for st.BufferedAmount() > maxBuffered {
				time.Sleep(1 * time.Millisecond)
			}
			if err := st.WriteDAT(buf[:n]); err != nil {
				return fmt.Errorf("write DAT: %w", err)
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return fmt.Errorf("read local: %w", readErr)
		}
	}

	// FIN with empty payload — server replies FIN with status.
	if err := st.WriteFIN(nil); err != nil {
		return fmt.Errorf("send FIN: %w", err)
	}
	for frame := range st.Inbox() {
		if !frame.IsFIN() {
			continue
		}
		if len(frame.Payload) == 0 {
			return nil
		}
		var fin fileStatus
		if err := json.Unmarshal(frame.Payload, &fin); err != nil {
			return fmt.Errorf("parse put result: %w", err)
		}
		if fin.Status == "ok" {
			return nil
		}
		return fmt.Errorf("server: %s", statusErr(fin))
	}
	return errors.New("stream closed without FIN")
}

// List enumerates remotePath (which must be a directory on the listener).
// Returned entries are the listener's FileStat shape — name, type
// ("file"|"directory"), size (for files), modified.
func (s *Session) List(remotePath string) ([]FileInfo, error) {
	st := s.OpenStream()
	defer st.Close()

	syn, _ := json.Marshal(protocol.FileOp{Type: "file", Op: "list", Path: remotePath})
	if err := st.s.sendFrame(st.st.id, protocol.FlagSYN|protocol.FlagFIN, syn); err != nil {
		return nil, fmt.Errorf("send list: %w", err)
	}

	first, ok := <-st.Inbox()
	if !ok {
		return nil, errors.New("stream closed before response")
	}
	if !first.IsSYN() {
		return nil, fmt.Errorf("expected SYN, got flags %#x", first.Flags)
	}
	var status fileStatus
	if err := json.Unmarshal(first.Payload, &status); err != nil {
		return nil, fmt.Errorf("parse list response: %w", err)
	}
	if status.Status != "ok" {
		return nil, fmt.Errorf("server: %s", statusErr(status))
	}

	// Body is one DAT frame with {entries:[...]}. (Listener may split if
	// the JSON ever exceeds MaxChunkSize — accumulate just in case.)
	var body []byte
	for frame := range st.Inbox() {
		if frame.IsFIN() {
			break
		}
		body = append(body, frame.Payload...)
	}
	if len(body) == 0 {
		return nil, nil
	}
	var resp struct {
		Entries []FileInfo `json:"entries"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("parse list body: %w", err)
	}
	return resp.Entries, nil
}

// statusErr extracts the human-readable error from a fileStatus payload.
// Falls back to the status string if no `error` field is present.
func statusErr(s fileStatus) string {
	if s.Error != "" {
		return s.Error
	}
	if s.Status != "" {
		return s.Status
	}
	return "unknown error"
}
