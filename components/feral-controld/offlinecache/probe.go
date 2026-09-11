package offlinecache

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	go_url "net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/semaphore"

	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

// errProbeBudgetExhausted marks a source the preflight never probed
// because the cast already spent maxProbeAttempts distinct wire URLs.
// Always an Inconclusive verdict — unprobed can never be dead.
var errProbeBudgetExhausted = errors.New("source probe: per-cast probe budget exhausted")

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
	// ProbeAlive: the origin answered with a non-error status. A 3xx
	// counts: the probe never follows redirects (see
	// newGuardedNoRedirectHTTPClientFor), so a redirect answer means
	// "answered, delegates elsewhere" — fail-open, the kiosk's own fetch
	// follows it.
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
	// ProbeInline: a data: URI with well-formed RFC 2397 METADATA — its
	// bytes travel inside the playlist body and there is nothing to
	// dial. The payload bytes themselves are deliberately NOT validated
	// (a base64 payload may legitimately be percent-encoded, so a naive
	// charset check would false-reject valid artwork — the worst
	// direction — and full fidelity means reimplementing the browser's
	// percent-decode-then-base64 pipeline); an inline item's bytes are
	// also the CASTER'S OWN, so unlike a remote origin there is no
	// information asymmetry for a probe to correct. A garbage payload
	// therefore probes Inline and fails in the player, exactly as it
	// did before the preflight existed.
	ProbeInline SourceProbeVerdict = "inline"
)

// SourceProbeResult is one item source's probe outcome.
//
// Source is query-redacted and truncated via redactSourceForLog at
// construction and is for the DAEMON LOG ONLY. It must never ride an
// error message or any response to a caster: resolved item sources are
// playlist content a playlistUrl or dynamic-playlist caller never
// supplied, and signed CDN URLs carry credentials in their query
// strings — the controller contract requires sanitized error messages.
// The redaction also protects the log itself, because uploadLogs ships
// the daemon log off-device in the support bundle. Callers report items
// by index and status instead (see commandrouter.SourceUnreachableError).
type SourceProbeResult struct {
	// Source is the probed URL, query-redacted and truncated, for
	// daemon-local logging only — see the struct doc's sanitization rule.
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
	httpClient  wrapper.HTTPClient
	guard       sourceGuard
	headerSlots *semaphore.Weighted
}

const (
	// maxAggregateProbeHeaderBytes is the process-wide ceiling charged for
	// peer-controlled TLS and response-header parsing during cast-time source
	// preflights. It is independent of the operator-configurable command storm
	// gate: disabling or widening that gate must not widen this bound.
	maxAggregateProbeHeaderBytes int64 = 8 << 20

	// The status-only probe parser accepts at most this many response bytes and
	// fields across informational plus final header blocks. It discards fields
	// through a fixed reader instead of materializing net/http's MIMEHeader map,
	// so many tiny distinct names cannot amplify heap use.
	maxProbeResponseHeaderBytes  int64 = 64 << 10
	maxProbeResponseHeaderFields       = 128
	maxProbeStatusLineBytes            = 1024
	probeResponseReadBufferBytes       = 4 << 10
	maxProbeHeaderBytesPerSlot         = maxProbeResponseHeaderBytes
	maxProbeTLSHandshakeBytes    int64 = 64 << 10

	// One budget unit charges four times the accepted response-header bytes.
	// Plain HTTP spends one unit. HTTPS spends four: its entire peer handshake is
	// capped at 64 KiB before crypto/tls can parse a certificate, and the 1 MiB
	// charge leaves 16x that input for x509 structures, response parsing, fixed
	// buffers, and allocator overhead. The semaphore accounts mixed HTTP/HTTPS
	// waves in these units instead of pretending their parser costs are equal.
	probeHeaderBudgetHeadroomFactor  int64 = 4
	maxProbeHeaderBudgetBytesPerSlot       = probeHeaderBudgetHeadroomFactor *
		maxProbeHeaderBytesPerSlot
	probeHTTPSHeaderBudgetSlots int64 = 4
	maxProbeHTTPSBudgetBytes          = probeHTTPSHeaderBudgetSlots *
		maxProbeHeaderBudgetBytesPerSlot

	// maxConcurrentHeaderProbes couples the aggregate ceiling to the guarded
	// status parser's per-response budget. Every worker acquires one shared slot
	// before it can issue the ranged GET and holds it until the response body is
	// closed. With the current values, 32 units admit at most 32 HTTP parsers or
	// eight TLS parsers inside the same 8 MiB envelope.
	maxConcurrentHeaderProbes = maxAggregateProbeHeaderBytes / maxProbeHeaderBudgetBytesPerSlot
)

