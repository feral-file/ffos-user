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
	go_http "net/http"
	"net/http/httptest"
	"strings"
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
	return newSourceProberWith(guard, newGuardedHTTPClientFor(guard, 2*time.Second))
}

func probeOneSource(t *testing.T, prober SourceProber, source string) SourceProbeResult {
	t.Helper()
	results := prober.ProbeSources(context.Background(), []string{source})
	require.Len(t, results, 1)
	return results[0]
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
