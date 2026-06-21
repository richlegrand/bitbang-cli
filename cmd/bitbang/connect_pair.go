package main

import (
	"fmt"
	"os"
	"regexp"
	"time"

	"github.com/richlegrand/bitbang/internal/client"
	"github.com/richlegrand/bitbang/internal/icehelper"
	"github.com/richlegrand/bitbang/internal/pairing"
	"github.com/richlegrand/bitbang/internal/sdp"
)

// pairCodePattern matches a 6-digit decimal code. runConnect uses it to
// decide between the pair flow (this file) and the existing URL flow.
var pairCodePattern = regexp.MustCompile(`^\d{6}$`)

// pairOutcome is the terminal state of a pair attempt as delivered by the
// signaling channel. ok=true means the listener sent pair_approved with a
// uid + access code; ok=false carries the reason from pair_rejected or a
// transport-level error.
type pairOutcome struct {
	ok         bool
	uid        string
	accessCode string
	reason     string
}

// runPairConnect implements the pair-code branch of `bitbang connect`. It
// opens a /ws/pair WebSocket on server, sends pair_init, then drives the
// answerer side of a WebRTC handshake against the listener that the server
// routes us to. Once the data channel opens, it computes the Short
// Authentication String (SAS) from the two DTLS fingerprints and displays
// it for the operator to read aloud — the listener's owner types what they
// hear and BitBang on that side compares against its independently-computed
// SAS.
//
// On pair_approved we surface a stable URL for future direct connects and
// return the remoteSpec so the caller can continue immediately into a normal
// shell session against the just-paired device. Persisting the device to
// ~/.bitbang/devices.json happens once in runConnect, after the subsequent
// direct connection actually succeeds — pairings and URL connects share that
// one save path. On pair_rejected the function surfaces the reason and exits
// the process non-zero.
//
// Connector plumbing (signaling WS + WebRTC dance) reuses
// internal/client.Signaling and internal/client.Peer in pair mode so that
// the two flows (URL connect and pair connect) share the same code paths
// for offer/answer/candidate exchange. The flow-specific bits live in
// this function: the initial pair_init message, the pair_routed ack,
// SAS display, and the pair_approved / pair_rejected handling.
func runPairConnect(code, server string, verbose, relay bool) remoteSpec {
	if !pairCodePattern.MatchString(code) {
		fail("connect: pair code must be 6 digits")
	}

	fmt.Fprintf(os.Stderr, "Pairing with %s...\n", server)

	sig := client.NewForPair(server)
	sig.Verbose = verbose

	// Channels driven by signaling callbacks. Each is sized so the read
	// loop never blocks for the consumers we run. errCh and outcomeCh
	// take at most one terminal value each; offerCh likewise (one offer
	// per pair attempt); candCh buffers trickle from the listener until
	// the drain goroutine is up.
	offerCh := make(chan client.Message, 1)
	candCh := make(chan client.Message, 16)
	routedCh := make(chan struct{}, 1)
	errCh := make(chan error, 1)
	outcomeCh := make(chan pairOutcome, 1)

	sig.OnOffer = func(m client.Message) { offerCh <- m }
	sig.OnCandidate = func(m client.Message) { candCh <- m }
	sig.OnError = func(msg string) {
		// Surface unknown_code (the most common pair_init failure) as
		// its own outcome so handlePairOutcome can render a friendly
		// message; anything else flows out as a generic error.
		if msg == "unknown_code" {
			outcomeCh <- pairOutcome{ok: false, reason: "unknown_code"}
			return
		}
		select {
		case errCh <- fmt.Errorf("signaling: %s", msg):
		default:
		}
	}
	sig.OnPairRouted = func() { routedCh <- struct{}{} }
	// pair_approved over signaling is now a bare, non-secret ack — the real
	// credentials arrive over the data channel (pair_credentials). We ignore
	// the signaling ack and treat the data-channel creds as the success
	// signal; only pair_rejected still flows through signaling.
	sig.OnPairRejected = func(reason string) {
		outcomeCh <- pairOutcome{ok: false, reason: reason}
	}

	if err := sig.Connect(); err != nil {
		fail("connect: %v", err)
	}
	defer sig.Close()

	if err := sig.SendPairInit(code, relay); err != nil {
		fail("connect: send pair_init: %v", err)
	}

	// Outer deadline: pair flow shouldn't take longer than the listener's
	// SAS-entry budget (60s) plus handshake slack. 90s is generous.
	deadline := time.After(90 * time.Second)

	// Wait for pair_routed. unknown_code (or any other early failure)
	// short-circuits via outcomeCh / errCh.
	select {
	case <-routedCh:
		if verbose {
			fmt.Fprintln(os.Stderr, "[pair] routed; waiting for offer from listener")
		}
	case out := <-outcomeCh:
		return handlePairOutcome(out, server)
	case err := <-errCh:
		fail("connect: %v", err)
	case <-deadline:
		fail("connect: pair attempt timed out waiting for routing (90s)")
	}

	// Wait for the listener's offer.
	var offer client.Message
	select {
	case offer = <-offerCh:
	case out := <-outcomeCh:
		return handlePairOutcome(out, server)
	case err := <-errCh:
		fail("connect: %v", err)
	case <-deadline:
		fail("connect: pair attempt timed out waiting for offer (90s)")
	}

	// Build a pair-mode Peer (no UID/code, no encrypted_request). Use the
	// ICE servers the signaling server stamped on the offer — phase-1 STUN
	// by default, so both peers gather srflx and can connect through NAT
	// instead of only on the same LAN. (The CLI has no lazy request_ice
	// fallback, just like the direct flow, so the hard ~25% would need
	// force_relay; that isn't wired into pairing yet.)
	iceServers := icehelper.ParseICEServers(offer)
	p, err := client.NewPairPeer(iceServers)
	if err != nil {
		fail("connect: new peer: %v", err)
	}
	defer p.Close()

	// Trickle local candidates through signaling.
	p.OnLocalCandidate(func(c map[string]interface{}) {
		_ = sig.SendCandidate(c)
	})

	answerSDP, _, err := p.HandleOffer(offer)
	if err != nil {
		fail("connect: handle offer: %v", err)
	}
	if err := sig.SendAnswer(answerSDP, ""); err != nil {
		fail("connect: send answer: %v", err)
	}

	// Drain inbound candidates until the data channel is open.
	candDone := make(chan struct{})
	go func() {
		for {
			select {
			case <-candDone:
				return
			case m := <-candCh:
				cdata, _ := m["candidate"].(map[string]interface{})
				if cdata != nil {
					_ = p.AddICECandidate(cdata)
				}
			}
		}
	}()

	// Wait for the data channel to open.
	select {
	case <-p.DCReady():
	case err := <-errCh:
		close(candDone)
		fail("connect: %v", err)
	case <-deadline:
		close(candDone)
		fail("connect: pair attempt timed out waiting for data channel (90s)")
	}

	// Commitment: the connector commits its nonce first, so a MITM can't grind
	// the SAS. Send commit, wait for the device's challenge, then reveal.
	rc, err := pairing.NewNonce()
	if err != nil {
		close(candDone)
		fail("connect: %v", err)
	}
	if err := p.DC.Send(pairing.BuildPairCommit(pairing.Commitment(rc))); err != nil {
		close(candDone)
		fail("connect: send pair_commit: %v", err)
	}

	var rd []byte
	select {
	case data := <-p.DCMessages():
		n, ok := pairing.ParsePairChallenge(data)
		if pairing.PairMessageType(data) != pairing.MsgPairChallenge || !ok {
			close(candDone)
			fail("connect: bad pair_challenge from listener")
		}
		rd = n
	case out := <-outcomeCh: // pair_rejected
		close(candDone)
		return handlePairOutcome(out, server)
	case err := <-errCh:
		close(candDone)
		fail("connect: %v", err)
	case <-deadline:
		close(candDone)
		fail("connect: pair attempt timed out waiting for challenge (90s)")
	}

	if err := p.DC.Send(pairing.BuildPairReveal(rc)); err != nil {
		close(candDone)
		fail("connect: send pair_reveal: %v", err)
	}

	// Compute and display the 6-digit SAS for the operator to read aloud.
	localFp := sdp.ExtractFingerprint(p.PC.LocalDescription().SDP)
	remoteFp := sdp.ExtractFingerprint(p.PC.RemoteDescription().SDP)
	if localFp == "" || remoteFp == "" {
		close(candDone)
		fail("connect: missing SDP fingerprints (local=%q remote=%q)", localFp, remoteFp)
	}
	displayConnectorSAS(pairing.ComputeSAS(rc, rd, localFp, remoteFp))

	// Wait for the listener's verdict: credentials over the data channel
	// (approval) or pair_rejected over signaling.
	select {
	case data := <-p.DCMessages():
		if pairing.PairMessageType(data) != pairing.MsgPairCredentials {
			close(candDone)
			fail("connect: unexpected data-channel message during pairing")
		}
		uid, _, ac, ok := pairing.ParsePairCredentials(data)
		if !ok {
			close(candDone)
			fail("connect: bad pair_credentials from listener")
		}
		close(candDone)
		return handlePairOutcome(pairOutcome{ok: true, uid: uid, accessCode: ac}, server)
	case out := <-outcomeCh:
		close(candDone)
		return handlePairOutcome(out, server)
	case err := <-errCh:
		close(candDone)
		fail("connect: %v", err)
	case <-deadline:
		close(candDone)
		fail("connect: pair attempt timed out waiting for approval (90s)")
	}
	return remoteSpec{} // unreachable; fail() exits
}

