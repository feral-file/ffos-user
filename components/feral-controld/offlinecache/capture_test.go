package offlinecache_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
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

// captureTestHarness wires a Capturer against a fake CDP websocket peer
// (fakeWSConn, reused from cdpsession_test.go) driven by a background
// goroutine that answers Send calls and injects Network.* events, plus a
// mocked HTTPClient that stands in for both the headless target discovery
// (/json) and the plain-GET byte fetch every successful resource triggers.
type captureTestHarness struct {
	ctrl           *gomock.Controller
	mockDownloader *mocks.MockOfflineCacheDownloader
	mockDialer     *mocks.MockWebSocketDialer
	mockHTTP       *mocks.MockHTTPClient
	conn           *fakeWSConn
	store          offlinecache.Store
	capturer       offlinecache.Capturer
}

func setupCapture(t *testing.T) *captureTestHarness {
	ctrl := gomock.NewController(t)
	mockDownloader := mocks.NewMockOfflineCacheDownloader(ctrl)
	mockDialer := mocks.NewMockWebSocketDialer(ctrl)
	mockHTTP := mocks.NewMockHTTPClient(ctrl)
	store, _ := newTestStore(t)
	logger := zaptest.NewLogger(t)

	mockDownloader.EXPECT().Acquire(gomock.Any()).Return("http://127.0.0.1:9223", nil).Times(1)
	mockDownloader.EXPECT().Release().Times(1)

	conn := newFakeWSConn()
	mockDialer.EXPECT().DialContext(gomock.Any(), "ws://127.0.0.1:9223/devtools/page/1", nil).
		Return(conn, nil, nil).Times(1)

	targetsBody := `[{"type":"page","webSocketDebuggerUrl":"ws://127.0.0.1:9223/devtools/page/1"}]`
	req, err := http.NewRequest(http.MethodGet, "http://127.0.0.1:9223/json", nil)
	require.NoError(t, err)
	mockHTTP.EXPECT().NewRequest(http.MethodGet, "http://127.0.0.1:9223/json", nil).Return(req, nil).Times(1)
	mockHTTP.EXPECT().Do(gomock.Any()).Return(&http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(targetsBody)),
	}, nil).Times(1)

	capturer := offlinecache.NewCapturer(
		mockDownloader, mockDialer, mockHTTP, store,
		wrapper.NewJSON(), wrapper.NewIO(), wrapper.NewClock(), 0, logger,
	)

	return &captureTestHarness{
		ctrl: ctrl, mockDownloader: mockDownloader, mockDialer: mockDialer,
		mockHTTP: mockHTTP, conn: conn, store: store, capturer: capturer,
	}
}

// domainEnableCallCount is how many outbound CDP calls capture.go always
// issues, in order, before the observation window begins: Network.enable,
// Page.enable, resetTargetState's Network.clearBrowserCache and
// Storage.clearDataForOrigin, then Page.navigate.
const domainEnableCallCount = 5

// answerDomainEnables drains and acks every call capture.go always
// issues before the observation window begins (see
// domainEnableCallCount).
func (h *captureTestHarness) answerDomainEnables(t *testing.T) {
	t.Helper()
	for i := 0; i < domainEnableCallCount; i++ {
		msg := h.conn.nextOutbound(t)
		h.ackEmpty(t, msg)
	}
}

func (h *captureTestHarness) ackEmpty(t *testing.T, msg map[string]interface{}) {
	t.Helper()
	reply, err := json.Marshal(map[string]interface{}{
		"id":     int64(msg["id"].(float64)),
		"result": map[string]interface{}{},
	})
	require.NoError(t, err)
	h.conn.pushReply(reply)
}

func (h *captureTestHarness) pushEvent(t *testing.T, method string, params interface{}) {
	t.Helper()
	data, err := json.Marshal(map[string]interface{}{"method": method, "params": params})
	require.NoError(t, err)
	h.conn.pushReply(data)
}

// drainAndAckRemaining is the last thing a test's scripted-event
// goroutine should do instead of returning immediately: it keeps acking
// any further outbound CDP calls (notably capture.go's post-capture
// clearObservedOriginsStorage, whose Storage.clearDataForOrigin call
// count depends on how many distinct origins THIS test's resources
// happen to touch) until Capture returns and its internal
// `defer session.Close()` closes the connection.
func (h *captureTestHarness) drainAndAckRemaining(t *testing.T) {
	t.Helper()
	h.conn.drainAndAckRemaining(t)
}

