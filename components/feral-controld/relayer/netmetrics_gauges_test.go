package relayer_test

import (
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/golang/mock/gomock"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/feral-file/ffos-user/components/feral-controld/netmetrics"
)

// netGauge reads one single-series family from the netmetrics registry.
// found=false means the family is absent (part of the contract: no close code
// exported until one is observed).
func netGauge(t *testing.T, name string) (value float64, found bool) {
	t.Helper()
	families, err := netmetrics.Gatherer().Gather()
	require.NoError(t, err)
	for _, mf := range families {
		if mf.GetName() != name {
			continue
		}
		require.Len(t, mf.GetMetric(), 1)
		m := mf.GetMetric()[0]
		if m.GetCounter() != nil {
			return m.GetCounter().GetValue(), true
		}
		return m.GetGauge().GetValue(), true
	}
	return 0, false
}

// TestConnectionGaugesFollowLifecycle pins the stage-0 relayer instrumentation
// (docs/wan-outage-observability.md): relayer_connected tracks the real
// connection, a teardown counts exactly one disconnect, and the close code of
// the frame that ended the session is exported. Deltas (not absolutes) because
// sibling tests in this package also open/close connections against the same
// process-wide registry.
func TestConnectionGaugesFollowLifecycle(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()
	setupMockTicker(ts)

	ts.mockClock.EXPECT().Now().Return(time.Time{}).AnyTimes()
	ts.mockDialer.EXPECT().
		DialContext(ts.ctx, gomock.Any(), nil).
		Return(ts.mockConn, &http.Response{StatusCode: http.StatusOK}, nil).
		Times(1)
	ts.mockConn.EXPECT().SetPongHandler(gomock.Any()).Times(1)
	ts.mockConn.EXPECT().WriteJSON(gomock.Any()).Return(nil).AnyTimes()
	ts.mockConn.EXPECT().SetReadDeadline(gomock.Any()).Return(nil).AnyTimes()

	// Read blocks until Close, then surfaces a close frame with a code the
	// gauge must capture (1011: server internal error — a value distinct from
	// anything a sibling test emits, so the assertion cannot pass by accident).
	readStarted := make(chan struct{})
	releaseRead := make(chan struct{})
	readReturned := make(chan struct{})
	ts.mockConn.EXPECT().
		ReadMessage().
		DoAndReturn(func() (int, []byte, error) {
			close(readStarted)
			<-releaseRead
			defer close(readReturned)
			return 0, nil, &websocket.CloseError{Code: websocket.CloseInternalServerErr, Text: "middlebox says no"}
		}).
		Times(1)
	ts.mockConn.EXPECT().
		WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), gomock.Any()).
		Return(nil).
		Times(1)
	releaseOnce := sync.Once{}
	ts.mockConn.EXPECT().
		Close().
		DoAndReturn(func() error {
			releaseOnce.Do(func() { close(releaseRead) })
			return nil
		}).
		Times(1)

	disconnectsBefore, _ := netGauge(t, "relayer_disconnects_total")

	require.NoError(t, ts.client.Connect(ts.ctx))
	<-readStarted
	v, found := netGauge(t, "relayer_connected")
	require.True(t, found)
	assert.Equal(t, 1.0, v, "gauge must report connected after Connect")

	ts.client.Close()
	select {
	case <-readReturned:
	case <-time.After(1 * time.Second):
		t.Fatal("read loop should return after close")
	}

	v, _ = netGauge(t, "relayer_connected")
	assert.Equal(t, 0.0, v, "gauge must report disconnected after Close")
	disconnectsAfter, _ := netGauge(t, "relayer_disconnects_total")
	assert.Equal(t, disconnectsBefore+1, disconnectsAfter, "one teardown = one disconnect")

	// The read loop observed the close frame after Close released it; the code
	// must be exported regardless of which teardown path won.
	assert.Eventually(t, func() bool {
		code, found := netGauge(t, "relayer_last_close_code")
		return found && code == float64(websocket.CloseInternalServerErr)
	}, time.Second, 10*time.Millisecond, "close code from the final frame must be exported")
}
