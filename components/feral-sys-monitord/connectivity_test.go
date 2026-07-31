package main

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"go.uber.org/zap"
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

	// The stale result must be discarded: no state write, no notification.
	select {
	case got := <-notified:
		t.Fatalf("stale initial probe result was notified (connected=%v)", got)
	case <-time.After(200 * time.Millisecond):
	}
	assert.False(t, c.GetLastConnected(), "retired generation must not write lastConnected")
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
