// Package bytestream contains transport-independent helpers for carrying a
// byte stream over a framed transport.
package bytestream

import (
	"context"
	"errors"
	"io"
	"time"
)

const (
	// FrameSize keeps a framed chunk below the SCTP message-size ceiling.
	FrameSize = 32 << 10
	// MaxBufferedAmount bounds queued framed data before applying
	// backpressure, for every sender on the data channel.
	//
	// It is not just a memory bound. BufferedAmount counts bytes queued plus
	// bytes in flight but not yet acked, so this doubles as a send window:
	// throughput cannot exceed MaxBufferedAmount/RTT, and anything queued
	// beyond the bandwidth-delay product is pure added latency. At 8MB a
	// browser download pushed an interactive request from 2ms to 125ms on a
	// LAN and to 680ms at 80ms RTT. 1MB is at or under the BDP of an ordinary
	// path, so the standing queue goes away without capping the window: at
	// 40ms RTT it measured the same throughput as 8MB with a fifth of the
	// latency, and it matches the per-stream window SWSP v4 already enforces
	// (protocol.InitialStreamWindow). Sizing it from the measured rate and
	// RTT instead would drop the cap on fast, distant links.
	MaxBufferedAmount uint64 = 1 << 20
)

// FrameWriter is the transport surface Pump needs. It is deliberately free of
// WebRTC types so the same pump can back TCP, serial, or another byte stream.
type FrameWriter interface {
	WriteDAT([]byte) error
	WriteFIN([]byte) error
	BufferedAmount() uint64
}

// Pump copies src into DAT frames and sends FIN when the source direction
// ends. A non-EOF read error still ends that direction and therefore sends FIN;
// cancellation suppresses FIN because the whole session is already closing.
func Pump(ctx context.Context, src io.Reader, dst FrameWriter) (int64, error) {
	buf := make([]byte, FrameSize)
	var total int64
	for {
		n, readErr := src.Read(buf)
		if n > 0 {
			for dst.BufferedAmount() > MaxBufferedAmount {
				select {
				case <-ctx.Done():
					return total, ctx.Err()
				case <-time.After(time.Millisecond):
				}
			}
			select {
			case <-ctx.Done():
				return total, ctx.Err()
			default:
			}
			if err := dst.WriteDAT(buf[:n]); err != nil {
				return total, err
			}
			total += int64(n)
		}
		if readErr != nil {
			if ctx.Err() != nil {
				return total, ctx.Err()
			}
			finErr := dst.WriteFIN(nil)
			if readErr == io.EOF {
				return total, finErr
			}
			return total, errors.Join(readErr, finErr)
		}
	}
}

// WriteFull writes all of p, including when the writer accepts only a prefix.
func WriteFull(w io.Writer, p []byte) error {
	for len(p) > 0 {
		n, err := w.Write(p)
		if n < 0 || n > len(p) {
			return errors.New("invalid write count")
		}
		p = p[n:]
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrNoProgress
		}
	}
	return nil
}

// CloseWrite half-closes the write direction when the value supports it.
func CloseWrite(v interface{}) error {
	if cw, ok := v.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return nil
}

// CloseRead half-closes the read direction when the value supports it.
func CloseRead(v interface{}) error {
	if cr, ok := v.(interface{ CloseRead() error }); ok {
		return cr.CloseRead()
	}
	return nil
}