var processSourceProbeHeaderSlots = semaphore.NewWeighted(maxConcurrentHeaderProbes)

// NewSourceProber builds the cast-time source prober. resolver is the
// DNS seam the source guard uses — pass net.DefaultResolver in
// production (see ErrUnsafeSource and bootstrap.go's classifier
// construction for why the system resolver specifically).
//
// The HTTP client is built here rather than taken from the caller so
// the one thing that must always be true — that these fetches ride the
// guarded status-only client, never the daemon-wide client — cannot depend on
// every call site remembering it.
func NewSourceProber(resolver AddrResolver) SourceProber {
	guard := sourceGuard{resolver: resolver}
	// No-redirect client on purpose: the probe's request-count bound is
	// ONE request per probed source, hard — following would multiply the
	// per-cast budget by up to maxSourceRedirects against attacker-chosen
	// origins. A 3xx answer is an answer (< 400 → alive, fail-open); the
	// kiosk follows it itself. See newGuardedNoRedirectHTTPClientFor.
	return newSourceProberWith(guard, newGuardedNoRedirectHTTPClientFor(guard, probeItemTimeout))
}

// newSourceProberWith is NewSourceProber with the guard and client
// supplied directly, so a test can hand in a guard carrying the
// isReserved override its httptest servers need.
func newSourceProberWith(guard sourceGuard, httpClient wrapper.HTTPClient) SourceProber {
	return &sourceProber{
		httpClient:  httpClient,
		guard:       guard,
		headerSlots: processSourceProbeHeaderSlots,
	}
}

