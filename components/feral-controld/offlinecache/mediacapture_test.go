package offlinecache_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
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

	capturer := offlinecache.NewMediaCapturer(mockHTTP, store, wrapper.NewJSON(), wrapper.NewClock(), maxDiskBytes, logger)

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
// TestMediaCapturer_Capture_GLTFManifest covers the one case where this
// single-file path downgrades its own coverage: a JSON .gltf manifest
// whose spec-defined buffers[].uri/images[].uri point at separate
// external files that are NOT captured (see gltfExternalDependencies'
// doc). Reporting such an item Complete (and therefore `ready`) would
// tell the controller a fully-cached item exists while fail-closed
// replay fails its buffer/texture requests offline. Self-contained
// manifests (data: URIs only), binary .glb, and non-glTF media must all
// keep Complete=true.
func TestMediaCapturer_Capture_GLTFManifest(t *testing.T) {
	const externalDeps = `{"buffers":[{"uri":"scene.bin"}],"images":[{"uri":"https://cdn.example.com/tex.png"},{"uri":"data:image/png;base64,AAAA"}]}`
	const selfContained = `{"buffers":[{"uri":"data:application/octet-stream;base64,AAAA"}],"images":[{"uri":"data:image/png;base64,AAAA"}]}`

	tests := []struct {
		name         string
		source       string
		contentType  string
		body         string
		wantComplete bool
		wantReason   string
	}{
		{
			name:         "gltf content-type with external buffer and image is partial",
			source:       "https://example.com/scene",
			contentType:  "model/gltf+json",
			body:         externalDeps,
			wantComplete: false,
			wantReason:   "gltf_external_dependency:scene.bin; gltf_external_dependency:https://cdn.example.com/tex.png",
		},
		{
			name: "gltf extension under a generic content-type is still checked",
			// CDNs commonly lose the model/gltf+json type — the URL
			// extension fallback must catch this (see isGLTFManifest).
			source:       "https://example.com/scene.gltf",
			contentType:  "application/octet-stream",
			body:         externalDeps,
			wantComplete: false,
			wantReason:   "gltf_external_dependency:scene.bin; gltf_external_dependency:https://cdn.example.com/tex.png",
		},
		{
			name:         "self-contained gltf manifest stays complete",
			source:       "https://example.com/scene.gltf",
			contentType:  "model/gltf+json",
			body:         selfContained,
			wantComplete: true,
		},
		{
			name: "binary glb is never parsed and stays complete",
			// A .glb embeds its payload; the manifest check must not
			// even attempt to JSON-parse it (its body here would parse
			// as JSON and yield external deps if it were).
			source:       "https://example.com/scene.gltf",
			contentType:  "model/gltf-binary",
			body:         externalDeps,
			wantComplete: true,
		},
		{
			name: "unparseable gltf keeps complete coverage",
			// The checker's own limits must never invent a downgrade —
			// see gltfExternalDependencies' best-effort doc.
			source:       "https://example.com/scene.gltf",
			contentType:  "model/gltf+json",
			body:         "not json at all {",
			wantComplete: true,
		},
		{
			name:         "non-gltf media is untouched by the manifest check",
			source:       "https://example.com/photo.png",
			contentType:  "image/png",
			body:         "png bytes",
			wantComplete: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := setupMediaCapture(t)
			defer h.ctrl.Finish()

			item := dp1playlist.PlaylistItem{ID: "item-1", Source: tc.source}
			h.expectGET(t, item.Source, newMediaResponse(http.StatusOK, tc.contentType, tc.body, nil))

			rec, err := h.capturer.Capture(context.Background(), item)
			require.NoError(t, err, "the manifest check must only ever adjust coverage, never fail a successful download")
			require.NotNil(t, rec)

			assert.Equal(t, tc.wantComplete, rec.Coverage.Complete)
			assert.Equal(t, tc.wantReason, rec.Coverage.Reason)

			saved, err := h.store.LoadItem("item-1")
			require.NoError(t, err)
			assert.Equal(t, rec.Coverage, saved.Coverage, "the persisted record must carry the same coverage verdict")
		})
	}
}

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

