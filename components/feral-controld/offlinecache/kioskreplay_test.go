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
		mockHTTP, mockDialer, wrapper.NewJSON(), wrapper.NewIO(), wrapper.NewClock(), zaptest.NewLogger(t))
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
	mockReplayer.EXPECT().AttachChild(gomock.Any(), "child-sess-1", gomock.Any()).
		DoAndReturn(func(offlinecache.CDPSession, string, offlinecache.CDPSession) bool {
			attached <- struct{}{}
			return true
		}).Times(1)
	detached := make(chan struct{}, 1)
	mockReplayer.EXPECT().DetachChild(gomock.Any(), "child-sess-1").
		Do(func(offlinecache.CDPSession, string) { detached <- struct{}{} }).Times(1)

	store, _ := newTestStore(t)
	kr := offlinecache.NewKioskReplay(mockReplayer, store, "http://127.0.0.1:9222",
		mockHTTP, mockDialer, wrapper.NewJSON(), wrapper.NewIO(), wrapper.NewClock(), zaptest.NewLogger(t))
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
// TestKioskReplay_AttachOnReconnect_AttachChildRejectionSkipsResume pins
// the caller-boundary half of the reconnect-race fix: when Replayer's
// AttachChild reports the root as superseded (false), handleTargetAttached
// must NOT attempt to resume the paused target either — see AttachChild's
// doc for why a rejected attach means the iframe's own top-level page is
// already gone, so there is nothing left on screen to unfreeze. This
// complements the replayer-level regression tests in replay_test.go
// (TestReplayer_AttachChild_RejectsSupersededRoot etc.), which prove the
// guard itself; this one proves kiosktargets.go actually respects a
// false return rather than resuming anyway.
func TestKioskReplay_AttachOnReconnect_AttachChildRejectionSkipsResume(t *testing.T) {
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
	rejected := make(chan struct{}, 1)
	// Simulate root having already been superseded by the time this
	// event's goroutine runs: AttachChild reports failure.
	mockReplayer.EXPECT().AttachChild(gomock.Any(), "child-sess-stale", gomock.Any()).
		DoAndReturn(func(offlinecache.CDPSession, string, offlinecache.CDPSession) bool {
			rejected <- struct{}{}
			return false
		}).Times(1)

	store, _ := newTestStore(t)
	kr := offlinecache.NewKioskReplay(mockReplayer, store, "http://127.0.0.1:9222",
		mockHTTP, mockDialer, wrapper.NewJSON(), wrapper.NewIO(), wrapper.NewClock(), zaptest.NewLogger(t))
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

	attachedEvt, err := json.Marshal(map[string]interface{}{
		"method": "Target.attachedToTarget",
		"params": map[string]interface{}{
			"sessionId":  "child-sess-stale",
			"targetInfo": map[string]interface{}{"targetId": "T1", "type": "iframe"},
		},
	})
	require.NoError(t, err)
	conn.pushReply(attachedEvt)

	select {
	case <-rejected:
	case <-time.After(2 * time.Second):
		t.Fatal("AttachChild was not called for the attached-target event")
	}

	// No Runtime.runIfWaitingForDebugger (or anything else) must be sent
	// on this session: a rejected AttachChild means the caller must skip
	// the resume entirely.
	select {
	case msg := <-conn.outbound:
		t.Fatalf("unexpected outbound message after AttachChild rejection: %v", msg)
	case <-time.After(200 * time.Millisecond):
	}
}

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
		mockHTTP, mockDialer, wrapper.NewJSON(), wrapper.NewIO(), wrapper.NewClock(), zaptest.NewLogger(t))

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
		mockHTTP, mockDialer, wrapper.NewJSON(), wrapper.NewIO(), wrapper.NewClock(), zaptest.NewLogger(t))
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
		mockHTTP, mockDialer, wrapper.NewJSON(), wrapper.NewIO(), wrapper.NewClock(), zaptest.NewLogger(t))

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
	mockReplayer.EXPECT().EnableForPlaylist(gomock.Any(), []string{sourceFor("cached-1")}, true).Return(nil).Times(1)

	kr := offlinecache.NewKioskReplay(mockReplayer, store, "http://127.0.0.1:9222",
		nil, nil, wrapper.NewJSON(), wrapper.NewIO(), wrapper.NewClock(), zaptest.NewLogger(t))

	require.NoError(t, kr.SyncPlaylist(context.Background(), []string{sourceFor("cached-1"), sourceFor("uncached-1"), ""}))
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
		EnableForPlaylist(gomock.Any(), []string{sourceFor("cached-1"), sourceFor("cached-2")}, false).
		Return(nil).Times(1)

	kr := offlinecache.NewKioskReplay(mockReplayer, store, "http://127.0.0.1:9222",
		nil, nil, wrapper.NewJSON(), wrapper.NewIO(), wrapper.NewClock(), zaptest.NewLogger(t))

	require.NoError(t, kr.SyncPlaylist(context.Background(), []string{sourceFor("cached-1"), sourceFor("cached-2"), ""}))
}

