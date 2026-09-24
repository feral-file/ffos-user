package main

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-sys-monitord/metric"
)

// TestInitialProbeResultDiscardedAfterStop: the initial reachability probe can
// still be dialing when the watcher is stopped/restarted; its result belongs
// to the retired generation and must be discarded — applying it would
// overwrite the replacement watcher's state and emit a stale connectivity
// transition (the ticker branch already had this guard; the initial branch
// did not).
func TestInitialProbeResultDiscardedAfterStop(t *testing.T) {
	c := NewConnectivity(context.Background(), zap.NewNop())

	notified := make(chan bool, 1)
	c.OnConnectivityChange(func(_ context.Context, connected bool) {
		notified <- connected
	})

	probeStarted := make(chan struct{})
	release := make(chan struct{})
	c.probe = func(time.Duration) (bool, error) {
		close(probeStarted)
		<-release
		return true, nil // "online" — but from a retired generation
	}

	c.Start()
	<-probeStarted

	// Retire the generation while the probe is mid-flight, then let the stale
	// result come back.
	c.Stop()
	c.resetDone()
	close(release)

	// The stale result must be discarded: no state write, no notification, and
	// no Prometheus export — the exported timeline must never carry a verdict
	// the D-Bus consumers were not told about (stage 0 gauge rule; this runs
	// before TestAppliedProbeResultExported, which is what makes the absence
	// assertable).
	select {
	case got := <-notified:
		t.Fatalf("stale initial probe result was notified (connected=%v)", got)
	case <-time.After(200 * time.Millisecond):
	}
	assert.False(t, c.GetLastConnected(), "retired generation must not write lastConnected")
	assert.False(t, reachabilityGaugeExported(t),
		"retired generation must not export a reachability sample")
}

// reachabilityGaugeExported reports whether net_internet_reachable currently
// has a series in the metric registry (absent = no probe result was ever
// applied in this process).
func reachabilityGaugeExported(t *testing.T) bool {
	t.Helper()
	families, err := metric.MetricsGatherer().Gather()
	require.NoError(t, err)
	for _, mf := range families {
		if mf.GetName() == "net_internet_reachable" {
			return true
		}
	}
	return false
}

// TestAppliedProbeResultExported: a probe result that survives the generation
// guard and becomes watcher state must also land in the Prometheus registry —
// that gauge is what the vmagent offline spool carries across an outage
// (docs/wan-outage-observability.md stage 0).
func TestAppliedProbeResultExported(t *testing.T) {
	c := NewConnectivity(context.Background(), zap.NewNop())

	notified := make(chan bool, 1)
	c.OnConnectivityChange(func(_ context.Context, connected bool) {
		notified <- connected
	})
	c.probe = func(time.Duration) (bool, error) { return true, nil }

	c.Start()
	select {
	case got := <-notified:
		assert.True(t, got)
	case <-time.After(2 * time.Second):
		t.Fatal("initial probe result was never notified")
	}
	c.Stop()

	assert.True(t, reachabilityGaugeExported(t), "applied probe result must be exported")
	assert.True(t, c.GetLastConnected())
}

// TestConnectivityGenerationSwapConcurrent is the -race regression for the
// doneChan generation swap: restart() replaces the channel (via resetDone)
// while notifyHandlers' goroutines and Stop() read it. The test exercises
// exactly restart's racy core — Stop + resetDone — against concurrent
// notifications, WITHOUT Start(), so no real ping loop dials out of the test
// environment. Failure mode is a race-detector report, not an assertion.
func TestConnectivityGenerationSwapConcurrent(t *testing.T) {
	c := NewConnectivity(context.Background(), zap.NewNop())
	c.OnConnectivityChange(func(context.Context, bool) {})

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		connected := i%2 == 0
		wg.Add(3)
		go func() {
			defer wg.Done()
			c.Stop()
		}()
		go func() {
			defer wg.Done()
			c.resetDone()
		}()
		go func() {
			defer wg.Done()
			c.notifyHandlers(context.Background(), connected)
		}()
	}
	wg.Wait()
	c.Stop()
}

// scriptedDial is one target's behavior in the CheckConnectivity tests.
type scriptedDial struct {
	after   time.Duration // how long the dial takes; ignored when blackhole
	succeed bool
	// blackhole models a dropped SYN: the dial never answers and returns only
	// when the check's context ends.
	blackhole bool
}