func TestCapturer_Capture_SingleResource(t *testing.T) {
	h := setupCapture(t)
	defer h.ctrl.Finish()

	h.mockHTTP.EXPECT().
		NewRequest(http.MethodGet, "https://example.com/index.html", nil).
		DoAndReturn(func(method, url string, body io.Reader) (*http.Request, error) {
			return http.NewRequest(method, url, body)
		}).Times(1)
	h.mockHTTP.EXPECT().Do(gomock.Any()).Return(&http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader("<html>art</html>")),
	}, nil).Times(1)

	go func() {
		h.answerDomainEnables(t)
		h.pushEvent(t, "Network.requestWillBeSent", map[string]interface{}{
			"requestId": "req-1",
			"request":   map[string]interface{}{"url": "https://example.com/index.html"},
		})
		h.pushEvent(t, "Network.responseReceived", map[string]interface{}{
			"requestId": "req-1",
			"response": map[string]interface{}{
				"url": "https://example.com/index.html", "status": 200, "mimeType": "text/html",
			},
		})
		h.drainAndAckRemaining(t)
	}()

	item := dp1playlist.PlaylistItem{ID: "item-1", Source: "https://example.com/index.html"}
	rec, err := h.capturer.Capture(context.Background(), item, 300)
	require.NoError(t, err)

	assert.Equal(t, "item-1", rec.ItemID)
	assert.Equal(t, "https://example.com/index.html", rec.Entry)
	assert.True(t, rec.Coverage.Complete)
	require.Len(t, rec.Resources, 1)
	res := rec.Resources[0]
	assert.Equal(t, "https://example.com/index.html", res.URL)
	assert.Equal(t, 200, res.Status)
	assert.Equal(t, "text/html", res.ContentType)
	require.NotEmpty(t, res.SHA256)

	blob, err := h.store.ReadBlob(res.SHA256)
	require.NoError(t, err)
	assert.Equal(t, "<html>art</html>", string(blob))

	saved, err := h.store.LoadItem("item-1")
	require.NoError(t, err)
	assert.Equal(t, rec.Resources, saved.Resources)
}

// TestCapturer_Capture_PostCaptureOriginStorageClearFailureIsBestEffort
// pins clearObservedOriginsStorage's best-effort contract: it runs AFTER
// the record this capture produced is already final (see its doc), so
// a Storage.clearDataForOrigin RPC failure for an observed resource's
// origin must only be logged, never turn an otherwise-successful
// capture into an error or block SaveItem.
func TestCapturer_Capture_PostCaptureOriginStorageClearFailureIsBestEffort(t *testing.T) {
	h := setupCapture(t)
	defer h.ctrl.Finish()

	h.mockHTTP.EXPECT().
		NewRequest(http.MethodGet, "https://example.com/index.html", nil).
		DoAndReturn(func(method, url string, body io.Reader) (*http.Request, error) {
			return http.NewRequest(method, url, body)
		}).Times(1)
	h.mockHTTP.EXPECT().Do(gomock.Any()).Return(&http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader("<html>art</html>")),
	}, nil).Times(1)

	go func() {
		h.answerDomainEnables(t)
		h.pushEvent(t, "Network.requestWillBeSent", map[string]interface{}{
			"requestId": "req-1",
			"request":   map[string]interface{}{"url": "https://example.com/index.html"},
		})
		h.pushEvent(t, "Network.responseReceived", map[string]interface{}{
			"requestId": "req-1",
			"response": map[string]interface{}{
				"url": "https://example.com/index.html", "status": 200, "mimeType": "text/html",
			},
		})

		// The post-capture clearObservedOriginsStorage call for this
		// item's single observed origin: reply with a CDP RPC error
		// (instead of ackEmpty) rather than a successful empty result.
		msg := h.conn.nextOutbound(t)
		assert.Equal(t, "Storage.clearDataForOrigin", msg["method"])
		reply, err := json.Marshal(map[string]interface{}{
			"id":    int64(msg["id"].(float64)),
			"error": map[string]interface{}{"code": -32000, "message": "simulated origin clear failure"},
		})
		require.NoError(t, err)
		h.conn.pushReply(reply)
	}()

	item := dp1playlist.PlaylistItem{ID: "item-1", Source: "https://example.com/index.html"}
	rec, err := h.capturer.Capture(context.Background(), item, 300)
	require.NoError(t, err, "a post-capture cleanup RPC failure must never fail the capture itself")

	require.Len(t, rec.Resources, 1)
	assert.Equal(t, "https://example.com/index.html", rec.Resources[0].URL)
	assert.True(t, rec.Coverage.Complete)

	saved, err := h.store.LoadItem("item-1")
	require.NoError(t, err, "the record must still be persisted despite the best-effort cleanup failure")
	assert.Equal(t, rec.Resources, saved.Resources)
}

// TestCapturer_Capture_ClearsBrowserCacheAndOriginStorageBeforeNavigate
// is the regression test for resetTargetState: the headless downloader
// runs one long-lived Chromium process whose page target and
// --user-data-dir are reused across every capture job (see
// downloader.go's doc), so without an explicit reset a Service Worker,
// IndexedDB entry, or Cache Storage entry left behind by a PRIOR item
// sharing this item's origin could intercept or seed this item's
// requests without any of them ever reaching the network — silently
// hiding resources resolveResources needs to see. Pins both the
// unconditional cache flush and the origin-scoped storage clear, in
// that order, before Page.navigate.
func TestCapturer_Capture_ClearsBrowserCacheAndOriginStorageBeforeNavigate(t *testing.T) {
	h := setupCapture(t)
	defer h.ctrl.Finish()

	item := dp1playlist.PlaylistItem{ID: "item-1", Source: "https://cdn.example.com:8443/index.html?x=1"}

	go func() {
		for i := 0; i < domainEnableCallCount; i++ {
			msg := h.conn.nextOutbound(t)
			switch i {
			case 0:
				assert.Equal(t, "Network.enable", msg["method"])
			case 1:
				assert.Equal(t, "Page.enable", msg["method"])
			case 2:
				assert.Equal(t, "Network.clearBrowserCache", msg["method"])
			case 3:
				assert.Equal(t, "Storage.clearDataForOrigin", msg["method"])
				params, ok := msg["params"].(map[string]interface{})
				require.True(t, ok)
				assert.Equal(t, "https://cdn.example.com:8443", params["origin"],
					"must scope the clear to the item's own origin, not the whole host or a bare domain")
				assert.Equal(t, "all", params["storageTypes"])
			case 4:
				assert.Equal(t, "Page.navigate", msg["method"],
					"the cache/storage reset must complete before navigation starts")
			}
			h.ackEmpty(t, msg)
		}
	}()

	_, err := h.capturer.Capture(context.Background(), item, 50)
	require.NoError(t, err)
}

