package offlinecache_test

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"

	"github.com/feral-file/ffos-user/components/feral-controld/mocks"
	"github.com/feral-file/ffos-user/components/feral-controld/offlinecache"
	"github.com/feral-file/ffos-user/components/feral-controld/relayer"
)

// waitForWSSend blocks until the background WS-delivery worker (see
// offlinecache.Notifier's doc) has actually called ws.SendAll, since
// OnItemStateChanged now only enqueues and returns — every test below
// that asserts on a SendAll call must synchronize on this rather than
// assuming SendAll already ran by the time OnItemStateChanged returns.
func waitForWSSend(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the background WS-delivery worker to call SendAll")
	}
}

func TestNotifier_OnItemStateChanged_SendsViaRelayerAndWS(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockRelayer := mocks.NewMockRelayer(ctrl)
	mockWS := mocks.NewMockWS(ctrl)
	status := offlinecache.ItemStatus{ItemID: "item-1", State: offlinecache.StateReady, CoverageComplete: true}

	mockRelayer.EXPECT().IsConnected().Return(true).Times(1)
	mockRelayer.EXPECT().Send(gomock.Any(), gomock.Any()).DoAndReturn(
		func(_ context.Context, data interface{}) error {
			m, ok := data.(map[string]interface{})
			assert.True(t, ok)
			assert.Equal(t, "notification", m["type"])
			assert.Equal(t, string(relayer.NOTIFICATION_TYPE_OFFLINE_CACHE_STATUS), m["notification_type"])
			assert.Equal(t, status, m["message"])
			assert.Equal(t, 1, m["persist_record_count"])
			return nil
		}).Times(1)
	done := make(chan struct{})
	mockWS.EXPECT().SendAll(gomock.Any()).DoAndReturn(func(interface{}) error {
		close(done)
		return nil
	}).Times(1)

	notifier := offlinecache.NewNotifier(mockRelayer, mockWS, zaptest.NewLogger(t))
	defer notifier.Close()
	notifier.OnItemStateChanged(status)

	waitForWSSend(t, done)
}

func TestNotifier_OnItemStateChanged_SkipsRelayerWhenDisconnected(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockRelayer := mocks.NewMockRelayer(ctrl)
	mockWS := mocks.NewMockWS(ctrl)

	mockRelayer.EXPECT().IsConnected().Return(false).Times(1)
	// Send must never be called while disconnected.
	done := make(chan struct{})
	mockWS.EXPECT().SendAll(gomock.Any()).DoAndReturn(func(interface{}) error {
		close(done)
		return nil
	}).Times(1)

	notifier := offlinecache.NewNotifier(mockRelayer, mockWS, zaptest.NewLogger(t))
	defer notifier.Close()
	notifier.OnItemStateChanged(offlinecache.ItemStatus{ItemID: "item-1", State: offlinecache.StateQueued})

	waitForWSSend(t, done)
}

func TestNotifier_OnItemStateChanged_NilRelayerAndWSAreNoop(t *testing.T) {
	notifier := offlinecache.NewNotifier(nil, nil, zaptest.NewLogger(t))
	defer notifier.Close()
	assert.NotPanics(t, func() {
		notifier.OnItemStateChanged(offlinecache.ItemStatus{ItemID: "item-1", State: offlinecache.StateFailed})
	})
}

func TestNotifier_OnItemStateChanged_LogsSendErrorsButDoesNotPanic(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockRelayer := mocks.NewMockRelayer(ctrl)
	mockWS := mocks.NewMockWS(ctrl)

	mockRelayer.EXPECT().IsConnected().Return(true).Times(1)
	mockRelayer.EXPECT().Send(gomock.Any(), gomock.Any()).Return(assertError("relayer down")).Times(1)
	done := make(chan struct{})
	mockWS.EXPECT().SendAll(gomock.Any()).DoAndReturn(func(interface{}) error {
		defer close(done)
		return assertError("no ws clients")
	}).Times(1)

	notifier := offlinecache.NewNotifier(mockRelayer, mockWS, zaptest.NewLogger(t))
	defer notifier.Close()
	assert.NotPanics(t, func() {
		notifier.OnItemStateChanged(offlinecache.ItemStatus{ItemID: "item-1", State: offlinecache.StateFailed, Reason: "boom"})
	})

	waitForWSSend(t, done)
}

