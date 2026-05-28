package client

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/richlegrand/bitbang/internal/protocol"
)

// Shell DAT tag bytes — must match the listener side
// (internal/streamtype/shell.go). The first byte of every shell DAT
// frame selects what to do with the rest of the payload.
const (
	shellTagStdin  byte = 0x00 // client → device
	shellTagStdout byte = 0x01 // device → client
	shellTagStderr byte = 0x02 // device → client
	shellTagSignal byte = 0x03 // client → device, payload = signal name
	shellTagResize byte = 0x04 // client → device, payload = [cols:u16][rows:u16] LE
)

// ShellOptions configures a shell stream opened via Session.Shell.
type ShellOptions struct {
	Argv []string          // command + args; empty means "use listener default"
	PTY  bool              // true → allocate a PTY on the listener (interactive)
	Cols int               // initial cols (PTY mode)
	Rows int               // initial rows (PTY mode)
	Env  map[string]string // extra env vars set on the spawned process
	Cwd  string            // working directory for the spawned process

	// I/O streams. Stdin nil means "no input" (caller doesn't intend to
	// write); Stdout/Stderr nil mean "discard." In PTY mode the listener
	// merges stderr into stdout; only Stdout receives output.
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer

	// Resizes carries terminal resize events. Caller closes when no
	// more resizes are coming (e.g. terminal exited). Nil = no resizes.
	Resizes <-chan ShellSize

	// Signals carries signal names ("INT", "TERM", "HUP", etc.) to be
	// forwarded to the remote process. Caller closes when done. Nil
	// = no signal forwarding. In PTY mode signals usually arrive as
	// stdin bytes (raw mode); this channel is for the non-PTY case
	// and for signals the terminal doesn't translate.
	Signals <-chan string
}

// ShellSize is a terminal-size tuple.
type ShellSize struct {
	Cols, Rows int
}

// ShellResult is what Shell returns when the remote process exits.
type ShellResult struct {
	ExitCode int
	Signal   string // non-empty if the process was killed by a signal
}

// Shell opens a shell stream, drives I/O, and blocks until the remote
// process exits (FIN with status received) or the channel dies. The
// caller's stdin/stdout/stderr come in via opts; the remote process's
// exit code comes back in the returned ShellResult.
func (s *Session) Shell(opts ShellOptions) (*ShellResult, error) {
	st := s.OpenStream()
	defer st.Close()

	// Build SYN payload from opts. Marshal as a flat map so optional
	// fields are omitted cleanly.
	synMap := map[string]interface{}{
		"type": "shell",
		"pty":  opts.PTY,
	}
	if len(opts.Argv) > 0 {
		synMap["argv"] = opts.Argv
	}
	if opts.Cols > 0 {
		synMap["cols"] = opts.Cols
	}
	if opts.Rows > 0 {
		synMap["rows"] = opts.Rows
	}
	if len(opts.Env) > 0 {
		synMap["env"] = opts.Env
	}
	if opts.Cwd != "" {
		synMap["cwd"] = opts.Cwd
	}
	synBytes, _ := json.Marshal(synMap)
	if err := st.WriteSYN(synBytes); err != nil {
		return nil, fmt.Errorf("send shell SYN: %w", err)
	}

	// Spawn the async pumps. They run until their source channel/reader
	// closes; they don't block this function from returning. On exit,
	// the deferred st.Close + the session's eventual DC teardown make
	// any in-flight WriteDAT fail, so the goroutines exit naturally.
	if opts.Stdin != nil {
		go pumpStdin(st, opts.Stdin)
	}
	if opts.Resizes != nil {
		go pumpResizes(st, opts.Resizes)
	}
	if opts.Signals != nil {
		go pumpSignals(st, opts.Signals)
	}

	// Drain inbox: route DAT bytes by tag, look for early error SYN,
	// terminate on FIN.
	var result ShellResult
	for frame := range st.Inbox() {
		if frame.IsSYN() {
			// Server only sends a SYN on shell streams when something
			// went wrong before spawn — bad JSON, exec error, etc.
			// Shape: {"error": "..."}. We surface it and return; the
			// FIN that follows will close the inbox and let our caller
			// proceed.
			var hdr struct {
				Error string `json:"error"`
			}
			if err := json.Unmarshal(frame.Payload, &hdr); err == nil && hdr.Error != "" {
				return nil, fmt.Errorf("server: %s", hdr.Error)
			}
			continue
		}

		if frame.IsFIN() {
			// {"exit_code": N, "signal": "..."} on normal exit; possibly
			// {"error": "..."} for a mid-stream failure.
			if len(frame.Payload) > 0 {
				var fin struct {
					ExitCode int    `json:"exit_code"`
					Signal   string `json:"signal"`
					Error    string `json:"error"`
				}
				_ = json.Unmarshal(frame.Payload, &fin)
				if fin.Error != "" {
					return nil, fmt.Errorf("server: %s", fin.Error)
				}
				result.ExitCode = fin.ExitCode
				result.Signal = fin.Signal
			}
			return &result, nil
		}

		// DAT frame — tag byte selects routing.
		if len(frame.Payload) < 1 {
			continue
		}
		tag := frame.Payload[0]
		body := frame.Payload[1:]
		switch tag {
		case shellTagStdout:
			if opts.Stdout != nil {
				_, _ = opts.Stdout.Write(body)
			}
		case shellTagStderr:
			if opts.Stderr != nil {
				_, _ = opts.Stderr.Write(body)
			}
			// Other tags from the device (shouldn't happen by design)
			// are silently dropped.
		}
	}

	// Inbox closed without FIN — data channel died. Return whatever
	// state we accumulated; the exit code is zero unless something
	// already set it.
	return &result, nil
}

// pumpStdin reads from r and emits each chunk as a DAT(stdin) frame.
// On EOF it sends FIN to tell the listener to close the remote stdin
// (so commands like `cat` finish). On any other error or write
// failure it just exits — the inbox-drainer in Shell will detect the
// session end via its own FIN/close.
func pumpStdin(st *Stream, r io.Reader) {
	buf := make([]byte, protocol.MaxChunkSize-1) // 1 byte for the tag
	const maxBuffered uint64 = 8 << 20
	for {
		n, err := r.Read(buf)
		if n > 0 {
			for st.BufferedAmount() > maxBuffered {
				time.Sleep(1 * time.Millisecond)
			}
			frame := make([]byte, 1+n)
			frame[0] = shellTagStdin
			copy(frame[1:], buf[:n])
			if writeErr := st.WriteDAT(frame); writeErr != nil {
				return
			}
		}
		if err == io.EOF {
			_ = st.WriteFIN(nil)
			return
		}
		if err != nil {
			return
		}
	}
}

func pumpResizes(st *Stream, ch <-chan ShellSize) {
	for sz := range ch {
		buf := make([]byte, 5)
		buf[0] = shellTagResize
		binary.LittleEndian.PutUint16(buf[1:3], uint16(sz.Cols))
		binary.LittleEndian.PutUint16(buf[3:5], uint16(sz.Rows))
		if err := st.WriteDAT(buf); err != nil {
			return
		}
	}
}

func pumpSignals(st *Stream, ch <-chan string) {
	for name := range ch {
		buf := make([]byte, 1+len(name))
		buf[0] = shellTagSignal
		copy(buf[1:], name)
		if err := st.WriteDAT(buf); err != nil {
			return
		}
	}
}