// TestCapturer_Capture_UnparsableSourceSkipsOriginClearButStillClearsCache
// pins resetTargetState's fallback: an origin that cannot be derived
// from item.Source must not abort the whole capture (Page.navigate is
// about to fail on the same malformed URL moments later with a clearer
// error anyway) — only the origin-scoped Storage.clearDataForOrigin
// call is skipped, while the unconditional Network.clearBrowserCache
// call still happens.
func TestCapturer_Capture_UnparsableSourceSkipsOriginClearButStillClearsCache(t *testing.T) {
	h := setupCapture(t)
	defer h.ctrl.Finish()

	item := dp1playlist.PlaylistItem{ID: "item-1", Source: "not-a-valid-origin-url"}

	go func() {
		msg := h.conn.nextOutbound(t)
		assert.Equal(t, "Network.enable", msg["method"])
		h.ackEmpty(t, msg)

		msg = h.conn.nextOutbound(t)
		assert.Equal(t, "Page.enable", msg["method"])
		h.ackEmpty(t, msg)

		msg = h.conn.nextOutbound(t)
		assert.Equal(t, "Network.clearBrowserCache", msg["method"])
		h.ackEmpty(t, msg)

		// No Storage.clearDataForOrigin call: "not-a-valid-origin-url"
		// has no scheme/host, so requestOrigin fails and resetTargetState
		// skips straight to letting Page.navigate itself surface the bad
		// URL.
		msg = h.conn.nextOutbound(t)
		assert.Equal(t, "Page.navigate", msg["method"])
		h.ackEmpty(t, msg)
	}()

	_, err := h.capturer.Capture(context.Background(), item, 50)
	require.NoError(t, err)
}

// TestCapturer_Capture_FiltersResponseHeadersToReplayableAllowlist pins
// two things at once: a cross-origin resource's CORS-relevant headers
// (present here in a mix of casings, since origin servers are free to
// send any) are captured onto Resource.Headers, canonicalized; and a
// header outside replayableResponseHeaders (Set-Cookie) is dropped
// rather than persisted to disk.
func TestCapturer_Capture_FiltersResponseHeadersToReplayableAllowlist(t *testing.T) {
	h := setupCapture(t)
	defer h.ctrl.Finish()

	h.mockHTTP.EXPECT().
		NewRequest(http.MethodGet, "https://cdn.example.com/module.js", nil).
		DoAndReturn(func(method, url string, body io.Reader) (*http.Request, error) {
			return http.NewRequest(method, url, body)
		}).Times(1)
	h.mockHTTP.EXPECT().Do(gomock.Any()).Return(&http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader("export default 1;")),
	}, nil).Times(1)

	go func() {
		h.answerDomainEnables(t)
		h.pushEvent(t, "Network.requestWillBeSent", map[string]interface{}{
			"requestId": "req-1",
			"request":   map[string]interface{}{"url": "https://cdn.example.com/module.js"},
		})
		h.pushEvent(t, "Network.responseReceived", map[string]interface{}{
			"requestId": "req-1",
			"response": map[string]interface{}{
				"url": "https://cdn.example.com/module.js", "status": 200, "mimeType": "application/javascript",
				"headers": map[string]interface{}{
					"access-control-allow-origin": "https://example.com", // lowercase, as some origins send it
					"Timing-Allow-Origin":         "*",
					"Set-Cookie":                  "session=abc", // must never be captured/persisted
					"Content-Length":              "18",          // transport header, must not be captured either
				},
			},
		})
		h.drainAndAckRemaining(t)
	}()

	item := dp1playlist.PlaylistItem{ID: "item-cors", Source: "https://cdn.example.com/module.js"}
	rec, err := h.capturer.Capture(context.Background(), item, 300)
	require.NoError(t, err)

	require.Len(t, rec.Resources, 1)
	res := rec.Resources[0]
	assert.Equal(t, map[string]string{
		"Access-Control-Allow-Origin": "https://example.com",
		"Timing-Allow-Origin":         "*",
	}, res.Headers)
}

