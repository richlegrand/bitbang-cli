package share

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// State is the on-disk record of a running share. The worker writes it
// after signaling registration succeeds, and a later share command removes
// it under the target lifecycle lock. It contains bearer credentials and is
// stored under ~/.bitbang/shares with 0700 directory and 0600 file modes.
type State struct {
	Socket      string `json:"socket,omitempty"`
	SessionID   string `json:"session_id"`
	SessionName string `json:"session_name,omitempty"`
	MgmtSession string `json:"mgmt_session"`
	UID         string `json:"uid"`
	Server      string `json:"server"`

	// ControlURL is absent for --read-only shares: the control
	// credential is never generated, so a broadcast share physically
	// cannot be typed into.
	ControlURL string `json:"control_url,omitempty"`
	ViewURL    string `json:"view_url"`
	MaxViewers int    `json:"max_viewers"`

	// Nonce identifies the attempt to start a share that produced this
	// state. The parent generates one per `bitbang share`, hands it to
	// the worker, and accepts back only state carrying it -- otherwise a
	// file left behind by a worker that was killed reads as the new
	// one's, and a dead share's URLs get printed as live.
	Nonce string `json:"nonce,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	// ExpiresAt zero means "until stopped" (--ttl 0).
	ExpiresAt time.Time `json:"expires_at,omitzero"`
	// TTLSeconds is the lifetime the share was started with (0 = until
	// stopped). Kept alongside ExpiresAt so re-running `share` can tell
	// a matching request from a changed one without doing arithmetic on
	// a clock that has since moved.
	TTLSeconds int `json:"ttl_seconds"`
}

// BaseDir is where per-share state directories live.
func BaseDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".bitbang", "shares"), nil
}

// Dir returns the state directory for a target hash.
func Dir(hash string) (string, error) {
	base, err := BaseDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, hash), nil
}

// LockPathFor returns the lifecycle lock beside a target's state
// directory.
//
// Beside it, not inside it: cleanup removes that directory while the
// lock is held, and a lock file that gets deleted excludes nobody -- the
// next process creates a fresh file at the same path and locks that
// instead. Naming it from the directory is what lets the sweep, which
// finds targets by reading the directory listing, lock each one without
// having to recompute a hash it never saw.
func LockPathFor(stateDir string) string { return stateDir + ".lock" }

// LockPath returns the lifecycle lock for a target hash.
func LockPath(hash string) (string, error) {
	dir, err := Dir(hash)
	if err != nil {
		return "", err
	}
	return LockPathFor(dir), nil
}

// TargetHash derives the stable directory / management-session key for
// a share target from the tmux socket and session ID.
func TargetHash(socket, sessionID string) string {
	sum := sha256.Sum256([]byte(socket + "\x00" + sessionID))
	return hex.EncodeToString(sum[:12])
}

// PrepareDir creates a share's state directory and proves it is
// writable.
//
// A worker's last words go in this directory (see SaveStartupError),
// which makes an unwritable one the one failure it cannot report on its
// own -- a read-only home, a full disk, a mode nobody meant to set. Its
// pane dies with it and takes the log too. Establishing the directory
// in the parent moves that whole class somewhere it can still be said
// out loud, before any worker is spawned to fail silently.
func PrepareDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	// MkdirAll is satisfied by a directory that already exists with a
	// mode that forbids writing to it, so ask for what is actually
	// needed rather than for its name.
	f, err := os.CreateTemp(dir, ".probe-*")
	if err != nil {
		return err
	}
	name := f.Name()
	_ = f.Close()
	return os.Remove(name)
}

const stateFile = "state.json"

// startupErrorFile holds a worker's last words when it dies before it
// can serve. Its pane goes with it, so this is the only place the
// parent can still read them from.
const startupErrorFile = "startup-error"

// SaveStartupError records why a worker gave up, stamped with the nonce
// of the start it belongs to. Best-effort: the caller is already failing
// and has nothing better to do with a second error.
func SaveStartupError(dir, nonce, msg string) {
	if dir == "" || msg == "" {
		return
	}
	_ = os.MkdirAll(dir, 0o700)
	_ = os.WriteFile(filepath.Join(dir, startupErrorFile), []byte(nonce+"\n"+msg), 0o600)
}

// TakeStartupError removes any recorded startup error and returns it
// only when it belongs to this start.
//
// The file has one name per target, so a worker whose parent was killed
// before it could read the file leaves its explanation lying where the
// next start would find it and report it as its own. The nonce is what
// makes that impossible; removing it either way is what stops it
// accumulating.
func TakeStartupError(dir, nonce string) string {
	path := filepath.Join(dir, startupErrorFile)
	data, err := os.ReadFile(path)
	_ = os.Remove(path)
	if err != nil {
		return ""
	}
	stamp, msg, ok := strings.Cut(string(data), "\n")
	if !ok || stamp != nonce {
		return ""
	}
	return strings.TrimSpace(msg)
}

// LoadState reads a share's state. Returns (nil, nil) when no state
// exists; a non-nil error means the file exists but can't be parsed
// (callers treat that as stale and clean it up).
func LoadState(dir string) (*State, error) {
	data, err := os.ReadFile(filepath.Join(dir, stateFile))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var st State
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, fmt.Errorf("corrupt share state: %w", err)
	}
	return &st, nil
}

// SaveState writes the state atomically with credential-grade permissions.
func SaveState(dir string, st *State) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	out, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(dir, stateFile)
	tmp, err := os.CreateTemp(dir, ".state-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(out); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// RemoveState deletes a share's state directory.
func RemoveState(dir string) error {
	return os.RemoveAll(dir)
}

// NewNonce returns a value naming one attempt to start a share. Same
// shape as an access code and generated the same way, but it authorises
// nothing: it travels on the worker's command line, where anyone who
// could read it could already read the state file it protects.
func NewNonce() (string, error) { return NewAccessCode() }

// NewAccessCode returns a fresh 64-bit access code in the same shape
// as identity access codes: 8 random bytes, base64url, 11 characters.
func NewAccessCode() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}
