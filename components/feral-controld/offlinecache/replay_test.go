package offlinecache_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"

	dp1playlist "github.com/display-protocol/dp1-go/playlist"
	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"

	"github.com/feral-file/ffos-user/components/feral-controld/mocks"
	"github.com/feral-file/ffos-user/components/feral-controld/offlinecache"
	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

const testStaticBaseURL = "http://127.0.0.1:8082"

type replayTestSetup struct {
	ctrl        *gomock.Controller
	store       offlinecache.Store
	mockStatic  *mocks.MockOfflineCacheStaticServer
	mockSession *mocks.MockCDPSession
	replayer    offlinecache.Replayer
	handler     func(json.RawMessage)
}

// setupReplay wires a Replayer against a real fsStore (so blob/item
// round-trips are genuine) plus mocked CDP session and static server, and
// captures the Fetch.requestPaused handler that Attach registers so tests
// can drive it directly, mirroring how the real CDPSession read pump would.
func setupReplay(t *testing.T, missPolicy offlinecache.MissPolicy) *replayTestSetup {
	t.Helper()
	ctrl := gomock.NewController(t)
	store, _ := newTestStore(t)
	mockStatic := mocks.NewMockOfflineCacheStaticServer(ctrl)
	mockStatic.EXPECT().BaseURL().Return(testStaticBaseURL).AnyTimes()
	mockSession := mocks.NewMockCDPSession(ctrl)

	rp := offlinecache.NewReplayer(store, mockStatic, missPolicy, wrapper.NewJSON(), zaptest.NewLogger(t))

	var handler func(json.RawMessage)
	mockSession.EXPECT().
		On("Fetch.requestPaused", gomock.Any()).
		Do(func(_ string, h func(json.RawMessage)) { handler = h }).
		Times(1)
	rp.Attach(mockSession)
	require.NotNil(t, handler)

	return &replayTestSetup{
		ctrl: ctrl, store: store, mockStatic: mockStatic, mockSession: mockSession,
		replayer: rp, handler: handler,
	}
}

// expectSend arms a one-shot expectation for method and returns a channel
// that receives the call's params. processRequestPaused always responds
// via a spawned goroutine (see replay.go's onRequestPaused doc), so tests
// must synchronize on this channel rather than asserting immediately after
// invoking the captured handler.
func (ts *replayTestSetup) expectSend(method string) chan map[string]interface{} {
	done := make(chan map[string]interface{}, 1)
	ts.mockSession.EXPECT().
		Send(gomock.Any(), method, gomock.Any()).
		DoAndReturn(func(_ context.Context, _ string, params map[string]interface{}) (json.RawMessage, error) {
			done <- params
			return json.RawMessage(`{}`), nil
		}).Times(1)
	return done
}

func (ts *replayTestSetup) stubFetchEnable() {
	ts.mockSession.EXPECT().Send(gomock.Any(), "Fetch.enable", gomock.Any()).Return(json.RawMessage(`{}`), nil).Times(1)
}

func awaitSend(t *testing.T, done chan map[string]interface{}) map[string]interface{} {
	t.Helper()
	select {
	case params := <-done:
		return params
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for CDP Send call")
		return nil
	}
}

func requestPausedEvent(t *testing.T, requestID, url string) json.RawMessage {
	t.Helper()
	data, err := json.Marshal(map[string]interface{}{
		"requestId": requestID,
		"request":   map[string]interface{}{"url": url},
	})
	require.NoError(t, err)
	return data
}

func headerValue(t *testing.T, params map[string]interface{}, name string) (string, bool) {
	t.Helper()
	raw, ok := params["responseHeaders"]
	if !ok || raw == nil {
		return "", false
	}
	headers, ok := raw.([]map[string]interface{})
	require.True(t, ok, "responseHeaders must be []map[string]interface{}")
	for _, h := range headers {
		if h["name"] == name {
			v, _ := h["value"].(string)
			return v, true
		}
	}
	return "", false
}

