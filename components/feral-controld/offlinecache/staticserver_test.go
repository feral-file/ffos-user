package offlinecache_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"

	"github.com/feral-file/ffos-user/components/feral-controld/offlinecache"
	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

func TestStaticServer_URLFor(t *testing.T) {
	store, _ := newTestStore(t)
	server := offlinecache.NewStaticServer("127.0.0.1:8082", store, wrapper.NewOS(), zaptest.NewLogger(t))

	url := server.URLFor("abc123", "video/mp4")
	assert.Equal(t, "http://127.0.0.1:8082/blobs/abc123?ct=video%2Fmp4", url)

	urlNoContentType := server.URLFor("abc123", "")
	assert.Equal(t, "http://127.0.0.1:8082/blobs/abc123", urlNoContentType)
}

func TestStaticServer_ServesBlobWithContentType(t *testing.T) {
	store, _ := newTestStore(t)
	hash := writeBlobString(t, store, "large asset payload")

	server := offlinecache.NewStaticServer("127.0.0.1:8082", store, wrapper.NewOS(), zaptest.NewLogger(t))
	ts := httptest.NewServer(server.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/blobs/" + hash + "?ct=text%2Fplain")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "text/plain", resp.Header.Get("Content-Type"))

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Equal(t, "large asset payload", string(body))
}

func TestStaticServer_SupportsRangeRequests(t *testing.T) {
	store, _ := newTestStore(t)
	hash := writeBlobString(t, store, "0123456789")

	server := offlinecache.NewStaticServer("127.0.0.1:8082", store, wrapper.NewOS(), zaptest.NewLogger(t))
	ts := httptest.NewServer(server.Handler())
	defer ts.Close()

	req, err := http.NewRequest(http.MethodGet, ts.URL+"/blobs/"+hash, nil)
	require.NoError(t, err)
	req.Header.Set("Range", "bytes=2-4")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	assert.Equal(t, http.StatusPartialContent, resp.StatusCode)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Equal(t, "234", string(body))
}

func TestStaticServer_UnknownBlob_404(t *testing.T) {
	store, _ := newTestStore(t)
	server := offlinecache.NewStaticServer("127.0.0.1:8082", store, wrapper.NewOS(), zaptest.NewLogger(t))
	ts := httptest.NewServer(server.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/blobs/does-not-exist")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestStaticServer_InvalidBlobID_400(t *testing.T) {
	store, _ := newTestStore(t)
	server := offlinecache.NewStaticServer("127.0.0.1:8082", store, wrapper.NewOS(), zaptest.NewLogger(t))
	ts := httptest.NewServer(server.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/blobs/../escape")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	// net/http's URL cleaning collapses "..", so this either 400s here or
	// resolves outside the route and 404s via mux — either is an acceptable
	// "not served" outcome; assert it is never a 200.
	assert.NotEqual(t, http.StatusOK, resp.StatusCode)
}

func TestStaticServer_MethodNotAllowed(t *testing.T) {
	store, _ := newTestStore(t)
	hash := writeBlobString(t, store, "data")

	server := offlinecache.NewStaticServer("127.0.0.1:8082", store, wrapper.NewOS(), zaptest.NewLogger(t))
	ts := httptest.NewServer(server.Handler())
	defer ts.Close()

	req, err := http.NewRequest(http.MethodPost, ts.URL+"/blobs/"+hash, nil)
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusMethodNotAllowed, resp.StatusCode)
}