// TestCapturer_Capture_NonRedirectErrorStatusMarksIncomplete pins that a
// 4xx/5xx response the page's own request observed live is not silently
// recorded as if it were a normal successful resource: without a stored
// body, replay would treat it as an honest miss (see replay.go's
// onRequestPaused default case), so Coverage must say so too rather than
// reporting Complete=true for a resource that can never faithfully
// replay as this status offline.
func TestCapturer_Capture_NonRedirectErrorStatusMarksIncomplete(t *testing.T) {
	h := setupCapture(t)
	defer h.ctrl.Finish()
	// No NewRequest/Do expectations: a non-2xx, non-redirect resource must
	// never trigger fetchAndStoreBody's re-fetch.

	go func() {
		h.answerDomainEnables(t)
		h.pushEvent(t, "Network.requestWillBeSent", map[string]interface{}{
			"requestId": "req-1",
			"request":   map[string]interface{}{"url": "https://example.com/missing.js"},
		})
		h.pushEvent(t, "Network.responseReceived", map[string]interface{}{
			"requestId": "req-1",
			"response": map[string]interface{}{
				"url": "https://example.com/missing.js", "status": 404, "mimeType": "text/plain",
			},
		})
		h.drainAndAckRemaining(t)
	}()

	item := dp1playlist.PlaylistItem{ID: "item-404", Source: "https://example.com/missing.js"}
	rec, err := h.capturer.Capture(context.Background(), item, 300)
	require.NoError(t, err)

	require.Len(t, rec.Resources, 1)
	res := rec.Resources[0]
	assert.Equal(t, 404, res.Status)
	assert.Empty(t, res.SHA256)
	assert.False(t, rec.Coverage.Complete)
	assert.Contains(t, rec.Coverage.Reason, "http_error(404):https://example.com/missing.js")
}

// TestCapturer_Capture_UnsafeMethodSuccessMarksIncompleteWithoutRefetching
// pins two things at once: capture never re-issues a POST/PUT/DELETE/...
// request merely to read its bytes (that would risk re-triggering the
// exact side effect — a mutation, an analytics/provenance call — the
// original request caused), and it does not silently claim
// Coverage.Complete=true for a resource it chose not to fetch.
func TestCapturer_Capture_UnsafeMethodSuccessMarksIncompleteWithoutRefetching(t *testing.T) {
	h := setupCapture(t)
	defer h.ctrl.Finish()
	// No NewRequest/Do expectations for the POST URL: gomock's strict
	// mode will fail the test if fetchAndStoreBody is called for it.

	go func() {
		h.answerDomainEnables(t)
		h.pushEvent(t, "Network.requestWillBeSent", map[string]interface{}{
			"requestId": "req-1",
			"request":   map[string]interface{}{"url": "https://example.com/api/report", "method": "POST"},
		})
		h.pushEvent(t, "Network.responseReceived", map[string]interface{}{
			"requestId": "req-1",
			"response": map[string]interface{}{
				"url": "https://example.com/api/report", "status": 200, "mimeType": "application/json",
			},
		})
		h.drainAndAckRemaining(t)
	}()

	item := dp1playlist.PlaylistItem{ID: "item-post", Source: "https://example.com/api/report"}
	rec, err := h.capturer.Capture(context.Background(), item, 300)
	require.NoError(t, err)

	require.Len(t, rec.Resources, 1)
	res := rec.Resources[0]
	assert.Equal(t, "POST", res.Method)
	assert.Equal(t, 200, res.Status)
	assert.Empty(t, res.SHA256, "an unsafe method's bytes must never be fetched")
	assert.False(t, rec.Coverage.Complete)
	assert.Contains(t, rec.Coverage.Reason, "unsupported_method(POST):https://example.com/api/report")
}

// TestCapturer_Capture_CORSPreflightAndActualRequestDoNotCollide pins
// that an OPTIONS CORS preflight and its paired actual (non-GET) request
// to the identical URL are captured as two distinct Resource entries
// (keyed by method, not URL alone) rather than one clobbering the other
// — see Resource.Method's doc for the mis-replay this closes. It also
// pins that the preflight's SHA256 is populated WITHOUT ever calling
// the mocked HTTPClient (no NewRequest/Do expectations set for OPTIONS
// below) — see resolveResources' OPTIONS case for why capture stores a
// canonical empty body directly instead of re-issuing the preflight
// over the network.
func TestCapturer_Capture_CORSPreflightAndActualRequestDoNotCollide(t *testing.T) {
	h := setupCapture(t)
	defer h.ctrl.Finish()

	go func() {
		h.answerDomainEnables(t)
		h.pushEvent(t, "Network.requestWillBeSent", map[string]interface{}{
			"requestId": "req-preflight",
			"request":   map[string]interface{}{"url": "https://api.example.com/data", "method": "OPTIONS"},
		})
		h.pushEvent(t, "Network.responseReceived", map[string]interface{}{
			"requestId": "req-preflight",
			"response": map[string]interface{}{
				"url": "https://api.example.com/data", "status": 204,
			},
		})
		h.pushEvent(t, "Network.requestWillBeSent", map[string]interface{}{
			"requestId": "req-actual",
			"request":   map[string]interface{}{"url": "https://api.example.com/data", "method": "PUT"},
		})
		h.pushEvent(t, "Network.responseReceived", map[string]interface{}{
			"requestId": "req-actual",
			"response": map[string]interface{}{
				"url": "https://api.example.com/data", "status": 200, "mimeType": "application/json",
			},
		})
		h.drainAndAckRemaining(t)
	}()

	item := dp1playlist.PlaylistItem{ID: "item-cors-preflight", Source: "https://api.example.com/data"}
	rec, err := h.capturer.Capture(context.Background(), item, 300)
	require.NoError(t, err)

	require.Len(t, rec.Resources, 2, "the preflight and the actual request must be two distinct entries")
	byMethod := map[string]offlinecache.Resource{}
	for _, r := range rec.Resources {
		byMethod[r.Method] = r
	}

	preflight := byMethod["OPTIONS"]
	assert.Equal(t, 204, preflight.Status)
	require.NotEmpty(t, preflight.SHA256, "a successful OPTIONS preflight must still get a storable (empty) body")
	blob, err := h.store.ReadBlob(preflight.SHA256)
	require.NoError(t, err)
	assert.Empty(t, blob, "the preflight's stored body must be the canonical empty body, never a network re-fetch")

	actual := byMethod["PUT"]
	assert.Equal(t, 200, actual.Status)
	assert.Empty(t, actual.SHA256, "PUT must never be re-fetched")

	assert.False(t, rec.Coverage.Complete)
	assert.Contains(t, rec.Coverage.Reason, "unsupported_method(PUT):https://api.example.com/data")
}