func TestReplayer_EnableForItem_LoadsItemAndEnablesFetch(t *testing.T) {
	ts := setupReplay(t, offlinecache.MissPolicyFailClosed)
	defer ts.ctrl.Finish()
	seedItem(t, ts.store, "item-1", "software payload")
	ts.stubFetchEnable()

	require.NoError(t, ts.replayer.EnableForItem(context.Background(), "item-1"))
}

func TestReplayer_EnableForItem_ItemNotFound(t *testing.T) {
	ts := setupReplay(t, offlinecache.MissPolicyFailClosed)
	defer ts.ctrl.Finish()

	err := ts.replayer.EnableForItem(context.Background(), "missing-item")
	assert.ErrorIs(t, err, offlinecache.ErrItemNotFound)
}

func TestReplayer_Attach_ClosesPreviouslyAttachedSession(t *testing.T) {
	ts := setupReplay(t, offlinecache.MissPolicyFailClosed)
	defer ts.ctrl.Finish()

	// setupReplay already attached ts.mockSession once. Re-attaching (the
	// shape of a kiosk reconnect via KioskReplay.AttachOnReconnect) must
	// close that superseded session rather than leaking its read pump.
	newSession := mocks.NewMockCDPSession(ts.ctrl)
	newSession.EXPECT().On("Fetch.requestPaused", gomock.Any()).Times(1)
	ts.mockSession.EXPECT().Close().Return(nil).Times(1)

	ts.replayer.Attach(newSession)
}

func TestReplayer_Attach_FirstCallDoesNotCloseAnything(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	store, _ := newTestStore(t)
	mockStatic := mocks.NewMockOfflineCacheStaticServer(ctrl)
	mockSession := mocks.NewMockCDPSession(ctrl)

	rp := offlinecache.NewReplayer(store, mockStatic, offlinecache.MissPolicyFailClosed, wrapper.NewJSON(), zaptest.NewLogger(t))
	// mockSession.Close is deliberately not expected: the first Attach has
	// no prior session to supersede.
	mockSession.EXPECT().On("Fetch.requestPaused", gomock.Any()).Times(1)
	rp.Attach(mockSession)
}

func TestReplayer_EnableForItem_NoSessionAttached(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	store, _ := newTestStore(t)
	seedItem(t, store, "item-1", "software payload")
	mockStatic := mocks.NewMockOfflineCacheStaticServer(ctrl)

	rp := offlinecache.NewReplayer(store, mockStatic, offlinecache.MissPolicyFailClosed, wrapper.NewJSON(), zaptest.NewLogger(t))
	err := rp.EnableForItem(context.Background(), "item-1")
	assert.Error(t, err)
}

func TestReplayer_EnableForPlaylist_UnionsMultipleItemsResources(t *testing.T) {
	ts := setupReplay(t, offlinecache.MissPolicyFailClosed)
	defer ts.ctrl.Finish()
	res1 := seedItem(t, ts.store, "item-1", "software payload one")
	res2 := seedItem(t, ts.store, "item-2", "software payload two")
	ts.stubFetchEnable()

	require.NoError(t, ts.replayer.EnableForPlaylist(context.Background(), []string{"item-1", "item-2"}, false))

	done1 := ts.expectSend("Fetch.fulfillRequest")
	ts.handler(requestPausedEvent(t, "req-1", res1.URL))
	awaitSend(t, done1)

	done2 := ts.expectSend("Fetch.fulfillRequest")
	ts.handler(requestPausedEvent(t, "req-2", res2.URL))
	awaitSend(t, done2)
}

func TestReplayer_EnableForPlaylist_UnknownItemErrorsAndLeavesScopeUnchanged(t *testing.T) {
	ts := setupReplay(t, offlinecache.MissPolicyFailClosed)
	defer ts.ctrl.Finish()
	seedItem(t, ts.store, "item-1", "software payload")

	err := ts.replayer.EnableForPlaylist(context.Background(), []string{"item-1", "missing-item"}, false)
	assert.ErrorIs(t, err, offlinecache.ErrItemNotFound)
}

