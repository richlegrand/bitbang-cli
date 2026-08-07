package streamtype

import (
	"context"
	"encoding/binary"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	ptylib "github.com/aymanbagabas/go-pty"
)

type shellLifecycleStream struct {
	id       uint32
	buffered atomic.Uint64
	writes   atomic.Int32
	fin      chan []byte
}

func (s *shellLifecycleStream) ID() uint32            { return s.id }
func (*shellLifecycleStream) ConnectPath() string     { return "/" }
func (*shellLifecycleStream) WriteSYN([]byte) error   { return nil }
func (s *shellLifecycleStream) WriteDAT([]byte) error { s.writes.Add(1); return nil }
func (s *shellLifecycleStream) WriteFIN(payload []byte) error {
	if s.fin != nil {
		s.fin <- append([]byte(nil), payload...)
	}
	return nil
}
func (*shellLifecycleStream) SendRaw(uint16, []byte) error { return nil }
func (s *shellLifecycleStream) BufferedAmount() uint64     { return s.buffered.Load() }

type synchronizedLifecyclePTY struct {
	resizeStarted chan struct{}
	resizeRelease chan struct{}
	resizeOnce    sync.Once
	resizeCount   atomic.Int32
}

func (*synchronizedLifecyclePTY) Read([]byte) (int, error)              { return 0, nil }
func (*synchronizedLifecyclePTY) Write(p []byte) (int, error)           { return len(p), nil }
func (*synchronizedLifecyclePTY) Close() error                          { return nil }
func (*synchronizedLifecyclePTY) Name() string                          { return "test-pty" }
func (*synchronizedLifecyclePTY) Command(string, ...string) *ptylib.Cmd { return nil }
func (*synchronizedLifecyclePTY) CommandContext(context.Context, string, ...string) *ptylib.Cmd {
	return nil
}
func (p *synchronizedLifecyclePTY) Resize(int, int) error {
	p.resizeCount.Add(1)
	p.resizeOnce.Do(func() { close(p.resizeStarted) })
	<-p.resizeRelease
	return nil
}
func (*synchronizedLifecyclePTY) Fd() uintptr { return 0 }

func TestPumpReaderCancellationBreaksBackpressure(t *testing.T) {
	stream := &shellLifecycleStream{id: 1}
	stream.buffered.Store(maxShellBuffered + 1)
	output := newShellOutput()
	output.Add(1)
	go func() {
		defer output.Done()
		NewShell(nil, false).pumpReader(stream, strings.NewReader("blocked"), shellTagStdout, output.cancelled())
	}()

	if output.wait(20 * time.Millisecond) {
		t.Fatal("output pump completed while the data channel remained backpressured")
	}
	output.cancel()
	if !output.wait(time.Second) {
		t.Fatal("output pump did not stop after cancellation")
	}
	if got := stream.writes.Load(); got != 0 {
		t.Fatalf("WriteDAT calls = %d, want 0 while backpressured", got)
	}
}

func TestFinishOutputBoundsBackpressureWait(t *testing.T) {
	stream := &shellLifecycleStream{id: 2}
	stream.buffered.Store(maxShellBuffered + 1)
	output := newShellOutput()
	output.Add(1)
	go func() {
		defer output.Done()
		NewShell(nil, false).pumpReader(stream, strings.NewReader("blocked"), shellTagStdout, output.cancelled())
	}()

	started := time.Now()
	if finishOutput(output, 20*time.Millisecond) {
		t.Fatal("finishOutput reported a clean drain while backpressure was pinned")
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("finishOutput took %v, want a bounded wait", elapsed)
	}
	if !output.wait(time.Second) {
		t.Fatal("timed-out output pump did not exit after cancellation")
	}
}

func TestWaitAndFinishBackpressureStillCompletesSession(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start helper process: %v", err)
	}
	defer func() { _ = cmd.Process.Kill() }()

	stream := &shellLifecycleStream{id: 3, fin: make(chan []byte, 1)}
	stream.buffered.Store(maxShellBuffered + 1)
	output := newShellOutput()
	output.Add(1)
	h := NewShell(nil, false)
	h.outputDrainTimeout = 20 * time.Millisecond
	terminal := &synchronizedLifecyclePTY{}
	sess := &shellSession{cmd: cmd, process: cmd.Process, ptyFile: terminal, output: output, done: make(chan struct{})}
	h.streams[stream.id] = sess
	previousActive := activeShellCount.Swap(1)
	defer activeShellCount.Store(previousActive)

	go func() {
		defer output.Done()
		h.pumpReader(stream, strings.NewReader("blocked"), shellTagStdout, output.cancelled())
	}()
	go h.waitAndFinish(stream, sess, []string{"helper"}, func() { activeShellCount.Add(-1) })

	select {
	case <-stream.fin:
	case <-time.After(2 * time.Second):
		t.Fatal("waitAndFinish did not send FIN after bounded output drain")
	}
	if got := activeShellCount.Load(); got != 0 {
		t.Fatalf("active shell count = %d, want 0", got)
	}
	h.mu.Lock()
	_, stillMapped := h.streams[stream.id]
	h.mu.Unlock()
	if stillMapped {
		t.Fatal("completed shell remained in the dispatch map")
	}
	if !output.wait(time.Second) {
		t.Fatal("backpressured output pump remained after session completion")
	}
}

func TestDetachSessionSerializesPTYUse(t *testing.T) {
	const streamID = 9
	terminal := &synchronizedLifecyclePTY{
		resizeStarted: make(chan struct{}),
		resizeRelease: make(chan struct{}),
	}
	sess := &shellSession{ptyFile: terminal}
	h := NewShell(nil, false)
	h.streams[streamID] = sess
	stream := &shellLifecycleStream{id: streamID}
	payload := make([]byte, 5)
	payload[0] = shellTagResize
	binary.LittleEndian.PutUint16(payload[1:3], 100)
	binary.LittleEndian.PutUint16(payload[3:5], 40)

	resizeDone := make(chan struct{})
	go func() {
		_ = h.OnDAT(stream, payload)
		close(resizeDone)
	}()
	<-terminal.resizeStarted

	detached := make(chan ptylib.Pty, 1)
	go func() { detached <- h.detachSession(streamID, sess) }()

	select {
	case <-detached:
		t.Fatal("PTY detached while Resize was still using it")
	case <-time.After(20 * time.Millisecond):
	}
	h.mu.Lock()
	_, stillMapped := h.streams[streamID]
	h.mu.Unlock()
	if stillMapped {
		t.Fatal("session remained dispatchable while shutdown waited for PTY use")
	}

	close(terminal.resizeRelease)
	<-resizeDone
	if got := <-detached; got != terminal {
		t.Fatalf("detached PTY = %T, want original terminal", got)
	}
	if err := h.OnDAT(stream, payload); err != nil {
		t.Fatalf("late resize: %v", err)
	}
	if got := terminal.resizeCount.Load(); got != 1 {
		t.Fatalf("resize calls = %d, want 1 after detachment", got)
	}
}
