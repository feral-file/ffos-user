package netlog

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestParseUptimeSeconds pins the /proc/uptime parse and its documented
// 0-on-error degradation — the uptime axis is what keeps a boot-time outage
// orderable while the SNTP-less wall clock is still wrong (plan §2).
func TestParseUptimeSeconds(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want float64
	}{
		{"normal", "12345.67 89012.34\n", 12345.67},
		{"single field", "42.00\n", 42.0},
		{"empty", "", 0},
		{"garbage", "not-a-number x\n", 0},
		{"negative rejected", "-5.0 1.0\n", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, parseUptimeSeconds([]byte(tt.in)))
		})
	}
}

// TestStampSourceProducesOrderedUptime: the production stamp source must
// yield non-decreasing uptime and a stable boot ID across calls within one
// process — the invariants every ring reader relies on. (On non-Linux dev
// hosts /proc is absent and both degrade to zero values, which the reader
// contract tolerates; the assertions hold either way.)
func TestStampSourceProducesOrderedUptime(t *testing.T) {
	src := newStampSource()
	a := src()
	b := src()
	assert.LessOrEqual(t, a.UptimeS, b.UptimeS, "uptime must never run backwards within a boot")
	assert.Equal(t, a.BootID, b.BootID, "boot ID must be stable within a process")
	assert.False(t, b.Wall.Before(a.Wall.Add(-1)), "wall stamps come from time.Now")
}
