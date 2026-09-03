package offlinecache

// White-box tests (package offlinecache, like the other *_wedge_test.go
// files) because the prober under test is constructed with the unexported
// guard-and-client seam (newSourceProberWith) — every httptest server lives
// on loopback, which the production guard rejects, so the loopbackIsPublic
// override from sourceguard_wedge_test.go is what lets a "public" origin
// exist at all. What these pin is the verdict table (#304): only an actual
// HTTP >= 400 answer is Dead; everything that prevents a definitive answer
// — network faults, guard refusals, over-long URLs, a spent context — is
// Inconclusive, because the handler treats Inconclusive in the cast's
// favor and an offline device must keep casting cached playlists.

import (
	"context"
	"fmt"
	"io"
	"net"
	go_http "net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestProber() SourceProber {
	// The resolver only matters for hostname sources (httptest URLs are
	// literal-IP and skip DNS); a static public answer keeps those from
	// touching real DNS.
	guard := sourceGuard{resolver: staticResolver{ip: "93.184.216.34"}, isReserved: loopbackIsPublic}
	// Same no-redirect client shape as production (NewSourceProber): the
	// request-count bound under test is only real if redirects are never
	// followed here either.
	return newSourceProberWith(guard, newGuardedNoRedirectHTTPClientFor(guard, 2*time.Second))
}

func probeOneSource(t *testing.T, prober SourceProber, source string) SourceProbeResult {
	t.Helper()
	results := prober.ProbeSources(context.Background(), []string{source})
	require.Len(t, results, 1)
	return results[0]
}

type blockingProbeHTTPClient struct {
	entered chan struct{}
	release chan struct{}
	active  atomic.Int64
	peak    atomic.Int64
}

func (c *blockingProbeHTTPClient) NewRequest(method string, url string, body io.Reader) (*go_http.Request, error) {
	return go_http.NewRequest(method, url, body)
}

func (c *blockingProbeHTTPClient) Do(req *go_http.Request) (*go_http.Response, error) {
	active := c.active.Add(1)
	defer c.active.Add(-1)
	for {
		peak := c.peak.Load()
		if active <= peak || c.peak.CompareAndSwap(peak, active) {
			break
		}
	}
	c.entered <- struct{}{}
	select {
	case <-c.release:
		return &go_http.Response{
			StatusCode: go_http.StatusOK,
			Body:       io.NopCloser(strings.NewReader("")),
			Header:     make(go_http.Header),
		}, nil
	case <-req.Context().Done():
		return nil, req.Context().Err()
	}
}

func (c *blockingProbeHTTPClient) Get(string) (*go_http.Response, error) {
	panic("unexpected Get")
}

func (c *blockingProbeHTTPClient) Post(string, string, io.Reader) (*go_http.Response, error) {
	panic("unexpected Post")
}

// TestSourceProber_HeaderBudgetIsSharedAcrossCalls proves that concurrent
// ProbeSources calls share one request-admission budget. The command storm
// gate is operator-configurable (and can be disabled), so the header-memory
// safety bound must live at the resource it protects rather than relying on
// any one gate configuration.
func TestSourceProber_HeaderBudgetIsSharedAcrossCalls(t *testing.T) {
	client := &blockingProbeHTTPClient{
		entered: make(chan struct{}, 4),
		release: make(chan struct{}, 4),
	}
	guard := sourceGuard{resolver: staticResolver{ip: "93.184.216.34"}}
	prober := newSourceProberWithHeaderSlots(
		guard,
		client,
		2*probeHTTPSHeaderBudgetSlots,
	)

	var wg sync.WaitGroup
	for call := 0; call < 2; call++ {
		call := call
		wg.Add(1)
		go func() {
			defer wg.Done()
			prober.ProbeSources(context.Background(), []string{
				fmt.Sprintf("https://93.184.216.34/%d-a", call),
				fmt.Sprintf("https://93.184.216.34/%d-b", call),
			})
		}()
	}

	for i := 0; i < 2; i++ {
		select {
		case <-client.entered:
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for the admitted probes")
		}
	}
	select {
	case <-client.entered:
		t.Fatal("a third probe entered before a header-budget slot was released")
	case <-time.After(100 * time.Millisecond):
	}

	client.release <- struct{}{}
	client.release <- struct{}{}
	for i := 0; i < 2; i++ {
		select {
		case <-client.entered:
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for a queued probe to take a released slot")
		}
	}
	client.release <- struct{}{}
	client.release <- struct{}{}
	wg.Wait()

	assert.Equal(t, int64(2), client.peak.Load())
}

