package share

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// fakeRunner records the tmux command lines it is asked to run and
// replays canned responses, so share's control flow can be exercised
// without a tmux server.
type fakeRunner struct {
	socket  string
	calls   [][]string
	replies map[string]string
	errs    map[string]error
}

func newFakeRunner() *fakeRunner {
	return &fakeRunner{replies: map[string]string{}, errs: map[string]error{}}
}

func (f *fakeRunner) Socket() string { return f.socket }

func (f *fakeRunner) Run(args ...string) (string, error) {
	f.calls = append(f.calls, args)
	key := args[0]
	if err, ok := f.errs[key]; ok {
		return "", err
	}
	return f.replies[key], nil
}

func TestCheckVersion(t *testing.T) {
	tests := []struct {
		version string
		wantErr bool
	}{
		{"tmux 3.2", false},
		{"tmux 3.2a", false},
		{"tmux 3.4", false},
		{"tmux 4.0", false},
		{"tmux 10.1", false},
		{"tmux 3.1b", true},
		{"tmux 2.8", true},
		{"tmux next-3.6", false}, // parses as 3.6
		{"tmux master", false},   // unparseable -> assume new enough
	}
	for _, tc := range tests {
		r := newFakeRunner()
		r.replies["-V"] = tc.version
		err := CheckVersion(r)
		if tc.wantErr && err == nil {
			t.Errorf("CheckVersion(%q): got nil, want error", tc.version)
		}
		if !tc.wantErr && err != nil {
			t.Errorf("CheckVersion(%q): got %v, want nil", tc.version, err)
		}
	}
}

func TestCheckVersionPropagatesRunError(t *testing.T) {
	r := newFakeRunner()
	r.errs["-V"] = errors.New("no server running")
	if err := CheckVersion(r); err == nil {
		t.Fatal("got nil error, want the runner's error")
	}
}

func TestSocketFromEnv(t *testing.T) {
	t.Setenv("TMUX", "/tmp/tmux-501/default,12345,0")
	if got := SocketFromEnv(); got != "/tmp/tmux-501/default" {
		t.Errorf("got %q, want the socket path", got)
	}
	t.Setenv("TMUX", "")
	if got := SocketFromEnv(); got != "" {
		t.Errorf("outside tmux: got %q, want empty", got)
	}
}

func TestDiscoverInsideTmux(t *testing.T) {
	t.Setenv("TMUX", "/tmp/sock,1,0")
	r := newFakeRunner()
	r.replies["display-message"] = "$4\twork\t/tmp/sock"

	tgt, err := Discover(r, "")
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if tgt.SessionID != "$4" || tgt.SessionName != "work" {
		t.Errorf("got %+v, want session $4 named work", tgt)
	}
	// Without an explicit target, the pane must be resolved from the
	// environment -- passing -t would target the wrong session.
	for _, a := range r.calls[0] {
		if a == "-t" {
			t.Error("Discover with no --target passed -t to tmux")
		}
	}
}

func TestDiscoverOutsideTmuxRequiresTarget(t *testing.T) {
	t.Setenv("TMUX", "")
	r := newFakeRunner()
	if _, err := Discover(r, ""); err == nil {
		t.Fatal("got nil error, want a demand for --target")
	}
	if len(r.calls) != 0 {
		t.Errorf("ran tmux %v before failing; should fail without invoking tmux", r.calls)
	}

	r.replies["display-message"] = "$7\tother\t/tmp/sock"
	tgt, err := Discover(r, "other")
	if err != nil {
		t.Fatalf("Discover with explicit target: %v", err)
	}
	if tgt.SessionID != "$7" {
		t.Errorf("got session %q, want $7", tgt.SessionID)
	}
}

// TestDiscoverUsesServerReportedSocket: a share is keyed on the socket
// path, and one server answers to several spellings. Taking tmux's own
// answer is what stops `share status` from outside tmux keying a
// different share than `share` did from inside it.
func TestDiscoverUsesServerReportedSocket(t *testing.T) {
	t.Setenv("TMUX", "/tmp/sock,1,0")
	r := newFakeRunner()
	r.socket = "" // reached via tmux's default socket
	r.replies["display-message"] = "$4\twork\t/private/tmp/tmux-501/default"

	tgt, err := Discover(r, "")
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if tgt.Socket != "/private/tmp/tmux-501/default" {
		t.Errorf("Socket = %q, want the path tmux reported", tgt.Socket)
	}
}

