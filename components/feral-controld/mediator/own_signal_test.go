package mediator_test

import (
	"context"
	"testing"

	"github.com/feral-file/godbus"
	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/feral-file/ffos-user/components/feral-controld/dbus"
	"github.com/feral-file/ffos-user/components/feral-controld/mediator"
	"github.com/feral-file/ffos-user/components/feral-controld/mocks"
)

// TestMediator_HandleDBusSignal_DropsOwnSignals pins that controld's own
// emitted signals, echoed back by the /com/feralfile namespace match, are
// dropped without logging, while an unknown signal from another daemon still
// gets its "Unknown signal" Warn.
func TestMediator_HandleDBusSignal_DropsOwnSignals(t *testing.T) {
	core, logs := observer.New(zap.DebugLevel)
	ctrl := gomock.NewController(t)
	mockDbus := mocks.NewMockDBus(ctrl)
	mockRelayer := mocks.NewMockRelayer(ctrl)

	var handler func(context.Context, godbus.DBusPayload) ([]interface{}, error)
	mockDbus.EXPECT().OnBusSignal(gomock.Any()).
		DoAndReturn(func(h func(context.Context, godbus.DBusPayload) ([]interface{}, error)) {
			handler = h
		}).Times(1)
	mockRelayer.EXPECT().OnRelayerMessage(gomock.Any()).Times(1)

	med := mediator.New(
		context.Background(),
		mockRelayer,
		mockDbus,
		mocks.NewMockCDP(ctrl),
		mocks.NewMockCommandHandler(ctrl),
		mocks.NewMockExecutor(ctrl),
		mocks.NewMockRefresher(ctrl),
		mocks.NewMockJSON(ctrl),
		zap.New(core),
	)
	med.Start()
	require.NotNil(t, handler)

	result, err := handler(context.Background(), godbus.DBusPayload{
		Interface: dbus.INTERFACE,
		Path:      dbus.PATH,
		Member:    dbus.EVENT_CDP_STUCK,
		Body:      []interface{}{false},
	})
	require.NoError(t, err)
	assert.Nil(t, result)
	assert.Zero(t, logs.Len(), "controld's own echoed signal must not be logged")

	_, err = handler(context.Background(), godbus.DBusPayload{
		Path:   godbus.Path("/com/feralfile/other"),
		Member: godbus.Member("unknown_signal"),
	})
	require.NoError(t, err)
	assert.Equal(t, 1, logs.FilterMessage("Unknown signal").Len(), "a foreign unknown signal must still be reported")
}
