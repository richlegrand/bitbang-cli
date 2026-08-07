package main

// The share command publishes a running tmux session. A detached tmux
// management session supervises the worker, so status and cleanup use tmux's
// own liveness rather than PID files.

import (
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/term"

	"github.com/richlegrand/bitbang/internal/share"
)

// workerSubcommand is the hidden `bitbang share __worker` entry the
// management session runs. Internal -- not listed in usage.
const workerSubcommand = "__worker"

// startTimeout bounds how long the foreground command waits for the
// worker to register with signaling and publish its state file.
const startTimeout = 15 * time.Second

// stopGrace is how long a worker gets to exit after SIGTERM before the
// stop escalates, and killGrace how long after SIGKILL. Variables so a
// test can shrink them; nothing reassigns them in the binary.
var (
	stopGrace = 5 * time.Second
	killGrace = 2 * time.Second
)

// sweepProbeTimeout bounds each old server probe. sweepBudget bounds the
// complete best-effort sweep so stale sockets cannot delay every command.
var (
	sweepProbeTimeout = time.Second
	sweepBudget       = 2 * time.Second
)

// sweepStep is a test hook for interleavings between sweep entries.
var sweepStep = func() {}

// killProcess is replaceable in tests so they never signal real PIDs.
var killProcess = signalShareProcess

type shareConfig struct {
	readOnly   bool
	ttl        string
	target     string
	socket     string
	server     string
	maxViewers int
	verbose    bool

	// ttlDuration is ttl parsed and validated at flag time, so a bad
	// value fails before we touch tmux or a running share.
	ttlDuration time.Duration

	// set records explicitly supplied flags. A bare re-run prints the
	// existing URLs; explicitly conflicting settings are rejected.
	set map[string]bool
}

func registerShareFlags(fs *flag.FlagSet, cfg *shareConfig) {
	fs.BoolVar(&cfg.readOnly, "read-only", false, "Viewers only -- no control credential is generated at all")
	fs.StringVar(&cfg.ttl, "ttl", "1h", "Share lifetime (Go duration like 1h or 30m; 0 = until stopped)")
	fs.StringVar(&cfg.target, "target", "", "tmux session to share (name or $id; default: the session you're in)")
	fs.StringVar(&cfg.socket, "socket", "", "tmux server socket path (default: the enclosing server, or tmux's default)")
	fs.StringVar(&cfg.server, "server", "bitba.ng", "Signaling server hostname")
	fs.IntVar(&cfg.maxViewers, "max-viewers", 16, "Max concurrent view-only peers")
	fs.BoolVar(&cfg.verbose, "v", false, "Verbose logging")
}

// dispatchShare routes `bitbang share [subcommand] [flags]`. Bare
// `share` (or `share --flag`) starts/reprints a share; status, stop,
// and rotate manage an existing one.
func dispatchShare(args []string) {
	if len(args) == 0 || args[0] == "" || args[0][0] == '-' {
		runShare(args)
		return
	}
	switch args[0] {
	case "status":
		runShareStatus(args[1:])
	case "stop":
		runShareStop(args[1:])
	case "rotate":
		runShareRotate(args[1:])
	case workerSubcommand:
		runShareWorker(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "bitbang share: unknown subcommand %q (expected status, stop, or rotate)\n\n", args[0])
		printUsage()
		os.Exit(2)
	}
}

func parseShareFlags(name string, args []string) shareConfig {
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	var cfg shareConfig
	registerShareFlags(fs, &cfg)
	fs.Parse(reorderArgs(fs, args))
	if fs.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "bitbang %s: unexpected argument %q\n", name, fs.Arg(0))
		os.Exit(2)
	}
	cfg.set = make(map[string]bool)
	fs.Visit(func(f *flag.Flag) { cfg.set[f.Name] = true })

	ttl, err := parseTTL(cfg.ttl)
	if err != nil {
		fmt.Fprintf(os.Stderr, "bitbang %s: %v\n", name, err)
		os.Exit(2)
	}
	cfg.ttlDuration = ttl
	if cfg.maxViewers < 0 || cfg.maxViewers > share.MaxViewers {
		fmt.Fprintf(os.Stderr, "bitbang %s: --max-viewers must be between 0 and %d\n", name, share.MaxViewers)
		os.Exit(2)
	}
	return cfg
}

// maxTTL is the worker's own ceiling, restated here so --ttl is
// rejected at the flag rather than inside the worker. See share.MaxTTL
// for why there is a ceiling at all.
const maxTTL = share.MaxTTL

// parseTTL parses --ttl. "0" (or empty) means until stopped.
//
// Worker state stores whole seconds, so finer durations are rejected rather
// than truncated. In particular, truncating a sub-second TTL to zero would
// turn it into the "until stopped" sentinel.
func parseTTL(s string) (time.Duration, error) {
	if s == "" || s == "0" {
		return 0, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("bad --ttl %q (want a Go duration like 1h or 30m, or 0 for until-stopped)", s)
	}
	if d < time.Second {
		return 0, fmt.Errorf("--ttl %q is under a second; use at least 1s (or 0 to run until stopped)", s)
	}
	if d%time.Second != 0 {
		return 0, fmt.Errorf("--ttl %q is not a whole number of seconds; %s would be silently rounded down",
			s, d.Truncate(time.Second))
	}
	if d > maxTTL {
		return 0, fmt.Errorf("--ttl %q is over the %s maximum; use 0 to run until stopped", s, maxTTL)
	}
	return d, nil
}