// TestCapturer_Capture_OPTIONSPreflightRequiringOriginHeaderStillCaptures
// is the regression test for the hazard resolveResources' OPTIONS case
// exists to avoid: a real CORS-aware origin server commonly only
// returns its 2xx/Access-Control-Allow-* preflight response when the
// REQUEST carries Origin and Access-Control-Request-Method (Chromium
// adds these itself when issuing a real preflight; a bare re-issued
// OPTIONS from controld's own daemon HTTP client would not). This test
// does not, and cannot, simulate "the origin would reject a header-less
// OPTIONS" directly against a real server — instead it pins the actual
// fix: setupCapture's mockHTTP has NO NewRequest/Do expectation
// configured at all here, so gomock's strict controller would fail
// this test the moment capture tried to re-issue ANY HTTP request for
// the preflight. A capture that instead succeeds, with a populated
// SHA256, proves the OPTIONS resource was satisfied entirely from the
// live-observed status/headers — never a second, potentially
// differently-shaped round-trip that could get a different answer than
// the browser's own preflight did.
func TestCapturer_Capture_OPTIONSPreflightRequiringOriginHeaderStillCaptures(t *testing.T) {
	h := setupCapture(t)
	defer h.ctrl.Finish()

	go func() {
		h.answerDomainEnables(t)
		h.pushEvent(t, "Network.requestWillBeSent", map[string]interface{}{
			"requestId": "req-preflight",
			"request":   map[string]interface{}{"url": "https://strict-cors.example.com/api", "method": "OPTIONS"},
		})
		h.pushEvent(t, "Network.responseReceived", map[string]interface{}{
			"requestId": "req-preflight",
			"response": map[string]interface{}{
				"url":    "https://strict-cors.example.com/api",
				"status": 200,
				"headers": map[string]interface{}{
					// The real preflight, carrying the browser's own
					// Origin/Access-Control-Request-Method, got this
					// Allow-Origin header live. A header-less re-fetch
					// against a strict server could easily get a 403 or
					// a response missing this header entirely — but
					// resolveResources must never even attempt that
					// re-fetch for OPTIONS, so this captured value is
					// what replay serves regardless.
					"Access-Control-Allow-Origin": "https://gallery.example.com",
				},
			},
		})
		h.drainAndAckRemaining(t)
	}()

	item := dp1playlist.PlaylistItem{ID: "item-strict-cors", Source: "https://strict-cors.example.com/api"}
	rec, err := h.capturer.Capture(context.Background(), item, 300)
	require.NoError(t, err)

	require.Len(t, rec.Resources, 1)
	res := rec.Resources[0]
	assert.Equal(t, "OPTIONS", res.Method)
	assert.Equal(t, 200, res.Status)
	require.NotEmpty(t, res.SHA256, "a strict-CORS preflight must still be captured without ever re-issuing the request")
	assert.Equal(t, "https://gallery.example.com", res.Headers["Access-Control-Allow-Origin"],
		"replay must serve the header the browser's OWN preflight observed live, not one from a re-fetch")
	assert.True(t, rec.Coverage.Complete)
}

func TestCapturer_Capture_RedirectChain(t *testing.T) {
	h := setupCapture(t)
	defer h.ctrl.Finish()

	h.mockHTTP.EXPECT().
		NewRequest(http.MethodGet, "https://example.com/lib@2.0/lib.min.js", nil).
		DoAndReturn(func(method, url string, body io.Reader) (*http.Request, error) {
			return http.NewRequest(method, url, body)
		}).Times(1)
	h.mockHTTP.EXPECT().Do(gomock.Any()).Return(&http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader("console.log(1)")),
	}, nil).Times(1)

	go func() {
		h.answerDomainEnables(t)
		h.pushEvent(t, "Network.requestWillBeSent", map[string]interface{}{
			"requestId": "req-1",
			"request":   map[string]interface{}{"url": "https://example.com/lib.min.js"},
		})
		h.pushEvent(t, "Network.requestWillBeSent", map[string]interface{}{
			"requestId": "req-1",
			"request":   map[string]interface{}{"url": "https://example.com/lib@2.0/lib.min.js"},
			"redirectResponse": map[string]interface{}{
				"url": "https://example.com/lib.min.js", "status": 302,
			},
		})
		h.pushEvent(t, "Network.responseReceived", map[string]interface{}{
			"requestId": "req-1",
			"response": map[string]interface{}{
				"url": "https://example.com/lib@2.0/lib.min.js", "status": 200, "mimeType": "application/javascript",
			},
		})
		h.drainAndAckRemaining(t)
	}()

	item := dp1playlist.PlaylistItem{ID: "item-redirect", Source: "https://example.com/lib.min.js"}
	rec, err := h.capturer.Capture(context.Background(), item, 300)
	require.NoError(t, err)

	require.Len(t, rec.Resources, 2)
	byURL := map[string]offlinecache.Resource{}
	for _, r := range rec.Resources {
		byURL[r.URL] = r
	}

	redirect := byURL["https://example.com/lib.min.js"]
	assert.Equal(t, 302, redirect.Status)
	assert.Equal(t, "https://example.com/lib@2.0/lib.min.js", redirect.RedirectTo)
	assert.True(t, redirect.IsRedirect())
	assert.Empty(t, redirect.SHA256, "a redirect hop has no body of its own")

	final := byURL["https://example.com/lib@2.0/lib.min.js"]
	assert.Equal(t, 200, final.Status)
	require.NotEmpty(t, final.SHA256)
}

