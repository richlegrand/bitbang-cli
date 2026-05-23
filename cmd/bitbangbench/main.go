// BitBangBench - throughput benchmark for the SWSP/data-channel pipeline.
//
// Reuses identity, signaling, and peer plumbing from bitbangproxy but swaps
// the proxy handler for a minimal "blast bytes as fast as possible" handler.
// Open the printed URL in a browser and the device will stream a fixed
// amount of data and report throughput on both ends.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"sync"
	"time"

	"github.com/pion/webrtc/v4"

	"github.com/richlegrand/bitbang/internal/identity"
	"github.com/richlegrand/bitbang/internal/peer"
	"github.com/richlegrand/bitbang/internal/protocol"
	"github.com/richlegrand/bitbang/internal/signaling"
)

type benchHandler struct {
	dc          *webrtc.DataChannel
	chunkSize   int
	totalMB     int
	maxBuffered uint64
	noPressure  bool

	mu      sync.Mutex
	started bool
}

func (h *benchHandler) handleMessage(data []byte) {
	frame, err := protocol.ParseFrame(data)
	if err != nil {
		log.Printf("parse frame: %v", err)
		return
	}

	// Stream 0 is control. Reply "ready" to any connect message.
	if frame.StreamID == 0 && frame.IsSYN() {
		ready, _ := json.Marshal(map[string]string{"type": "ready"})
		_ = h.dc.Send(protocol.BuildFrame(0, protocol.FlagSYN|protocol.FlagFIN, ready))
		return
	}

	// First app request triggers a stream. Subsequent requests are ignored
	// so refresh/favicon/etc don't disturb a running benchmark.
	if frame.IsSYN() {
		h.mu.Lock()
		if h.started {
			h.mu.Unlock()
			// Reply with empty 200 so browser doesn't hang
			h.respond404(frame.StreamID)
			return
		}
		h.started = true
		h.mu.Unlock()

		go h.stream(frame.StreamID)
	}
}

func (h *benchHandler) respond404(streamID uint32) {
	meta, _ := json.Marshal(map[string]interface{}{
		"status":  404,
		"headers": map[string]string{"Content-Type": "text/plain"},
	})
	_ = h.dc.Send(protocol.BuildFrame(streamID, protocol.FlagSYN, meta))
	_ = h.dc.Send(protocol.BuildFrame(streamID, protocol.FlagFIN, nil))
}

func (h *benchHandler) stream(streamID uint32) {
	// Response headers
	meta, _ := json.Marshal(map[string]interface{}{
		"status": 200,
		"headers": map[string]string{
			"Content-Type":   "application/octet-stream",
			"Content-Length": fmt.Sprintf("%d", int64(h.totalMB)*1024*1024),
		},
	})
	if err := h.dc.Send(protocol.BuildFrame(streamID, protocol.FlagSYN, meta)); err != nil {
		log.Printf("send SYN: %v", err)
		return
	}

	chunk := make([]byte, h.chunkSize)
	totalBytes := int64(h.totalMB) * 1024 * 1024
	sent := int64(0)
	start := time.Now()
	nextLogMB := int64(50)

	mode := "backpressure"
	if h.noPressure {
		mode = "no-pressure (blast)"
	}
	log.Printf("Streaming %d MB in %d-byte chunks, mode=%s, maxBuffered=%d",
		h.totalMB, h.chunkSize, mode, h.maxBuffered)

	for sent < totalBytes {
		if !h.noPressure {
			for h.dc.BufferedAmount() > h.maxBuffered {
				time.Sleep(1 * time.Millisecond)
			}
		}

		n := h.chunkSize
		if int64(n) > totalBytes-sent {
			n = int(totalBytes - sent)
		}
		if err := h.dc.Send(protocol.BuildFrame(streamID, protocol.FlagDAT, chunk[:n])); err != nil {
			log.Printf("send DAT failed (sent=%d): %v", sent, err)
			return
		}
		sent += int64(n)

		mb := sent / (1024 * 1024)
		if mb >= nextLogMB {
			elapsed := time.Since(start).Seconds()
			speed := float64(mb) / elapsed
			log.Printf("Sent %d MB (%.1f MB/s, BufferedAmount=%d)",
				mb, speed, h.dc.BufferedAmount())
			nextLogMB += 50
		}
	}

	_ = h.dc.Send(protocol.BuildFrame(streamID, protocol.FlagFIN, nil))
	elapsed := time.Since(start).Seconds()
	speed := float64(sent) / 1024 / 1024 / elapsed
	log.Printf("DONE: %d MB in %.2fs (%.2f MB/s)",
		sent/(1024*1024), elapsed, speed)
}