// TestDiscoverRefusesForeignSocketWithoutTarget: with --socket pointing
// at another server and no --target, display-message would resolve that
// server's current session -- publishing, read-write, a session the user
// is not looking at.
func TestDiscoverRefusesForeignSocketWithoutTarget(t *testing.T) {
	t.Setenv("TMUX", "/tmp/serverA,1,0")
	r := newFakeRunner()
	r.socket = "/tmp/serverB"
	r.replies["display-message"] = "$9\tsomething-else\t/tmp/serverB"

	if _, err := Discover(r, ""); err == nil {
		t.Fatal("Discover resolved against a different server than the caller is in")
	}
	// With an explicit target the user has said which session they mean.
	if _, err := Discover(r, "other"); err != nil {
		t.Errorf("Discover with an explicit target: %v", err)
	}
}

func TestDiscoverRejectsUnparseableOutput(t *testing.T) {
	t.Setenv("TMUX", "/tmp/sock,1,0")
	r := newFakeRunner()
	r.replies["display-message"] = "no session id here"
	if _, err := Discover(r, ""); err == nil {
		t.Fatal("got nil error, want rejection of output without a $id")
	}
}

func TestAttachEnvStripsTmuxVars(t *testing.T) {
	in := []string{
		"PATH=/usr/bin",
		"TMUX=/tmp/sock,1,0",
		"TMUX_PANE=%3",
		"TERM=screen-256color",
		"HOME=/home/x",
	}
	got := AttachEnv(in)
	for _, kv := range got {
		if strings.HasPrefix(kv, "TMUX=") || strings.HasPrefix(kv, "TMUX_PANE=") {
			t.Errorf("AttachEnv kept %q -- tmux refuses to attach with $TMUX set", kv)
		}
	}
	if !contains(got, "TERM=screen-256color") {
		t.Error("AttachEnv dropped an existing TERM")
	}
	if !contains(got, "PATH=/usr/bin") || !contains(got, "HOME=/home/x") {
		t.Errorf("AttachEnv dropped unrelated variables: %v", got)
	}
}

// TestAttachEnvReplacesUnusableTerm: tmux refuses to attach under a
// capability-free TERM, so passing one through means every peer's
// attach exits the moment it starts.
func TestAttachEnvReplacesUnusableTerm(t *testing.T) {
	for _, bad := range []string{"dumb", "unknown", ""} {
		got := AttachEnv([]string{"PATH=/usr/bin", "TERM=" + bad})
		if !contains(got, "TERM=xterm-256color") {
			t.Errorf("TERM=%q: got %v, want the default substituted", bad, got)
		}
		if bad != "" && contains(got, "TERM="+bad) {
			t.Errorf("TERM=%q was passed through; tmux cannot attach under it", bad)
		}
	}
	// A usable TERM is still the operator's to choose.
	got := AttachEnv([]string{"TERM=screen-256color"})
	if !contains(got, "TERM=screen-256color") {
		t.Errorf("got %v, want a usable TERM preserved", got)
	}
}

func TestAttachEnvDefaultsTerm(t *testing.T) {
	got := AttachEnv([]string{"PATH=/usr/bin"})
	if !contains(got, "TERM=xterm-256color") {
		t.Errorf("got %v, want a defaulted TERM", got)
	}
	// An empty TERM is as useless as a missing one.
	got = AttachEnv([]string{"TERM="})
	if !contains(got, "TERM=xterm-256color") {
		t.Errorf("got %v, want empty TERM replaced", got)
	}
	if contains(got, "TERM=") {
		t.Errorf("got %v, want the empty TERM removed", got)
	}
}

func contains(hay []string, needle string) bool {
	for _, s := range hay {
		if s == needle {
			return true
		}
	}
	return false
}

