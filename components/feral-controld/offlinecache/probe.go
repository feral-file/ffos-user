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
	// ProbeDead: a definitive verdict that the item cannot load — the
	// origin ANSWERED the player-equivalent GET with an HTTP error that
	// does not depend on who is asking (see verdictForStatus for which
	// statuses qualify), or the source is a malformed data: URI, which
	// the player cannot render either and which, unlike a network fault,
	// can never heal on its own.
	ProbeDead SourceProbeVerdict = "dead"
	// ProbeInconclusive: no definitive answer — a network error, a
	// timeout, the phase ceiling, a guard refusal, an over-long URL, or
	// an HTTP status whose meaning depends on request identity or server
	// health (401/403/407/429, and all 5xx — see verdictForStatus).
	// Deliberately NOT dead: an offline device casting a fully-cached
	// playlist (the displayPlaylist cached-copy fallback) probes this
	// way for every item and must keep working, so callers treat
	// inconclusive exactly like alive when deciding a cast's fate.
	ProbeInconclusive SourceProbeVerdict = "inconclusive"
	// ProbeInline: a well-formed data: URI — its bytes travel inside the
	// playlist body, there is nothing to dial, and it can always "load".
	ProbeInline SourceProbeVerdict = "inline"
)

// SourceProbeResult is one item source's probe outcome.
//
// Source is truncated via truncateSourceForLog at construction and is
// for the DAEMON LOG ONLY. It must never ride an error message or any
// response to a caster: resolved item sources are playlist content a
// playlistUrl or dynamic-playlist caller never supplied, and signed CDN
// URLs carry credentials in their query strings — the controller
// contract requires sanitized error messages. Callers report items by
// index and status instead (see commandrouter.SourceUnreachableError).
type SourceProbeResult struct {
	// Source is the probed URL, truncated, for daemon-local logging only
	// — see the struct doc's sanitization rule.
	Source string
	// Verdict is the probe outcome; see the SourceProbeVerdict values.
	Verdict SourceProbeVerdict
	// Status is the HTTP status behind the verdict when a response was
	// obtained (Alive, Dead, and the identity-dependent/5xx Inconclusive
	// cases), 0 when none was (Inline, network-fault Inconclusive,
	// malformed data:).
	Status int
	// Err carries why no definitive answer was obtained (or why a data:
	// URI was judged Dead). nil for a plain HTTP answer.
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

// probeItemTimeout bounds ONE source's COMPLETE probe: probeOne derives
// a child context with this deadline and runs the guard's DNS
// resolution and the single ranged GET under it, so no item can consume
// more than this from the phase budget no matter which stage stalls
// (the client's own Timeout carries the same value as a backstop).
// Deliberately tighter than classifyItemTimeout: classification decides
// whether an item is cacheable and can afford patience, while this
// probe sits on the synchronous cast path where a slow origin must cost
// seconds, not tens of them — and a slow origin is Inconclusive, which
// counts in the cast's favor, so cutting a straggler short never kills
// a good cast.
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
// the same bottleneck would just drift apart. It is also the TOTAL
// goroutine bound for a probe pass: ProbeSources runs a fixed worker
// pool of this size over an index feed, never a goroutine per item —
// the unauthenticated hub accepts a 4 MiB playlist body with no item
// cap, so per-item goroutines would let one hostile cast queue tens of
// thousands of stacks on a constrained device.
const probeConcurrency = classifyConcurrency

func (p *sourceProber) ProbeSources(ctx context.Context, sources []string) []SourceProbeResult {
	results := make([]SourceProbeResult, len(sources))
	ctx, cancel := context.WithTimeout(ctx, probePhaseCeiling)
	defer cancel()

	workers := probeConcurrency
	if len(sources) < workers {
		workers = len(sources)
	}
	indices := make(chan int)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range indices {
				// Ceiling already hit: never probed, so never dead. The
				// check keeps the drain O(1) per remaining item instead
				// of each one burning a doomed dial.
				if ctx.Err() != nil {
					results[i] = SourceProbeResult{
						Source:  truncateSourceForLog(sources[i]),
						Verdict: ProbeInconclusive,
						Err:     ctx.Err(),
					}
					continue
				}
				results[i] = p.probeOne(ctx, sources[i])
			}
		}()
	}
	for i := range sources {
		indices <- i
	}
	close(indices)
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

	// One deadline for the item's WHOLE probe — guard DNS resolution
	// included, which otherwise runs before any client timeout applies.
	ctx, cancel := context.WithTimeout(ctx, probeItemTimeout)
	defer cancel()

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

	status, err := p.rangedGETStatus(ctx, source)
	if err != nil {
		result.Verdict, result.Err = ProbeInconclusive, err
		return result
	}
	result.Status = status
	result.Verdict = verdictForStatus(status)
	return result
}

// verdictForStatus maps a player-equivalent GET's status to a verdict.
//
// Only statuses whose meaning does not depend on who is asking or on
// transient server health may be Dead — the probe's request identity is
// NOT the kiosk's (no cookies, and Go's default User-Agent where the
// kiosk's uarewrite policy deliberately rewrites the UA for
// bot-challenging origins like ipfs.io, see that package), so:
//
//   - 401/403/407/429 are Inconclusive: an auth wall, a bot challenge,
//     or a rate limit can answer differently for the player, and #296
//     is the shipped proof that a 403 to one client is a 200 to
//     another. Judging these dead would reject casts the kiosk renders.
//   - 408 is Inconclusive: a request timeout is transient by
//     definition, same footing as a network-level timeout.
//   - 416 is Inconclusive because it is PROBE-SHAPE-dependent, not
//     identity-dependent: the probe adds a Range header the player's
//     document fetch does not send, so a zero-length or range-hostile
//     resource can 416 the probe while serving the player fine.
//   - 5xx is Inconclusive: server trouble is routinely transient, and
//     the fail-open bias says a maybe-down origin does not kill a cast.
//   - The remaining 4xx (400, 404, 410, ...) are Dead: the origin
//     understood the request and said the resource is not there for
//     anyone. This is #304's repro class.
func verdictForStatus(status int) SourceProbeVerdict {
	switch {
	case status < http.StatusBadRequest:
		return ProbeAlive
	case status == http.StatusUnauthorized,
		status == http.StatusForbidden,
		status == http.StatusProxyAuthRequired,
		status == http.StatusRequestTimeout,
		status == http.StatusTooManyRequests,
		status == http.StatusRequestedRangeNotSatisfiable:
		return ProbeInconclusive
	case status < http.StatusInternalServerError:
		return ProbeDead
	default:
		return ProbeInconclusive
	}
}

// rangedGETStatus performs the single, decisive probe request: a GET,
// because that is what the player's renderer actually issues (a HEAD
// verdict can disagree with the GET the artwork will get — origins
// exist that 200 a HEAD and 4xx the GET, and vice versa), range-bounded
// so the daemon never pulls an asset body just to read a status line.
// Origins that ignore Range (200 with the full body) are still bounded:
// the body is drained through a capped io.CopyN before the connection
// is torn down — same shape as rangedGETClassify.
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

	_, _ = io.CopyN(io.Discard, resp.Body, ClassifyProbeRangeBytes)

	// A 206 answer means the asset is loadable; StatusCode is already
	// what verdictForStatus needs (206 < 400).
	return resp.StatusCode, nil
}