func TestReplayer_EnableForPlaylist_EmptyListIsValidNoScope(t *testing.T) {
	ts := setupReplay(t, offlinecache.MissPolicyFailClosed)
	defer ts.ctrl.Finish()
	ts.stubFetchEnable()

	require.NoError(t, ts.replayer.EnableForPlaylist(context.Background(), nil, false))

	// With no items in scope, even a URL that would otherwise resolve
	// (there is none seeded here) must fail closed.
	done := ts.expectSend("Fetch.failRequest")
	ts.handler(requestPausedEvent(t, "req-1", "https://example.com/anything.js"))
	awaitSend(t, done)
}

func TestReplayer_EnableForPlaylist_MixedScopeMissPassesThroughEvenUnderFailClosed(t *testing.T) {
	ts := setupReplay(t, offlinecache.MissPolicyFailClosed)
	defer ts.ctrl.Finish()
	res := seedItem(t, ts.store, "item-1", "software payload")
	ts.stubFetchEnable()

	// mixed=true models KioskReplay.SyncPlaylist enabling only the cached
	// items of a playlist that also contains an uncached sibling: a miss
	// (the sibling's own request) must pass through to the live network
	// even though the configured policy is fail_closed, or the sibling
	// item could never play. A cached hit must still be served normally.
	require.NoError(t, ts.replayer.EnableForPlaylist(context.Background(), []string{"item-1"}, true))

	missDone := ts.expectSend("Fetch.continueRequest")
	ts.handler(requestPausedEvent(t, "req-miss", "https://example.com/uncached-sibling.js"))
	awaitSend(t, missDone)

	hitDone := ts.expectSend("Fetch.fulfillRequest")
	ts.handler(requestPausedEvent(t, "req-hit", res.URL))
	awaitSend(t, hitDone)
}

func TestReplayer_EnableForPlaylist_NonMixedScopeMissStillFailsClosed(t *testing.T) {
	ts := setupReplay(t, offlinecache.MissPolicyFailClosed)
	defer ts.ctrl.Finish()
	seedItem(t, ts.store, "item-1", "software payload")
	ts.stubFetchEnable()

	// mixed=false (every item in the displayed playlist is cached) must
	// keep the strict fail_closed guarantee: nothing here should ever
	// legitimately need the live network.
	require.NoError(t, ts.replayer.EnableForPlaylist(context.Background(), []string{"item-1"}, false))

	done := ts.expectSend("Fetch.failRequest")
	ts.handler(requestPausedEvent(t, "req-1", "https://example.com/unrelated.js"))
	awaitSend(t, done)
}

func TestReplayer_Disable_DisablesFetchAndClearsScope(t *testing.T) {
	ts := setupReplay(t, offlinecache.MissPolicyFailClosed)
	defer ts.ctrl.Finish()
	res := seedItem(t, ts.store, "item-1", "software payload")
	ts.stubFetchEnable()
	require.NoError(t, ts.replayer.EnableForItem(context.Background(), "item-1"))

	ts.mockSession.EXPECT().Send(gomock.Any(), "Fetch.disable", gomock.Any()).Return(json.RawMessage(`{}`), nil).Times(1)
	require.NoError(t, ts.replayer.Disable(context.Background()))

	// The scope is now cleared, so a previously-cached URL must fall
	// through to the miss policy instead of being fulfilled.
	done := ts.expectSend("Fetch.failRequest")
	ts.handler(requestPausedEvent(t, "req-1", res.URL))
	awaitSend(t, done)
}

