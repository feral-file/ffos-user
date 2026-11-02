package mediator_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/feral-file/godbus"
	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/feral-file/ffos-user/components/feral-controld/cdp"
	"github.com/feral-file/ffos-user/components/feral-controld/dbus"
	"github.com/feral-file/ffos-user/components/feral-controld/mediator"
)

// An online edge wakes the v2 carrier BEFORE the legacy reconnect (which may
// block for as long as the legacy host keeps failing), whether or not the
// legacy socket is connected, and an offline edge does not wake it
// (feral-file#3551 G3 bench F2).
func TestMediator_ConnectivityEdgeWakesRelayV2(t *testing.T) {
	for _, tc := range []struct {
		name            string
		connected       bool
		legacyConnected bool
		want            []string
	}{
		{"online edge, legacy down", true, false, []string{"wake-v2", "connect"}},
		{"online edge, legacy up", true, true, []string{"wake-v2"}},
		{"offline edge", false, false, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts := setup(t)
			defer ts.teardown()
			pr := &probingRelayer{MockRelayer: ts.mockRelayer}
			ts.mediator = mediator.New(ts.ctx, pr, ts.mockDbus, ts.mockCDP, ts.mockCommandHandler,
				ts.mockExecutor, ts.mockRefresher, ts.mockJSON, ts.logger)
			ts.mediator.SetConnectivityPushHook(func() {
				select {
				case ts.connPushDone <- struct{}{}:
				default:
				}
			})
			sink, ok := ts.mediator.(interface{ SetRelayV2Waker(func()) })
			require.True(t, ok, "mediator must expose the SetRelayV2Waker seam")
			sink.SetRelayV2Waker(func() { pr.record("wake-v2") })
			wireConnectivitySession(t, ts)

			ts.mockCDP.EXPECT().
				Send(cdp.METHOD_EVALUATE, map[string]interface{}{
					"expression": fmt.Sprintf("window.handleConnectivityChange(%t)", tc.connected),
				}).
				Return(map[string]interface{}{"result": "ok"}, nil).
				Times(1)
			ts.mockRelayer.EXPECT().IsConnected().Return(tc.legacyConnected).AnyTimes()
			ts.mockRelayer.EXPECT().RetryableConnect(gomock.Any()).DoAndReturn(func(context.Context) error {
				pr.record("connect")
				return nil
			}).AnyTimes()

			var handler func(context.Context, godbus.DBusPayload) ([]interface{}, error)
			ts.mockDbus.EXPECT().OnBusSignal(gomock.Any()).DoAndReturn(
				func(h func(context.Context, godbus.DBusPayload) ([]interface{}, error)) { handler = h }).Times(1)
			ts.mockRelayer.EXPECT().OnRelayerMessage(gomock.Any()).Times(1)
			ts.mediator.Start()

			_, err := handler(ts.ctx, godbus.DBusPayload{
				Member: dbus.MONITORD_EVENT_CONNECTIVITY_CHANGE, Body: []interface{}{tc.connected},
			})
			require.NoError(t, err)
			ts.waitForConnectivityPush(t)

			pr.mu.Lock()
			defer pr.mu.Unlock()
			var got []string
			for _, c := range pr.calls {
				if c != "probe" { // the retired-probe arm is pinned elsewhere
					got = append(got, c)
				}
			}
			assert.Equal(t, tc.want, got)
		})
	}
}