// TestNotifier_OnItemStateChanged_DoesNotBlockOnSlowWSDelivery is the
// regression test for the P2 finding that ws.SendAll ran synchronously
// on OnItemStateChanged's caller — Service's single capture-worker
// goroutine, see service.go's notify — with no aggregate bound across
// however many hub clients were connected (each individual write is
// bounded by ws.go's sendWriteWait, but that loop as a whole is not).
// mockWS.SendAll blocks indefinitely here (standing in for several
// slow/wedged hub clients) until the test explicitly releases it, yet
// OnItemStateChanged for many subsequent item-state transitions must
// still return immediately: the whole point of Notifier's background
// delivery worker is that the capture worker never waits on however long
// an individual SendAll call takes.
func TestNotifier_OnItemStateChanged_DoesNotBlockOnSlowWSDelivery(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockRelayer := mocks.NewMockRelayer(ctrl)
	mockWS := mocks.NewMockWS(ctrl)
	mockRelayer.EXPECT().IsConnected().Return(false).AnyTimes()

	release := make(chan struct{})
	sendStarted := make(chan struct{}, 1)
	mockWS.EXPECT().SendAll(gomock.Any()).DoAndReturn(func(interface{}) error {
		select {
		case sendStarted <- struct{}{}:
		default:
		}
		<-release // held open until the test explicitly releases it below
		return nil
	}).AnyTimes()

	notifier := offlinecache.NewNotifier(mockRelayer, mockWS, zaptest.NewLogger(t))
	defer func() {
		close(release)
		notifier.Close()
	}()

	// This first enqueue is what the background worker picks up and
	// blocks on inside the (deliberately stuck) SendAll above.
	notifier.OnItemStateChanged(offlinecache.ItemStatus{ItemID: "item-1", State: offlinecache.StateDownloading})
	select {
	case <-sendStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the background worker to start its deliberately-blocked SendAll call")
	}

	// Simulates the real capture worker moving on to notify about many
	// further items while the FIRST notification's delivery is still
	// stuck — every one of these must return immediately regardless.
	start := time.Now()
	for i := 0; i < 50; i++ {
		notifier.OnItemStateChanged(offlinecache.ItemStatus{
			ItemID: fmt.Sprintf("item-%d", i+2), State: offlinecache.StateReady,
		})
	}
	elapsed := time.Since(start)

	assert.Less(t, elapsed, 500*time.Millisecond,
		"OnItemStateChanged must never block on a slow/wedged WS delivery — the real capture worker's whole download queue would otherwise stall behind it")
}

// TestNotifier_Close_IsSafeToCallMultipleTimesAndWhenWSDisabled pins
// Close's own contract: idempotent (no panic on a double-close of the
// underlying done channel), and a no-op when ws was nil at construction
// (no background worker was ever started to stop).
func TestNotifier_Close_IsSafeToCallMultipleTimesAndWhenWSDisabled(t *testing.T) {
	notifier := offlinecache.NewNotifier(nil, nil, zaptest.NewLogger(t))
	assert.NotPanics(t, func() {
		notifier.Close()
		notifier.Close()
	})

	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	mockWS := mocks.NewMockWS(ctrl)
	mockWS.EXPECT().SendAll(gomock.Any()).Return(nil).AnyTimes()
	withWS := offlinecache.NewNotifier(nil, mockWS, zaptest.NewLogger(t))
	assert.NotPanics(t, func() {
		withWS.Close()
		withWS.Close()
	})
}

// TestNotifier_OnItemStateChanged_DoesNotBlockOnSlowRelayerDelivery is
// the regression test for admission paying for delivery. The relayer
// send used to run inline here, bounded per send by notifySendTimeout
// (5s) — and this is called once per item from enqueue, which
// DownloadPlaylist drives in a serial loop, so one playlist request
// against a connected-but-backpressured relayer became (item count) x 5s
// of blocking inside command admission: minutes past the LAN hub's own
// response deadline for a command documented to acknowledge promptly.
//
// The relayer Send here blocks until the test releases it, standing in
// for that backpressure. Each call must still return immediately.
func TestNotifier_OnItemStateChanged_DoesNotBlockOnSlowRelayerDelivery(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockRelayer := mocks.NewMockRelayer(ctrl)
	release := make(chan struct{})
	blocked := make(chan struct{}, 1)

	mockRelayer.EXPECT().IsConnected().Return(true).AnyTimes()
	mockRelayer.EXPECT().Send(gomock.Any(), gomock.Any()).DoAndReturn(
		func(context.Context, interface{}) error {
			select {
			case blocked <- struct{}{}:
			default:
			}
			<-release
			return nil
		}).AnyTimes()

	notifier := offlinecache.NewNotifier(mockRelayer, nil, zaptest.NewLogger(t))
	defer func() {
		close(release)
		notifier.Close()
	}()

	// Wait until a send is genuinely stuck, so the timing below is
	// measuring "queued behind a wedged transport" and not a race.
	notifier.OnItemStateChanged(offlinecache.ItemStatus{ItemID: "item-0", State: offlinecache.StateQueued})
	select {
	case <-blocked:
	case <-time.After(2 * time.Second):
		t.Fatal("the worker never reached the relayer send")
	}

	// A playlist's worth of admissions, every one of them while the
	// relayer is wedged.
	const items = 200
	start := time.Now()
	for i := 0; i < items; i++ {
		notifier.OnItemStateChanged(offlinecache.ItemStatus{
			ItemID: fmt.Sprintf("item-%d", i+1), State: offlinecache.StateQueued,
		})
	}
	elapsed := time.Since(start)

	assert.Less(t, elapsed, 2*time.Second,
		"admission must not pay for delivery: %d notifications took %s with the relayer wedged", items, elapsed)
}

