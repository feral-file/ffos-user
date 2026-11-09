package mediahealth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Verdict is the outcome of probing one item source, in the vocabulary the
// cast admission rule (commandrouter) needs.
type Verdict string

const (
	// VerdictAlive: the origin answered and the bytes passed L0 (or no
	// content rule applied). A 3xx that the guarded redirect chain could
	// not resolve is NOT alive — see ProbeOne.
	VerdictAlive Verdict = "alive"
	// VerdictDead: a conclusive failure — an identity-independent HTTP
	// error, a content rule, an unresolvable host, or a proven-malformed
	// data: URI. The only verdict that counts toward rejecting a cast.
	VerdictDead Verdict = "dead"
	// VerdictInconclusive: no conclusive answer — a transient status
	// (identity-dependent 4xx, 5xx, 429), a timeout, a transport error, a
	// guard refusal, an over-long URL, or a source the pass never reached.
	// Treated exactly like alive when deciding a cast's fate.
	VerdictInconclusive Verdict = "inconclusive"
	// VerdictInline: a well-formed data: URI; nothing to dial.
	VerdictInline Verdict = "inline"
)

// ProbeResult is one source's probe outcome.
type ProbeResult struct {
	// Source is the probed source as the Fetcher's redacted label (query,
	// fragment and userinfo removed, truncated) — for the daemon log ONLY.
	// It must never ride an error message or a reply: resolved sources are
	// playlist content the caster may never have supplied, and signed CDN
	// URLs carry credentials in their query strings. The raw source is kept
	// by the caller, keyed by input position.
	Source  string
	Verdict Verdict
	// Status is the HTTP status behind the verdict when a response was
	// obtained, 0 when none was.
	Status int
	// Reason/Detail/Declared/Sniffed describe a dead verdict or a transient
	// attempt for the record (Reason is empty for a plain alive).
	Reason   Reason
	Detail   string
	Declared string
	Sniffed  string
	// WireURL is the URL actually dialed (post-rewrite, post-redirect) when
	// one was.
	WireURL string
	// Err carries why no conclusive answer was obtained.
	Err error
	// FromStore marks a verdict served from a conclusive stored record
	// without a network request (Prober.Preflight).
	FromStore bool
	// RetryAfter is the origin's Retry-After on a 429/503 (0 when none or
	// on any other answer): how long the host asked to be left alone. The
	// store bounds the record's transient hold by it and the HostLimiter
	// cools the host for it.
	RetryAfter time.Duration
	// Unreachable classifies a transient result in which the origin gave
	// no usable answer at all — DNS without a definitive answer, no
	// response (timeout, refused or reset connection, TLS failure), or a
	// server error — as opposed to an answer that is merely identity- or
	// load-dependent (401/403/429, 503 with Retry-After), which is an
	// answer. NetworkHealthy says whether, at that moment, the device
	// demonstrably reached OTHER origins (Prober sets it): only then does
	// the failure count toward the record's unreachable streak, so a device
	// with no network never marks its wall broken (Store.ApplyProbe,
	// UnreachableDeadAfter).
	Unreachable    Unreachable
	NetworkHealthy bool
	// GatewayFailed marks a gateway's 404 on an arweave transaction that a
	// second gateway SERVED (confirmAbsence): the work exists and one
	// gateway failed to retrieve it. Reported as issue server_error, but it
	// is an answer — the transaction is there — so it never advances, and
	// in fact ends, the unreachable streak (PR #438 review, F24).
	GatewayFailed bool
	// ServedBy is the gateway origin that served a content-addressed source
	// after the source's own gateway answered 404/410 (confirmAbsence, only
	// when the kiosk override is active). The store records it and the
	// kiosk interceptor loads the source through it.
	ServedBy string
	// Recorded is set by the Prober: the result changed the store. False
	// for results the store never records (guard and length refusals,
	// skipped hosts) AND for writes the store refused (the low-disk guard,
	// a failed transaction) — the lookahead holds such a source, because
	// its record will not make it non-due (PR #438 review, F21).
	Recorded bool
}

// Unreachable names how an origin failed to answer (ProbeResult.Unreachable).
type Unreachable string

