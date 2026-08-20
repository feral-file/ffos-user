package uarewrite

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest"

	"github.com/feral-file/ffos-user/components/feral-controld/mocks"
	"github.com/feral-file/ffos-user/components/feral-controld/offlinecache"
	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

func testInterceptorWithClock(t *testing.T, hosts []string, clock wrapper.Clock) *Interceptor {
	t.Helper()

	policy, err := New(hosts, "feral-player/test")
	require.NoError(t, err)

	i := NewInterceptor(policy, "http://127.0.0.1:9222", nil, nil, wrapper.NewJSON(), nil, clock, zaptest.NewLogger(t))
	t.Cleanup(func() { _ = i.Close() })
	return i
}

func testInterceptor(t *testing.T, hosts []string) *Interceptor {
	t.Helper()

	policy, err := New(hosts, "feral-player/test")
	require.NoError(t, err)

	i := NewInterceptor(policy, "http://127.0.0.1:9222", nil, nil, wrapper.NewJSON(), nil, wrapper.NewClock(), zaptest.NewLogger(t))
	// The supervisor goroutine starts on the first AttachOnReconnect; Close
	// is what stops it, so every test tears it down even on failure paths.
	t.Cleanup(func() { _ = i.Close() })
	return i
}

// mockClockWithTameTicker builds a MockClock whose NewTicker returns a ticker
// that never fires (a nil channel blocks forever in select). Tests drive the
// supervisor's PASS (ensureArmed) synchronously instead of racing its loop;
// the ticker exists only so the loop can start and park without panicking.
func mockClockWithTameTicker(t *testing.T, ctrl *gomock.Controller) *mocks.MockClock {
	t.Helper()

	clock := mocks.NewMockClock(ctrl)
	ticker := mocks.NewMockTicker(ctrl)
	ticker.EXPECT().C().Return(nil).AnyTimes()
	ticker.EXPECT().Stop().AnyTimes()
	clock.EXPECT().NewTicker(gomock.Any()).Return(ticker).AnyTimes()
	return clock
}

func pausedEvent(t *testing.T, requestID, url string, headers map[string]string, responseStatus *int) json.RawMessage {
	t.Helper()

	payload := map[string]interface{}{
		"requestId": requestID,
		"request": map[string]interface{}{
			"url":     url,
			"headers": headers,
		},
	}
	if responseStatus != nil {
		payload["responseStatusCode"] = *responseStatus
	}
	raw, err := json.Marshal(payload)
	require.NoError(t, err)
	return raw
}

// headerValue pulls one header out of the continueRequest args the
// interceptor built, so assertions read against the wire shape CDP receives
// rather than against an internal helper.
func headerValue(args map[string]interface{}, name string) (string, bool) {
	headers, ok := args["headers"].([]map[string]string)
	if !ok {
		return "", false
	}
	for _, h := range headers {
		if h["name"] == name {
			return h["value"], true
		}
	}
	return "", false
}

func TestProcessPausedRewritesMatchingHost(t *testing.T) {
	t.Parallel()

	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	session := mocks.NewMockCDPSession(ctrl)
	var got map[string]interface{}
	session.EXPECT().
		Send(gomock.Any(), "Fetch.continueRequest", gomock.Any()).
		DoAndReturn(func(_ interface{}, _ string, args map[string]interface{}) (json.RawMessage, error) {
			got = args
			return nil, nil
		})

	i := testInterceptor(t, []string{"ipfs.io"})
	i.processPaused(session, pausedEvent(t, "req-1", "https://ipfs.io/ipfs/QmAbc",
		map[string]string{"User-Agent": "Mozilla/5.0 Chrome/150", "Accept": "image/*"}, nil))

	assert.Equal(t, "req-1", got["requestId"])
	ua, ok := headerValue(got, "User-Agent")
	require.True(t, ok, "rewritten request must carry a User-Agent header")
	assert.Equal(t, "feral-player/test", ua)

	accept, ok := headerValue(got, "Accept")
	require.True(t, ok, "unrelated headers must survive the rewrite")
	assert.Equal(t, "image/*", accept)
}

