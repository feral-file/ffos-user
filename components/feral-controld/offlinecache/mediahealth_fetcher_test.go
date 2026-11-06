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
	"golang.org/x/sync/semaphore"

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

// hopRewriter is a mediahealth.Rewriter double for the redirect tests: every
// URL on the retired host is re-pointed at the replacement origin with the
// policy agent; the replacement itself is unmatched.
type hopRewriter struct{ retired, replacement string }

func (r hopRewriter) RewriteURL(raw string) (string, bool) {
	if strings.HasPrefix(raw, r.retired) {
		return r.replacement + strings.TrimPrefix(raw, r.retired), true
	}
	return "", false
}
func (r hopRewriter) Matches(raw string) bool { return strings.HasPrefix(raw, r.retired) }
func (hopRewriter) UserAgent() string         { return "feral-player/2.0" }

// F14 (PR #438 review, round 4): a redirect onto a retired gateway must be
// re-pointed through the kiosk's policy before the dial — the bot's exact
// scenario: the retired gateway answers 404, the replacement 200, and the
// probe used to record `broken` from the hop the kiosk never fetches.
func TestMediaHealthFetcher_RedirectHopsGoThroughTheGatewayPolicy(t *testing.T) {
	var retiredHits atomic.Int32
	retired := httptest.NewServer(go_http.HandlerFunc(func(w go_http.ResponseWriter, _ *go_http.Request) {
		retiredHits.Add(1)
		w.WriteHeader(go_http.StatusNotFound)
	}))
	defer retired.Close()
	var gotAgent, gotPath string
	replacement := httptest.NewServer(go_http.HandlerFunc(func(w go_http.ResponseWriter, r *go_http.Request) {
		gotAgent, gotPath = r.Header.Get("User-Agent"), r.URL.Path
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(minimalPNGForTest())
	}))
	defer replacement.Close()
	origin := httptest.NewServer(go_http.HandlerFunc(func(w go_http.ResponseWriter, r *go_http.Request) {
		go_http.Redirect(w, r, retired.URL+"/ipfs/bafy/a.png", go_http.StatusFound)
	}))
	defer origin.Close()

	rw := hopRewriter{retired: retired.URL, replacement: replacement.URL}
	probe := &mediahealth.Probe{
		Fetcher:   newTestFetcher(),
		Validator: mediahealth.NewContentValidator(mediahealth.DefaultProbeMaxBytes, nil),
		Rewriter:  rw,
	}
	res := probe.ProbeOne(context.Background(), origin.URL+"/art")
	assert.Equal(t, mediahealth.VerdictAlive, res.Verdict, "the destination the kiosk fetches is healthy")
	assert.Equal(t, replacement.URL+"/ipfs/bafy/a.png", res.WireURL)
	assert.Equal(t, int32(0), retiredHits.Load(), "the retired gateway is never dialed")
	assert.Equal(t, "/ipfs/bafy/a.png", gotPath)
	assert.NotEqual(t, "feral-player/2.0", gotAgent, "the replacement origin is unmatched: client default agent, not the policy one")
	assert.NotEmpty(t, gotAgent)

	// The policy agent applies on a matched hop that is not rewritten, and
	// is removed again on a later unmatched hop.
	var hostileAgent, finalAgent string
	final := httptest.NewServer(go_http.HandlerFunc(func(w go_http.ResponseWriter, r *go_http.Request) {
		finalAgent = r.Header.Get("User-Agent")
		w.WriteHeader(go_http.StatusOK)
	}))
	defer final.Close()
	hostile := httptest.NewServer(go_http.HandlerFunc(func(w go_http.ResponseWriter, r *go_http.Request) {
		hostileAgent = r.Header.Get("User-Agent")
		go_http.Redirect(w, r, final.URL+"/x", go_http.StatusFound)
	}))
	defer hostile.Close()
	first := httptest.NewServer(go_http.HandlerFunc(func(w go_http.ResponseWriter, r *go_http.Request) {
		go_http.Redirect(w, r, hostile.URL+"/ipns/name", go_http.StatusFound)
	}))
	defer first.Close()
	agentOnly := hopRewriter{retired: hostile.URL, replacement: ""}
	resp, err := newTestFetcher().Fetch(context.Background(), mediahealth.FetchRequest{
		URL: first.URL + "/a", MaxBodyBytes: 16,
		Redirect: func(target string) (string, string) {
			if strings.HasPrefix(target, hostile.URL) {
				return target, agentOnly.UserAgent()
			}
			return target, ""
		},
	})
	require.NoError(t, err)
	assert.Equal(t, go_http.StatusOK, resp.Status)
	assert.Equal(t, "feral-player/2.0", hostileAgent)
	assert.NotEqual(t, "feral-player/2.0", finalAgent, "the policy agent does not leak onto an unmatched hop")

	// Without a hook the client follows the raw hop, as before.
	resp, err = newTestFetcher().Fetch(context.Background(), mediahealth.FetchRequest{URL: origin.URL + "/art", MaxBodyBytes: 16})
	require.NoError(t, err)
	assert.Equal(t, go_http.StatusNotFound, resp.Status)
	assert.Equal(t, int32(1), retiredHits.Load())
}