const (
	// UnreachableDNS: resolution failed without NXDOMAIN (SERVFAIL, a
	// resolver timeout, a refusal).
	UnreachableDNS Unreachable = "dns"
	// UnreachableNoAnswer: no HTTP response — the item deadline expired
	// before one, or the connection was refused, reset or failed TLS.
	UnreachableNoAnswer Unreachable = "no_answer"
	// UnreachableServer: a 5xx without Retry-After.
	UnreachableServer Unreachable = "server_error"
)

// Rewriter is the kiosk's gateway policy, as this package needs it: which
// wire URL and User-Agent the kiosk would actually use for a source. The
// production value is *uarewrite.Policy; nil means "probe the source as
// written with the client's default agent".
type Rewriter interface {
	// RewriteURL returns the URL a content-addressed request on a hostile
	// gateway is redirected to, and whether a rewrite applies.
	RewriteURL(rawURL string) (string, bool)
	// Matches reports whether the host is one the policy rewrites the
	// User-Agent for (when RewriteURL does not apply).
	Matches(rawURL string) bool
	// UserAgent is the replacement agent for a matching request.
	UserAgent() string
}

// Probe performs ONE source's L0 probe: the data: shortcut, the kiosk
// rewrite, the ranged GET with the indexer's 206/416/short-prefix rules,
// the status table, then the content rules.
type Probe struct {
	Fetcher   Fetcher
	Validator *ContentValidator
	Rewriter  Rewriter
	// MaxBytes is the probe window (DefaultProbeMaxBytes when 0).
	MaxBytes int
	// ItemTimeout bounds one source's WHOLE probe, guard DNS included
	// (DefaultItemTimeout when 0).
	ItemTimeout time.Duration
	// Gateways are the ordered fallback gateways per content-addressed
	// network (NewGateways defaults when nil); see confirmAbsence.
	Gateways *Gateways
	// OverrideActive says the kiosk interceptor will load a work through
	// the gateway the probe found serving it (Record.ServedBy). Only then
	// is such a work `ok`: without the override the wall would still load
	// the failing gateway, and the verdict must not claim what the wall
	// cannot do — it is reported as a gateway failure instead.
	OverrideActive bool
}

const (
	// DefaultProbeMaxBytes is the indexer's L0 window: enough for every
	// container signature, the sniffer's own 3 KiB window, and the head of a
	// gateway listing or error page.
	DefaultProbeMaxBytes = 32 * 1024
	// minClassifiableBytes is the smallest valid 206 prefix worth handing to
	// content validation when the resource is larger than it: 512 bytes is
	// the standard sniffing window, and a shorter prefix of binary media can
	// sniff as text and false-broken real artworks.
	minClassifiableBytes = 512
	// DefaultItemTimeout is deliberately short: a slow origin costs seconds on
	// the synchronous cast path, and a slow origin is inconclusive, which
	// counts in the cast's favor.
	DefaultItemTimeout = 5 * time.Second
)

// ProbeOne probes one source within ItemTimeout. It never returns an
// error: everything that prevents a conclusive answer is that source's
// inconclusive result.
func (p *Probe) ProbeOne(ctx context.Context, source string) ProbeResult {
	return p.ProbeOneWithin(ctx, source, 0)
}

// ProbeOneWithin is ProbeOne with the item deadline given by the caller
// (ItemTimeout when timeout <= 0). The cast pass keeps the short default:
// it sits on the cast request's latency. Background probes pass a longer
// one (Prober.BackgroundItemTimeout).
func (p *Probe) ProbeOneWithin(ctx context.Context, source string, timeout time.Duration) ProbeResult {
	if IsDataURI(source) {
		return probeDataURI(source)
	}
	if timeout <= 0 {
		timeout = p.ItemTimeout
	}
	if timeout <= 0 {
		timeout = DefaultItemTimeout
	}
	// One deadline for the probe AND any confirmation it needs, so a
	// confirmed absence never costs more than the caller's item budget.
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	res := p.probeOnce(ctx, source)
	if res.Verdict == VerdictDead && res.Reason == ReasonHTTPStatus &&
		(res.Status == http.StatusNotFound || res.Status == http.StatusGone) {
		if ref, ok := p.gateways().Ref(source); ok {
			return p.confirmAbsence(ctx, res, ref)
		}
	}
	return res
}

