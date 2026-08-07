package streamtype

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"sync"
	"time"

	"github.com/richlegrand/bitbang/internal/protocol"
)

// Filesystem is the minimal surface a FileHandler needs to satisfy SWSP
// `file`-type ops. fileshare.FileShare implements this structurally so the
// streamtype package doesn't depend on fileshare.
type Filesystem interface {
	// Stat returns metadata for a path. Returns ErrNotFound if missing.
	StatPath(relPath string) (FileStat, error)
	// ListPath returns the entries of a directory.
	ListPath(relPath string) ([]FileStat, error)
	// OpenRead opens a file for reading.
	OpenRead(relPath string) (io.ReadCloser, FileStat, error)
	// OpenWrite opens a file for writing. If overwrite is false and the
	// path exists, returns ErrExists.
	OpenWrite(relPath string, overwrite bool) (io.WriteCloser, error)
}

// FileStat is the per-entry metadata returned to clients.
type FileStat struct {
	Name     string `json:"name"`
	Type     string `json:"type"` // "file" | "directory"
	Size     int64  `json:"size,omitempty"`
	Modified int64  `json:"modified"` // Unix seconds
	Mime     string `json:"mime,omitempty"`
}

// ErrNotFound and ErrExists are the well-known errors Filesystem
// implementations can return.
var (
	ErrNotFound = errors.New("not found")
	ErrExists   = errors.New("exists")
)

// FileHandler implements StreamHandler for type="file" — native SWSP file
// operations (get/put/list). Used by `bitbang cp`.
//
// Wire shape (see plan / SWSP v3 spec):
//
//	get:  client SYN {op:"get",path}  → server SYN {status:"ok",size,...}
//	      server DAT bytes...        → server FIN
//	put:  client SYN {op:"put",path,overwrite?,size?}
//	      server SYN {status:"ok"}    (ack, ready to receive)
//	      client DAT bytes...        → client FIN
//	      server FIN {status:"ok"} or {error}
//	list: client SYN {op:"list",path} → server SYN {status:"ok"}
//	      server DAT {entries:[...]} → server FIN
type FileHandler struct {
	FS      Filesystem
	Verbose bool

	mu      sync.Mutex
	streams map[uint32]*filePending
}

type filePending struct {
	op     string
	pw     *io.PipeWriter
	closer io.Closer
	done   chan struct{}

	mu        sync.Mutex
	closed    bool
	closeOnce sync.Once
}

// NewFile constructs a FileHandler backed by the given Filesystem.
func NewFile(fs Filesystem, verbose bool) *FileHandler {
	return &FileHandler{
		FS:      fs,
		Verbose: verbose,
		streams: make(map[uint32]*filePending),
	}
}

func (h *FileHandler) Type() string             { return "file" }
func (h *FileHandler) OnConnect(_ string) error { return nil }

func (h *FileHandler) OnSYN(s Stream, payload []byte, final bool) error {
	var op protocol.FileOp
	if err := json.Unmarshal(payload, &op); err != nil {
		h.sendFileError(s, "bad request: "+err.Error())
		return nil
	}

	switch op.Op {
	case "get":
		ps := &filePending{op: "get", done: make(chan struct{})}
		h.trackStream(s.ID(), ps)
		go h.handleGet(s, op, ps)
	case "list":
		go h.handleList(s, op)
	case "put":
		// Validate the destination up-front. If OpenWrite would fail
		// (uploads disabled, path traversal, exists+!overwrite, …) we
		// reject with a single error SYN+FIN before any ack. Acking
		// "ok" and then failing mid-stream is a footgun: the client
		// dutifully streams the bytes and never sees the error because
		// its put loop is already in the data-pumping phase.
		w, err := h.FS.OpenWrite(op.Path, op.Overwrite)
		if err != nil {
			log.Printf("Put rejected: %s (%v)", op.Path, err)
			h.sendFileError(s, fileErrMessage(err, op.Path))
			return nil
		}
		// Zero-byte upload (SYN|FIN with no body): write nothing, close,
		// ack via FIN trailer.
		if final {
			_ = w.Close()
			log.Printf("Received: %s (0 bytes)", op.Path)
			done, _ := json.Marshal(map[string]string{"status": "ok"})
			_ = s.WriteSYN(done)
			_ = s.WriteFIN(nil)
			return nil
		}
		pr, pw := io.Pipe()
		ps := &filePending{op: "put", pw: pw, closer: w, done: make(chan struct{})}
		h.trackStream(s.ID(), ps)
		// Ack now that we know OpenWrite succeeded.
		ack, _ := json.Marshal(map[string]string{"status": "ok"})
		_ = s.WriteSYN(ack)
		go h.handlePut(s, op.Path, w, pr, ps)
	default:
		h.sendFileError(s, "unknown op: "+op.Op)
	}
	return nil
}

func (h *FileHandler) OnDAT(s Stream, payload []byte) error {
	h.mu.Lock()
	ps := h.streams[s.ID()]
	h.mu.Unlock()
	if ps == nil || ps.op != "put" {
		return nil
	}
	if len(payload) > 0 {
		_, err := ps.pw.Write(payload)
		return err
	}
	return nil
}

func (h *FileHandler) OnFIN(s Stream, payload []byte) error {
	h.mu.Lock()
	ps := h.streams[s.ID()]
	h.mu.Unlock()
	if ps == nil || ps.op != "put" {
		return nil
	}
	if len(payload) > 0 {
		if _, err := ps.pw.Write(payload); err != nil {
			return err
		}
	}
	return ps.pw.Close()
}