// shareTarget bundles everything resolved from the flags: the tmux
// runner (bound to the right socket), the target session, and the
// derived state-dir / management-session names.
type shareTarget struct {
	cfg    shareConfig
	runner share.Runner
	target share.Target
	dir    string
	lock   string
	mgmt   string
}

func resolveShareTarget(cfg shareConfig) (*shareTarget, error) {
	if !shareHostSupported {
		return nil, errors.New("bitbang share requires tmux on Unix or WSL; native Windows can still open share URLs with bitbang connect")
	}
	if _, err := exec.LookPath("tmux"); err != nil {
		return nil, fmt.Errorf("tmux not found in PATH -- `bitbang share` attaches to a running tmux session; start one with `tmux` first")
	}
	socket := cfg.socket
	if socket == "" {
		socket = share.SocketFromEnv()
	}
	runner := share.NewRunner(socket)
	if err := share.CheckVersion(runner); err != nil {
		return nil, err
	}
	tgt, err := share.Discover(runner, cfg.target)
	if err != nil {
		return nil, err
	}
	// Key on the server's own answer, not the spelling we reached it
	// by, so `share status` from outside tmux finds the share that
	// `share` started from inside it.
	if tgt.Socket == "" {
		tgt.Socket = socket
	}
	hash := share.TargetHash(tgt.Socket, tgt.SessionID)
	dir, err := share.Dir(hash)
	if err != nil {
		return nil, err
	}
	lock, err := share.LockPath(hash)
	if err != nil {
		return nil, err
	}
	return &shareTarget{
		cfg:    cfg,
		runner: runner,
		target: tgt,
		dir:    dir,
		lock:   lock,
		mgmt:   "_bbshare_" + hash,
	}, nil
}

