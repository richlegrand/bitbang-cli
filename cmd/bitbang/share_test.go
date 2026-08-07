//go:build unix

package main

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/richlegrand/bitbang/internal/share"
)

func TestParseTTL(t *testing.T) {
	tests := []struct {
		in      string
		want    time.Duration
		wantErr bool
	}{
		{"1h", time.Hour, false},
		{"30m", 30 * time.Minute, false},
		{"90s", 90 * time.Second, false},
		{"1s", time.Second, false},
		{"0", 0, false},  // explicit "until stopped"
		{"", 0, false},   // unset behaves like 0
		{"-5m", 0, true}, // a share that expired before it started is a bug, not a feature
		{"forever", 0, true},
		{"10", 0, true}, // unitless is ambiguous -- reject rather than guess
		// Sub-second values must be refused, not rounded: the worker
		// takes whole seconds, so truncation would turn a short share
		// into a permanent one -- the opposite of the request.
		{"500ms", 0, true},
		{"1ns", 0, true},
		// Fractional values above a second are truncated by the same
		// whole-seconds conversion, so 1500ms would quietly become 1s.
		{"1500ms", 0, true},
		{"90500ms", 0, true},
		{"1.5s", 0, true},
		// Absurdly long values are refused so the seconds count stays
		// inside an int on 32-bit builds.
		{"9000h", 0, true},
		{"2000000h", 0, true},
	}
	for _, tc := range tests {
		got, err := parseTTL(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("parseTTL(%q): got nil error, want rejection", tc.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseTTL(%q): %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("parseTTL(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// TestParseConnectURLEphemeral: a share URL must be recognized as
// ephemeral so connect never writes it to devices.json, while ordinary
// device URLs keep saving.
func TestParseConnectURLEphemeral(t *testing.T) {
	tests := []struct {
		arg           string
		wantCode      string
		wantEphemeral bool
	}{
		{"https://bitba.ng/UID123#CODE456!ephemeral", "CODE456", true},
		{"https://bitba.ng/UID123#CODE456", "CODE456", false},
		{"bitba.ng/UID123#CODE456!ephemeral", "CODE456", true},
		{"UID123#CODE456!ephemeral", "CODE456", true},
		{"https://bitba.ng/UID123#CODE456!debug", "CODE456", false},
		{"https://bitba.ng/UID123#CODE456!debug,ephemeral", "CODE456", true},
		{"https://bitba.ng/UID123#CODE456!ephemeral,debug", "CODE456", true},
	}
	for _, tc := range tests {
		rs, ok := parseConnectURL(tc.arg)
		if !ok {
			t.Errorf("parseConnectURL(%q): not parsed", tc.arg)
			continue
		}
		if rs.Code != tc.wantCode {
			t.Errorf("parseConnectURL(%q) code = %q, want %q", tc.arg, rs.Code, tc.wantCode)
		}
		if rs.Ephemeral != tc.wantEphemeral {
			t.Errorf("parseConnectURL(%q) ephemeral = %v, want %v", tc.arg, rs.Ephemeral, tc.wantEphemeral)
		}
		if rs.UID != "UID123" {
			t.Errorf("parseConnectURL(%q) uid = %q, want UID123", tc.arg, rs.UID)
		}
	}
}

func TestParseRemoteSpecEphemeral(t *testing.T) {
	rs, ok := parseRemoteSpec("https://bitba.ng/UID123#CODE456!ephemeral:/tmp/file")
	if !ok {
		t.Fatal("parseRemoteSpec: not parsed")
	}
	if rs.Code != "CODE456" || !rs.Ephemeral || rs.Path != "/tmp/file" {
		t.Errorf("got %+v, want code CODE456, ephemeral, path /tmp/file", rs)
	}

	rs, ok = parseRemoteSpec("https://bitba.ng/UID123#CODE456:/tmp/file")
	if !ok {
		t.Fatal("parseRemoteSpec (plain): not parsed")
	}
	if rs.Ephemeral {
		t.Error("a plain device URL was marked ephemeral")
	}
}

// TestParseTTLNeverSilentlyMeansForever is the property behind the
// sub-second rejection: every accepted TTL must survive the conversion
// to whole seconds that gets handed to the worker, where 0 is the
// "until stopped" sentinel.
func TestParseTTLNeverSilentlyMeansForever(t *testing.T) {
	for _, in := range []string{"1s", "20s", "90s", "30m", "1h", "8h", "720h", "1h30m"} {
		d, err := parseTTL(in)
		if err != nil {
			t.Errorf("parseTTL(%q): %v", in, err)
			continue
		}
		secs := int(d / time.Second)
		if secs <= 0 {
			t.Errorf("parseTTL(%q) = %v, which becomes %d seconds -- the worker would read that as no expiry", in, d, secs)
		}
		// Whole-second conversion must be lossless, or the share would
		// expire at a different time than the one requested.
		if got := time.Duration(secs) * time.Second; got != d {
			t.Errorf("parseTTL(%q) = %v but reaches the worker as %v", in, d, got)
		}
	}
}

func TestShareConflicts(t *testing.T) {
	running := &share.State{
		ControlURL: "https://bitba.ng/uid#ctrl!ephemeral",
		ViewURL:    "https://bitba.ng/uid#view!ephemeral",
		MaxViewers: 16,
		Server:     "bitba.ng",
		TTLSeconds: 3600,
	}
	readOnlyRunning := &share.State{
		ViewURL:    "https://bitba.ng/uid#view!ephemeral",
		MaxViewers: 16,
		Server:     "bitba.ng",
		TTLSeconds: 3600,
	}

	cfg := func(set map[string]bool, mutate func(*shareConfig)) shareConfig {
		c := shareConfig{ttl: "1h", ttlDuration: time.Hour, maxViewers: 16, server: "bitba.ng", set: set}
		if mutate != nil {
			mutate(&c)
		}
		return c
	}

	tests := []struct {
		name    string
		cfg     shareConfig
		state   *share.State
		wantAny bool
	}{
		{"bare re-run reprints", cfg(map[string]bool{}, nil), running, false},
		{"read-only against a control share", cfg(map[string]bool{"read-only": true},
			func(c *shareConfig) { c.readOnly = true }), running, true},
		{"read-only against a read-only share", cfg(map[string]bool{"read-only": true},
			func(c *shareConfig) { c.readOnly = true }), readOnlyRunning, false},
		{"control wanted from a read-only share", cfg(map[string]bool{"read-only": true},
			func(c *shareConfig) { c.readOnly = false }), readOnlyRunning, true},
		{"different ttl", cfg(map[string]bool{"ttl": true},
			func(c *shareConfig) { c.ttl, c.ttlDuration = "30m", 30*time.Minute }), running, true},
		{"same ttl restated", cfg(map[string]bool{"ttl": true}, nil), running, false},
		{"different max-viewers", cfg(map[string]bool{"max-viewers": true},
			func(c *shareConfig) { c.maxViewers = 2 }), running, true},
		{"different server", cfg(map[string]bool{"server": true},
			func(c *shareConfig) { c.server = "test.bitba.ng" }), running, true},
		// A flag that was never typed must not be compared, or every
		// default would argue with a share started using other flags.
		{"untyped flags ignored", cfg(map[string]bool{},
			func(c *shareConfig) { c.readOnly, c.maxViewers = true, 2 }), running, false},
	}

	for _, tc := range tests {
		got := shareConflicts(tc.cfg, tc.state)
		if (len(got) > 0) != tc.wantAny {
			t.Errorf("%s: conflicts = %v, want any = %v", tc.name, got, tc.wantAny)
		}
	}
}

// fakeShareRunner answers tmux probes from a script, so the liveness
// classification can be exercised without a tmux server.
type fakeShareRunner struct {
	sessionAlive bool
	paneDead     bool
	// probeErr, when set, is what a probe returns instead of an answer --
	// a tmux that could not be run at all.
	probeErr error
	// serverUnreachable makes the target-independent probe fail too,
	// which is how a live server behind an inaccessible socket looks:
	// tmux exits non-zero for everything, and none of it is an answer.
	serverUnreachable bool
	// dieAfterProbes models a worker that exits once asked to stop:
	// after this many liveness probes the session reports gone. Zero
	// means it never exits on its own.
	dieAfterProbes int
	probes         int
	calls          []string

	// mgmt is the management session name the listing reports, matched
	// locally by the classifier rather than by a tmux target.
	mgmt string

	// panePIDs are the successive answers to #{pane_pid}, the last one
	// repeating -- a second, different value is how a test models the
	// kernel reusing the number after the worker exits. The default
	// answers 1, which the stop path refuses to signal, so a test never
	// aims a signal at a real process by accident.
	panePIDs []int
	pidReads int
}

func (f *fakeShareRunner) Socket() string { return "" }

func (f *fakeShareRunner) Run(args ...string) (string, error) {
	f.calls = append(f.calls, strings.Join(args, " "))
	switch args[0] {
	case "list-panes":
		// The classifier's one command: target-independent, so its
		// success is the whole answer and its failure is no answer at
		// all.
		if f.probeErr != nil {
			return "", f.probeErr
		}
		if f.serverUnreachable {
			return "", errors.New("tmux list-panes: error connecting to /tmp/x (Permission denied)")
		}
		f.probes++
		if f.dieAfterProbes > 0 && f.probes > f.dieAfterProbes {
			f.sessionAlive = false
		}
		// Something unrelated is always listed, so "our session is
		// absent" is distinguishable from "the listing was empty".
		lines := []string{"someone-elses-session\t0\t999"}
		if f.sessionAlive {
			dead := "0"
			if f.paneDead {
				dead = "1"
			}
			pid := 1
			if len(f.panePIDs) > 0 {
				i := f.pidReads
				if i >= len(f.panePIDs) {
					i = len(f.panePIDs) - 1
				}
				pid = f.panePIDs[i]
			}
			f.pidReads++
			lines = append(lines, f.mgmt+"\t"+dead+"\t"+strconv.Itoa(pid))
		}
		return strings.Join(lines, "\n"), nil
	case "kill-session":
		f.sessionAlive = false
	}
	return "", nil
}

// targetsUsed returns every -t argument the fake was asked for.
func (f *fakeShareRunner) targetsUsed() []string {
	var out []string
	for _, c := range f.calls {
		fields := strings.Fields(c)
		for i, fld := range fields {
			if fld == "-t" && i+1 < len(fields) {
				out = append(out, fields[i+1])
			}
		}
	}
	return out
}

func newLivenessTarget(t *testing.T, alive bool) (*shareTarget, *fakeShareRunner) {
	t.Helper()
	r := &fakeShareRunner{sessionAlive: alive, mgmt: "_bbshare_test"}
	base := t.TempDir()
	return &shareTarget{
		runner: r,
		target: share.Target{SessionID: "$1", SessionName: "work"},
		dir:    filepath.Join(base, "hash"),
		lock:   filepath.Join(base, "hash.lock"),
		mgmt:   "_bbshare_test",
	}, r
}

// TestLoadLiveShareClassification is the guard against deleting a live
// share's only record. State that cannot be read says nothing about
// whether the worker is running, so tmux is always asked.
func TestLoadLiveShareClassification(t *testing.T) {
	good := &share.State{SessionID: "$1", ViewURL: "https://x/y#z", UID: "u"}

	t.Run("readable state, worker running", func(t *testing.T) {
		st, _ := newLivenessTarget(t, true)
		if err := share.SaveState(st.dir, good); err != nil {
			t.Fatal(err)
		}
		if s, l := loadLiveShare(st); l != shareLive || s == nil {
			t.Errorf("got liveness %v, want shareLive with state", l)
		}
	})

	t.Run("readable state, worker gone", func(t *testing.T) {
		st, _ := newLivenessTarget(t, false)
		if err := share.SaveState(st.dir, good); err != nil {
			t.Fatal(err)
		}
		if _, l := loadLiveShare(st); l != shareStale {
			t.Errorf("got liveness %v, want shareStale", l)
		}
	})

	t.Run("corrupt state, worker running", func(t *testing.T) {
		st, r := newLivenessTarget(t, true)
		if err := os.MkdirAll(st.dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(st.dir, "state.json"), []byte("{broken"), 0o600); err != nil {
			t.Fatal(err)
		}
		_, l := loadLiveShare(st)
		if l != shareUnmanaged {
			t.Errorf("got liveness %v, want shareUnmanaged -- deleting here strips a live share's only record", l)
		}
		var probed bool
		for _, c := range r.calls {
			if strings.HasPrefix(c, "list-panes") {
				probed = true
			}
		}
		if !probed {
			t.Error("tmux was never asked; unreadable state was assumed dead")
		}
	})

	t.Run("corrupt state, worker gone", func(t *testing.T) {
		st, _ := newLivenessTarget(t, false)
		if err := os.MkdirAll(st.dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(st.dir, "state.json"), []byte("{broken"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, l := loadLiveShare(st); l != shareStale {
			t.Errorf("got liveness %v, want shareStale", l)
		}
	})

	t.Run("no state, worker running (orphan)", func(t *testing.T) {
		st, _ := newLivenessTarget(t, true)
		if _, l := loadLiveShare(st); l != shareUnmanaged {
			t.Errorf("got liveness %v, want shareUnmanaged for an orphaned management session", l)
		}
	})

	t.Run("no state, nothing running", func(t *testing.T) {
		st, _ := newLivenessTarget(t, false)
		if _, l := loadLiveShare(st); l != shareAbsent {
			t.Errorf("got liveness %v, want shareAbsent", l)
		}
	})
}

// tmux permits prefix matches for unanchored session targets. Commands that
// name the management session must use exact targets.
func TestMgmtTargetsAreExact(t *testing.T) {
	st, r := newLivenessTarget(t, true)
	r.paneDead = true // the husk path, which is what issues kill-session
	loadLiveShare(st)
	if err := cleanupStale(st); err != nil {
		t.Fatalf("cleanupStale: %v", err)
	}

	if len(r.targetsUsed()) == 0 {
		t.Fatal("expected at least one tmux target")
	}
	for _, tgt := range r.targetsUsed() {
		if !strings.HasPrefix(tgt, "=") {
			t.Errorf("target %q is not anchored; tmux would prefix-match it", tgt)
		}
	}
}

// remain-on-exit keeps a dead pane listed, but its PID must not be signaled.
func TestDeadPaneIsNotLive(t *testing.T) {
	st, r := newLivenessTarget(t, true)
	r.paneDead = true

	if got := probeMgmt(st); got != mgmtDead {
		t.Errorf("probeMgmt = %v, want mgmtDead for a session whose pane has exited", got)
	}
	if err := share.SaveState(st.dir, &share.State{SessionID: "$1", ViewURL: "u", UID: "x"}); err != nil {
		t.Fatal(err)
	}
	if _, l := loadLiveShare(st); l != shareStale {
		t.Errorf("liveness = %v, want shareStale for a dead pane", l)
	}

	// And nothing may be signalled. The PID comes back in the same
	// answer as the dead flag now, so the guarantee is not that we avoid
	// asking -- it is that a dead pane yields no PID to send anything to.
	sig := captureSignals(t)
	r.panePIDs = []int{4242}
	if !stopWorker(st) {
		t.Fatal("stopWorker failed to clean a dead management pane")
	}
	if len(sig.sent) != 0 {
		t.Errorf("signalled %v from a dead pane; that PID belongs to a process that already exited", sig.sent)
	}
}

// A retained dead pane keeps the management session name unavailable until
// cleanup removes the session.
func TestDeadMgmtSessionIsReaped(t *testing.T) {
	st, r := newLivenessTarget(t, true)
	r.paneDead = true
	if err := share.SaveState(st.dir, &share.State{SessionID: "$1", ViewURL: "u", UID: "x"}); err != nil {
		t.Fatal(err)
	}

	if err := cleanupStale(st); err != nil {
		t.Fatalf("cleanupStale: %v", err)
	}

	if r.sessionAlive {
		t.Error("the dead management session was left listed; the next `bitbang share` would fail on duplicate session")
	}
	killed := false
	for _, c := range r.calls {
		if strings.HasPrefix(c, "kill-session") {
			killed = true
			if !strings.Contains(c, "="+st.mgmt) {
				t.Errorf("kill-session target %q is not anchored", c)
			}
		}
	}
	if !killed {
		t.Error("no kill-session was issued for the dead management session")
	}
	if _, err := os.Stat(filepath.Join(st.dir, "state.json")); !os.IsNotExist(err) {
		t.Error("stale state survived cleanup")
	}
}

// TestLiveMgmtSessionIsNeverReaped is the other half: a session whose
// pane is alive holds a running worker, and killing it would take the
// worker's URLs away with no record left of them.
func TestLiveMgmtSessionIsNeverReaped(t *testing.T) {
	st, r := newLivenessTarget(t, true)
	reapDeadMgmt(st, probeMgmt(st))
	if !r.sessionAlive {
		t.Fatal("reapDeadMgmt killed a session with a live pane")
	}
}

// TestUnknownProbeNeverCleansUp: a tmux that cannot be run is not an
// answer. Mapping that to "gone" would delete a live share's state on a
// transient fork failure.
func TestUnknownProbeNeverCleansUp(t *testing.T) {
	// Every caller that can delete state or kill a session, not just
	// the classifier. stopWorker used to fall through its own guard on
	// an unknown probe and report success it had not established.
	callers := map[string]func(*shareTarget){
		"loadLiveShare": func(st *shareTarget) { loadLiveShare(st) },
		"stopWorker":    func(st *shareTarget) { stopWorker(st) },
		"cleanupStale": func(st *shareTarget) {
			// And it must say so, not refuse in silence: its callers
			// announce "cleaned up stale state" on the strength of it.
			if err := cleanupStale(st); err == nil {
				panic("cleanupStale reported success after refusing to clean up")
			}
		},
	}
	for name, call := range callers {
		t.Run(name, func(t *testing.T) {
			st, r := newLivenessTarget(t, true)
			r.probeErr = errors.New("tmux list-panes: fork/exec /usr/bin/tmux: resource temporarily unavailable")

			if got := probeMgmt(st); got != mgmtUnknown {
				t.Fatalf("probeMgmt = %v, want mgmtUnknown when tmux could not be asked", got)
			}
			if err := share.SaveState(st.dir, &share.State{SessionID: "$1", ViewURL: "u", UID: "x"}); err != nil {
				t.Fatal(err)
			}

			call(st)

			if _, err := os.Stat(filepath.Join(st.dir, "state.json")); err != nil {
				t.Error("state was deleted after an unanswerable probe")
			}
			for _, c := range r.calls {
				if strings.HasPrefix(c, "kill-session") {
					t.Error("killed a session tmux could not be asked about")
				}
			}
		})
	}

	t.Run("stopWorker reports failure", func(t *testing.T) {
		st, r := newLivenessTarget(t, true)
		r.probeErr = errors.New("tmux list-panes: no answer")
		if stopWorker(st) {
			t.Error("stopWorker reported the share stopped without ever confirming it")
		}
	})

	t.Run("classification stays live", func(t *testing.T) {
		st, r := newLivenessTarget(t, true)
		r.probeErr = errors.New("tmux list-panes: no answer")
		if err := share.SaveState(st.dir, &share.State{SessionID: "$1", ViewURL: "u", UID: "x"}); err != nil {
			t.Fatal(err)
		}
		if _, l := loadLiveShare(st); l == shareStale || l == shareAbsent {
			t.Errorf("liveness = %v, which callers clean up -- a transient probe failure would orphan a live share", l)
		}
	})
}

// signalLog captures what stopWorker would send where, and shrinks the
// grace periods so a unit test does not sit through them.
type signalLog struct {
	sent []string
}

func captureSignals(t *testing.T) *signalLog {
	t.Helper()
	log := &signalLog{}
	oldKill, oldStop, oldKillGrace := killProcess, stopGrace, killGrace
	killProcess = func(pid int, sig syscall.Signal) error {
		log.sent = append(log.sent, fmt.Sprintf("%d:%v", pid, sig))
		return nil
	}
	stopGrace, killGrace = 50*time.Millisecond, 50*time.Millisecond
	t.Cleanup(func() { killProcess, stopGrace, killGrace = oldKill, oldStop, oldKillGrace })
	return log
}

// TestStopWorkerDoesNotEscalateToAReusedPID: between the SIGTERM and
// the SIGKILL the worker exits, and the kernel is free to hand its
// number to anything. Five seconds is long enough for that on a machine
// that cycles PIDs, so the escalation asks tmux again and goes ahead
// only if the same number is still there.
func TestStopWorkerDoesNotEscalateToAReusedPID(t *testing.T) {
	sig := captureSignals(t)
	st, r := newLivenessTarget(t, true)
	// First read is the worker; every read after it is whatever took
	// the number over. The session stays alive so the stop cannot
	// succeed and has to reach the escalation.
	r.panePIDs = []int{4242, 5150}

	if stopWorker(st) {
		t.Fatal("stopWorker reported success while the management pane remained live")
	}

	for _, s := range sig.sent {
		if strings.HasSuffix(s, "killed") && !strings.HasPrefix(s, "4242:") {
			t.Errorf("SIGKILL sent to %s, which is not the process we signalled", s)
		}
		if strings.HasPrefix(s, "5150:") {
			t.Errorf("signalled %s -- that PID belongs to whatever reused the number", s)
		}
	}
	if len(sig.sent) != 1 || !strings.HasPrefix(sig.sent[0], "4242:") {
		t.Errorf("signals sent = %v, want exactly one SIGTERM to the worker", sig.sent)
	}
}

// TestStopWorkerEscalatesToTheSameWorker is the other half: a worker
// that ignores SIGTERM and is still the pane's process does get killed.
func TestStopWorkerEscalatesToTheSameWorker(t *testing.T) {
	sig := captureSignals(t)
	st, r := newLivenessTarget(t, true)
	r.panePIDs = []int{4242}

	if stopWorker(st) {
		t.Fatal("stopWorker reported success while the fake worker remained live")
	}

	if len(sig.sent) != 2 || sig.sent[0] != "4242:terminated" || sig.sent[1] != "4242:killed" {
		t.Errorf("signals sent = %v, want SIGTERM then SIGKILL to 4242", sig.sent)
	}
}

func TestWaitForStateRejectsAnotherStartsState(t *testing.T) {
	st, _ := newLivenessTarget(t, true)
	if err := share.SaveState(st.dir, &share.State{
		SessionID: "$1", ViewURL: "https://old.example/#dead", UID: "u", Nonce: "a-previous-start",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := waitForState(st, "this-start", 300*time.Millisecond); err == nil {
		t.Fatal("waitForState handed back a previous start's state as this start's")
	}
}

func TestWaitForStateRequiresALiveWorker(t *testing.T) {
	st, r := newLivenessTarget(t, true)
	r.paneDead = true
	if err := share.SaveState(st.dir, &share.State{
		SessionID: "$1", ViewURL: "https://x.example/#y", UID: "u", Nonce: "this-start",
	}); err != nil {
		t.Fatal(err)
	}
	_, err := waitForState(st, "this-start", 5*time.Second)
	if err == nil {
		t.Fatal("waitForState reported a worker ready after it had already exited")
	}
	if !strings.Contains(err.Error(), "exited") {
		t.Errorf("error = %q, want it to say the worker exited", err)
	}
}

func TestWaitForStateAcceptsThisStart(t *testing.T) {
	st, _ := newLivenessTarget(t, true)
	want := &share.State{
		SessionID: "$1", ViewURL: "https://x.example/#y", UID: "u", Nonce: "this-start",
	}
	if err := share.SaveState(st.dir, want); err != nil {
		t.Fatal(err)
	}
	got, err := waitForState(st, "this-start", 5*time.Second)
	if err != nil {
		t.Fatalf("waitForState: %v", err)
	}
	if got.ViewURL != want.ViewURL {
		t.Errorf("ViewURL = %q, want %q", got.ViewURL, want.ViewURL)
	}
}

func TestWaitForStateOutlastsAnUnansweredProbe(t *testing.T) {
	st, r := newLivenessTarget(t, true)
	r.probeErr = errors.New("tmux list-panes: fork/exec: resource temporarily unavailable")

	start := time.Now()
	_, err := waitForState(st, "this-start", 400*time.Millisecond)
	if err == nil {
		t.Fatal("expected the wait to run out")
	}
	if strings.Contains(err.Error(), "exited") {
		t.Errorf("error = %q, but nothing established that the worker exited", err)
	}
	if elapsed := time.Since(start); elapsed < 300*time.Millisecond {
		t.Errorf("gave up after %s; an unanswered probe should not cut the wait short", elapsed)
	}
}

// If the pane exits after SIGTERM, its PID must not receive SIGKILL.
func TestStopWorkerDoesNotEscalateToAVanishedPane(t *testing.T) {
	sig := captureSignals(t)
	st, r := newLivenessTarget(t, true)
	r.panePIDs = []int{4242}
	// Alive for the PID read and the first wait, gone by the time the
	// escalation looks again.
	r.dieAfterProbes = 2

	if !stopWorker(st) {
		t.Fatal("stopWorker reported failure against a worker that exited")
	}
	if len(sig.sent) != 1 || sig.sent[0] != "4242:terminated" {
		t.Errorf("signals sent = %v, want only the SIGTERM; the pane was gone before the escalation", sig.sent)
	}
}

// TestUnreachableServerIsNotAnAnswer is the counterexample to reading a
// tmux exit status as a verdict. A live server whose socket has become
// inaccessible answers every command with exit 1 -- `error connecting to
// PATH (Permission denied)` -- which is indistinguishable from the
// session being gone, while the session is in fact still running.
// Reproduced against a real server with chmod 000 on its socket.
//
// The session stays ALIVE in this fake on purpose. That is the whole
// case: the answer the classifier must not give is "gone", about a
// worker that is sitting there serving. Only the listing failed.
func TestUnreachableServerIsNotAnAnswer(t *testing.T) {
	t.Run("classified unknown", func(t *testing.T) {
		st, r := newLivenessTarget(t, true) // the worker is running...
		r.serverUnreachable = true          // ...and the listing cannot say so
		if got := probeMgmt(st); got != mgmtUnknown {
			t.Errorf("probeMgmt = %v, want mgmtUnknown; the session may be alive behind an unreachable socket", got)
		}
	})

	t.Run("server answers, target really is absent", func(t *testing.T) {
		st, r := newLivenessTarget(t, false)
		r.serverUnreachable = false
		if got := probeMgmt(st); got != mgmtGone {
			t.Errorf("probeMgmt = %v, want mgmtGone; the server answered and it has no such session", got)
		}
	})

	t.Run("state survives", func(t *testing.T) {
		st, r := newLivenessTarget(t, true)
		r.serverUnreachable = true
		if err := share.SaveState(st.dir, &share.State{SessionID: "$1", ViewURL: "u", UID: "x"}); err != nil {
			t.Fatal(err)
		}
		if stopWorker(st) {
			t.Error("stopWorker reported success without reaching the server")
		}
		if err := cleanupStale(st); err == nil {
			t.Error("cleanupStale reported success without ever reaching the server")
		}
		if _, err := os.Stat(filepath.Join(st.dir, "state.json")); err != nil {
			t.Error("a live share's state was deleted because tmux exited 1 for an unrelated reason")
		}
	})
}

// TestLockSerializesTargetLifecycle: classification, cleanup, session
// creation and accepting published state are individually safe and
// unsafe together -- a second command can replace the target between any
// two of them. The lock is what makes the sequence one turn.
func TestLockSerializesTargetLifecycle(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "target.lock")

	first, _, err := share.TryLock(path)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	if _, holder, err := share.TryLock(path); !errors.Is(err, share.ErrLockBusy) {
		t.Errorf("second acquire = %v (holder %d), want ErrLockBusy", err, holder)
	}
	first.Release()
	second, _, err := share.TryLock(path)
	if err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
	second.Release()
}

// TestLockOutlivesTheStateDirectory: cleanup removes the state
// directory while the lock is held. A lock file inside it would be
// deleted mid-sequence, and the next process would create a fresh file
// at the same path and lock that instead -- excluding nobody.
func TestLockOutlivesTheStateDirectory(t *testing.T) {
	st, _ := newLivenessTarget(t, false)
	if !strings.HasPrefix(st.dir, filepath.Dir(st.lock)) {
		t.Fatalf("test setup: lock %q is not beside state dir %q", st.lock, st.dir)
	}
	if strings.HasPrefix(st.lock, st.dir+string(filepath.Separator)) {
		t.Errorf("lock %q lives inside the state directory %q, which cleanup deletes", st.lock, st.dir)
	}
}

// TestEveryCommandTakesTheTargetLock is the wiring test. The flock unit
// test above proves only the primitive -- it would pass with every
// enterTarget call deleted, which is exactly the part that matters. So
// this holds the target's lock from outside and checks that each public
// command refuses to proceed.
//
// It holds the lock rather than racing two real starts: racing them
// meant waiting out the production startup timeout, sixteen seconds per
// run, and it only ever exercised one command.
func TestEveryCommandTakesTheTargetLock(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	for _, bin := range []string{"tmux", "go"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not installed", bin)
		}
	}

	// Short base path: a Unix socket path is capped near 104 bytes.
	base, err := os.MkdirTemp("/tmp", "bblock")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })

	bb := filepath.Join(base, "bb")
	if out, err := exec.Command("go", "build", "-o", bb, ".").CombinedOutput(); err != nil {
		t.Skipf("cannot build the CLI under test: %v\n%s", err, out)
	}

	socket := filepath.Join(base, "s")
	home := filepath.Join(base, "home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	tmux := func(args ...string) *exec.Cmd {
		return exec.Command("tmux", append([]string{"-S", socket}, args...)...)
	}
	if err := tmux("new-session", "-d", "-s", "work", "cat").Run(); err != nil {
		t.Skipf("cannot start an isolated tmux server: %v", err)
	}
	t.Cleanup(func() { _ = tmux("kill-server").Run() })

	// Derive the same lock path the CLI will, from the server's own
	// answers, so this fails loudly if that derivation ever changes.
	idOut, err := tmux("display-message", "-p", "-t", "=work:", "#{session_id}\t#{socket_path}").Output()
	if err != nil {
		t.Fatalf("display-message: %v", err)
	}
	fields := strings.Split(strings.TrimSpace(string(idOut)), "\t")
	if len(fields) < 2 {
		t.Fatalf("unexpected display-message output %q", idOut)
	}
	hash := share.TargetHash(fields[1], fields[0])
	lockPath := filepath.Join(home, ".bitbang", "shares", hash+".lock")
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o700); err != nil {
		t.Fatal(err)
	}

	held, _, err := share.TryLock(lockPath)
	if err != nil {
		t.Fatalf("TryLock: %v", err)
	}
	defer held.Release()

	for _, args := range [][]string{
		{"share"},
		{"share", "status"},
		{"share", "stop"},
		{"share", "rotate"},
	} {
		full := append(append([]string{}, args...), "--target", "work", "--socket", socket,
			"--server", "nonexistent.invalid", "--ttl", "1m")
		cmd := exec.Command(bb, full...)
		cmd.Env = append(os.Environ(), "HOME="+home)
		out, err := cmd.CombinedOutput()
		if err == nil {
			t.Errorf("`%s` ran to completion while the target lock was held:\n%s",
				strings.Join(args, " "), out)
			continue
		}
		if !strings.Contains(string(out), "Another `bitbang share` command is working") {
			t.Errorf("`%s` did not take the target lock; it failed with:\n%s",
				strings.Join(args, " "), out)
		}
	}

	// Nothing may have been created or destroyed while it was locked out.
	sessions, _ := tmux("list-sessions", "-F", "#{session_name}").Output()
	if strings.Contains(string(sessions), "_bbshare_") {
		t.Errorf("a locked-out command still created a management session: %s", sessions)
	}
}

// TestSweepRemovesUnreachableState: a worker whose *source* session was
// deleted leaves state no ordinary command can reach -- resolving a
// target begins by asking tmux for a session ID that no longer exists,
// and a new session of the same name hashes elsewhere. Anything left by
// a worker that was killed outright is in the same position. The sweep
// is the only thing that collects either.
func TestSweepRemovesUnreachableState(t *testing.T) {
	// Short base path: the recorded socket has to be one connect()
	// could actually have reached, or "no server there" is untestable --
	// a path past the sockaddr_un limit fails with EINVAL, which is not
	// an answer and is deliberately not treated as one.
	base, err := os.MkdirTemp("/tmp", "bbsweep")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	t.Setenv("HOME", base)
	shares := filepath.Join(base, ".bitbang", "shares")
	if err := os.MkdirAll(shares, 0o700); err != nil {
		t.Fatal(err)
	}

	// A share on a socket that no longer exists: its management session
	// cannot be listed, so nothing is running for it.
	stranded := filepath.Join(shares, "deadbeef")
	if err := share.SaveState(stranded, &share.State{
		Socket: filepath.Join(base, "gone.sock"), SessionID: "$0", SessionName: "work",
		MgmtSession: "_bbshare_deadbeef", ViewURL: "https://x.example/#dead", UID: "u",
	}); err != nil {
		t.Fatal(err)
	}
	// An empty directory and an orphaned lock, the other two ways a
	// killed command leaves litter.
	empty := filepath.Join(shares, "cafe0000")
	if err := os.MkdirAll(empty, 0o700); err != nil {
		t.Fatal(err)
	}
	orphanLock := filepath.Join(shares, "f00d0000.lock")
	if err := os.WriteFile(orphanLock, []byte("1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// And the target this command is here for, which must be untouched.
	mine := filepath.Join(shares, "myhash")
	if err := share.SaveState(mine, &share.State{
		SessionID: "$1", MgmtSession: "_bbshare_myhash", ViewURL: "https://x.example/#mine", UID: "u",
	}); err != nil {
		t.Fatal(err)
	}

	sweepStranded(mine)

	if _, err := os.Stat(stranded); !os.IsNotExist(err) {
		t.Error("state for a share on a vanished server survived the sweep; nothing else can ever reach it")
	}
	if _, err := os.Stat(empty); !os.IsNotExist(err) {
		t.Error("an empty share directory survived the sweep")
	}
	if _, err := os.Stat(orphanLock); !os.IsNotExist(err) {
		t.Error("a lock file with no target survived the sweep")
	}
	if _, err := os.Stat(filepath.Join(mine, "state.json")); err != nil {
		t.Error("the sweep removed the state of the target the command is working on")
	}
}

// TestSweepLeavesWhatItCannotAccountFor: the sweep deletes credential
// files, so silence is never evidence. A live share, a state file it
// cannot read, and a directory another command holds the lock on all
// have to survive.
func TestSweepLeavesWhatItCannotAccountFor(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	base, err := os.MkdirTemp("/tmp", "bbsweep")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	t.Setenv("HOME", base)
	shares := filepath.Join(base, ".bitbang", "shares")
	if err := os.MkdirAll(shares, 0o700); err != nil {
		t.Fatal(err)
	}

	socket := filepath.Join(base, "s")
	tmux := func(args ...string) *exec.Cmd {
		return exec.Command("tmux", append([]string{"-S", socket}, args...)...)
	}
	if err := tmux("new-session", "-d", "-s", "_bbshare_live", "cat").Run(); err != nil {
		t.Skipf("cannot start an isolated tmux server: %v", err)
	}
	t.Cleanup(func() { _ = tmux("kill-server").Run() })

	live := filepath.Join(shares, "live")
	if err := share.SaveState(live, &share.State{
		Socket: socket, SessionID: "$0", MgmtSession: "_bbshare_live",
		ViewURL: "https://x.example/#live", UID: "u",
	}); err != nil {
		t.Fatal(err)
	}

	unreadable := filepath.Join(shares, "unreadable")
	if err := os.MkdirAll(unreadable, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(unreadable, "state.json"), []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}

	// A directory another command is working on: no state yet, because
	// it has not published, and the lock is what says so.
	busy := filepath.Join(shares, "busy")
	if err := os.MkdirAll(busy, 0o700); err != nil {
		t.Fatal(err)
	}
	busyLock, _, err := share.TryLock(share.LockPathFor(busy))
	if err != nil {
		t.Fatal(err)
	}
	defer busyLock.Release()

	sweepStranded(filepath.Join(shares, "somewhere-else"))

	if _, err := os.Stat(filepath.Join(live, "state.json")); err != nil {
		t.Error("the sweep deleted a live share's credentials")
	}
	if _, err := os.Stat(filepath.Join(unreadable, "state.json")); err != nil {
		t.Error("the sweep deleted state it could not read, which names no server to check")
	}
	if _, err := os.Stat(busy); err != nil {
		t.Error("the sweep deleted a directory another command held the lock on")
	}
}

// TestServerIsGoneOnlyOnProof: the sweep deletes credential files on
// this answer, and tmux renders all three of these conditions as the
// same exit status, so the errno is what has to separate them. Built on
// plain unix sockets -- no tmux needed to establish what connect() says.
func TestServerIsGoneOnlyOnProof(t *testing.T) {
	base, err := os.MkdirTemp("/tmp", "bbsock")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })

	t.Run("no socket at all", func(t *testing.T) {
		if !serverIsGone(filepath.Join(base, "never-existed")) {
			t.Error("a path with nothing at it was not recognised as having no server")
		}
	})

	t.Run("something listening", func(t *testing.T) {
		path := filepath.Join(base, "live")
		ln, err := net.Listen("unix", path)
		if err != nil {
			t.Fatal(err)
		}
		defer ln.Close()
		if serverIsGone(path) {
			t.Error("a socket with a listener was called gone")
		}
	})

	t.Run("stale socket, nothing listening", func(t *testing.T) {
		// A tmux socket file may outlive its server.
		path := filepath.Join(base, "stale")
		ln, err := net.Listen("unix", path)
		if err != nil {
			t.Fatal(err)
		}
		if unixLn, ok := ln.(*net.UnixListener); ok {
			unixLn.SetUnlinkOnClose(false)
		}
		_ = ln.Close()
		if _, err := os.Stat(path); err != nil {
			t.Skipf("could not stage a stale socket: %v", err)
		}
		if !serverIsGone(path) {
			t.Error("a socket with nothing listening was not recognised as having no server")
		}
	})

	t.Run("unreachable socket", func(t *testing.T) {
		if os.Getuid() == 0 {
			t.Skip("root connects whatever the mode")
		}
		path := filepath.Join(base, "forbidden")
		ln, err := net.Listen("unix", path)
		if err != nil {
			t.Fatal(err)
		}
		defer ln.Close()
		if err := os.Chmod(path, 0o000); err != nil {
			t.Skipf("cannot restrict the socket: %v", err)
		}
		if serverIsGone(path) {
			t.Error("a live server behind an inaccessible socket was called gone; its share would lose its only record")
		}
	})
}

// sweepFixture stands up a real tmux server plus a shares directory,
// since what the sweep decides is exactly what tmux tells it.
type sweepFixture struct {
	socket string
	shares string
	tmux   func(args ...string) *exec.Cmd
}

func newSweepFixture(t *testing.T) *sweepFixture {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	// Short base path: a Unix socket path is capped near 104 bytes.
	base, err := os.MkdirTemp("/tmp", "bbswp")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	t.Setenv("HOME", base)

	f := &sweepFixture{socket: filepath.Join(base, "s"), shares: filepath.Join(base, ".bitbang", "shares")}
	f.tmux = func(args ...string) *exec.Cmd {
		return exec.Command("tmux", append([]string{"-S", f.socket}, args...)...)
	}
	if err := os.MkdirAll(f.shares, 0o700); err != nil {
		t.Fatal(err)
	}
	// A session that outlives the shares, so the server stays up to be
	// asked -- as the worker's own management session does in production.
	if err := f.tmux("new-session", "-d", "-s", "keep-alive", "cat").Run(); err != nil {
		t.Skipf("cannot start an isolated tmux server: %v", err)
	}
	t.Cleanup(func() { _ = f.tmux("kill-server").Run() })
	return f
}

func (f *sweepFixture) writeState(t *testing.T, name, mgmt string) string {
	t.Helper()
	dir := filepath.Join(f.shares, name)
	if err := share.SaveState(dir, &share.State{
		Socket: f.socket, SessionID: "$9", SessionName: "work", MgmtSession: mgmt,
		ViewURL: "https://x.example/#" + name, UID: "u",
	}); err != nil {
		t.Fatal(err)
	}
	return dir
}

// Start a share between sweep entries and ensure its liveness is checked only
// after the sweep takes that target's lock.
func TestSweepReadsLivenessAfterTakingEachLock(t *testing.T) {
	f := newSweepFixture(t)

	// Processed first, and genuinely stale: nothing by that name runs.
	stale := f.writeState(t, "aaa", "_bbshare_aaa")
	// Processed second. Its session does not exist yet.
	fresh := f.writeState(t, "zzz", "_bbshare_zzz")

	started := false
	old := sweepStep
	sweepStep = func() {
		if started {
			return
		}
		started = true
		if err := f.tmux("new-session", "-d", "-s", "_bbshare_zzz", "cat").Run(); err != nil {
			t.Errorf("new-session: %v", err)
		}
	}
	t.Cleanup(func() { sweepStep = old })

	sweepStranded(filepath.Join(f.shares, "not-a-target"))

	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Error("a genuinely stale directory survived; the sweep is not doing its job at all")
	}
	if _, err := os.Stat(filepath.Join(fresh, "state.json")); err != nil {
		t.Error("the sweep deleted a share that came up while it was working, " +
			"which means the verdict was read before that target's lock was taken")
	}
}

// TestSweepReapsRetainedHusk: with a global remain-on-exit, a dead
// worker's management session stays listed. Reading only session names
// counted that as running and preserved it forever -- and when the
// source session is what disappeared, no targeted command can ever come
// back for it, so the state and the husk both sit there permanently.
func TestSweepReapsRetainedHusk(t *testing.T) {
	f := newSweepFixture(t)
	if err := f.tmux("set", "-g", "remain-on-exit", "on").Run(); err != nil {
		t.Fatal(err)
	}
	if err := f.tmux("new-session", "-d", "-s", "_bbshare_husk", "true").Run(); err != nil {
		t.Fatal(err)
	}
	dir := f.writeState(t, "husk", "_bbshare_husk")

	// Wait for the pane to actually be dead, not merely started.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		out, _ := f.tmux("list-panes", "-t", "=_bbshare_husk:", "-F", "#{pane_dead}").Output()
		if strings.Contains(string(out), "1") {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	sweepStranded(filepath.Join(f.shares, "not-a-target"))

	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Error("a dead worker's state survived because its husk was still listed")
	}
	out, _ := f.tmux("list-sessions", "-F", "#{session_name}").Output()
	if strings.Contains(string(out), "_bbshare_husk") {
		t.Errorf("the husk was left holding its name, which blocks the next share: %s", out)
	}
}

// TestSweepLeavesALiveShareOnTheSameServer is the converse, and the one
// that matters most: a running share must survive a sweep triggered by
// something else entirely.
func TestSweepLeavesALiveShareOnTheSameServer(t *testing.T) {
	f := newSweepFixture(t)
	if err := f.tmux("new-session", "-d", "-s", "_bbshare_live", "cat").Run(); err != nil {
		t.Fatal(err)
	}
	live := f.writeState(t, "live", "_bbshare_live")
	stale := f.writeState(t, "stale", "_bbshare_stale")

	sweepStranded(filepath.Join(f.shares, "not-a-target"))

	if _, err := os.Stat(filepath.Join(live, "state.json")); err != nil {
		t.Error("the sweep deleted a running share's credentials")
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Error("the stale directory beside it survived")
	}
}

// A process that accepts the old socket but never speaks tmux must not block
// share commands indefinitely.
func TestSweepDoesNotHangOnAWedgedSocket(t *testing.T) {
	f := newSweepFixture(t)
	old := sweepProbeTimeout
	sweepProbeTimeout = 300 * time.Millisecond
	t.Cleanup(func() { sweepProbeTimeout = old })

	wedged := filepath.Join(filepath.Dir(f.shares), "wedged.sock")
	ln, err := net.Listen("unix", wedged)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			_ = conn // accepted and then ignored, forever
		}
	}()

	dir := filepath.Join(f.shares, "wedged")
	if err := share.SaveState(dir, &share.State{
		Socket: wedged, SessionID: "$9", MgmtSession: "_bbshare_wedged",
		ViewURL: "https://x.example/#w", UID: "u",
	}); err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})
	go func() { sweepStranded(filepath.Join(f.shares, "not-a-target")); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the sweep hung on a socket that accepts connections and never answers")
	}

	if _, err := os.Stat(filepath.Join(dir, "state.json")); err != nil {
		t.Error("state was deleted on a probe that timed out, which established nothing")
	}
}