// TestSourceProber_ProbeIsOneRangedGET pins the probe's request shape: a
// single GET (what the player's renderer actually issues — a HEAD verdict
// can disagree with the GET the artwork will get), range-bounded so the
// probe never pulls an asset body through the daemon, and no HEAD at all.
func TestSourceProber_ProbeIsOneRangedGET(t *testing.T) {
	var methods []string
	var sawRange string
	origin := httptest.NewServer(go_http.HandlerFunc(func(w go_http.ResponseWriter, r *go_http.Request) {
		methods = append(methods, r.Method)
		sawRange = r.Header.Get("Range")
		w.WriteHeader(go_http.StatusOK)
	}))
	defer origin.Close()

	result := probeOneSource(t, newTestProber(), origin.URL)

	assert.Equal(t, ProbeAlive, result.Verdict)
	assert.Equal(t, go_http.StatusOK, result.Status)
	assert.NoError(t, result.Err)
	assert.Equal(t, []string{go_http.MethodGet}, methods,
		"exactly one request, a GET — the player-equivalent verb")
	assert.Equal(t, fmt.Sprintf("bytes=0-%d", ClassifyProbeRangeBytes-1), sawRange,
		"the probe GET must be range-bounded")
}

// TestSourceProber_HTTPErrorIsDead is the #304 repro shape: the origin
// understood the player-equivalent GET and answered an error that does
// not depend on request identity.
func TestSourceProber_HTTPErrorIsDead(t *testing.T) {
	origin := httptest.NewServer(go_http.HandlerFunc(func(w go_http.ResponseWriter, _ *go_http.Request) {
		go_http.Error(w, "bad request", go_http.StatusBadRequest)
	}))
	defer origin.Close()

	result := probeOneSource(t, newTestProber(), origin.URL)

	assert.Equal(t, ProbeDead, result.Verdict)
	assert.Equal(t, go_http.StatusBadRequest, result.Status)
}

// TestSourceProber_IdentityDependentStatusIsInconclusive pins the
// verdict table's identity rule: the probe's request identity is not the
// kiosk's (no cookies, Go's default User-Agent where uarewrite rewrites
// the kiosk's for bot-challenging origins — #296 is the shipped proof a
// 403 to one client is a 200 to another), so an auth wall, bot
// challenge, or rate limit must never kill a cast the player might
// render. Same rule for 5xx: server trouble is routinely transient.
func TestSourceProber_IdentityDependentStatusIsInconclusive(t *testing.T) {
	for _, status := range []int{
		go_http.StatusUnauthorized,
		go_http.StatusForbidden,
		go_http.StatusNotAcceptable,
		go_http.StatusRequestTimeout,
		go_http.StatusTooManyRequests,
		go_http.StatusRequestedRangeNotSatisfiable,
		go_http.StatusInternalServerError,
		go_http.StatusServiceUnavailable,
	} {
		origin := httptest.NewServer(go_http.HandlerFunc(func(w go_http.ResponseWriter, _ *go_http.Request) {
			w.WriteHeader(status)
		}))
		result := probeOneSource(t, newTestProber(), origin.URL)
		origin.Close()

		assert.Equal(t, ProbeInconclusive, result.Verdict, "status %d", status)
		assert.Equal(t, status, result.Status, "status %d", status)
	}
}

func TestSourceProber_NetworkFaultIsInconclusive(t *testing.T) {
	origin := httptest.NewServer(go_http.HandlerFunc(func(w go_http.ResponseWriter, _ *go_http.Request) {
		w.WriteHeader(go_http.StatusOK)
	}))
	deadURL := origin.URL
	origin.Close() // nothing listens there any more

	result := probeOneSource(t, newTestProber(), deadURL)

	assert.Equal(t, ProbeInconclusive, result.Verdict)
	assert.Error(t, result.Err)
	assert.Zero(t, result.Status)
}