// TestCheckConnectivityStaged pins the two-stage probe (feral-file#3539):
// the fallback (mainland) targets are dialed only when the primary (Google)
// stage has not succeeded, the first success returns without waiting for
// blackholed peers, and one target's failure never cancels another's
// success.
func TestCheckConnectivityStaged(t *testing.T) {
	const (
		timeout       = 2 * time.Second
		fallbackDelay = 500 * time.Millisecond
	)
	refuse := scriptedDial{}
	blackhole := scriptedDial{blackhole: true}
	ok := func(after time.Duration) scriptedDial { return scriptedDial{after: after, succeed: true} }

	tests := []struct {
		name          string
		primary       map[string]scriptedDial
		fallback      map[string]scriptedDial
		wantConnected bool
		wantFallback  bool          // whether any fallback target was dialed
		maxElapsed    time.Duration // upper bound on the check's wall time
	}{
		{
			name:          "primary answers: fallback never dialed",
			primary:       map[string]scriptedDial{"p1": ok(0), "p2": ok(0)},
			fallback:      map[string]scriptedDial{"f1": ok(0)},
			wantConnected: true,
			maxElapsed:    fallbackDelay / 2,
		},
		{
			name:          "slow primary inside the delay: fallback never dialed",
			primary:       map[string]scriptedDial{"p1": ok(50 * time.Millisecond), "p2": blackhole},
			fallback:      map[string]scriptedDial{"f1": ok(0)},
			wantConnected: true,
			maxElapsed:    fallbackDelay,
		},
		{
			name:          "first success returns without waiting for a blackholed peer",
			primary:       map[string]scriptedDial{"p1": blackhole, "p2": ok(20 * time.Millisecond)},
			fallback:      map[string]scriptedDial{"f1": ok(0)},
			wantConnected: true,
			maxElapsed:    fallbackDelay,
		},
		{
			name:          "one fast refusal does not cancel a slower success",
			primary:       map[string]scriptedDial{"p1": refuse, "p2": ok(50 * time.Millisecond)},
			fallback:      map[string]scriptedDial{"f1": ok(0)},
			wantConnected: true,
			maxElapsed:    fallbackDelay,
		},
		{
			name:          "primary blackholed (mainland): fallback after the delay",
			primary:       map[string]scriptedDial{"p1": blackhole, "p2": blackhole},
			fallback:      map[string]scriptedDial{"f1": blackhole, "f2": ok(10 * time.Millisecond)},
			wantConnected: true,
			wantFallback:  true,
			maxElapsed:    fallbackDelay + timeout/4,
		},
		{
			name:          "primary refused fast: fallback without waiting for the delay",
			primary:       map[string]scriptedDial{"p1": refuse, "p2": refuse},
			fallback:      map[string]scriptedDial{"f1": ok(0)},
			wantConnected: true,
			wantFallback:  true,
			maxElapsed:    fallbackDelay / 2,
		},
		{
			name:          "primary slow but working after fallback starts still counts",
			primary:       map[string]scriptedDial{"p1": ok(fallbackDelay + 100*time.Millisecond)},
			fallback:      map[string]scriptedDial{"f1": blackhole},
			wantConnected: true,
			wantFallback:  true,
			maxElapsed:    timeout / 2,
		},
		{
			name:          "everything refused: offline, no error",
			primary:       map[string]scriptedDial{"p1": refuse, "p2": refuse},
			fallback:      map[string]scriptedDial{"f1": refuse, "f2": refuse},
			wantConnected: false,
			wantFallback:  true,
			maxElapsed:    fallbackDelay / 2,
		},
		{
			name:          "everything blackholed: offline at the timeout, not beyond",
			primary:       map[string]scriptedDial{"p1": blackhole},
			fallback:      map[string]scriptedDial{"f1": blackhole},
			wantConnected: false,
			wantFallback:  true,
			maxElapsed:    timeout + timeout/4,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			savedPrimary, savedFallback := PRIMARY_PING_TARGETS, FALLBACK_PING_TARGETS
			PRIMARY_PING_TARGETS = mapKeys(tc.primary)
			FALLBACK_PING_TARGETS = mapKeys(tc.fallback)
			t.Cleanup(func() { PRIMARY_PING_TARGETS, FALLBACK_PING_TARGETS = savedPrimary, savedFallback })

			var mu sync.Mutex
			var dialed []string
			c := NewConnectivity(context.Background(), zap.NewNop())
			c.fallbackDelay = fallbackDelay
			c.dial = func(ctx context.Context, target string, _ time.Duration) (net.Conn, error) {
				mu.Lock()
				dialed = append(dialed, target)
				mu.Unlock()
				script, found := tc.primary[target]
				if !found {
					script = tc.fallback[target]
				}
				if script.blackhole {
					<-ctx.Done()
					return nil, ctx.Err()
				}
				select {
				case <-time.After(script.after):
				case <-ctx.Done():
					return nil, ctx.Err()
				}
				if !script.succeed {
					return nil, errors.New("connect: connection refused")
				}
				a, b := net.Pipe()
				go func() { _ = b.Close() }()
				return a, nil
			}

			start := time.Now()
			connected, err := c.CheckConnectivity(timeout)
			elapsed := time.Since(start)

			require.NoError(t, err)
			assert.Equal(t, tc.wantConnected, connected)
			assert.LessOrEqual(t, elapsed, tc.maxElapsed, "check took too long")

			mu.Lock()
			defer mu.Unlock()
			usedFallback := false
			for _, target := range dialed {
				if _, isFallback := tc.fallback[target]; isFallback {
					usedFallback = true
				}
			}
			assert.Equal(t, tc.wantFallback, usedFallback, "fallback dialed = %v, dialed targets %v", usedFallback, dialed)
		})
	}
}

func mapKeys(m map[string]scriptedDial) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}
