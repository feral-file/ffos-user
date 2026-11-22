package relayer_test

import (
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/golang/mock/gomock"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"

	"github.com/feral-file/ffos-user/components/feral-controld/relayer"
)

// fakeNow is a settable clock reading for the retirement-latch tests.
type fakeNow struct {
	mu sync.Mutex
	t  time.Time
}

func (f *fakeNow) now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.t
}

func (f *fakeNow) set(t time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.t = t
}

func retiredOf(t *testing.T, r relayer.Relayer) interface {
	Retired() bool
	AllowRetiredProbe()
} {
	t.Helper()
	v, ok := r.(interface {
		Retired() bool
		AllowRetiredProbe()
	})
	require.True(t, ok)
	return v
}

func expect410(ts *testSetup, times int) {
	ts.mockDialer.EXPECT().
		DialContext(ts.ctx, gomock.Any(), nil).
		Return(nil, &http.Response{StatusCode: http.StatusGone}, websocket.ErrBadHandshake).
		Times(times)
}

// A 410 latches retired; afterwards nothing is dialed until the escalating
// probe is due: 1 min, then 2, 4, ... capped at 1 h.
func TestLegacyRetired_LatchesAndProbesWithEscalation(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()
	clock := &fakeNow{t: time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)}
	ts.mockClock.EXPECT().Now().DoAndReturn(clock.now).AnyTimes()
	r := retiredOf(t, ts.client)

	expect410(ts, 1)
	err := ts.client.RetryableConnect(ts.ctx)
	assert.ErrorIs(t, err, relayer.ErrLegacyRetired)
	var permanent relayer.PermanentError
	assert.ErrorAs(t, err, &permanent, "the underlying permanent error stays visible")
	assert.True(t, r.Retired())

	// The first gate check starts the clock; nothing dials before a probe is
	// due (an extra DialContext would break the Times(1) above).
	assert.ErrorIs(t, ts.client.RetryableConnect(ts.ctx), relayer.ErrLegacyRetired)
	at := clock.now()
	for _, interval := range []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute} {
		clock.set(at.Add(interval - time.Second))
		assert.ErrorIs(t, ts.client.RetryableConnect(ts.ctx), relayer.ErrLegacyRetired, "before %v", interval)
		expect410(ts, 1)
		at = at.Add(interval)
		clock.set(at)
		assert.ErrorIs(t, ts.client.RetryableConnect(ts.ctx), relayer.ErrLegacyRetired, "probe at %v", interval)
	}

	// The interval caps at an hour.
	for i := 0; i < 10; i++ {
		expect410(ts, 1)
		at = at.Add(time.Hour)
		clock.set(at)
		assert.ErrorIs(t, ts.client.RetryableConnect(ts.ctx), relayer.ErrLegacyRetired)
	}
	clock.set(at.Add(59 * time.Minute))
	assert.ErrorIs(t, ts.client.RetryableConnect(ts.ctx), relayer.ErrLegacyRetired, "capped at one dial an hour")
}

// A non-temporary DNS failure (a removed host, or a captive portal's resolver)
// latches the same way, through categorizeWebsocketError's DNS branch.
func TestLegacyRetired_NXDOMAINLatches(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()
	ts.mockClock.EXPECT().Now().Return(time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)).AnyTimes()
	r := retiredOf(t, ts.client)

	ts.mockDialer.EXPECT().
		DialContext(ts.ctx, gomock.Any(), nil).
		Return(nil, nil, &net.DNSError{Err: "no such host", Name: "v1-gone.invalid", IsNotFound: true}).
		Times(1)
	assert.ErrorIs(t, ts.client.RetryableConnect(ts.ctx), relayer.ErrLegacyRetired)
	assert.True(t, r.Retired())
	assert.ErrorIs(t, ts.client.RetryableConnect(ts.ctx), relayer.ErrLegacyRetired, "no redial while latched")
}

// An online edge (AllowRetiredProbe) earns exactly one dial, e.g. after a
// captive portal's NXDOMAIN latched the relayer.
func TestLegacyRetired_OnlineEdgeDialsOnce(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()
	clock := &fakeNow{t: time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)}
	ts.mockClock.EXPECT().Now().DoAndReturn(clock.now).AnyTimes()
	r := retiredOf(t, ts.client)

	expect410(ts, 1)
	assert.ErrorIs(t, ts.client.RetryableConnect(ts.ctx), relayer.ErrLegacyRetired)
	assert.ErrorIs(t, ts.client.RetryableConnect(ts.ctx), relayer.ErrLegacyRetired)

	expect410(ts, 1)
	r.AllowRetiredProbe()
	assert.ErrorIs(t, ts.client.RetryableConnect(ts.ctx), relayer.ErrLegacyRetired)
	assert.ErrorIs(t, ts.client.RetryableConnect(ts.ctx), relayer.ErrLegacyRetired, "one dial per edge")
}

// A successful probe clears the latch.
func TestLegacyRetired_SuccessfulProbeClears(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()
	setupMockTicker(ts)
	clock := &fakeNow{t: time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)}
	ts.mockClock.EXPECT().Now().DoAndReturn(clock.now).AnyTimes()
	r := retiredOf(t, ts.client)

	expect410(ts, 1)
	assert.ErrorIs(t, ts.client.RetryableConnect(ts.ctx), relayer.ErrLegacyRetired)

	release := make(chan struct{})
	ts.mockDialer.EXPECT().
		DialContext(ts.ctx, gomock.Any(), nil).
		Return(ts.mockConn, &http.Response{StatusCode: http.StatusSwitchingProtocols}, nil).
		Times(1)
	ts.mockConn.EXPECT().SetPongHandler(gomock.Any()).AnyTimes()
	ts.mockConn.EXPECT().SetReadDeadline(gomock.Any()).Return(nil).AnyTimes()
	ts.mockConn.EXPECT().WriteJSON(gomock.Any()).Return(nil).AnyTimes()
	ts.mockConn.EXPECT().ReadMessage().DoAndReturn(func() (int, []byte, error) {
		<-release
		return 0, nil, websocket.ErrCloseSent
	}).AnyTimes()
	ts.mockConn.EXPECT().WriteControl(websocket.CloseMessage, gomock.Any(), gomock.Any()).Return(nil).AnyTimes()
	ts.mockConn.EXPECT().Close().Return(nil).AnyTimes()

	r.AllowRetiredProbe()
	require.NoError(t, ts.client.RetryableConnect(ts.ctx))
	assert.False(t, r.Retired())

	ts.client.Close()
	close(release)
}

// No legacy endpoint configured: never dial, latched from the first attempt.
func TestLegacyRetired_EmptyEndpointNeverDials(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()
	client := relayer.New("", "", ts.mockDialer, ts.mockRandomizer, ts.mockClock, ts.mockOS, ts.mockJSON, zaptest.NewLogger(t))
	ts.mockClock.EXPECT().Now().Return(time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)).AnyTimes()

	assert.ErrorIs(t, client.Connect(ts.ctx), relayer.ErrLegacyRetired)
	assert.ErrorIs(t, client.RetryableConnect(ts.ctx), relayer.ErrLegacyRetired)
	assert.True(t, retiredOf(t, client).Retired())
}