// probeOnce is one source's probe under the caller's deadline.
func (p *Probe) probeOnce(ctx context.Context, source string) ProbeResult {
	wireURL, agent := p.wire(source)
	result := ProbeResult{WireURL: wireURL}

	maxBytes := p.MaxBytes
	if maxBytes <= 0 {
		maxBytes = DefaultProbeMaxBytes
	}
	resp, err := p.fetch(ctx, wireURL, agent, maxBytes, true)
	if err != nil {
		return p.fromFetchError(result, err)
	}
	result.Source = resp.label
	if resp.FinalURL != "" {
		result.WireURL = resp.FinalURL
	}
	result.Status = resp.Status

	switch {
	case resp.Status == http.StatusPartialContent:
		start, end, total, ok := parseContentRange(resp.Header.Get("Content-Range"))
		if !ok || start != 0 {
			// A 206 whose Content-Range does not describe a from-zero prefix
			// is not serving the bytes the validator's rules are defined
			// over. Retry without Range: the unranged read is still capped
			// at maxBytes and yields the true prefix.
			return p.retryUnranged(ctx, result, wireURL, agent, maxBytes)
		}
		// The response promised exactly end+1 bytes — NOT the full object
		// length. Truncation is judged against that promise, never the
		// Content-Range total (which would false-broken valid media).
		promised := end + 1
		if promised < minClassifiableBytes && (total < 0 || promised < total) {
			return p.retryUnranged(ctx, result, wireURL, agent, maxBytes)
		}
		return p.validate(result, resp, promised)
	case resp.Status >= 200 && resp.Status < 300:
		// The whole 2xx range is a fetch success: 203 arrives via
		// transforming proxies with valid media, and 204's empty body is a
		// content verdict (zero_length), not an HTTP one.
		return p.validate(result, resp, resp.ContentLength)
	case resp.Status == http.StatusRequestedRangeNotSatisfiable:
		// The server rejects Range outright; the player's document fetch
		// never sends one. Retry once without it.
		return p.retryUnranged(ctx, result, wireURL, agent, maxBytes)
	default:
		return statusResult(result, resp)
	}
}

// probeWhole is one source's probe as the kiosk would request it: no Range
// header, the body read capped at the probe window. Used to confirm a
// fallback gateway (confirmAbsence); the primary probe stays ranged so a
// 200 KB artwork does not cost 200 KB per lookahead step.
func (p *Probe) probeWhole(ctx context.Context, source string) ProbeResult {
	wireURL, agent := p.wire(source)
	maxBytes := p.MaxBytes
	if maxBytes <= 0 {
		maxBytes = DefaultProbeMaxBytes
	}
	return p.retryUnranged(ctx, ProbeResult{WireURL: wireURL}, wireURL, agent, maxBytes)
}

// statusResult judges a non-2xx answer by the status table and, when the
// origin asked for a pause (429/503), carries its Retry-After along.
func statusResult(result ProbeResult, resp *fetchOutcome) ProbeResult {
	result.Verdict = verdictForStatus(resp.Status)
	result.Reason = ReasonHTTPStatus
	result.Detail = fmt.Sprintf("HTTP %d", resp.Status)
	if result.Verdict == VerdictInconclusive {
		result.Err = fmt.Errorf("media health: transient HTTP %d", resp.Status)
		if coolsHost(resp.Status) {
			result.RetryAfter, _ = parseRetryAfter(resp.Header.Get("Retry-After"), time.Now())
		}
		if resp.Status >= 500 && result.RetryAfter == 0 {
			// A server error is the origin failing to serve. A 503 that
			// names a Retry-After is the origin asking for a pause — alive,
			// like a 429 — and stays out of the streak.
			result.Unreachable = UnreachableServer
		}
	}
	return result
}

