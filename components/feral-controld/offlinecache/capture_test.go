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
		wrapper.NewJSON(), wrapper.NewIO(), wrapper.NewClock(), logger,
	)

	return &captureTestHarness{
		ctrl: ctrl, mockDownloader: mockDownloader, mockDialer: mockDialer,
		mockHTTP: mockHTTP, conn: conn, store: store, capturer: capturer,
	}
}

// answerDomainEnables drains and acks the Network.enable/Page.enable/
// Page.navigate calls capture.go always issues before the observation
// window begins.
func (h *captureTestHarness) answerDomainEnables(t *testing.T) {
	t.Helper()
	for i := 0; i < 3; i++ {
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
	}()

	item := dp1playlist.PlaylistItem{ID: "item-blob", Source: "https://example.com/index.html"}
	rec, err := h.capturer.Capture(context.Background(), item, 300)
	require.NoError(t, err)

	require.Len(t, rec.Resources, 1, "blob: URLs must never be recorded as resources")
	assert.Equal(t, "https://example.com/index.html", rec.Resources[0].URL)
}

func TestCapturer_Capture_RequiresIDAndSource(t *testing.T) {
	store, _ := newTestStore(t)
	capturer := offlinecache.NewCapturer(nil, nil, nil, store, wrapper.NewJSON(), wrapper.NewIO(), wrapper.NewClock(), zaptest.NewLogger(t))

	_, err := capturer.Capture(context.Background(), dp1playlist.PlaylistItem{}, 0)
	assert.Error(t, err)
}

func TestCapturer_Capture_AcquireFails(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockDownloader := mocks.NewMockOfflineCacheDownloader(ctrl)
	mockDownloader.EXPECT().Acquire(gomock.Any()).Return("", assertError("busy")).Times(1)

	store, _ := newTestStore(t)
	capturer := offlinecache.NewCapturer(mockDownloader, nil, nil, store, wrapper.NewJSON(), wrapper.NewIO(), wrapper.NewClock(), zaptest.NewLogger(t))

	item := dp1playlist.PlaylistItem{ID: "item-1", Source: "https://example.com/index.html"}
	_, err := capturer.Capture(context.Background(), item, 0)
	assert.Error(t, err)
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