// TestReplayer_Disable_FetchDisableFailureForcesPassThroughInsteadOfFailClosed
// is the regression test pinning that Disable must not clear scope to a
// fail-closed state BEFORE confirming Fetch.disable actually succeeded on
// Chromium's side. If Fetch.disable's CDP call
// itself fails, this must not turn every subsequent request into a
// Fetch.failRequest (which would break normal online playback outright);
// it must instead force pass-through on miss, same as a mixed scope does.
func TestReplayer_Disable_FetchDisableFailureForcesPassThroughInsteadOfFailClosed(t *testing.T) {
	ts := setupReplay(t, offlinecache.MissPolicyFailClosed)
	defer ts.ctrl.Finish()
	res := seedItem(t, ts.store, "item-1", "software payload")
	ts.stubFetchEnable()
	require.NoError(t, ts.replayer.EnableForItem(context.Background(), "item-1"))

	ts.mockSession.EXPECT().Send(gomock.Any(), "Fetch.disable", gomock.Any()).
		Return(nil, assert.AnError).Times(1)
	err := ts.replayer.Disable(context.Background())
	assert.ErrorIs(t, err, assert.AnError)

	// A previously-cached URL is still served: Disable's failure did not
	// wipe the previous scope, only relaxed its miss policy.
	hitDone := ts.expectSend("Fetch.fulfillRequest")
	ts.handler(requestPausedEvent(t, "req-hit", res.URL))
	awaitSend(t, hitDone)

	// An unrelated URL — exactly what every request would look like on
	// whatever screen is now on-screen after a failed Disable — passes
	// through to the live network instead of being fail_closed.
	missDone := ts.expectSend("Fetch.continueRequest")
	ts.handler(requestPausedEvent(t, "req-miss", "https://example.com/unrelated.js"))
	awaitSend(t, missDone)
}

func TestReplayer_Disable_NoSessionAttached_Noop(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	store, _ := newTestStore(t)
	mockStatic := mocks.NewMockOfflineCacheStaticServer(ctrl)

	rp := offlinecache.NewReplayer(store, mockStatic, offlinecache.MissPolicyFailClosed, wrapper.NewJSON(), zaptest.NewLogger(t))
	assert.NoError(t, rp.Disable(context.Background()))
}

func TestReplayer_ProcessRequestPaused_FulfillsSmallBlob(t *testing.T) {
	ts := setupReplay(t, offlinecache.MissPolicyFailClosed)
	defer ts.ctrl.Finish()
	res := seedItem(t, ts.store, "item-1", "<html>art</html>")
	ts.stubFetchEnable()
	require.NoError(t, ts.replayer.EnableForItem(context.Background(), "item-1"))

	done := ts.expectSend("Fetch.fulfillRequest")
	ts.handler(requestPausedEvent(t, "req-1", res.URL))
	params := awaitSend(t, done)

	assert.EqualValues(t, 200, params["responseCode"])
	body, ok := params["body"].(string)
	require.True(t, ok)
	decoded, err := base64.StdEncoding.DecodeString(body)
	require.NoError(t, err)
	assert.Equal(t, "<html>art</html>", string(decoded))
	ct, ok := headerValue(t, params, "Content-Type")
	require.True(t, ok)
	assert.Equal(t, "text/html", ct)
}

func TestReplayer_ProcessRequestPaused_PartialContentBlobNormalizedTo200(t *testing.T) {
	ts := setupReplay(t, offlinecache.MissPolicyFailClosed)
	defer ts.ctrl.Finish()

	// Capture observed this resource as 206 (the page's own request used
	// a Range header), but capture.go's fetchAndStoreBody always fetches
	// and stores the complete body regardless. Replaying the stored 206
	// verbatim with the full body and no Content-Range would be spec-
	// non-compliant; this resource is well under largeAssetThreshold so
	// it takes the inline-fulfill path, not the static-server redirect.
	hash := writeBlobString(t, ts.store, "audio-bytes")
	rec := &offlinecache.ItemRecord{
		ItemID: "item-206",
		Resources: []offlinecache.Resource{
			{URL: "https://example.com/track.mp3", Status: 206, SHA256: hash, ContentType: "audio/mpeg"},
		},
		Coverage: offlinecache.Coverage{Complete: true},
	}
	require.NoError(t, ts.store.SaveItem(rec))
	ts.stubFetchEnable()
	require.NoError(t, ts.replayer.EnableForItem(context.Background(), "item-206"))

	done := ts.expectSend("Fetch.fulfillRequest")
	ts.handler(requestPausedEvent(t, "req-1", "https://example.com/track.mp3"))
	params := awaitSend(t, done)

	assert.EqualValues(t, 200, params["responseCode"])
	body, ok := params["body"].(string)
	require.True(t, ok)
	decoded, err := base64.StdEncoding.DecodeString(body)
	require.NoError(t, err)
	assert.Equal(t, "audio-bytes", string(decoded))
}

