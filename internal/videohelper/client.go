// Package videohelper bridges a secondary "video" PeerConnection to an
// external media process (e.g. a Python aiortc helper driving the camera).
//
// The device (this Go binary) owns the bitba.ng signaling connection and the
// data PeerConnection. Video stays in the helper process: this package shuttles
// the video PC's SDP/ICE between the helper and the per-session VideoBridge,
// which in turn relays them to the browser over the *already-verified* data
// channel (stream-0 control frames). So the video PC inherits the data
// channel's trust and needs no signaling-server changes.
//
// Transport to the helper is an inherited socketpair FD (passed by the parent
// that spawned us), carrying newline-delimited JSON. Messages are keyed by
// `client` so one helper process serves every concurrent browser session.
//
// Wire protocol (device ⇄ helper):
//
//	device → helper  {"kind":"open",      "client":"<id>"}
//	helper → device  {"kind":"offer",     "client":"<id>", "sdp":"..."}
//	device → helper  {"kind":"answer",    "client":"<id>", "sdp":"..."}
//	helper → device  {"kind":"candidate", "client":"<id>", "candidate":{...}}
//	device → helper  {"kind":"candidate", "client":"<id>", "candidate":{...}}
//	device → helper  {"kind":"close",     "client":"<id>"}
package videohelper

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"sync"
)

type wireMsg struct {
	Kind       string                   `json:"kind"`
	Client     string                   `json:"client"`
	SDP        string                   `json:"sdp,omitempty"`
	Candidate  map[string]interface{}   `json:"candidate,omitempty"`
	IceServers []map[string]interface{} `json:"ice_servers,omitempty"`
}

// Client is one connection to the helper process, shared across all sessions.
type Client struct {
	conn net.Conn

	mu      sync.Mutex // guards enc writes and the bridges map (+ bridge callbacks)
	enc     *json.Encoder
	bridges map[string]*Bridge
}

// DialFD adopts an inherited socketpair FD (passed by the spawning parent) as
// the helper transport. The FD must be a connected stream socket.
func DialFD(fd int) (*Client, error) {
	f := os.NewFile(uintptr(fd), "videohelper")
	if f == nil {
		return nil, fmt.Errorf("invalid video helper fd %d", fd)
	}
	conn, err := net.FileConn(f) // dups the fd
	f.Close()
	if err != nil {
		return nil, fmt.Errorf("video helper fd %d: %w", fd, err)
	}
	c := &Client{
		conn:    conn,
		enc:     json.NewEncoder(conn),
		bridges: make(map[string]*Bridge),
	}
	go c.readLoop()
	return c, nil
}

func (c *Client) readLoop() {
	dec := json.NewDecoder(c.conn)
	for {
		var m wireMsg
		if err := dec.Decode(&m); err != nil {
			log.Printf("videohelper: connection closed: %v", err)
			return
		}
		c.mu.Lock()
		b := c.bridges[m.Client]
		var onOffer func(string)
		var onCand func(map[string]interface{})
		if b != nil {
			onOffer = b.onOffer
			onCand = b.onCandidate
		}
		c.mu.Unlock()

		switch m.Kind {
		case "offer":
			if onOffer != nil {
				onOffer(m.SDP)
			}
		case "candidate":
			if onCand != nil {
				onCand(m.Candidate)
			}
		}
	}
}

func (c *Client) send(m wireMsg) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.enc.Encode(m); err != nil {
		log.Printf("videohelper: send %q for %s: %v", m.Kind, m.Client, err)
	}
}

// Bridge returns the per-session VideoBridge for a browser client. iceServers
// (from the data PC's signaling) are forwarded to the helper so its video PC
// can use the same STUN/TURN — needed when the peers have no direct path. The
// returned value satisfies session.VideoBridge (structurally — no import cycle).
func (c *Client) Bridge(clientID string, iceServers []map[string]interface{}) *Bridge {
	b := &Bridge{client: clientID, c: c, iceServers: iceServers}
	c.mu.Lock()
	c.bridges[clientID] = b
	c.mu.Unlock()
	return b
}

// UpdateICEServers replaces the ICE servers a client's bridge forwards to the
// helper. Called when the data channel's TURN fallback fetches relay creds, so
// a video PC created afterward (its open is sent at session-ready, after the
// fallback on a relay path) carries the same STUN/TURN and can gather relay
// candidates instead of host-only.
func (c *Client) UpdateICEServers(clientID string, iceServers []map[string]interface{}) {
	c.mu.Lock()
	if b := c.bridges[clientID]; b != nil {
		b.iceServers = iceServers
	}
	c.mu.Unlock()
}

// Bridge is the per-session relay handed to a Session. It forwards the browser
// side (Start/Answer/Candidate/Close) to the helper, and the helper side
// (offer/candidate) back to the Session via the callbacks given to Start.
type Bridge struct {
	client     string
	c          *Client
	iceServers []map[string]interface{}

	// Set once under c.mu in Start, read under c.mu in readLoop.
	onOffer     func(sdp string)
	onCandidate func(map[string]interface{})
}

func (b *Bridge) Start(onOffer func(sdp string), onCandidate func(map[string]interface{})) {
	b.c.mu.Lock()
	b.onOffer = onOffer
	b.onCandidate = onCandidate
	ice := b.iceServers
	b.c.mu.Unlock()
	b.c.send(wireMsg{Kind: "open", Client: b.client, IceServers: ice})
}

func (b *Bridge) Answer(sdp string) {
	b.c.send(wireMsg{Kind: "answer", Client: b.client, SDP: sdp})
}

func (b *Bridge) Candidate(cand map[string]interface{}) {
	b.c.send(wireMsg{Kind: "candidate", Client: b.client, Candidate: cand})
}

func (b *Bridge) Close() {
	b.c.send(wireMsg{Kind: "close", Client: b.client})
	b.c.mu.Lock()
	delete(b.c.bridges, b.client)
	b.c.mu.Unlock()
}