// retryUnranged re-issues the probe without a Range header, once. A second
// 206/416 is taken as the answer rather than looping.
func (p *Probe) retryUnranged(ctx context.Context, result ProbeResult, wireURL, agent string, maxBytes int) ProbeResult {
	resp, err := p.fetch(ctx, wireURL, agent, maxBytes, false)
	if err != nil {
		return p.fromFetchError(result, err)
	}
	result.Source = resp.label
	if resp.FinalURL != "" {
		result.WireURL = resp.FinalURL
	}
	result.Status = resp.Status
	switch {
	case resp.Status == http.StatusPartialContent:
		// A 206 to a range-less request is protocol-broken regardless of how
		// plausible its Content-Range looks — trusting it would let a gateway
		// serve different bytes on the retry path than on a normal fetch.
		result.Verdict = VerdictDead
		result.Reason = ReasonHTTPStatus
		result.Detail = fmt.Sprintf("206 with Content-Range %q to an unranged request", resp.Header.Get("Content-Range"))
		return result
	case resp.Status >= 200 && resp.Status < 300:
		return p.validate(result, resp, resp.ContentLength)
	default:
		return statusResult(result, resp)
	}
}

// validate runs the content rules over a 2xx body.
func (p *Probe) validate(result ProbeResult, resp *fetchOutcome, promised int64) ProbeResult {
	body := resp.Body
	if resp.ReadErr != nil {
		// A deadline or cancellation mid-body is the probe's own clock
		// running out (a slow healthy origin, the phase ceiling), never
		// evidence about the bytes — judged BEFORE any byte count, because
		// a timed-out short read looks exactly like a truncated one.
		kind := fetchErrorKind(resp.ReadErr)
		if kind == FetchErrTimeout || !isConclusiveTruncation(len(body), promised, p.window()) {
			// Without independent evidence of premature termination (a
			// closed connection short of what the response promised) a
			// mid-body read failure is network weather, not a verdict.
			result.Verdict = VerdictInconclusive
			result.Reason = ReasonTransport
			result.Err = &FetchError{Kind: kind, Err: resp.ReadErr}
			return result
		}
	}
	if p.Validator == nil {
		result.Verdict = VerdictAlive
		result.Declared = normalizeContentType(resp.Header.Get("Content-Type"))
		return result
	}
	cv := p.Validator.Validate(resp.Header.Get("Content-Type"), body, promised)
	result.Declared, result.Sniffed = cv.Declared, cv.Sniffed
	if cv.OK {
		result.Verdict = VerdictAlive
		return result
	}
	result.Verdict = VerdictDead
	result.Reason = cv.Reason
	result.Detail = cv.Detail
	return result
}

func (p *Probe) window() int {
	if p.MaxBytes > 0 {
		return p.MaxBytes
	}
	return DefaultProbeMaxBytes
}

// isConclusiveTruncation reports whether a mid-body read failure is evidence
// of a truncated resource rather than transient network weather: the
// response promised a length, fewer bytes than that arrived, and the read
// stopped short of the probe cap (so the shortfall is the server's).
func isConclusiveTruncation(got int, promised int64, window int) bool {
	return promised > 0 && int64(got) < promised && got < window
}

func (p *Probe) fromFetchError(result ProbeResult, err error) ProbeResult {
	if fe, ok := errors.AsType[*FetchError](err); ok && fe.Kind == FetchErrDNS {
		// NXDOMAIN is the one transport-class failure that is a fact about
		// the source, not the weather: the host is gone for everyone.
		result.Verdict = VerdictDead
		result.Reason = ReasonDNS
		result.Detail = fe.Err.Error()
		result.Err = err
		return result
	}
	result.Verdict = VerdictInconclusive
	switch fetchErrorKind(err) {
	case FetchErrDNSUnresolved:
		result.Reason = ReasonTransport
		result.Unreachable = UnreachableDNS
	case FetchErrGuard, FetchErrTooLong:
		// Policy and size objections, not liveness: the display path still
		// forwards such a source today (whether it should is #3471's
		// territory), so they must not become dead here.
		result.Reason = ""
	default:
		// A timeout before any response, a refused or reset connection, a
		// TLS failure: the origin gave nothing back. (A deadline that
		// expires MID-BODY never reaches here — it has a status, it is an
		// answer, see validate.)
		result.Reason = ReasonTransport
		result.Unreachable = UnreachableNoAnswer
	}
	result.Err = err
	return result
}

