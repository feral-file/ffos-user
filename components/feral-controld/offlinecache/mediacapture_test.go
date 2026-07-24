package offlinecache_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"strings"
	"testing"

	dp1playlist "github.com/display-protocol/dp1-go/playlist"
	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"

	"github.com/feral-file/ffos-user/components/feral-controld/mocks"
	"github.com/feral-file/ffos-user/components/feral-controld/offlinecache"
	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

// mediaCaptureTestHarness wires a MediaCapturer against a mocked
// HTTPClient (the only network seam this browser-free path has) plus a
// real fsStore, matching capture_test.go's convention of exercising the
// real store rather than mocking every ReadDir/WriteFile call.
type mediaCaptureTestHarness struct {
	ctrl     *gomock.Controller
	mockHTTP *mocks.MockHTTPClient
	store    offlinecache.Store
	capturer offlinecache.MediaCapturer
}

func setupMediaCapture(t *testing.T) *mediaCaptureTestHarness {
	return setupMediaCaptureWithMaxDiskBytes(t, 0)
}

func setupMediaCaptureWithMaxDiskBytes(t *testing.T, maxDiskBytes int64) *mediaCaptureTestHarness {
	t.Helper()
	ctrl := gomock.NewController(t)
	mockHTTP := mocks.NewMockHTTPClient(ctrl)
	store, _ := newTestStore(t)
	logger := zaptest.NewLogger(t)

	capturer := offlinecache.NewMediaCapturer(mockHTTP, store, wrapper.NewClock(), maxDiskBytes, logger)

	return &mediaCaptureTestHarness{ctrl: ctrl, mockHTTP: mockHTTP, store: store, capturer: capturer}
}

// expectGET sets up the one HTTP round trip mediaCapturer.Capture ever
// makes: a plain GET, no Range header, matching fetchResource's doc for
// why redirects are followed transparently by httpClient itself rather
// than handled here.
func (h *mediaCaptureTestHarness) expectGET(t *testing.T, url string, resp *http.Response) *gomock.Call {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	require.NoError(t, err)
	h.mockHTTP.EXPECT().NewRequest(http.MethodGet, url, nil).Return(req, nil).Times(1)
	return h.mockHTTP.EXPECT().Do(gomock.Any()).Return(resp, nil).Times(1)
}

// expectGETCapturingRequest is expectGET's counterpart for tests that need
// to inspect the *http.Request actually handed to httpClient.Do — e.g. to
// assert on headers fetchResource sets after NewRequest returns, which a
// fixed expected-request value (as expectGET uses) can't observe.
func (h *mediaCaptureTestHarness) expectGETCapturingRequest(
	t *testing.T, url string, resp *http.Response,
) *[]*http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	require.NoError(t, err)
	h.mockHTTP.EXPECT().NewRequest(http.MethodGet, url, nil).Return(req, nil).Times(1)
	captured := make([]*http.Request, 0, 1)
	h.mockHTTP.EXPECT().Do(gomock.Any()).DoAndReturn(func(r *http.Request) (*http.Response, error) {
		captured = append(captured, r)
		return resp, nil
	}).Times(1)
	return &captured
}

