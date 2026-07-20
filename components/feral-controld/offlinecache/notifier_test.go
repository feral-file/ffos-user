package offlinecache_test

import (
	"context"
	"testing"

	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/assert"
	"go.uber.org/zap/zaptest"

	"github.com/feral-file/ffos-user/components/feral-controld/mocks"
	"github.com/feral-file/ffos-user/components/feral-controld/offlinecache"
	"github.com/feral-file/ffos-user/components/feral-controld/relayer"
)

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
	mockWS.EXPECT().SendAll(gomock.Any()).Return(nil).Times(1)

	notifier := offlinecache.NewNotifier(mockRelayer, mockWS, zaptest.NewLogger(t))
	notifier.OnItemStateChanged(status)
}

func TestNotifier_OnItemStateChanged_SkipsRelayerWhenDisconnected(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockRelayer := mocks.NewMockRelayer(ctrl)
	mockWS := mocks.NewMockWS(ctrl)

	mockRelayer.EXPECT().IsConnected().Return(false).Times(1)
	// Send must never be called while disconnected.
	mockWS.EXPECT().SendAll(gomock.Any()).Return(nil).Times(1)

	notifier := offlinecache.NewNotifier(mockRelayer, mockWS, zaptest.NewLogger(t))
	notifier.OnItemStateChanged(offlinecache.ItemStatus{ItemID: "item-1", State: offlinecache.StateQueued})
}

func TestNotifier_OnItemStateChanged_NilRelayerAndWSAreNoop(t *testing.T) {
	notifier := offlinecache.NewNotifier(nil, nil, zaptest.NewLogger(t))
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
	mockWS.EXPECT().SendAll(gomock.Any()).Return(assertError("no ws clients")).Times(1)

	notifier := offlinecache.NewNotifier(mockRelayer, mockWS, zaptest.NewLogger(t))
	assert.NotPanics(t, func() {
		notifier.OnItemStateChanged(offlinecache.ItemStatus{ItemID: "item-1", State: offlinecache.StateFailed, Reason: "boom"})
	})
}
