package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/richlegrand/bitbang/internal/bytestream"
	"github.com/richlegrand/bitbang/internal/protocol"
	"github.com/richlegrand/bitbang/internal/streamtype"
	"github.com/richlegrand/bitbang/internal/tcpforward"
)

type rejectingTCPHandler struct {
	fin chan uint32
}

func (h *rejectingTCPHandler) Type() string             { return "tcp" }
func (h *rejectingTCPHandler) OnConnect(_ string) error { return nil }
func (h *rejectingTCPHandler) OnSYN(s streamtype.Stream, _ []byte, _ bool) error {
	return s.SendRaw(protocol.FlagSYN|protocol.FlagFIN, []byte(`{"status":"error","error":"rejected"}`))
}
func (h *rejectingTCPHandler) OnDAT(streamtype.Stream, []byte) error { return nil }
func (h *rejectingTCPHandler) OnFIN(s streamtype.Stream, _ []byte) error {
	h.fin <- s.ID()
	return nil
}

func startTCPEcho(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("echo listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				_, _ = io.Copy(conn, conn)
				if tcp, ok := conn.(*net.TCPConn); ok {
					_ = tcp.CloseWrite()
				}
			}()
		}
	}()
	return ln
}

func unusedTCPPorts(t *testing.T, count int) []int {
	t.Helper()
	listeners := make([]net.Listener, 0, count)
	ports := make([]int, 0, count)
	for i := 0; i < count; i++ {
		ln, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			for _, listener := range listeners {
				_ = listener.Close()
			}
			t.Fatalf("temporary listen: %v", err)
		}
		listeners = append(listeners, ln)
		ports = append(ports, ln.Addr().(*net.TCPAddr).Port)
	}
	for _, listener := range listeners {
		_ = listener.Close()
	}
	return ports
}

func roundTripForward(t *testing.T, port int, payload []byte) {
	t.Helper()
	if err := forwardRoundTrip(port, payload); err != nil {
		t.Fatal(err)
	}
}

func forwardRoundTrip(port int, payload []byte) error {
	conn, err := net.DialTimeout("tcp4", fmt.Sprintf("127.0.0.1:%d", port), 2*time.Second)
	if err != nil {
		return fmt.Errorf("dial local forward: %w", err)
	}
	tcp := conn.(*net.TCPConn)
	defer tcp.Close()
	if _, err := tcp.Write(payload); err != nil {
		return fmt.Errorf("write local forward: %w", err)
	}
	if err := tcp.CloseWrite(); err != nil {
		return fmt.Errorf("half-close local forward: %w", err)
	}
	got, err := io.ReadAll(tcp)
	if err != nil {
		return fmt.Errorf("read local forward: %w", err)
	}
	if !bytes.Equal(got, payload) {
		return fmt.Errorf("echoed %d bytes matching=%v, want %d", len(got), bytes.Equal(got, payload), len(payload))
	}
	return nil
}