// A wider pattern set than this policy asked for is the expected steady state
// once Fetch arming is shared with offline replay: this handler will then see
// requests that are not ours. Passing them through UNTOUCHED is what keeps the
// scoping guarantee — rewriting an origin nobody reasoned about can break
// artworks that render today.
func TestProcessPausedPassesThroughUnmatchedHost(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		url  string
	}{
		{name: "unlisted origin", url: "https://gateway.pinata.cloud/ipfs/QmAbc"},
		{name: "player shell", url: "http://127.0.0.1:8080/playlist"},
		{name: "subdomain of a listed host", url: "https://cdn.ipfs.io/ipfs/QmAbc"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			ctrl := gomock.NewController(t)
			defer ctrl.Finish()

			session := mocks.NewMockCDPSession(ctrl)
			var got map[string]interface{}
			session.EXPECT().
				Send(gomock.Any(), "Fetch.continueRequest", gomock.Any()).
				DoAndReturn(func(_ interface{}, _ string, args map[string]interface{}) (json.RawMessage, error) {
					got = args
					return nil, nil
				})

			i := testInterceptor(t, []string{"ipfs.io"})
			i.processPaused(session, pausedEvent(t, "req-2", tt.url,
				map[string]string{"User-Agent": "Mozilla/5.0 Chrome/150"}, nil))

			assert.Equal(t, "req-2", got["requestId"])
			assert.NotContains(t, got, "headers",
				"a non-matching request must continue with its original headers")
		})
	}
}

// The policy arms Request stage only. A Response-stage pause means something
// else armed a wider pattern on this session, and by then the bytes are
// already fetched — rewriting the request headers would be meaningless.
func TestProcessPausedIgnoresResponseStagePause(t *testing.T) {
	t.Parallel()

	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	session := mocks.NewMockCDPSession(ctrl)
	var got map[string]interface{}
	session.EXPECT().
		Send(gomock.Any(), "Fetch.continueRequest", gomock.Any()).
		DoAndReturn(func(_ interface{}, _ string, args map[string]interface{}) (json.RawMessage, error) {
			got = args
			return nil, nil
		})

	status := 200
	i := testInterceptor(t, []string{"ipfs.io"})
	i.processPaused(session, pausedEvent(t, "req-3", "https://ipfs.io/ipfs/QmAbc",
		map[string]string{"User-Agent": "Mozilla/5.0 Chrome/150"}, &status))

	assert.Equal(t, "req-3", got["requestId"])
	assert.NotContains(t, got, "headers")
}

// A request that is paused and never continued blocks that asset forever,
// which is worse than the bug being fixed. Pin the invariant that every
// answerable event produces exactly one continueRequest.
func TestProcessPausedAlwaysAnswersAnswerableEvents(t *testing.T) {
	t.Parallel()

	for _, url := range []string{
		"https://ipfs.io/ipfs/QmAbc",
		"https://gateway.pinata.cloud/ipfs/QmAbc",
		"data:image/png;base64,iVBORw0KGgo=",
		"",
	} {
		ctrl := gomock.NewController(t)
		session := mocks.NewMockCDPSession(ctrl)
		session.EXPECT().
			Send(gomock.Any(), "Fetch.continueRequest", gomock.Any()).
			Return(nil, nil).
			Times(1)

		i := testInterceptor(t, []string{"ipfs.io"})
		i.processPaused(session, pausedEvent(t, "req-4", url, nil, nil))
		ctrl.Finish()
	}
}

// No requestId means no handle to answer by, so there is nothing to send.
// Asserting "no Send at all" keeps a future edit from inventing a
// continueRequest against a zero-value id, which targets no request and only
// adds a rejected CDP call.
func TestProcessPausedWithoutRequestIDSendsNothing(t *testing.T) {
	t.Parallel()

	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	session := mocks.NewMockCDPSession(ctrl)
	session.EXPECT().Send(gomock.Any(), gomock.Any(), gomock.Any()).Times(0)

	i := testInterceptor(t, []string{"ipfs.io"})
	i.processPaused(session, pausedEvent(t, "", "https://ipfs.io/ipfs/QmAbc", nil, nil))
}

func TestProcessPausedWithMalformedEventSendsNothing(t *testing.T) {
	t.Parallel()

	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	session := mocks.NewMockCDPSession(ctrl)
	session.EXPECT().Send(gomock.Any(), gomock.Any(), gomock.Any()).Times(0)

	i := testInterceptor(t, []string{"ipfs.io"})
	i.processPaused(session, json.RawMessage(`{"requestId": 12345}`))
}

// A dead socket must not panic the read-pump's child goroutine: the daemon
// keeps running with an unarmed kiosk and re-arms on the next reconnect.
func TestProcessPausedSurvivesSendFailure(t *testing.T) {
	t.Parallel()

	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	session := mocks.NewMockCDPSession(ctrl)
	session.EXPECT().
		Send(gomock.Any(), "Fetch.continueRequest", gomock.Any()).
		Return(nil, errors.New("socket closed"))

	i := testInterceptor(t, []string{"ipfs.io"})
	assert.NotPanics(t, func() {
		i.processPaused(session, pausedEvent(t, "req-5", "https://ipfs.io/ipfs/QmAbc",
			map[string]string{"User-Agent": "Mozilla/5.0 Chrome/150"}, nil))
	})
}

