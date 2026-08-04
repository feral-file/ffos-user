package offlinecache_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	dp1playlist "github.com/display-protocol/dp1-go/playlist"
	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"

	constants "github.com/feral-file/ffos-user/components/feral-controld/constant"
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

	rp := offlinecache.NewReplayer(store, mockStatic, missPolicy, constants.WEBAPP_URL, wrapper.NewJSON(), zaptest.NewLogger(t))

	var handler func(json.RawMessage)
	mockSession.EXPECT().
		On("Fetch.requestPaused", gomock.Any()).
		Do(func(_ string, h func(json.RawMessage)) { handler = h }).
		Times(1)
	rp.Attach("", mockSession)
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
	return requestPausedEventWithMethod(t, requestID, url, "")
}

// requestPausedEventWithMethod is requestPausedEvent with an explicit
// method, for tests that need to pin method-sensitive matching (see
// Resource.Method's doc). An empty method (requestPausedEvent's case)
// omits the field entirely, matching what a plain GET fetch/CDP event
// looks like on the wire.
func requestPausedEventWithMethod(t *testing.T, requestID, url, method string) json.RawMessage {
	t.Helper()
	request := map[string]interface{}{"url": url}
	if method != "" {
		request["method"] = method
	}
	data, err := json.Marshal(map[string]interface{}{
		"requestId": requestID,
		"request":   request,
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

	require.NoError(t, ts.replayer.EnableForItem(context.Background(), sourceFor("item-1")))
}

func TestReplayer_EnableForItem_ItemNotFound(t *testing.T) {
	ts := setupReplay(t, offlinecache.MissPolicyFailClosed)
	defer ts.ctrl.Finish()

	err := ts.replayer.EnableForItem(context.Background(), sourceFor("missing-item"))
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

	ts.replayer.Attach("", newSession)
}

// TestReplayer_ProcessRequestPaused_DelayedEventFromSupersededSessionIsDropped
// is the regression test for the "stale request ID on a new CDP session"
// finding: an earlier version of processRequestPaused re-read r.session
// instead of using the session its handler closure was bound to, so a
// Fetch.requestPaused event already dispatched from an OLD session's
// read pump — but not yet processed — would answer on whatever session
// a concurrent Attach had since swapped in, sending
// Fetch.fulfillRequest/continueRequest/failRequest with a requestId that
// only ever existed on the OLD connection. Neither mock session has a
// Send expectation registered below: the fix drops the event outright
// once its bound session is no longer current (see processRequestPaused's
// doc), so NEITHER session should ever see a Send call as a result of
// firing oldHandler after the reconnect. A regression that resurrects
// the "answer on r.session" behavior would call newSession.Send(...)
// here, which gomock fails as an unexpected call the instant it happens.
func TestReplayer_ProcessRequestPaused_DelayedEventFromSupersededSessionIsDropped(t *testing.T) {
	ts := setupReplay(t, offlinecache.MissPolicyFailClosed)
	defer ts.ctrl.Finish()
	res := seedItem(t, ts.store, "item-1", "software payload")
	ts.stubFetchEnable()
	require.NoError(t, ts.replayer.EnableForItem(context.Background(), sourceFor("item-1")))

	// oldHandler is bound (via Attach's closure) to ts.mockSession — the
	// OLD session, about to be superseded below.
	oldHandler := ts.handler

	newSession := mocks.NewMockCDPSession(ts.ctrl)
	newSession.EXPECT().On("Fetch.requestPaused", gomock.Any()).Times(1)
	ts.mockSession.EXPECT().Close().Return(nil).Times(1)
	ts.replayer.Attach("", newSession)

	// Fires the OLD session's bound handler directly, mirroring a real
	// CDPSession read pump delivering an event that was already queued
	// before Attach ran. processRequestPaused runs this on its own
	// goroutine (see onRequestPaused's doc); the sleep below gives that
	// goroutine a chance to run — and, if the bug regressed, to hit
	// gomock's unexpected-call failure — before ctrl.Finish() below.
	oldHandler(requestPausedEvent(t, "req-1", res.URL))
	time.Sleep(100 * time.Millisecond)
}

// TestReplayer_EnableForPlaylist_SerializesAgainstConcurrentAttach is the
// regression test for the second half of the "stale CDP session" finding:
// EnableForPlaylist/EnableForItem used to read r.session, release the
// lock, and only THEN call Fetch.enable on the captured value — leaving a
// window where a concurrent Attach (kiosk reconnect) could swap AND close
// that exact session before the Send ran. The Send would then fail
// against an already-dying connection while r.resources/r.mixedScope
// (committed moments earlier) still claimed a scope was active, silently
// leaving the NEW session's Fetch domain never enabled. transitionMu (see
// its doc) closes this by making Attach block until EnableForItem's own
// in-flight Fetch.enable completes: this test proves that ordering
// directly rather than only checking the eventual outcome.
func TestReplayer_EnableForPlaylist_SerializesAgainstConcurrentAttach(t *testing.T) {
	ts := setupReplay(t, offlinecache.MissPolicyFailClosed)
	defer ts.ctrl.Finish()
	seedItem(t, ts.store, "item-1", "software payload")

	sendStarted := make(chan struct{})
	releaseSend := make(chan struct{})
	ts.mockSession.EXPECT().Send(gomock.Any(), "Fetch.enable", gomock.Any()).
		DoAndReturn(func(context.Context, string, map[string]interface{}) (json.RawMessage, error) {
			close(sendStarted)
			<-releaseSend
			return json.RawMessage(`{}`), nil
		}).Times(1)

	enableDone := make(chan error, 1)
	go func() {
		enableDone <- ts.replayer.EnableForItem(context.Background(), sourceFor("item-1"))
	}()

	select {
	case <-sendStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("EnableForItem's Fetch.enable Send never started")
	}

	newSession := mocks.NewMockCDPSession(ts.ctrl)
	attachDone := make(chan struct{})
	go func() {
		newSession.EXPECT().On("Fetch.requestPaused", gomock.Any()).Times(1)
		ts.mockSession.EXPECT().Close().Return(nil).Times(1)
		ts.replayer.Attach("", newSession)
		close(attachDone)
	}()

	// Attach must NOT proceed while EnableForItem's Fetch.enable Send is
	// still blocked above: if it did, it would be free to swap/close
	// ts.mockSession out from under that still-in-flight Send.
	select {
	case <-attachDone:
		t.Fatal("Attach must not proceed while EnableForItem's Fetch.enable is still in flight")
	case <-time.After(150 * time.Millisecond):
	}

	close(releaseSend)
	select {
	case <-attachDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Attach did not proceed once Fetch.enable completed")
	}
	require.NoError(t, <-enableDone)
}

func TestReplayer_Attach_FirstCallDoesNotCloseAnything(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	store, _ := newTestStore(t)
	mockStatic := mocks.NewMockOfflineCacheStaticServer(ctrl)
	mockSession := mocks.NewMockCDPSession(ctrl)

	rp := offlinecache.NewReplayer(store, mockStatic, offlinecache.MissPolicyFailClosed, constants.WEBAPP_URL, wrapper.NewJSON(), zaptest.NewLogger(t))
	// mockSession.Close is deliberately not expected: the first Attach has
	// no prior session to supersede.
	mockSession.EXPECT().On("Fetch.requestPaused", gomock.Any()).Times(1)
	rp.Attach("", mockSession)
}

func TestReplayer_EnableForItem_NoSessionAttached(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	store, _ := newTestStore(t)
	seedItem(t, store, "item-1", "software payload")
	mockStatic := mocks.NewMockOfflineCacheStaticServer(ctrl)

	rp := offlinecache.NewReplayer(store, mockStatic, offlinecache.MissPolicyFailClosed, constants.WEBAPP_URL, wrapper.NewJSON(), zaptest.NewLogger(t))
	err := rp.EnableForItem(context.Background(), sourceFor("item-1"))
	assert.Error(t, err)
}

func TestReplayer_EnableForPlaylist_UnionsMultipleItemsResources(t *testing.T) {
	ts := setupReplay(t, offlinecache.MissPolicyFailClosed)
	defer ts.ctrl.Finish()
	res1 := seedItem(t, ts.store, "item-1", "software payload one")
	res2 := seedItem(t, ts.store, "item-2", "software payload two")
	ts.stubFetchEnable()

	require.NoError(t, ts.replayer.EnableForPlaylist(context.Background(), []string{sourceFor("item-1"), sourceFor("item-2")}, false))

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

	err := ts.replayer.EnableForPlaylist(context.Background(), []string{sourceFor("item-1"), sourceFor("missing-item")}, false)
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
	require.NoError(t, ts.replayer.EnableForPlaylist(context.Background(), []string{sourceFor("item-1")}, true))

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
	require.NoError(t, ts.replayer.EnableForPlaylist(context.Background(), []string{sourceFor("item-1")}, false))

	done := ts.expectSend("Fetch.failRequest")
	ts.handler(requestPausedEvent(t, "req-1", "https://example.com/unrelated.js"))
	awaitSend(t, done)
}

// TestReplayer_KioskShellRequestsPassThroughUnderFailClosed pins the
// recovery-navigation exemption: the ff-player shell (constant.WEBAPP_URL)
// is never in the captured resource map, so without an explicit
// pass-through a fail-closed non-mixed scope would Fetch.failRequest the
// daemon's own Page.navigate back to the shell (boot recovery,
// refreshArtwork escalation, watchdog navigate), wedging the kiosk on
// Chromium's error page until a browser restart. See
// processRequestPaused's kiosk-shell comment.
func TestReplayer_KioskShellRequestsPassThroughUnderFailClosed(t *testing.T) {
	tests := []struct {
		name string
		url  string
		want string
	}{
		{
			name: "shell document navigation",
			url:  constants.WEBAPP_URL,
			want: "Fetch.continueRequest",
		},
		{
			name: "shell bundle asset",
			url:  constants.WEBAPP_URL + "assets/index-abc123.js",
			want: "Fetch.continueRequest",
		},
		//nolint:gosec // G101 reads the userinfo below as a credential; it is
		// the attack fixture this case exists to reject, not a secret.
		{
			// Origin equality must be on parsed components: a URL whose
			// userinfo merely LOOKS like the shell host must not escape
			// offline isolation (same hazard isStaticServerFollowUp
			// documents).
			name: "userinfo lookalike still fails closed",
			url:  "http://127.0.0.1:8080@evil.example/",
			want: "Fetch.failRequest",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts := setupReplay(t, offlinecache.MissPolicyFailClosed)
			defer ts.ctrl.Finish()
			seedItem(t, ts.store, "item-1", "software payload")
			ts.stubFetchEnable()

			// mixed=false: the strictest scope, where every non-exempt
			// miss fails closed.
			require.NoError(t, ts.replayer.EnableForPlaylist(context.Background(), []string{sourceFor("item-1")}, false))

			done := ts.expectSend(tt.want)
			ts.handler(requestPausedEvent(t, "req-1", tt.url))
			awaitSend(t, done)
		})
	}
}

// TestReplayer_ProcessRequestPaused_MethodMismatchIsTreatedAsMiss pins
// that a resource captured for one method (GET) is never fulfilled for a
// paused request using a DIFFERENT method (POST) to the identical URL —
// see Resource.Method's doc for why method is part of a resource's
// identity, not just a descriptive field.
func TestReplayer_ProcessRequestPaused_MethodMismatchIsTreatedAsMiss(t *testing.T) {
	ts := setupReplay(t, offlinecache.MissPolicyFailClosed)
	defer ts.ctrl.Finish()
	res := seedItem(t, ts.store, "item-1", "software payload") // seeded with an implicit GET Resource
	ts.stubFetchEnable()
	require.NoError(t, ts.replayer.EnableForItem(context.Background(), sourceFor("item-1")))

	// A GET to the exact same URL still hits, proving the cached entry is
	// reachable at all.
	getDone := ts.expectSend("Fetch.fulfillRequest")
	ts.handler(requestPausedEventWithMethod(t, "req-get", res.URL, "GET"))
	awaitSend(t, getDone)

	// A POST to the same URL must miss (fail closed here), never be
	// fulfilled from the GET's cached bytes.
	postDone := ts.expectSend("Fetch.failRequest")
	ts.handler(requestPausedEventWithMethod(t, "req-post", res.URL, "POST"))
	awaitSend(t, postDone)
}

// TestReplayer_ProcessRequestPaused_DistinctMethodsToSameURLServeIndependently
// pins that two Resources for the identical URL but different methods
// (as capture.go now records for a CORS preflight/actual-request pair)
// each replay from their own entry rather than one being lost to a map
// collision.
func TestReplayer_ProcessRequestPaused_DistinctMethodsToSameURLServeIndependently(t *testing.T) {
	ts := setupReplay(t, offlinecache.MissPolicyFailClosed)
	defer ts.ctrl.Finish()

	url := "https://api.example.com/data"
	getHash := writeBlobString(t, ts.store, "GET body")
	optionsHash := writeBlobString(t, ts.store, "")
	require.NoError(t, ts.store.SaveItem(&offlinecache.ItemRecord{
		Item:  dp1playlist.PlaylistItem{ID: "item-multi-method", Source: url},
		Entry: url,
		Resources: []offlinecache.Resource{
			{URL: url, Status: 200, SHA256: getHash, ContentType: "application/json"},
			{URL: url, Status: 204, SHA256: optionsHash, Method: "OPTIONS"},
		},
		Coverage: offlinecache.Coverage{Complete: true},
	}))
	ts.stubFetchEnable()
	require.NoError(t, ts.replayer.EnableForItem(context.Background(), url))

	getDone := ts.expectSend("Fetch.fulfillRequest")
	ts.handler(requestPausedEventWithMethod(t, "req-get", url, "GET"))
	getParams := awaitSend(t, getDone)
	assert.Equal(t, base64.StdEncoding.EncodeToString([]byte("GET body")), getParams["body"])

	optionsDone := ts.expectSend("Fetch.fulfillRequest")
	ts.handler(requestPausedEventWithMethod(t, "req-options", url, "OPTIONS"))
	optionsParams := awaitSend(t, optionsDone)
	assert.EqualValues(t, 204, optionsParams["responseCode"])
}

// TestReplayer_ProcessRequestPaused_StripsPlayerAppendedDisplayModeParam
// is the regression test for a field-reproduced bug: ff-player's
// ArtworkPlayer.tsx unconditionally appends "&display_mode=fit|crop" to
// a software/iframe artwork's URL before navigating to it, but
// capture.go always navigates to (and therefore stores resources keyed
// on) the bare item.Source. Without the stripPlayerAppendedParams retry
// in processRequestPaused, EVERY iframe-type item's own top-level
// document request misses the exact-URL lookup unconditionally, and
// under fail_closed (the effective policy once a whole displayed
// playlist is cached — mixed=false) that failed the navigation itself
// with net::ERR_FAILED, breaking offline playback for every software
// artwork. See docs/offline-artwork-capture.md §6.
func TestReplayer_ProcessRequestPaused_StripsPlayerAppendedDisplayModeParam(t *testing.T) {
	ts := setupReplay(t, offlinecache.MissPolicyFailClosed)
	defer ts.ctrl.Finish()
	res := seedItem(t, ts.store, "item-1", "software payload") // res.URL has no query string
	ts.stubFetchEnable()
	require.NoError(t, ts.replayer.EnableForItem(context.Background(), sourceFor("item-1")))

	liveURL := res.URL + "?display_mode=fit"
	done := ts.expectSend("Fetch.fulfillRequest")
	ts.handler(requestPausedEvent(t, "req-1", liveURL))
	params := awaitSend(t, done)
	assert.Equal(t, base64.StdEncoding.EncodeToString([]byte("software payload")), params["body"])
}

// TestReplayer_ProcessRequestPaused_StripsDisplayModeAfterEmptyLeadingPair
// is the regression test for the second field reproduction of the same
// bug, found on FF1-191TYKPB with a fully cached Art Blocks item
// ("Trossets #65"): ff-player appended its hint with
// `url.search += "&display_mode=fit"`, and on a source with NO query
// string that emits "...147000065?&display_mode=fit" — an empty leading
// pair. Stripping display_mode alone left "...147000065?", whose trailing
// "?" does not match the captured key "...147000065", so the item missed
// and fail-closed to Chromium's error page despite reporting ready/100%.
// It hit every query-less source; the CDN previews that carry
// "?edition_number=..." masked it because their "&" is legitimate there.
// ff-player now emits the correct separator, but the daemon must survive
// the old bundle too — the two ship on independent update cadences.
func TestReplayer_ProcessRequestPaused_StripsDisplayModeAfterEmptyLeadingPair(t *testing.T) {
	ts := setupReplay(t, offlinecache.MissPolicyFailClosed)
	defer ts.ctrl.Finish()
	res := seedItem(t, ts.store, "item-1", "software payload") // res.URL has no query string
	ts.stubFetchEnable()
	require.NoError(t, ts.replayer.EnableForItem(context.Background(), sourceFor("item-1")))

	liveURL := res.URL + "?&display_mode=fit"
	done := ts.expectSend("Fetch.fulfillRequest")
	ts.handler(requestPausedEvent(t, "req-1", liveURL))
	params := awaitSend(t, done)
	assert.Equal(t, base64.StdEncoding.EncodeToString([]byte("software payload")), params["body"])
}

// TestReplayer_ProcessRequestPaused_EmptyPairAloneStillMisses pins the
// narrowness of the empty-pair drop above: it is a normalization applied
// only while already rewriting a URL to strip an allowlisted param, never
// a standalone "?" tolerance. A request carrying an unrelated param must
// still miss, so the empty-pair handling can never widen what matches a
// cached resource.
func TestReplayer_ProcessRequestPaused_EmptyPairAloneStillMisses(t *testing.T) {
	ts := setupReplay(t, offlinecache.MissPolicyFailClosed)
	defer ts.ctrl.Finish()
	res := seedItem(t, ts.store, "item-1", "software payload")
	ts.stubFetchEnable()
	require.NoError(t, ts.replayer.EnableForItem(context.Background(), sourceFor("item-1")))

	liveURL := res.URL + "?&some_other_param=1"
	done := ts.expectSend("Fetch.failRequest")
	ts.handler(requestPausedEvent(t, "req-1", liveURL))
	awaitSend(t, done)
}

// TestReplayer_ProcessRequestPaused_StripsDisplayModeWithoutReorderingOtherParams
// pins that stripping display_mode never routes through a
// parse-then-net/url.Values.Encode() round trip: Values.Encode() sorts
// keys alphabetically, which would turn a captured
// "?edition_number=0&blockchain=bitmark" into
// "?blockchain=bitmark&edition_number=0" on lookup and reintroduce
// exactly the mismatch this fix closes, just for a different reason.
func TestReplayer_ProcessRequestPaused_StripsDisplayModeWithoutReorderingOtherParams(t *testing.T) {
	ts := setupReplay(t, offlinecache.MissPolicyFailClosed)
	defer ts.ctrl.Finish()

	capturedURL := "https://cdn.example.com/previews/abc/?edition_number=0&blockchain=bitmark"
	hash := writeBlobString(t, ts.store, "software payload")
	require.NoError(t, ts.store.SaveItem(&offlinecache.ItemRecord{
		Item:      dp1playlist.PlaylistItem{ID: "item-1", Source: capturedURL},
		Entry:     capturedURL,
		Resources: []offlinecache.Resource{{URL: capturedURL, Status: 200, SHA256: hash, ContentType: "text/html"}},
		Coverage:  offlinecache.Coverage{Complete: true},
	}))
	ts.stubFetchEnable()
	require.NoError(t, ts.replayer.EnableForItem(context.Background(), capturedURL))

	// Exactly what ArtworkPlayer.tsx produces: the ORIGINAL query order
	// preserved, with "&display_mode=crop" appended at the end.
	liveURL := capturedURL + "&display_mode=crop"
	done := ts.expectSend("Fetch.fulfillRequest")
	ts.handler(requestPausedEvent(t, "req-1", liveURL))
	params := awaitSend(t, done)
	assert.Equal(t, base64.StdEncoding.EncodeToString([]byte("software payload")), params["body"])
}

// TestReplayer_ProcessRequestPaused_UnknownExtraParamStillMisses pins
// that the display_mode allowlist is genuinely narrow: an unrelated
// extra query param a request happens to carry must still miss (and
// fail closed here), never be silently matched to a cached resource
// whose URL lacks it. A blanket "ignore any extra query param"
// normalization would risk serving the wrong bytes for an artwork whose
// own logic legitimately varies content by query key; this test would
// catch that regression.
func TestReplayer_ProcessRequestPaused_UnknownExtraParamStillMisses(t *testing.T) {
	ts := setupReplay(t, offlinecache.MissPolicyFailClosed)
	defer ts.ctrl.Finish()
	res := seedItem(t, ts.store, "item-1", "software payload")
	ts.stubFetchEnable()
	require.NoError(t, ts.replayer.EnableForItem(context.Background(), sourceFor("item-1")))

	liveURL := res.URL + "?some_other_param=1"
	done := ts.expectSend("Fetch.failRequest")
	ts.handler(requestPausedEvent(t, "req-1", liveURL))
	awaitSend(t, done)
}

// seedMediaItem saves a single-resource ItemRecord for a direct-download
// (ClassMedia/ClassUnknown) item — mediacapture.go's shape: one GET
// Resource keyed on the bare source URL, with an explicit ContentType
// and an (optional) CORS header a native <video crossOrigin="anonymous">
// element would check.
func seedMediaItem(t *testing.T, store offlinecache.Store, itemID, sourceURL, contentType, blobContent string, headers map[string]string) offlinecache.Resource {
	t.Helper()
	hash := writeBlobString(t, store, blobContent)
	res := offlinecache.Resource{URL: sourceURL, Status: 200, SHA256: hash, ContentType: contentType, Headers: headers}
	require.NoError(t, store.SaveItem(&offlinecache.ItemRecord{
		Item:      dp1playlist.PlaylistItem{ID: itemID, Source: sourceURL},
		Entry:     sourceURL,
		Resources: []offlinecache.Resource{res},
		Coverage:  offlinecache.Coverage{Complete: true},
	}))
	return res
}

// TestReplayer_ProcessRequestPaused_AnswersHEADContentTypeProbeFromGETResource
// is the regression test for ff-player's own pre-render content-type
// probe (getContentTypeFromURL): before a native <img>/<video>/<audio>
// element ever issues the GET it actually renders from, the player
// issues a HEAD to the bare source with cache-busting query params
// appended (see headProbeQueryParams' doc). capture.go/mediacapture.go
// only ever store a GET resource, so without this HEAD-from-GET
// fallback that probe misses unconditionally under fail_closed and the
// player degrades to its iframe fallback renderer instead of the
// correct native element — see docs/offline-artwork-capture.md §6.
func TestReplayer_ProcessRequestPaused_AnswersHEADContentTypeProbeFromGETResource(t *testing.T) {
	ts := setupReplay(t, offlinecache.MissPolicyFailClosed)
	defer ts.ctrl.Finish()
	res := seedMediaItem(t, ts.store, "item-media", "https://example.com/video.mp4", "video/mp4",
		"fake video bytes", map[string]string{"Access-Control-Allow-Origin": "*"})
	ts.stubFetchEnable()
	require.NoError(t, ts.replayer.EnableForItem(context.Background(), "https://example.com/video.mp4"))

	probeURL := res.URL + "?v=1234567890&x-request=xhr"
	done := ts.expectSend("Fetch.fulfillRequest")
	ts.handler(requestPausedEventWithMethod(t, "req-1", probeURL, "HEAD"))
	params := awaitSend(t, done)

	assert.EqualValues(t, 200, params["responseCode"])
	_, hasBody := params["body"]
	assert.False(t, hasBody, "a HEAD response must never carry a body")
	ct, ok := headerValue(t, params, "Content-Type")
	require.True(t, ok)
	assert.Equal(t, "video/mp4", ct)
	cors, ok := headerValue(t, params, "Access-Control-Allow-Origin")
	require.True(t, ok, "the GET resource's allowlisted CORS headers must also be replayed on the HEAD answer")
	assert.Equal(t, "*", cors)
}

// TestReplayer_ProcessRequestPaused_HEADProbeStripDoesNotReorderOtherParams
// is headProbeQueryParams' analog to
// TestReplayer_ProcessRequestPaused_StripsDisplayModeWithoutReorderingOtherParams:
// stripping v/x-request must never go through a parse-then-Encode()
// round trip that would reorder (and thus mismatch) a captured URL's
// other query parameters.
func TestReplayer_ProcessRequestPaused_HEADProbeStripDoesNotReorderOtherParams(t *testing.T) {
	ts := setupReplay(t, offlinecache.MissPolicyFailClosed)
	defer ts.ctrl.Finish()
	capturedURL := "https://cdn.example.com/art/video.mp4?edition_number=0&blockchain=bitmark"
	seedMediaItem(t, ts.store, "item-media", capturedURL, "video/mp4", "fake video bytes", nil)
	ts.stubFetchEnable()
	require.NoError(t, ts.replayer.EnableForItem(context.Background(), capturedURL))

	// Exactly what ff-player's getContentTypeFromURL produces: the
	// ORIGINAL query preserved, with "&v=...&x-request=xhr" appended.
	probeURL := capturedURL + "&v=1699999999999&x-request=xhr"
	done := ts.expectSend("Fetch.fulfillRequest")
	ts.handler(requestPausedEventWithMethod(t, "req-1", probeURL, "HEAD"))
	params := awaitSend(t, done)
	assert.EqualValues(t, 200, params["responseCode"])
}

// TestReplayer_ProcessRequestPaused_HEADMissesWhenNoGETResourceCaptured
// pins that the HEAD fallback only ever substitutes method for the
// SAME URL (see resources' doc): a HEAD probe for a URL that was never
// captured at all (no GET resource either) must still fail closed like
// any other genuine miss, not be silently waved through.
func TestReplayer_ProcessRequestPaused_HEADMissesWhenNoGETResourceCaptured(t *testing.T) {
	ts := setupReplay(t, offlinecache.MissPolicyFailClosed)
	defer ts.ctrl.Finish()
	seedMediaItem(t, ts.store, "item-media", "https://example.com/video.mp4", "video/mp4", "fake video bytes", nil)
	ts.stubFetchEnable()
	require.NoError(t, ts.replayer.EnableForItem(context.Background(), "https://example.com/video.mp4"))

	done := ts.expectSend("Fetch.failRequest")
	probeURL := "https://example.com/never-captured.mp4?v=123&x-request=xhr"
	ts.handler(requestPausedEventWithMethod(t, "req-1", probeURL, "HEAD"))
	awaitSend(t, done)
}

// TestReplayer_ProcessRequestPaused_HEADFollowsGETResourceRedirect pins
// that a HEAD probe to a URL whose GET resource is itself a redirect
// hop observes that SAME redirect (matching real HTTP semantics for
// HEAD), rather than the fallback special-casing redirects away.
func TestReplayer_ProcessRequestPaused_HEADFollowsGETResourceRedirect(t *testing.T) {
	ts := setupReplay(t, offlinecache.MissPolicyFailClosed)
	defer ts.ctrl.Finish()
	require.NoError(t, ts.store.SaveItem(&offlinecache.ItemRecord{
		Item:  dp1playlist.PlaylistItem{ID: "item-redirect", Source: "https://example.com/video.mp4"},
		Entry: "https://example.com/video.mp4",
		Resources: []offlinecache.Resource{
			{URL: "https://example.com/video.mp4", Status: 302, RedirectTo: "https://cdn.example.com/video-v2.mp4"},
		},
		Coverage: offlinecache.Coverage{Complete: true},
	}))
	ts.stubFetchEnable()
	require.NoError(t, ts.replayer.EnableForItem(context.Background(), "https://example.com/video.mp4"))

	done := ts.expectSend("Fetch.fulfillRequest")
	probeURL := "https://example.com/video.mp4?v=123&x-request=xhr"
	ts.handler(requestPausedEventWithMethod(t, "req-1", probeURL, "HEAD"))
	params := awaitSend(t, done)
	assert.EqualValues(t, 302, params["responseCode"])
	assert.Nil(t, params["body"], "a redirect hop has no body")
	loc, ok := headerValue(t, params, "Location")
	require.True(t, ok)
	assert.Equal(t, "https://cdn.example.com/video-v2.mp4", loc)
}

// TestReplayer_ProcessRequestPaused_HEADMissesWhenGETResourceHasNoBody
// pins the SHA256=="" case: a GET resource that was captured but whose
// own fetch failed (an honest miss with no bytes to serve — see
// Resource's doc) must not be answered as if it were a valid probe hit
// either.
func TestReplayer_ProcessRequestPaused_HEADMissesWhenGETResourceHasNoBody(t *testing.T) {
	ts := setupReplay(t, offlinecache.MissPolicyFailClosed)
	defer ts.ctrl.Finish()
	require.NoError(t, ts.store.SaveItem(&offlinecache.ItemRecord{
		Item:  dp1playlist.PlaylistItem{ID: "item-broken", Source: "https://example.com/video.mp4"},
		Entry: "https://example.com/video.mp4",
		Resources: []offlinecache.Resource{
			{URL: "https://example.com/video.mp4", Status: 0}, // no SHA256: fetch failed at capture time
		},
		Coverage: offlinecache.Coverage{Complete: false, Reason: "fetch_failed:https://example.com/video.mp4"},
	}))
	ts.stubFetchEnable()
	require.NoError(t, ts.replayer.EnableForItem(context.Background(), "https://example.com/video.mp4"))

	done := ts.expectSend("Fetch.failRequest")
	probeURL := "https://example.com/video.mp4?v=123&x-request=xhr"
	ts.handler(requestPausedEventWithMethod(t, "req-1", probeURL, "HEAD"))
	awaitSend(t, done)
}

func TestReplayer_Disable_DisablesFetchAndClearsScope(t *testing.T) {
	ts := setupReplay(t, offlinecache.MissPolicyFailClosed)
	defer ts.ctrl.Finish()
	res := seedItem(t, ts.store, "item-1", "software payload")
	ts.stubFetchEnable()
	require.NoError(t, ts.replayer.EnableForItem(context.Background(), sourceFor("item-1")))

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
	require.NoError(t, ts.replayer.EnableForItem(context.Background(), sourceFor("item-1")))

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

	rp := offlinecache.NewReplayer(store, mockStatic, offlinecache.MissPolicyFailClosed, constants.WEBAPP_URL, wrapper.NewJSON(), zaptest.NewLogger(t))
	assert.NoError(t, rp.Disable(context.Background()))
}

func TestReplayer_ProcessRequestPaused_FulfillsSmallBlob(t *testing.T) {
	ts := setupReplay(t, offlinecache.MissPolicyFailClosed)
	defer ts.ctrl.Finish()
	res := seedItem(t, ts.store, "item-1", "<html>art</html>")
	ts.stubFetchEnable()
	require.NoError(t, ts.replayer.EnableForItem(context.Background(), sourceFor("item-1")))

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

// TestReplayer_OnRequestPaused_AdmissionBoundResolvesOverflowViaMissPolicy
// is the regression test for the unbounded-goroutine-per-event fan-out
// flagged in the PR #229 review: a request-heavy artwork with
// Fetch.enable patterned on "*" could previously spawn an unbounded
// number of processRequestPaused goroutines, each a candidate to hold a
// full cached resource's bytes plus a pending CDP send.
//
// It fills every one of RequestPausedAdmission's semaphore slots with a
// genuine cache HIT for the same cached URL, deliberately stuck
// mid-flight (Fetch.fulfillRequest blocks on release below, simulating
// a slow write to a busy kiosk socket), then fires one more concurrent
// event for that SAME URL. If admission were not bounded — or if
// overflow were queued behind the busy slots instead of resolved
// immediately — this last event would also eventually call
// Fetch.fulfillRequest once release unblocks. Instead it must resolve
// right away via Fetch.failRequest (fail_closed's miss response),
// proving both that admission is bounded and that overflow is answered
// without waiting on it.
func TestReplayer_OnRequestPaused_AdmissionBoundResolvesOverflowViaMissPolicy(t *testing.T) {
	ts := setupReplay(t, offlinecache.MissPolicyFailClosed)
	defer ts.ctrl.Finish()
	res := seedItem(t, ts.store, "item-hot", "hot payload")
	ts.stubFetchEnable()
	require.NoError(t, ts.replayer.EnableForItem(context.Background(), sourceFor("item-hot")))

	release := make(chan struct{})
	var admitted sync.WaitGroup
	admitted.Add(offlinecache.RequestPausedAdmission)
	ts.mockSession.EXPECT().
		Send(gomock.Any(), "Fetch.fulfillRequest", gomock.Any()).
		DoAndReturn(func(context.Context, string, map[string]interface{}) (json.RawMessage, error) {
			defer admitted.Done()
			<-release
			return json.RawMessage(`{}`), nil
		}).
		Times(offlinecache.RequestPausedAdmission)
	overflowDone := ts.expectSend("Fetch.failRequest")

	// onRequestPaused's admission acquire runs synchronously on the
	// calling goroutine (only the actual work is handed off — see its
	// doc), so calling the captured handler this many times in a plain
	// loop deterministically fills every slot before the loop's final,
	// (RequestPausedAdmission+1)th call — no goroutine-scheduling races
	// to account for here.
	for i := 0; i < offlinecache.RequestPausedAdmission; i++ {
		ts.handler(requestPausedEvent(t, fmt.Sprintf("req-hit-%d", i), res.URL))
	}
	ts.handler(requestPausedEvent(t, "req-overflow", res.URL))

	// Must resolve promptly even though every admission slot is still
	// stuck on release below — a queued (rather than bound) overflow
	// design would instead time out here.
	awaitSend(t, overflowDone)

	close(release)
	admitted.Wait()
}

// TestReplayer_OnRequestPaused_OverflowStillPassesThroughKioskShell pins
// that the kiosk-shell exemption holds on the admission-overflow path
// too: resolveOverflow answers via the miss policy WITHOUT a cache
// lookup, so without its own shell check a recovery navigation that
// happened to land during backlog pressure would be nondeterministically
// Fetch.failRequest-ed under fail_closed — the exact wedge the
// processRequestPaused exemption exists to prevent. Same slot-filling
// shape as the test above; only the overflow event's URL differs.
func TestReplayer_OnRequestPaused_OverflowStillPassesThroughKioskShell(t *testing.T) {
	ts := setupReplay(t, offlinecache.MissPolicyFailClosed)
	defer ts.ctrl.Finish()
	res := seedItem(t, ts.store, "item-hot", "hot payload")
	ts.stubFetchEnable()
	require.NoError(t, ts.replayer.EnableForItem(context.Background(), sourceFor("item-hot")))

	release := make(chan struct{})
	var admitted sync.WaitGroup
	admitted.Add(offlinecache.RequestPausedAdmission)
	ts.mockSession.EXPECT().
		Send(gomock.Any(), "Fetch.fulfillRequest", gomock.Any()).
		DoAndReturn(func(context.Context, string, map[string]interface{}) (json.RawMessage, error) {
			defer admitted.Done()
			<-release
			return json.RawMessage(`{}`), nil
		}).
		Times(offlinecache.RequestPausedAdmission)
	overflowDone := ts.expectSend("Fetch.continueRequest")

	for i := 0; i < offlinecache.RequestPausedAdmission; i++ {
		ts.handler(requestPausedEvent(t, fmt.Sprintf("req-hit-%d", i), res.URL))
	}
	ts.handler(requestPausedEvent(t, "req-shell-nav", constants.WEBAPP_URL))

	awaitSend(t, overflowDone)

	close(release)
	admitted.Wait()
}

// TestReplayer_ProcessRequestPaused_HeaderlessResourceFulfillsWithNonNullHeaders
// is the regression test for the "shader stalls forever" bug: a cached
// resource with no Content-Type and no extra headers must still fulfill,
// and its Fetch.fulfillRequest params must carry a non-null (JSON `[]`,
// not `null`) responseHeaders — Chromium rejects a present-but-null
// responseHeaders with "Invalid parameters", leaving the paused request
// hung. This path is reached in the field mainly by cross-origin iframe
// sub-resources (e.g. p5.js shader files) now that OOPIF targets are
// intercepted.
func TestReplayer_ProcessRequestPaused_HeaderlessResourceFulfillsWithNonNullHeaders(t *testing.T) {
	ts := setupReplay(t, offlinecache.MissPolicyFailClosed)
	defer ts.ctrl.Finish()

	hash := writeBlobString(t, ts.store, "void main(){}")
	rec := &offlinecache.ItemRecord{
		Item: dp1playlist.PlaylistItem{Source: "https://cdn.example.com/shaders/post.frag"},
		Resources: []offlinecache.Resource{
			// No ContentType, no Headers — the exact shape that produced a
			// nil headers slice before the fix.
			{URL: "https://cdn.example.com/shaders/post.frag", Status: 200, SHA256: hash},
		},
		Coverage: offlinecache.Coverage{Complete: true},
	}
	require.NoError(t, ts.store.SaveItem(rec))
	ts.stubFetchEnable()
	require.NoError(t, ts.replayer.EnableForItem(context.Background(), "https://cdn.example.com/shaders/post.frag"))

	done := ts.expectSend("Fetch.fulfillRequest")
	ts.handler(requestPausedEvent(t, "req-1", "https://cdn.example.com/shaders/post.frag"))
	params := awaitSend(t, done)

	assert.EqualValues(t, 200, params["responseCode"])
	raw, ok := params["responseHeaders"]
	require.True(t, ok, "responseHeaders must be present")
	require.NotNil(t, raw, "responseHeaders must be a non-nil slice (JSON [] not null), or Chromium rejects the fulfill")
	headers, ok := raw.([]map[string]interface{})
	require.True(t, ok, "responseHeaders must be a header array")
	assert.Empty(t, headers, "a headerless resource yields an empty (but non-nil) header array")
	body, ok := params["body"].(string)
	require.True(t, ok)
	decoded, err := base64.StdEncoding.DecodeString(body)
	require.NoError(t, err)
	assert.Equal(t, "void main(){}", string(decoded))
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
		Item: dp1playlist.PlaylistItem{Source: "https://example.com/track.mp3"},
		Resources: []offlinecache.Resource{
			{URL: "https://example.com/track.mp3", Status: 206, SHA256: hash, ContentType: "audio/mpeg"},
		},
		Coverage: offlinecache.Coverage{Complete: true},
	}
	require.NoError(t, ts.store.SaveItem(rec))
	ts.stubFetchEnable()
	require.NoError(t, ts.replayer.EnableForItem(context.Background(), "https://example.com/track.mp3"))

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

// TestReplayer_ProcessRequestPaused_InlineFulfillReplaysCORSHeaders is the
// regression test for the CORS-header gap: a captured cross-origin
// resource (e.g. a module script or font requested with crossorigin=""),
// well under largeAssetThreshold so it takes the inline-fulfill path,
// must replay with the same allowlisted Access-Control-* headers it was
// captured with — replaying only Content-Type/status would still be
// rejected by Chromium's own CORS enforcement even though the body bytes
// are correct.
func TestReplayer_ProcessRequestPaused_InlineFulfillReplaysCORSHeaders(t *testing.T) {
	ts := setupReplay(t, offlinecache.MissPolicyFailClosed)
	defer ts.ctrl.Finish()

	hash := writeBlobString(t, ts.store, "export default 1;")
	rec := &offlinecache.ItemRecord{
		Item: dp1playlist.PlaylistItem{Source: "https://cdn.example.com/module.js"},
		Resources: []offlinecache.Resource{
			{
				URL: "https://cdn.example.com/module.js", Status: 200, SHA256: hash, ContentType: "application/javascript",
				Headers: map[string]string{
					"Access-Control-Allow-Origin": "https://example.com",
					"Timing-Allow-Origin":         "*",
				},
			},
		},
		Coverage: offlinecache.Coverage{Complete: true},
	}
	require.NoError(t, ts.store.SaveItem(rec))
	ts.stubFetchEnable()
	require.NoError(t, ts.replayer.EnableForItem(context.Background(), "https://cdn.example.com/module.js"))

	done := ts.expectSend("Fetch.fulfillRequest")
	ts.handler(requestPausedEvent(t, "req-1", "https://cdn.example.com/module.js"))
	params := awaitSend(t, done)

	origin, ok := headerValue(t, params, "Access-Control-Allow-Origin")
	require.True(t, ok)
	assert.Equal(t, "https://example.com", origin)
	timing, ok := headerValue(t, params, "Timing-Allow-Origin")
	require.True(t, ok)
	assert.Equal(t, "*", timing)
}

// TestReplayer_ProcessRequestPaused_LargeAssetRedirectPassesCORSHeadersToStaticServer
// pins that the large-asset path threads the captured headers through to
// StaticServer.URLFor (not just the 302 fulfill itself), since a
// cors-mode fetch() CORS-checks the FINAL response of a redirect chain
// against the static server's own loopback origin.
func TestReplayer_ProcessRequestPaused_LargeAssetRedirectPassesCORSHeadersToStaticServer(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockStore := mocks.NewMockOfflineCacheStore(ctrl)
	mockStatic := mocks.NewMockOfflineCacheStaticServer(ctrl)
	mockStatic.EXPECT().BaseURL().Return(testStaticBaseURL).AnyTimes()
	mockSession := mocks.NewMockCDPSession(ctrl)

	rp := offlinecache.NewReplayer(mockStore, mockStatic, offlinecache.MissPolicyFailClosed, constants.WEBAPP_URL, wrapper.NewJSON(), zaptest.NewLogger(t))

	var handler func(json.RawMessage)
	mockSession.EXPECT().On("Fetch.requestPaused", gomock.Any()).Do(func(_ string, h func(json.RawMessage)) { handler = h }).Times(1)
	rp.Attach("", mockSession)

	corsHeaders := map[string]string{"Access-Control-Allow-Origin": "https://example.com"}
	rec := &offlinecache.ItemRecord{
		Resources: []offlinecache.Resource{
			{URL: "https://example.com/movie.mp4", Status: 200, SHA256: "deadbeef", ContentType: "video/mp4", Headers: corsHeaders},
		},
	}
	mockStore.EXPECT().LoadItem(offlinecache.SourceKey("https://example.com/movie.mp4")).Return(rec, nil).Times(1)
	mockSession.EXPECT().Send(gomock.Any(), "Fetch.enable", gomock.Any()).Return(json.RawMessage(`{}`), nil).Times(1)
	require.NoError(t, rp.EnableForItem(context.Background(), "https://example.com/movie.mp4"))

	mockStore.EXPECT().BlobSize("deadbeef").Return(int64(300*1024*1024), nil).Times(1)
	mockStatic.EXPECT().IsListening().Return(true).Times(1)
	mockStatic.EXPECT().URLFor("deadbeef", "video/mp4", corsHeaders).Return(testStaticBaseURL + "/blobs/deadbeef?ct=video%2Fmp4").Times(1)

	done := make(chan map[string]interface{}, 1)
	mockSession.EXPECT().Send(gomock.Any(), "Fetch.fulfillRequest", gomock.Any()).
		DoAndReturn(func(_ context.Context, _ string, params map[string]interface{}) (json.RawMessage, error) {
			done <- params
			return json.RawMessage(`{}`), nil
		}).Times(1)

	handler(requestPausedEvent(t, "req-1", "https://example.com/movie.mp4"))
	params := awaitSend(t, done)

	origin, ok := headerValue(t, params, "Access-Control-Allow-Origin")
	require.True(t, ok)
	assert.Equal(t, "https://example.com", origin)
}

func TestReplayer_ProcessRequestPaused_RedirectResource(t *testing.T) {
	ts := setupReplay(t, offlinecache.MissPolicyFailClosed)
	defer ts.ctrl.Finish()

	finalHash := writeBlobString(t, ts.store, "console.log(1)")
	rec := &offlinecache.ItemRecord{
		Item:  dp1playlist.PlaylistItem{ID: "item-redirect", Source: "https://example.com/lib.min.js"},
		Entry: "https://example.com/lib.min.js",
		Resources: []offlinecache.Resource{
			{URL: "https://example.com/lib.min.js", Status: 302, RedirectTo: "https://example.com/lib@2/lib.min.js"},
			{URL: "https://example.com/lib@2/lib.min.js", Status: 200, SHA256: finalHash, ContentType: "application/javascript"},
		},
		Coverage: offlinecache.Coverage{Complete: true},
	}
	require.NoError(t, ts.store.SaveItem(rec))

	ts.stubFetchEnable()
	require.NoError(t, ts.replayer.EnableForItem(context.Background(), "https://example.com/lib.min.js"))

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

	rp := offlinecache.NewReplayer(mockStore, mockStatic, offlinecache.MissPolicyFailClosed, constants.WEBAPP_URL, wrapper.NewJSON(), zaptest.NewLogger(t))

	var handler func(json.RawMessage)
	mockSession.EXPECT().On("Fetch.requestPaused", gomock.Any()).Do(func(_ string, h func(json.RawMessage)) { handler = h }).Times(1)
	rp.Attach("", mockSession)

	rec := &offlinecache.ItemRecord{
		Resources: []offlinecache.Resource{
			{URL: "https://example.com/movie.mp4", Status: 200, SHA256: "deadbeef", ContentType: "video/mp4"},
		},
	}
	mockStore.EXPECT().LoadItem(offlinecache.SourceKey("https://example.com/movie.mp4")).Return(rec, nil).Times(1)
	mockSession.EXPECT().Send(gomock.Any(), "Fetch.enable", gomock.Any()).Return(json.RawMessage(`{}`), nil).Times(1)
	require.NoError(t, rp.EnableForItem(context.Background(), "https://example.com/movie.mp4"))

	mockStore.EXPECT().BlobSize("deadbeef").Return(int64(300*1024*1024), nil).Times(1)
	mockStatic.EXPECT().IsListening().Return(true).Times(1)
	mockStatic.EXPECT().URLFor("deadbeef", "video/mp4", gomock.Nil()).Return(testStaticBaseURL + "/blobs/deadbeef?ct=video%2Fmp4").Times(1)

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

// TestReplayer_ProcessRequestPaused_LargeAssetMissesWhenStaticServerNotListening
// is the regression test for the static-server-unavailable hazard: if
// the server never actually bound (e.g. its port collided with an
// unrelated process, or Listen simply failed at startup), redirecting
// to it anyway would either dead-end (nobody home) or, worse, be
// silently absorbed by whatever unrelated service does own that port —
// neither of which Chromium can distinguish from a genuinely broken
// cached asset. An oversized resource must instead fall through to the
// ordinary fail_closed miss path, and URLFor must never even be called.
func TestReplayer_ProcessRequestPaused_LargeAssetMissesWhenStaticServerNotListening(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockStore := mocks.NewMockOfflineCacheStore(ctrl)
	mockStatic := mocks.NewMockOfflineCacheStaticServer(ctrl)
	mockStatic.EXPECT().BaseURL().Return(testStaticBaseURL).AnyTimes()
	mockSession := mocks.NewMockCDPSession(ctrl)

	rp := offlinecache.NewReplayer(mockStore, mockStatic, offlinecache.MissPolicyFailClosed, constants.WEBAPP_URL, wrapper.NewJSON(), zaptest.NewLogger(t))

	var handler func(json.RawMessage)
	mockSession.EXPECT().On("Fetch.requestPaused", gomock.Any()).Do(func(_ string, h func(json.RawMessage)) { handler = h }).Times(1)
	rp.Attach("", mockSession)

	rec := &offlinecache.ItemRecord{
		Resources: []offlinecache.Resource{
			{URL: "https://example.com/movie.mp4", Status: 200, SHA256: "deadbeef", ContentType: "video/mp4"},
		},
	}
	mockStore.EXPECT().LoadItem(offlinecache.SourceKey("https://example.com/movie.mp4")).Return(rec, nil).Times(1)
	mockSession.EXPECT().Send(gomock.Any(), "Fetch.enable", gomock.Any()).Return(json.RawMessage(`{}`), nil).Times(1)
	require.NoError(t, rp.EnableForItem(context.Background(), "https://example.com/movie.mp4"))

	mockStore.EXPECT().BlobSize("deadbeef").Return(int64(300*1024*1024), nil).Times(1)
	mockStatic.EXPECT().IsListening().Return(false).Times(1)
	// Deliberately no URLFor expectation: gomock's strict controller
	// fails this test if fulfillFromBlob calls it despite the server
	// not listening.

	done := make(chan map[string]interface{}, 1)
	mockSession.EXPECT().Send(gomock.Any(), "Fetch.failRequest", gomock.Any()).
		DoAndReturn(func(_ context.Context, _ string, params map[string]interface{}) (json.RawMessage, error) {
			done <- params
			return json.RawMessage(`{}`), nil
		}).Times(1)

	handler(requestPausedEvent(t, "req-1", "https://example.com/movie.mp4"))
	awaitSend(t, done)
}

func TestReplayer_ProcessRequestPaused_BlobMissingTreatedAsMiss(t *testing.T) {
	ts := setupReplay(t, offlinecache.MissPolicyFailClosed)
	defer ts.ctrl.Finish()

	rec := &offlinecache.ItemRecord{
		Item: dp1playlist.PlaylistItem{Source: "https://example.com/gone.js"},
		Resources: []offlinecache.Resource{
			{URL: "https://example.com/gone.js", Status: 200, SHA256: "not-a-real-hash", ContentType: "application/javascript"},
		},
	}
	require.NoError(t, ts.store.SaveItem(rec))
	ts.stubFetchEnable()
	require.NoError(t, ts.replayer.EnableForItem(context.Background(), "https://example.com/gone.js"))

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
	require.NoError(t, ts.replayer.EnableForItem(context.Background(), sourceFor("item-1")))

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
	require.NoError(t, ts.replayer.EnableForItem(context.Background(), sourceFor("item-1")))

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

// TestReplayer_ProcessRequestPaused_StaticServerLookalikeURLsAreNotPassedThrough
// is the regression test for the static-server trust bypass: the
// pass-through gate must validate the parsed loopback origin + /blobs/
// path, never a naive strings.HasPrefix against BaseURL(). Each URL below
// has testStaticBaseURL ("http://127.0.0.1:8082") as a raw string prefix
// yet does NOT actually target the loopback blobs endpoint, so continuing
// it would let a crafted request escape offline isolation to the real
// network. Under fail_closed with no scope active, the correct outcome is
// Fetch.failRequest (a miss), never Fetch.continueRequest.
func TestReplayer_ProcessRequestPaused_StaticServerLookalikeURLsAreNotPassedThrough(t *testing.T) {
	cases := []struct {
		name string
		url  string
	}{
		//nolint:gosec // G101 reads the userinfo below as a credential; it is
		// the attack fixture this case exists to reject, not a secret.
		{
			// "127.0.0.1:8082" is RFC 3986 userinfo here; the real host
			// is evil.example. url.Host excludes userinfo, so the origin
			// check rejects it.
			name: "userinfo lookalike targets a different host",
			url:  "http://127.0.0.1:8082@evil.example/blobs/deadbeef",
		},
		{
			// A different host that merely begins with the base URL text.
			name: "host suffix lookalike",
			url:  "http://127.0.0.1:8082.evil.example/blobs/deadbeef",
		},
		{
			// Correct loopback origin but NOT under the /blobs/ route the
			// static server serves — replay never emits these, so it must
			// not blanket-trust the whole origin.
			name: "correct origin but non-blobs path",
			url:  testStaticBaseURL + "/etc/passwd",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ts := setupReplay(t, offlinecache.MissPolicyFailClosed)
			defer ts.ctrl.Finish()
			done := ts.expectSend("Fetch.failRequest")
			ts.handler(requestPausedEvent(t, "req-1", tc.url))
			params := awaitSend(t, done)
			assert.Equal(t, "Failed", params["errorReason"],
				"a static-server lookalike URL must be treated as a miss, never passed through to the network")
		})
	}
}

// attachChildTarget attaches a fresh mock child session (a cross-origin
// iframe target) to ts.replayer and returns both the mock and the
// Fetch.requestPaused handler bound to it, mirroring what kiosktargets.go
// does on a real Target.attachedToTarget. onEnable, when non-nil, is armed
// as the child's Fetch.enable expectation (used when a scope is already
// active, so attach immediately arms Fetch on the new target).
func attachChildTarget(t *testing.T, ts *replayTestSetup, sessionID string, expectFetchEnable bool) (*mocks.MockCDPSession, func(json.RawMessage)) {
	t.Helper()
	child := mocks.NewMockCDPSession(ts.ctrl)
	var childHandler func(json.RawMessage)
	child.EXPECT().On("Fetch.requestPaused", gomock.Any()).
		Do(func(_ string, h func(json.RawMessage)) { childHandler = h }).Times(1)
	if expectFetchEnable {
		child.EXPECT().Send(gomock.Any(), "Fetch.enable", gomock.Any()).Return(json.RawMessage(`{}`), nil).Times(1)
	}
	ts.replayer.Attach(sessionID, child)
	require.NotNil(t, childHandler)
	return child, childHandler
}

// TestReplayer_AttachChild_WhileEnabledArmsFetchImmediately pins that a
// child iframe attaching AFTER a scope is already enabled gets Fetch
// enabled on it right away — the display path enables scope before the
// kiosk navigates and creates the iframe, so relying only on the next
// EnableForPlaylist pass would leave the iframe un-intercepted meanwhile.
func TestReplayer_AttachChild_WhileEnabledArmsFetchImmediately(t *testing.T) {
	ts := setupReplay(t, offlinecache.MissPolicyFailClosed)
	defer ts.ctrl.Finish()
	res := seedItem(t, ts.store, "item-1", "software payload")
	ts.stubFetchEnable()
	require.NoError(t, ts.replayer.EnableForItem(context.Background(), sourceFor("item-1")))

	// Attaching the child while enabled must arm Fetch on it (expected).
	child, childHandler := attachChildTarget(t, ts, "child-1", true)

	// A request paused on the child target resolves from the same cached
	// resource union as the page — the iframe's own request is now served
	// from cache instead of falling through to the offline network. It
	// must be answered on the CHILD session, not the page.
	done := make(chan map[string]interface{}, 1)
	child.EXPECT().Send(gomock.Any(), "Fetch.fulfillRequest", gomock.Any()).
		DoAndReturn(func(_ context.Context, _ string, params map[string]interface{}) (json.RawMessage, error) {
			done <- params
			return json.RawMessage(`{}`), nil
		}).Times(1)
	childHandler(requestPausedEvent(t, "req-c", res.URL))
	awaitSend(t, done)
}

// TestReplayer_EnableForPlaylist_FansOutToAllTargets pins that enabling a
// scope arms Fetch on every currently-attached target (the page and each
// iframe), not just the top-level page.
func TestReplayer_EnableForPlaylist_FansOutToAllTargets(t *testing.T) {
	ts := setupReplay(t, offlinecache.MissPolicyFailClosed)
	defer ts.ctrl.Finish()
	seedItem(t, ts.store, "item-1", "software payload")

	// Child attaches BEFORE enabling, so no attach-time Fetch.enable.
	child, _ := attachChildTarget(t, ts, "child-1", false)

	// Enabling now must send Fetch.enable to BOTH the page and the child.
	ts.mockSession.EXPECT().Send(gomock.Any(), "Fetch.enable", gomock.Any()).Return(json.RawMessage(`{}`), nil).Times(1)
	child.EXPECT().Send(gomock.Any(), "Fetch.enable", gomock.Any()).Return(json.RawMessage(`{}`), nil).Times(1)
	require.NoError(t, ts.replayer.EnableForItem(context.Background(), sourceFor("item-1")))
}

// TestReplayer_Disable_FansOutToAllTargets pins that Disable turns Fetch
// off on every attached target and then clears scope.
func TestReplayer_Disable_FansOutToAllTargets(t *testing.T) {
	ts := setupReplay(t, offlinecache.MissPolicyFailClosed)
	defer ts.ctrl.Finish()
	seedItem(t, ts.store, "item-1", "software payload")

	child, _ := attachChildTarget(t, ts, "child-1", false)

	ts.mockSession.EXPECT().Send(gomock.Any(), "Fetch.enable", gomock.Any()).Return(json.RawMessage(`{}`), nil).Times(1)
	child.EXPECT().Send(gomock.Any(), "Fetch.enable", gomock.Any()).Return(json.RawMessage(`{}`), nil).Times(1)
	require.NoError(t, ts.replayer.EnableForItem(context.Background(), sourceFor("item-1")))

	ts.mockSession.EXPECT().Send(gomock.Any(), "Fetch.disable", gomock.Any()).Return(json.RawMessage(`{}`), nil).Times(1)
	child.EXPECT().Send(gomock.Any(), "Fetch.disable", gomock.Any()).Return(json.RawMessage(`{}`), nil).Times(1)
	require.NoError(t, ts.replayer.Disable(context.Background()))
}

// TestReplayer_Disable_PartialTargetFailureForcesPassThrough pins that if
// even one target's Fetch.disable fails, replay cannot prove interception
// is off everywhere, so it keeps scope and forces the same pass-through
// relaxation a mixed scope uses (never fail_closed) — while still trying
// every target.
func TestReplayer_Disable_PartialTargetFailureForcesPassThrough(t *testing.T) {
	ts := setupReplay(t, offlinecache.MissPolicyFailClosed)
	defer ts.ctrl.Finish()
	res := seedItem(t, ts.store, "item-1", "software payload")

	child, _ := attachChildTarget(t, ts, "child-1", false)
	ts.mockSession.EXPECT().Send(gomock.Any(), "Fetch.enable", gomock.Any()).Return(json.RawMessage(`{}`), nil).Times(1)
	child.EXPECT().Send(gomock.Any(), "Fetch.enable", gomock.Any()).Return(json.RawMessage(`{}`), nil).Times(1)
	require.NoError(t, ts.replayer.EnableForItem(context.Background(), sourceFor("item-1")))

	// The page disables fine, but the child's Fetch.disable fails. Both
	// are still attempted.
	ts.mockSession.EXPECT().Send(gomock.Any(), "Fetch.disable", gomock.Any()).Return(json.RawMessage(`{}`), nil).Times(1)
	child.EXPECT().Send(gomock.Any(), "Fetch.disable", gomock.Any()).Return(nil, assert.AnError).Times(1)
	err := ts.replayer.Disable(context.Background())
	assert.ErrorIs(t, err, assert.AnError)

	// Scope was NOT cleared: a cached hit still serves, and an unrelated
	// miss passes through instead of failing closed.
	hitDone := ts.expectSend("Fetch.fulfillRequest")
	ts.handler(requestPausedEvent(t, "req-hit", res.URL))
	awaitSend(t, hitDone)

	missDone := ts.expectSend("Fetch.continueRequest")
	ts.handler(requestPausedEvent(t, "req-miss", "https://example.com/unrelated.js"))
	awaitSend(t, missDone)
}

// TestReplayer_Attach_RootReattachDropsChildTargets pins that a fresh
// top-level connection (a kiosk reconnect) wipes every child target from
// the prior connection: their sessionIds are meaningless against the new
// socket, so a delayed event bound to an old child must be dropped rather
// than answered.
func TestReplayer_Attach_RootReattachDropsChildTargets(t *testing.T) {
	ts := setupReplay(t, offlinecache.MissPolicyFailClosed)
	defer ts.ctrl.Finish()

	_, oldChildHandler := attachChildTarget(t, ts, "child-1", false)

	// Reconnect: a new top-level session supersedes the old one (which is
	// closed) and drops the old child.
	newRoot := mocks.NewMockCDPSession(ts.ctrl)
	newRoot.EXPECT().On("Fetch.requestPaused", gomock.Any()).Times(1)
	ts.mockSession.EXPECT().Close().Return(nil).Times(1)
	ts.replayer.Attach("", newRoot)

	// A late event from the dropped child must be ignored: neither the old
	// child nor the new root has a Send expectation, so any attempt to
	// answer it fails the strict gomock controller.
	oldChildHandler(requestPausedEvent(t, "req-stale", "https://example.com/whatever.js"))
	time.Sleep(100 * time.Millisecond)
}

// TestReplayer_AttachChild_SucceedsWhenRootIsCurrent pins that AttachChild
// behaves exactly like the plain Attach child path when root still
// matches the currently active top-level session — the common,
// no-reconnect-in-flight case.
func TestReplayer_AttachChild_SucceedsWhenRootIsCurrent(t *testing.T) {
	ts := setupReplay(t, offlinecache.MissPolicyFailClosed)
	defer ts.ctrl.Finish()

	child := mocks.NewMockCDPSession(ts.ctrl)
	child.EXPECT().On("Fetch.requestPaused", gomock.Any()).Times(1)
	attached := ts.replayer.AttachChild(ts.mockSession, "child-1", child)
	assert.True(t, attached, "AttachChild must succeed when root is still the current top-level session")
}

// TestReplayer_AttachChild_RejectsSupersededRoot pins the fix for a
// reconnect race (PR #229 review): kiosktargets.go's
// Target.attachedToTarget handler runs on its own goroutine, off the CDP
// read pump that delivered the event, so a kiosk reconnect
// (Attach("", newRoot)) can supersede root before that goroutine's
// AttachChild call actually runs. AttachChild must detect this and
// refuse to attach — otherwise a child session bound to the now-dead OLD
// root's socket would be injected into the CURRENT generation's target
// set, and the next EnableForPlaylist/Disable fan-out would try (and
// fail or stall on the dead socket's send timeout) to Fetch.enable/
// disable a session that can never respond.
func TestReplayer_AttachChild_RejectsSupersededRoot(t *testing.T) {
	ts := setupReplay(t, offlinecache.MissPolicyFailClosed)
	defer ts.ctrl.Finish()
	staleRoot := ts.mockSession

	// Reconnect: a new top-level session supersedes staleRoot.
	newRoot := mocks.NewMockCDPSession(ts.ctrl)
	newRoot.EXPECT().On("Fetch.requestPaused", gomock.Any()).Times(1)
	staleRoot.EXPECT().Close().Return(nil).Times(1)
	ts.replayer.Attach("", newRoot)

	// A delayed handler for an event that was read on staleRoot's pump
	// before the reconnect now tries to attach against staleRoot. The
	// child mock has NO expectations set at all (no On, no Send) — if
	// AttachChild wrongly proceeded, the strict gomock controller would
	// fail this test on the unexpected On call attachChild makes.
	child := mocks.NewMockCDPSession(ts.ctrl)
	attached := ts.replayer.AttachChild(staleRoot, "child-stale", child)
	assert.False(t, attached, "AttachChild must reject a root that a later reconnect has already superseded")
}

// TestReplayer_DetachChild_SucceedsWhenRootIsCurrent pins that
// DetachChild behaves exactly like the plain Detach path when root still
// matches the currently active top-level session.
func TestReplayer_DetachChild_SucceedsWhenRootIsCurrent(t *testing.T) {
	ts := setupReplay(t, offlinecache.MissPolicyFailClosed)
	defer ts.ctrl.Finish()

	child, childHandler := attachChildTarget(t, ts, "child-1", false)
	child.EXPECT().Close().Return(nil).Times(1)
	ts.replayer.DetachChild(ts.mockSession, "child-1")

	// After detach, a late event on that child is dropped (no Send).
	childHandler(requestPausedEvent(t, "req-stale", "https://example.com/whatever.js"))
	time.Sleep(100 * time.Millisecond)
}

// TestReplayer_DetachChild_RejectsSupersededRoot is DetachChild's
// counterpart to AttachChild's reconnect-race guard above: a stale
// Target.detachedFromTarget delivered on a superseded root must never be
// allowed to touch the CURRENT generation's target set — otherwise, in
// the (should-never-happen, since CDP mints a fresh sessionId per
// connection) case a stale event's sessionId collided with a live
// child's, it would incorrectly drop a target that is still in active
// use.
func TestReplayer_DetachChild_RejectsSupersededRoot(t *testing.T) {
	ts := setupReplay(t, offlinecache.MissPolicyFailClosed)
	defer ts.ctrl.Finish()
	staleRoot := ts.mockSession

	// Reconnect supersedes staleRoot, then a legitimate child attaches
	// under the NEW root using the same sessionId a stale event below
	// will reference.
	newRoot := mocks.NewMockCDPSession(ts.ctrl)
	newRoot.EXPECT().On("Fetch.requestPaused", gomock.Any()).Times(1)
	staleRoot.EXPECT().Close().Return(nil).Times(1)
	ts.replayer.Attach("", newRoot)

	liveChild := mocks.NewMockCDPSession(ts.ctrl)
	liveChild.EXPECT().On("Fetch.requestPaused", gomock.Any()).Times(1)
	require.True(t, ts.replayer.AttachChild(newRoot, "child-1", liveChild))

	// liveChild has NO Close() expectation: if DetachChild wrongly
	// proceeded against the stale root, the strict gomock controller
	// would fail this test on the unexpected Close() call.
	ts.replayer.DetachChild(staleRoot, "child-1")
}

// TestReplayer_Detach_ClosesAndDropsChildTarget pins that detaching a
// child unregisters it (Close) and stops routing its events.
func TestReplayer_Detach_ClosesAndDropsChildTarget(t *testing.T) {
	ts := setupReplay(t, offlinecache.MissPolicyFailClosed)
	defer ts.ctrl.Finish()

	child, childHandler := attachChildTarget(t, ts, "child-1", false)
	child.EXPECT().Close().Return(nil).Times(1)
	ts.replayer.Detach("child-1")

	// After detach, a late event on that child is dropped (no Send).
	childHandler(requestPausedEvent(t, "req-stale", "https://example.com/whatever.js"))
	time.Sleep(100 * time.Millisecond)
}

// TestReplayer_Detach_UnknownSessionIsNoop pins that detaching a sessionId
// that was never attached is a harmless no-op.
func TestReplayer_Detach_UnknownSessionIsNoop(t *testing.T) {
	ts := setupReplay(t, offlinecache.MissPolicyFailClosed)
	defer ts.ctrl.Finish()
	ts.replayer.Detach("never-attached")
	// Detaching the empty (top-level) sessionId must also be ignored so it
	// can never wipe live top-level interception.
	ts.replayer.Detach("")
}

// TestReplayer_TransportFailureRetiresAndClosesTheSession pins the first
// half of replay recovery. A gorilla/websocket connection is unusable
// after a write error or an exceeded write deadline, but a failed Send
// used to be reported to the caller with the dead session left installed
// — so every later scope call sent into the corpse. Worse, because the
// socket was never closed, Chromium could still consider this client
// attached with Fetch armed at pattern "*": CDP releases paused requests
// when a client DISCONNECTS, so a half-dead socket that is never closed
// can leave requests paused and hang playback.
func TestReplayer_TransportFailureRetiresAndClosesTheSession(t *testing.T) {
	t.Run("a transport failure retires the root", func(t *testing.T) {
		ts := setupReplay(t, offlinecache.MissPolicyFailClosed)
		defer ts.ctrl.Finish()
		seedItem(t, ts.store, "item-1", "software payload")

		ts.mockSession.EXPECT().Send(gomock.Any(), "Fetch.enable", gomock.Any()).
			Return(nil, fmt.Errorf("write failed: %w", offlinecache.ErrCDPTransport)).Times(1)
		// Closing is what makes Chromium let go of the paused requests.
		ts.mockSession.EXPECT().Close().Return(nil).Times(1)

		err := ts.replayer.EnableForItem(context.Background(), sourceFor("item-1"))
		require.ErrorIs(t, err, offlinecache.ErrCDPTransport)
		assert.False(t, ts.replayer.RootAttached(),
			"a retired root must be reported as gone so KioskReplay.SyncPlaylist knows to re-dial")
	})

	t.Run("a CDP error reply leaves the session installed", func(t *testing.T) {
		ts := setupReplay(t, offlinecache.MissPolicyFailClosed)
		defer ts.ctrl.Finish()
		seedItem(t, ts.store, "item-1", "software payload")

		// The peer answered, so the connection is healthy and only the
		// command was refused. Tearing it down here would churn a
		// working session — no Close expectation, so gomock fails the
		// test if one happens.
		ts.mockSession.EXPECT().Send(gomock.Any(), "Fetch.enable", gomock.Any()).
			Return(nil, assert.AnError).Times(1)

		require.Error(t, ts.replayer.EnableForItem(context.Background(), sourceFor("item-1")))
		assert.True(t, ts.replayer.RootAttached(),
			"a rejected command must not be mistaken for a dead connection")
	})
}

// TestReplayer_AttachChild_UnarmedChildIsNotResumable is the regression
// test for the silent fail_closed bypass. A child target whose
// Fetch.enable fails is not intercepted at all, so resuming it lets the
// iframe fetch straight from the network while the scope still claims
// fail_closed and status still reports the item cached. AttachChild's
// bool is the caller's authorization to resume, so it must be false.
func TestReplayer_AttachChild_UnarmedChildIsNotResumable(t *testing.T) {
	newChild := func(t *testing.T, ts *replayTestSetup, sendErr error) *mocks.MockCDPSession {
		t.Helper()
		child := mocks.NewMockCDPSession(ts.ctrl)
		child.EXPECT().On("Fetch.requestPaused", gomock.Any()).Times(1)
		child.EXPECT().Send(gomock.Any(), "Fetch.enable", gomock.Any()).Return(nil, sendErr).Times(1)
		return child
	}

	t.Run("fail_closed keeps the child paused", func(t *testing.T) {
		ts := setupReplay(t, offlinecache.MissPolicyFailClosed)
		defer ts.ctrl.Finish()
		seedItem(t, ts.store, "item-1", "software payload")
		ts.stubFetchEnable()
		require.NoError(t, ts.replayer.EnableForItem(context.Background(), sourceFor("item-1")))

		// A transport failure also retires the session, so it is closed.
		child := newChild(t, ts, fmt.Errorf("write failed: %w", offlinecache.ErrCDPTransport))
		child.EXPECT().Close().Return(nil).Times(1)

		assert.False(t, ts.replayer.AttachChild(ts.mockSession, "child-1", child),
			"an unarmed child must never be resumed under fail_closed: its requests would bypass replay entirely")
	})

	t.Run("pass_through resumes, since the network is already permitted", func(t *testing.T) {
		ts := setupReplay(t, offlinecache.MissPolicyPassThrough)
		defer ts.ctrl.Finish()
		seedItem(t, ts.store, "item-1", "software payload")
		ts.stubFetchEnable()
		require.NoError(t, ts.replayer.EnableForItem(context.Background(), sourceFor("item-1")))

		child := newChild(t, ts, fmt.Errorf("write failed: %w", offlinecache.ErrCDPTransport))
		child.EXPECT().Close().Return(nil).Times(1)

		assert.True(t, ts.replayer.AttachChild(ts.mockSession, "child-1", child),
			"under pass_through a miss already reaches the network, so a hung iframe buys nothing")
	})

	t.Run("a successfully armed child is resumable", func(t *testing.T) {
		ts := setupReplay(t, offlinecache.MissPolicyFailClosed)
		defer ts.ctrl.Finish()
		seedItem(t, ts.store, "item-1", "software payload")
		ts.stubFetchEnable()
		require.NoError(t, ts.replayer.EnableForItem(context.Background(), sourceFor("item-1")))

		child := newChild(t, ts, nil)
		assert.True(t, ts.replayer.AttachChild(ts.mockSession, "child-1", child))
	})
}

// TestReplayer_OnRequestPaused_OverflowGoroutinesAreBounded pins the
// second half of the admission story. The overflow path was a bare
// `go resolveOverflow(...)` per excess event — "O(1)-cheap and
// short-lived" in the work it does, but not in how long it can take:
// resolveOverflow answers through CDPSession.Send, which blocks on that
// session's writeMu before its own deadline is ever consulted. Against a
// backpressured socket those goroutines accumulated at page-load rate,
// which is the resource exhaustion the admission bound exists to prevent
// in the first place.
//
// Every response here is stuck mid-flight, so nothing drains: the number
// of Send calls Chromium can provoke must stop at the two bounds instead
// of tracking the number of paused requests.
func TestReplayer_OnRequestPaused_OverflowGoroutinesAreBounded(t *testing.T) {
	ts := setupReplay(t, offlinecache.MissPolicyFailClosed)
	defer ts.ctrl.Finish()
	res := seedItem(t, ts.store, "item-hot", "hot payload")
	ts.stubFetchEnable()
	require.NoError(t, ts.replayer.EnableForItem(context.Background(), sourceFor("item-hot")))

	release := make(chan struct{})
	var sends atomic.Int64
	stuck := func(context.Context, string, map[string]interface{}) (json.RawMessage, error) {
		sends.Add(1)
		<-release
		return json.RawMessage(`{}`), nil
	}
	// Both response shapes wedge: hits (admitted slots) and misses
	// (overflow slots).
	ts.mockSession.EXPECT().Send(gomock.Any(), "Fetch.fulfillRequest", gomock.Any()).DoAndReturn(stuck).AnyTimes()
	ts.mockSession.EXPECT().Send(gomock.Any(), "Fetch.failRequest", gomock.Any()).DoAndReturn(stuck).AnyTimes()
	// Saturating both bounds retires the session, which closes it — see
	// the assertion at the end for why closing is the point.
	closed := make(chan struct{})
	ts.mockSession.EXPECT().Close().DoAndReturn(func() error {
		close(closed)
		return nil
	}).Times(1)

	// Far more paused requests than both bounds combined.
	const flood = offlinecache.RequestPausedAdmission + offlinecache.OverflowAdmission + 500
	for i := 0; i < flood; i++ {
		ts.handler(requestPausedEvent(t, fmt.Sprintf("req-%d", i), res.URL))
	}

	// Everything that got in is now wedged on release, so the in-flight
	// count can only be what the two semaphores admitted. Give the
	// spawned goroutines a moment to actually reach Send before counting.
	maxInFlight := int64(offlinecache.RequestPausedAdmission + offlinecache.OverflowAdmission)
	require.Eventually(t, func() bool { return sends.Load() > 0 }, 2*time.Second, 10*time.Millisecond)
	require.Never(t, func() bool { return sends.Load() > maxInFlight }, 300*time.Millisecond, 20*time.Millisecond,
		"concurrent responses must be capped by the two admission bounds, not by how many requests Chromium paused")

	// Bounding alone is not enough. Everything past the bounds was once
	// dropped with a warning, which did not shed load — a paused request
	// that is never continued, fulfilled or failed stays paused forever,
	// hanging that resource and any page waiting on it. There is no way
	// to answer those requests either, since answering means Send and
	// Send is what ran out. So the session is retired and CLOSED, which
	// is what makes Chromium release every request it has paused on this
	// target: the page proceeds against the live network instead of
	// hanging, and the next SyncPlaylist re-dials.
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("a session that saturated both admission bounds must be closed, or the requests it can no longer answer stay paused forever")
	}
	assert.Eventually(t, func() bool { return !ts.replayer.RootAttached() }, 2*time.Second, 10*time.Millisecond,
		"the retired session must be dropped from the target set so SyncPlaylist knows to re-dial")

	close(release)
}

// TestReplayer_OnRequestPaused_SaturationNotifiesScopeLost pins the
// recovery half of the saturation trade-off. Retiring the session is what
// keeps the page from hanging, but it also means Fetch interception is
// gone: a fail_closed scope is enforced by NOTHING until something calls
// SyncPlaylist again. Left to itself that something is playlist-refresher's
// periodic pass, up to PLAYLIST_REFRESH_INTERVAL (5 minutes) away, and for
// that whole window a fully cached artwork is served from the live network
// while getOfflineCacheStatus still reports it ready.
//
// main.go wires this handler to PlaylistRefresher.ForceRefresh — the same
// recovery the kiosk-reconnect path already uses for the identical "scope
// was invalidated and the periodic loop has not noticed" problem. That is
// best-effort acceleration of the window, not a bound on it (see
// docs/offline-artwork-capture.md for the paths that still fall back to
// the periodic tick).
//
// The handler must fire AFTER the root is actually retired. Notifying
// first would send the recovery pass at a root still present in targets:
// it would re-arm scope on a session about to be closed and see
// RootAttached() == true, so it would never re-dial — spending the one
// buffered ForceRefresh signal without recovering anything.
func TestReplayer_OnRequestPaused_SaturationNotifiesScopeLost(t *testing.T) {
	ts := setupReplay(t, offlinecache.MissPolicyFailClosed)
	defer ts.ctrl.Finish()
	res := seedItem(t, ts.store, "item-hot", "hot payload")
	ts.stubFetchEnable()
	require.NoError(t, ts.replayer.EnableForItem(context.Background(), sourceFor("item-hot")))

	release := make(chan struct{})
	stuck := func(context.Context, string, map[string]interface{}) (json.RawMessage, error) {
		<-release
		return json.RawMessage(`{}`), nil
	}
	ts.mockSession.EXPECT().Send(gomock.Any(), "Fetch.fulfillRequest", gomock.Any()).DoAndReturn(stuck).AnyTimes()
	ts.mockSession.EXPECT().Send(gomock.Any(), "Fetch.failRequest", gomock.Any()).DoAndReturn(stuck).AnyTimes()
	ts.mockSession.EXPECT().Close().Return(nil).Times(1)

	// Records whether the session was already retired when the handler
	// ran, which is the ordering the recovery depends on.
	attachedAtCallback := make(chan bool, 1)
	ts.replayer.(offlinecache.ScopeLostRegistrar).SetOnScopeLost(func() {
		select {
		case attachedAtCallback <- ts.replayer.RootAttached():
		default:
		}
	})

	const flood = offlinecache.RequestPausedAdmission + offlinecache.OverflowAdmission + 500
	for i := 0; i < flood; i++ {
		ts.handler(requestPausedEvent(t, fmt.Sprintf("req-%d", i), res.URL))
	}

	select {
	case rootStillAttached := <-attachedAtCallback:
		assert.False(t, rootStillAttached,
			"the scope-lost handler must run after the session is retired, or the re-arm it triggers races the retirement")
	case <-time.After(2 * time.Second):
		t.Fatal("saturation must notify the scope-lost handler, or a fail_closed scope stays unenforced until playlist-refresher's next periodic pass")
	}

	close(release)
}

// TestReplayer_SetOnScopeLost_NilHandlerIsSafe pins that the handler is
// genuinely optional: unset (or explicitly cleared), saturation must still
// retire cleanly and simply fall back to the periodic-pass recovery that
// existed before this hook.
func TestReplayer_SetOnScopeLost_NilHandlerIsSafe(t *testing.T) {
	ts := setupReplay(t, offlinecache.MissPolicyFailClosed)
	defer ts.ctrl.Finish()
	res := seedItem(t, ts.store, "item-hot", "hot payload")
	ts.stubFetchEnable()
	require.NoError(t, ts.replayer.EnableForItem(context.Background(), sourceFor("item-hot")))

	ts.replayer.(offlinecache.ScopeLostRegistrar).SetOnScopeLost(func() {})
	ts.replayer.(offlinecache.ScopeLostRegistrar).SetOnScopeLost(nil) // cleared again

	release := make(chan struct{})
	stuck := func(context.Context, string, map[string]interface{}) (json.RawMessage, error) {
		<-release
		return json.RawMessage(`{}`), nil
	}
	ts.mockSession.EXPECT().Send(gomock.Any(), "Fetch.fulfillRequest", gomock.Any()).DoAndReturn(stuck).AnyTimes()
	ts.mockSession.EXPECT().Send(gomock.Any(), "Fetch.failRequest", gomock.Any()).DoAndReturn(stuck).AnyTimes()
	closed := make(chan struct{})
	ts.mockSession.EXPECT().Close().DoAndReturn(func() error {
		close(closed)
		return nil
	}).Times(1)

	const flood = offlinecache.RequestPausedAdmission + offlinecache.OverflowAdmission + 500
	for i := 0; i < flood; i++ {
		ts.handler(requestPausedEvent(t, fmt.Sprintf("req-%d", i), res.URL))
	}

	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("saturation must still retire the session with no scope-lost handler wired")
	}

	close(release)
}

// TestReplayer_OnRequestPaused_ChildSaturationRetiresTheRootSession is the
// regression test for saturation delivered on a CHILD (flat-mode OOPIF)
// target. onRequestPaused is registered per target, and the admission
// semaphores are per-replayer, so whichever target's event happens to lose
// the race is the one that trips saturation — and on a real device the
// burst comes from the cross-origin artwork iframe, so that is the LIKELY
// case, not an exotic one.
//
// Retiring the tripping target would be worse than useless there.
// flatSession.Close is a per-target detach that only unregisters handlers
// (it must never close the shared socket), so Chromium would keep the
// connection, keep Fetch armed at "*" on that iframe, and keep its
// requests paused — while the replayer no longer has a handler to answer
// them or a targets entry to re-arm. That is exactly the permanent hang
// retiring exists to prevent, and RootAttached() would still be true, so
// SyncPlaylist would not re-dial and the scope-lost recovery could not fix
// it either.
//
// Saturation is a property of the SOCKET, so the ROOT is what must be
// retired: closing it closes the real websocket, which is what makes
// Chromium release every paused request on every target on it.
func TestReplayer_OnRequestPaused_ChildSaturationRetiresTheRootSession(t *testing.T) {
	ts := setupReplay(t, offlinecache.MissPolicyFailClosed)
	defer ts.ctrl.Finish()
	res := seedItem(t, ts.store, "item-hot", "hot payload")
	ts.stubFetchEnable()
	require.NoError(t, ts.replayer.EnableForItem(context.Background(), sourceFor("item-hot")))

	// Attach a child and capture ITS Fetch.requestPaused handler, so the
	// flood below is delivered on the child session exactly as Chromium
	// would deliver an iframe's paused requests.
	var childHandler func(json.RawMessage)
	child := mocks.NewMockCDPSession(ts.ctrl)
	child.EXPECT().On("Fetch.requestPaused", gomock.Any()).
		Do(func(_ string, h func(json.RawMessage)) { childHandler = h }).Times(1)
	child.EXPECT().Send(gomock.Any(), "Fetch.enable", gomock.Any()).Return(json.RawMessage(`{}`), nil).Times(1)
	require.True(t, ts.replayer.AttachChild(ts.mockSession, "child-1", child))
	require.NotNil(t, childHandler)

	release := make(chan struct{})
	stuck := func(context.Context, string, map[string]interface{}) (json.RawMessage, error) {
		<-release
		return json.RawMessage(`{}`), nil
	}
	child.EXPECT().Send(gomock.Any(), "Fetch.fulfillRequest", gomock.Any()).DoAndReturn(stuck).AnyTimes()
	child.EXPECT().Send(gomock.Any(), "Fetch.failRequest", gomock.Any()).DoAndReturn(stuck).AnyTimes()

	// The ROOT is what must be closed. The child must NOT be: closing it
	// releases nothing and strands its paused requests permanently.
	rootClosed := make(chan struct{})
	ts.mockSession.EXPECT().Close().DoAndReturn(func() error {
		close(rootClosed)
		return nil
	}).Times(1)

	const flood = offlinecache.RequestPausedAdmission + offlinecache.OverflowAdmission + 500
	for i := 0; i < flood; i++ {
		childHandler(requestPausedEvent(t, fmt.Sprintf("req-%d", i), res.URL))
	}

	select {
	case <-rootClosed:
	case <-time.After(2 * time.Second):
		t.Fatal("saturation tripped by a child target must retire the ROOT session: closing the child only unregisters handlers, so Chromium releases nothing and that iframe's requests hang forever")
	}
	assert.Eventually(t, func() bool { return !ts.replayer.RootAttached() }, 2*time.Second, 10*time.Millisecond,
		"the root must leave the target set so SyncPlaylist knows to re-dial; otherwise no recovery path fires at all")

	close(release)
}
