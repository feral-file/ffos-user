package offlinecache_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/assert"
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
