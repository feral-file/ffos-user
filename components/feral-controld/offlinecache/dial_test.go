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

func TestDialPageSession_DialsFirstPageTarget(t *testing.T) {
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