func main() {
	server := flag.String("server", "bitba.ng", "Signaling server hostname")
	chunkSize := flag.Int("chunk", 32768, "Frame payload size (max 65535)")
	totalMB := flag.Int("mb", 500, "Total MB to stream")
	maxBuffered := flag.Int("buffer", 8<<20, "Backpressure threshold (BufferedAmount cap, bytes)")
	noPressure := flag.Bool("nopressure", false, "Disable backpressure (raw blast)")
	ephemeral := flag.Bool("ephemeral", false, "Use a temporary identity (new URL each run)")
	flag.Parse()

	if *chunkSize < 1 || *chunkSize > 65535 {
		fmt.Fprintf(os.Stderr, "chunk size must be 1..65535\n")
		os.Exit(1)
	}

	id, err := identity.Load("bitbangbench", *ephemeral)
	if err != nil {
		fmt.Fprintf(os.Stderr, "identity: %v\n", err)
		os.Exit(1)
	}

	url := fmt.Sprintf("https://%s/%s?debug", *server, id.UID)
	fmt.Println()
	fmt.Println("Open this URL in the browser to run the benchmark:")
	fmt.Println("  " + url)
	fmt.Printf("\nConfig: chunk=%d, totalMB=%d, maxBuffered=%d, noPressure=%v\n\n",
		*chunkSize, *totalMB, *maxBuffered, *noPressure)

	var mu sync.Mutex
	connections := make(map[string]*peer.Connection)
	handlers := make(map[string]*benchHandler)

	client := signaling.NewClient(*server, id)
	client.Verbose = true

	client.Connect(func(msg signaling.Message) {
		msgType, _ := msg["type"].(string)
		switch msgType {
		case "request":
			clientID, _ := msg["client_id"].(string)
			log.Printf("Connection from %s", clientID)

			h := &benchHandler{
				chunkSize:   *chunkSize,
				totalMB:     *totalMB,
				maxBuffered: uint64(*maxBuffered),
				noPressure:  *noPressure,
			}

			conn, err := peer.HandleRequest(msg, client, id, func(data []byte) {
				h.handleMessage(data)
			}, true)
			if err != nil {
				log.Printf("HandleRequest: %v", err)
				return
			}
			h.dc = conn.DC

			mu.Lock()
			connections[clientID] = conn
			handlers[clientID] = h
			mu.Unlock()

		case "answer":
			clientID, _ := msg["client_id"].(string)
			sdp, _ := msg["sdp"].(string)
			encrypted, _ := msg["encrypted_request"].(string)
			mu.Lock()
			conn := connections[clientID]
			mu.Unlock()
			if conn == nil {
				return
			}
			if err := conn.HandleAnswer(sdp, encrypted); err != nil {
				log.Printf("HandleAnswer: %v", err)
			}

		case "candidate":
			clientID, _ := msg["client_id"].(string)
			cand, _ := msg["candidate"].(map[string]interface{})
			mu.Lock()
			conn := connections[clientID]
			mu.Unlock()
			if conn == nil {
				return
			}
			if err := conn.AddICECandidate(cand); err != nil {
				log.Printf("AddICECandidate: %v", err)
			}

		case "error":
			log.Printf("Signaling error: %v", msg["message"])
		}
	})
}
