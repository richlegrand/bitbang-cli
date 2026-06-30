package peer

import (
	"testing"
	"time"

	"github.com/richlegrand/bitbang/internal/signaling"
)

// TestRelayWaitFor pins the device-side direct-bias policy and, importantly,
// the force_relay wire-field name: a typo/rename would silently drop the
// short-circuit (forced relay would wait the full grace again) with no other
// test failing. A missing or non-bool value must default to the full grace.
func TestRelayWaitFor(t *testing.T) {
	cases := []struct {
		name string
		msg  signaling.Message
		want time.Duration
	}{
		{"force_relay true → skip grace", signaling.Message{"force_relay": true}, 0},
		{"force_relay false → full grace", signaling.Message{"force_relay": false}, relayAcceptanceMinWait},
		{"force_relay absent → full grace", signaling.Message{}, relayAcceptanceMinWait},
		{"force_relay non-bool → full grace", signaling.Message{"force_relay": "yes"}, relayAcceptanceMinWait},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := relayWaitFor(tc.msg); got != tc.want {
				t.Errorf("relayWaitFor(%v) = %v, want %v", tc.msg, got, tc.want)
			}
		})
	}
}