func TestSourceProber_DataURIVerdicts(t *testing.T) {
	prober := newTestProber()

	valid := probeOneSource(t, prober, "data:image/png;base64,aGVsbG8=")
	assert.Equal(t, ProbeInline, valid.Verdict)
	assert.NoError(t, valid.Err)

	// No comma anywhere: unusable to the player too, and unlike a network
	// fault it can never heal — Dead, not Inconclusive.
	malformed := probeOneSource(t, prober, "data:image/png;base64")
	assert.Equal(t, ProbeDead, malformed.Verdict)
	assert.Error(t, malformed.Err)
}

// TestSourceProber_GuardRefusalIsInconclusive pins the policy/liveness
// separation: a source the guard refuses (here the production guard, no
// override — a loopback pivot and a file: scheme) is never probed, and the
// refusal must NOT count as the origin being down — reporting it Dead would
// smuggle a security-policy change into a liveness check.
func TestSourceProber_GuardRefusalIsInconclusive(t *testing.T) {
	guard := sourceGuard{} // production predicate, no loopback relaxation
	prober := newSourceProberWith(guard, newGuardedHTTPClientFor(guard, 2*time.Second))

	loopback := probeOneSource(t, prober, "http://127.0.0.1:9222/json/new")
	assert.Equal(t, ProbeInconclusive, loopback.Verdict)
	assert.ErrorIs(t, loopback.Err, ErrUnsafeSource)

	file := probeOneSource(t, prober, "file:///etc/shadow")
	assert.Equal(t, ProbeInconclusive, file.Verdict)
	assert.ErrorIs(t, file.Err, ErrUnsafeSource)
}

func TestSourceProber_OverlongSourceIsInconclusive(t *testing.T) {
	long := "https://origin.example/" + strings.Repeat("a", MaxSourceURLBytes)

	result := probeOneSource(t, newTestProber(), long)

	assert.Equal(t, ProbeInconclusive, result.Verdict)
	assert.ErrorIs(t, result.Err, ErrSourceTooLong)
	// The display path has no admission length bound, so the result must
	// carry a truncated echo, never the full hostile string.
	assert.Less(t, len(result.Source), MaxSourceURLBytes)
	assert.Contains(t, result.Source, "…[+")
}

func TestSourceProber_ResultsKeepInputOrder(t *testing.T) {
	alive := httptest.NewServer(go_http.HandlerFunc(func(w go_http.ResponseWriter, _ *go_http.Request) {
		w.WriteHeader(go_http.StatusOK)
	}))
	defer alive.Close()
	dead := httptest.NewServer(go_http.HandlerFunc(func(w go_http.ResponseWriter, _ *go_http.Request) {
		go_http.Error(w, "gone", go_http.StatusGone)
	}))
	defer dead.Close()

	sources := []string{dead.URL, "data:text/plain,hi", alive.URL}
	results := newTestProber().ProbeSources(context.Background(), sources)

	require.Len(t, results, 3)
	assert.Equal(t, ProbeDead, results[0].Verdict)
	assert.Equal(t, go_http.StatusGone, results[0].Status)
	assert.Equal(t, ProbeInline, results[1].Verdict)
	assert.Equal(t, ProbeAlive, results[2].Verdict)
}

// TestSourceProber_SpentContextIsInconclusive pins the ceiling shape: a
// context that is already done (the degenerate form of the phase ceiling
// firing) yields Inconclusive for every item — never Dead, so a timed-out
// preflight can never kill a cast.
func TestSourceProber_SpentContextIsInconclusive(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// Far more items than worker slots, deliberately: the pool must
	// drain a large list against a spent context quickly (the O(1)
	// ctx.Err path), and every result must land Inconclusive — never
	// Dead — so a timed-out preflight can never kill a cast.
	sources := make([]string, 50)
	for i := range sources {
		sources[i] = fmt.Sprintf("https://origin.example/%d", i)
	}

	results := newTestProber().ProbeSources(ctx, sources)

	require.Len(t, results, len(sources))
	for _, r := range results {
		assert.Equal(t, ProbeInconclusive, r.Verdict)
		assert.Error(t, r.Err)
	}
}

