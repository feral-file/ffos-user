package offlinecache

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

// probe.go answers one question for the DISPLAY path — "does this item's
// source answer HTTP right now?" — at cast-accept time, so a playlist
// whose every source is definitively dead fails the cast loudly instead
// of being forwarded to the player and reported as playing
// (feral-file/ffos-user#304). The player cannot make this call itself: a
// cross-origin iframe pointed at an HTTP error renders the error body
// and fires load normally, so the daemon is the only place the status
// code is observable.
//
// It lives in THIS package, not a new one, because a probe dials
// untrusted playlist-supplied URLs (the same input the source guard's
// threat model describes — see ErrUnsafeSource) and this package owns
// the machinery that makes that safe: the guard, the guarded transport,
// and the log-truncation bound.

// SourceProbeVerdict is the outcome of probing one item source.
type SourceProbeVerdict string

const (
	// ProbeAlive: the origin answered with a non-error status.
	ProbeAlive SourceProbeVerdict = "alive"
	// ProbeDead: the origin ANSWERED, and the answer was an HTTP error
	// (>= 400) — the one verdict definitive enough to count against a
	// cast. Also used for a malformed data: URI, which the player cannot
	// render either.
	ProbeDead SourceProbeVerdict = "dead"
	// ProbeInconclusive: no definitive answer — a network error, a
	// timeout, the phase ceiling, a guard refusal, or an over-long URL.
	// Deliberately NOT dead: an offline device casting a fully-cached
	// playlist (the displayPlaylist cached-copy fallback) probes this
	// way for every item and must keep working, so callers treat
	// inconclusive exactly like alive when deciding a cast's fate.
	ProbeInconclusive SourceProbeVerdict = "inconclusive"
	// ProbeInline: a data: URI — its bytes travel inside the playlist
	// body, there is nothing to dial, and it can always "load".
	ProbeInline SourceProbeVerdict = "inline"
)

// SourceProbeResult is one item source's probe outcome. Results are
// safe to log and to echo in a command error as they are: Source is
// already truncated via truncateSourceForLog at construction, because
// the display path has no admission length bound the way the download
// path does (MaxSourceURLBytes), so an unbounded hostile URL would
// otherwise ride the error into logs and RPC replies.
type SourceProbeResult struct {
	// Source is the probed URL, truncated for logging/echoing.
	Source string
	// Verdict is the probe outcome; see the SourceProbeVerdict values.
	Verdict SourceProbeVerdict
	// Status is the HTTP status behind an Alive or Dead verdict, 0 when
	// no response was obtained (Inline, Inconclusive, malformed data:).
	Status int
	// Err carries why a probe was Inconclusive (or why a data: URI was
	// judged Dead). nil for a plain HTTP answer.
	Err error
}

//go:generate mockgen -source=probe.go -destination=../mocks/offlinecache_probe.go -package=mocks -mock_names=SourceProber=MockSourceProber
type SourceProber interface {
	// ProbeSources probes every source concurrently and returns one
	// result per source, in input order. It never fails as a whole:
	// anything that prevents a definitive answer is that item's
	// Inconclusive result, so the caller always gets a full slice back
	// within probePhaseCeiling.
	ProbeSources(ctx context.Context, sources []string) []SourceProbeResult
}

type sourceProber struct {
	httpClient wrapper.HTTPClient
	guard      sourceGuard
}

// NewSourceProber builds the cast-time source prober. resolver is the
// DNS seam the source guard uses — pass net.DefaultResolver in
// production (see ErrUnsafeSource and bootstrap.go's classifier
// construction for why the system resolver specifically).
//
// The HTTP client is built here rather than taken from the caller so
// the one thing that must always be true — that these fetches ride the
// guarded transport, never the daemon-wide client — cannot depend on
// every call site remembering it.
func NewSourceProber(resolver AddrResolver) SourceProber {
	guard := sourceGuard{resolver: resolver}
	return newSourceProberWith(guard, newGuardedHTTPClientFor(guard, probeItemTimeout))
}

// newSourceProberWith is NewSourceProber with the guard and client
// supplied directly, so a test can hand in a guard carrying the
// isReserved override its httptest servers need.
func newSourceProberWith(guard sourceGuard, httpClient wrapper.HTTPClient) SourceProber {
	return &sourceProber{httpClient: httpClient, guard: guard}
}

// probeItemTimeout bounds ONE source's probe (HEAD plus a possible
// ranged-GET fallback: it is the guarded client's whole-request
// timeout, and both requests ride that client). Deliberately tighter
// than classifyItemTimeout: classification decides whether an item is
// cacheable and can afford patience, while this probe sits on the
// synchronous cast path where a slow origin must cost seconds, not tens
// of them — and a slow origin is Inconclusive, which counts in the
// cast's favor, so cutting a straggler short never kills a good cast.
const probeItemTimeout = 5 * time.Second

