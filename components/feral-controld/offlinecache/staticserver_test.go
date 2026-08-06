package offlinecache_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"

	"github.com/feral-file/ffos-user/components/feral-controld/offlinecache"
	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

func TestStaticServer_URLFor(t *testing.T) {
	store, _ := newTestStore(t)
	server := offlinecache.NewStaticServer("127.0.0.1:8082", store, wrapper.NewOS(), zaptest.NewLogger(t))

	url := server.URLFor("abc123", "video/mp4", nil)
	assert.Equal(t, "http://127.0.0.1:8082/blobs/abc123?ct=video%2Fmp4", url)

	urlNoContentType := server.URLFor("abc123", "", nil)
	assert.Equal(t, "http://127.0.0.1:8082/blobs/abc123", urlNoContentType)
}

// TestStaticServer_URLFor_EmbedsHeaders pins that headers is encoded into
// the URL as repeated "h=Name=Value" query entries, round-trippable by
// handleBlob (see TestStaticServer_ServesBlobWithCORSHeaders).
func TestStaticServer_URLFor_EmbedsHeaders(t *testing.T) {
	store, _ := newTestStore(t)
	server := offlinecache.NewStaticServer("127.0.0.1:8082", store, wrapper.NewOS(), zaptest.NewLogger(t))

	rawURL := server.URLFor("abc123", "", map[string]string{"Access-Control-Allow-Origin": "https://example.com"})
	parsed, err := url.Parse(rawURL)
	require.NoError(t, err)
	assert.Equal(t, []string{"Access-Control-Allow-Origin=https://example.com"}, parsed.Query()["h"])
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

// TestStaticServer_ServesBlobWithoutContentTypeWhenOriginDeclaredNone is
// this path's half of the declared-vs-sniffed split (see
// Resource.ContentType): no "ct" means the captured origin declared no
// Content-Type, and the large-asset fallback must reproduce that absence
// as faithfully as replay's inline fulfill does. http.ServeContent
// otherwise fills one in by sniffing the body — blobs are named by hash,
// so there is no extension for it to prefer first — reintroducing the
// invented declaration on assets over largeAssetThreshold.
func TestStaticServer_ServesBlobWithoutContentTypeWhenOriginDeclaredNone(t *testing.T) {
	store, _ := newTestStore(t)
	hash := writeBlobString(t, store, "body { margin: 0; width: 100vw; }")

	server := offlinecache.NewStaticServer("127.0.0.1:8082", store, wrapper.NewOS(), zaptest.NewLogger(t))
	ts := httptest.NewServer(server.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/blobs/" + hash)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Empty(t, resp.Header.Get("Content-Type"),
		"ServeContent's own sniff must stay suppressed, or Chromium receives a declaration the origin never made")

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Equal(t, "body { margin: 0; width: 100vw; }", string(body))
}

// TestStaticServer_ServesBlobWithCORSHeaders is the regression test for
// the large-asset replay path's CORS gap: a cross-origin resource served
// through this loopback fallback (see staticServer's doc) must still
// carry the captured Access-Control-* headers on the FINAL response, or
// Chromium's own CORS enforcement rejects it even though the bytes are
// correct — this is what URLFor's "h" query params round-trip into.
func TestStaticServer_ServesBlobWithCORSHeaders(t *testing.T) {
	store, _ := newTestStore(t)
	hash := writeBlobString(t, store, "large cross-origin asset payload")

	server := offlinecache.NewStaticServer("127.0.0.1:8082", store, wrapper.NewOS(), zaptest.NewLogger(t))
	ts := httptest.NewServer(server.Handler())
	defer ts.Close()

	target := server.URLFor(hash, "text/plain", map[string]string{
		"Access-Control-Allow-Origin":  "https://example.com",
		"Cross-Origin-Resource-Policy": "cross-origin",
	})
	parsed, err := url.Parse(target)
	require.NoError(t, err)

	resp, err := http.Get(ts.URL + "/blobs/" + hash + "?" + parsed.RawQuery)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "https://example.com", resp.Header.Get("Access-Control-Allow-Origin"))
	assert.Equal(t, "cross-origin", resp.Header.Get("Cross-Origin-Resource-Policy"))
}

