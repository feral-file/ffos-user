package offlinecache_test

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"

	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/feral-file/ffos-user/components/feral-controld/mocks"
	"github.com/feral-file/ffos-user/components/feral-controld/offlinecache"
)

// publicResolver answers every name with a routable public address, so
// the classifier tests below exercise classification rather than the
// source guard. sourceguard_test.go covers the guard itself.
type publicResolver struct{}

func (publicResolver) LookupIPAddr(_ context.Context, _ string) ([]net.IPAddr, error) {
	return []net.IPAddr{{IP: net.ParseIP("93.184.216.34")}}, nil
}

func newTestResponse(statusCode int, contentType string) *http.Response {
	return &http.Response{
		StatusCode: statusCode,
		Header:     http.Header{"Content-Type": []string{contentType}},
		Body:       io.NopCloser(strings.NewReader("")),
	}
}

func TestClassifier_Classify(t *testing.T) {
	tests := []struct {
		name        string
		contentType string
		want        offlinecache.MediaClass
	}{
		{name: "html entry document", contentType: "text/html; charset=utf-8", want: offlinecache.ClassSoftware},
		{name: "xhtml entry document", contentType: "application/xhtml+xml", want: offlinecache.ClassSoftware},
		{name: "javascript entry", contentType: "application/javascript", want: offlinecache.ClassSoftware},
		{name: "image", contentType: "image/png", want: offlinecache.ClassMedia},
		{name: "video", contentType: "video/mp4", want: offlinecache.ClassMedia},
		{name: "audio", contentType: "audio/mpeg", want: offlinecache.ClassMedia},
		{name: "unknown binary", contentType: "application/octet-stream", want: offlinecache.ClassUnknown},
		{name: "missing content-type", contentType: "", want: offlinecache.ClassUnknown},
		{name: "svg image", contentType: "image/svg+xml", want: offlinecache.ClassMedia},
		{name: "gltf model", contentType: "model/gltf-binary", want: offlinecache.ClassUnknown},
		{name: "pdf document", contentType: "application/pdf", want: offlinecache.ClassUnknown},
		{name: "apple hls manifest", contentType: "application/vnd.apple.mpegurl", want: offlinecache.ClassStreaming},
		{name: "x-mpegurl hls manifest", contentType: "application/x-mpegurl; charset=utf-8", want: offlinecache.ClassStreaming},
		// DASH manifests must classify as ClassStreaming, not
		// ClassUnknown: see MediaClass's doc on why a DASH manifest that
		// fell through to the single-file direct-download path would
		// cache only the manifest with Coverage.Complete=true, then
		// fail every segment request offline (feral-file/ffos-user#229
		// review discussion).
		{name: "dash manifest", contentType: "application/dash+xml", want: offlinecache.ClassStreaming},
		{name: "dash manifest with charset", contentType: "application/dash+xml; charset=utf-8", want: offlinecache.ClassStreaming},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()

			mockHTTP := mocks.NewMockHTTPClient(ctrl)
			req, err := http.NewRequest(http.MethodHead, "https://example.com/art", nil)
			require.NoError(t, err)

			mockHTTP.EXPECT().
				NewRequest(http.MethodHead, "https://example.com/art", nil).
				Return(req, nil).
				Times(1)
			mockHTTP.EXPECT().
				Do(gomock.Any()).
				Return(newTestResponse(http.StatusOK, tt.contentType), nil).
				Times(1)

			classifier := offlinecache.NewClassifier(mockHTTP, publicResolver{})
			got, err := classifier.Classify(context.Background(), "https://example.com/art")
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

// TestClassifier_Classify_M3U8URLIsStreamingWithoutNetworkCall pins that
// a .m3u8 URL is classified as ClassStreaming purely from its extension,
// before any HEAD/GET is ever issued — the mock has zero expectations
// set, so gomock's strict controller would fail this test outright if a
// future edit made isStreamingURL's check run AFTER (or conditional on)
// a network round trip.
func TestClassifier_Classify_M3U8URLIsStreamingWithoutNetworkCall(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockHTTP := mocks.NewMockHTTPClient(ctrl)
	classifier := offlinecache.NewClassifier(mockHTTP, publicResolver{})

	got, err := classifier.Classify(context.Background(), "https://example.com/live/master.m3u8?token=abc123")
	require.NoError(t, err)
	assert.Equal(t, offlinecache.ClassStreaming, got)
}

// TestClassifier_Classify_M3U8URLIsCaseInsensitive pins that the
// extension check is not defeated by an origin that happens to serve an
// upper-cased path segment.
func TestClassifier_Classify_M3U8URLIsCaseInsensitive(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockHTTP := mocks.NewMockHTTPClient(ctrl)
	classifier := offlinecache.NewClassifier(mockHTTP, publicResolver{})

	got, err := classifier.Classify(context.Background(), "https://example.com/live/MASTER.M3U8")
	require.NoError(t, err)
	assert.Equal(t, offlinecache.ClassStreaming, got)
}

// TestClassifier_Classify_MPDURLIsStreamingWithoutNetworkCall mirrors
// the .m3u8 case above for DASH: a .mpd URL must classify as
// ClassStreaming purely from its extension, before any HEAD/GET is ever
// issued, so a DASH manifest can never slip through to ClassUnknown's
// single-file capture path even when an origin serves it with a
// misleading or absent Content-Type.
func TestClassifier_Classify_MPDURLIsStreamingWithoutNetworkCall(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockHTTP := mocks.NewMockHTTPClient(ctrl)
	classifier := offlinecache.NewClassifier(mockHTTP, publicResolver{})

	got, err := classifier.Classify(context.Background(), "https://example.com/live/manifest.mpd?token=abc123")
	require.NoError(t, err)
	assert.Equal(t, offlinecache.ClassStreaming, got)
}

// TestClassifier_Classify_MPDURLIsCaseInsensitive mirrors the .m3u8 case
// above for DASH.
func TestClassifier_Classify_MPDURLIsCaseInsensitive(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockHTTP := mocks.NewMockHTTPClient(ctrl)
	classifier := offlinecache.NewClassifier(mockHTTP, publicResolver{})

	got, err := classifier.Classify(context.Background(), "https://example.com/live/MANIFEST.MPD")
	require.NoError(t, err)
	assert.Equal(t, offlinecache.ClassStreaming, got)
}

func TestClassifier_Classify_FallsBackToRangedGETWhenHEADRejected(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockHTTP := mocks.NewMockHTTPClient(ctrl)

	headReq, err := http.NewRequest(http.MethodHead, "https://example.com/art", nil)
	require.NoError(t, err)
	getReq, err := http.NewRequest(http.MethodGet, "https://example.com/art", nil)
	require.NoError(t, err)

	mockHTTP.EXPECT().NewRequest(http.MethodHead, "https://example.com/art", nil).Return(headReq, nil).Times(1)
	mockHTTP.EXPECT().Do(gomock.Any()).Return(newTestResponse(http.StatusMethodNotAllowed, ""), nil).Times(1)

	mockHTTP.EXPECT().NewRequest(http.MethodGet, "https://example.com/art", nil).Return(getReq, nil).Times(1)
	mockHTTP.EXPECT().Do(gomock.Any()).DoAndReturn(func(req *http.Request) (*http.Response, error) {
		// A compliant origin honoring the Range header is the expected
		// happy path: only the small probed slice ever crosses the wire.
		wantRange := fmt.Sprintf("bytes=0-%d", offlinecache.ClassifyProbeRangeBytes-1)
		assert.Equal(t, wantRange, req.Header.Get("Range"))
		return newTestResponse(http.StatusPartialContent, "text/html"), nil
	}).Times(1)

	classifier := offlinecache.NewClassifier(mockHTTP, publicResolver{})
	got, err := classifier.Classify(context.Background(), "https://example.com/art")
	require.NoError(t, err)
	assert.Equal(t, offlinecache.ClassSoftware, got)
}

// boundedReader simulates an origin that ignores the Range header and
// streams an effectively unbounded body (e.g. a live/huge video) back with
// a 200 instead of a 206. It never reaches EOF on its own, so if
// classifyWithRangedGET failed to cap its read, this test would hang
// instead of failing fast.
type boundedReader struct {
	totalRead int
}

func (r *boundedReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'x'
	}
	r.totalRead += len(p)
	return len(p), nil
}

func (r *boundedReader) Close() error { return nil }

func TestClassifier_Classify_BoundsReadWhenOriginIgnoresRange(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockHTTP := mocks.NewMockHTTPClient(ctrl)

	headReq, err := http.NewRequest(http.MethodHead, "https://example.com/video.mp4", nil)
	require.NoError(t, err)
	getReq, err := http.NewRequest(http.MethodGet, "https://example.com/video.mp4", nil)
	require.NoError(t, err)

	mockHTTP.EXPECT().NewRequest(http.MethodHead, "https://example.com/video.mp4", nil).Return(headReq, nil).Times(1)
	mockHTTP.EXPECT().Do(gomock.Any()).Return(newTestResponse(http.StatusMethodNotAllowed, ""), nil).Times(1)

	body := &boundedReader{}
	mockHTTP.EXPECT().NewRequest(http.MethodGet, "https://example.com/video.mp4", nil).Return(getReq, nil).Times(1)
	mockHTTP.EXPECT().Do(gomock.Any()).Return(&http.Response{
		StatusCode: http.StatusOK, // origin ignored Range, sending the full asset
		Header:     http.Header{"Content-Type": []string{"video/mp4"}},
		Body:       body,
	}, nil).Times(1)

	classifier := offlinecache.NewClassifier(mockHTTP, publicResolver{})
	got, err := classifier.Classify(context.Background(), "https://example.com/video.mp4")
	require.NoError(t, err)
	assert.Equal(t, offlinecache.ClassMedia, got)
	assert.Equal(t, offlinecache.ClassifyProbeRangeBytes, body.totalRead,
		"classify must never read more than the probe cap from a body that never reaches EOF")
}

func TestClassifier_Classify_RequestError(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockHTTP := mocks.NewMockHTTPClient(ctrl)
	req, err := http.NewRequest(http.MethodHead, "https://example.com/art", nil)
	require.NoError(t, err)

	mockHTTP.EXPECT().NewRequest(http.MethodHead, "https://example.com/art", nil).Return(req, nil).Times(1)
	mockHTTP.EXPECT().Do(gomock.Any()).Return(nil, assertError("network unreachable")).Times(1)

	classifier := offlinecache.NewClassifier(mockHTTP, publicResolver{})
	_, err = classifier.Classify(context.Background(), "https://example.com/art")
	assert.Error(t, err)
}

type assertError string

func (e assertError) Error() string { return string(e) }

// TestClassifier_Classify_DataURIsAreInlineWithoutNetworkCall pins the
// regression this class was added for: a playlist carrying an inline
// base64 cover image used to reach http.Client, which cannot dial a
// data: URI, and every such item failed classification with "unsupported
// protocol scheme \"data\"" — a permanent, unretryable skip for an item
// that needs no fetching at all.
//
// The HTTP mock has zero expectations, so gomock's strict controller
// fails this outright if a future edit lets a data: URI reach a probe.
func TestClassifier_Classify_DataURIsAreInlineWithoutNetworkCall(t *testing.T) {
	// Media types spanning the classes a data: URI plausibly carries,
	// plus the RFC 2397 shapes that are easy to mis-parse.
	uris := []string{
		"data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAWgAAAJq",
		"data:image/gif;base64,R0lGODlhMAIwApEAAG+mq6xlVEBAQL+/vyH",
		"data:image/svg+xml;base64,PHN2ZyB2ZXJzaW9uPSIxLjEi",
		"data:image/svg+xml,%3Csvg%20xmlns%3D%22http%3A//www.w3.org/2000/svg%22%3E%3C/svg%3E",
		"data:text/html;charset=utf-8;base64,PGh0bWw+",
		"data:,Hello%2C%20World%21",              // media type omitted entirely
		"data:;base64,SGVsbG8=",                  // encoding only, no media type
		"DATA:image/png;base64,iVBORw0KGgo=",     // scheme is case-insensitive
		"data:image/png;name=base64.png,abc",     // ";base64" only as a true suffix
		"data:text/plain;charset=US-ASCII,plain", // parameter preserved
	}

	for _, uri := range uris {
		t.Run(uri[:min(len(uri), 48)], func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()

			classifier := offlinecache.NewClassifier(
				mocks.NewMockHTTPClient(ctrl), publicResolver{})

			got, err := classifier.Classify(context.Background(), uri)
			require.NoError(t, err)
			assert.Equal(t, offlinecache.ClassInline, got)
		})
	}
}