// TestCapturer_Capture_RedirectMethodChangeAttributesOriginalHopMethod
// pins that the redirected-from hop's Resource.Method reflects the
// method of ITS OWN requestWillBeSent event (the original request),
// never the post-redirect hop's method — even when a redirect changes
// the method (e.g. a 303 turning a POST into a GET), which is exactly
// the ordering hazard attachHandlers' Network.requestWillBeSent doc
// comment calls out: methodForRequest must be read for a requestId
// BEFORE that same event's trackRequest call overwrites it.
func TestCapturer_Capture_RedirectMethodChangeAttributesOriginalHopMethod(t *testing.T) {
	h := setupCapture(t)
	defer h.ctrl.Finish()

	h.mockHTTP.EXPECT().
		NewRequest(http.MethodGet, "https://example.com/result", nil).
		DoAndReturn(func(method, url string, body io.Reader) (*http.Request, error) {
			return http.NewRequest(method, url, body)
		}).Times(1)
	h.mockHTTP.EXPECT().Do(gomock.Any()).Return(&http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader("ok")),
	}, nil).Times(1)

	go func() {
		h.answerDomainEnables(t)
		h.pushEvent(t, "Network.requestWillBeSent", map[string]interface{}{
			"requestId": "req-1",
			"request":   map[string]interface{}{"url": "https://example.com/submit", "method": "POST"},
		})
		// The 303 hop: request.method here is the POST-REDIRECT method
		// (GET), which must not be mistaken for the redirected-from
		// hop's (POST) own method.
		h.pushEvent(t, "Network.requestWillBeSent", map[string]interface{}{
			"requestId": "req-1",
			"request":   map[string]interface{}{"url": "https://example.com/result", "method": "GET"},
			"redirectResponse": map[string]interface{}{
				"url": "https://example.com/submit", "status": 303,
			},
		})
		h.pushEvent(t, "Network.responseReceived", map[string]interface{}{
			"requestId": "req-1",
			"response": map[string]interface{}{
				"url": "https://example.com/result", "status": 200, "mimeType": "text/plain",
			},
		})
		h.drainAndAckRemaining(t)
	}()

	item := dp1playlist.PlaylistItem{ID: "item-redirect-method-change", Source: "https://example.com/submit"}
	rec, err := h.capturer.Capture(context.Background(), item, 300)
	require.NoError(t, err)

	require.Len(t, rec.Resources, 2)
	byURL := map[string]offlinecache.Resource{}
	for _, r := range rec.Resources {
		byURL[r.URL] = r
	}

	redirect := byURL["https://example.com/submit"]
	assert.Equal(t, 303, redirect.Status)
	assert.Equal(t, "https://example.com/result", redirect.RedirectTo)
	assert.Equal(t, "POST", redirect.Method,
		"the redirected-from hop's method must be its OWN original method, not the post-redirect hop's method")

	final := byURL["https://example.com/result"]
	assert.Equal(t, 200, final.Status)
	assert.Empty(t, final.Method, "GET is the empty-string convention — see Resource.Method's doc")
	require.NotEmpty(t, final.SHA256)
}

func TestCapturer_Capture_LoadingFailedMarksIncomplete(t *testing.T) {
	h := setupCapture(t)
	defer h.ctrl.Finish()

	go func() {
		h.answerDomainEnables(t)
		h.pushEvent(t, "Network.requestWillBeSent", map[string]interface{}{
			"requestId": "req-1",
			"request":   map[string]interface{}{"url": "https://example.com/broken.js"},
		})
		h.pushEvent(t, "Network.loadingFailed", map[string]interface{}{
			"requestId": "req-1",
			"errorText": "net::ERR_CONNECTION_RESET",
		})
	}()

	item := dp1playlist.PlaylistItem{ID: "item-broken", Source: "https://example.com/broken.js"}
	rec, err := h.capturer.Capture(context.Background(), item, 300)
	require.NoError(t, err)

	assert.False(t, rec.Coverage.Complete)
	assert.Contains(t, rec.Coverage.Reason, "https://example.com/broken.js")
	assert.Contains(t, rec.Coverage.Reason, "net::ERR_CONNECTION_RESET")
}

