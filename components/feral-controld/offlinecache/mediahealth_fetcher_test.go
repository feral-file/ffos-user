package offlinecache

import (
	"context"
	"errors"
	"net"
	go_http "net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/feral-file/ffos-user/components/feral-controld/mediahealth"
)

// Every httptest server lives on loopback, which the production guard
// rejects, so these tests inject loopbackIsPublic (sourceguard_wedge_test)
// exactly as the guard's own tests do.
func newTestFetcher() mediahealth.Fetcher {
	guard := sourceGuard{resolver: staticResolver{ip: "93.184.216.34"}, isReserved: loopbackIsPublic}
	return newMediaHealthFetcherWith(guard, newGuardedProbeHTTPClientFor(guard, 0))
}

func TestMediaHealthFetcher_RangedGETBoundedBodyAndLabel(t *testing.T) {
	var gotRange, gotAgent string
	origin := httptest.NewServer(go_http.HandlerFunc(func(w go_http.ResponseWriter, r *go_http.Request) {
		gotRange = r.Header.Get("Range")
		gotAgent = r.Header.Get("User-Agent")
		w.Header().Set("Content-Type", "image/png")
		w.WriteHeader(go_http.StatusOK)
		_, _ = w.Write(make([]byte, 100_000)) // ignores Range: must still be bounded locally
	}))
	defer origin.Close()

	resp, err := newTestFetcher().Fetch(context.Background(), mediahealth.FetchRequest{
		URL: origin.URL + "/a.png?X-Amz-Signature=secret", UserAgent: "feral-player/2.0", RangeBytes: 4096, MaxBodyBytes: 4096,
	})
	require.NoError(t, err)
	assert.Equal(t, "bytes=0-4095", gotRange)
	assert.Equal(t, "feral-player/2.0", gotAgent)
	assert.Equal(t, go_http.StatusOK, resp.Status)
	assert.Len(t, resp.Body, 4096, "never more than MaxBodyBytes, whatever the origin sends")
	assert.Equal(t, "image/png", resp.Header.Get("Content-Type"))
	label := resp.Header.Get(mediahealth.LabelHeader)
	assert.Equal(t, origin.URL+"/a.png?…", label)
	assert.NotContains(t, label, "secret")
	assert.Equal(t, origin.URL+"/a.png?X-Amz-Signature=secret", resp.FinalURL)
}

func TestMediaHealthFetcher_FollowsGuardedRedirectsAndReportsFinalURL(t *testing.T) {
	target := httptest.NewServer(go_http.HandlerFunc(func(w go_http.ResponseWriter, _ *go_http.Request) {
		w.Header().Set("Content-Type", "image/gif")
		_, _ = w.Write([]byte("GIF89a"))
	}))
	defer target.Close()
	origin := httptest.NewServer(go_http.HandlerFunc(func(w go_http.ResponseWriter, r *go_http.Request) {
		go_http.Redirect(w, r, target.URL+"/final.gif", go_http.StatusFound)
	}))
	defer origin.Close()

	resp, err := newTestFetcher().Fetch(context.Background(), mediahealth.FetchRequest{URL: origin.URL + "/a", MaxBodyBytes: 64})
	require.NoError(t, err)
	assert.Equal(t, go_http.StatusOK, resp.Status)
	assert.Equal(t, target.URL+"/final.gif", resp.FinalURL)
	assert.Equal(t, "GIF89a", string(resp.Body))
}

func TestMediaHealthFetcher_RedirectToReservedIsAGuardError(t *testing.T) {
	origin := httptest.NewServer(go_http.HandlerFunc(func(w go_http.ResponseWriter, r *go_http.Request) {
		go_http.Redirect(w, r, "http://169.254.169.254/latest/meta-data", go_http.StatusFound)
	}))
	defer origin.Close()

	_, err := newTestFetcher().Fetch(context.Background(), mediahealth.FetchRequest{URL: origin.URL + "/a", MaxBodyBytes: 64})
	require.Error(t, err)
	fe, ok := errors.AsType[*mediahealth.FetchError](err)
	require.True(t, ok)
	assert.Equal(t, mediahealth.FetchErrGuard, fe.Kind)
	assert.ErrorIs(t, err, ErrUnsafeSource)
}

func TestMediaHealthFetcher_ErrorClassification(t *testing.T) {
	f := newTestFetcher()

	_, err := f.Fetch(context.Background(), mediahealth.FetchRequest{URL: "file:///etc/shadow"})
	fe, ok := errors.AsType[*mediahealth.FetchError](err)
	require.True(t, ok)
	assert.Equal(t, mediahealth.FetchErrGuard, fe.Kind)

	_, err = f.Fetch(context.Background(), mediahealth.FetchRequest{URL: "https://o/" + strings.Repeat("x", MaxSourceURLBytes)})
	fe, ok = errors.AsType[*mediahealth.FetchError](err)
	require.True(t, ok)
	assert.Equal(t, mediahealth.FetchErrTooLong, fe.Kind)

	nx := sourceGuard{resolver: nxdomainResolver{}, isReserved: loopbackIsPublic}
	_, err = newMediaHealthFetcherWith(nx, newGuardedProbeHTTPClientFor(nx, 0)).Fetch(context.Background(), mediahealth.FetchRequest{URL: "https://gone.example/a"})
	fe, ok = errors.AsType[*mediahealth.FetchError](err)
	require.True(t, ok)
	assert.Equal(t, mediahealth.FetchErrDNS, fe.Kind, "NXDOMAIN is a fact about the source")

	ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	time.Sleep(time.Millisecond)
	_, err = f.Fetch(ctx, mediahealth.FetchRequest{URL: "http://127.0.0.1:1/a"})
	fe, ok = errors.AsType[*mediahealth.FetchError](err)
	require.True(t, ok)
	assert.Equal(t, mediahealth.FetchErrTimeout, fe.Kind)

	closed := httptest.NewServer(go_http.NotFoundHandler())
	closed.Close()
	_, err = f.Fetch(context.Background(), mediahealth.FetchRequest{URL: closed.URL + "/a"})
	fe, ok = errors.AsType[*mediahealth.FetchError](err)
	require.True(t, ok)
	assert.Equal(t, mediahealth.FetchErrTransport, fe.Kind)
}