// lockTarget takes the non-blocking per-target lifecycle lock. It spans
// classification, cleanup, worker creation, and state acceptance so another
// command cannot replace the share between a decision and its effect. Workers
// do not hold it for the lifetime of a share.
func lockTarget(st *shareTarget) *share.Lock {
	if err := os.MkdirAll(filepath.Dir(st.lock), 0o700); err != nil {
		fmt.Fprintf(os.Stderr, "Cannot create %s: %v\n", filepath.Dir(st.lock), err)
		os.Exit(1)
	}
	lock, holder, err := share.TryLock(st.lock)
	if err == nil {
		return lock
	}
	if !errors.Is(err, share.ErrLockBusy) {
		fmt.Fprintf(os.Stderr, "Cannot lock %s: %v\n", st.lock, err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "Another `bitbang share` command is working on tmux session %q",
		st.target.SessionName)
	if holder > 0 {
		fmt.Fprintf(os.Stderr, " (pid %d)", holder)
	}
	fmt.Fprintln(os.Stderr, ".\nWait for it to finish and try again.")
	os.Exit(1)
	return nil
}

// enterTarget begins a share command: take the target's lifecycle lock,
// then collect what earlier shares left lying around.
func enterTarget(st *shareTarget) *share.Lock {
	lock := lockTarget(st)
	sweepStranded(st.dir)
	return lock
}

// sweepStranded removes the state directories of shares whose workers
// are gone, skipping the one this command is here for.
//
// It exists because a worker cannot clean up after itself and the
// ordinary stale path cannot always reach it. Every share command
// resolves its target by asking tmux for a session ID -- so when the
// *source* session is what disappeared, there is no lookup left that
// arrives at its directory, and a new session of the same name hashes
// somewhere else entirely. That directory would sit there with a dead
// share's URLs in it forever. The same is true of anything left by a
// worker that was killed outright, which is a case nothing else covers.
//
// Every removal is guarded the same way the targeted path is guarded.
// Each directory is taken under its own lock, non-blocking, so a
// directory another command is working on is simply left for next time;
// nothing here needs to succeed on any particular run. State that
// cannot be read is left alone, since it names no server to ask about.
// A share is removed only when a successful tmux listing shows its worker
// gone, or when the socket itself proves that no tmux server remains. Other
// probe failures preserve the state.
func sweepStranded(skip string) {
	base, err := share.BaseDir()
	if err != nil {
		return
	}
	entries, err := os.ReadDir(base)
	if err != nil {
		return
	}
	deadline := time.Now().Add(sweepBudget)
	warned := false
	for _, entry := range entries {
		if time.Now().After(deadline) {
			// Nothing here has to finish on any given run, and the
			// caller is waiting on a lock for work they did not ask for.
			log.Printf("Stopped tidying old share state after %s; the rest will be looked at next time", sweepBudget)
			return
		}
		name := entry.Name()
		if !entry.IsDir() {
			// A lock file whose target is gone. Taking it first is what
			// keeps this from deleting the lock of a command that is
			// between acquiring it and creating its directory.
			if dir, ok := strings.CutSuffix(name, ".lock"); ok && dir != "" {
				sweepOrphanLock(filepath.Join(base, dir))
			}
			continue
		}
		dir := filepath.Join(base, name)
		if dir == skip {
			continue
		}
		if socket := sweepOne(dir); socket != "" && !warned {
			// Once per command. A share nothing can account for is kept,
			// correctly -- but it is also paid for on every share command
			// from here on, and the operator can only act on that if
			// somebody says which one it is.
			warned = true
			log.Printf("An old share's tmux server at %s did not answer within %s; "+
				"leaving %s alone. Remove that directory if the share is finished.",
				socket, sweepProbeTimeout, dir)
		}
		sweepStep()
	}
}

func sweepOrphanLock(dir string) {
	lock, _, err := share.TryLock(share.LockPathFor(dir))
	if err != nil {
		return
	}
	defer lock.Release()
	if _, err := os.Stat(dir); err == nil || !errors.Is(err, fs.ErrNotExist) {
		return
	}
	_ = os.Remove(share.LockPathFor(dir))
}

// sweepOne collects one target's leavings, or leaves them where they
// are. It returns the socket it could not get an answer out of, so the
// caller can mention it once instead of once per directory.
//
// All evidence is collected after taking this target's lock. Reusing a
// listing collected for another target would allow a share started during
// the sweep to be mistaken for stale state.
func sweepOne(dir string) string {
	lock, _, err := share.TryLock(share.LockPathFor(dir))
	if err != nil {
		return "" // held by another command, or unusable -- either way, not now
	}
	defer lock.Release()

	st, err := share.LoadState(dir)
	if err != nil {
		return "" // unreadable: it names no server, so there is nothing to ask
	}
	if st == nil {
		// No state and no command working here: a directory whose share
		// never got as far as publishing, or whose state is already gone.
		removeStranded(dir)
		return ""
	}
	if st.MgmtSession == "" {
		return ""
	}

	// Bounded, because this is the one place a share command touches
	// servers it has no business waiting on. An old socket path taken
	// over by something that accepts connections and never speaks tmux
	// would otherwise hang every share, status, stop and rotate on this
	// machine.
	runner := share.NewRunner(st.Socket)
	runner.Timeout = sweepProbeTimeout

	out, err := runner.Run("list-panes", "-a", "-F", paneListFormat)
	if err != nil {
		// No answer. The only thing that settles it from here is a
		// socket with provably nothing behind it.
		if serverIsGone(st.Socket) {
			log.Printf("Removing stranded share state for tmux session %q at %s", st.SessionName, dir)
			removeStranded(dir)
			return ""
		}
		return st.Socket
	}

	switch state, _ := findPane(out, st.MgmtSession); state {
	case mgmtRunning:
		return ""
	case mgmtDead:
		// A husk kept listed by a global remain-on-exit. Its worker is
		// gone, but the name is still taken, and nothing else will ever
		// come back for it -- the targeted path needs a source session
		// that no longer exists. Reap it exactly, and keep the state if
		// that fails, so the next sweep tries again rather than leaving
		// a name with no record of what holds it.
		if _, err := runner.Run("kill-session", "-t", "="+st.MgmtSession); err != nil {
			log.Printf("Could not remove the dead management session %s: %v", st.MgmtSession, err)
			return ""
		}
	}
	log.Printf("Removing stranded share state for tmux session %q at %s", st.SessionName, dir)
	removeStranded(dir)
	return ""
}

// serverIsGone reports whether a tmux server is definitively absent
// from a socket. Asked only once tmux itself has failed to answer.
//
// A dead tmux server may leave its socket file behind, so this connects
// rather than using stat. ECONNREFUSED and ENOENT prove there is no server;
// permission and timeout errors do not.
//
// Bounded for the same reason the tmux call before it is: connect() on
// a unix socket blocks while the listener's backlog is full.
func serverIsGone(socket string) bool {
	if socket == "" {
		return false
	}
	conn, err := net.DialTimeout("unix", socket, sweepProbeTimeout)
	if err == nil {
		_ = conn.Close()
		return false
	}
	return errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, fs.ErrNotExist)
}

// removeStranded drops a finished share's directory and lock file. The caller
// holds that lock and has already established that no share remains.
func removeStranded(dir string) {
	if err := share.RemoveState(dir); err != nil {
		log.Printf("Could not remove stranded share state at %s: %v", dir, err)
		return
	}
	_ = os.Remove(share.LockPathFor(dir))
}

// tmux falls back to prefix matching for session names. "=name" forces an
// exact session target; commands that require a window use "=name:".
func (st *shareTarget) sessionTarget() string { return "=" + st.mgmt }
func (st *shareTarget) windowTarget() string  { return "=" + st.mgmt + ":" }

// mgmtState is what tmux can tell us about the management session.
// "unknown" exists because a failed probe is not an answer: a fork
// failure would otherwise read as "gone" and get a live share's state
// deleted.
type mgmtState int

