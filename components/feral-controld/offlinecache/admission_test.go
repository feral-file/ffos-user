package offlinecache_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"

	"github.com/feral-file/ffos-user/components/feral-controld/offlinecache"
	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

// admissionFakeClock is a hand-rolled wrapper.Clock whose Now is advanced
// explicitly by tests. Simpler than gomock's MockClock for table tests
// where only Now matters; the remaining methods exist to satisfy the
// interface and fail loudly if the gate ever starts using them.
type admissionFakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newAdmissionFakeClock() *admissionFakeClock {
	return &admissionFakeClock{now: time.Unix(1_700_000_000, 0)}
}

func (c *admissionFakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *admissionFakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func (c *admissionFakeClock) Sleep(time.Duration) { panic("Sleep not expected") }
func (c *admissionFakeClock) SleepContext(context.Context, time.Duration) error {
	panic("SleepContext not expected")
}
func (c *admissionFakeClock) NewTicker(time.Duration) wrapper.Ticker {
	panic("NewTicker not expected")
}

func testAdmissionPolicy() offlinecache.AdmissionPolicy {
	return offlinecache.AdmissionPolicy{
		SoftwareBlockMemoryPercent: 80,
		SoftwareBlockCPUTempC:      85,
		MediaBlockMemoryPercent:    90,
		MediaBlockCPUTempC:         90,
		MetricsStaleAfter:          15 * time.Second,
	}
}

func newTestGate(t *testing.T) (*offlinecache.AdmissionGate, *admissionFakeClock) {
	t.Helper()
	clock := newAdmissionFakeClock()
	gate := offlinecache.NewAdmissionGate(testAdmissionPolicy(), clock, wrapper.NewJSON(), zaptest.NewLogger(t))
	return gate, clock
}

// sysMetricsJSON builds a payload in monitord's sysmetrics wire shape.
// usedPct is expressed against a fixed 16000 MB capacity.
func sysMetricsJSON(usedPct, cpuTempC float64) []byte {
	return sysMetricsJSONTotal(16000, usedPct, cpuTempC)
}

// sysMetricsJSONTotal is sysMetricsJSON with an explicit total-memory
// size, for the derived-threshold cases where RAM size is the variable.
func sysMetricsJSONTotal(maxMB, usedPct, cpuTempC float64) []byte {
	return fmt.Appendf(nil,
		`{"cpu":{"max_frequency":4800,"current_frequency":2000,"max_temperature":95,"current_temperature":%g},`+
			`"gpu":{"gpu_busy":50},`+
			`"memory":{"max_capacity":%g,"used_capacity":%g},`+
			`"disk":{"total_capacity":1,"used_capacity":1,"available_capacity":1},"uptime":1}`,
		cpuTempC, maxMB, maxMB*usedPct/100)
}

func TestAdmissionGateThresholdsPerClass(t *testing.T) {
	tests := []struct {
		name    string
		usedPct float64
		tempC   float64
		class   offlinecache.MediaClass
		allowed bool
	}{
		{"software under both thresholds", 79, 84, offlinecache.ClassSoftware, true},
		{"software over memory", 81, 60, offlinecache.ClassSoftware, false},
		{"software over temp", 50, 86, offlinecache.ClassSoftware, false},
		{"software exactly at thresholds admits", 80, 85, offlinecache.ClassSoftware, true},
		{"media admitted where software blocks", 85, 87, offlinecache.ClassMedia, true},
		{"media over memory", 91, 60, offlinecache.ClassMedia, false},
		{"media over temp", 50, 91, offlinecache.ClassMedia, false},
		{"unknown class uses media thresholds", 85, 87, offlinecache.ClassUnknown, true},
		{"unknown class blocks past media thresholds", 91, 60, offlinecache.ClassUnknown, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gate, _ := newTestGate(t)
			gate.Observe(sysMetricsJSON(tt.usedPct, tt.tempC))
			d := gate.Admit(tt.class)
			assert.Equal(t, tt.allowed, d.Allowed, "reason: %s", d.Reason)
			if !tt.allowed {
				assert.NotEmpty(t, d.Reason)
			}
		})
	}
}

func TestAdmissionGateHysteresis(t *testing.T) {
	gate, _ := newTestGate(t)

	// Block software on memory (81% > 80%).
	gate.Observe(sysMetricsJSON(81, 60))
	require.False(t, gate.Admit(offlinecache.ClassSoftware).Allowed)

	// In the hysteresis band (78% is below block 80 but above resume 75):
	// still blocked.
	gate.Observe(sysMetricsJSON(78, 60))
	assert.False(t, gate.Admit(offlinecache.ClassSoftware).Allowed,
		"a sample between resume and block thresholds must stay blocked")

	// Below resume threshold (74% < 75%): unlatches.
	gate.Observe(sysMetricsJSON(74, 60))
	assert.True(t, gate.Admit(offlinecache.ClassSoftware).Allowed)

	// And stays admitted on the next call (latch actually cleared).
	assert.True(t, gate.Admit(offlinecache.ClassSoftware).Allowed)
}

func TestAdmissionGateHysteresisRequiresBothSignalsRecovered(t *testing.T) {
	gate, _ := newTestGate(t)

	// Block on both memory and temp.
	gate.Observe(sysMetricsJSON(85, 90))
	require.False(t, gate.Admit(offlinecache.ClassSoftware).Allowed)

	// Memory fully recovered but temp still above its resume point
	// (81 > 85-5): stays blocked.
	gate.Observe(sysMetricsJSON(50, 81))
	assert.False(t, gate.Admit(offlinecache.ClassSoftware).Allowed)

	// Both recovered: resumes.
	gate.Observe(sysMetricsJSON(50, 79))
	assert.True(t, gate.Admit(offlinecache.ClassSoftware).Allowed)
}

