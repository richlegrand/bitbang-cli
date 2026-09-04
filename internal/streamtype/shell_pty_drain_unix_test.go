//go:build unix

package streamtype

import (
	"testing"
	"time"

	ptylib "github.com/aymanbagabas/go-pty"
)

// finishPTY has to reach EOF on its own, not by running out the clock.
//
// This process holds the pty's slave open for its lifetime, so a read on
// the master returns neither EOF nor EIO however long ago the child died.
// Before finishPTY closed the slave, the wait could not succeed in PTY
// mode at all: every interactive session ran the full timeout down and
// was then cancelled, which is five seconds between typing `exit` and the
// process going away.
func TestFinishPTYDrainsWithoutWaitingOutTheTimeout(t *testing.T) {
	terminal, err := ptylib.New()
	if err != nil {
		t.Skipf("no pty available: %v", err)
	}

	cmd := terminal.Command("/bin/sh", "-c", "printf 'tail-bytes'; exit 0")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}

	// The pump finishPTY waits on, reading until the master ends.
	output := newShellOutput()
	read := make(chan []byte, 1)
	output.Add(1)
	go func() {
		defer output.Done()
		var got []byte
		buf := make([]byte, 512)
		for {
			n, err := terminal.Read(buf)
			got = append(got, buf[:n]...)
			if err != nil {
				read <- got
				return
			}
		}
	}()

	_ = cmd.Wait()

	// Generous enough that a pass means it ended on EOF rather than on
	// the deadline, and short enough that a regression fails the test
	// rather than slowing it down.
	const timeout = 3 * time.Second
	start := time.Now()
	drained := finishPTY(terminal, output, timeout)
	elapsed := time.Since(start)

	if !drained {
		t.Errorf("finishPTY timed out after %v instead of reaching EOF", elapsed)
	}
	if elapsed > timeout/2 {
		t.Errorf("finishPTY took %v, close to the %v timeout -- it is waiting the clock out", elapsed, timeout)
	}

	// Whatever the shell wrote before exiting still has to arrive.
	select {
	case got := <-read:
		if string(got) != "tail-bytes" {
			t.Errorf("read %q, want the bytes written before exit", got)
		}
	case <-time.After(time.Second):
		t.Error("the pump never finished")
	}
}
