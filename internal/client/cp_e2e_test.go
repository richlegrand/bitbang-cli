package client

// e2e tests for the `cp` data path (Session.Get / Session.Put), backed by a
// real file-stream listener over a temp share.

import (
	"bytes"
	mathrand "math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/richlegrand/bitbang/internal/fileshare"
	"github.com/richlegrand/bitbang/internal/protocol"
	"github.com/richlegrand/bitbang/internal/streamtype"
)

// fileSession wires a file-stream listener over shareDir and returns a connected
// connector Session. Listener + session teardown is registered via t.Cleanup.
func fileSession(t *testing.T, shareDir string, uploadEnabled bool) *Session {
	t.Helper()
	id := ephemeralID(t)
	share, err := fileshare.New(shareDir)
	if err != nil {
		t.Fatalf("fileshare.New: %v", err)
	}
	share.UploadEnabled = uploadEnabled // mirrors the listener's --upload flag

	relay := newFakeSignaling()
	t.Cleanup(relay.Close)
	startListener(relay.host(), id, streamtype.NewFile(share, false))
	waitRegistered(t, relay)

	sess := mustDial(t, relay.host(), id, "file")
	t.Cleanup(func() { sess.Close() })
	return sess
}

// TestSession_CopyFile_GetAndPut exercises the `cp` data path end-to-end: the
// connector's Session.Get / Session.Put — the same calls cmd/bitbang/cp.go makes
// for `cp <url>:/f -` (download) and `cp - <url>:/f` (upload).
func TestSession_CopyFile_GetAndPut(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: spins up real pion peer connections")
	}
	shareDir := t.TempDir()
	const want = "cp round-trip payload 9b2c\n"
	if err := os.WriteFile(filepath.Join(shareDir, "hello.txt"), []byte(want), 0o644); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	sess := fileSession(t, shareDir, true)

	// Download (Get): cp <url>:/hello.txt -
	var got bytes.Buffer
	info, err := sess.Get("/hello.txt", &got)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.String() != want {
		t.Errorf("Get content = %q, want %q", got.String(), want)
	}
	if info.Size != int64(len(want)) {
		t.Errorf("Get FileInfo.Size = %d, want %d", info.Size, len(want))
	}

	// Upload (Put): cp - <url>:/uploaded.txt
	const up = "uploaded via Put f00d\n"
	if err := sess.Put("/uploaded.txt", strings.NewReader(up), false); err != nil {
		t.Fatalf("Put: %v", err)
	}
	onDisk, err := os.ReadFile(filepath.Join(shareDir, "uploaded.txt"))
	if err != nil {
		t.Fatalf("read uploaded file: %v", err)
	}
	if string(onDisk) != up {
		t.Errorf("uploaded file = %q, want %q", string(onDisk), up)
	}
}

// TestSession_CopyFile_ChunkedGet downloads a file several times larger than
// protocol.MaxChunkSize, forcing the listener to emit multiple DAT frames and
// the connector to reassemble them. The not-a-multiple size also exercises a
// short final frame, and the seeded-random content makes an exact bytes.Equal
// catch any truncation, reorder, or corruption a single-chunk test would miss.
func TestSession_CopyFile_ChunkedGet(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: spins up real pion peer connections")
	}
	shareDir := t.TempDir()
	want := make([]byte, protocol.MaxChunkSize*8+123) // 8 full chunks + a partial one
	mathrand.New(mathrand.NewSource(42)).Read(want)
	if err := os.WriteFile(filepath.Join(shareDir, "big.bin"), want, 0o644); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	sess := fileSession(t, shareDir, false)

	var got bytes.Buffer
	info, err := sess.Get("/big.bin", &got)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if info.Size != int64(len(want)) {
		t.Errorf("FileInfo.Size = %d, want %d", info.Size, len(want))
	}
	if got.Len() != len(want) {
		t.Fatalf("got %d bytes, want %d (chunk reassembly truncated or over-read)", got.Len(), len(want))
	}
	if !bytes.Equal(got.Bytes(), want) {
		t.Error("downloaded bytes differ from source across chunk boundaries (corruption/reorder)")
	}
}

