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

func TestSourceProber_HealthyOriginIsAlive(t *testing.T) {
	origin := httptest.NewServer(go_http.HandlerFunc(func(w go_http.ResponseWriter, _ *go_http.Request) {
		w.WriteHeader(go_http.StatusOK)
	}))
	defer origin.Close()

	result := probeOneSource(t, newTestProber(), origin.URL)

	assert.Equal(t, ProbeAlive, result.Verdict)
	assert.Equal(t, go_http.StatusOK, result.Status)
	assert.NoError(t, result.Err)
}

// TestSourceProber_HeadRejectingOriginIsAlive pins the GET fallback: an
// origin that answers HEAD with an error but serves GET fine (a real CDN
// shape — see headClassify's rationale) must not be judged dead, and the
// confirming GET must be range-bounded so the probe never pulls an asset
// body through the daemon.
func TestSourceProber_HeadRejectingOriginIsAlive(t *testing.T) {
	var sawRange string
	origin := httptest.NewServer(go_http.HandlerFunc(func(w go_http.ResponseWriter, r *go_http.Request) {
		if r.Method == go_http.MethodHead {
			w.WriteHeader(go_http.StatusMethodNotAllowed)
			return
		}
		sawRange = r.Header.Get("Range")
		w.WriteHeader(go_http.StatusOK)
	}))
	defer origin.Close()

	result := probeOneSource(t, newTestProber(), origin.URL)

	assert.Equal(t, ProbeAlive, result.Verdict)
	assert.Equal(t, go_http.StatusOK, result.Status)
	assert.Equal(t, fmt.Sprintf("bytes=0-%d", ClassifyProbeRangeBytes-1), sawRange,
		"the confirming GET must be range-bounded")
}

// TestSourceProber_HTTPErrorOnBothVerbsIsDead is the #304 repro shape: an
// origin that answers — with an error — for both HEAD and the confirming
// GET is the one definitive Dead verdict this prober can issue.
func TestSourceProber_HTTPErrorOnBothVerbsIsDead(t *testing.T) {
	origin := httptest.NewServer(go_http.HandlerFunc(func(w go_http.ResponseWriter, _ *go_http.Request) {
		go_http.Error(w, "bad request", go_http.StatusBadRequest)
	}))
	defer origin.Close()

	result := probeOneSource(t, newTestProber(), origin.URL)

	assert.Equal(t, ProbeDead, result.Verdict)
	assert.Equal(t, go_http.StatusBadRequest, result.Status)
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

	results := newTestProber().ProbeSources(ctx, []string{
		"https://origin.example/a", "https://origin.example/b",
	})

	require.Len(t, results, 2)
	for _, r := range results {
		assert.Equal(t, ProbeInconclusive, r.Verdict)
		assert.Error(t, r.Err)
	}
}
