package share

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/richlegrand/bitbang/internal/auth"
	"github.com/richlegrand/bitbang/internal/identity"
	"github.com/richlegrand/bitbang/internal/peer"
	"github.com/richlegrand/bitbang/internal/protocol"
	"github.com/richlegrand/bitbang/internal/session"
	"github.com/richlegrand/bitbang/internal/shellweb"
	"github.com/richlegrand/bitbang/internal/signaling"
	"github.com/richlegrand/bitbang/internal/streamtype"
)

const (
	// MaxViewers is the largest accepted --max-viewers value.
	MaxViewers = 1024
	// MaxTTL is the longest finite share lifetime.
	MaxTTL = 365 * 24 * time.Hour

	peerEstablishTimeout = 60 * time.Second
	refusedPeerGrace     = 20 * time.Second
	sourceCheckInterval  = 5 * time.Second
	maxPendingPeerBytes  = 256 << 10
)

// ErrPreempted means another process registered the worker's ephemeral UID.
var ErrPreempted = signaling.ErrPreempted

// WorkerConfig describes one tmux session publication.
type WorkerConfig struct {
	SessionID   string
	SessionName string
	MgmtSession string
	StateDir    string
	Server      string
	Socket      string
	TTL         time.Duration
	MaxViewers  int
	ReadOnly    bool
	Verbose     bool

	// Nonce names the start attempt this worker belongs to. It is
	// written into the state file and means nothing to the worker
	// itself; the parent uses it to tell this worker's state from a
	// predecessor's leftovers. See State.Nonce.
	Nonce string

	Runner Runner
	Env    []string
}

// Worker owns signaling, peer admission, and teardown for one share.
type Worker struct {
	cfg       WorkerConfig
	runner    Runner
	id        *identity.Identity
	signaling *signaling.Client

	controlCode string
	viewCode    string
	controlURL  string
	viewURL     string

	controlArgv []string
	viewArgv    []string
	forcedEnv   []string
	controlWeb  http.Handler
	viewWeb     http.Handler
	control     *roleSlots
	viewers     *roleSlots

	mu        sync.Mutex
	peers     map[string]*sharePeer
	closed    bool
	startedAt time.Time
	expiresAt time.Time
	readyOnce sync.Once
	errOnce   sync.Once
	errs      chan error
}

// NewWorker validates cfg and creates the ephemeral role credentials.
func NewWorker(cfg WorkerConfig) (*Worker, error) {
	if cfg.SessionID == "" || cfg.MgmtSession == "" || cfg.StateDir == "" {
		return nil, errors.New("share worker: session, management session, and state directory are required")
	}
	if cfg.Server == "" {
		cfg.Server = "bitba.ng"
	}
	if cfg.MaxViewers < 0 || cfg.MaxViewers > MaxViewers {
		return nil, fmt.Errorf("share worker: max viewers must be between 0 and %d", MaxViewers)
	}
	if cfg.TTL < 0 || cfg.TTL > MaxTTL || cfg.TTL%time.Second != 0 {
		return nil, fmt.Errorf("share worker: TTL must be 0 or a whole number of seconds up to %s", MaxTTL)
	}
	if cfg.Runner == nil {
		cfg.Runner = NewRunner(cfg.Socket)
	}
	if cfg.Env == nil {
		cfg.Env = os.Environ()
	}

	id, err := identity.Load("share", true)
	if err != nil {
		return nil, fmt.Errorf("create ephemeral identity: %w", err)
	}
	controlCode := ""
	viewCode := id.Code
	if !cfg.ReadOnly {
		controlCode = id.Code
		viewCode, err = distinctAccessCode(controlCode, NewAccessCode)
		if err != nil {
			return nil, fmt.Errorf("create view credential: %w", err)
		}
	}

	tmuxBase := []string{"tmux"}
	if cfg.Socket != "" {
		tmuxBase = append(tmuxBase, "-S", cfg.Socket)
	}
	controlArgv := append(append([]string(nil), tmuxBase...), "attach-session", "-t", cfg.SessionID)
	viewArgv := append(append([]string(nil), tmuxBase...), "attach-session", "-r", "-t", cfg.SessionID)

	sc := signaling.NewClient(cfg.Server, id)
	sc.Verbose = cfg.Verbose
	sc.WantCode = false
	sc.OnPreempted = func() {}

	w := &Worker{
		cfg:         cfg,
		runner:      cfg.Runner,
		id:          id,
		signaling:   sc,
		controlCode: controlCode,
		viewCode:    viewCode,
		controlArgv: controlArgv,
		viewArgv:    viewArgv,
		forcedEnv:   AttachEnv(cfg.Env),
		controlWeb:  shellweb.New().HTTPHandler(),
		viewWeb:     shellweb.New(shellweb.WithViewOnly()).HTTPHandler(),
		control:     newRoleSlots(1, "share already has a controller -- try again after they disconnect"),
		viewers:     newRoleSlots(cfg.MaxViewers, fmt.Sprintf("share is full (max %d viewers)", cfg.MaxViewers)),
		peers:       make(map[string]*sharePeer),
		errs:        make(chan error, 1),
	}
	if cfg.ReadOnly {
		w.control = newRoleSlots(0, "this share is view-only")
	}
	w.controlURL = ""
	if !cfg.ReadOnly {
		w.controlURL = sc.CodeURL(controlCode, "ephemeral")
	}
	w.viewURL = sc.CodeURL(viewCode, "ephemeral")
	return w, nil
}