// fetchOutcome is a FetchResponse plus the redacted label the fetcher
// attached for logging.
type fetchOutcome struct {
	*FetchResponse
	label string
}

func (p *Probe) fetch(ctx context.Context, wireURL, agent string, maxBytes int, ranged bool) (*fetchOutcome, error) {
	req := FetchRequest{URL: wireURL, UserAgent: agent, MaxBodyBytes: maxBytes}
	if p.Rewriter != nil {
		// The same policy the initial URL went through, applied per hop
		// by the fetcher (FetchRequest.Redirect).
		req.Redirect = p.wire
	}
	if ranged {
		req.RangeBytes = maxBytes
	}
	resp, err := p.Fetcher.Fetch(ctx, req)
	if err != nil {
		return nil, err
	}
	return &fetchOutcome{FetchResponse: resp, label: labelOf(resp, wireURL)}, nil
}

// labelOf prefers the fetcher's redacted label (carried on the response
// headers under a private key so the Fetcher interface stays one method);
// falls back to a conservative local redaction.
func labelOf(resp *FetchResponse, wireURL string) string {
	if resp != nil && resp.Header != nil {
		if l := resp.Header.Get(LabelHeader); l != "" {
			return l
		}
	}
	return redactForLog(wireURL)
}

// LabelHeader is the pseudo-header a Fetcher sets on its response to carry
// the log-safe label of what it dialed. Never sent on the wire — it is
// stamped on the response after the fact.
const LabelHeader = "X-Ffos-Media-Health-Label"

// wire computes the URL and User-Agent the kiosk would use for source:
// a content-addressed path on a hostile gateway is redirected (and keeps the
// default agent, as the kiosk interceptor does); any other request on such
// a host gets the policy agent; everything else is dialed as written.
func (p *Probe) wire(source string) (string, string) {
	if p.Rewriter == nil {
		return source, ""
	}
	if rewritten, ok := p.Rewriter.RewriteURL(source); ok {
		return rewritten, ""
	}
	if p.Rewriter.Matches(source) {
		return source, p.Rewriter.UserAgent()
	}
	return source, ""
}

func probeDataURI(source string) ProbeResult {
	result := ProbeResult{Source: redactForLog(source)}
	if _, proven, err := dataURIMediaType(source); err != nil {
		result.Err = err
		result.Reason = ReasonDataURIInvalid
		result.Detail = err.Error()
		if proven {
			// The whole URI fit inside the scan and still has no comma: no
			// valid RFC 2397 reading exists, and unlike a network fault this
			// can never heal.
			result.Verdict = VerdictDead
		} else {
			result.Verdict = VerdictInconclusive
		}
		return result
	}
	declared, _, _ := dataURIMediaType(source)
	reason, detail, sniffed, decoded := checkDataURIPayload(source, declared)
	result.Declared, result.Sniffed = normalizeContentType(declared), sniffed
	switch {
	case !decoded:
		// Well-formed metadata over a payload that does not decode: no
		// verdict, but a reasoned inconclusive, so the record holds the
		// source for RetryTransient instead of the walk re-picking it.
		result.Verdict = VerdictInconclusive
		result.Reason = ReasonDataURIInvalid
		result.Detail = "data: payload does not decode"
		result.Err = fmt.Errorf("media health: %s", result.Detail)
	case reason != "":
		// The payload is all there is: like a malformed URI, this can never
		// heal, and the wall shows exactly this.
		result.Verdict = VerdictDead
		result.Reason = reason
		result.Detail = detail
	default:
		result.Verdict = VerdictInline
	}
	return result
}