const (
	mgmtGone    mgmtState = iota // tmux answered: no such session
	mgmtDead                     // tmux answered: session listed, its pane has exited
	mgmtRunning                  // tmux answered: session exists with a live pane
	mgmtUnknown                  // tmux could not be asked
)

// exited reports that the worker process is definitively not running.
// tmux spawned the pane's process and reaps it, so both a session that
// has gone and one left listed with a dead pane are proof of that; they
// differ only in whether a husk is left to clear away.
func (m mgmtState) exited() bool { return m == mgmtGone || m == mgmtDead }

// probeMgmtPane reports the management session's state and, when one is
// running, the PID of its pane.
//
// The target-independent list-panes call returns liveness and PID from one
// snapshot. A successful listing that omits the exact session name proves it
// is gone; a failed listing proves nothing. Dead panes and PIDs at or below 1
// are never signaled.
func probeMgmtPane(st *shareTarget) (mgmtState, int) {
	out, err := st.runner.Run("list-panes", "-a", "-F", paneListFormat)
	if err != nil {
		return mgmtUnknown, 0
	}
	return findPane(out, st.mgmt)
}

// paneListFormat is the one snapshot every liveness question is
// answered from, so the targeted probe and the sweep cannot come to
// different conclusions about the same pane.
const paneListFormat = "#{session_name}\t#{pane_dead}\t#{pane_pid}"

// findPane locates a session in a paneListFormat listing. Absent from a
// listing that worked means gone; a retained dead pane is not running.
func findPane(listing, session string) (mgmtState, int) {
	for _, line := range strings.Split(listing, "\n") {
		fields := strings.Split(strings.TrimRight(line, "\r"), "\t")
		if len(fields) < 3 || fields[0] != session {
			continue
		}
		if fields[1] == "1" {
			return mgmtDead, 0
		}
		pid, err := strconv.Atoi(strings.TrimSpace(fields[2]))
		if err != nil || pid <= 1 {
			return mgmtRunning, 0
		}
		return mgmtRunning, pid
	}
	return mgmtGone, 0
}

func probeMgmt(st *shareTarget) mgmtState {
	state, _ := probeMgmtPane(st)
	return state
}

// workerPID reports the management pane's process and whether tmux
// vouched for it being alive in the same breath.
func workerPID(st *shareTarget) (int, bool) {
	state, pid := probeMgmtPane(st)
	return pid, state == mgmtRunning && pid > 0
}

// shareLiveness is what a target's state directory and management
// session say between them.
type shareLiveness int

const (
	shareAbsent    shareLiveness = iota // nothing on disk, nothing running
	shareLive                           // state readable, worker running
	shareStale                          // state left by a worker that is gone
	shareUnmanaged                      // worker running, state missing or corrupt
)

// loadLiveShare classifies the target from one list-panes snapshot and
// the state file.
//
// The state file is never trusted alone. A worker whose state is
// unreadable is still serving its URLs, so treating that as stale -- and
// deleting the directory, as the callers do -- would strip the only
// record of a live share and leave a management session nobody can
// name. The same goes for state that has gone missing entirely, which
// is what a parent killed mid-startup leaves behind.
func loadLiveShare(st *shareTarget) (*share.State, shareLiveness) {
	s, err := share.LoadState(st.dir)
	switch probeMgmt(st) {
	case mgmtUnknown:
		// Never clean up on a guess. Callers treat this as live, which
		// costs a confusing message; treating it as gone would cost a
		// running share its state.
		log.Printf("Could not ask tmux whether the share is running; assuming it is")
		if err != nil || s == nil {
			return nil, shareUnmanaged
		}
		return s, shareLive
	case mgmtRunning:
		if err != nil {
			log.Printf("Share state is unreadable: %v", err)
			return nil, shareUnmanaged
		}
		if s == nil {
			return nil, shareUnmanaged
		}
		return s, shareLive
	default: // gone, or listed with a dead pane -- either way, not running
		if err != nil {
			log.Printf("Share state is unreadable: %v", err)
			return &share.State{}, shareStale
		}
		if s == nil {
			return nil, shareAbsent
		}
		return s, shareStale
	}
}

// reportUnmanaged explains a running share whose URLs cannot be recovered.
func reportUnmanaged(st *shareTarget) {
	fmt.Fprintf(os.Stderr, "A share is running for tmux session %q, but its state file is missing or unreadable.\n",
		st.target.SessionName)
	fmt.Fprintln(os.Stderr, "Its URLs cannot be recovered. Run `bitbang share rotate` to replace it with a new")
	fmt.Fprintln(os.Stderr, "share, or `bitbang share stop` to end it.")
	os.Exit(1)
}