// wholeRequestTimeoutClient is a wrapper.HTTPClient whose underlying
// http.Client carries a whole-request timeout — the SHAPE of the
// daemon-wide wrapper.NewHTTPClient this download path used to be wired
// with, where http.Client.Timeout covers the response BODY and not just
// the headers. The production value is 30s (wrapper.HTTPClientTimeout);
// this stand-in is measured in milliseconds so the regression below can
// be pinned in under a second instead of over half a minute.
type wholeRequestTimeoutClient struct{ client *http.Client }

func (c wholeRequestTimeoutClient) NewRequest(method, url string, body io.Reader) (*http.Request, error) {
	return http.NewRequest(method, url, body)
}
func (c wholeRequestTimeoutClient) Do(req *http.Request) (*http.Response, error) {
	//nolint:gosec // G704 flags the caller-supplied URL as SSRF taint. This is
	// a test double whose only callers are the httptest servers in this file.
	return c.client.Do(req)
}
func (c wholeRequestTimeoutClient) Get(url string) (*http.Response, error) {
	return c.client.Get(url)
}
func (c wholeRequestTimeoutClient) Post(url, contentType string, body io.Reader) (*http.Response, error) {
	return c.client.Post(url, contentType, body)
}

// tricklingServer serves a body in chunks separated by gap, flushing each
// one, so the response takes a predictable wall-clock time to finish
// while making steady progress throughout — a compressed stand-in for the
// large-asset-on-a-slow-uplink case this path exists to support. Returns
// the server and the exact payload it will serve.
func tricklingServer(t *testing.T, chunks int, chunkSize int, gap time.Duration) (*httptest.Server, string) {
	t.Helper()
	payload := strings.Repeat("x", chunks*chunkSize)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "video/mp4")
		w.WriteHeader(http.StatusOK)
		flusher, ok := w.(http.Flusher)
		require.True(t, ok, "httptest response writer must support flushing for a trickled body")
		for i := 0; i < chunks; i++ {
			if _, err := io.WriteString(w, payload[i*chunkSize:(i+1)*chunkSize]); err != nil {
				return // client hung up (cancellation test); stop trickling
			}
			flusher.Flush()
			select {
			case <-r.Context().Done():
				return
			case <-time.After(gap):
			}
		}
	}))
	t.Cleanup(srv.Close)
	return srv, payload
}

func newRealClientMediaCapturer(t *testing.T, client wrapper.HTTPClient, maxDiskBytes int64) (offlinecache.MediaCapturer, offlinecache.Store) {
	t.Helper()
	store, _ := newTestStore(t)
	return offlinecache.NewMediaCapturer(client, store, wrapper.NewJSON(), wrapper.NewClock(), maxDiskBytes, zaptest.NewLogger(t)), store
}