// TestCapturer_Capture_UnresolvedRequestAtDeadlineMarksIncomplete is the
// regression test pinning that a request observed via requestWillBeSent
// but never reaching responseReceived/loadingFailed before the capture
// window closes must not vanish from the record entirely while
// Coverage.Complete still reports true.
func TestCapturer_Capture_UnresolvedRequestAtDeadlineMarksIncomplete(t *testing.T) {
	h := setupCapture(t)
	defer h.ctrl.Finish()

	go func() {
		h.answerDomainEnables(t)
		h.pushEvent(t, "Network.requestWillBeSent", map[string]interface{}{
			"requestId": "req-1",
			"request":   map[string]interface{}{"url": "https://example.com/still-loading.js"},
		})
		// Deliberately never send responseReceived/loadingFailed for
		// req-1: this reproduces a resource whose outcome the page
		// never observed before the capture window closed.
	}()

	item := dp1playlist.PlaylistItem{ID: "item-hang", Source: "https://example.com/index.html"}
	rec, err := h.capturer.Capture(context.Background(), item, 300)
	require.NoError(t, err)

	assert.False(t, rec.Coverage.Complete, "a request with no terminal event by the deadline must not report Complete=true")
	assert.Contains(t, rec.Coverage.Reason, "unresolved_at_deadline")
	assert.Contains(t, rec.Coverage.Reason, "https://example.com/still-loading.js")
	assert.Empty(t, rec.Resources, "an unresolved request has no Resource entry to include")
}

// TestCapturer_Capture_RedirectChainUnresolvedFinalHopMarksIncomplete pins
// that pending-tracking follows a redirect chain's requestId to its LATEST
// hop rather than being satisfied by an earlier hop's own recordResource
// call: the first hop gets a Resource entry (from redirectResponse) purely
// as a side effect of observing the redirect, which must not be mistaken
// for the chain as a whole having resolved.
func TestCapturer_Capture_RedirectChainUnresolvedFinalHopMarksIncomplete(t *testing.T) {
	h := setupCapture(t)
	defer h.ctrl.Finish()

	go func() {
		h.answerDomainEnables(t)
		h.pushEvent(t, "Network.requestWillBeSent", map[string]interface{}{
			"requestId": "req-1",
			"request":   map[string]interface{}{"url": "https://example.com/lib.min.js"},
		})
		h.pushEvent(t, "Network.requestWillBeSent", map[string]interface{}{
			"requestId": "req-1",
			"request":   map[string]interface{}{"url": "https://example.com/lib@2.0/lib.min.js"},
			"redirectResponse": map[string]interface{}{
				"url": "https://example.com/lib.min.js", "status": 302,
			},
		})
		// The final hop's own response is never observed: req-1 stays
		// pending on its latest (redirected-to) URL.
		h.drainAndAckRemaining(t)
	}()

	item := dp1playlist.PlaylistItem{ID: "item-redirect-hang", Source: "https://example.com/lib.min.js"}
	rec, err := h.capturer.Capture(context.Background(), item, 300)
	require.NoError(t, err)

	assert.False(t, rec.Coverage.Complete)
	assert.Contains(t, rec.Coverage.Reason, "unresolved_at_deadline")
	assert.Contains(t, rec.Coverage.Reason, "https://example.com/lib@2.0/lib.min.js")
	require.Len(t, rec.Resources, 1, "the redirect hop itself was observed and recorded")
	assert.True(t, rec.Resources[0].IsRedirect())
}

func TestCapturer_Capture_CSPBlockedReason(t *testing.T) {
	h := setupCapture(t)
	defer h.ctrl.Finish()

	go func() {
		h.answerDomainEnables(t)
		h.pushEvent(t, "Network.requestWillBeSent", map[string]interface{}{
			"requestId": "req-1",
			"request":   map[string]interface{}{"url": "https://cdn.example.com/p5.min.js"},
		})
		h.pushEvent(t, "Network.loadingFailed", map[string]interface{}{
			"requestId":     "req-1",
			"errorText":     "net::ERR_BLOCKED_BY_CSP",
			"blockedReason": "csp",
		})
	}()

	item := dp1playlist.PlaylistItem{ID: "item-csp", Source: "https://example.com/index.html"}
	rec, err := h.capturer.Capture(context.Background(), item, 300)
	require.NoError(t, err)

	assert.False(t, rec.Coverage.Complete)
	assert.Contains(t, rec.Coverage.Reason, offlinecache.ReasonCSPBlocked)
}

func TestCapturer_Capture_IgnoresBlobAndDataURLs(t *testing.T) {
	h := setupCapture(t)
	defer h.ctrl.Finish()

	h.mockHTTP.EXPECT().
		NewRequest(http.MethodGet, "https://example.com/index.html", nil).
		DoAndReturn(func(method, url string, body io.Reader) (*http.Request, error) {
			return http.NewRequest(method, url, body)
		}).Times(1)
	h.mockHTTP.EXPECT().Do(gomock.Any()).Return(&http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader("html")),
	}, nil).Times(1)

	go func() {
		h.answerDomainEnables(t)
		h.pushEvent(t, "Network.requestWillBeSent", map[string]interface{}{
			"requestId": "req-1",
			"request":   map[string]interface{}{"url": "https://example.com/index.html"},
		})
		h.pushEvent(t, "Network.responseReceived", map[string]interface{}{
			"requestId": "req-1",
			"response": map[string]interface{}{
				"url": "https://example.com/index.html", "status": 200, "mimeType": "text/html",
			},
		})
		h.pushEvent(t, "Network.requestWillBeSent", map[string]interface{}{
			"requestId": "req-2",
			"request":   map[string]interface{}{"url": "blob:https://example.com/abcd-1234"},
		})
		h.pushEvent(t, "Network.responseReceived", map[string]interface{}{
			"requestId": "req-2",
			"response": map[string]interface{}{
				"url": "blob:https://example.com/abcd-1234", "status": 200, "mimeType": "application/octet-stream",
			},
		})
		h.drainAndAckRemaining(t)
	}()

	item := dp1playlist.PlaylistItem{ID: "item-blob", Source: "https://example.com/index.html"}
	rec, err := h.capturer.Capture(context.Background(), item, 300)
	require.NoError(t, err)

	require.Len(t, rec.Resources, 1, "blob: URLs must never be recorded as resources")
	assert.Equal(t, "https://example.com/index.html", rec.Resources[0].URL)
}