func TestKioskReplay_SyncPlaylist_NoCachedItemsDisables(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockReplayer := mocks.NewMockOfflineCacheReplayer(ctrl)
	store, _ := newTestStore(t)

	mockReplayer.EXPECT().Disable(gomock.Any()).Return(nil).Times(1)

	kr := offlinecache.NewKioskReplay(mockReplayer, store, "http://127.0.0.1:9222",
		nil, nil, wrapper.NewJSON(), wrapper.NewIO(), wrapper.NewClock(), zaptest.NewLogger(t))

	require.NoError(t, kr.SyncPlaylist(context.Background(), []string{sourceFor("uncached-1"), sourceFor("uncached-2")}))
}

func TestKioskReplay_SyncPlaylist_EmptyItemIDsDisables(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockReplayer := mocks.NewMockOfflineCacheReplayer(ctrl)
	store, _ := newTestStore(t)

	mockReplayer.EXPECT().Disable(gomock.Any()).Return(nil).Times(1)

	kr := offlinecache.NewKioskReplay(mockReplayer, store, "http://127.0.0.1:9222",
		nil, nil, wrapper.NewJSON(), wrapper.NewIO(), wrapper.NewClock(), zaptest.NewLogger(t))

	require.NoError(t, kr.SyncPlaylist(context.Background(), nil))
}

// redialHarness wires a KioskReplay whose dial always succeeds against a
// fresh fake DevTools peer, so a test can drive as many re-dials as it
// needs without restating the HTTP/websocket plumbing each time. clock is
// returned so a test can move time past redialCooldown.
type redialHarness struct {
	kr           offlinecache.KioskReplay
	mockReplayer *mocks.MockOfflineCacheReplayer
	clock        *mocks.MockClock
	dials        *int
}

func setupRedial(t *testing.T, store offlinecache.Store) *redialHarness {
	t.Helper()
	ctrl := gomock.NewController(t)
	t.Cleanup(ctrl.Finish)

	mockReplayer := mocks.NewMockOfflineCacheReplayer(ctrl)
	mockHTTP := mocks.NewMockHTTPClient(ctrl)
	mockDialer := mocks.NewMockWebSocketDialer(ctrl)
	mockClock := mocks.NewMockClock(ctrl)

	dials := 0
	mockHTTP.EXPECT().NewRequest(http.MethodGet, "http://127.0.0.1:9222/json", nil).DoAndReturn(
		func(method, url string, _ io.Reader) (*http.Request, error) {
			return http.NewRequest(method, url, nil)
		}).AnyTimes()
	mockHTTP.EXPECT().Do(gomock.Any()).DoAndReturn(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(`[{"type":"page","webSocketDebuggerUrl":"ws://127.0.0.1:9222/devtools/page/1"}]`)),
		}, nil
	}).AnyTimes()
	mockDialer.EXPECT().DialContext(gomock.Any(), "ws://127.0.0.1:9222/devtools/page/1", nil).DoAndReturn(
		func(context.Context, string, http.Header) (wrapper.WebSocketConn, *http.Response, error) {
			dials++
			conn := newFakeWSConn()
			// Auto-ack the Target.setAutoAttach handshake
			// enableChildTargetAutoAttach performs on every fresh
			// session, so a re-dial completes without the test having to
			// pump the fake peer by hand.
			go conn.drainAndAckRemaining(t)
			t.Cleanup(func() { _ = conn.Close() })
			return conn, nil, nil
		}).AnyTimes()

	kr := offlinecache.NewKioskReplay(mockReplayer, store, "http://127.0.0.1:9222",
		mockHTTP, mockDialer, wrapper.NewJSON(), wrapper.NewIO(), mockClock, zaptest.NewLogger(t))

	return &redialHarness{kr: kr, mockReplayer: mockReplayer, clock: mockClock, dials: &dials}
}