// TestSession_CopyFile_ChunkedPut uploads a file larger than protocol.MaxChunkSize,
// forcing the connector to send multiple DAT frames and the listener to
// reassemble them onto disk. Exact bytes.Equal against the source catches any
// chunk-boundary bug on the upload path.
func TestSession_CopyFile_ChunkedPut(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: spins up real pion peer connections")
	}
	shareDir := t.TempDir()
	sess := fileSession(t, shareDir, true)

	want := make([]byte, protocol.MaxChunkSize*8+123) // 8 full chunks + a partial one
	mathrand.New(mathrand.NewSource(7)).Read(want)
	if err := sess.Put("/big-up.bin", bytes.NewReader(want), false); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(shareDir, "big-up.bin"))
	if err != nil {
		t.Fatalf("read uploaded file: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("uploaded %d bytes, want %d", len(got), len(want))
	}
	if !bytes.Equal(got, want) {
		t.Error("uploaded bytes differ from source across chunk boundaries (corruption/reorder)")
	}
}

// TestSession_CopyFile_GetMissing asserts a download of a nonexistent path
// surfaces the listener's "not found" error rather than hanging or succeeding.
func TestSession_CopyFile_GetMissing(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: spins up real pion peer connections")
	}
	sess := fileSession(t, t.TempDir(), false)

	var buf bytes.Buffer
	_, err := sess.Get("/does-not-exist.txt", &buf)
	if err == nil {
		t.Fatal("Get of missing file: want error, got nil")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error = %q, want to contain \"not found\"", err)
	}
}

// TestSession_CopyFile_UploadDisabled asserts a Put against a listener started
// without --upload is rejected (and nothing is written).
func TestSession_CopyFile_UploadDisabled(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: spins up real pion peer connections")
	}
	shareDir := t.TempDir()
	sess := fileSession(t, shareDir, false) // uploads OFF

	err := sess.Put("/blocked.txt", strings.NewReader("nope"), false)
	if err == nil {
		t.Fatal("Put with uploads disabled: want error, got nil")
	}
	if !strings.Contains(err.Error(), "not enabled") {
		t.Errorf("error = %q, want to contain \"not enabled\"", err)
	}
	if _, statErr := os.Stat(filepath.Join(shareDir, "blocked.txt")); statErr == nil {
		t.Error("file was written despite uploads being disabled")
	}
}

// TestSession_CopyFile_OverwriteConflict asserts Put with overwrite=false over
// an existing path is rejected and leaves the original intact, while
// overwrite=true replaces it.
func TestSession_CopyFile_OverwriteConflict(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: spins up real pion peer connections")
	}
	shareDir := t.TempDir()
	const original = "original contents\n"
	if err := os.WriteFile(filepath.Join(shareDir, "exists.txt"), []byte(original), 0o644); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	sess := fileSession(t, shareDir, true)

	// overwrite=false → rejected, original untouched.
	err := sess.Put("/exists.txt", strings.NewReader("new contents"), false)
	if err == nil {
		t.Fatal("Put overwrite=false over existing file: want error, got nil")
	}
	if !strings.Contains(err.Error(), "exists") {
		t.Errorf("error = %q, want to contain \"exists\"", err)
	}
	if cur, _ := os.ReadFile(filepath.Join(shareDir, "exists.txt")); string(cur) != original {
		t.Errorf("file changed despite rejected overwrite: %q, want %q", cur, original)
	}

	// overwrite=true → replaces.
	const replaced = "replaced contents\n"
	if err := sess.Put("/exists.txt", strings.NewReader(replaced), true); err != nil {
		t.Fatalf("Put overwrite=true: %v", err)
	}
	if cur, _ := os.ReadFile(filepath.Join(shareDir, "exists.txt")); string(cur) != replaced {
		t.Errorf("after overwrite=true: %q, want %q", cur, replaced)
	}
}