// TestNotifier_OnItemStateChanged_MaxSizePlaylistWithWedgedTransportLosesNoItem
// is the saturation regression test for the bounded delivery queue. The
// queue used to hold one slot per EVENT, capped at 256, and drop whatever
// arrived once it was full. enqueue emits one "queued" per item and
// downloadPlaylist drives that in a tight serial loop, so a max-size DP-1
// playlist (1024 items — the same bound defaultMaxQueueLen is derived
// from) overran the buffer on the "queued" burst alone, before any
// downloading/terminal transitions, and accepted items silently lost
// progress the wire contract promises to push. A wedged transport made it
// worse but was never required to trigger it.
//
// The queue now holds one slot per ITEM, coalescing successive states into
// the latest one (see notifyQueue's doc), so this drives every item
// through its full queued -> downloading -> ready sequence with the
// transport wedged for the entire burst, then releases it and requires
// that EVERY item ends up delivered at its final state.
func TestNotifier_OnItemStateChanged_MaxSizePlaylistWithWedgedTransportLosesNoItem(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	// The DP-1 per-playlist cap: the largest single DownloadPlaylist the
	// daemon accepts, and therefore the largest burst it must not lose.
	const items = 1024

	mockRelayer := mocks.NewMockRelayer(ctrl)
	mockWS := mocks.NewMockWS(ctrl)
	mockRelayer.EXPECT().IsConnected().Return(false).AnyTimes()

	release := make(chan struct{})
	sendStarted := make(chan struct{}, 1)
	var mu sync.Mutex
	lastSeen := make(map[string]offlinecache.ItemState, items)
	sendCalls := 0
	mockWS.EXPECT().SendAll(gomock.Any()).DoAndReturn(func(data interface{}) error {
		select {
		case sendStarted <- struct{}{}:
		default:
		}
		// Wedged until the test releases it; a closed channel then lets
		// every subsequent send through immediately so the queue drains.
		<-release
		envelope, ok := data.(map[string]interface{})
		if !ok {
			return nil
		}
		status, ok := envelope["message"].(offlinecache.ItemStatus)
		if !ok {
			return nil
		}
		mu.Lock()
		lastSeen[status.ItemID] = status.State
		sendCalls++
		mu.Unlock()
		return nil
	}).AnyTimes()

	// Once-guarded so the deferred cleanup can unwedge the transport on an
	// early t.Fatal without double-closing the channel the happy path
	// already released.
	var releaseOnce sync.Once
	unwedge := func() { releaseOnce.Do(func() { close(release) }) }

	notifier := offlinecache.NewNotifier(mockRelayer, mockWS, zaptest.NewLogger(t))
	defer func() {
		unwedge()
		notifier.Close()
	}()

	// Wedge the worker inside a delivery first, so the whole burst below
	// provably piles up behind a stuck transport rather than racing it.
	notifier.OnItemStateChanged(offlinecache.ItemStatus{ItemID: "item-0", State: offlinecache.StateQueued})
	select {
	case <-sendStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("the worker never reached its first send")
	}

	start := time.Now()
	for _, state := range []offlinecache.ItemState{
		offlinecache.StateQueued, offlinecache.StateDownloading, offlinecache.StateReady,
	} {
		for i := 0; i < items; i++ {
			notifier.OnItemStateChanged(offlinecache.ItemStatus{
				ItemID: fmt.Sprintf("item-%d", i), State: state,
			})
		}
	}
	elapsed := time.Since(start)
	assert.Less(t, elapsed, 2*time.Second,
		"admission must not pay for delivery: %d notifications took %s with the transport wedged", items*3, elapsed)

	unwedge()

	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(lastSeen) == items
	}, 10*time.Second, 10*time.Millisecond,
		"every item of a max-size playlist must eventually be delivered, not dropped once the buffer fills")

	mu.Lock()
	defer mu.Unlock()
	for i := 0; i < items; i++ {
		id := fmt.Sprintf("item-%d", i)
		assert.Equal(t, offlinecache.StateReady, lastSeen[id],
			"%s must end at the last state it actually reached", id)
	}
	// Coalescing is what makes the bound safe: 3x items transitions were
	// pushed, but only the latest per item ever needs delivering, so the
	// transport does far less work than a per-event buffer would have
	// demanded of it (and never had to drop anything to stay bounded).
	assert.Less(t, sendCalls, items*3,
		"successive states of one item must coalesce rather than each occupying their own slot")
}