func runShare(args []string) {
	cfg := parseShareFlags("share", args)
	st, err := resolveShareTarget(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
	defer enterTarget(st).Release()
	existing, liveness := loadLiveShare(st)
	switch liveness {
	case shareUnmanaged:
		reportUnmanaged(st)
	case shareStale:
		if err := cleanupStale(st); err != nil {
			fmt.Fprintf(os.Stderr, "%v\n", err)
			os.Exit(1)
		}
	}
	if liveness == shareLive {
		// Re-running `share` reprints the live share's URLs. That is
		// only honest while the request matches what is running: a
		// share started read-write cannot answer `--read-only` by
		// handing back its control URL.
		if conflicts := shareConflicts(st.cfg, existing); len(conflicts) > 0 {
			fmt.Fprintf(os.Stderr, "A share is already running for tmux session %q, with different settings:\n",
				st.target.SessionName)
			for _, c := range conflicts {
				fmt.Fprintf(os.Stderr, "  - %s\n", c)
			}
			fmt.Fprintln(os.Stderr, "\nRun `bitbang share rotate` with those flags to replace it (issues new URLs),")
			fmt.Fprintln(os.Stderr, "or `bitbang share stop` to end it first.")
			os.Exit(1)
		}
		fmt.Printf("Share already running for session %q -- reusing it (run `bitbang share rotate` for fresh URLs).\n\n",
			st.target.SessionName)
		printShareURLs(existing)
		return
	}
	startShare(st)
}

// shareConflicts lists the ways the explicitly-requested options differ
// from the share already running for this target. Only flags the user
// typed are compared, so `bitbang share` with no flags always reprints
// and never argues with itself over a default.
func shareConflicts(cfg shareConfig, s *share.State) []string {
	var out []string
	if cfg.set["read-only"] {
		runningReadOnly := s.ControlURL == ""
		if cfg.readOnly && !runningReadOnly {
			out = append(out, "--read-only requested, but the running share has a control URL")
		} else if !cfg.readOnly && runningReadOnly {
			out = append(out, "control access requested, but the running share is view-only")
		}
	}
	if cfg.set["ttl"] {
		if want := int(cfg.ttlDuration / time.Second); want != s.TTLSeconds {
			out = append(out, fmt.Sprintf("--ttl %s requested, running share uses %s",
				describeTTL(want), describeTTL(s.TTLSeconds)))
		}
	}
	if cfg.set["max-viewers"] && cfg.maxViewers != s.MaxViewers {
		out = append(out, fmt.Sprintf("--max-viewers %d requested, running share allows %d",
			cfg.maxViewers, s.MaxViewers))
	}
	if cfg.set["server"] && cfg.server != s.Server {
		out = append(out, fmt.Sprintf("--server %s requested, running share uses %s", cfg.server, s.Server))
	}
	return out
}

func describeTTL(seconds int) string {
	if seconds <= 0 {
		return "no expiry"
	}
	return (time.Duration(seconds) * time.Second).String()
}

func runShareRotate(args []string) {
	cfg := parseShareFlags("share rotate", args)
	st, err := resolveShareTarget(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
	defer enterTarget(st).Release()
	switch _, liveness := loadLiveShare(st); liveness {
	case shareLive, shareUnmanaged:
		// stopWorker needs only the management session name, so a
		// share with no usable state is still stoppable.
		if !stopWorker(st) {
			os.Exit(1)
		}
	case shareStale:
		if err := cleanupStale(st); err != nil {
			fmt.Fprintf(os.Stderr, "%v\n", err)
			os.Exit(1)
		}
	}
	startShare(st)
}

func runShareStatus(args []string) {
	cfg := parseShareFlags("share status", args)
	st, err := resolveShareTarget(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
	defer enterTarget(st).Release()
	existing, liveness := loadLiveShare(st)
	switch liveness {
	case shareUnmanaged:
		reportUnmanaged(st)
	case shareStale:
		if err := cleanupStale(st); err != nil {
			fmt.Fprintf(os.Stderr, "%v\n", err)
			os.Exit(1)
		}
		fmt.Println("No active share (cleaned up stale state).")
		os.Exit(1)
	case shareAbsent:
		fmt.Printf("No active share for session %q.\n", st.target.SessionName)
		os.Exit(1)
	}
	fmt.Printf("Sharing tmux session %q (since %s)\n",
		existing.SessionName, existing.CreatedAt.Local().Format("15:04:05"))
	printShareURLs(existing)
}

func runShareStop(args []string) {
	cfg := parseShareFlags("share stop", args)
	st, err := resolveShareTarget(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
	defer enterTarget(st).Release()
	switch _, liveness := loadLiveShare(st); liveness {
	case shareStale:
		if err := cleanupStale(st); err != nil {
			fmt.Fprintf(os.Stderr, "%v\n", err)
			os.Exit(1)
		}
		fmt.Println("No active share (cleaned up stale state).")
		return
	case shareAbsent:
		fmt.Printf("No active share for session %q.\n", st.target.SessionName)
		return
	}
	if !stopWorker(st) {
		os.Exit(1)
	}
	fmt.Println("Share stopped.")
}

// startShare spawns the detached worker and waits for it to come up.
func startShare(st *shareTarget) {
	ttl := st.cfg.ttlDuration
	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Cannot locate own binary: %v\n", err)
		os.Exit(1)
	}

	// Prepare the directory in the parent so permission and disk failures
	// can be reported even if the worker cannot write its startup error.
	if err := share.PrepareDir(st.dir); err != nil {
		fmt.Fprintf(os.Stderr, "Cannot prepare the share state directory %s: %v\n", st.dir, err)
		os.Exit(1)
	}

	// An operator running with remain-on-exit on globally leaves the
	// husk of a dead worker's session holding the name this one needs.
	if err := reapDeadMgmt(st, probeMgmt(st)); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}

	nonce, err := share.NewNonce()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Cannot start share worker: %v\n", err)
		os.Exit(1)
	}

	// Clear anything a previous attempt left. The nonce already stops it
	// being read as ours; this stops it accumulating.
	share.TakeStartupError(st.dir, nonce)

	workerArgv := []string{
		exe, "share", workerSubcommand,
		"-session", st.target.SessionID,
		"-session-name", st.target.SessionName,
		"-mgmt", st.mgmt,
		"-statedir", st.dir,
		"-server", st.cfg.server,
		"-nonce", nonce,
		"-ttl-seconds", strconv.Itoa(int(ttl / time.Second)),
		"-max-viewers", strconv.Itoa(st.cfg.maxViewers),
	}
	if st.target.Socket != "" {
		workerArgv = append(workerArgv, "-socket", st.target.Socket)
	}
	if st.cfg.readOnly {
		workerArgv = append(workerArgv, "-read-only")
	}
	if st.cfg.verbose {
		workerArgv = append(workerArgv, "-v")
	}
	quoted := make([]string, len(workerArgv))
	for i, a := range workerArgv {
		quoted[i] = share.ShellQuote(a)
	}
	// `exec` so the pane's #{pane_pid} IS the worker (tmux runs the
	// command string through a shell) -- stop signals it directly.
	cmdline := "exec " + strings.Join(quoted, " ")

	if _, err := st.runner.Run("new-session", "-d", "-s", st.mgmt, cmdline); err != nil {
		fmt.Fprintf(os.Stderr, "Cannot start share worker: %v\n", err)
		if strings.Contains(err.Error(), "duplicate session") {
			fmt.Fprintf(os.Stderr, "\nThe management session %s already exists, which means another\n", st.mgmt)
			fmt.Fprintln(os.Stderr, "`bitbang share` is starting one for this same tmux session. Run")
			fmt.Fprintln(os.Stderr, "`bitbang share status` once it has settled.")
		}
		os.Exit(1)
	}

	state, err := waitForState(st, nonce, startTimeout)
	if err != nil {
		// Read the diagnosis before stopping the worker: capture-pane
		// needs the pane that stopWorker is about to take away. A
		// worker that died recorded why on its way out, and its pane
		// went with it, so the file is the only copy; a worker that is
		// merely slow is still running, and its pane still holds the
		// log saying what it is waiting on.
		detail := share.TakeStartupError(st.dir, nonce)
		if detail == "" {
			out, captureErr := st.runner.Run("capture-pane", "-p", "-S", "-", "-t", st.windowTarget())
			if captureErr != nil {
				log.Printf("Could not read the worker's output: %v", captureErr)
			}
			detail = strings.TrimSpace(out)
		}
		fmt.Fprintf(os.Stderr, "Share failed to start: %v\n", err)
		if detail != "" {
			fmt.Fprintf(os.Stderr, "Worker output:\n  %s\n", strings.ReplaceAll(detail, "\n", "\n  "))
		}
		// Giving up on a worker is stopping a worker. Routing through
		// the same path as `share stop` is what keeps a start that
		// timed out on a live-but-slow worker from walking away and
		// leaving it serving URLs nobody has a record of.
		stopWorker(st)
		os.Exit(1)
	}

	fmt.Printf("Sharing tmux session %q\n\n", st.target.SessionName)
	printShareURLs(state)
	printWindowSizeNote(st)
}

// printWindowSizeNote warns when the shared window won't resize to
// whoever is currently driving it. tmux defaults `window-size` to
// `latest`, which is what makes "reopen it on your phone" render at
// phone size; an operator who has overridden it keeps their setting,
// since a share changes no tmux options. See share.EffectiveWindowSize
// for why the command does not write the option.
func printWindowSizeNote(st *shareTarget) {
	switch share.EffectiveWindowSize(st.runner, st.target.SessionID) {
	case "latest", "":
		// "" means the probe failed; don't invent advice from nothing.
	case "manual":
		fmt.Println("\nNote: this window's `window-size` is `manual`, so remote clients see it")
		fmt.Println("      at its fixed size rather than their own.")
	default:
		fmt.Println("\nNote: this window's `window-size` is not `latest`, so it won't resize to")
		fmt.Println("      whoever is currently driving it. `set -w window-size latest` to change that.")
	}
}

// waitForState accepts state only when its nonce matches this start and tmux
// still reports the worker running. An exited worker fails immediately; an
// unavailable tmux server leaves the result unresolved until timeout.
func waitForState(st *shareTarget, nonce string, timeout time.Duration) (*share.State, error) {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		s, _ := share.LoadState(st.dir)
		ours := s != nil && s.Nonce == nonce && s.ViewURL != ""
		state := probeMgmt(st)
		switch {
		case ours && state == mgmtRunning:
			return s, nil
		case state.exited():
			// Either it never got as far as writing state, or it wrote
			// and then died. Both are the same news.
			return nil, errors.New("worker exited during startup")
		}
		select {
		case <-ticker.C:
		case <-timer.C:
			return nil, fmt.Errorf("worker did not become ready within %s", timeout)
		}
	}
}