// TestClassifier_Classify_MalformedDataURIFails pins that a data: URI
// with no metadata-terminating comma is a real classification failure
// rather than a silent inline pass: the player cannot render it either,
// so accepting it would report an item as handled that never was.
func TestClassifier_Classify_MalformedDataURIFails(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	classifier := offlinecache.NewClassifier(
		mocks.NewMockHTTPClient(ctrl), publicResolver{})

	got, err := classifier.Classify(context.Background(), "data:image/png;base64iVBORw0KGgo")
	require.Error(t, err)
	assert.Equal(t, offlinecache.ClassUnknown, got)
}

// TestClassifier_Classify_HugeDataURIDoesNotScanWholePayload pins the
// bound on the metadata scan. A malformed multi-megabyte inline asset
// (no comma anywhere) must cost a fixed prefix scan, not a walk of the
// entire blob, on a path that runs once per playlist item on a
// constrained device.
func TestClassifier_Classify_HugeDataURIDoesNotScanWholePayload(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	classifier := offlinecache.NewClassifier(
		mocks.NewMockHTTPClient(ctrl), publicResolver{})

	// 4 MiB of base64-ish payload with the comma pushed far past the
	// scan bound, so finding it would mean the bound is not enforced.
	huge := "data:image/png;base64" + strings.Repeat("A", 4<<20) + ",payload"

	got, err := classifier.Classify(context.Background(), huge)
	require.Error(t, err)
	assert.Equal(t, offlinecache.ClassUnknown, got)
}