func TestSession_TCPForwardingEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: spins up real pion peer connections and TCP listeners")
	}
	echo := startTCPEcho(t)
	echoPort := echo.Addr().(*net.TCPAddr).Port

	id := ephemeralID(t)
	relay := newFakeSignaling()
	t.Cleanup(relay.Close)
	tcpHandler := streamtype.NewTCP(false)
	startListener(relay.host(), id, streamtype.NewShell([]string{"sh"}, false), tcpHandler)
	waitRegistered(t, relay)
	sess := mustDial(t, relay.host(), id, "shell", "tcp")
	t.Cleanup(sess.Close)
	if !containsString(sess.ServerCaps, "tcp") {
		t.Fatalf("server caps = %v, want tcp", sess.ServerCaps)
	}

	ports := unusedTCPPorts(t, 3)
	goodLocal, badLocal, badRemote := ports[0], ports[1], ports[2]
	forwarder, err := sess.StartLocalForwarding([]tcpforward.Forward{
		{LocalPort: goodLocal, Host: "127.0.0.1", Port: echoPort},
		{LocalPort: badLocal, Host: "127.0.0.1", Port: badRemote},
	}, false)
	if err != nil {
		t.Fatalf("StartLocalForwarding: %v", err)
	}
	t.Cleanup(forwarder.Close)

	// More than the old 64-frame client queue could retain. Exact binary
	// comparison catches drops, duplication, and frame-boundary corruption.
	large := make([]byte, bytestream.FrameSize*70+137)
	for i := range large {
		large[i] = byte(i * 29)
	}
	roundTripForward(t, goodLocal, large)

	// Independent stream IDs must allow simultaneous local TCP connections.
	var wg sync.WaitGroup
	errs := make(chan error, 4)
	for i := 0; i < 4; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			payload := bytes.Repeat([]byte{byte(i), 0, 0xff}, 4000+i)
			errs <- forwardRoundTrip(goodLocal, payload)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}

	// One remote dial failure closes only that accepted connection.
	bad, err := net.DialTimeout("tcp4", fmt.Sprintf("127.0.0.1:%d", badLocal), 2*time.Second)
	if err != nil {
		t.Fatalf("dial failing local mapping: %v", err)
	}
	_ = bad.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, _ = bad.Write([]byte("trigger"))
	buf := make([]byte, 1)
	if _, err := bad.Read(buf); err == nil {
		t.Fatal("failed target connection stayed open")
	}
	_ = bad.Close()
	roundTripForward(t, goodLocal, []byte("healthy after isolated failure"))

	// Session completion tears down both idle accepted sockets and listeners.
	idle, err := net.DialTimeout("tcp4", fmt.Sprintf("127.0.0.1:%d", goodLocal), 2*time.Second)
	if err != nil {
		t.Fatalf("dial idle forwarded connection: %v", err)
	}
	sess.Close()
	forwarder.Close()
	_ = idle.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := idle.Read(buf); err == nil {
		t.Fatal("idle forwarded connection survived session teardown")
	}
	_ = idle.Close()
	if conn, err := net.DialTimeout("tcp4", fmt.Sprintf("127.0.0.1:%d", goodLocal), 100*time.Millisecond); err == nil {
		_ = conn.Close()
		t.Fatal("local listener survived session teardown")
	}
}