// TestNotifier_OnItemStateChanged_ConcurrentProducersLoseNoItem exercises
// the wake handoff at the interleaving the other tests structurally cannot
// reach: pushes landing while the worker is actively draining or about to
// sleep. That is the window a lost wakeup lives in — wake is a 1-buffered
// signal, so a push whose token is swallowed while the worker is between
// its drain loop and its next receive would strand that item until some
// unrelated later push happened to wake the worker again.
//
// The transport is free-running here (no wedge) precisely so the drain
// loop and the producers genuinely race, and every item is required to
// arrive at its final state.
func TestNotifier_OnItemStateChanged_ConcurrentProducersLoseNoItem(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	const producers = 8
	const itemsPer = 64

	mockRelayer := mocks.NewMockRelayer(ctrl)
	mockWS := mocks.NewMockWS(ctrl)
	mockRelayer.EXPECT().IsConnected().Return(false).AnyTimes()

	var mu sync.Mutex
	lastSeen := make(map[string]offlinecache.ItemState, producers*itemsPer)
	mockWS.EXPECT().SendAll(gomock.Any()).DoAndReturn(func(data interface{}) error {
		envelope, ok := data.(map[string]interface{})
		if !ok {
			return nil
		}
		status, ok := envelope["message"].(offlinecache.ItemStatus)
		if !ok {
			return nil
		}
		mu.Lock()
		lastSeen[status.ItemID] = status.State
		mu.Unlock()
		return nil
	}).AnyTimes()

	notifier := offlinecache.NewNotifier(mockRelayer, mockWS, zaptest.NewLogger(t))
	defer notifier.Close()

	var wg sync.WaitGroup
	for p := 0; p < producers; p++ {
		wg.Add(1)
		go func(p int) {
			defer wg.Done()
			// queued then ready per item, mirroring the real producer mix
			// (admission emits queued, the capture worker emits terminal).
			for _, state := range []offlinecache.ItemState{offlinecache.StateQueued, offlinecache.StateReady} {
				for i := 0; i < itemsPer; i++ {
					notifier.OnItemStateChanged(offlinecache.ItemStatus{
						ItemID: fmt.Sprintf("p%d-item-%d", p, i), State: state,
					})
				}
			}
		}(p)
	}
	wg.Wait()

	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		if len(lastSeen) != producers*itemsPer {
			return false
		}
		for _, state := range lastSeen {
			if state != offlinecache.StateReady {
				return false
			}
		}
		return true
	}, 10*time.Second, 10*time.Millisecond,
		"every item must reach the transport at its final state; a stranded item means the wake handoff dropped a signal")
}