// newSourceProberWithHeaderSlots gives a white-box test its own small budget;
// production constructors always share processSourceProbeHeaderSlots.
func newSourceProberWithHeaderSlots(guard sourceGuard, httpClient wrapper.HTTPClient, slots int64) SourceProber {
	if slots < 1 {
		slots = 1
	}
	return &sourceProber{
		httpClient:  httpClient,
		guard:       guard,
		headerSlots: semaphore.NewWeighted(slots),
	}
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

// maxProbeAttempts caps how many DISTINCT DIALING wire URLs one cast may
// probe. data: URIs are settled synchronously outside this budget — they
// never dial, and letting them consume attempts would let inline items
// shield a real dead HTTP source from its verdict (see the dedup pass).
// The worker pool bounds concurrency and the ceiling bounds duration, but
// neither bounds request COUNT against fast origins — and dedup alone is
// not a bound either, since a hostile playlist can mint endless
// wire-distinct URLs (query variants) against one target. Sources past
// the cap are Inconclusive (fail open, never dead). 256 comfortably
// covers real playlists (the dynamic-resolution path caps items at 255)
// while keeping the worst case a burst, not a stream.
const maxProbeAttempts = 256

// probeWireKey is the dedup key for one source: the URL as it would
// appear ON THE WIRE. Fragments never leave the client (HTTP strips
// them), so keying on the raw string would let `#unique-N` suffixes
// defeat the dedup and turn one target into N probes. A string that
// does not parse as a URL keys as itself — it will fail the guard
// before dialing anyway.
func probeWireKey(source string) string {
	u, err := go_url.Parse(source)
	if err != nil {
		return source
	}
	u.Fragment = ""
	return u.String()
}

func (p *sourceProber) ProbeSources(ctx context.Context, sources []string) []SourceProbeResult {
	results := make([]SourceProbeResult, len(sources))
	ctx, cancel := context.WithTimeout(ctx, probePhaseCeiling)
	defer cancel()

	// Wire-identical sources are probed ONCE and share the verdict.
	// Semantically free (same wire URL, same answer, within one probe
	// pass), and it removes the request amplifier the worker pool alone
	// does not: the pool bounds concurrency, not request COUNT, so a
	// hostile 4 MiB playlist repeating one tiny public URL thousands of
	// times would otherwise turn a single unauthenticated cast into
	// transport-speed traffic against that origin for the whole phase
	// ceiling. Keyed on probeWireKey, not the raw string — see its doc.
	// maxProbeAttempts is the second half of the same bound, for
	// wire-DISTINCT floods.
	// Capacity hints are capped at what the budget can actually admit:
	// sizing them to the raw item count would hand an attacker-sized
	// allocation to a playlist that repeats one tiny URL (#308 review).
	firstIndex := make(map[string]int, min(len(sources), maxProbeAttempts))
	keyOf := make([]string, len(sources))
	uniqueIndices := make([]int, 0, min(len(sources), maxProbeAttempts))
	for i, source := range sources {
		key := probeWireKey(source)
		keyOf[i] = key
		if _, seen := firstIndex[key]; seen {
			continue
		}
		// data: URIs are settled synchronously and OUTSIDE the budget:
		// they never dial, so charging them attempts would let a
		// playlist packed with malformed inline items (a pure-CPU Dead
		// verdict) exhaust the budget and push a real dead HTTP source
		// into Inconclusive — forwarding an entirely unusable cast past
		// the rejection contract (#308 review round 5). probeOne's data:
		// branch is a bounded metadata scan, no I/O, so settling it here
		// costs what the dedup pass already costs.
		if isDataURI(source) {
			firstIndex[key] = i
			results[i] = p.probeOne(ctx, source)
			continue
		}
		if len(uniqueIndices) == maxProbeAttempts {
			// Over budget: this source is never probed, so it can never
			// be dead. Not entered into firstIndex either — later
			// duplicates of it fall through to the same Inconclusive.
			results[i] = SourceProbeResult{
				Source:  redactSourceForLog(source),
				Verdict: ProbeInconclusive,
				Err:     errProbeBudgetExhausted,
			}
			continue
		}
		firstIndex[key] = i
		uniqueIndices = append(uniqueIndices, i)
	}

	workers := probeConcurrency
	if len(uniqueIndices) < workers {
		workers = len(uniqueIndices)
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
						Source:  redactSourceForLog(sources[i]),
						Verdict: ProbeInconclusive,
						Err:     ctx.Err(),
					}
					continue
				}
				results[i] = p.probeOneWithinHeaderBudget(ctx, sources[i])
			}
		}()
	}
	for _, i := range uniqueIndices {
		indices <- i
	}
	close(indices)
	wg.Wait()

	// Fan the unique verdicts back out to the duplicate positions, after
	// the barrier so every read sees a settled result. Over-budget keys
	// are absent from firstIndex — every occurrence already wrote its
	// own Inconclusive in the dedup pass above.
	for i := range sources {
		if first, probed := firstIndex[keyOf[i]]; probed && first != i {
			results[i] = results[first]
		}
	}
	return results
}

// probeOneWithinHeaderBudget admits the only part of a source preflight that
// can retain response headers. headerSlots is shared by every production
// SourceProber instance, so concurrent casts cannot multiply the memory bound
// by widening or disabling the command storm gate. Waiting past the phase
// ceiling is Inconclusive, preserving the preflight's fail-open contract.
func (p *sourceProber) probeOneWithinHeaderBudget(ctx context.Context, source string) SourceProbeResult {
	weight := probeHeaderBudgetWeight(source)
	if err := p.headerSlots.Acquire(ctx, weight); err != nil {
		return SourceProbeResult{
			Source:  redactSourceForLog(source),
			Verdict: ProbeInconclusive,
			Err:     err,
		}
	}
	defer p.headerSlots.Release(weight)
	return p.probeOne(ctx, source)
}

func probeHeaderBudgetWeight(source string) int64 {
	u, err := go_url.Parse(source)
	if err == nil && strings.EqualFold(u.Scheme, "https") {
		return probeHTTPSHeaderBudgetSlots
	}
	return 1
}