// stopWorker ends a running share and reports whether it is actually
// gone.
//
// Order matters: the worker is confirmed dead BEFORE the management
// session and state file are removed. Those two are the only handles
// left for a second attempt, so tearing them down first would strand a
// worker that is still serving its URLs with nothing to manage it by.
//
// "Confirmed" means tmux said so. A tmux that could not be asked is not
// a confirmation and never authorises the cleanup -- it gets the same
// treatment as a worker that refused to die: say so, change nothing,
// leave the operator something to retry.
func stopWorker(st *shareTarget) bool {
	// Nothing signalled, nothing to wait for: the grace period exists to
	// let a SIGTERM land, and waiting out five seconds for an effect we
	// never caused would only delay an answer we already have.
	pid, signalled := workerPID(st)
	if signalled {
		_ = killProcess(pid, syscall.SIGTERM)
		if !waitForWorkerExit(st, stopGrace) {
			// It ignored SIGTERM or is wedged. Escalate only against a
			// pane tmux still vouches for, still running the number we
			// signalled: once the worker exits the kernel may hand that
			// number to anything, and five seconds is long enough for
			// it to. Asking again narrows the window to the gap between
			// the answer and the signal, which is as far as a PID can
			// be trusted without pidfd or kqueue.
			if again, ok := workerPID(st); ok && again == pid {
				_ = killProcess(pid, syscall.SIGKILL)
				waitForWorkerExit(st, killGrace)
			}
		}
	}

	switch state := probeMgmt(st); {
	case state.exited():
		// Confirmed gone -- now it is safe to drop the handles.
		if err := cleanupStale(st); err != nil {
			fmt.Fprintf(os.Stderr, "The worker exited, but %v\n", err)
			return false
		}
		return true
	case state == mgmtUnknown:
		fmt.Fprintln(os.Stderr, "Could not stop the share: tmux did not answer, so there is no telling")
		fmt.Fprintln(os.Stderr, "whether the worker exited. Nothing was removed -- retry `bitbang share stop`")
		fmt.Fprintln(os.Stderr, "once tmux is reachable again.")
		return false
	default:
		if pid > 0 {
			fmt.Fprintf(os.Stderr, "Could not stop the share: worker process %d is still running.\n", pid)
		} else {
			fmt.Fprintln(os.Stderr, "Could not stop the share: its management session is still running.")
		}
		fmt.Fprintln(os.Stderr, "Its URLs stay answerable until it exits. The share is left in place")
		fmt.Fprintln(os.Stderr, "so `bitbang share stop` can be retried.")
		return false
	}
}