type nxdomainResolver struct{}

func (nxdomainResolver) LookupIPAddr(_ context.Context, host string) ([]net.IPAddr, error) {
	return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
}

func TestMediaHealthFetcher_NoConnectionReuse(t *testing.T) {
	var conns atomic.Int32
	origin := httptest.NewUnstartedServer(go_http.HandlerFunc(func(w go_http.ResponseWriter, _ *go_http.Request) {
		w.WriteHeader(go_http.StatusOK)
	}))
	origin.Config.ConnState = func(_ net.Conn, state go_http.ConnState) {
		if state == go_http.StateNew {
			conns.Add(1)
		}
	}
	origin.Start()
	defer origin.Close()

	f := newTestFetcher()
	for i := 0; i < 3; i++ {
		_, err := f.Fetch(context.Background(), mediahealth.FetchRequest{URL: origin.URL + "/a", MaxBodyBytes: 16})
		require.NoError(t, err)
	}
	assert.Equal(t, int32(3), conns.Load(), "keep-alives off: one dial per probe, so net/http can never replay a request")
}

// TestRedactSourceForLog pins the log-side credential redaction: the daemon
// log rides off-device in uploadLogs bundles, and a presigned CDN URL's
// credential parameters commonly sit inside truncateSourceForLog's 256-byte
// window — so the query string must be GONE, not merely shortened, before
// truncation. Host and path survive for operator grep.
func TestRedactSourceForLog(t *testing.T) {
	signed := "https://cdn.example.com/art/piece.html?X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Credential=AKIA123%2F20260901&X-Amz-Signature=deadbeef"
	got := redactSourceForLog(signed)
	assert.Equal(t, "https://cdn.example.com/art/piece.html?…", got)
	assert.NotContains(t, got, "X-Amz")
	assert.Equal(t, "https://cdn.example.com/a.html", redactSourceForLog("https://cdn.example.com/a.html"))
	assert.Equal(t, "https://cdn.example.com/a.html", redactSourceForLog("https://cdn.example.com/a.html#frag"))
	assert.Equal(t, "http://bad\x7f?…", redactSourceForLog("http://bad\x7f?X-Amz-Signature=deadbeef"))
	assert.Equal(t, "https://cdn.example.com/a.html", redactSourceForLog("https://id:secret@cdn.example.com/a.html"))
	got = redactSourceForLog("https://token@cdn.example.com/a.html?sig=deadbeef")
	assert.Equal(t, "https://cdn.example.com/a.html?…", got)
	assert.NotContains(t, got, "token")
	assert.Equal(t, "data:text/html,hello", redactSourceForLog("data:text/html,hello"))
}

// The two packages key the same item the same way, and parse a data: URI
// the same way: mediahealth cannot import this package, so the equalities
// are pinned here.
func TestMediaHealth_KeyAndDataURIParityWithOfflineCache(t *testing.T) {
	for _, src := range []string{"https://o/a", "https://o/a?x=1#f", "data:,x"} {
		assert.Equal(t, SourceKey(src), mediahealth.Key(src), src)
	}
	for _, uri := range []string{"data:image/png;base64,AAAA", "data:,hello", "DATA:text/plain;charset=utf-8,x", "data:nocomma"} {
		_, ocErr := dataURIMediaType(uri)
		assert.Equal(t, ocErr == nil, mediahealth.IsDataURI(uri) && dataURIParses(uri), uri)
	}
}

func dataURIParses(uri string) bool {
	res := (&mediahealth.Probe{}).ProbeOne(context.Background(), uri)
	return res.Verdict == mediahealth.VerdictInline
}

// F4 (PR #438 review): a redirect hop's signed URL must not survive into the
// fetch error that the cast path logs and the store keeps as lastError.
func TestMediaHealthFetcher_ErrorsCarryNoCredentials(t *testing.T) {
	origin := httptest.NewServer(go_http.HandlerFunc(func(w go_http.ResponseWriter, r *go_http.Request) {
		go_http.Redirect(w, r, "https://gone.example/a.png?X-Amz-Signature=deadbeef", go_http.StatusFound)
	}))
	defer origin.Close()
	nx := sourceGuard{resolver: nxdomainResolver{}, isReserved: loopbackIsPublic}
	f := newMediaHealthFetcherWith(nx, newGuardedProbeHTTPClientFor(nx, 0))
	_, err := f.Fetch(context.Background(), mediahealth.FetchRequest{URL: origin.URL + "/a", MaxBodyBytes: 64})
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "deadbeef")
	assert.NotContains(t, err.Error(), "X-Amz")
	assert.Contains(t, err.Error(), "gone.example")
	fe, ok := errors.AsType[*mediahealth.FetchError](err)
	require.True(t, ok)
	assert.Equal(t, mediahealth.FetchErrDNS, fe.Kind, "the class still classifies through the redaction")
}