func TestReplayer_ProcessRequestPaused_RedirectResource(t *testing.T) {
	ts := setupReplay(t, offlinecache.MissPolicyFailClosed)
	defer ts.ctrl.Finish()

	finalHash := writeBlobString(t, ts.store, "console.log(1)")
	rec := &offlinecache.ItemRecord{
		ItemID: "item-redirect",
		Item:   dp1playlist.PlaylistItem{ID: "item-redirect", Source: "https://example.com/lib.min.js"},
		Entry:  "https://example.com/lib.min.js",
		Resources: []offlinecache.Resource{
			{URL: "https://example.com/lib.min.js", Status: 302, RedirectTo: "https://example.com/lib@2/lib.min.js"},
			{URL: "https://example.com/lib@2/lib.min.js", Status: 200, SHA256: finalHash, ContentType: "application/javascript"},
		},
		Coverage: offlinecache.Coverage{Complete: true},
	}
	require.NoError(t, ts.store.SaveItem(rec))

	ts.stubFetchEnable()
	require.NoError(t, ts.replayer.EnableForItem(context.Background(), "item-redirect"))

	done := ts.expectSend("Fetch.fulfillRequest")
	ts.handler(requestPausedEvent(t, "req-1", "https://example.com/lib.min.js"))
	params := awaitSend(t, done)

	assert.EqualValues(t, 302, params["responseCode"])
	assert.Nil(t, params["body"], "a redirect hop has no body")
	location, ok := headerValue(t, params, "Location")
	require.True(t, ok)
	assert.Equal(t, "https://example.com/lib@2/lib.min.js", location)
}

func TestReplayer_ProcessRequestPaused_LargeAssetRedirectsToStatic(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockStore := mocks.NewMockOfflineCacheStore(ctrl)
	mockStatic := mocks.NewMockOfflineCacheStaticServer(ctrl)
	mockStatic.EXPECT().BaseURL().Return(testStaticBaseURL).AnyTimes()
	mockSession := mocks.NewMockCDPSession(ctrl)

	rp := offlinecache.NewReplayer(mockStore, mockStatic, offlinecache.MissPolicyFailClosed, wrapper.NewJSON(), zaptest.NewLogger(t))

	var handler func(json.RawMessage)
	mockSession.EXPECT().On("Fetch.requestPaused", gomock.Any()).Do(func(_ string, h func(json.RawMessage)) { handler = h }).Times(1)
	rp.Attach(mockSession)

	rec := &offlinecache.ItemRecord{
		ItemID: "item-large",
		Resources: []offlinecache.Resource{
			{URL: "https://example.com/movie.mp4", Status: 200, SHA256: "deadbeef", ContentType: "video/mp4"},
		},
	}
	mockStore.EXPECT().LoadItem("item-large").Return(rec, nil).Times(1)
	mockSession.EXPECT().Send(gomock.Any(), "Fetch.enable", gomock.Any()).Return(json.RawMessage(`{}`), nil).Times(1)
	require.NoError(t, rp.EnableForItem(context.Background(), "item-large"))

	mockStore.EXPECT().BlobSize("deadbeef").Return(int64(300*1024*1024), nil).Times(1)
	mockStatic.EXPECT().URLFor("deadbeef", "video/mp4").Return(testStaticBaseURL + "/blobs/deadbeef?ct=video%2Fmp4").Times(1)

	done := make(chan map[string]interface{}, 1)
	mockSession.EXPECT().Send(gomock.Any(), "Fetch.fulfillRequest", gomock.Any()).
		DoAndReturn(func(_ context.Context, _ string, params map[string]interface{}) (json.RawMessage, error) {
			done <- params
			return json.RawMessage(`{}`), nil
		}).Times(1)

	handler(requestPausedEvent(t, "req-1", "https://example.com/movie.mp4"))
	params := awaitSend(t, done)

	assert.EqualValues(t, 302, params["responseCode"])
	assert.Nil(t, params["body"])
	location, ok := headerValue(t, params, "Location")
	require.True(t, ok)
	assert.Equal(t, testStaticBaseURL+"/blobs/deadbeef?ct=video%2Fmp4", location)
}

