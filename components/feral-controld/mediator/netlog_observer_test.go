package mediator_test

import (
	"context"
	"sync"
	"testing"

	"github.com/feral-file/godbus"
	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/feral-file/ffos-user/components/feral-controld/cdp"
	"github.com/feral-file/ffos-user/components/feral-controld/dbus"
)

// TestMediator_InternetObserverSeesAppliedEdge pins the netlog wiring seam:
// SetInternetObserver receives every APPLIED connectivity_change verdict (the
// same value written to the mediator's cache), so the flight recorder's
// timeline can never disagree with what the mediator acted on.
func TestMediator_InternetObserverSeesAppliedEdge(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()
	wireConnectivitySession(t, ts)

	var mu sync.Mutex
	var seen []bool
	sink, ok := ts.mediator.(interface{ SetInternetObserver(func(bool)) })
	require.True(t, ok, "mediator must expose the SetInternetObserver seam")
	sink.SetInternetObserver(func(connected bool) {
		mu.Lock()
		seen = append(seen, connected)
		mu.Unlock()
	})

	ts.mockCDP.EXPECT().
		Send(cdp.METHOD_EVALUATE, gomock.Any()).
		Return(map[string]interface{}{"result": "ok"}, nil).
		AnyTimes()
	ts.mockRelayer.EXPECT().IsConnected().Return(true).AnyTimes()

	var capturedHandler func(context.Context, godbus.DBusPayload) ([]interface{}, error)
	ts.mockDbus.EXPECT().
		OnBusSignal(gomock.Any()).
		DoAndReturn(func(handler func(context.Context, godbus.DBusPayload) ([]interface{}, error)) {
			capturedHandler = handler
		}).Times(1)
	ts.mockRelayer.EXPECT().OnRelayerMessage(gomock.Any()).Times(1)
	ts.mediator.Start()

	for _, v := range []bool{true, false} {
		_, err := capturedHandler(ts.ctx, godbus.DBusPayload{
			Member: dbus.MONITORD_EVENT_CONNECTIVITY_CHANGE,
			Body:   []interface{}{v},
		})
		require.NoError(t, err)
		ts.waitForConnectivityPush(t)
	}

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, []bool{true, false}, seen,
		"observer must see each applied edge, in order")
}