// TestStaticServer_IgnoresNonAllowlistedHeaderQueryParam pins handleBlob's
// defense-in-depth re-check: a "h" query param naming a header outside
// replayableResponseHeaders must never be echoed back, even though
// URLFor itself (the only production caller) would never construct one.
func TestStaticServer_IgnoresNonAllowlistedHeaderQueryParam(t *testing.T) {
	store, _ := newTestStore(t)
	hash := writeBlobString(t, store, "payload")

	server := offlinecache.NewStaticServer("127.0.0.1:8082", store, wrapper.NewOS(), zaptest.NewLogger(t))
	ts := httptest.NewServer(server.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/blobs/" + hash + "?h=" + url.QueryEscape("Set-Cookie=session=evil"))
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Empty(t, resp.Header.Get("Set-Cookie"))
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

// TestStaticServer_Listen_IsListeningReflectsBindAndShutdownState is the
// regression test for the Listen/Serve split (see StaticServer's doc):
// IsListening must report false before any bind attempt, true once
// Listen succeeds, and false again after a graceful Shutdown — so a
// caller (main.go, and Replayer's own IsListening gate) can rely on it
// as the definitive "is this server actually reachable" signal rather
// than inferring availability from a background goroutine's log output.
func TestStaticServer_Listen_IsListeningReflectsBindAndShutdownState(t *testing.T) {
	store, _ := newTestStore(t)
	server := offlinecache.NewStaticServer("127.0.0.1:0", store, wrapper.NewOS(), zaptest.NewLogger(t))

	assert.False(t, server.IsListening(), "must not report listening before Listen is ever called")

	require.NoError(t, server.Listen())
	assert.True(t, server.IsListening())

	// Idempotent: a second Listen after a successful bind must not
	// error or attempt to rebind the same address.
	require.NoError(t, server.Listen())

	serveErr := make(chan error, 1)
	go func() { serveErr <- server.Serve() }()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	require.NoError(t, server.Shutdown(shutdownCtx))

	select {
	case err := <-serveErr:
		assert.NoError(t, err, "Serve must return cleanly (http.ErrServerClosed swallowed) after a graceful Shutdown")
	case <-time.After(2 * time.Second):
		t.Fatal("Serve did not return after Shutdown")
	}

	assert.False(t, server.IsListening(), "must report not-listening once Shutdown has torn the listener down")
}

// TestStaticServer_Serve_BeforeListenReturnsError pins that Serve
// refuses to run without a prior successful Listen, rather than
// silently binding for the caller — main.go's whole point in splitting
// these apart is to observe the bind result BEFORE spawning Serve, so a
// Serve that binds anyway would defeat that.
func TestStaticServer_Serve_BeforeListenReturnsError(t *testing.T) {
	store, _ := newTestStore(t)
	server := offlinecache.NewStaticServer("127.0.0.1:0", store, wrapper.NewOS(), zaptest.NewLogger(t))

	assert.Error(t, server.Serve())
}

// TestStaticServer_Listen_URLForUsesActualBoundEphemeralPort is the
// regression test for the config-accepted "127.0.0.1:0" case (see
// bootstrap_test.go's TestBootstrap_WiresEveryComponent, which already
// exercises this exact address): a staticServerAddr of port 0 asks the OS
// for an arbitrary free ephemeral port, but URLFor/BaseURL previously kept
// building URLs against the LITERAL configured ":0" host:port forever
// after — a redirect target nothing is ever listening on. Listen must
// update the address URLFor/BaseURL build against to the REAL bound
// address so a large-asset redirect (replay.go's fulfillFromBlob) is
// actually reachable.
func TestStaticServer_Listen_URLForUsesActualBoundEphemeralPort(t *testing.T) {
	store, _ := newTestStore(t)
	hash := writeBlobString(t, store, "payload")

	server := offlinecache.NewStaticServer("127.0.0.1:0", store, wrapper.NewOS(), zaptest.NewLogger(t))
	require.NoError(t, server.Listen())
	defer func() { _ = server.Shutdown(context.Background()) }()

	base := server.BaseURL()
	assert.NotContains(t, base, ":0", "BaseURL must reflect the OS-assigned ephemeral port, not the configured wildcard 0")

	go func() { _ = server.Serve() }()

	// End-to-end proof, not just a string check: URLFor's own output
	// (built from the same updated address) must actually be reachable
	// once Listen has bound a real port.
	resp, err := http.Get(server.URLFor(hash, "", nil))
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

// TestStaticServer_ServesBlobs_CorruptedLargeBlobFailsClosed is the
// regression test for the large-asset streaming path bypassing
// Store.ReadBlob's hash verification: handleBlob opens BlobPath directly
// (ReadBlob would buffer the whole file into memory, unusable for
// gigabyte-scale assets — see the package doc) after only a BlobSize
// existence/size check, which on its own would happily stream corrupted
// bytes back as a 200. verifyBlobContent must catch the mismatch and fail
// closed instead of silently serving the wrong content.
func TestStaticServer_ServesBlobs_CorruptedLargeBlobFailsClosed(t *testing.T) {
	store, _ := newTestStore(t)
	hash := writeBlobString(t, store, "the correct, original payload")

	// Overwrite the blob's on-disk bytes directly, keeping its
	// content-addressed filename unchanged — simulating disk-level
	// corruption (bit rot, a partial/failed write some layer beneath
	// the store's own atomic rename did not catch) rather than going
	// through WriteBlob, which would simply compute a DIFFERENT hash for
	// different content.
	require.NoError(t, os.WriteFile(store.BlobPath(hash), []byte("corrupted bytes, wrong content"), 0o600))

	server := offlinecache.NewStaticServer("127.0.0.1:8082", store, wrapper.NewOS(), zaptest.NewLogger(t))
	ts := httptest.NewServer(server.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/blobs/" + hash)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	assert.Equal(t, http.StatusInternalServerError, resp.StatusCode,
		"a blob whose on-disk content no longer matches its hash must fail closed, not be served as if it were valid")
}

// TestStaticServer_ServesBlobs_VerificationSuccessIsCachedAcrossRequests
// pins the accepted trade-off documented on verifyBlobContent: a
// successful verification is remembered for this process's lifetime so a
// Range request scrubbing through a large cached video is not forced to
// re-hash the ENTIRE file on every single request. The direct, deliberate
// consequence — corruption introduced strictly AFTER the first
// successful request is not caught by a later one — is exactly what this
// test documents, so a future change to that trade-off shows up here
// rather than silently regressing either direction.
func TestStaticServer_ServesBlobs_VerificationSuccessIsCachedAcrossRequests(t *testing.T) {
	store, _ := newTestStore(t)
	hash := writeBlobString(t, store, "the correct, original payload")

	server := offlinecache.NewStaticServer("127.0.0.1:8082", store, wrapper.NewOS(), zaptest.NewLogger(t))
	ts := httptest.NewServer(server.Handler())
	defer ts.Close()

	firstResp, err := http.Get(ts.URL + "/blobs/" + hash)
	require.NoError(t, err)
	firstBody, err := io.ReadAll(firstResp.Body)
	require.NoError(t, err)
	_ = firstResp.Body.Close()
	require.Equal(t, http.StatusOK, firstResp.StatusCode)
	require.Equal(t, "the correct, original payload", string(firstBody))

	require.NoError(t, os.WriteFile(store.BlobPath(hash), []byte("corrupted after the first verification"), 0o600))

	secondResp, err := http.Get(ts.URL + "/blobs/" + hash)
	require.NoError(t, err)
	secondBody, err := io.ReadAll(secondResp.Body)
	require.NoError(t, err)
	_ = secondResp.Body.Close()

	assert.Equal(t, http.StatusOK, secondResp.StatusCode,
		"a hash already verified once this process's lifetime must not be re-checked on a later request")
	assert.True(t, strings.HasPrefix(string(secondBody), "corrupted"),
		"the cached-success path serves whatever is currently on disk without re-hashing it")
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
