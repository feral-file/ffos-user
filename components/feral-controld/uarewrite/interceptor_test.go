package uarewrite

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"

	"github.com/feral-file/ffos-user/components/feral-controld/mocks"
	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

func testInterceptor(t *testing.T, hosts []string) *Interceptor {
	t.Helper()

	policy, err := New(hosts, "feral-player/test")
	require.NoError(t, err)

	return NewInterceptor(policy, "http://127.0.0.1:9222", nil, nil, wrapper.NewJSON(), nil, zaptest.NewLogger(t))
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