func distinctAccessCode(exclude string, generate func() (string, error)) (string, error) {
	for range 8 {
		code, err := generate()
		if err != nil {
			return "", err
		}
		if code != exclude {
			return code, nil
		}
	}
	return "", errors.New("could not generate a distinct access code")
}

// Run publishes the target until the context is canceled, the TTL expires,
// the source session disappears, or signaling is preempted.
func (w *Worker) Run(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	w.mu.Lock()
	if !w.startedAt.IsZero() {
		w.mu.Unlock()
		return errors.New("share worker can only run once")
	}
	w.startedAt = time.Now()
	if w.cfg.TTL > 0 {
		w.expiresAt = w.startedAt.Add(w.cfg.TTL)
	}
	w.mu.Unlock()
	defer w.shutdown()
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	if _, err := w.runner.Run("has-session", "-t", w.cfg.SessionID); err != nil {
		return fmt.Errorf("source session is not running: %w", err)
	}
	w.signaling.OnReady = w.onReady

	connectDone := make(chan error, 1)
	go func() { connectDone <- w.signaling.Connect(w.handleMessage) }()

	sourceGone := watchSource(runCtx, w.runner, w.cfg.SessionID, sourceCheckInterval)
	var ttl *time.Timer
	var ttlC <-chan time.Time
	if w.cfg.TTL > 0 {
		ttl = time.NewTimer(w.cfg.TTL)
		ttlC = ttl.C
		defer ttl.Stop()
	}

	select {
	case <-ctx.Done():
		log.Printf("Share ending: requested")
		return nil
	case <-ttlC:
		log.Printf("Share ending: TTL expired")
		return nil
	case <-sourceGone:
		log.Printf("Share ending: source session gone")
		return nil
	case err := <-w.errs:
		return err
	case err := <-connectDone:
		if errors.Is(err, signaling.ErrPreempted) {
			return ErrPreempted
		}
		if err != nil {
			return fmt.Errorf("signaling stopped: %w", err)
		}
		return nil
	}
}

// authorize classifies a presented access code into a role.
//
// Both comparisons run on every call, so the timing of a rejection does
// not say which of the two codes a guess was tested against. That holds
// for contents only: subtle.ConstantTimeCompare returns as soon as the
// lengths differ, so a candidate of the wrong length stays
// distinguishable as one.
//
// A read-only share issues a single credential -- the identity's own
// code, serving as the view code -- and grants control to nothing.
func (w *Worker) authorize(code string) (protocol.Access, bool) {
	// With no control code to test against, the probe is pointed at the
	// view code so both comparisons still run.
	controlProbe := w.controlCode
	if w.cfg.ReadOnly {
		controlProbe = w.viewCode
	}
	isControl := subtle.ConstantTimeCompare([]byte(code), []byte(controlProbe)) == 1
	isView := subtle.ConstantTimeCompare([]byte(code), []byte(w.viewCode)) == 1
	if isControl && !w.cfg.ReadOnly {
		return protocol.AccessControl, true
	}
	if isView {
		return protocol.AccessView, true
	}
	return protocol.AccessDefault, false
}

