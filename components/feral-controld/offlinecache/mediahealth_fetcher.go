package offlinecache

import (
	"context"
	"errors"
	"fmt"
	go_io "io"
	"net"
	go_http "net/http"
	go_url "net/url"
	"strings"

	"golang.org/x/sync/semaphore"

	"github.com/feral-file/ffos-user/components/feral-controld/mediahealth"
	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

// mediahealth_fetcher.go is the ONLY mediahealth.Fetcher: the L0 media
// probe's single guarded, bounded GET (feral-file/ffos-user#392). It lives
// in this package, not in mediahealth, because a probe dials untrusted
// playlist-supplied URLs — the input ErrUnsafeSource's threat model
// describes — and this package owns the machinery that makes that safe:
// the source guard, the guarded dial, the guarded redirect chain, and the
// log-redaction bound. mediahealth may not import this package (its import
// lint pins that), so the dependency points this way: offlinecache
// implements mediahealth's interface.
//
// It replaces probe.go's cast-time preflight (#304), whose status-only
// HTTP/1 parser read no body at all. L0 needs the first 32 KiB, so this
// fetcher reads through net/http's parser on the guarded transport
// (64 KiB response-header ceiling, bounded redirects, no proxy) and bounds
// the rest here: a process-wide admission budget on in-flight parses,
// keep-alives off so no request is ever silently replayed, and a hard cap
// on body bytes read.

const (
	// maxConcurrentProbeFetches is the process-wide ceiling on in-flight
	// probe responses, counted in units: plain HTTP spends one, HTTPS
	// four, so at most 32 HTTP or 8 TLS peers can have this daemon parsing
	// their headers and certificates at once — whatever the cast storm gate
	// (operator-configurable, and independent of this bound) admits. The
	// cast pass's own worker pool (16) and the lookahead (1) sit inside it.
	maxConcurrentProbeFetches int64 = 32
	probeFetchHTTPSWeight     int64 = 4
)

var processProbeFetchSlots = semaphore.NewWeighted(maxConcurrentProbeFetches)

// probeFetchWeight is what one hop to rawURL costs in admission units.
func probeFetchWeight(rawURL string) int64 {
	if u, err := go_url.Parse(rawURL); err == nil && strings.EqualFold(u.Scheme, "https") {
		return probeFetchHTTPSWeight
	}
	return 1
}

// probeAdmission is one request's hold on the process-wide probe budget,
// re-balanced per hop: a plain-HTTP request that redirects to HTTPS must
// pay the TLS weight before the handshake, or a playlist of such
// redirects would put 32 TLS peers in flight against a documented ceiling
// of 8 (PR #438 review, round 5, F18). It rides the request context so
// the probe client's CheckRedirect (the one place a hop is known before
// it is dialed) can reach it; net/http hands every hop the initial
// request's context, and calls CheckRedirect on the goroutine running Do,
// so held is never touched concurrently.
type probeAdmission struct {
	slots *semaphore.Weighted
	held  int64
}

// admissionKey carries the request's *probeAdmission on its context.
type admissionKey struct{}

// ensure grows the hold to want units. It releases the current hold
// before acquiring the larger one — never hold-and-wait, which under a
// burst of upgrading redirects would have every request holding one unit
// and waiting for three more that nobody can give up. The previous hop's
// response is already closed by the time CheckRedirect runs, so nothing
// is in flight on the released units.
func (a *probeAdmission) ensure(ctx context.Context, want int64) error {
	if want <= a.held {
		return nil
	}
	a.release()
	if err := a.slots.Acquire(ctx, want); err != nil {
		return err
	}
	a.held = want
	return nil
}

func (a *probeAdmission) release() {
	if a.held > 0 {
		a.slots.Release(a.held)
		a.held = 0
	}
}

// NewMediaHealthFetcher builds the production fetcher over the system
// resolver (net.DefaultResolver — the guard's view of a name must match
// what would actually be dialed, see ErrUnsafeSource).
//
// The client is built here rather than taken from the caller so the one
// thing that must always be true — that these fetches ride the guarded
// client, never the daemon-wide one — cannot depend on a call site
// remembering it. No whole-request timeout on the client: the probe's own
// per-item context deadline bounds each request, DNS included.
func NewMediaHealthFetcher(resolver AddrResolver) mediahealth.Fetcher {
	guard := sourceGuard{resolver: resolver}
	return newMediaHealthFetcherWith(guard, newGuardedProbeHTTPClientFor(guard, 0))
}

// newMediaHealthFetcherWith is NewMediaHealthFetcher with the guard and
// client supplied directly, so a test can hand in a guard carrying the
// isReserved override its httptest servers need.
func newMediaHealthFetcherWith(guard sourceGuard, client wrapper.HTTPClient) mediahealth.Fetcher {
	return &mediaHealthFetcher{guard: guard, client: client, slots: processProbeFetchSlots}
}

type mediaHealthFetcher struct {
	guard  sourceGuard
	client wrapper.HTTPClient
	slots  *semaphore.Weighted
}

// Fetch performs one guarded GET. Every error is a *mediahealth.FetchError
// so the probe can classify it without string matching:
//
//   - too_long: the source exceeds MaxSourceURLBytes (no dial).
//   - guard: the source guard refused the destination or a redirect hop
//     (no dial, or a dial that was stopped) — ErrUnsafeSource underneath.
//   - dns: the host does not resolve (NXDOMAIN).
//   - timeout: the context deadline or a transport timeout.
//   - transport: everything else.
func (f *mediaHealthFetcher) Fetch(ctx context.Context, req mediahealth.FetchRequest) (*mediahealth.FetchResponse, error) {
	if err := checkSourceLength(req.URL); err != nil {
		return nil, &mediahealth.FetchError{Kind: mediahealth.FetchErrTooLong, Err: err}
	}
	// The guard runs before any network round trip; its DNS lookup is the
	// one place a missing host is observed, hence the NXDOMAIN check here.
	if err := f.guard.check(ctx, req.URL); err != nil {
		return nil, classifyFetchError(err)
	}

	adm := &probeAdmission{slots: f.slots}
	if err := adm.ensure(ctx, probeFetchWeight(req.URL)); err != nil {
		return nil, &mediahealth.FetchError{Kind: mediahealth.FetchErrTimeout, Err: err}
	}
	defer adm.release()
	ctx = context.WithValue(ctx, admissionKey{}, adm)

	httpReq, err := f.client.NewRequest(go_http.MethodGet, req.URL, nil)
	if err != nil {
		return nil, &mediahealth.FetchError{Kind: mediahealth.FetchErrTransport, Err: fmt.Errorf("build GET request: %w", err)}
	}
	if req.Redirect != nil {
		ctx = context.WithValue(ctx, redirectHookKey{}, req.Redirect)
	}
	httpReq = httpReq.WithContext(ctx)
	if req.RangeBytes > 0 {
		httpReq.Header.Set("Range", fmt.Sprintf("bytes=0-%d", req.RangeBytes-1))
	}
	if req.UserAgent != "" {
		httpReq.Header.Set("User-Agent", req.UserAgent)
	}

	resp, err := f.client.Do(httpReq)
	if err != nil {
		return nil, classifyFetchError(err)
	}
	// Closed without draining: the body may be arbitrarily large when a
	// server ignores the Range header, and keep-alives are off so there is
	// no connection worth preserving by reading it to the end.
	defer func() { _ = resp.Body.Close() }()

	out := &mediahealth.FetchResponse{
		Status:        resp.StatusCode,
		Header:        resp.Header.Clone(),
		ContentLength: resp.ContentLength,
	}
	if resp.Request != nil && resp.Request.URL != nil {
		out.FinalURL = resp.Request.URL.String()
	}
	if out.Header == nil {
		out.Header = go_http.Header{}
	}
	// The log-safe label travels on the response so mediahealth never has
	// to re-derive the redaction rule (the one shared by every log line
	// about a source in this package).
	out.Header.Set(mediahealth.LabelHeader, redactSourceForLog(req.URL))
	if req.MaxBodyBytes > 0 {
		body, readErr := go_io.ReadAll(go_io.LimitReader(resp.Body, int64(req.MaxBodyBytes)))
		out.Body = body
		out.ReadErr = readErr
	}
	return out, nil
}

// classifyFetchError maps a guard or transport error to its FetchError
// kind. Order matters: a guard refusal wrapped by net/http (a redirect hop
// the guard stopped) must stay a guard verdict, and an NXDOMAIN must be
// told apart from a resolver that merely timed out.
func classifyFetchError(err error) error {
	err = redactFetchError(err)
	switch {
	case errors.Is(err, ErrUnsafeSource):
		return &mediahealth.FetchError{Kind: mediahealth.FetchErrGuard, Err: err}
	case errors.Is(err, ErrSourceTooLong):
		return &mediahealth.FetchError{Kind: mediahealth.FetchErrTooLong, Err: err}
	}
	if dnsErr, ok := errors.AsType[*net.DNSError](err); ok {
		if dnsErr.IsNotFound {
			return &mediahealth.FetchError{Kind: mediahealth.FetchErrDNS, Err: err}
		}
		// Everything else a resolver can say short of NXDOMAIN — SERVFAIL
		// ("server misbehaving"), a lookup timeout, a refusal. Checked
		// before the generic timeout below so a DNS timeout is counted
		// toward the DNS streak, not lost as plain network weather.
		return &mediahealth.FetchError{Kind: mediahealth.FetchErrDNSUnresolved, Err: err}
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return &mediahealth.FetchError{Kind: mediahealth.FetchErrTimeout, Err: err}
	}
	if netErr, ok := errors.AsType[net.Error](err); ok && netErr.Timeout() {
		return &mediahealth.FetchError{Kind: mediahealth.FetchErrTimeout, Err: err}
	}
	return &mediahealth.FetchError{Kind: mediahealth.FetchErrTransport, Err: err}
}

// redactFetchError strips credential-bearing URL text from a net/http error
// before it can reach a log line or a stored lastError. *url.Error renders
// its URL verbatim — query string included — and on a redirect hop that is
// a URL the playlist never showed us; the wrapped cause is kept so the
// error classes (DNS, timeout, guard) still match through errors.Is/As.
func redactFetchError(err error) error {
	if urlErr, ok := errors.AsType[*go_url.Error](err); ok {
		return &go_url.Error{Op: urlErr.Op, URL: redactSourceForLog(urlErr.URL), Err: urlErr.Err}
	}
	return err
}

// redactSourceForLog prepares a source URL for the daemon log: the query
// string and fragment are dropped before the usual truncation. The daemon
// log leaves the device in uploadLogs support bundles, and signed CDN URLs
// carry their credentials (e.g. X-Amz-Signature) in the query string —
// truncation alone is not a redaction, because a presigned URL's credential
// parameters commonly sit inside the first 256 bytes. Scheme, host, and
// path survive, which is what an operator greps a probe failure by; a
// trailing "?…" marks that a query was removed. A URL that does not parse
// is cut at the first '?' rather than trusted to be credential-free. data:
// URIs pass straight to truncation — their bounded prefix is content, not a
// credential carrier, matching how the rest of this package logs them.
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