func newMediaResponse(statusCode int, contentType, body string, extraHeaders map[string]string) *http.Response {
	h := http.Header{}
	if contentType != "" {
		h.Set("Content-Type", contentType)
	}
	for k, v := range extraHeaders {
		h.Set(k, v)
	}
	return &http.Response{
		StatusCode: statusCode,
		Header:     h,
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func TestMediaCapturer_Capture_Success(t *testing.T) {
	h := setupMediaCapture(t)
	defer h.ctrl.Finish()

	item := dp1playlist.PlaylistItem{ID: "item-1", Source: "https://example.com/video.mp4"}
	const body = "fake video bytes"
	h.expectGET(t, item.Source, newMediaResponse(http.StatusOK, "video/mp4", body, map[string]string{
		"Access-Control-Allow-Origin": "*",
		// Set-Cookie must never be persisted/replayed — see
		// replayableResponseHeaders' doc; included here to pin that
		// filterReplayableHeaders drops it just like capture.go's path
		// does, not merely that mediaCapture.go happens not to send it.
		"Set-Cookie": "sid=abc123",
	}))

	rec, err := h.capturer.Capture(context.Background(), item)
	require.NoError(t, err)
	require.NotNil(t, rec)

	assert.Equal(t, "item-1", rec.ItemID)
	assert.Equal(t, item, rec.Item)
	assert.Equal(t, item.Source, rec.Entry)
	assert.True(t, rec.Coverage.Complete)
	require.Len(t, rec.Resources, 1)

	res := rec.Resources[0]
	assert.Equal(t, item.Source, res.URL, "the resource must be keyed on the ORIGINAL source URL, never a resolved redirect target")
	assert.Equal(t, http.StatusOK, res.Status)
	assert.Equal(t, "video/mp4", res.ContentType)
	assert.Equal(t, sha256Hex(body), res.SHA256)
	assert.Equal(t, map[string]string{"Access-Control-Allow-Origin": "*"}, res.Headers,
		"only the CORS allowlist must survive; Set-Cookie must never be persisted")

	blob, err := h.store.ReadBlob(res.SHA256)
	require.NoError(t, err)
	assert.Equal(t, body, string(blob))

	saved, err := h.store.LoadItem("item-1")
	require.NoError(t, err)
	assert.Equal(t, rec.Resources, saved.Resources, "Capture must have persisted the record via SaveItem, not just returned it")
}

// TestMediaCapturer_Capture_SendsOriginHeader is the regression test for
// the CORS-capture gap this path used to have: ff-player's real
// <video crossOrigin="anonymous"> element always sends an Origin header,
// and CDN/S3-backed CORS configs commonly only emit
// Access-Control-Allow-Origin when a request carries one — a bare
// Origin-less GET (this path's previous behavior) could get a
// byte-identical response with those headers silently absent, leaving
// Resource.Headers empty and every offline replay of the asset failing
// Chromium's own CORS enforcement despite correct bytes. fetchResource
// must set Origin to the kiosk's own origin so captured headers match
// what the live player actually observes.
func TestMediaCapturer_Capture_SendsOriginHeader(t *testing.T) {
	h := setupMediaCapture(t)
	defer h.ctrl.Finish()

	item := dp1playlist.PlaylistItem{ID: "item-1", Source: "https://example.com/video.mp4"}
	captured := h.expectGETCapturingRequest(
		t, item.Source,
		newMediaResponse(http.StatusOK, "video/mp4", "bytes", map[string]string{
			"Access-Control-Allow-Origin": "*",
		}),
	)

	_, err := h.capturer.Capture(context.Background(), item)
	require.NoError(t, err)

	require.Len(t, *captured, 1)
	assert.Equal(t, "http://127.0.0.1:8080", (*captured)[0].Header.Get("Origin"),
		"must send the kiosk's own origin so CORS-conditional CDNs answer the same way they would for the real player")
}

// TestMediaCapturer_Capture_StoresUnderOriginalSourceEvenAfterRedirect
// pins that the saved Resource.URL is always item.Source, never
// whatever URL the (already redirect-resolved, from this call's point
// of view) *http.Response reports it landed on — see fetchResource's
// doc for why: the kiosk's native <img>/<video>/<audio> element only
// ever requests the bare item.Source, so that is the only URL replay
// ever needs to answer.
func TestMediaCapturer_Capture_StoresUnderOriginalSourceEvenAfterRedirect(t *testing.T) {
	h := setupMediaCapture(t)
	defer h.ctrl.Finish()

	item := dp1playlist.PlaylistItem{ID: "item-1", Source: "https://example.com/short-link/abc"}
	const body = "resolved bytes after following a redirect"
	resp := newMediaResponse(http.StatusOK, "image/png", body, nil)
	// http.Client resolves this internally; the Response Chromium's own
	// client.Do would hand back here already reflects the FINAL landed
	// request, which is deliberately a different URL than item.Source.
	resp.Request, _ = http.NewRequest(http.MethodGet, "https://cdn.example.com/resolved/abc.png", nil)
	h.expectGET(t, item.Source, resp)

	rec, err := h.capturer.Capture(context.Background(), item)
	require.NoError(t, err)
	require.Len(t, rec.Resources, 1)
	assert.Equal(t, item.Source, rec.Resources[0].URL)
}

func TestMediaCapturer_Capture_NonSuccessStatusReturnsError(t *testing.T) {
	h := setupMediaCapture(t)
	defer h.ctrl.Finish()

	item := dp1playlist.PlaylistItem{ID: "item-1", Source: "https://example.com/gone.jpg"}
	h.expectGET(t, item.Source, newMediaResponse(http.StatusNotFound, "text/plain", "not found", nil))

	rec, err := h.capturer.Capture(context.Background(), item)
	assert.Error(t, err)
	assert.Nil(t, rec)

	_, loadErr := h.store.LoadItem("item-1")
	assert.ErrorIs(t, loadErr, offlinecache.ErrItemNotFound, "a failed fetch must never leave a saved record behind")
}

func TestMediaCapturer_Capture_FetchErrorReturnsError(t *testing.T) {
	h := setupMediaCapture(t)
	defer h.ctrl.Finish()

	item := dp1playlist.PlaylistItem{ID: "item-1", Source: "https://example.com/video.mp4"}
	req, err := http.NewRequest(http.MethodGet, item.Source, nil)
	require.NoError(t, err)
	h.mockHTTP.EXPECT().NewRequest(http.MethodGet, item.Source, nil).Return(req, nil).Times(1)
	h.mockHTTP.EXPECT().Do(gomock.Any()).Return(nil, assertError("network unreachable")).Times(1)

	rec, err := h.capturer.Capture(context.Background(), item)
	assert.Error(t, err)
	assert.Nil(t, rec)
}

func TestMediaCapturer_Capture_RequiresIDAndSource(t *testing.T) {
	h := setupMediaCapture(t)
	defer h.ctrl.Finish()
	// No HTTP expectations at all: an invalid item must be rejected
	// before ever reaching the network.

	_, err := h.capturer.Capture(context.Background(), dp1playlist.PlaylistItem{})
	assert.Error(t, err)
}

// TestMediaCapturer_Capture_DiskBudgetExhaustedReturnsErrorWithoutFetch
// is the media-path counterpart to capture.go's disk-budget tests: once
// the store is already at/over maxDiskBytes, Capture must reject BEFORE
// ever issuing the GET — the mock has no NewRequest/Do expectation set,
// so gomock's strict controller fails this test outright if that
// ordering regresses.
func TestMediaCapturer_Capture_DiskBudgetExhaustedReturnsErrorWithoutFetch(t *testing.T) {
	h := setupMediaCaptureWithMaxDiskBytes(t, 5)
	defer h.ctrl.Finish()

	// Pre-existing usage (10 bytes) alone already exceeds the 5-byte
	// ceiling, so newDiskBudgetFromStore's remaining is clamped to 0
	// before this item's own fetch is ever attempted.
	writeBlobString(t, h.store, "0123456789")

	item := dp1playlist.PlaylistItem{ID: "item-1", Source: "https://example.com/video.mp4"}
	rec, err := h.capturer.Capture(context.Background(), item)
	assert.Error(t, err)
	assert.Nil(t, rec)
}

// TestMediaCapturer_Capture_RejectsBodyLargerThanRemainingBudget pins
// that a resource larger than the remaining room (capBytes from
// budget.reserve(), passed straight through to store.WriteBlob) is
// rejected outright — WriteBlob's own ErrBlobTooLarge guard (store.go)
// means an oversized body is never silently truncated and stored as if
// it were the whole asset; it is treated as a failed capture instead,
// consistent with capture.go's identical per-resource guarantee.
func TestMediaCapturer_Capture_RejectsBodyLargerThanRemainingBudget(t *testing.T) {
	h := setupMediaCaptureWithMaxDiskBytes(t, 5)
	defer h.ctrl.Finish()

	item := dp1playlist.PlaylistItem{ID: "item-1", Source: "https://example.com/video.mp4"}
	h.expectGET(t, item.Source, newMediaResponse(http.StatusOK, "video/mp4", "0123456789", nil))

	rec, err := h.capturer.Capture(context.Background(), item)
	assert.Error(t, err)
	assert.Nil(t, rec)

	_, loadErr := h.store.LoadItem("item-1")
	assert.ErrorIs(t, loadErr, offlinecache.ErrItemNotFound, "a rejected oversized fetch must never leave a saved record behind")
}
