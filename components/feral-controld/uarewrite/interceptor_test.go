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
	"go.uber.org/zap/zaptest"

	"github.com/feral-file/ffos-user/components/feral-controld/mocks"
	"github.com/feral-file/ffos-user/components/feral-controld/offlinecache"
	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

func testInterceptorWithClock(t *testing.T, hosts []string, clock wrapper.Clock) *Interceptor {
	t.Helper()

	policy, err := New(hosts, "feral-player/test")
	require.NoError(t, err)

	return NewInterceptor(policy, "http://127.0.0.1:9222", nil, nil, wrapper.NewJSON(), nil, clock, zaptest.NewLogger(t))
}

func testInterceptor(t *testing.T, hosts []string) *Interceptor {
	t.Helper()

	policy, err := New(hosts, "feral-player/test")
	require.NoError(t, err)

	return NewInterceptor(policy, "http://127.0.0.1:9222", nil, nil, wrapper.NewJSON(), nil, wrapper.NewClock(), zaptest.NewLogger(t))
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
// so it can die while the primary stays healthy — and AttachOnReconnect's only
// other caller is the primary's reconnect hook, which in that case never
// fires. Without self-recovery the rewrite is retired permanently, and the
// failure is invisible: when a DevTools client drops, Chromium releases the
// requests it had paused and stops honoring its Fetch patterns, so traffic
// keeps flowing with the browser User-Agent restored. Nothing stalls, nothing
// new is logged, and the artworks this fixes silently revert to failing.
func TestClaimRedialOnTransportFailure(t *testing.T) {
	t.Parallel()

	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	clock := mocks.NewMockClock(ctrl)
	clock.EXPECT().Now().Return(time.Unix(1000, 0)).AnyTimes()

	session := mocks.NewMockCDPSession(ctrl)
	i := testInterceptorWithClock(t, []string{"ipfs.io"}, clock)
	i.session = session

	assert.True(t, i.claimRedial(session, fmt.Errorf("wrapped: %w", offlinecache.ErrCDPTransport)),
		"a transport failure on the live session must claim a re-dial")
}

// A CDP error REPLY or an expired caller context leaves the socket perfectly
// healthy. Re-dialing those would be pointless churn against a kiosk that is
// answering fine — the session package classifies at the source precisely so
// consumers do not have to infer "dead" by exclusion.
func TestClaimRedialIgnoresNonTransportErrors(t *testing.T) {
	t.Parallel()

	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	clock := mocks.NewMockClock(ctrl)
	clock.EXPECT().Now().Return(time.Unix(1000, 0)).AnyTimes()

	session := mocks.NewMockCDPSession(ctrl)
	i := testInterceptorWithClock(t, []string{"ipfs.io"}, clock)
	i.session = session

	for _, err := range []error{
		errors.New("Fetch.continueRequest: invalid requestId"),
		context.DeadlineExceeded,
	} {
		assert.False(t, i.claimRedial(session, err),
			"error %v left the socket healthy and must not trigger a re-dial", err)
	}
}

// A dead socket fails EVERY request it was holding. Without the identity
// check, one drop would enqueue a re-dial per in-flight request, and a
// reconnect that already completed would be torn down by a late arrival from
// the old session.
func TestClaimRedialIgnoresSupersededSession(t *testing.T) {
	t.Parallel()

	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	clock := mocks.NewMockClock(ctrl)
	clock.EXPECT().Now().Return(time.Unix(1000, 0)).AnyTimes()

	old := mocks.NewMockCDPSession(ctrl)
	current := mocks.NewMockCDPSession(ctrl)

	i := testInterceptorWithClock(t, []string{"ipfs.io"}, clock)
	i.session = current

	assert.False(t, i.claimRedial(old, offlinecache.ErrCDPTransport),
		"a failure from a superseded session must not disturb the live one")
}

// The trigger is per-request, so one dead socket produces a burst of identical
// failures. The cooldown is what keeps that burst from becoming a dial storm
// against a kiosk that is already sick.
func TestClaimRedialHonorsCooldown(t *testing.T) {
	t.Parallel()

	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	base := time.Unix(1000, 0)
	clock := mocks.NewMockClock(ctrl)
	gomock.InOrder(
		clock.EXPECT().Now().Return(base),
		clock.EXPECT().Now().Return(base.Add(redialCooldown-time.Second)),
		clock.EXPECT().Now().Return(base.Add(redialCooldown)),
	)

	session := mocks.NewMockCDPSession(ctrl)
	i := testInterceptorWithClock(t, []string{"ipfs.io"}, clock)
	i.session = session

	assert.True(t, i.claimRedial(session, offlinecache.ErrCDPTransport), "first failure claims")
	assert.False(t, i.claimRedial(session, offlinecache.ErrCDPTransport), "inside cooldown must not claim")
	assert.True(t, i.claimRedial(session, offlinecache.ErrCDPTransport), "past cooldown claims again")
}