func TestReplayer_ProcessRequestPaused_BlobMissingTreatedAsMiss(t *testing.T) {
	ts := setupReplay(t, offlinecache.MissPolicyFailClosed)
	defer ts.ctrl.Finish()

	rec := &offlinecache.ItemRecord{
		ItemID: "item-1",
		Resources: []offlinecache.Resource{
			{URL: "https://example.com/gone.js", Status: 200, SHA256: "not-a-real-hash", ContentType: "application/javascript"},
		},
	}
	require.NoError(t, ts.store.SaveItem(rec))
	ts.stubFetchEnable()
	require.NoError(t, ts.replayer.EnableForItem(context.Background(), "item-1"))

	done := ts.expectSend("Fetch.failRequest")
	ts.handler(requestPausedEvent(t, "req-1", "https://example.com/gone.js"))
	params := awaitSend(t, done)
	assert.Equal(t, "req-1", params["requestId"])
}

func TestReplayer_ProcessRequestPaused_MissFailClosed(t *testing.T) {
	ts := setupReplay(t, offlinecache.MissPolicyFailClosed)
	defer ts.ctrl.Finish()
	seedItem(t, ts.store, "item-1", "software payload")
	ts.stubFetchEnable()
	require.NoError(t, ts.replayer.EnableForItem(context.Background(), "item-1"))

	done := ts.expectSend("Fetch.failRequest")
	ts.handler(requestPausedEvent(t, "req-1", "https://example.com/unrelated.js"))
	params := awaitSend(t, done)
	assert.Equal(t, "Failed", params["errorReason"])
}

func TestReplayer_ProcessRequestPaused_MissPassThrough(t *testing.T) {
	ts := setupReplay(t, offlinecache.MissPolicyPassThrough)
	defer ts.ctrl.Finish()
	seedItem(t, ts.store, "item-1", "software payload")
	ts.stubFetchEnable()
	require.NoError(t, ts.replayer.EnableForItem(context.Background(), "item-1"))

	done := ts.expectSend("Fetch.continueRequest")
	ts.handler(requestPausedEvent(t, "req-1", "https://example.com/unrelated.js"))
	params := awaitSend(t, done)
	assert.Equal(t, "req-1", params["requestId"])
}

func TestReplayer_ProcessRequestPaused_StaticServerURLAlwaysPassesThrough(t *testing.T) {
	ts := setupReplay(t, offlinecache.MissPolicyFailClosed)
	defer ts.ctrl.Finish()
	// No EnableForItem call at all: even with no scope active, requests to
	// replay's own static-asset fallback must never be fail-closed, or the
	// large-asset 302 it issued would immediately dead-end.
	done := ts.expectSend("Fetch.continueRequest")
	ts.handler(requestPausedEvent(t, "req-1", testStaticBaseURL+"/blobs/deadbeef?ct=video%2Fmp4"))
	awaitSend(t, done)
}