// TestNotifier_CloseWithin_ReturnsOnBudgetWhenDeliveryIsWedged is the
// regression test for shutdown budget exhaustion. Close waits for an
// in-flight delivery to finish, and neither leg finishes fast enough for
// that: the relayer send waits on that connection's mutex (bounded, but
// several times over the whole shutdown timeout), and ws.WS.SendAll bounds
// each per-connection write but not the loop across however many hub
// clients are connected. main.go force-exits SHUTDOWN_TIMEOUT after
// cancellation, and every cleanup step registered before the notifier runs
// AFTER it (LIFO), so waiting it out here strands all of them.
//
// Bounding the wait does not save all of them — both transports take, for
// their own teardown, the very mutex the abandoned delivery still holds,
// so it just blocks at whichever comes next (see CloseWithin's doc) — but
// mint-pairing and the playlist refresher run either way, and a hub-leg
// wedge additionally frees the relayer, provisioning and mDNS.
//
// CloseWithin must therefore return on its budget while the delivery is
// still wedged, and must still have signaled the worker so nothing new is
// picked up.
func TestNotifier_CloseWithin_ReturnsOnBudgetWhenDeliveryIsWedged(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockRelayer := mocks.NewMockRelayer(ctrl)
	mockWS := mocks.NewMockWS(ctrl)
	mockRelayer.EXPECT().IsConnected().Return(false).AnyTimes()

	release := make(chan struct{})
	sendStarted := make(chan struct{}, 1)
	var sends atomic.Int64
	mockWS.EXPECT().SendAll(gomock.Any()).DoAndReturn(func(interface{}) error {
		sends.Add(1)
		select {
		case sendStarted <- struct{}{}:
		default:
		}
		<-release // stands in for a wedged hub client
		return nil
	}).AnyTimes()

	notifier := offlinecache.NewNotifier(mockRelayer, mockWS, zaptest.NewLogger(t))
	// Released only after the assertions, so the wedge is genuinely still
	// in flight while CloseWithin returns. Released explicitly below rather
	// than only here, since the drop-on-close assertion needs the worker to
	// have actually finished; this stays as the failure-path safety net.
	var releaseOnce sync.Once
	unwedge := func() { releaseOnce.Do(func() { close(release) }) }
	defer func() {
		unwedge()
		notifier.Close()
	}()

	notifier.OnItemStateChanged(offlinecache.ItemStatus{ItemID: "item-1", State: offlinecache.StateQueued})
	select {
	case <-sendStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("the worker never reached its deliberately-wedged delivery")
	}
	// Queue more work behind the wedge: none of it may be picked up after
	// CloseWithin signals, budget expiry notwithstanding.
	for i := 0; i < 10; i++ {
		notifier.OnItemStateChanged(offlinecache.ItemStatus{
			ItemID: fmt.Sprintf("item-%d", i+2), State: offlinecache.StateReady,
		})
	}

	const budget = 100 * time.Millisecond
	start := time.Now()
	stopped := notifier.CloseWithin(budget)
	elapsed := time.Since(start)

	assert.False(t, stopped, "CloseWithin must report that the worker did not stop, not silently claim a clean shutdown")
	assert.Less(t, elapsed, time.Second,
		"CloseWithin must return on its budget rather than waiting out a wedged SendAll: shutdown has %s for everything", budget)

	// Asserting the drop-on-close behavior requires letting the wedge go
	// and waiting for the worker to actually exit. Asserting the count
	// while it is still blocked inside the first SendAll would be vacuous:
	// a single worker cannot have made a second call yet whether or not it
	// honors done at all.
	unwedge()
	notifier.Close()
	assert.Equal(t, int64(1), sends.Load(),
		"the worker must abandon the 10 notifications queued behind the wedge once CloseWithin signaled, not deliver them on the way out")
}

// TestNotifier_CloseWithin_ReturnsImmediatelyWhenIdle pins the happy path
// (the overwhelmingly common one at shutdown): an idle worker stops well
// inside the budget and is reported as stopped, so the warning main.go
// logs on expiry stays meaningful rather than firing on every shutdown.
func TestNotifier_CloseWithin_ReturnsImmediatelyWhenIdle(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	mockWS := mocks.NewMockWS(ctrl)
	mockWS.EXPECT().SendAll(gomock.Any()).Return(nil).AnyTimes()

	// Deliberately the PRODUCTION budget, not a generous test value: this
	// is what demonstrates that 250ms is actually sufficient for the path
	// every healthy shutdown takes, so main.go's expiry warning stays
	// meaningful rather than firing routinely.
	notifier := offlinecache.NewNotifier(nil, mockWS, zaptest.NewLogger(t))
	assert.True(t, notifier.CloseWithin(offlinecache.ShutdownCloseBudget), "an idle worker must stop well inside the production budget")

	// Idempotent, and safe alongside Close, matching Close's own contract.
	assert.True(t, notifier.CloseWithin(offlinecache.ShutdownCloseBudget))
	assert.NotPanics(t, notifier.Close)
}

// TestNotifier_CloseWithin_NoTransportsIsANoop mirrors Close's nil-guard:
// with neither transport configured no worker was ever started, so there
// is nothing to wait on and the call must report success rather than
// burning its budget.
func TestNotifier_CloseWithin_NoTransportsIsANoop(t *testing.T) {
	notifier := offlinecache.NewNotifier(nil, nil, zaptest.NewLogger(t))
	start := time.Now()
	assert.True(t, notifier.CloseWithin(offlinecache.ShutdownCloseBudget))
	assert.Less(t, time.Since(start), time.Second)
}