// TestKioskReplay_SyncPlaylist_RedialsWhenTheRootDiedWithPrimaryCDPHealthy
// is the regression test for replay recovering on its own. The replay
// session is a separate socket from the daemon's primary CDP connection,
// so it can die while the primary stays up — and the primary's onConnect
// hook is the only OTHER caller of AttachOnReconnect. Before this, a dead
// replay socket was never retired or re-dialed, so every later sync sent
// into the corpse and offline replay was silently lost until the kiosk
// restarted.
//
// The primary CDP connection is deliberately absent from this test: no
// reconnect event occurs, and recovery still has to happen.
func TestKioskReplay_SyncPlaylist_RedialsWhenTheRootDiedWithPrimaryCDPHealthy(t *testing.T) {
	store, _ := newTestStore(t)
	seedItem(t, store, "item-1", "software payload")
	h := setupRedial(t, store)

	h.clock.EXPECT().Now().Return(time.Now()).AnyTimes()

	// First sync: the scope call fails because the socket is dead, and
	// the replayer reports no root left (it retired the dead one).
	gomock.InOrder(
		h.mockReplayer.EXPECT().EnableForPlaylist(gomock.Any(), []string{sourceFor("item-1")}, false).
			Return(offlinecache.ErrCDPTransport).Times(1),
		h.mockReplayer.EXPECT().RootAttached().Return(false).Times(1),
		// Re-dial installs a fresh root...
		h.mockReplayer.EXPECT().Attach("", gomock.Any()).Times(1),
		// ...and the scope is re-applied to it, restoring exactly what
		// the store says should be replayable right now.
		h.mockReplayer.EXPECT().EnableForPlaylist(gomock.Any(), []string{sourceFor("item-1")}, false).
			Return(nil).Times(1),
	)

	require.NoError(t, h.kr.SyncPlaylist(context.Background(), []string{sourceFor("item-1")}),
		"the sync must recover within itself, not leave replay dead until the next kiosk restart")
	assert.Equal(t, 1, *h.dials, "exactly one re-dial")
}

// TestKioskReplay_SyncPlaylist_DoesNotRedialWhenTheRootIsStillAttached
// pins the other half of the classification: a scope call can fail while
// the connection is perfectly healthy (a target refusing Fetch.enable, a
// caller's ctx expiring). Re-dialing then would tear down a working
// session and churn the kiosk for no reason.
func TestKioskReplay_SyncPlaylist_DoesNotRedialWhenTheRootIsStillAttached(t *testing.T) {
	store, _ := newTestStore(t)
	seedItem(t, store, "item-1", "software payload")
	h := setupRedial(t, store)

	h.clock.EXPECT().Now().Return(time.Now()).AnyTimes()
	h.mockReplayer.EXPECT().EnableForPlaylist(gomock.Any(), []string{sourceFor("item-1")}, false).
		Return(assert.AnError).Times(1)
	h.mockReplayer.EXPECT().RootAttached().Return(true).Times(1)
	// No Attach expectation: gomock's strict controller fails the test if
	// a re-dial happens anyway.

	err := h.kr.SyncPlaylist(context.Background(), []string{sourceFor("item-1")})
	require.ErrorIs(t, err, assert.AnError, "the original failure must be reported, not masked by a recovery attempt")
	assert.Equal(t, 0, *h.dials)
}

// TestKioskReplay_SyncPlaylist_RedialIsRateLimited pins the bound on
// recovery. SyncPlaylist runs on every displayPlaylist and every
// refresher pass, and a dial against a down kiosk costs a real blocking
// round trip — without spacing, a kiosk that is simply gone would put
// that cost on the front of every display command.
func TestKioskReplay_SyncPlaylist_RedialIsRateLimited(t *testing.T) {
	store, _ := newTestStore(t)
	seedItem(t, store, "item-1", "software payload")
	h := setupRedial(t, store)

	base := time.Now()
	// Second sync happens a second later — well inside the cooldown.
	gomock.InOrder(
		h.clock.EXPECT().Now().Return(base).Times(1),
		h.clock.EXPECT().Now().Return(base.Add(time.Second)).Times(1),
	)

	h.mockReplayer.EXPECT().EnableForPlaylist(gomock.Any(), []string{sourceFor("item-1")}, false).
		Return(offlinecache.ErrCDPTransport).AnyTimes()
	h.mockReplayer.EXPECT().RootAttached().Return(false).AnyTimes()
	h.mockReplayer.EXPECT().Attach("", gomock.Any()).Times(1)

	// First sync re-dials; the fresh session's own enable fails too (the
	// kiosk is genuinely unwell), so the error is reported.
	require.Error(t, h.kr.SyncPlaylist(context.Background(), []string{sourceFor("item-1")}))
	assert.Equal(t, 1, *h.dials)

	// Second sync, one second later: no second dial, and the caller still
	// learns the real failure rather than a misleading dial error.
	err := h.kr.SyncPlaylist(context.Background(), []string{sourceFor("item-1")})
	require.ErrorIs(t, err, offlinecache.ErrCDPTransport)
	assert.Equal(t, 1, *h.dials, "a re-dial inside the cooldown must not be attempted")
}