// TestSourceProber_DuplicateSourcesProbedOnce pins the dedup: the worker
// pool bounds concurrency but not request count, so identical sources must
// share one probe — otherwise a playlist repeating one URL turns a single
// cast into transport-speed traffic against that origin.
func TestSourceProber_DuplicateSourcesProbedOnce(t *testing.T) {
	var requests atomic.Int32
	origin := httptest.NewServer(go_http.HandlerFunc(func(w go_http.ResponseWriter, _ *go_http.Request) {
		requests.Add(1)
		go_http.Error(w, "gone", go_http.StatusGone)
	}))
	defer origin.Close()

	sources := []string{origin.URL, origin.URL, origin.URL, origin.URL}
	results := newTestProber().ProbeSources(context.Background(), sources)

	assert.Equal(t, int32(1), requests.Load(), "identical sources share one probe request")
	require.Len(t, results, len(sources))
	for _, r := range results {
		assert.Equal(t, ProbeDead, r.Verdict)
		assert.Equal(t, go_http.StatusGone, r.Status)
	}
}

// TestSourceProber_FragmentVariantsShareOneProbe pins the wire-key dedup
// (#308 review): fragments never leave the client, so `#unique-N`
// suffixes name the same wire URL and must not defeat the dedup.
func TestSourceProber_FragmentVariantsShareOneProbe(t *testing.T) {
	var requests atomic.Int32
	origin := httptest.NewServer(go_http.HandlerFunc(func(w go_http.ResponseWriter, _ *go_http.Request) {
		requests.Add(1)
		go_http.Error(w, "gone", go_http.StatusGone)
	}))
	defer origin.Close()

	sources := []string{
		origin.URL + "/art#frag-1",
		origin.URL + "/art#frag-2",
		origin.URL + "/art#frag-3",
		origin.URL + "/art",
	}
	results := newTestProber().ProbeSources(context.Background(), sources)

	assert.Equal(t, int32(1), requests.Load(), "fragment variants share one wire probe")
	require.Len(t, results, len(sources))
	for _, r := range results {
		assert.Equal(t, ProbeDead, r.Verdict)
	}
}

// TestSourceProber_ProbeBudgetFailsOpen pins the per-cast attempt cap:
// wire-distinct URLs past maxProbeAttempts are never probed and land
// Inconclusive — a cast over budget can therefore never be rejected on
// the strength of unprobed items.
func TestSourceProber_ProbeBudgetFailsOpen(t *testing.T) {
	var requests atomic.Int32
	origin := httptest.NewServer(go_http.HandlerFunc(func(w go_http.ResponseWriter, _ *go_http.Request) {
		requests.Add(1)
		go_http.Error(w, "gone", go_http.StatusGone)
	}))
	defer origin.Close()

	over := 20
	sources := make([]string, maxProbeAttempts+over)
	for i := range sources {
		sources[i] = fmt.Sprintf("%s/art?n=%d", origin.URL, i)
	}
	results := newTestProber().ProbeSources(context.Background(), sources)

	assert.Equal(t, int32(maxProbeAttempts), requests.Load(), "requests stop at the budget")
	require.Len(t, results, len(sources))
	for i, r := range results {
		if i < maxProbeAttempts {
			assert.Equal(t, ProbeDead, r.Verdict, "probed item %d", i)
		} else {
			assert.Equal(t, ProbeInconclusive, r.Verdict, "over-budget item %d", i)
			assert.ErrorIs(t, r.Err, errProbeBudgetExhausted, "over-budget item %d", i)
		}
	}
}

