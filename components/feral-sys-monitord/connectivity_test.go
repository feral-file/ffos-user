package main

import (
	"context"
	"sync"
	"testing"

	"go.uber.org/zap"
)

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
