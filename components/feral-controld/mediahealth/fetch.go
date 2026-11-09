package mediahealth

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
)

// Fetcher performs one guarded, bounded GET of a playlist item source. The
// only production implementation lives in offlinecache
// (NewMediaHealthFetcher), over the source guard, the guarded dial and the
// redirect guard that make dialing playlist-supplied URLs safe — this
// package must never build an HTTP client of its own.
//
// Contract: Fetch returns a FetchResponse for ANY HTTP answer (every status,
// 3xx after the guarded redirect chain included) and an error only when no
// answer was obtained. Errors are classified with FetchError so the probe
// can tell a transient failure from a conclusive one without string
// matching. Body holds at most req.MaxBodyBytes bytes; the implementation
// must never read further, whatever the origin sends.
type Fetcher interface {
	Fetch(ctx context.Context, req FetchRequest) (*FetchResponse, error)
}

// FetchRequest is one probe request.
type FetchRequest struct {
	// URL is the wire URL to dial — already rewritten by the kiosk's
	// gateway policy where that applies.
	URL string
	// UserAgent overrides the client's User-Agent when non-empty.
	UserAgent string
	// RangeBytes > 0 sends `Range: bytes=0-<RangeBytes-1>`; 0 sends no Range.
	RangeBytes int
	// MaxBodyBytes caps how much of the body is read into Body.
	MaxBodyBytes int
	// Redirect, when set, is applied to every redirect target before the
	// guard and the dial: it returns the URL to dial for that hop and the
	// User-Agent for it ("" = the client default). The kiosk interceptor
	// evaluates its gateway policy per request, redirect hops included,
	// so a hop onto a retired gateway's CID path goes to the replacement
	// gateway there; a probe that followed the raw hop recorded the
	// retired gateway's 404 as broken for a source the kiosk plays fine
	// (PR #438 review, round 4, F14).
	Redirect RedirectHook
}

// RedirectHook maps one redirect target to the wire URL and User-Agent
// to use for it (see FetchRequest.Redirect).
type RedirectHook func(target string) (wireURL, userAgent string)

// FetchResponse is the bounded view of an HTTP answer.
type FetchResponse struct {
	Status int
	Header http.Header
	// Body is the first min(MaxBodyBytes, len) bytes. ReadErr is a mid-body
	// read failure, kept separate from the status so the caller can judge
	// truncation against ContentLength.
	Body    []byte
	ReadErr error
	// ContentLength is the response's declared length, -1 when unknown.
	ContentLength int64
	// FinalURL is the URL that answered (after redirects). Empty means the
	// request URL.
	FinalURL string
}

// FetchErrorKind classifies why no answer was obtained.
type FetchErrorKind string

const (
	// FetchErrGuard: the source guard refused the destination (a reserved
	// address, a non-http scheme, a redirect somewhere unsafe). A POLICY
	// verdict, not a liveness one: inconclusive, never dead.
	FetchErrGuard FetchErrorKind = "guard"
	// FetchErrTooLong: the source exceeds the URL length bound. Inconclusive.
	FetchErrTooLong FetchErrorKind = "too_long"
	// FetchErrDNS: the host does not resolve (NXDOMAIN). Conclusive
	// (source_missing) — the indexer treats it the same.
	FetchErrDNS FetchErrorKind = "dns"
	// FetchErrDNSUnresolved: resolution failed WITHOUT a definitive answer
	// (SERVFAIL, a resolver timeout, a refused query). Transient per
	// attempt — it can be the device's own resolver — but a streak of them
	// while other names resolve concludes broken (Store.ApplyProbe,
	// DNSDeadAfter).
	FetchErrDNSUnresolved FetchErrorKind = "dns_unresolved"
	// FetchErrTimeout: the item deadline or the phase ceiling expired.
	// Transient.
	FetchErrTimeout FetchErrorKind = "timeout"
	// FetchErrTransport: any other transport failure (TLS, reset, protocol).
	// Transient — the probe cannot tell causes apart inside this class.
	FetchErrTransport FetchErrorKind = "transport"
)

// FetchError wraps a fetch failure with its class.
type FetchError struct {
	Kind FetchErrorKind
	Err  error
}

func (e *FetchError) Error() string { return fmt.Sprintf("media health fetch (%s): %v", e.Kind, e.Err) }
func (e *FetchError) Unwrap() error { return e.Err }

// fetchErrorKind returns err's class, defaulting to transport for an
// unclassified error so an implementation bug reads as transient (fail
// open), never as conclusive.
func fetchErrorKind(err error) FetchErrorKind {
	if fe, ok := errors.AsType[*FetchError](err); ok {
		return fe.Kind
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return FetchErrTimeout
	}
	if netErr, ok := errors.AsType[net.Error](err); ok && netErr.Timeout() {
		return FetchErrTimeout
	}
	return FetchErrTransport
}
