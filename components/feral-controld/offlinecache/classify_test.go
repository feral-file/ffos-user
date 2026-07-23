package offlinecache_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/feral-file/ffos-user/components/feral-controld/mocks"
	"github.com/feral-file/ffos-user/components/feral-controld/offlinecache"
)

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

			classifier := offlinecache.NewClassifier(mockHTTP)
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
	classifier := offlinecache.NewClassifier(mockHTTP)

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
	classifier := offlinecache.NewClassifier(mockHTTP)

	got, err := classifier.Classify(context.Background(), "https://example.com/live/MASTER.M3U8")
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

	classifier := offlinecache.NewClassifier(mockHTTP)
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

	classifier := offlinecache.NewClassifier(mockHTTP)
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

	classifier := offlinecache.NewClassifier(mockHTTP)
	_, err = classifier.Classify(context.Background(), "https://example.com/art")
	assert.Error(t, err)
}

type assertError string

func (e assertError) Error() string { return string(e) }