// TestMediaCapturer_Capture_SlowBodyOutlivesAWholeRequestTimeout is the
// regression test for the wiring bug where Bootstrap handed this path the
// daemon-wide 30s client: because http.Client.Timeout bounds the whole
// request INCLUDING the body, every asset that took longer than 30s to
// transfer failed unconditionally — which is most of what this subsystem
// exists to cache (the store budget is measured in GiB, and
// staticserver.go only matters above 200 MB). Both halves run against the
// same trickling server so the only variable is the client.
func TestMediaCapturer_Capture_SlowBodyOutlivesAWholeRequestTimeout(t *testing.T) {
	item := func(srv *httptest.Server) dp1playlist.PlaylistItem {
		return dp1playlist.PlaylistItem{ID: "item-1", Source: srv.URL + "/video.mp4"}
	}

	t.Run("a whole-request timeout kills a slow but healthy transfer", func(t *testing.T) {
		srv, _ := tricklingServer(t, 10, 64, 40*time.Millisecond) // ~400ms of steady progress
		capturer, store := newRealClientMediaCapturer(t,
			wholeRequestTimeoutClient{client: &http.Client{Timeout: 120 * time.Millisecond}}, 0)

		rec, err := capturer.Capture(context.Background(), item(srv))
		require.Error(t, err, "this is the pre-fix behavior being pinned, not a desired outcome")
		assert.Nil(t, rec)
		_, loadErr := store.LoadItem("item-1")
		assert.ErrorIs(t, loadErr, offlinecache.ErrItemNotFound)
	})

	t.Run("the timeout-free capture client completes it", func(t *testing.T) {
		srv, payload := tricklingServer(t, 10, 64, 40*time.Millisecond)
		capturer, store := newRealClientMediaCapturer(t, wrapper.NewHTTPClientWithoutTimeout(), 0)

		rec, err := capturer.Capture(context.Background(), item(srv))
		require.NoError(t, err)
		require.Len(t, rec.Resources, 1)

		// The whole asset, not a truncated prefix: a body cut off
		// mid-transfer must never be stored as if it were complete.
		blob, err := store.ReadBlob(rec.Resources[0].SHA256)
		require.NoError(t, err)
		assert.Equal(t, payload, string(blob))
		assert.True(t, rec.Coverage.Complete)
	})
}

// TestMediaCapturer_Capture_TimeoutFreeClientStillHonorsCancellation pins
// the other half of removing the client timeout: cancellation must still
// be the thing that stops a download, or "no whole-request timeout" would
// mean "unstoppable". mediaDownloadTimeout is the absolute ceiling on top
// of this; ctx cancellation must not have to wait for it.
func TestMediaCapturer_Capture_TimeoutFreeClientStillHonorsCancellation(t *testing.T) {
	// 200 chunks x 50ms = ~10s if it ever ran to completion, far longer
	// than the cancellation below allows.
	srv, _ := tricklingServer(t, 200, 64, 50*time.Millisecond)
	capturer, store := newRealClientMediaCapturer(t, wrapper.NewHTTPClientWithoutTimeout(), 0)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(150 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	rec, err := capturer.Capture(ctx, dp1playlist.PlaylistItem{ID: "item-1", Source: srv.URL + "/video.mp4"})
	elapsed := time.Since(start)

	require.Error(t, err)
	assert.Nil(t, rec)
	assert.Less(t, elapsed, 5*time.Second, "cancellation must abort the transfer promptly, not wait out the download ceiling")
	_, loadErr := store.LoadItem("item-1")
	assert.ErrorIs(t, loadErr, offlinecache.ErrItemNotFound, "a canceled download must leave no record behind")
}

// TestMediaCapturer_Capture_TimeoutFreeClientStillEnforcesDiskLimit is the
// third property removing the client timeout must not cost: the
// maxDiskBytes ceiling is enforced by WriteBlob's own cap, independently
// of any transport timeout, so a body that streams for a long time can
// still never exceed the store's remaining room.
func TestMediaCapturer_Capture_TimeoutFreeClientStillEnforcesDiskLimit(t *testing.T) {
	srv, payload := tricklingServer(t, 8, 64, 20*time.Millisecond)
	// A ceiling well under what the server will send (8*64 = 512 bytes).
	capturer, store := newRealClientMediaCapturer(t, wrapper.NewHTTPClientWithoutTimeout(), 128)

	rec, err := capturer.Capture(context.Background(), dp1playlist.PlaylistItem{ID: "item-1", Source: srv.URL + "/video.mp4"})
	require.Error(t, err, "a body over the remaining disk budget must fail, not be silently truncated")
	assert.Nil(t, rec)
	assert.Greater(t, len(payload), 128)

	_, loadErr := store.LoadItem("item-1")
	assert.ErrorIs(t, loadErr, offlinecache.ErrItemNotFound)
}