// displayConnectorSAS prints the SAS that the operator must read aloud to
// the listener's owner. The listener side never sees this number — they
// must hear it from the connector and type it.
func displayConnectorSAS(sas string) {
	fmt.Println()
	fmt.Println("Your pairing code: " + sas)
	fmt.Println()
	fmt.Println("Read this number to the device owner.")
	fmt.Println("Waiting for approval...")
}

// handlePairOutcome surfaces the final state to the user. On failure it
// prints the reason and exits non-zero. On success it prints the URL the
// operator can use directly next time and returns a remoteSpec so the caller
// can continue immediately into a normal connect flow. Persisting the device
// to the known-hosts table is the caller's job (runConnect), so that pairings
// and URL connects share a single save-on-success path.
func handlePairOutcome(r pairOutcome, server string) remoteSpec {
	if !r.ok {
		switch r.reason {
		case "sas_mismatch":
			fmt.Fprintln(os.Stderr, "Pair rejected: code mismatch.")
		case "timeout":
			fmt.Fprintln(os.Stderr, "Pair rejected: listener didn't confirm in time.")
		case "user_declined":
			fmt.Fprintln(os.Stderr, "Pair rejected by the device owner.")
		case "unknown_code":
			fmt.Fprintln(os.Stderr, "Pair rejected: code not found or expired.")
		default:
			fmt.Fprintf(os.Stderr, "Pair rejected: %s\n", r.reason)
		}
		os.Exit(1)
	}

	url := fmt.Sprintf("https://%s/%s#%s", server, r.uid, r.accessCode)
	fmt.Println()
	fmt.Println("Paired.")
	fmt.Printf("URL: %s\n", url)

	return remoteSpec{
		Server: server,
		UID:    r.uid,
		Code:   r.accessCode,
	}
}