// probePhaseCeiling is the bound on the whole preflight. Same
// two-nested-bounds shape as classifyItemTimeout/classifyPhaseCeiling
// and for the same reason (a single shared budget silently starves the
// tail of a large playlist): the per-item timeout decides an item's
// fate, this ceiling only bounds the PROBE'S OWN share of the cast
// request's time budget. It deliberately does NOT guarantee the whole
// command answers inside the hub's 30s write deadline: the DP-1
// resolution that precedes the probe on the same request rides the
// daemon client's own 30s per-request timeout (multiple sequential
// fetches for dynamic playlists) and nothing bounds the sum — the hub
// hands Process a deadline-less context, so no downstream stage can see
// the write deadline to subdivide it. What the ceiling does buy: a cast
// that previously answered with T seconds to spare still answers with
// T-10 to spare, worst case, and a probe against a black-holing origin
// can never hang the command. Items still in flight at the ceiling land
// Inconclusive, in the cast's favor.
const probePhaseCeiling = 10 * time.Second

// probeConcurrency reuses classifyConcurrency deliberately: same
// origins, same uplink, same measured sweet spot (see that constant's
// on-device numbers) — two independently-tuned fan-out levers against
// the same bottleneck would just drift apart.
const probeConcurrency = classifyConcurrency

func (p *sourceProber) ProbeSources(ctx context.Context, sources []string) []SourceProbeResult {
	results := make([]SourceProbeResult, len(sources))
	ctx, cancel := context.WithTimeout(ctx, probePhaseCeiling)
	defer cancel()

	sem := make(chan struct{}, probeConcurrency)
	var wg sync.WaitGroup
	for i, source := range sources {
		wg.Add(1)
		go func(i int, source string) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				// Ceiling hit while queued: never probed, so never dead.
				results[i] = SourceProbeResult{
					Source:  truncateSourceForLog(source),
					Verdict: ProbeInconclusive,
					Err:     ctx.Err(),
				}
				return
			}
			results[i] = p.probeOne(ctx, source)
		}(i, source)
	}
	wg.Wait()
	return results
}

func (p *sourceProber) probeOne(ctx context.Context, source string) SourceProbeResult {
	result := SourceProbeResult{Source: truncateSourceForLog(source)}

	// data: first, ahead of the guard, mirroring Classify: these are
	// never dialed and the guard deliberately refuses them. A malformed
	// one is Dead, not Inconclusive — the player cannot render it either,
	// and unlike a network fault this can never heal on its own.
	if isDataURI(source) {
		if _, err := dataURIMediaType(source); err != nil {
			result.Verdict, result.Err = ProbeDead, err
			return result
		}
		result.Verdict = ProbeInline
		return result
	}

	// An over-long URL is refused without dialing (same bound and
	// rationale as MaxSourceURLBytes) but judged Inconclusive, not Dead:
	// the objection is what holding the string costs, not whether the
	// origin is up.
	if err := checkSourceLength(source); err != nil {
		result.Verdict, result.Err = ProbeInconclusive, err
		return result
	}

	// A guard refusal is a POLICY verdict, not a liveness one: the
	// display path forwards such a source to the player today (whether
	// it should is #3471's territory, not this probe's), so reporting
	// it Dead would smuggle a security-policy change into a liveness
	// check. Inconclusive keeps the cast's behavior unchanged while the
	// refusal still lands in the log.
	if err := p.guard.check(ctx, source); err != nil {
		result.Verdict, result.Err = ProbeInconclusive, err
		return result
	}

	status, err := p.headStatus(ctx, source)
	if err != nil {
		result.Verdict, result.Err = ProbeInconclusive, err
		return result
	}
	if status < http.StatusBadRequest {
		result.Verdict, result.Status = ProbeAlive, status
		return result
	}

	// HEAD answered >= 400. Some origins reject HEAD as such (405, or a
	// blanket 4xx), so a GET must confirm before the item counts as dead
	// — same fallback shape as Classify's headClassify/rangedGETClassify
	// pair, and range-bounded for the same reason (never pull a multi-GB
	// asset through the daemon to learn a status code).
	status, err = p.rangedGETStatus(ctx, source)
	if err != nil {
		result.Verdict, result.Err = ProbeInconclusive, err
		return result
	}
	result.Status = status
	if status < http.StatusBadRequest {
		result.Verdict = ProbeAlive
	} else {
		result.Verdict = ProbeDead
	}
	return result
}

func (p *sourceProber) headStatus(ctx context.Context, url string) (int, error) {
	req, err := p.httpClient.NewRequest(http.MethodHead, url, nil)
	if err != nil {
		return 0, fmt.Errorf("source probe: build HEAD request: %w", err)
	}
	req = req.WithContext(ctx)

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return 0, fmt.Errorf("source probe: HEAD request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode, nil
}

func (p *sourceProber) rangedGETStatus(ctx context.Context, url string) (int, error) {
	req, err := p.httpClient.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return 0, fmt.Errorf("source probe: build GET request: %w", err)
	}
	req = req.WithContext(ctx)
	req.Header.Set("Range", fmt.Sprintf("bytes=0-%d", ClassifyProbeRangeBytes-1))

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return 0, fmt.Errorf("source probe: GET request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// Bound the read whether the origin honored Range (206) or ignored
	// it (200) — see rangedGETClassify's identical drain.
	_, _ = io.CopyN(io.Discard, resp.Body, ClassifyProbeRangeBytes)

	// A 206 answer to a ranged GET means the asset itself is loadable;
	// StatusCode is already what the verdict needs (206 < 400).
	return resp.StatusCode, nil
}
