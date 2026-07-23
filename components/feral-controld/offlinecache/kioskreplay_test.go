package offlinecache_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
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

// ackOutbound reads the next outbound CDP call from conn and replies with
// an empty result keyed to its id, mirroring how a real DevTools peer
// answers a command. Returns the parsed outbound message so a caller can
// assert on its method/params/sessionId.
func ackOutbound(t *testing.T, conn *fakeWSConn) map[string]interface{} {
	t.Helper()
	msg := conn.nextOutbound(t)
	reply, err := json.Marshal(map[string]interface{}{
		"id":     int64(msg["id"].(float64)),
		"result": map[string]interface{}{},
	})
	require.NoError(t, err)
	conn.pushReply(reply)
	return msg
}

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
	// The top-level target is attached with the empty sessionID.
	mockReplayer.EXPECT().Attach("", gomock.Any()).Times(1)

	store, _ := newTestStore(t)
	kr := offlinecache.NewKioskReplay(mockReplayer, store, "http://127.0.0.1:9222",
		mockHTTP, mockDialer, wrapper.NewJSON(), wrapper.NewIO(), zaptest.NewLogger(t))
	defer func() { _ = conn.Close() }()

	// AttachOnReconnect now also enables flat-mode child-target
	// auto-attach, which issues a Target.setAutoAttach command and waits
	// for its reply; run it in a goroutine so the fake peer can ack.
	errCh := make(chan error, 1)
	go func() { errCh <- kr.AttachOnReconnect(context.Background()) }()

	setAutoAttach := ackOutbound(t, conn)
	assert.Equal(t, "Target.setAutoAttach", setAutoAttach["method"])
	params, ok := setAutoAttach["params"].(map[string]interface{})
	require.True(t, ok)
	// waitForDebuggerOnStart is the load-bearing flag: it pauses a new
	// child target before its first request so interception can be armed
	// first (see kiosktargets.go).
	assert.Equal(t, true, params["waitForDebuggerOnStart"])
	assert.Equal(t, true, params["flatten"])
	assert.Equal(t, true, params["autoAttach"])

	select {
	case err := <-errCh:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("AttachOnReconnect did not complete after Target.setAutoAttach was acked")
	}
}

// TestKioskReplay_AttachOnReconnect_AttachesAndDetachesChildTargets drives
// the full OOPIF lifecycle end to end: once auto-attach is enabled, a
// Target.attachedToTarget event (a cross-origin iframe appearing) must
// attach replay to that child's session, resume the paused target, and a
// later Target.detachedFromTarget must detach it.
func TestKioskReplay_AttachOnReconnect_AttachesAndDetachesChildTargets(t *testing.T) {
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

	mockReplayer.EXPECT().Attach("", gomock.Any()).Times(1)
	attached := make(chan struct{}, 1)
	mockReplayer.EXPECT().Attach("child-sess-1", gomock.Any()).
		Do(func(string, offlinecache.CDPSession) { attached <- struct{}{} }).Times(1)
	detached := make(chan struct{}, 1)
	mockReplayer.EXPECT().Detach("child-sess-1").
		Do(func(string) { detached <- struct{}{} }).Times(1)

	store, _ := newTestStore(t)
	kr := offlinecache.NewKioskReplay(mockReplayer, store, "http://127.0.0.1:9222",
		mockHTTP, mockDialer, wrapper.NewJSON(), wrapper.NewIO(), zaptest.NewLogger(t))
	defer func() { _ = conn.Close() }()

	errCh := make(chan error, 1)
	go func() { errCh <- kr.AttachOnReconnect(context.Background()) }()
	ackOutbound(t, conn) // Target.setAutoAttach
	select {
	case err := <-errCh:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("AttachOnReconnect did not complete")
	}

	// A cross-origin iframe attaches. The handler runs on its own
	// goroutine (off the read pump), so synchronize on the Attach Do
	// signal before asserting the resume call.
	attachedEvt, err := json.Marshal(map[string]interface{}{
		"method": "Target.attachedToTarget",
		"params": map[string]interface{}{
			"sessionId":  "child-sess-1",
			"targetInfo": map[string]interface{}{"targetId": "T1", "type": "iframe"},
		},
	})
	require.NoError(t, err)
	conn.pushReply(attachedEvt)

	select {
	case <-attached:
	case <-time.After(2 * time.Second):
		t.Fatal("child target was not attached to replay")
	}

	// The paused child must be resumed, and the resume must be scoped to
	// the child's own sessionId so it reaches that target, not the page.
	resume := ackOutbound(t, conn)
	assert.Equal(t, "Runtime.runIfWaitingForDebugger", resume["method"])
	assert.Equal(t, "child-sess-1", resume["sessionId"])

	// The iframe navigates away / is removed.
	detachedEvt, err := json.Marshal(map[string]interface{}{
		"method": "Target.detachedFromTarget",
		"params": map[string]interface{}{"sessionId": "child-sess-1"},
	})
	require.NoError(t, err)
	conn.pushReply(detachedEvt)

	select {
	case <-detached:
	case <-time.After(2 * time.Second):
		t.Fatal("child target was not detached from replay")
	}
}