// onReady writes the state file after the first successful signaling
// registration. The parent command blocks on that file appearing, so
// writing it here is what guarantees the URLs it prints are already
// answerable.
func (w *Worker) onReady() {
	w.readyOnce.Do(func() {
		w.mu.Lock()
		if w.closed {
			w.mu.Unlock()
			return
		}
		state := &State{
			Socket:      w.cfg.Socket,
			SessionID:   w.cfg.SessionID,
			SessionName: w.cfg.SessionName,
			MgmtSession: w.cfg.MgmtSession,
			UID:         w.id.UID,
			Server:      w.cfg.Server,
			Nonce:       w.cfg.Nonce,
			ControlURL:  w.controlURL,
			ViewURL:     w.viewURL,
			MaxViewers:  w.cfg.MaxViewers,
			CreatedAt:   w.startedAt,
			ExpiresAt:   w.expiresAt,
			TTLSeconds:  int(w.cfg.TTL / time.Second),
		}
		err := SaveState(w.cfg.StateDir, state)
		w.mu.Unlock()
		if err != nil {
			w.report(fmt.Errorf("write share state: %w", err))
			return
		}
		log.Printf("Share registered: session %s (%s), TTL %s", w.cfg.SessionID, w.cfg.SessionName, w.cfg.TTL)
	})
}

func (w *Worker) report(err error) {
	w.errOnce.Do(func() { w.errs <- err })
}

func (w *Worker) handleMessage(msg signaling.Message) {
	switch msgType, _ := msg["type"].(string); msgType {
	case "request":
		w.handleRequest(msg)
	case "answer":
		w.handleAnswer(msg)
	case "candidate":
		w.handleCandidate(msg)
	case "error":
		log.Printf("Signaling error: %v", msg["message"])
	}
}

// handleRequest builds a peer connection for an incoming request and
// starts its establishment deadline. A request over the connection cap,
// or one reusing a client ID that is already active, is dropped in
// silence: there is no data channel yet to explain a refusal over.
//
// The cap is re-checked once the connection exists, because building it
// runs without the lock and a shutdown can finish in that gap.
// Signaling dispatches serially today, so a second request cannot
// arrive meanwhile; the check does not rely on that staying true.
func (w *Worker) handleRequest(msg signaling.Message) {
	clientID, _ := msg["client_id"].(string)
	if clientID == "" {
		return
	}

	w.mu.Lock()
	maxPeers := w.cfg.MaxViewers + 5
	full := w.closed || len(w.peers) >= maxPeers
	_, duplicate := w.peers[clientID]
	w.mu.Unlock()
	if full || duplicate {
		log.Printf("Rejecting %s: share is at its connection limit or client ID is already active", clientID)
		return
	}

	p := &sharePeer{}
	conn, err := peer.HandleRequest(msg, w.signaling, w.id, p.handleMessage, w.cfg.Verbose)
	if err != nil {
		log.Printf("Failed to create peer connection: %v", err)
		return
	}
	p.setConn(conn)
	conn.Authorize = w.authorize

	w.mu.Lock()
	if w.closed || len(w.peers) >= maxPeers || w.peers[clientID] != nil {
		w.mu.Unlock()
		conn.Close()
		return
	}
	w.peers[clientID] = p
	w.mu.Unlock()

	conn.SetOnClose(func() { w.dropPeer(clientID, p, "connection closed") })
	p.armEstablishment(peerEstablishTimeout, func() {
		w.dropPeer(clientID, p, "did not finish connecting within "+peerEstablishTimeout.String())
	})
}

// handleAnswer verifies the connector's answer and admits the peer. A
// rejected answer closes the connection: a wrong code or a mismatched
// fingerprint gets no second attempt on the same handshake, and pion
// refuses a repeat answer anyway.
func (w *Worker) handleAnswer(msg signaling.Message) {
	clientID, _ := msg["client_id"].(string)
	p := w.peer(clientID)
	if p == nil {
		return
	}
	sdp, _ := msg["sdp"].(string)
	encrypted, _ := msg["encrypted_request"].(string)
	if err := p.conn.HandleAnswer(sdp, encrypted); err != nil {
		log.Printf("Failed to handle answer: %v", err)
		p.conn.Close()
		return
	}
	w.buildSession(clientID, p)
}

func (w *Worker) handleCandidate(msg signaling.Message) {
	clientID, _ := msg["client_id"].(string)
	p := w.peer(clientID)
	if p == nil {
		return
	}
	candidate, _ := msg["candidate"].(map[string]interface{})
	if err := p.conn.AddICECandidate(candidate); err != nil && w.cfg.Verbose {
		log.Printf("Candidate for %s: %v", clientID, err)
	}
}

