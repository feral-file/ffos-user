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

// TestNewStaticServer_CoercesNonLoopbackHostTo127001 is the regression
// test pinning that a misconfigured offlineCache.staticServerAddr (e.g. a
// typo like "0.0.0.0:8082") must never be bound to verbatim, since that
// would expose cached artwork blobs over the LAN with no authentication.
// NewStaticServer must force the host to 127.0.0.1 regardless, preserving
// the configured port.
func TestNewStaticServer_CoercesNonLoopbackHostTo127001(t *testing.T) {
	store, _ := newTestStore(t)
	server := offlinecache.NewStaticServer("0.0.0.0:8082", store, wrapper.NewOS(), zaptest.NewLogger(t))
	assert.Equal(t, "http://127.0.0.1:8082", server.BaseURL())
}

// TestNewStaticServer_NormalizesUnspecifiedHostTo127001 covers the most
// dangerous misconfiguration: an empty host (e.g. ":8082") looks harmless
// in config but net.Listen treats it as "listen on all interfaces."
func TestNewStaticServer_NormalizesUnspecifiedHostTo127001(t *testing.T) {
	store, _ := newTestStore(t)
	server := offlinecache.NewStaticServer(":8082", store, wrapper.NewOS(), zaptest.NewLogger(t))
	assert.Equal(t, "http://127.0.0.1:8082", server.BaseURL())
}

// TestNewStaticServer_KeepsConfiguredLoopbackAddrUnchanged pins that a
// correctly-configured loopback address (IPv4 loopback, IPv6 loopback, or
// the "localhost" alias) is never rewritten.
func TestNewStaticServer_KeepsConfiguredLoopbackAddrUnchanged(t *testing.T) {
	store, _ := newTestStore(t)

	for _, addr := range []string{"127.0.0.1:8082", "127.5.6.7:8082", "[::1]:8082", "localhost:8082"} {
		t.Run(addr, func(t *testing.T) {
			server := offlinecache.NewStaticServer(addr, store, wrapper.NewOS(), zaptest.NewLogger(t))
			assert.Equal(t, "http://"+addr, server.BaseURL())
		})
	}
}

// TestNewStaticServer_InvalidAddrFallsBackToDefault covers a
// staticServerAddr that is not even a valid host:port pair.
func TestNewStaticServer_InvalidAddrFallsBackToDefault(t *testing.T) {
	store, _ := newTestStore(t)
	server := offlinecache.NewStaticServer("not-a-valid-addr", store, wrapper.NewOS(), zaptest.NewLogger(t))
	assert.Equal(t, "http://"+offlinecache.DefaultStaticServerAddr, server.BaseURL())
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