func TestAdmissionGateLatchesAreIndependentPerBucket(t *testing.T) {
	gate, _ := newTestGate(t)

	// 85%/87°C blocks software but not media.
	gate.Observe(sysMetricsJSON(85, 87))
	require.False(t, gate.Admit(offlinecache.ClassSoftware).Allowed)
	require.True(t, gate.Admit(offlinecache.ClassMedia).Allowed)

	// Software sits in its hysteresis band (77% mem, 82°C — above resume
	// 75%/80°C): software still blocked, media still fine.
	gate.Observe(sysMetricsJSON(77, 82))
	assert.False(t, gate.Admit(offlinecache.ClassSoftware).Allowed)
	assert.True(t, gate.Admit(offlinecache.ClassMedia).Allowed)
}

// TestAdmissionGateSoftwareMemoryDerivedFromReserve pins the coupling
// between the gate and HeadlessLimits.MemoryMaxBytes: the software
// memory threshold is whichever is STRICTER of the configured static
// percentage and "the capture growing to its full cgroup ceiling still
// lands under the safety line". The 8 GB case is the one a static
// threshold gets wrong — 80% + a 2 GiB capture is 105%, an OOM the gate
// exists to prevent.
func TestAdmissionGateSoftwareMemoryDerivedFromReserve(t *testing.T) {
	const gib = 1 << 30
	tests := []struct {
		name      string
		totalMB   float64
		reserve   int64
		usedPct   float64
		wantAllow bool
	}{
		{"16GB: derived 77.5% blocks at 79% where static 80% would admit", 16000, 2 * gib, 79, false},
		{"16GB: admits below the derived line", 16000, 2 * gib, 70, true},
		{"8GB: derived 65% blocks at 70% (static 80% would OOM)", 8000, 2 * gib, 70, false},
		{"8GB: admits below the derived line", 8000, 2 * gib, 60, true},
		{"32GB: derived 83.75% is looser, so static 80% governs", 32000, 2 * gib, 82, false},
		{"no reserve (cap disabled): static 80% governs", 16000, 0, 79, true},
		{"tiny device: cap cannot fit, falls back to static", 2000, 2 * gib, 70, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			policy := testAdmissionPolicy()
			policy.SoftwareReserveBytes = tt.reserve
			policy.MemorySafetyCeilingPercent = 90
			gate := offlinecache.NewAdmissionGate(policy, newAdmissionFakeClock(), wrapper.NewJSON(), zaptest.NewLogger(t))

			gate.Observe(sysMetricsJSONTotal(tt.totalMB, tt.usedPct, 40))
			d := gate.Admit(offlinecache.ClassSoftware)
			assert.Equal(t, tt.wantAllow, d.Allowed, "reason: %s", d.Reason)

			// The reserve never applies to the browser-free media path.
			assert.True(t, gate.Admit(offlinecache.ClassMedia).Allowed,
				"media downloads spawn no chromium, so the headless cap must not gate them")
		})
	}
}

func TestAdmissionGateFailsOpenWithoutMetrics(t *testing.T) {
	gate, _ := newTestGate(t)

	// Never observed anything: admit both classes.
	assert.True(t, gate.Admit(offlinecache.ClassSoftware).Allowed)
	assert.True(t, gate.Admit(offlinecache.ClassMedia).Allowed)
}

func TestAdmissionGateFailsOpenOnStaleMetrics(t *testing.T) {
	gate, clock := newTestGate(t)

	// Fresh hot sample blocks.
	gate.Observe(sysMetricsJSON(95, 94))
	require.False(t, gate.Admit(offlinecache.ClassSoftware).Allowed)

	// Sample ages past the staleness horizon: fail open, and the latch is
	// released (blocking on dead data would be fail-closed by another name).
	clock.Advance(16 * time.Second)
	assert.True(t, gate.Admit(offlinecache.ClassSoftware).Allowed)

	// Metrics resume, still hot: blocks again (staleWarned reset path).
	gate.Observe(sysMetricsJSON(95, 94))
	assert.False(t, gate.Admit(offlinecache.ClassSoftware).Allowed)
}

func TestAdmissionGateDropsMalformedPayloadKeepingLastSample(t *testing.T) {
	gate, _ := newTestGate(t)

	gate.Observe(sysMetricsJSON(95, 60))
	require.False(t, gate.Admit(offlinecache.ClassSoftware).Allowed)

	// A corrupt signal must not blind the gate: last good (hot) sample is
	// retained and still blocks.
	gate.Observe([]byte("{not json"))
	assert.False(t, gate.Admit(offlinecache.ClassSoftware).Allowed)
}

func TestAdmissionGateFailsOpenOnBrokenSensors(t *testing.T) {
	gate, _ := newTestGate(t)

	// MaxCapacity 0 (failed /proc/meminfo read) disables the memory
	// signal; temp 0 (failed sensors read) never exceeds a threshold.
	gate.Observe([]byte(`{"cpu":{"current_temperature":0},"memory":{"max_capacity":0,"used_capacity":12000}}`))
	assert.True(t, gate.Admit(offlinecache.ClassSoftware).Allowed)
	assert.True(t, gate.Admit(offlinecache.ClassMedia).Allowed)
}