// TestSourceProber_LongValidDataURIMetadataFailsOpen pins the #308 F4
// rule: a comma beyond the bounded metadata scan is not proof of a
// malformed URI (RFC 2397 allows long parameters), so scan exhaustion is
// Inconclusive; only a URI that fits inside the scan and still has no
// comma is proven malformed and Dead.
func TestSourceProber_LongValidDataURIMetadataFailsOpen(t *testing.T) {
	prober := newTestProber()

	longParam := strings.Repeat("a", maxDataURIMetadataBytes+64)
	longValid := "data:image/png;name=" + longParam + ",aGVsbG8="
	result := probeOneSource(t, prober, longValid)
	assert.Equal(t, ProbeInconclusive, result.Verdict,
		"a comma beyond the scan bound is unproven, not dead")
	assert.Error(t, result.Err)

	shortMalformed := "data:image/png;base64"
	proven := probeOneSource(t, prober, shortMalformed)
	assert.Equal(t, ProbeDead, proven.Verdict,
		"a short URI with no comma anywhere is proven malformed")
}

// TestSourceProber_RedirectsAreAnswersNotFollowed pins the #310 F1 fix:
// the probe never follows a redirect — the 3xx IS the answer (alive,
// fail-open; the kiosk follows it itself) — so one probed source is at
// most ONE outbound request no matter how the origin answers, and a
// redirect chain cannot multiply the per-cast budget.
func TestSourceProber_RedirectsAreAnswersNotFollowed(t *testing.T) {
	var destRequests atomic.Int32
	dest := httptest.NewServer(go_http.HandlerFunc(func(w go_http.ResponseWriter, _ *go_http.Request) {
		destRequests.Add(1)
		w.WriteHeader(go_http.StatusOK)
	}))
	defer dest.Close()

	var originRequests atomic.Int32
	origin := httptest.NewServer(go_http.HandlerFunc(func(w go_http.ResponseWriter, r *go_http.Request) {
		originRequests.Add(1)
		w.Header().Set("Location", dest.URL)
		w.WriteHeader(go_http.StatusFound)
	}))
	defer origin.Close()

	result := probeOneSource(t, newTestProber(), origin.URL)

	assert.Equal(t, ProbeAlive, result.Verdict, "a 3xx answer is alive, fail-open")
	assert.Equal(t, go_http.StatusFound, result.Status)
	assert.Equal(t, int32(1), originRequests.Load(), "exactly one request to the origin")
	assert.Zero(t, destRequests.Load(), "the redirect target is never contacted")
}

// TestSourceProber_RedirectChainCannotExceedBudget pins the request-count
// arithmetic end to end: a full budget of sources, every one answering
// with a redirect (the amplification shape from the #310 review), still
// produces exactly maxProbeAttempts outbound requests — not
// maxProbeAttempts x maxSourceRedirects.
func TestSourceProber_RedirectChainCannotExceedBudget(t *testing.T) {
	var requests atomic.Int32
	origin := httptest.NewServer(go_http.HandlerFunc(func(w go_http.ResponseWriter, r *go_http.Request) {
		requests.Add(1)
		// Self-redirect: with following enabled this would burn the whole
		// redirect allowance per source.
		w.Header().Set("Location", r.URL.Path)
		w.WriteHeader(go_http.StatusMovedPermanently)
	}))
	defer origin.Close()

	over := 10
	sources := make([]string, maxProbeAttempts+over)
	for i := range sources {
		sources[i] = fmt.Sprintf("%s/loop?n=%d", origin.URL, i)
	}
	results := newTestProber().ProbeSources(context.Background(), sources)

	assert.Equal(t, int32(maxProbeAttempts), requests.Load(),
		"total outbound requests are bounded by the probe budget, redirects included")
	require.Len(t, results, len(sources))
	for i, r := range results {
		if i < maxProbeAttempts {
			assert.Equal(t, ProbeAlive, r.Verdict, "item %d: a redirect answer is alive", i)
		} else {
			assert.Equal(t, ProbeInconclusive, r.Verdict, "item %d: over budget", i)
		}
	}
}

