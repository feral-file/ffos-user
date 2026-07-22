package offlinecache_test

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"

	"github.com/feral-file/ffos-user/components/feral-controld/mocks"
	"github.com/feral-file/ffos-user/components/feral-controld/offlinecache"
	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

// TestDialPageSession_DialsTheSinglePageTarget confirms discovery skips
// non-page targets and dials the one page target. Non-page targets
// (other/background_page) are ignored; only the presence of exactly one
// "page" target is a valid attach.
func TestDialPageSession_DialsTheSinglePageTarget(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockHTTP := mocks.NewMockHTTPClient(ctrl)
	mockDialer := mocks.NewMockWebSocketDialer(ctrl)
	conn := newFakeWSConn()

	req, err := http.NewRequest(http.MethodGet, "http://127.0.0.1:9222/json", nil)
	require.NoError(t, err)
	mockHTTP.EXPECT().NewRequest(http.MethodGet, "http://127.0.0.1:9222/json", nil).Return(req, nil).Times(1)
	mockHTTP.EXPECT().Do(gomock.Any()).Return(&http.Response{
		StatusCode: http.StatusOK,
		Body: io.NopCloser(strings.NewReader(
			`[{"type":"other","webSocketDebuggerUrl":"ws://127.0.0.1:9222/devtools/other/1"},` +
				`{"type":"page","webSocketDebuggerUrl":"ws://127.0.0.1:9222/devtools/page/1"}]`)),
	}, nil).Times(1)
	mockDialer.EXPECT().DialContext(gomock.Any(), "ws://127.0.0.1:9222/devtools/page/1", nil).
		Return(conn, nil, nil).Times(1)

	session, err := offlinecache.DialPageSession(
		context.Background(), "http://127.0.0.1:9222",
		mockHTTP, mockDialer, wrapper.NewJSON(), wrapper.NewIO(), zaptest.NewLogger(t),
	)
	require.NoError(t, err)
	require.NotNil(t, session)
	_ = session.Close()
}

// TestDialPageSession_MultiplePageTargetsRejected pins the fix for the
// unstable-target-order hazard: when Chromium exposes more than one page
// target (a popup, an extra about:blank, a background page opened by the
// runtime), discovery must NOT pick one nondeterministically — replay
// could scope Fetch interception to the wrong kiosk page and capture could
// navigate the wrong headless page. It must error out (mirroring the cdp
// package's ErrMultiplePageTargetsFound) and never dial.
func TestDialPageSession_MultiplePageTargetsRejected(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockHTTP := mocks.NewMockHTTPClient(ctrl)
	mockDialer := mocks.NewMockWebSocketDialer(ctrl)

	req, err := http.NewRequest(http.MethodGet, "http://127.0.0.1:9222/json", nil)
	require.NoError(t, err)
	mockHTTP.EXPECT().NewRequest(http.MethodGet, "http://127.0.0.1:9222/json", nil).Return(req, nil).Times(1)
	mockHTTP.EXPECT().Do(gomock.Any()).Return(&http.Response{
		StatusCode: http.StatusOK,
		Body: io.NopCloser(strings.NewReader(
			`[{"type":"page","webSocketDebuggerUrl":"ws://127.0.0.1:9222/devtools/page/1"},` +
				`{"type":"page","webSocketDebuggerUrl":"ws://127.0.0.1:9222/devtools/page/2"}]`)),
	}, nil).Times(1)
	// DialContext must never be called: the ctrl.Finish() deferred above
	// asserts no unexpected dial happened.

	_, err = offlinecache.DialPageSession(
		context.Background(), "http://127.0.0.1:9222",
		mockHTTP, mockDialer, wrapper.NewJSON(), wrapper.NewIO(), zaptest.NewLogger(t),
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "multiple page targets")
}

func TestDialPageSession_NoPageTargetFound(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockHTTP := mocks.NewMockHTTPClient(ctrl)
	mockDialer := mocks.NewMockWebSocketDialer(ctrl)

	req, err := http.NewRequest(http.MethodGet, "http://127.0.0.1:9222/json", nil)
	require.NoError(t, err)
	mockHTTP.EXPECT().NewRequest(http.MethodGet, "http://127.0.0.1:9222/json", nil).Return(req, nil).Times(1)
	mockHTTP.EXPECT().Do(gomock.Any()).Return(&http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(`[{"type":"background_page","webSocketDebuggerUrl":"ws://x"}]`)),
	}, nil).Times(1)

	_, err = offlinecache.DialPageSession(
		context.Background(), "http://127.0.0.1:9222",
		mockHTTP, mockDialer, wrapper.NewJSON(), wrapper.NewIO(), zaptest.NewLogger(t),
	)
	assert.Error(t, err)
}

func TestDialPageSession_TargetsRequestFails(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockHTTP := mocks.NewMockHTTPClient(ctrl)
	mockDialer := mocks.NewMockWebSocketDialer(ctrl)

	req, err := http.NewRequest(http.MethodGet, "http://127.0.0.1:9222/json", nil)
	require.NoError(t, err)
	mockHTTP.EXPECT().NewRequest(http.MethodGet, "http://127.0.0.1:9222/json", nil).Return(req, nil).Times(1)
	mockHTTP.EXPECT().Do(gomock.Any()).Return(nil, assertError("connection refused")).Times(1)

	_, err = offlinecache.DialPageSession(
		context.Background(), "http://127.0.0.1:9222",
		mockHTTP, mockDialer, wrapper.NewJSON(), wrapper.NewIO(), zaptest.NewLogger(t),
	)
	assert.Error(t, err)
}

func TestDialPageSession_DialFails(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockHTTP := mocks.NewMockHTTPClient(ctrl)
	mockDialer := mocks.NewMockWebSocketDialer(ctrl)

	req, err := http.NewRequest(http.MethodGet, "http://127.0.0.1:9222/json", nil)
	require.NoError(t, err)
	mockHTTP.EXPECT().NewRequest(http.MethodGet, "http://127.0.0.1:9222/json", nil).Return(req, nil).Times(1)
	mockHTTP.EXPECT().Do(gomock.Any()).Return(&http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(`[{"type":"page","webSocketDebuggerUrl":"ws://127.0.0.1:9222/devtools/page/1"}]`)),
	}, nil).Times(1)
	mockDialer.EXPECT().DialContext(gomock.Any(), "ws://127.0.0.1:9222/devtools/page/1", nil).
		Return(nil, nil, assertError("dial refused")).Times(1)

	_, err = offlinecache.DialPageSession(
		context.Background(), "http://127.0.0.1:9222",
		mockHTTP, mockDialer, wrapper.NewJSON(), wrapper.NewIO(), zaptest.NewLogger(t),
	)
	assert.Error(t, err)
}