func TestCapturer_Capture_RequiresIDAndSource(t *testing.T) {
	store, _ := newTestStore(t)
	capturer := offlinecache.NewCapturer(nil, nil, nil, store, wrapper.NewJSON(), wrapper.NewIO(), wrapper.NewClock(), 0, zaptest.NewLogger(t))

	_, err := capturer.Capture(context.Background(), dp1playlist.PlaylistItem{}, 0)
	assert.Error(t, err)
}

func TestCapturer_Capture_AcquireFails(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockDownloader := mocks.NewMockOfflineCacheDownloader(ctrl)
	mockDownloader.EXPECT().Acquire(gomock.Any()).Return("", assertError("busy")).Times(1)

	store, _ := newTestStore(t)
	capturer := offlinecache.NewCapturer(mockDownloader, nil, nil, store, wrapper.NewJSON(), wrapper.NewIO(), wrapper.NewClock(), 0, zaptest.NewLogger(t))

	item := dp1playlist.PlaylistItem{ID: "item-1", Source: "https://example.com/index.html"}
	_, err := capturer.Capture(context.Background(), item, 0)
	assert.Error(t, err)
}

// TestCapturer_Capture_ParentCancellationAfterNavigateAbortsWithoutSaving
// is the end-to-end, black-box counterpart to
// TestWaitForObservationWindow_CtxCancellationWinsRegardlessOfSelectBranch
// in capture_wedge_test.go: it drives a real Capture call through a
// cancellation racing the post-navigate observation wait and asserts the
// full-stack outcome (error returned, nothing saved). Real goroutine
// scheduling makes it very unlikely this alone lands in the exact
// ambiguous-select window on any given run (see that whitebox test's
// doc for why only two independently-already-canceled contexts can
// force it deterministically) — this test instead pins the black-box
// contract "cancellation racing the observation window must never save
// a partial record," repeated for some general race coverage.
func TestCapturer_Capture_ParentCancellationAfterNavigateAbortsWithoutSaving(t *testing.T) {
	const iterations = 50
	for i := 0; i < iterations; i++ {
		h := setupCapture(t)

		navigateAcked := make(chan struct{})
		ctx, cancel := context.WithCancel(context.Background())

		go func() {
			h.answerDomainEnables(t) // acks Network.enable, Page.enable, Page.navigate
			close(navigateAcked)
			// Deliberately never send any Network.* events: nothing
			// about this item resolves before cancellation.
		}()

		go func() {
			<-navigateAcked
			// No synchronization delay here on purpose: Capture falls
			// straight from the navigate Send returning into the
			// target select with no intervening work, so the race
			// under test (navCtx.Done() and ctx.Done() becoming ready
			// together) is only actually reachable if cancel() is
			// called close to when Capture reaches the select, not
			// once it is already safely parked inside it (a
			// deliberate delay here would make the ctx.Done() branch
			// win deterministically and defeat the point of this
			// test — see the racecheck this was validated against).
			cancel()
		}()

		// A large window ensures navCtx's own deadline is never what
		// ends the select; only the explicit cancel above can.
		item := dp1playlist.PlaylistItem{ID: "item-cancel", Source: "https://example.com/index.html"}
		rec, err := h.capturer.Capture(ctx, item, 60_000)

		assert.Nil(t, rec, "iteration %d: a canceled capture must not return a record", i)
		assert.ErrorIs(t, err, context.Canceled, "iteration %d", i)

		_, loadErr := h.store.LoadItem("item-cancel")
		assert.ErrorIs(t, loadErr, offlinecache.ErrItemNotFound,
			"iteration %d: a canceled capture must not save a partial/incomplete ItemRecord", i)

		h.ctrl.Finish()
	}
}

func TestCapturer_Close_DelegatesToDownloader(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockDownloader := mocks.NewMockOfflineCacheDownloader(ctrl)
	mockDownloader.EXPECT().Close().Return(nil).Times(1)

	store, _ := newTestStore(t)
	capturer := offlinecache.NewCapturer(mockDownloader, nil, nil, store, wrapper.NewJSON(), wrapper.NewIO(), wrapper.NewClock(), 0, zaptest.NewLogger(t))

	assert.NoError(t, capturer.Close())
}

func TestCapturer_Capture_UsesDefaultWindowWhenUnset(t *testing.T) {
	// A 0ms window must fall back to captureWindowDefault rather than
	// returning immediately with no observation at all.
	h := setupCapture(t)
	defer h.ctrl.Finish()

	go h.answerDomainEnables(t)

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	item := dp1playlist.PlaylistItem{ID: "item-1", Source: "https://example.com/index.html"}
	_, err := h.capturer.Capture(ctx, item, 0)
	// The observation window (captureWindowDefault, 20s) will not have
	// elapsed within this test's short outer deadline, so Capture must
	// return the context's deadline error rather than a default-window
	// completion — this proves 0 did not silently become "no window".
	assert.ErrorIs(t, err, context.DeadlineExceeded)
}
