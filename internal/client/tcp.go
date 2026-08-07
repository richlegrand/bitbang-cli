package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sync"

	"github.com/richlegrand/bitbang/internal/bytestream"
	"github.com/richlegrand/bitbang/internal/protocol"
	"github.com/richlegrand/bitbang/internal/tcpforward"
)

// LocalForwarder owns atomically-bound local listeners and all accepted
// connections for one connector session.
type LocalForwarder struct {
	session *Session
	ctx     context.Context
	cancel  context.CancelFunc

	closeOnce sync.Once
	listeners []net.Listener
	mu        sync.Mutex
	conns     map[net.Conn]struct{}
	acceptWG  sync.WaitGroup
	connWG    sync.WaitGroup
}

type tcpStatus struct {
	Status string `json:"status"`
	Error  string `json:"error"`
}

// StartLocalForwarding binds every mapping before accepting any connection.
// If one bind fails, all earlier binds are released and no forward is started.
func (s *Session) StartLocalForwarding(forwards []tcpforward.Forward, gateway bool) (*LocalForwarder, error) {
	ctx, cancel := context.WithCancel(context.Background())
	f := &LocalForwarder{
		session: s,
		ctx:     ctx,
		cancel:  cancel,
		conns:   make(map[net.Conn]struct{}),
	}
	for _, forward := range forwards {
		if forward.LocalPort < 1 || forward.LocalPort > 65535 {
			f.closeBound()
			return nil, fmt.Errorf("-L %s: local port is outside 1-65535", forward)
		}
		if err := tcpforward.ValidateTarget(forward.Host, forward.Port); err != nil {
			f.closeBound()
			return nil, fmt.Errorf("-L %s: %w", forward, err)
		}
		ln, err := net.Listen("tcp4", forward.BindAddress(gateway))
		if err != nil {
			f.closeBound()
			return nil, fmt.Errorf("bind %s: %w", forward.BindAddress(gateway), err)
		}
		f.listeners = append(f.listeners, ln)
	}

	for i, ln := range f.listeners {
		f.acceptWG.Add(1)
		go f.acceptLoop(ln, forwards[i])
	}
	go func() {
		select {
		case <-s.Done():
			f.Close()
		case <-f.ctx.Done():
		}
	}()
	return f, nil
}

func (f *LocalForwarder) acceptLoop(ln net.Listener, forward tcpforward.Forward) {
	defer f.acceptWG.Done()
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		if !f.track(conn) {
			_ = conn.Close()
			return
		}
		f.connWG.Add(1)
		go func() {
			defer f.connWG.Done()
			defer f.untrack(conn)
			if err := f.forward(conn, forward); err != nil && f.ctx.Err() == nil {
				fmt.Fprintf(stderr, "Forward %s failed: %v\n", forward, err)
			}
		}()
	}
}

func (f *LocalForwarder) forward(conn net.Conn, forward tcpforward.Forward) error {
	defer conn.Close()
	st := f.session.OpenStream()
	established := false
	defer func() {
		if !established {
			// The listener's SYN|FIN closes only its direction. Close ours too
			// so it can release the per-stream routing entry after setup fails.
			_ = st.WriteFIN(nil)
		}
		st.Close()
	}()

	syn, _ := json.Marshal(protocol.TCPOpen{Type: "tcp", Host: forward.Host, Port: forward.Port})
	if err := st.WriteSYN(syn); err != nil {
		return fmt.Errorf("open stream: %w", err)
	}

	var first protocol.Frame
	select {
	case <-f.ctx.Done():
		return f.ctx.Err()
	case frame, ok := <-st.Inbox():
		if !ok {
			return errors.New("stream closed before tcp response")
		}
		first = frame
	}
	if !first.IsSYN() {
		return fmt.Errorf("expected tcp SYN response, got flags %#x", first.Flags)
	}
	var status tcpStatus
	if err := json.Unmarshal(first.Payload, &status); err != nil {
		return fmt.Errorf("parse tcp response: %w", err)
	}
	if status.Status != "ok" {
		if status.Error == "" {
			status.Error = status.Status
		}
		return errors.New(status.Error)
	}
	if first.IsFIN() {
		return errors.New("tcp stream closed during setup")
	}
	established = true

	ctx, cancel := context.WithCancel(f.ctx)
	defer cancel()
	watchDone := make(chan struct{})
	defer close(watchDone)
	go func() {
		select {
		case <-ctx.Done():
			// A v4 data write may be waiting for stream credit. Closing the
			// local socket alone cannot wake that wait; abandoning the stream can.
			st.Close()
			_ = conn.Close()
		case <-watchDone:
		}
	}()
	done := make(chan error, 2)
	go func() {
		_, err := bytestream.Pump(ctx, conn, st)
		done <- err
	}()
	go func() { done <- receiveTCP(ctx, conn, st.Inbox()) }()

	for i := 0; i < 2; i++ {
		err := <-done
		if err != nil && ctx.Err() == nil {
			cancel()
			_ = conn.Close()
		}
	}
	return nil
}

func receiveTCP(ctx context.Context, conn net.Conn, inbox <-chan protocol.Frame) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case frame, ok := <-inbox:
			if !ok {
				return errors.New("tcp stream closed without FIN")
			}
			if frame.IsFIN() {
				return bytestream.CloseWrite(conn)
			}
			if frame.IsSYN() || len(frame.Payload) == 0 {
				continue
			}
			if err := bytestream.WriteFull(conn, frame.Payload); err != nil {
				return err
			}
		}
	}
}

func (f *LocalForwarder) track(conn net.Conn) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ctx.Err() != nil {
		return false
	}
	f.conns[conn] = struct{}{}
	return true
}

func (f *LocalForwarder) untrack(conn net.Conn) {
	f.mu.Lock()
	delete(f.conns, conn)
	f.mu.Unlock()
}

func (f *LocalForwarder) closeBound() {
	f.cancel()
	for _, ln := range f.listeners {
		_ = ln.Close()
	}
}

// Close releases local listeners and active connections, then waits for their
// forwarding goroutines to exit.
func (f *LocalForwarder) Close() {
	f.closeOnce.Do(func() {
		f.closeBound()
		// Accept loops own every connWG.Add call. Waiting for them first
		// guarantees no positive Add can race the final connWG.Wait.
		f.acceptWG.Wait()
		f.mu.Lock()
		conns := make([]net.Conn, 0, len(f.conns))
		for conn := range f.conns {
			conns = append(conns, conn)
		}
		f.mu.Unlock()
		for _, conn := range conns {
			_ = conn.Close()
		}
		f.connWG.Wait()
	})
}