func (w *Worker) peer(clientID string) *sharePeer {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return nil
	}
	return w.peers[clientID]
}

// buildSession admits an authorized peer: it pins the peer to the tmux
// command its role allows, takes the matching role slot, and installs
// the session. Frames that arrive before that point are queued by
// handleMessage and delivered once the session exists, so nothing is
// lost and nothing runs before a role has been assigned.
//
// A peer whose role is full still gets a session, backed by a handler
// that refuses every terminal and says why. Dropping the connection
// outright would reach the browser as a failed connection, pointing the
// user at the wrong cause.
func (w *Worker) buildSession(clientID string, p *sharePeer) {
	access := p.conn.Access()
	sh := streamtype.NewShell(nil, w.cfg.Verbose)
	sh.ForcedEnv = w.forcedEnv

	var front http.Handler
	var slots *roleSlots
	switch access {
	case protocol.AccessControl:
		sh.ForcedArgv = w.controlArgv
		front = w.controlWeb
		slots = w.control
	case protocol.AccessView:
		sh.ForcedArgv = w.viewArgv
		sh.ViewOnly = true
		front = w.viewWeb
		slots = w.viewers
	default:
		w.dropPeer(clientID, p, "answer did not grant a role")
		return
	}

	release, busy := slots.acquire()
	refused := release == nil
	if refused {
		log.Printf("Peer %s refused: %s", clientID, busy)
		sh.AcquireSlot = func() (func(), string) { return nil, busy }
	} else if !p.hold(release) {
		release()
		sh.Close()
		return
	} else {
		var streamTaken bool
		var streamMu sync.Mutex
		sh.AcquireSlot = func() (func(), string) {
			streamMu.Lock()
			defer streamMu.Unlock()
			if p.isClosed() {
				return nil, "this connection is closing"
			}
			if streamTaken {
				return nil, "this connection already has a terminal open"
			}
			streamTaken = true
			return func() {
				streamMu.Lock()
				streamTaken = false
				streamMu.Unlock()
			}, ""
		}
	}

	sess := session.New(p.conn.DC, auth.New(""), w.cfg.Verbose,
		sh, streamtype.NewHTTPLocal(front, w.cfg.Verbose))
	sess.Access = access
	sess.OnReady = p.markEstablished
	if !p.publish(sh, sess) {
		sh.Close()
		return
	}
	if refused {
		p.armRefusal(refusedPeerGrace, func() {
			w.dropPeer(clientID, p, "refused peer's grace elapsed")
		})
	}
	log.Printf("Peer %s authorized: %s", clientID, access)
}

// dropPeer tears a peer down and forgets it. Every trigger calls this;
// only the call that does the work logs.
func (w *Worker) dropPeer(clientID string, p *sharePeer, why string) {
	if !p.teardown() {
		return
	}
	w.mu.Lock()
	if w.peers[clientID] == p {
		delete(w.peers, clientID)
	}
	w.mu.Unlock()
	log.Printf("Peer %s gone: %s", clientID, why)
}

// shutdown stops signaling and tears every peer down in parallel. It leaves
// state removal to a later CLI command, which can hold the target lifecycle
// lock across the read, ownership decision, and removal.
func (w *Worker) shutdown() {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return
	}
	w.closed = true
	peers := make([]*sharePeer, 0, len(w.peers))
	for _, p := range w.peers {
		peers = append(peers, p)
	}
	w.peers = make(map[string]*sharePeer)
	w.mu.Unlock()

	w.signaling.Stop()
	var wg sync.WaitGroup
	for _, p := range peers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p.teardown()
		}()
	}
	wg.Wait()
}

// watchSource closes the returned channel after a successful list-sessions
// response no longer contains the source. A failed tmux command is ignored
// because it cannot distinguish a missing session from an unreachable server.
func watchSource(ctx context.Context, runner Runner, sessionID string, interval time.Duration) <-chan struct{} {
	gone := make(chan struct{})
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				out, err := runner.Run("list-sessions", "-F", "#{session_id}")
				if err != nil {
					continue
				}
				if !listsSession(out, sessionID) {
					close(gone)
					return
				}
			}
		}
	}()
	return gone
}

// listsSession reports whether a `list-sessions -F '#{session_id}'`
// listing contains an exact session ID.
func listsSession(listing, sessionID string) bool {
	for _, line := range strings.Split(listing, "\n") {
		if strings.TrimSpace(line) == sessionID {
			return true
		}
	}
	return false
}