// verdictForStatus maps a GET's status to a verdict. Only statuses whose
// meaning does not depend on who is asking or on transient server health
// may be dead:
//
//   - 401/403/407/429 are inconclusive: an auth wall, a bot challenge or a
//     rate limit can answer differently for the player (#296 is the shipped
//     proof that a 403 to one client is a 200 to another).
//   - 406 is inconclusive: content negotiation answers the probe's headers.
//   - 408 is inconclusive: a request timeout is transient by definition.
//   - 416 never reaches here (retried unranged); kept inconclusive for the
//     retry's own 416.
//   - 3xx is inconclusive here because the guarded client follows
//     redirects; a 3xx that survives means the chain was cut (too many hops,
//     an unsafe hop) and nothing was judged.
//   - 5xx is inconclusive: server trouble is routinely transient.
//   - The remaining 4xx (400, 404, 410, ...) are dead: the origin understood
//     the request and said the resource is not there for anyone.
func verdictForStatus(status int) Verdict {
	switch {
	case status >= 200 && status < 300:
		return VerdictAlive
	case status < 400:
		return VerdictInconclusive
	case status == http.StatusUnauthorized,
		status == http.StatusForbidden,
		status == http.StatusNotAcceptable,
		status == http.StatusProxyAuthRequired,
		status == http.StatusRequestTimeout,
		status == http.StatusTooManyRequests,
		status == http.StatusRequestedRangeNotSatisfiable:
		return VerdictInconclusive
	case status < 500:
		return VerdictDead
	default:
		return VerdictInconclusive
	}
}

// parseContentRange parses a Content-Range header under the full satisfied
// single-range grammar: "bytes <start>-<end>/<total|*>" with digit-only
// positions, start <= end, and (when numeric) total > end. Anything else is
// rejected so a misbehaving gateway cannot get arbitrary bytes trusted as
// the resource prefix. total is -1 for the unknown-length form.
func parseContentRange(header string) (start, end, total int64, ok bool) {
	after, found := strings.CutPrefix(strings.TrimSpace(header), "bytes ")
	if !found {
		return 0, 0, 0, false
	}
	rangePart, totalPart, found := strings.Cut(after, "/")
	if !found {
		return 0, 0, 0, false
	}
	startStr, endStr, found := strings.Cut(rangePart, "-")
	if !found {
		return 0, 0, 0, false
	}
	start, errStart := strconv.ParseInt(strings.TrimSpace(startStr), 10, 64)
	end, errEnd := strconv.ParseInt(strings.TrimSpace(endStr), 10, 64)
	if errStart != nil || errEnd != nil || start < 0 || end < start {
		return 0, 0, 0, false
	}
	total = -1
	if totalPart = strings.TrimSpace(totalPart); totalPart != "*" {
		var errTotal error
		total, errTotal = strconv.ParseInt(totalPart, 10, 64)
		if errTotal != nil || total <= end {
			return 0, 0, 0, false
		}
	}
	return start, end, total, true
}

// redactForLog is the local fallback label: scheme, host and path only,
// bounded. The offline cache's fetcher supplies the authoritative label
// (same rule, shared with every other log line about a source).
func redactForLog(rawURL string) string {
	const maxLabel = 256
	if IsDataURI(rawURL) {
		if len(rawURL) > maxLabel {
			return rawURL[:maxLabel] + "…"
		}
		return rawURL
	}
	if i := strings.IndexAny(rawURL, "?#"); i >= 0 {
		rawURL = rawURL[:i] + "?…"
	}
	if at := strings.Index(rawURL, "@"); at >= 0 {
		if scheme := strings.Index(rawURL, "://"); scheme >= 0 && at > scheme {
			rawURL = rawURL[:scheme+3] + rawURL[at+1:]
		}
	}
	if len(rawURL) > maxLabel {
		return rawURL[:maxLabel] + "…"
	}
	return rawURL
}

// gateways returns the configured fallback gateways (defaults when unset).
func (p *Probe) gateways() *Gateways {
	if p.Gateways != nil {
		return p.Gateways
	}
	return defaultGateways
}

var defaultGateways = NewGateways(nil, nil)

