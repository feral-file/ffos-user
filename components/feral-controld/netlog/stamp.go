package netlog

import (
	"os"
	"strconv"
	"strings"
	"time"
)

// stampSource produces the wall+uptime+bootID triple. A func seam (not an
// interface) because tests only need to substitute deterministic values.
type stampSource func() Stamp

// newStampSource builds the production stamp source. The boot ID is read once
// (it cannot change within a boot); uptime is read per stamp — events are rare
// (transitions, not ticks), so a /proc read per record is negligible.
func newStampSource() stampSource {
	bootID := readBootID()
	return func() Stamp {
		return Stamp{
			Wall:    time.Now(),
			UptimeS: readUptimeSeconds(),
			BootID:  bootID,
		}
	}
}

// readBootID returns the kernel boot UUID, or "" off-Linux / on error. A
// missing boot ID degrades ordering across boots, not correctness — records
// still carry wall + uptime.
func readBootID() string {
	data, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// readUptimeSeconds returns CLOCK_BOOTTIME seconds from /proc/uptime (0 on
// error). Chosen over Go's process-monotonic clock because it is comparable
// across controld restarts within one boot — a Restart=always daemon bounce
// mid-outage must not reset the timeline's monotonic axis.
func readUptimeSeconds() float64 {
	data, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return 0
	}
	return parseUptimeSeconds(data)
}

// parseUptimeSeconds parses /proc/uptime's first field ("12345.67 8901.23").
// Degrades to 0 on any malformed input: a broken uptime read weakens
// cross-restart ordering but must never fail a record write.
func parseUptimeSeconds(data []byte) float64 {
	fields := strings.Fields(string(data))
	if len(fields) < 1 {
		return 0
	}
	v, err := strconv.ParseFloat(fields[0], 64)
	if err != nil || v < 0 {
		return 0
	}
	return v
}