// TestSourceProber_NoConnectionReuseAcrossProbes pins the #311 round-2
// fix at its mechanism: every probe dials a FRESH connection (keep-alives
// disabled), because net/http replays an idempotent GET only on a REUSED
// idle connection that dies — no reuse, no replay, and the per-cast
// request budget stays literal even against an origin that seeds and
// drops idle connections.
func TestSourceProber_NoConnectionReuseAcrossProbes(t *testing.T) {
	var conns atomic.Int32
	server := httptest.NewUnstartedServer(go_http.HandlerFunc(func(w go_http.ResponseWriter, _ *go_http.Request) {
		w.WriteHeader(go_http.StatusOK)
	}))
	server.Config.ConnState = func(_ net.Conn, state go_http.ConnState) {
		if state == go_http.StateNew {
			conns.Add(1)
		}
	}
	server.Start()
	defer server.Close()

	prober := newTestProber()
	const probes = 3
	for i := 0; i < probes; i++ {
		result := probeOneSource(t, prober, fmt.Sprintf("%s/item?n=%d", server.URL, i))
		assert.Equal(t, ProbeAlive, result.Verdict, "probe %d", i)
	}
	assert.Equal(t, int32(probes), conns.Load(),
		"each probe must arrive on its own new connection — an idle connection left open would be the replay surface")
}

// TestSourceProber_DataURIsDoNotConsumeDialBudget pins the #308-round-5
// rule: inline items never dial, so they must not charge the outbound
// budget — otherwise a playlist packed with malformed data: URIs (a
// pure-CPU Dead verdict) would push a real dead HTTP source into
// budget-exhausted Inconclusive and shield an entirely unusable cast
// from rejection.
func TestSourceProber_DataURIsDoNotConsumeDialBudget(t *testing.T) {
	var requests atomic.Int32
	origin := httptest.NewServer(go_http.HandlerFunc(func(w go_http.ResponseWriter, _ *go_http.Request) {
		requests.Add(1)
		go_http.Error(w, "not found", go_http.StatusNotFound)
	}))
	defer origin.Close()

	// A full budget's worth of distinct malformed inline items, then the
	// one real HTTP source.
	sources := make([]string, 0, maxProbeAttempts+1)
	for i := 0; i < maxProbeAttempts; i++ {
		sources = append(sources, fmt.Sprintf("data:image/png;name=%d;base64", i))
	}
	sources = append(sources, origin.URL+"/dead")

	results := newTestProber().ProbeSources(context.Background(), sources)

	require.Len(t, results, maxProbeAttempts+1)
	assert.Equal(t, int32(1), requests.Load(), "the one HTTP source is still probed")
	for i, r := range results {
		assert.Equal(t, ProbeDead, r.Verdict, "item %d: every item is definitively dead", i)
	}
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

	// No query: untouched apart from the usual truncation path.
	assert.Equal(t, "https://cdn.example.com/a.html",
		redactSourceForLog("https://cdn.example.com/a.html"))

	// Fragments are dropped too (never sent on the wire; nothing to grep).
	assert.Equal(t, "https://cdn.example.com/a.html",
		redactSourceForLog("https://cdn.example.com/a.html#frag"))

	// An unparseable URL is cut at the first '?' rather than trusted.
	assert.Equal(t, "http://bad\x7f?…",
		redactSourceForLog("http://bad\x7f?X-Amz-Signature=deadbeef"))

	// Userinfo is the other credential channel in a URL and must not
	// survive either — password or token, with or without a query.
	assert.Equal(t, "https://cdn.example.com/a.html",
		redactSourceForLog("https://id:secret@cdn.example.com/a.html"))
	got = redactSourceForLog("https://token@cdn.example.com/a.html?sig=deadbeef")
	assert.Equal(t, "https://cdn.example.com/a.html?…", got)
	assert.NotContains(t, got, "token")

	// data: URIs keep the pre-existing bounded-prefix logging behavior.
	assert.Equal(t, "data:text/html,hello",
		redactSourceForLog("data:text/html,hello"))

	// Redaction happens BEFORE truncation, so a signed URL with a huge
	// query never leaks its head into the truncated form.
	huge := "https://cdn.example.com/p.html?sig=" + strings.Repeat("s", maxLoggedSourceBytes*2)
	assert.Equal(t, "https://cdn.example.com/p.html?…", redactSourceForLog(huge))
}
