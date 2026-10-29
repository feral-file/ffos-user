package mediator_test

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/feral-file/godbus"
	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/feral-file/ffos-user/components/feral-controld/cdp"
	"github.com/feral-file/ffos-user/components/feral-controld/dbus"
	"github.com/feral-file/ffos-user/components/feral-controld/mediator"
	"github.com/feral-file/ffos-user/components/feral-controld/mocks"
	"github.com/feral-file/ffos-user/components/feral-controld/relayer"
)

// probingRelayer is the mock relayer plus the retirement-probe seam the
// mediator type-asserts for (relayer.AllowRetiredProbe).
type probingRelayer struct {
	*mocks.MockRelayer
	mu    sync.Mutex
	calls []string
}

func (p *probingRelayer) AllowRetiredProbe() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, "probe")
}

func (p *probingRelayer) record(call string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, call)
}

// An offline→online edge must arm the latched relayer's probe BEFORE it
// reconnects: a captive portal's NXDOMAIN latched it, and without the arm
// the device would stay off the legacy relayer for the whole probe interval.
// A still-retired relayer is not an error.
func TestMediator_ConnectivityEdgeArmsRetiredProbe(t *testing.T) {
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
	wireConnectivitySession(t, ts)

	ts.mockCDP.EXPECT().
		Send(cdp.METHOD_EVALUATE, map[string]interface{}{"expression": "window.handleConnectivityChange(true)"}).
		Return(map[string]interface{}{"result": "ok"}, nil).
		Times(1)
	ts.mockRelayer.EXPECT().IsConnected().Return(false).AnyTimes()
	ts.mockRelayer.EXPECT().RetryableConnect(gomock.Any()).DoAndReturn(func(context.Context) error {
		pr.record("connect")
		return fmt.Errorf("%w: gone", relayer.ErrLegacyRetired)
	}).Times(1)

	var handler func(context.Context, godbus.DBusPayload) ([]interface{}, error)
	ts.mockDbus.EXPECT().OnBusSignal(gomock.Any()).DoAndReturn(
		func(h func(context.Context, godbus.DBusPayload) ([]interface{}, error)) { handler = h }).Times(1)
	ts.mockRelayer.EXPECT().OnRelayerMessage(gomock.Any()).Times(1)
	ts.mediator.Start()

	_, err := handler(ts.ctx, godbus.DBusPayload{Member: dbus.MONITORD_EVENT_CONNECTIVITY_CHANGE, Body: []interface{}{true}})
	require.NoError(t, err)
	ts.waitForConnectivityPush(t)

	pr.mu.Lock()
	defer pr.mu.Unlock()
	assert.Equal(t, []string{"probe", "connect"}, pr.calls)
}