// Close must be safe before any session exists — run() defers it
// unconditionally, including on a startup path that never reached the CDP
// connect hook.
func TestCloseWithoutSessionIsNoOp(t *testing.T) {
	t.Parallel()

	i := testInterceptor(t, []string{"ipfs.io"})
	assert.NoError(t, i.Close())
}

// The interceptor's socket is INDEPENDENT of the daemon's primary CDP client,
// so it can die while the primary stays healthy — and the primary's reconnect
// hook, the only external caller of AttachOnReconnect, then never fires. The
// pieces below are the recovery machinery for that: recoverFrom accelerates,
// retire keeps state honest, claimAttach paces, and the supervisor is the
// guarantee that none of it depends on Fetch traffic still flowing.

func TestRecoverableSendFailure(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "transport failure", err: fmt.Errorf("wrapped: %w", offlinecache.ErrCDPTransport), want: true},
		// A timeout means the write went out and no reply came back. The
		// socket may be open, but a Fetch client that cannot get replies is
		// not intercepting anything — and Chromium is still holding the
		// requests it paused for us. Recovery (retire, so Chromium releases
		// them) is the only way those assets fail fast instead of hanging.
		{name: "reply timeout", err: context.DeadlineExceeded, want: true},
		// The peer ANSWERED. The socket is healthy; re-dialing a kiosk
		// that answers fine is pointless churn.
		{name: "cdp error reply", err: errors.New("Fetch.continueRequest: invalid requestId"), want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, recoverableSendFailure(tt.err))
		})
	}
}

// A timed-out continueRequest must retire (close) the session: Chromium only
// releases a paused request when the Fetch client that paused it detaches,
// so without the close the asset hangs past every bound we claim to enforce.
func TestProcessPausedTimeoutRetiresSession(t *testing.T) {
	t.Parallel()

	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	clock := mockClockWithTameTicker(t, ctrl)
	clock.EXPECT().Now().Return(time.Unix(1000, 0)).AnyTimes()

	session := mocks.NewMockCDPSession(ctrl)
	session.EXPECT().
		Send(gomock.Any(), "Fetch.continueRequest", gomock.Any()).
		Return(nil, context.DeadlineExceeded)
	closed := make(chan struct{})
	session.EXPECT().Close().DoAndReturn(func() error {
		close(closed)
		return nil
	})

	i := testInterceptorWithClock(t, []string{"ipfs.io"}, clock)
	i.session = session
	// Point the dial seam at a permanent failure so the accelerator's
	// follow-up attach cannot dial a real socket from a unit test.
	i.dial = failingDial(errors.New("no kiosk in unit tests"))

	i.processPaused(session, pausedEvent(t, "req-t1", "https://ipfs.io/ipfs/QmAbc",
		map[string]string{"User-Agent": "Mozilla/5.0 Chrome/150"}, nil))

	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the wedged session to be closed")
	}

	i.mu.Lock()
	defer i.mu.Unlock()
	assert.Nil(t, i.session, "a retired session must leave the honest not-armed state behind")
}

// A dead socket fails EVERY request it was holding. Retirement is identity-
// guarded so late arrivals from the old session cannot disturb the live one
// a reconnect already installed.
func TestRetireIgnoresSupersededSession(t *testing.T) {
	t.Parallel()

	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	old := mocks.NewMockCDPSession(ctrl) // Close must NOT be called
	current := mocks.NewMockCDPSession(ctrl)
	// The helper's cleanup closes whatever is installed; that teardown
	// close is expected — retire closing OLD is what must never happen.
	current.EXPECT().Close().Return(nil).MaxTimes(1)

	i := testInterceptor(t, []string{"ipfs.io"})
	i.session = current

	assert.False(t, i.retire(old))

	i.mu.Lock()
	defer i.mu.Unlock()
	assert.Equal(t, current, i.session, "the live session must survive a stale retirement")
}

// One dial per cooldown window, no matter how many triggers fire: the
// trigger is per-request, so a dead socket produces a burst of identical
// failures, and the supervisor ticks on top of that.
func TestClaimAttachPacesAndSingleFlights(t *testing.T) {
	t.Parallel()

	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	base := time.Unix(1000, 0)
	clock := mockClockWithTameTicker(t, ctrl)
	gomock.InOrder(
		clock.EXPECT().Now().Return(base),
		clock.EXPECT().Now().Return(base.Add(redialCooldown-time.Second)),
		clock.EXPECT().Now().Return(base.Add(redialCooldown)),
	)

	i := testInterceptorWithClock(t, []string{"ipfs.io"}, clock)

	assert.True(t, i.claimAttach(), "first claim wins")
	assert.False(t, i.claimAttach(), "in-flight attempt must block a second")
	i.releaseAttach()
	assert.False(t, i.claimAttach(), "inside the cooldown window must not claim")
	assert.True(t, i.claimAttach(), "past the cooldown claims again")
}