func (h *FileHandler) OnReset(s Stream, _, _ string) {
	h.mu.Lock()
	ps := h.streams[s.ID()]
	delete(h.streams, s.ID())
	h.mu.Unlock()
	if ps != nil {
		ps.close()
	}
}

func (h *FileHandler) handleGet(s Stream, op protocol.FileOp, ps *filePending) {
	defer h.finishStream(s.ID(), ps)
	r, stat, err := h.FS.OpenRead(op.Path)
	if err != nil {
		log.Printf("Get rejected: %s (%v)", op.Path, err)
		h.sendFileError(s, fileErrMessage(err, op.Path))
		return
	}
	if !ps.setCloser(r) {
		return
	}

	meta, _ := json.Marshal(map[string]interface{}{
		"status":   "ok",
		"size":     stat.Size,
		"modified": stat.Modified,
		"mime":     stat.Mime,
	})
	if err := s.WriteSYN(meta); err != nil {
		return
	}

	const maxBuffered = 8 << 20
	buf := make([]byte, protocol.MaxChunkSize)
	var total int64
	backpressureTick := time.NewTicker(time.Millisecond)
	defer backpressureTick.Stop()
	for {
		n, readErr := r.Read(buf)
		if n > 0 {
			for s.BufferedAmount() > maxBuffered {
				select {
				case <-ps.done:
					return
				case <-backpressureTick.C:
				}
			}
			if err := s.WriteDAT(buf[:n]); err != nil {
				log.Printf("Sent: %s (interrupted after %d bytes: %v)", op.Path, total, err)
				return
			}
			total += int64(n)
		}
		if readErr != nil {
			break
		}
	}
	_ = s.WriteFIN(nil)
	log.Printf("Sent: %s (%d bytes)", op.Path, total)
}

// handlePut copies the body into the (already-opened) writer and emits
// the final status. Run as a goroutine after OnSYN has ack'd the
// upload. Mid-stream errors (disk full, broken pipe, etc.) go in the
// FIN trailer — NOT as a separate SYN — so the client's put loop,
// which is waiting for FIN by the time we get here, can see them.
func (h *FileHandler) handlePut(s Stream, path string, w io.WriteCloser, body io.Reader, ps *filePending) {
	defer h.finishStream(s.ID(), ps)
	n, err := io.Copy(w, body)
	if err != nil {
		log.Printf("Put failed mid-stream: %s (after %d bytes: %v)", path, n, err)
		finErr, _ := json.Marshal(map[string]string{
			"status": "error",
			"error":  "write failed: " + err.Error(),
		})
		_ = s.WriteFIN(finErr)
		return
	}
	log.Printf("Received: %s (%d bytes)", path, n)
	done, _ := json.Marshal(map[string]string{"status": "ok"})
	_ = s.WriteFIN(done)
}

func (h *FileHandler) trackStream(id uint32, ps *filePending) {
	h.mu.Lock()
	old := h.streams[id]
	h.streams[id] = ps
	h.mu.Unlock()
	if old != nil {
		old.close()
	}
}

func (h *FileHandler) finishStream(id uint32, want *filePending) {
	h.mu.Lock()
	if h.streams[id] == want {
		delete(h.streams, id)
	}
	h.mu.Unlock()
	want.close()
}

func (ps *filePending) setCloser(closer io.Closer) bool {
	ps.mu.Lock()
	if ps.closed {
		ps.mu.Unlock()
		_ = closer.Close()
		return false
	}
	ps.closer = closer
	ps.mu.Unlock()
	return true
}

func (ps *filePending) close() {
	ps.closeOnce.Do(func() {
		if ps.done != nil {
			close(ps.done)
		}
		ps.mu.Lock()
		ps.closed = true
		pw := ps.pw
		closer := ps.closer
		ps.mu.Unlock()
		if pw != nil {
			_ = pw.CloseWithError(context.Canceled)
		}
		if closer != nil {
			_ = closer.Close()
		}
	})
}

func (h *FileHandler) handleList(s Stream, op protocol.FileOp) {
	entries, err := h.FS.ListPath(op.Path)
	if err != nil {
		log.Printf("List rejected: %s (%v)", op.Path, err)
		h.sendFileError(s, fileErrMessage(err, op.Path))
		return
	}
	log.Printf("Listed: %s (%d entries)", op.Path, len(entries))
	body, _ := json.Marshal(map[string]interface{}{"entries": entries})
	if err := protocol.ValidateFramePayload(body); err != nil {
		h.sendFileError(s, "directory listing is too large")
		return
	}

	hdr, _ := json.Marshal(map[string]string{"status": "ok"})
	if err := s.WriteSYN(hdr); err != nil {
		return
	}
	_ = s.WriteDAT(body)
	_ = s.WriteFIN(nil)
}

// sendFileError emits SYN+FIN with an {error: "..."} payload. Used when
// the request can't proceed (path traversal, missing file, etc.).
func (h *FileHandler) sendFileError(s Stream, msg string) {
	hdr, _ := json.Marshal(map[string]string{"status": "error", "error": msg})
	_ = s.WriteSYN(hdr)
	_ = s.WriteFIN(nil)
}

func fileErrMessage(err error, path string) string {
	switch {
	case errors.Is(err, ErrNotFound):
		return "not found: " + path
	case errors.Is(err, ErrExists):
		return "exists: " + path
	default:
		return err.Error()
	}
}