// waitForWorkerExit polls until tmux reports the worker gone or the
// budget runs out, reporting whether it exited.
//
// tmux's answer is the whole test. It spawned the pane's process and
// reaps it, so a session that has gone -- or one left listed with a dead
// pane -- is proof the process is not running. Adding a kill(pid, 0)
// would only reintroduce the ambiguity tmux had just resolved: after
// the number is reused it reports a live process that is not ours, and
// waiting for it to stop being alive means waiting forever.
func waitForWorkerExit(st *shareTarget, budget time.Duration) bool {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	timer := time.NewTimer(budget)
	defer timer.Stop()
	for {
		if probeMgmt(st).exited() {
			return true
		}
		select {
		case <-ticker.C:
		case <-timer.C:
			return false
		}
	}
}

// cleanupStale drops what a dead worker left behind: its state
// directory, and the husk of its management session when there is one.
//
// It asks tmux again rather than trusting the classification that sent
// it here. Every caller does check first, so the guard is a backstop --
// but this is the one function in the share path that deletes a
// credential file, and the reason it exists is that "the caller already
// checked" is exactly the assumption that goes stale.
func cleanupStale(st *shareTarget) error {
	state := probeMgmt(st)
	if !state.exited() {
		return fmt.Errorf("did not clean up %s: tmux no longer reports the worker as exited", st.dir)
	}
	if err := reapDeadMgmt(st, state); err != nil {
		return err
	}
	if err := share.RemoveState(st.dir); err != nil {
		return fmt.Errorf("could not remove stale share state at %s: %w", st.dir, err)
	}
	return nil
}