func TestShellQuote(t *testing.T) {
	tests := map[string]string{
		"":                    "''",
		"plain":               "'plain'",
		"/path/with space":    "'/path/with space'",
		"it's":                `'it'\''s'`,
		"; rm -rf /":          "'; rm -rf /'",
		"$(whoami)":           "'$(whoami)'",
		"`id`":                "'`id`'",
		"a'; touch /tmp/x; '": `'a'\''; touch /tmp/x; '\'''`,
	}
	for in, want := range tests {
		if got := ShellQuote(in); got != want {
			t.Errorf("ShellQuote(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTargetHashDistinguishesSocketAndSession(t *testing.T) {
	a := TargetHash("/tmp/a", "$1")
	b := TargetHash("/tmp/a", "$2")
	c := TargetHash("/tmp/b", "$1")
	if a == b || a == c || b == c {
		t.Errorf("hashes collide: %q %q %q", a, b, c)
	}
	if a != TargetHash("/tmp/a", "$1") {
		t.Error("TargetHash is not stable across calls")
	}
	// Must be usable as a directory and tmux session-name component.
	if strings.ContainsAny(a, "/. \t") {
		t.Errorf("hash %q contains characters unsafe for paths/session names", a)
	}
	if len(a) != 24 {
		t.Errorf("hash length = %d, want 24 hex characters", len(a))
	}
}

func TestStateRoundTripAndPermissions(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "shares", "abc123")
	want := &State{
		Socket:      "/tmp/sock",
		SessionID:   "$2",
		SessionName: "work",
		MgmtSession: "_bbshare_abc123",
		UID:         "uid-xyz",
		Server:      "bitba.ng",
		ControlURL:  "https://bitba.ng/uid-xyz#ctrl!ephemeral",
		ViewURL:     "https://bitba.ng/uid-xyz#view!ephemeral",
		MaxViewers:  16,
		CreatedAt:   time.Now().Truncate(time.Second),
		ExpiresAt:   time.Now().Add(time.Hour).Truncate(time.Second),
		TTLSeconds:  3600,
	}
	if err := SaveState(dir, want); err != nil {
		t.Fatalf("SaveState: %v", err)
	}

	if runtime.GOOS != "windows" {
		fi, err := os.Stat(filepath.Join(dir, "state.json"))
		if err != nil {
			t.Fatalf("stat: %v", err)
		}
		if perm := fi.Mode().Perm(); perm != 0o600 {
			t.Errorf("state.json mode = %o, want 600", perm)
		}
		di, err := os.Stat(dir)
		if err != nil {
			t.Fatalf("stat dir: %v", err)
		}
		if perm := di.Mode().Perm(); perm != 0o700 {
			t.Errorf("state dir mode = %o, want 700", perm)
		}
	}

	got, err := LoadState(dir)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if got.ControlURL != want.ControlURL || got.ViewURL != want.ViewURL ||
		got.SessionID != want.SessionID || got.MaxViewers != want.MaxViewers ||
		got.TTLSeconds != want.TTLSeconds {
		t.Errorf("round trip mismatch:\n got %+v\nwant %+v", got, want)
	}
	if !got.ExpiresAt.Equal(want.ExpiresAt) {
		t.Errorf("ExpiresAt: got %v, want %v", got.ExpiresAt, want.ExpiresAt)
	}

	// No leftover temp file from the atomic write.
	if matches, err := filepath.Glob(filepath.Join(dir, ".state-*.tmp")); err != nil || len(matches) != 0 {
		t.Errorf("temporary state files remain after SaveState: %v (glob error: %v)", matches, err)
	}
}

func TestLoadStateMissingAndCorrupt(t *testing.T) {
	dir := t.TempDir()
	st, err := LoadState(dir)
	if err != nil || st != nil {
		t.Fatalf("missing state: got (%v, %v), want (nil, nil)", st, err)
	}

	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadState(dir); err == nil {
		t.Fatal("corrupt state: got nil error, want a parse failure so callers clean it up")
	}
}

func TestSaveStateOverwrites(t *testing.T) {
	dir := t.TempDir()
	if err := SaveState(dir, &State{ViewURL: "first", SessionID: "$1"}); err != nil {
		t.Fatal(err)
	}
	if err := SaveState(dir, &State{ViewURL: "second", SessionID: "$1"}); err != nil {
		t.Fatal(err)
	}
	got, err := LoadState(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got.ViewURL != "second" {
		t.Errorf("got %q, want the second write to win", got.ViewURL)
	}
}

func TestRemoveState(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "s")
	if err := SaveState(dir, &State{ViewURL: "u", SessionID: "$1"}); err != nil {
		t.Fatal(err)
	}
	if err := RemoveState(dir); err != nil {
		t.Fatalf("RemoveState: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Error("state directory survived RemoveState")
	}
	// Removing a share that was never started must not error.
	if err := RemoveState(dir); err != nil {
		t.Errorf("second RemoveState: %v", err)
	}
}

func TestNewAccessCodeShape(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		code, err := NewAccessCode()
		if err != nil {
			t.Fatalf("NewAccessCode: %v", err)
		}
		// 8 random bytes, base64url, no padding -- same shape as an
		// identity access code.
		if len(code) != 11 {
			t.Fatalf("code %q has length %d, want 11", code, len(code))
		}
		if strings.ContainsAny(code, "+/=") {
			t.Fatalf("code %q is not URL-safe base64", code)
		}
		if seen[code] {
			t.Fatalf("duplicate code %q across draws", code)
		}
		seen[code] = true
	}
}

// TestPrepareDirRefusesUnwritable: the parent establishes the state
// directory precisely so an unwritable one is reported by something
// that can still speak. A worker that discovers it has nowhere to write
// its last words has nowhere to write them.
func TestPrepareDirRefusesUnwritable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not enforce Unix mode bits")
	}
	if os.Getuid() == 0 {
		t.Skip("root writes through any mode")
	}
	parent := t.TempDir()
	dir := filepath.Join(parent, "shares")
	if err := os.Mkdir(dir, 0o500); err != nil {
		t.Fatal(err)
	}

	// MkdirAll alone is satisfied here -- the directory exists. Only
	// trying to write says otherwise.
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("precondition: MkdirAll should succeed on an existing dir: %v", err)
	}
	if err := PrepareDir(dir); err == nil {
		t.Error("PrepareDir accepted a directory it cannot write to")
	}
}