// redactSourceForLog prepares a source URL for the daemon log: the query
// string and fragment are dropped before the usual truncation. The daemon log
// leaves the device in uploadLogs support bundles, and signed CDN URLs carry
// their credentials (e.g. X-Amz-Signature) in the query string — truncation
// alone is not a redaction, because a presigned URL's credential parameters
// commonly sit inside the first 256 bytes. Scheme, host, and path survive,
// which is what an operator greps a probe failure by; a trailing "?…" marks
// that a query was removed. A URL that does not parse is cut at the first
// '?' rather than trusted to be credential-free. data: URIs pass straight to
// truncation — their bounded prefix is content, not a credential carrier,
// matching how the rest of this package logs them.
func redactSourceForLog(source string) string {
	if isDataURI(source) {
		return truncateSourceForLog(source)
	}
	u, err := go_url.Parse(source)
	if err != nil {
		if i := strings.IndexByte(source, '?'); i >= 0 {
			return truncateSourceForLog(source[:i] + "?…")
		}
		return truncateSourceForLog(source)
	}
	hadQuery := u.RawQuery != "" || u.ForceQuery
	u.RawQuery = ""
	u.ForceQuery = false
	u.Fragment = ""
	u.RawFragment = ""
	// Userinfo is the other standard credential channel in a URL
	// (https://id:secret@host/...); u.String() would render it verbatim,
	// password included, so it is dropped along with the query.
	u.User = nil
	redacted := u.String()
	if hadQuery {
		redacted += "?…"
	}
	return truncateSourceForLog(redacted)
}

func (p *sourceProber) probeOne(ctx context.Context, source string) SourceProbeResult {
	result := SourceProbeResult{Source: redactSourceForLog(source)}

	// data: first, ahead of the guard, mirroring Classify: these are
	// never dialed and the guard deliberately refuses them. Malformed
	// METADATA is Dead only when it is PROVEN malformed: the whole URI
	// fits inside the metadata scan bound and still has no comma, so no
	// valid RFC 2397 reading of it exists — the player cannot parse it
	// either, and unlike a network fault this can never heal. A comma
	// that merely sits BEYOND the bounded scan is not proof of anything
	// (RFC 2397 allows long parameters before the delimiter), so
	// scan-limit exhaustion fails open as Inconclusive. Payload bytes
	// are deliberately not validated — see ProbeInline's doc for why.
	if isDataURI(source) {
		if _, err := dataURIMediaType(source); err != nil {
			if len(source) > len(dataURIScheme)+maxDataURIMetadataBytes {
				result.Verdict, result.Err = ProbeInconclusive, err
			} else {
				result.Verdict, result.Err = ProbeDead, err
			}
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
//   - 406 is Inconclusive: content negotiation answers the probe's
//     request headers, not the kiosk's — an origin can refuse the
//     probe's representation while serving one Chromium renders.
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
		status == http.StatusNotAcceptable,
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
// Origins that ignore Range (200 with the full body) are still bounded: the
// status-only client scans and discards a bounded HTTP/1 header block, closes
// the socket before the body, and returns an empty response shell. It never
// constructs a header map or parses trailers. Redirects are never followed
// because Location is discarded with every other field (the 3xx status itself
// is the answer), so "single request" is literal: one probed source is at most
// one outbound request, no matter what the origin answers.
//
// The Range header is the one deliberate deviation from
// player-equivalence, kept because it is the bound on what a hostile
// origin can make the daemon receive (a compliant origin answers with at
// most this range). The local read bound comes from closing the one-shot socket
// immediately after the status-only header scan. The verdict table pays for
// the deviation where it can bite: 416, the one status Range itself can
// provoke, is Inconclusive.
// Residual, accepted: an edge/WAF that 400s ranged or Go-UA requests
// wholesale reads as dead; it only affects a cast when EVERY item sits
// behind such an edge, and fail-open on 400 would give up the #304
// repro class itself.
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

	// A 206 answer means the asset is loadable; StatusCode is already
	// what verdictForStatus needs (206 < 400).
	return resp.StatusCode, nil
}