// The supervisor pass with nothing installed must attach — this is what
// recovers BOTH an initial attach failure and a failed re-dial, neither of
// which any Fetch event can ever retrigger.
func TestEnsureArmedAttachesWhenNothingInstalled(t *testing.T) {
	t.Parallel()

	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	clock := mockClockWithTameTicker(t, ctrl)
	clock.EXPECT().Now().Return(time.Unix(1000, 0)).AnyTimes()

	i := testInterceptorWithClock(t, []string{"ipfs.io"}, clock)
	dialed := make(chan struct{}, 1)
	i.dial = func(context.Context, string, wrapper.HTTPClient, wrapper.WebSocketDialer, wrapper.JSON, wrapper.IO, *zap.Logger) (offlinecache.CDPSession, error) {
		dialed <- struct{}{}
		return nil, errors.New("no kiosk in unit tests")
	}

	i.ensureArmed()

	select {
	case <-dialed:
	case <-time.After(2 * time.Second):
		t.Fatal("ensureArmed with no session must attempt an attach")
	}
}

// A session that dies IDLE produces no failed request — the probe is the
// only thing that can notice it. It must classify via the same
// recoverableSendFailure gate, retire the corpse, and attach.
func TestEnsureArmedRetiresDeadSessionFoundByProbe(t *testing.T) {
	t.Parallel()

	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	clock := mockClockWithTameTicker(t, ctrl)
	clock.EXPECT().Now().Return(time.Unix(1000, 0)).AnyTimes()

	session := mocks.NewMockCDPSession(ctrl)
	session.EXPECT().
		Send(gomock.Any(), "Fetch.enable", gomock.Any()).
		Return(nil, fmt.Errorf("%w: cdp session closed", offlinecache.ErrCDPTransport))
	session.EXPECT().Close().Return(nil)

	i := testInterceptorWithClock(t, []string{"ipfs.io"}, clock)
	i.session = session
	dialed := make(chan struct{}, 1)
	i.dial = func(context.Context, string, wrapper.HTTPClient, wrapper.WebSocketDialer, wrapper.JSON, wrapper.IO, *zap.Logger) (offlinecache.CDPSession, error) {
		dialed <- struct{}{}
		return nil, errors.New("no kiosk in unit tests")
	}

	i.ensureArmed()

	select {
	case <-dialed:
	case <-time.After(2 * time.Second):
		t.Fatal("a dead session found by the probe must trigger an attach")
	}

	i.mu.Lock()
	defer i.mu.Unlock()
	assert.Nil(t, i.session)
}

// A probe REFUSED on a live socket must not churn: the kiosk answered, so
// re-dialing it reproduces the same refusal at dial cost.
func TestEnsureArmedLeavesLiveSessionOnRefusedProbe(t *testing.T) {
	t.Parallel()

	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	session := mocks.NewMockCDPSession(ctrl)
	session.EXPECT().
		Send(gomock.Any(), "Fetch.enable", gomock.Any()).
		Return(nil, errors.New("Fetch.enable: some CDP refusal"))
	// No dial expectation: a refused probe must not churn. The single
	// permitted Close is the helper's teardown — the assertion below that
	// the session is STILL INSTALLED after ensureArmed is what proves the
	// probe did not retire it.
	session.EXPECT().Close().Return(nil).MaxTimes(1)

	i := testInterceptor(t, []string{"ipfs.io"})
	i.session = session

	i.ensureArmed()

	i.mu.Lock()
	defer i.mu.Unlock()
	assert.Equal(t, session, i.session)
}

// Close must stop the supervisor and refuse late attaches, so a dial racing
// shutdown cannot install a session nobody will ever close.
func TestCloseStopsSupervisorAndRefusesAttach(t *testing.T) {
	t.Parallel()

	i := testInterceptor(t, []string{"ipfs.io"})
	i.dial = failingDial(errors.New("no kiosk in unit tests"))

	require.NoError(t, i.Close())
	assert.NoError(t, i.Close(), "Close must be idempotent")

	err := i.AttachOnReconnect(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "closed")
}

// failingDial is a dial seam that always fails — recovery paths in unit
// tests must never open a real socket.
func failingDial(err error) dialFunc {
	return func(context.Context, string, wrapper.HTTPClient, wrapper.WebSocketDialer, wrapper.JSON, wrapper.IO, *zap.Logger) (offlinecache.CDPSession, error) {
		return nil, err
	}
}