func TestPrepareDirCreatesAndLeavesNothing(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "a", "b")
	if err := PrepareDir(dir); err != nil {
		t.Fatalf("PrepareDir: %v", err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o700 {
		perm := info.Mode().Perm()
		t.Errorf("mode = %o, want 700 for a directory holding credentials", perm)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("PrepareDir left %d files behind; the write probe must clean up", len(entries))
	}
}

// TestStartupErrorRoundTrip: a worker that dies before it can serve
// takes its pane with it, so this file is the only copy of why.
func TestStartupErrorRoundTrip(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")

	if got := TakeStartupError(dir, "n1"); got != "" {
		t.Errorf("TakeStartupError on a missing directory = %q, want empty", got)
	}

	SaveStartupError(dir, "n1", "dial bitba.ng: no such host\n")
	got := TakeStartupError(dir, "n1")
	if got != "dial bitba.ng: no such host" {
		t.Errorf("TakeStartupError = %q, want the worker's message, trimmed", got)
	}

	// Taken means taken: a later failure must not be explained by an
	// earlier one's leftovers.
	if again := TakeStartupError(dir, "n1"); again != "" {
		t.Errorf("TakeStartupError left %q behind for the next start to misreport", again)
	}
}

// TestStartupErrorIsScopedToItsStart: the file has one name per target,
// so a worker whose parent was killed before reading it leaves its
// explanation where the next start would find it. Reporting another
// attempt's failure as your own sends the operator after the wrong
// thing entirely.
func TestStartupErrorIsScopedToItsStart(t *testing.T) {
	dir := t.TempDir()
	SaveStartupError(dir, "an-older-start", "the reason that start failed")

	if got := TakeStartupError(dir, "this-start"); got != "" {
		t.Errorf("TakeStartupError = %q, which belongs to a different start", got)
	}
	// Removed all the same, or it waits for the start whose nonce it
	// happens to match -- which is never.
	if _, err := os.Stat(filepath.Join(dir, "startup-error")); !os.IsNotExist(err) {
		t.Error("a foreign startup error was left behind to accumulate")
	}
}

// TestSaveStartupErrorPermissions: the message can name a hostname or a
// path, and it sits in the same directory as the credential file.
func TestSaveStartupErrorPermissions(t *testing.T) {
	dir := t.TempDir()
	SaveStartupError(dir, "n1", "boom")
	info, err := os.Stat(filepath.Join(dir, "startup-error"))
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		perm := info.Mode().Perm()
		t.Errorf("mode = %o, want 600", perm)
	}
}