// reapDeadMgmt removes a management session tmux is keeping listed only
// because the operator set remain-on-exit globally.
//
// There is nothing running in it -- that is what makes the pane dead --
// but the session still holds its name, and `new-session -s` on a name
// already taken fails with "duplicate session". Left alone, one dead
// worker would block every future share of that target. The target is
// anchored, so this can only ever reach our own session, and a pane
// tmux reports as dead is a process tmux has already reaped.
func reapDeadMgmt(st *shareTarget, state mgmtState) error {
	if state != mgmtDead {
		return nil
	}
	if _, err := st.runner.Run("kill-session", "-t", st.sessionTarget()); err != nil {
		return fmt.Errorf("could not remove the dead management session %s, which still holds the name the next share needs: %w",
			st.mgmt, err)
	}
	return nil
}

// humanUntil uses seconds for short TTLs and minutes for longer ones.
func humanUntil(t time.Time) time.Duration {
	d := time.Until(t)
	if d < 0 {
		return 0
	}
	if d < time.Minute {
		return d.Round(time.Second)
	}
	return d.Round(time.Minute)
}

// printShareURLs renders the QR (control URL when one exists, view URL
// otherwise) and the credential lines, mirroring serve's output style.
func printShareURLs(s *share.State) {
	bold, reset := "", ""
	if term.IsTerminal(int(os.Stdout.Fd())) {
		bold, reset = "\033[1m", "\033[0m"
	}
	qrURL := s.ControlURL
	if qrURL == "" {
		qrURL = s.ViewURL
	}
	fmt.Print(smallQR(qrURL))
	if s.ControlURL != "" {
		fmt.Printf("Control URL: %s%s%s\n", bold, s.ControlURL, reset)
		fmt.Printf("             anyone with it can type -- same authority as your own keyboard\n")
		fmt.Printf("View URL:    %s\n", s.ViewURL)
		fmt.Printf("             watch only, up to %d viewers\n", s.MaxViewers)
	} else {
		fmt.Printf("View URL: %s%s%s\n", bold, s.ViewURL, reset)
		fmt.Printf("          watch only (no control credential exists), up to %d viewers\n", s.MaxViewers)
	}
	if s.ExpiresAt.IsZero() {
		fmt.Println("Expires:  when stopped (`bitbang share stop`)")
	} else {
		fmt.Printf("Expires:  %s (in %s) -- `bitbang share stop` to end early\n",
			s.ExpiresAt.Local().Format("15:04"), humanUntil(s.ExpiresAt))
	}
}

// runShareWorker is the hidden process hosted by the detached tmux
// management session. The CLI owns flag parsing, signals, and exit codes;
// internal/share owns the listener lifecycle.
func runShareWorker(args []string) {
	fs := flag.NewFlagSet("share "+workerSubcommand, flag.ExitOnError)
	var (
		sessionID   = fs.String("session", "", "target tmux session id")
		sessionName = fs.String("session-name", "", "target session display name")
		mgmt        = fs.String("mgmt", "", "management session name")
		stateDir    = fs.String("statedir", "", "share state directory")
		server      = fs.String("server", "bitba.ng", "signaling server")
		nonce       = fs.String("nonce", "", "start-attempt nonce, recorded in the state file")
		socket      = fs.String("socket", "", "tmux socket path")
		ttlSeconds  = fs.Int("ttl-seconds", 0, "lifetime in seconds (0 = until stopped)")
		maxViewers  = fs.Int("max-viewers", 16, "max concurrent viewers")
		readOnly    = fs.Bool("read-only", false, "no control credential")
		verbose     = fs.Bool("v", false, "verbose")
	)
	fs.Parse(args)
	maxTTLSeconds := int(share.MaxTTL / time.Second)
	// A missing nonce is worth failing on rather than starting a worker
	// whose state the parent is guaranteed not to accept.
	if *sessionID == "" || *mgmt == "" || *stateDir == "" || *nonce == "" ||
		*ttlSeconds < 0 || *ttlSeconds > maxTTLSeconds ||
		*maxViewers < 0 || *maxViewers > share.MaxViewers {
		msg := fmt.Sprintf("share worker: invalid internal flags (ttl 0..%d, viewers 0..%d)",
			maxTTLSeconds, share.MaxViewers)
		fmt.Fprintln(os.Stderr, msg)
		share.SaveStartupError(*stateDir, *nonce, msg)
		os.Exit(2)
	}

	worker, err := share.NewWorker(share.WorkerConfig{
		SessionID:   *sessionID,
		SessionName: *sessionName,
		MgmtSession: *mgmt,
		StateDir:    *stateDir,
		Server:      *server,
		Socket:      *socket,
		Nonce:       *nonce,
		TTL:         time.Duration(*ttlSeconds) * time.Second,
		MaxViewers:  *maxViewers,
		ReadOnly:    *readOnly,
		Verbose:     *verbose,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		share.SaveStartupError(*stateDir, *nonce, err.Error())
		os.Exit(1)
	}

	ctx, stop := shareWorkerContext()
	defer stop()
	if err := worker.Run(ctx); err != nil {
		fmt.Fprintln(os.Stderr, err)
		// The pane dies with this process, so leave the reason where
		// the parent can still read it.
		share.SaveStartupError(*stateDir, *nonce, err.Error())
		if errors.Is(err, share.ErrPreempted) {
			os.Exit(2)
		}
		os.Exit(1)
	}
}
