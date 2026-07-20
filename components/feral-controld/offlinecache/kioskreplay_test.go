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

func TestKioskReplay_AttachOnReconnect_DialsAndAttaches(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockReplayer := mocks.NewMockOfflineCacheReplayer(ctrl)
	mockHTTP := mocks.NewMockHTTPClient(ctrl)
	mockDialer := mocks.NewMockWebSocketDialer(ctrl)
	conn := newFakeWSConn()

	req, err := http.NewRequest(http.MethodGet, "http://127.0.0.1:9222/json", nil)
	require.NoError(t, err)
	mockHTTP.EXPECT().NewRequest(http.MethodGet, "http://127.0.0.1:9222/json", nil).Return(req, nil).Times(1)
	mockHTTP.EXPECT().Do(gomock.Any()).Return(&http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(`[{"type":"page","webSocketDebuggerUrl":"ws://127.0.0.1:9222/devtools/page/1"}]`)),
	}, nil).Times(1)
	mockDialer.EXPECT().DialContext(gomock.Any(), "ws://127.0.0.1:9222/devtools/page/1", nil).
		Return(conn, nil, nil).Times(1)
	mockReplayer.EXPECT().Attach(gomock.Any()).Times(1)

	store, _ := newTestStore(t)
	kr := offlinecache.NewKioskReplay(mockReplayer, store, "http://127.0.0.1:9222",
		mockHTTP, mockDialer, wrapper.NewJSON(), wrapper.NewIO(), zaptest.NewLogger(t))

	require.NoError(t, kr.AttachOnReconnect(context.Background()))
}

func TestKioskReplay_AttachOnReconnect_DialFailure(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockReplayer := mocks.NewMockOfflineCacheReplayer(ctrl)
	mockHTTP := mocks.NewMockHTTPClient(ctrl)
	mockDialer := mocks.NewMockWebSocketDialer(ctrl)

	req, err := http.NewRequest(http.MethodGet, "http://127.0.0.1:9222/json", nil)
	require.NoError(t, err)
	mockHTTP.EXPECT().NewRequest(http.MethodGet, "http://127.0.0.1:9222/json", nil).Return(req, nil).Times(1)
	mockHTTP.EXPECT().Do(gomock.Any()).Return(nil, assertError("connection refused")).Times(1)
	// Attach must never be called when the dial itself failed.

	store, _ := newTestStore(t)
	kr := offlinecache.NewKioskReplay(mockReplayer, store, "http://127.0.0.1:9222",
		mockHTTP, mockDialer, wrapper.NewJSON(), wrapper.NewIO(), zaptest.NewLogger(t))

	assert.Error(t, kr.AttachOnReconnect(context.Background()))
}

func TestKioskReplay_SyncPlaylist_EnablesOnlyCachedItems(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockReplayer := mocks.NewMockOfflineCacheReplayer(ctrl)
	store, _ := newTestStore(t)
	seedItem(t, store, "cached-1", "software payload")

	mockReplayer.EXPECT().EnableForPlaylist(gomock.Any(), []string{"cached-1"}).Return(nil).Times(1)

	kr := offlinecache.NewKioskReplay(mockReplayer, store, "http://127.0.0.1:9222",
		nil, nil, wrapper.NewJSON(), wrapper.NewIO(), zaptest.NewLogger(t))

	require.NoError(t, kr.SyncPlaylist(context.Background(), []string{"cached-1", "uncached-1", ""}))
}

func TestKioskReplay_SyncPlaylist_NoCachedItemsDisables(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockReplayer := mocks.NewMockOfflineCacheReplayer(ctrl)
	store, _ := newTestStore(t)

	mockReplayer.EXPECT().Disable(gomock.Any()).Return(nil).Times(1)

	kr := offlinecache.NewKioskReplay(mockReplayer, store, "http://127.0.0.1:9222",
		nil, nil, wrapper.NewJSON(), wrapper.NewIO(), zaptest.NewLogger(t))

	require.NoError(t, kr.SyncPlaylist(context.Background(), []string{"uncached-1", "uncached-2"}))
}

func TestKioskReplay_SyncPlaylist_EmptyItemIDsDisables(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockReplayer := mocks.NewMockOfflineCacheReplayer(ctrl)
	store, _ := newTestStore(t)

	mockReplayer.EXPECT().Disable(gomock.Any()).Return(nil).Times(1)

	kr := offlinecache.NewKioskReplay(mockReplayer, store, "http://127.0.0.1:9222",
		nil, nil, wrapper.NewJSON(), wrapper.NewIO(), zaptest.NewLogger(t))

	require.NoError(t, kr.SyncPlaylist(context.Background(), nil))
}