func TestMediaHealthFetcher_RewrittenHopIsGuarded(t *testing.T) {
	origin := httptest.NewServer(go_http.HandlerFunc(func(w go_http.ResponseWriter, r *go_http.Request) {
		go_http.Redirect(w, r, "http://harmless.example/ipfs/bafy", go_http.StatusFound)
	}))
	defer origin.Close()
	_, err := newTestFetcher().Fetch(context.Background(), mediahealth.FetchRequest{
		URL: origin.URL + "/a", MaxBodyBytes: 16,
		Redirect: func(string) (string, string) { return "http://169.254.169.254/latest/meta-data", "" },
	})
	require.Error(t, err)
	fe, ok := errors.AsType[*mediahealth.FetchError](err)
	require.True(t, ok)
	assert.Equal(t, mediahealth.FetchErrGuard, fe.Kind, "the guard judges the rewritten destination")

	_, err = newTestFetcher().Fetch(context.Background(), mediahealth.FetchRequest{
		URL: origin.URL + "/a", MaxBodyBytes: 16,
		Redirect: func(string) (string, string) { return "https://o/" + strings.Repeat("x", MaxSourceURLBytes), "" },
	})
	require.Error(t, err)
	fe, ok = errors.AsType[*mediahealth.FetchError](err)
	require.True(t, ok)
	assert.Equal(t, mediahealth.FetchErrTooLong, fe.Kind)
}

// minimalPNGForTest is a 1×1 PNG: enough for the validator's container
// check (mediahealth keeps its own copy in its tests; the package cannot
// be imported for test helpers).
func minimalPNGForTest() []byte {
	return []byte{
		0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A,
		0x00, 0x00, 0x00, 0x0D, 0x49, 0x48, 0x44, 0x52,
		0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
		0x08, 0x06, 0x00, 0x00, 0x00, 0x1F, 0x15, 0xC4, 0x89,
		0x00, 0x00, 0x00, 0x0A, 0x49, 0x44, 0x41, 0x54,
		0x78, 0x9C, 0x63, 0x00, 0x01, 0x00, 0x00, 0x05, 0x00, 0x01,
		0x0D, 0x0A, 0x2D, 0xB4,
		0x00, 0x00, 0x00, 0x00, 0x49, 0x45, 0x4E, 0x44, 0xAE, 0x42, 0x60, 0x82,
	}
}