// confirmAbsence re-checks a gateway's 404/410 on a content-addressed path
// against the network's other gateways, in order, at most
// maxConfirmGateways, inside the caller's deadline. A gateway's 404 there
// means "this gateway could not retrieve it", not "it does not exist": on
// the office FF1, 7 of 60 healthy works in a labeled fixture read 404 from
// arweave.net while ar-io.dev served them (#392).
//
//   - A gateway serves it: its bytes are judged by the normal content rules.
//     Valid media is `ok` with ServedBy = that gateway's origin, which the
//     kiosk interceptor then loads it through (Record.ServedBy) — so the
//     verdict and the wall agree. Without the interceptor (OverrideActive
//     false) it is a gateway failure instead: issue server_error, an
//     answer that ends the unreachable streak, never a verdict or a hint.
//   - A gateway's content rule fails (a type mismatch, an error page): that
//     is a verdict about the content itself, and it stands.
//   - Every gateway says 404/410: broken, confirmed.
//   - Nothing else answers — a 5xx, a timeout, OR any other status (402,
//     403, 429: the gateway's policy, never the content's) — and likewise
//     when no other gateway is configured for the network: a transient
//     server_error feeding the streak, because one gateway's 404 is not
//     evidence the content is gone; a deadline that ran out carries the
//     timeout error, so the cast pass's short deadline records nothing.
//
// The confirmation is a WHOLE-FILE request (no Range header, the body
// read capped at the probe window), the request shape the kiosk makes:
// ar-io.dev served a 32 KiB range of a 195 MB video and answered the
// whole-file load with 402, and the probe's "served" verdict put every
// item on the wall through a gateway that would not play it (office FF1,
// 2026-11-06). A gateway "serves it" only when it answers the request the
// player sends.
func (p *Probe) confirmAbsence(ctx context.Context, res ProbeResult, ref ContentRef) ProbeResult {
	firstStatus := res.Status
	confirmed := 0
	var lastErr error
	var lastNote string
	candidates := p.gateways().candidates(ref, hostOf(res.WireURL))
	for _, origin := range candidates {
		alt := p.probeWhole(ctx, ref.URLOn(origin))
		altHost := hostOf(origin)
		switch {
		case alt.Verdict == VerdictAlive:
			if !p.OverrideActive {
				res.Verdict = VerdictInconclusive
				res.Unreachable = ""
				res.GatewayFailed = true
				res.Detail = fmt.Sprintf("HTTP %d from the gateway; %s serves it", firstStatus, altHost)
				res.Err = fmt.Errorf("media health: gateway answered %d for content %s serves", firstStatus, altHost)
				return res
			}
			alt.ServedBy = origin
			alt.Detail = fmt.Sprintf("HTTP %d from %s; served by %s", firstStatus, hostOf(res.WireURL), altHost)
			return alt
		case alt.Verdict == VerdictDead && alt.Reason == ReasonHTTPStatus && (alt.Status == http.StatusNotFound || alt.Status == http.StatusGone):
			confirmed++
			lastNote = fmt.Sprintf("%s also answered HTTP %d", altHost, alt.Status)
		case alt.Verdict == VerdictDead && alt.Reason != ReasonHTTPStatus:
			// A content rule on the bytes another gateway served: a verdict
			// about the content. Any other status it answers (402, 400, a
			// 206 to the unranged request) is that gateway's own behavior
			// and falls through to "could not confirm".
			alt.Detail = fmt.Sprintf("%s (served by %s after HTTP %d from the gateway)", alt.Detail, altHost, firstStatus)
			return alt
		default:
			lastErr = alt.Err
			lastNote = fmt.Sprintf("%s could not confirm it", altHost)
		}
	}
	if len(candidates) > 0 && confirmed == len(candidates) {
		res.Detail = fmt.Sprintf("%s; %s", res.Detail, lastNote)
		return res
	}
	res.Verdict = VerdictInconclusive
	res.Unreachable = UnreachableServer
	res.Detail = fmt.Sprintf("HTTP %d from the gateway; %s", firstStatus, lastNote)
	switch {
	case len(candidates) == 0:
		// No other gateway configured for this network: the 404 stays
		// unconfirmed. It is reported as the gateway's problem
		// (server_error) and, repeated across a day, concludes through
		// the unreachable streak — never on one gateway's word.
		res.Detail = fmt.Sprintf("HTTP %d from the gateway; no other gateway configured to confirm it", firstStatus)
		res.Err = fmt.Errorf("media health: gateway answered %d, unconfirmed (no other gateway configured)", firstStatus)
		return res
	case lastErr != nil && fetchErrorKind(lastErr) == FetchErrTimeout:
		res.Err = lastErr
	default:
		res.Err = fmt.Errorf("media health: gateway answered %d, unconfirmed (%s)", firstStatus, lastNote)
	}
	return res
}
