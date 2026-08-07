//go:build unix

package share

import (
	"strings"
	"sync"
)

// tmuxCapture is a streamtype.Stream that accumulates whatever a
// handler writes, so the integration tests can assert on what an
// attached tmux client actually rendered.
type tmuxCapture struct {
	mu  sync.Mutex
	out []byte
}

func newTmuxCapture() *tmuxCapture { return &tmuxCapture{} }

func (c *tmuxCapture) ID() uint32              { return 1 }
func (c *tmuxCapture) ConnectPath() string     { return "/" }
func (c *tmuxCapture) WriteSYN(p []byte) error { return nil }
func (c *tmuxCapture) WriteDAT(p []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	// Frames are [tag][payload]; the tests only care about output text.
	if len(p) > 1 {
		c.out = append(c.out, p[1:]...)
	}
	return nil
}
func (c *tmuxCapture) WriteFIN(p []byte) error          { return nil }
func (c *tmuxCapture) SendRaw(_ uint16, p []byte) error { return c.WriteDAT(p) }
func (c *tmuxCapture) BufferedAmount() uint64           { return 0 }

// text returns everything written so far, with ANSI escape sequences
// stripped so assertions can match plain content.
func (c *tmuxCapture) text() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := string(c.out)
	var b strings.Builder
	for i := 0; i < len(s); {
		if s[i] == 0x1b {
			// Skip CSI/OSC/two-byte escapes up to a plausible terminator.
			j := i + 1
			for j < len(s) && !strings.ContainsRune("@ABCDEFGHJKSTfhilmnprstu\x07", rune(s[j])) {
				j++
			}
			i = j + 1
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}
