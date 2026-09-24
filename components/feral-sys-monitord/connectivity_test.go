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

// TestCheckConnectivityOneRefusalDoesNotCancelASlowerSuccess: every target
// dials in parallel and the verdict is any-success. Before this test the
// per-target error was returned into the errgroup, which cancelled the shared
// context on the FIRST failure and aborted every dial still in flight — a
// target that refused fast (an egress rule, a blocked prefix answering RST)
// turned a healthy network into "offline". The mainland-reachable targets
// added for feral-file#3539 made that path routine on networks that block
// exactly one family of resolvers, so the failure must stay local to its
// target.
func TestCheckConnectivityOneRefusalDoesNotCancelASlowerSuccess(t *testing.T) {
	saved := PING_TARGET_ADDRESS
	PING_TARGET_ADDRESS = []string{"refuses-fast", "succeeds-slowly"}
	t.Cleanup(func() { PING_TARGET_ADDRESS = saved })

	c := NewConnectivity(context.Background(), zap.NewNop())
	c.dial = func(ctx context.Context, target string, _ time.Duration) (net.Conn, error) {
		switch target {
		case "refuses-fast":
			return nil, errors.New("connect: connection refused")
		default:
			select {
			case <-time.After(50 * time.Millisecond):
				a, b := net.Pipe()
				go func() { _ = b.Close() }()
				return a, nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
	}

	ok, err := c.CheckConnectivity(time.Second)
	require.NoError(t, err)
	assert.True(t, ok, "one refused target must not cancel the dial that was about to succeed")
}