// TestKioskReplay_AttachOnReconnect_AutoAttachSetupFailureIsReported pins
// that a failing Target.setAutoAttach surfaces as an error from
// AttachOnReconnect (its doc frames this as non-fatal to top-level
// interception — the caller logs it and still re-syncs scope — but the
// error must still propagate so it is visible, not swallowed). The
// top-level target is still attached first.
func TestKioskReplay_AttachOnReconnect_AutoAttachSetupFailureIsReported(t *testing.T) {
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
	mockReplayer.EXPECT().Attach("", gomock.Any()).Times(1)

	store, _ := newTestStore(t)
	kr := offlinecache.NewKioskReplay(mockReplayer, store, "http://127.0.0.1:9222",
		mockHTTP, mockDialer, wrapper.NewJSON(), wrapper.NewIO(), zaptest.NewLogger(t))

	errCh := make(chan error, 1)
	go func() { errCh <- kr.AttachOnReconnect(context.Background()) }()

	// Reply to Target.setAutoAttach with a CDP error instead of a result.
	msg := conn.nextOutbound(t)
	assert.Equal(t, "Target.setAutoAttach", msg["method"])
	reply, err := json.Marshal(map[string]interface{}{
		"id":    int64(msg["id"].(float64)),
		"error": map[string]interface{}{"code": -32601, "message": "not supported"},
	})
	require.NoError(t, err)
	conn.pushReply(reply)

	select {
	case err := <-errCh:
		require.Error(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("AttachOnReconnect did not return after Target.setAutoAttach failed")
	}
	_ = conn.Close()
}

// TestKioskReplay_AttachOnReconnect_MalformedChildAttachEventIsInert pins
// that a Target.attachedToTarget event without a sessionId is ignored: no
// child Attach, no resume, no panic — the warn-and-return path stays
// inert.
func TestKioskReplay_AttachOnReconnect_MalformedChildAttachEventIsInert(t *testing.T) {
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
	// Only the top-level Attach("") is ever expected: no child Attach for a
	// sessionId-less event. gomock's strict controller fails if any
	// Attach("<nonempty>", ...) or Detach is called.
	mockReplayer.EXPECT().Attach("", gomock.Any()).Times(1)

	store, _ := newTestStore(t)
	kr := offlinecache.NewKioskReplay(mockReplayer, store, "http://127.0.0.1:9222",
		mockHTTP, mockDialer, wrapper.NewJSON(), wrapper.NewIO(), zaptest.NewLogger(t))
	defer func() { _ = conn.Close() }()

	errCh := make(chan error, 1)
	go func() { errCh <- kr.AttachOnReconnect(context.Background()) }()
	ackOutbound(t, conn) // Target.setAutoAttach
	select {
	case err := <-errCh:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("AttachOnReconnect did not complete")
	}

	// A child-attach event with no sessionId must be ignored entirely.
	malformed, err := json.Marshal(map[string]interface{}{
		"method": "Target.attachedToTarget",
		"params": map[string]interface{}{
			"targetInfo": map[string]interface{}{"targetId": "T1", "type": "iframe"},
		},
	})
	require.NoError(t, err)
	conn.pushReply(malformed)

	// Give the dispatch goroutine a chance to (not) act. No resume Send
	// should ever appear on the wire; drain briefly to prove inertness.
	select {
	case <-conn.outbound:
		t.Fatal("a sessionId-less attach event must not trigger any CDP Send")
	case <-time.After(200 * time.Millisecond):
	}
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

func TestKioskReplay_SyncPlaylist_EnablesOnlyCachedItemsAsMixedScope(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockReplayer := mocks.NewMockOfflineCacheReplayer(ctrl)
	store, _ := newTestStore(t)
	seedItem(t, store, "cached-1", "software payload")

	// "uncached-1" is a real item in the displayed playlist that has no
	// capture on disk: SyncPlaylist must flag this scope as mixed so
	// Replayer relaxes fail_closed to pass-through for it, or its
	// requests would be wrongly failed while cached-1 is also in scope.
	mockReplayer.EXPECT().EnableForPlaylist(gomock.Any(), []string{"cached-1"}, true).Return(nil).Times(1)

	kr := offlinecache.NewKioskReplay(mockReplayer, store, "http://127.0.0.1:9222",
		nil, nil, wrapper.NewJSON(), wrapper.NewIO(), zaptest.NewLogger(t))

	require.NoError(t, kr.SyncPlaylist(context.Background(), []string{"cached-1", "uncached-1", ""}))
}

func TestKioskReplay_SyncPlaylist_AllItemsCachedIsNotMixedScope(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockReplayer := mocks.NewMockOfflineCacheReplayer(ctrl)
	store, _ := newTestStore(t)
	seedItem(t, store, "cached-1", "software payload one")
	seedItem(t, store, "cached-2", "software payload two")

	// Every real item in the playlist is cached (the "" entry is not a
	// real item and must not count against that), so the fail_closed
	// guarantee should still hold: mixed must be false.
	mockReplayer.EXPECT().
		EnableForPlaylist(gomock.Any(), []string{"cached-1", "cached-2"}, false).
		Return(nil).Times(1)

	kr := offlinecache.NewKioskReplay(mockReplayer, store, "http://127.0.0.1:9222",
		nil, nil, wrapper.NewJSON(), wrapper.NewIO(), zaptest.NewLogger(t))

	require.NoError(t, kr.SyncPlaylist(context.Background(), []string{"cached-1", "cached-2", ""}))
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