func TestSession_TCPFlowControlIsolatesStalledStream(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: spins up real pion peer connections, a TCP stream, and a shell process")
	}

	listenerConn, targetConn := net.Pipe()
	t.Cleanup(func() {
		_ = listenerConn.Close()
		_ = targetConn.Close()
	})

	tcpHandler := streamtype.NewTCP(false)
	dialed := make(chan struct{})
	var dialOnce sync.Once
	tcpHandler.DialContext = func(context.Context, string, string) (net.Conn, error) {
		dialOnce.Do(func() { close(dialed) })
		return listenerConn, nil
	}

	id := ephemeralID(t)
	relay := newFakeSignaling()
	t.Cleanup(relay.Close)
	startListener(relay.host(), id, streamtype.NewShell(nil, false), tcpHandler)
	waitRegistered(t, relay)
	sess := mustDial(t, relay.host(), id, "shell", "tcp")
	t.Cleanup(sess.Close)
	if sess.NegotiatedVersion != protocol.SWSPVersion {
		t.Fatalf("negotiated version = %d, want %d", sess.NegotiatedVersion, protocol.SWSPVersion)
	}

	stalled := sess.OpenStream()
	defer stalled.Close()
	syn, _ := json.Marshal(protocol.TCPOpen{Type: "tcp", Host: "stalled.test", Port: 9000})
	if err := stalled.WriteSYN(syn); err != nil {
		t.Fatalf("open stalled TCP stream: %v", err)
	}
	select {
	case <-dialed:
	case <-time.After(2 * time.Second):
		t.Fatal("listener did not dial stalled target")
	}
	select {
	case frame := <-stalled.Inbox():
		if !frame.IsSYN() || frame.IsFIN() {
			t.Fatalf("TCP response flags = %#x, want SYN", frame.Flags)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for TCP response")
	}

	chunk := make([]byte, bytestream.FrameSize)
	for sent := 0; sent < protocol.InitialStreamWindow; sent += len(chunk) {
		if err := stalled.WriteDAT(chunk); err != nil {
			t.Fatalf("fill stalled stream window at %d bytes: %v", sent, err)
		}
	}

	blockedWrite := make(chan error, 1)
	go func() { blockedWrite <- stalled.WriteDAT(chunk) }()
	select {
	case err := <-blockedWrite:
		t.Fatalf("write beyond stalled stream window returned early: %v", err)
	case <-time.After(150 * time.Millisecond):
	}

	shellInR, shellInW := io.Pipe()
	shellOutR, shellOutW := io.Pipe()
	defer shellInR.Close()
	defer shellInW.Close()
	defer shellOutR.Close()
	defer shellOutW.Close()

	type shellResult struct {
		result *ShellResult
		err    error
	}
	shellDone := make(chan shellResult, 1)
	argv := shellHelperArgv(t, "cat")
	go func() {
		result, err := sess.Shell(ShellOptions{
			Argv:   argv,
			Env:    map[string]string{shellHelperEnv: "1"},
			Stdin:  shellInR,
			Stdout: shellOutW,
		})
		shellDone <- shellResult{result: result, err: err}
	}()

	marker := []byte("shell-progress-42")
	writeDone := make(chan error, 1)
	go func() {
		_, err := shellInW.Write(marker)
		writeDone <- err
	}()
	readDone := make(chan error, 1)
	go func() {
		got := make([]byte, len(marker))
		_, err := io.ReadFull(shellOutR, got)
		if err == nil && !bytes.Equal(got, marker) {
			err = fmt.Errorf("shell output %q, want %q", got, marker)
		}
		readDone <- err
	}()
	select {
	case err := <-readDone:
		if err != nil {
			t.Fatalf("shell while TCP stream stalled: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("shell stream blocked behind stalled TCP stream")
	}
	select {
	case err := <-writeDone:
		if err != nil {
			t.Fatalf("write shell marker: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("shell input write did not complete")
	}
	_ = shellInW.Close()
	select {
	case got := <-shellDone:
		if got.err != nil {
			t.Fatalf("close shell while TCP stream stalled: %v", got.err)
		}
		if got.result.ExitCode != 0 {
			t.Fatalf("shell while TCP stream stalled exit = %d, want 0", got.result.ExitCode)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("shell did not exit after stdin closed")
	}

	drainDone := make(chan error, 1)
	go func() {
		_, err := io.CopyN(io.Discard, targetConn, int64(protocol.InitialStreamWindow+len(chunk)))
		drainDone <- err
	}()
	select {
	case err := <-blockedWrite:
		if err != nil {
			t.Fatalf("stalled TCP write after target resumed: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("stalled TCP write did not resume after target drained")
	}
	if err := stalled.WriteFIN(nil); err != nil {
		t.Fatalf("close stalled TCP write direction: %v", err)
	}
	select {
	case err := <-drainDone:
		if err != nil {
			t.Fatalf("drain stalled target: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("stalled target did not receive the complete payload")
	}
	_ = targetConn.Close()

	select {
	case frame, ok := <-stalled.Inbox():
		if !ok || !frame.IsFIN() {
			t.Fatalf("stalled TCP teardown frame = %#v, open=%v; want FIN", frame, ok)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("stalled TCP stream did not tear down")
	}
}

func TestSession_TCPForwardSetupFailureClosesClientDirection(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: spins up real pion peer connections and a TCP listener")
	}
	id := ephemeralID(t)
	relay := newFakeSignaling()
	t.Cleanup(relay.Close)
	handler := &rejectingTCPHandler{fin: make(chan uint32, 1)}
	startListener(relay.host(), id, handler)
	waitRegistered(t, relay)
	sess := mustDial(t, relay.host(), id, "tcp")
	t.Cleanup(sess.Close)

	localPort := unusedTCPPorts(t, 1)[0]
	forwarder, err := sess.StartLocalForwarding([]tcpforward.Forward{{
		LocalPort: localPort,
		Host:      "rejected.internal",
		Port:      80,
	}}, false)
	if err != nil {
		t.Fatalf("StartLocalForwarding: %v", err)
	}
	t.Cleanup(forwarder.Close)

	conn, err := net.DialTimeout("tcp4", fmt.Sprintf("127.0.0.1:%d", localPort), 2*time.Second)
	if err != nil {
		t.Fatalf("dial local forward: %v", err)
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := conn.Read(make([]byte, 1)); err == nil {
		t.Fatal("rejected forward remained open")
	}

	select {
	case <-handler.fin:
	case <-time.After(2 * time.Second):
		t.Fatal("client did not close its side of the rejected TCP stream")
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