// F18 (PR #438 review, round 5): a plain-HTTP probe that redirects to HTTPS
// must hold the TLS weight before the handshake, with no hold-and-wait.
func TestMediaHealthFetcher_RedirectToHTTPSPaysTheTLSWeight(t *testing.T) {
	guard := sourceGuard{resolver: staticResolver{ip: "93.184.216.34"}, isReserved: loopbackIsPublic}

	// The mechanics: an http→https hop grows the hold from 1 to 4; an
	// https→https hop keeps 4; a refused hop acquires nothing.
	slots := semaphore.NewWeighted(probeFetchHTTPSWeight)
	adm := &probeAdmission{slots: slots}
	require.NoError(t, adm.ensure(context.Background(), 1))
	ctx := context.WithValue(context.Background(), admissionKey{}, adm)
	hop, _ := go_http.NewRequestWithContext(ctx, go_http.MethodGet, "https://origin.example/a", nil)
	require.NoError(t, guard.probeCheckRedirect(hop, nil))
	assert.Equal(t, probeFetchHTTPSWeight, adm.held)
	assert.False(t, slots.TryAcquire(1), "the whole TLS weight is held")
	hop2, _ := go_http.NewRequestWithContext(ctx, go_http.MethodGet, "https://origin.example/b", nil)
	require.NoError(t, guard.probeCheckRedirect(hop2, []*go_http.Request{hop}))
	assert.Equal(t, probeFetchHTTPSWeight, adm.held)
	adm.release()
	assert.True(t, slots.TryAcquire(probeFetchHTTPSWeight), "released in full")
	slots.Release(probeFetchHTTPSWeight)

	refused := &probeAdmission{slots: slots}
	ctxRefused := context.WithValue(context.Background(), admissionKey{}, refused)
	bad, _ := go_http.NewRequestWithContext(ctxRefused, go_http.MethodGet, "ftp://origin.example/a", nil)
	require.Error(t, guard.probeCheckRedirect(bad, nil))
	assert.Equal(t, int64(0), refused.held)

	// End to end: with only one unit free, the upgrade waits for admission
	// (never starting the handshake) until the deadline, and leaks nothing.
	tight := semaphore.NewWeighted(probeFetchHTTPSWeight)
	require.True(t, tight.TryAcquire(probeFetchHTTPSWeight-1))
	origin := httptest.NewServer(go_http.HandlerFunc(func(w go_http.ResponseWriter, r *go_http.Request) {
		go_http.Redirect(w, r, "https://127.0.0.1:1/a", go_http.StatusFound)
	}))
	defer origin.Close()
	f := &mediaHealthFetcher{guard: guard, client: newGuardedProbeHTTPClientFor(guard, 0), slots: tight}
	ctxTimeout, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	_, err := f.Fetch(ctxTimeout, mediahealth.FetchRequest{URL: origin.URL + "/a", MaxBodyBytes: 16})
	require.Error(t, err)
	fe, ok := errors.AsType[*mediahealth.FetchError](err)
	require.True(t, ok)
	assert.Equal(t, mediahealth.FetchErrTimeout, fe.Kind, "admission, not the dial, is what the request waited on")
	tight.Release(probeFetchHTTPSWeight - 1)
	assert.True(t, tight.TryAcquire(probeFetchHTTPSWeight), "nothing leaked after the failed upgrade")

	// An http→http hop keeps the single unit.
	one := semaphore.NewWeighted(1)
	plain := httptest.NewServer(go_http.HandlerFunc(func(w go_http.ResponseWriter, _ *go_http.Request) { w.WriteHeader(go_http.StatusOK) }))
	defer plain.Close()
	hopper := httptest.NewServer(go_http.HandlerFunc(func(w go_http.ResponseWriter, r *go_http.Request) {
		go_http.Redirect(w, r, plain.URL+"/b", go_http.StatusFound)
	}))
	defer hopper.Close()
	f = &mediaHealthFetcher{guard: guard, client: newGuardedProbeHTTPClientFor(guard, 0), slots: one}
	resp, err := f.Fetch(context.Background(), mediahealth.FetchRequest{URL: hopper.URL + "/a", MaxBodyBytes: 16})
	require.NoError(t, err)
	assert.Equal(t, go_http.StatusOK, resp.Status)
	assert.True(t, one.TryAcquire(1))
}

// Resolution failures short of NXDOMAIN are their own class, so the store
// can count a streak of them; NXDOMAIN stays definitive and a non-DNS
// timeout stays plain network weather.
func TestClassifyFetchError_DNSClasses(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want mediahealth.FetchErrorKind
	}{
		{"nxdomain", &net.DNSError{Err: "no such host", Name: "a.example", IsNotFound: true}, mediahealth.FetchErrDNS},
		{"servfail", &net.DNSError{Err: "server misbehaving", Name: "a.example", IsTemporary: true}, mediahealth.FetchErrDNSUnresolved},
		{"lookup timeout", &net.DNSError{Err: "i/o timeout", Name: "a.example", IsTimeout: true}, mediahealth.FetchErrDNSUnresolved},
		{"wrapped servfail", &net.OpError{Op: "dial", Err: &net.DNSError{Err: "server misbehaving", Name: "a.example"}}, mediahealth.FetchErrDNSUnresolved},
		{"deadline", context.DeadlineExceeded, mediahealth.FetchErrTimeout},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fe, ok := errors.AsType[*mediahealth.FetchError](classifyFetchError(tc.err))
			require.True(t, ok)
			assert.Equal(t, tc.want, fe.Kind)
		})
	}
}