// Multiple wedged sockets must still respect the total sweep budget.
func TestSweepStaysWithinItsBudget(t *testing.T) {
	f := newSweepFixture(t)
	oldProbe, oldBudget := sweepProbeTimeout, sweepBudget
	sweepProbeTimeout, sweepBudget = 200*time.Millisecond, 300*time.Millisecond
	t.Cleanup(func() { sweepProbeTimeout, sweepBudget = oldProbe, oldBudget })

	const wedged = 5
	for i := range wedged {
		sock := filepath.Join(filepath.Dir(f.shares), fmt.Sprintf("wedged%d.sock", i))
		ln, err := net.Listen("unix", sock)
		if err != nil {
			t.Fatal(err)
		}
		defer ln.Close()
		go func() {
			for {
				conn, err := ln.Accept()
				if err != nil {
					return
				}
				_ = conn // accepted and then ignored, forever
			}
		}()
		dir := filepath.Join(f.shares, fmt.Sprintf("wedged%d", i))
		if err := share.SaveState(dir, &share.State{
			Socket: sock, SessionID: "$9", MgmtSession: "_bbshare_w",
			ViewURL: "https://x.example/#w", UID: "u",
		}); err != nil {
			t.Fatal(err)
		}
	}

	start := time.Now()
	sweepStranded(filepath.Join(f.shares, "not-a-target"))
	elapsed := time.Since(start)

	// Unbounded, five entries would cost 5 x (200ms + 200ms + 200ms).
	// The budget stops after the first that overruns it.
	if elapsed > 1500*time.Millisecond {
		t.Errorf("the sweep took %s; pathological entries are stacking in front of the caller", elapsed)
	}

	// And nothing may be deleted on the way: every one of those probes
	// timed out, which establishes nothing at all.
	//
	// (The delay is also reported once -- see the log line in
	// sweepStranded -- because a share nothing can account for is kept
	// correctly, and then paid for on every command from here on.)
	for i := range wedged {
		path := filepath.Join(f.shares, fmt.Sprintf("wedged%d", i), "state.json")
		if _, err := os.Stat(path); err != nil {
			t.Errorf("state %d was deleted after a probe that only ran out of time", i)
		}
	}
}
