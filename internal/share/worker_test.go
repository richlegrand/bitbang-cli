package share

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/richlegrand/bitbang/internal/protocol"
	"github.com/richlegrand/bitbang/internal/session"
	"github.com/richlegrand/bitbang/internal/streamtype"
)

func TestNewWorkerValidatesLimits(t *testing.T) {
	base := WorkerConfig{
		SessionID:   "$1",
		MgmtSession: "mgmt",
		StateDir:    t.TempDir(),
		MaxViewers:  1,
		Runner:      newFakeRunner(),
	}
	for _, mutate := range []func(*WorkerConfig){
		func(c *WorkerConfig) { c.MaxViewers = -1 },
		func(c *WorkerConfig) { c.MaxViewers = MaxViewers + 1 },
		func(c *WorkerConfig) { c.TTL = 1500 * time.Millisecond },
		func(c *WorkerConfig) { c.TTL = MaxTTL + time.Second },
	} {
		cfg := base
		mutate(&cfg)
		if _, err := NewWorker(cfg); err == nil {
			t.Fatalf("NewWorker accepted invalid config: %+v", cfg)
		}
	}
}

func TestReadOnlyWorkerHasOnlyViewCredential(t *testing.T) {
	w, err := NewWorker(WorkerConfig{
		SessionID:   "$1",
		MgmtSession: "mgmt",
		StateDir:    t.TempDir(),
		MaxViewers:  1,
		ReadOnly:    true,
		Runner:      newFakeRunner(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if w.controlCode != "" || w.controlURL != "" {
		t.Fatalf("read-only worker created control access: code=%q URL=%q", w.controlCode, w.controlURL)
	}
	if w.viewCode != w.id.Code {
		t.Error("read-only worker generated a second credential instead of using its identity code for viewing")
	}
	if access, ok := w.authorize(w.viewCode); !ok || access != protocol.AccessView {
		t.Fatalf("view credential authorized as (%q, %v)", access, ok)
	}
}

func TestDistinctAccessCodeRetriesCollision(t *testing.T) {
	codes := []string{"same", "same", "different"}
	code, err := distinctAccessCode("same", func() (string, error) {
		next := codes[0]
		codes = codes[1:]
		return next, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if code != "different" {
		t.Fatalf("distinctAccessCode = %q, want different", code)
	}
}

func TestWorkerPublishesStateOnceAndNotAfterShutdown(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "active")
	w, err := NewWorker(WorkerConfig{
		SessionID:   "$3",
		SessionName: "work",
		MgmtSession: "mgmt",
		StateDir:    stateDir,
		Server:      "example.test",
		TTL:         time.Minute,
		MaxViewers:  4,
		Runner:      newFakeRunner(),
	})
	if err != nil {
		t.Fatal(err)
	}
	w.startedAt = time.Now()
	w.expiresAt = w.startedAt.Add(time.Minute)
	w.onReady()

	state, err := LoadState(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if state == nil || state.ControlURL == "" || state.ViewURL == "" || state.ControlURL == state.ViewURL {
		t.Fatalf("worker published invalid role URLs: %+v", state)
	}
	if state.TTLSeconds != 60 || state.MaxViewers != 4 {
		t.Fatalf("worker published wrong limits: %+v", state)
	}

	// Shutdown leaves state for a later command holding the lifecycle lock.
	w.shutdown()
	if after, err := LoadState(stateDir); err != nil || after == nil {
		t.Fatalf("shutdown deleted state a worker cannot safely delete: (%+v, %v)", after, err)
	}

	// What must not survive shutdown is a second publication: a late
	// readiness callback here would resurrect a share that has already
	// handed its peers back.
	if err := RemoveState(stateDir); err != nil {
		t.Fatal(err)
	}
	w.onReady()
	if state, err := LoadState(stateDir); err != nil || state != nil {
		t.Fatalf("late readiness republished a stopped share: (%+v, %v)", state, err)
	}
}

func TestWorkerDoesNotMutateWindowSize(t *testing.T) {
	runner := newFakeRunner()
	w, err := NewWorker(WorkerConfig{
		SessionID:   "$1",
		MgmtSession: "mgmt",
		StateDir:    filepath.Join(t.TempDir(), "state"),
		MaxViewers:  1,
		Runner:      runner,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := w.Run(ctx); err != nil {
		t.Fatal(err)
	}
	for _, call := range runner.calls {
		for _, arg := range call {
			if arg == "window-size" {
				t.Fatalf("worker mutated tmux window sizing: %v", call)
			}
		}
	}
}

func TestRoleSlots(t *testing.T) {
	slots := newRoleSlots(2, "share is full")
	first, _ := slots.acquire()
	second, _ := slots.acquire()
	if first == nil || second == nil {
		t.Fatal("slot pool refused within its capacity")
	}
	if release, busy := slots.acquire(); release != nil || busy != "share is full" {
		t.Fatalf("over-capacity acquire = (%v, %q)", release != nil, busy)
	}

	first()
	next, _ := slots.acquire()
	if next == nil {
		t.Fatal("released slot was not reusable")
	}
	next()
	next()
	if a, _ := slots.acquire(); a == nil {
		t.Fatal("slot disappeared after idempotent release")
	}
	if b, _ := slots.acquire(); b != nil {
		t.Fatal("double release created an extra slot")
	}
}

func TestSharePeerTeardownReleasesOnce(t *testing.T) {
	var released atomic.Int32
	p := &sharePeer{}
	p.hold(func() { released.Add(1) })

	var wins atomic.Int32
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if p.teardown() {
				wins.Add(1)
			}
		}()
	}
	wg.Wait()
	if wins.Load() != 1 || released.Load() != 1 {
		t.Fatalf("teardown wins=%d releases=%d, want 1 each", wins.Load(), released.Load())
	}
	if p.hold(func() {}) {
		t.Fatal("closed peer accepted a new reservation")
	}
}

func TestSharePeerAdmissionRaceReturnsSlot(t *testing.T) {
	slots := newRoleSlots(1, "full")
	p := &sharePeer{}
	release, _ := slots.acquire()
	p.teardown()
	if !p.hold(release) {
		release()
	}
	if next, _ := slots.acquire(); next == nil {
		t.Fatal("role slot leaked when the peer closed during admission")
	}
}

func TestSharePeerEstablishmentDeadlineCanBeCanceled(t *testing.T) {
	expired := make(chan struct{})
	p := &sharePeer{}
	p.armEstablishment(20*time.Millisecond, func() { close(expired) })
	p.markEstablished()
	select {
	case <-expired:
		t.Fatal("establishment deadline fired after the peer became ready")
	case <-time.After(100 * time.Millisecond):
	}
	p.teardown()
}

func TestSharePeerTeardownClosesPublishedShell(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "started")
	sh := streamtype.NewShell(nil, false)
	sh.ForcedArgv = []string{"/bin/sh", "-c", "touch \"$0\"", marker}
	p := &sharePeer{}
	if !p.publish(sh, &session.Session{}) {
		t.Fatal("live peer refused publication")
	}
	if !p.teardown() {
		t.Fatal("live peer was not torn down")
	}

	stream := &workerTestStream{}
	syn, _ := json.Marshal(map[string]any{"type": "shell"})
	if err := sh.OnSYN(stream, syn, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("teardown left the published handler able to execute; stat error=%v", err)
	}
}

func TestSharePeerPublishRacesTeardown(t *testing.T) {
	for i := 0; i < 200; i++ {
		p := &sharePeer{}
		sh := streamtype.NewShell(nil, false)
		var wg sync.WaitGroup
		var published bool
		wg.Add(2)
		go func() {
			defer wg.Done()
			published = p.publish(sh, &session.Session{})
		}()
		go func() {
			defer wg.Done()
			p.teardown()
		}()
		wg.Wait()

		p.mu.Lock()
		installed := p.shell
		closed := p.closed
		p.mu.Unlock()
		if !closed || published != (installed == sh) {
			t.Fatalf("iteration %d: published=%v installed=%v closed=%v", i, published, installed == sh, closed)
		}
	}
}

type workerTestStream struct{}

func (*workerTestStream) ID() uint32                   { return 1 }
func (*workerTestStream) ConnectPath() string          { return "/" }
func (*workerTestStream) WriteSYN([]byte) error        { return nil }
func (*workerTestStream) WriteDAT([]byte) error        { return nil }
func (*workerTestStream) WriteFIN([]byte) error        { return nil }
func (*workerTestStream) SendRaw(uint16, []byte) error { return nil }
func (*workerTestStream) BufferedAmount() uint64       { return 0 }

var _ streamtype.Stream = (*workerTestStream)(nil)

// A blocked peer delivery must not block the shared signaling loop.
func TestPublishDoesNotDrainOnCallerGoroutine(t *testing.T) {
	blocked := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once

	p := &sharePeer{}
	p.deliver = func([]byte) {
		once.Do(func() { close(blocked) })
		<-release
	}
	defer close(release)

	// A frame is already queued, so the drain has something to park on.
	p.handleMessage([]byte("queued"))

	returned := make(chan bool, 1)
	go func() { returned <- p.publish(nil, nil) }()

	select {
	case ok := <-returned:
		if !ok {
			t.Fatal("publish refused on a live peer")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("publish blocked on delivery -- the signaling loop would be stalled with it")
	}

	select {
	case <-blocked:
	case <-time.After(5 * time.Second):
		t.Fatal("the queued frame was never delivered")
	}
}

// TestDrainStopsOnTeardown: a peer that goes away mid-drain must not
// keep feeding a session that is being torn down. Exactly one delivery
// -- the one already in flight -- may complete; the rest of the backlog
// must be abandoned.
func TestDrainStopsOnTeardown(t *testing.T) {
	const queued = 8
	var mu sync.Mutex
	delivered := 0
	started := make(chan struct{}, queued)
	proceed := make(chan struct{})

	p := &sharePeer{}
	p.deliver = func([]byte) {
		mu.Lock()
		delivered++
		mu.Unlock()
		started <- struct{}{}
		<-proceed
	}
	for i := 0; i < queued; i++ {
		p.handleMessage([]byte("frame"))
	}
	if !p.publish(nil, nil) {
		t.Fatal("publish refused on a live peer")
	}

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("drain never started")
	}
	p.teardown()
	close(proceed) // release the in-flight delivery

	// Wait for the drain to actually finish rather than for a duration
	// that looks long enough. dispatching is set before the goroutine
	// starts and cleared in its defer, so once it is false the count is
	// final and cannot grow under a slower machine.
	waitFor(t, "the drain goroutine to exit", func() bool {
		p.mu.Lock()
		defer p.mu.Unlock()
		return !p.dispatching
	})
	mu.Lock()
	got := delivered
	mu.Unlock()
	if got != 1 {
		t.Errorf("delivered %d of %d queued frames after teardown, want exactly the 1 already in flight", got, queued)
	}
}
