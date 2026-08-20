package uarewrite

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-controld/offlinecache"
	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

// continueTimeout bounds one Fetch.continueRequest round trip.
//
// A paused request is BLOCKED until we answer, so the ceiling here is a
// ceiling on how long an artwork asset can stall waiting on the daemon. It is
// deliberately short: if the CDP socket is wedged, failing fast and letting
// the page see a network error beats holding the asset open indefinitely,
// because the player's own retry can then make progress.
const continueTimeout = 10 * time.Second

// redialCooldown is the minimum spacing between recovery re-dials, and
// redialTimeout bounds one such attempt.
//
// Spacing matters because the trigger is a per-request failure: a dead socket
// fails every paused request it was holding, so without a cooldown one
// Chromium hiccup becomes a dial storm against a kiosk that is already sick.
// The value mirrors offlinecache's own replay re-dial (kioskreplay.go) rather
// than inventing a second cadence for the same event on the same target.
const (
	redialCooldown = 30 * time.Second
	redialTimeout  = 30 * time.Second
)

// Interceptor applies a Policy to live kiosk traffic.
//
// It owns its own event-driven CDP session to the kiosk rather than sharing
// the daemon's synchronous cdp client, and that is not a preference: the
// synchronous client has no read pump (it reads only while a Send is
// outstanding and discards interleaved frames), so arming any event-producing
// domain on it would let events pile up in the socket between commands and
// force every later Send to drain the backlog before finding its reply.
//
// KNOWN ARCHITECTURAL DEBT, deliberately taken and tracked in #296: this adds
// a THIRD CDP client on the kiosk target when offlinecache is also enabled,
// and offlinecache's AGENTS.md records multi-client CDP behavior on this
// Chromium build as unvalidated. The shipping design is a Fetch seam shared
// with offline replay; this shape exists so the fix can be validated on real
// hardware BEFORE that refactor is attempted, which is the same de-risking
// order the offlinecache notes ask for. Do not widen its responsibilities —
// when the shared seam lands, this type becomes an interceptor registered on
// it and its session ownership goes away.
//
// The dependency on offlinecache is likewise transitional: only
// DialPageSession and the CDPSession interface are used, both of which move
// to a neutral package as part of that refactor.
type Interceptor struct {
	policy   *Policy
	endpoint string

	httpClient wrapper.HTTPClient
	dialer     wrapper.WebSocketDialer
	json       wrapper.JSON
	io         wrapper.IO
	logger     *zap.Logger

	// mu guards session across a reconnect swap. The request path reads it
	// through the session captured at handler-registration time instead, so
	// a paused request never contends with a reconnect.
	mu      sync.Mutex
	session offlinecache.CDPSession

	// redialMu guards lastRedial, the cooldown clock behind recoverFrom.
	// Separate from mu: a recovery decision is not a session swap, and it
	// must not serialize against the request path.
	redialMu   sync.Mutex
	lastRedial time.Time
	clock      wrapper.Clock
}

// NewInterceptor builds an Interceptor. It performs no I/O; call
// AttachOnReconnect to establish the session.
func NewInterceptor(
	policy *Policy,
	endpoint string,
	httpClient wrapper.HTTPClient,
	dialer wrapper.WebSocketDialer,
	jsonWrapper wrapper.JSON,
	ioWrapper wrapper.IO,
	clock wrapper.Clock,
	logger *zap.Logger,
) *Interceptor {
	return &Interceptor{
		policy:     policy,
		endpoint:   endpoint,
		httpClient: httpClient,
		dialer:     dialer,
		json:       jsonWrapper,
		io:         ioWrapper,
		clock:      clock,
		logger:     logger,
	}
}

// AttachOnReconnect dials a fresh session to the kiosk, registers the
// request handler, and arms Fetch for the policy's patterns.
//
// Wire this to the daemon's CDP onConnect hook: the kiosk restarts (OOM
// recovery, watchdog action, manual restart) and every restart mints a new
// target, so a session established once at startup would silently stop
// intercepting after the first restart — and the failure it guards against
// is invisible from the daemon side, which is exactly the failure mode that
// makes it worth re-arming explicitly rather than hoping.
//
// Any previous session is closed first so a reconnect cannot leave a second
// Fetch client armed on a target we no longer track.
func (i *Interceptor) AttachOnReconnect(ctx context.Context) error {
	session, err := offlinecache.DialPageSession(ctx, i.endpoint, i.httpClient, i.dialer, i.json, i.io, i.logger)
	if err != nil {
		return fmt.Errorf("uarewrite: dial kiosk CDP session: %w", err)
	}

	// Bind the handler to THIS session. processPaused answers on the same
	// session the event arrived on, so a stale event from a superseded
	// connection can never be answered on the live one.
	session.On("Fetch.requestPaused", func(params json.RawMessage) {
		i.onRequestPaused(session, params)
	})

	if _, err := session.Send(ctx, "Fetch.enable", map[string]interface{}{
		"patterns": i.policy.FetchPatterns(),
	}); err != nil {
		// Arming failed, so this session intercepts nothing. Close it
		// rather than leaving a live connection that looks attached but
		// enforces no policy.
		if cerr := session.Close(); cerr != nil {
			i.logger.Debug("uarewrite: closing unarmed session", zap.Error(cerr))
		}
		return fmt.Errorf("uarewrite: Fetch.enable on kiosk: %w", err)
	}

	i.mu.Lock()
	previous := i.session
	i.session = session
	i.mu.Unlock()

	if previous != nil {
		if err := previous.Close(); err != nil {
			i.logger.Debug("uarewrite: closing superseded session", zap.Error(err))
		}
	}

	i.logger.Info("uarewrite: kiosk User-Agent rewrite armed",
		zap.Strings("hosts", i.policy.Hosts()),
		zap.String("user_agent", i.policy.UserAgent()))
	return nil
}

// Close tears down the session. Safe to call when never attached.
func (i *Interceptor) Close() error {
	i.mu.Lock()
	session := i.session
	i.session = nil
	i.mu.Unlock()

	if session == nil {
		return nil
	}
	return session.Close()
}

// onRequestPaused hands the request off to its own goroutine.
//
// CDPSession delivers events synchronously on the read pump, and answering a
// paused request requires a Send whose reply arrives on that same pump — so
// replying inline would deadlock the pump against the reply it is waiting to
// read. The goroutine is unbounded by design and safe to leave so: Fetch is
// armed ONLY for the policy's host patterns, so the arrival rate is bounded
// by how many assets a matching origin serves, not by total page traffic.
// That bound disappears if this type is ever armed with a wider pattern set.
func (i *Interceptor) onRequestPaused(session offlinecache.CDPSession, params json.RawMessage) {
	go i.processPaused(session, params)
}

// fetchRequestPaused is the subset of Fetch.requestPaused this needs.
type fetchRequestPaused struct {
	RequestID string `json:"requestId"`
	Request   struct {
		URL     string            `json:"url"`
		Headers map[string]string `json:"headers"`
	} `json:"request"`
	// ResponseStatusCode is present only for a Response-stage pause. The
	// policy arms Request stage only, so seeing it set means something
	// else armed a wider pattern on this session — continue untouched
	// rather than rewriting a request whose bytes are already fetched.
	ResponseStatusCode *int `json:"responseStatusCode"`
}

// processPaused answers exactly one paused request, on the session it
// arrived on.
//
// Every path that CAN answer must answer. A paused request that is never
// continued blocks that asset forever, which turns a header-rewrite bug into
// a hung artwork — strictly worse than the failure being fixed. So a
// non-matching URL and a Response-stage pause both fall through to a plain
// continueRequest rather than returning early.
//
// The two exceptions are not choices: a malformed event and an event with no
// requestId leave us with no handle to answer BY, so that request stays
// paused until Chromium tears the session down. Both are logged at Warn
// because the visible symptom is a stalled asset with no other explanation.
// Do not "fix" them into a continueRequest with a zero-value requestId —
// that would target no request and merely add a rejected CDP call to the log.
func (i *Interceptor) processPaused(session offlinecache.CDPSession, params json.RawMessage) {
	var paused fetchRequestPaused
	if err := i.json.Unmarshal(params, &paused); err != nil {
		i.logger.Warn("uarewrite: cannot parse Fetch.requestPaused", zap.Error(err))
		return
	}
	if paused.RequestID == "" {
		i.logger.Warn("uarewrite: Fetch.requestPaused carried no requestId")
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), continueTimeout)
	defer cancel()

	args := map[string]interface{}{"requestId": paused.RequestID}

	// Re-check the URL even though Fetch was armed with the policy's own
	// patterns. The check is cheap and it is what keeps this correct if the
	// session ever carries a wider pattern set than this policy asked for —
	// the shared-seam refactor will arm the UNION of every consumer's
	// patterns, at which point this handler starts seeing requests that are
	// not ours and must pass them through untouched.
	rewritten := false
	if paused.ResponseStatusCode == nil && i.policy.Matches(paused.Request.URL) {
		args["headers"] = i.policy.RewriteHeaders(paused.Request.Headers)
		rewritten = true
	}

	if _, err := session.Send(ctx, "Fetch.continueRequest", args); err != nil {
		i.logger.Warn("uarewrite: Fetch.continueRequest failed",
			zap.String("url", paused.Request.URL),
			zap.Bool("rewritten", rewritten),
			zap.Error(err))
		i.recoverFrom(session, err)
		return
	}

	if rewritten {
		i.logger.Debug("uarewrite: rewrote User-Agent",
			zap.String("url", paused.Request.URL))
	}
}

// recoverFrom re-arms interception when the failure that produced err was the
// SOCKET dying rather than a command being refused.
//
// This session is a separate WebSocket from the daemon's primary CDP client,
// with its own read pump and write deadlines, so it can die on its own while
// the primary stays perfectly healthy. AttachOnReconnect's only other caller
// is main.go's onConnect hook, which fires on the PRIMARY connection — in
// that case never. Without this, one dead socket retires the rewrite
// permanently, until the kiosk or the primary connection restarts.
//
// The failure is invisible without this path, which is why it is worth
// handling rather than logging: when a DevTools client drops, Chromium
// releases the requests it had paused and stops honoring its Fetch patterns,
// so traffic keeps flowing — with the browser User-Agent restored. Nothing
// stalls, nothing new is logged, and the artworks this PR fixes silently
// revert to failing.
//
// ErrCDPTransport is what separates the two shapes. The session package
// classifies at the source (see its doc): a transport error means the
// connection is unusable, while a CDP error REPLY or an expired caller
// context leaves the socket perfectly healthy. Re-dialing the latter would be
// pointless churn against a kiosk that is answering fine.
func (i *Interceptor) recoverFrom(session offlinecache.CDPSession, err error) {
	if !i.claimRedial(session, err) {
		return
	}

	// Off the request goroutine: AttachOnReconnect dials synchronously
	// (bounded, but seconds against a sick kiosk) and this path is reached
	// from the CDP read pump's child goroutine.
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), redialTimeout)
		defer cancel()

		if err := i.AttachOnReconnect(ctx); err != nil {
			// Leave it retired. The next failed request past the cooldown
			// tries again, and a primary-connection reconnect re-arms it
			// regardless, so a permanently down kiosk costs one bounded
			// dial per cooldown rather than a spin.
			i.logger.Warn("uarewrite: re-dial after transport failure failed", zap.Error(err))
		}
	}()
}

// claimRedial decides whether this failure should trigger a re-dial, and
// claims the cooldown slot if so. Split out from recoverFrom purely so the
// decision is testable without performing a real dial — the dial itself needs
// a live kiosk, the decision is where every mistake would hide.
//
// Returns false, without claiming anything, for the three cases that must not
// re-dial: an error that left the socket healthy, a failure on a session that
// has already been superseded, and an attempt inside the cooldown window.
func (i *Interceptor) claimRedial(session offlinecache.CDPSession, err error) bool {
	if !errors.Is(err, offlinecache.ErrCDPTransport) {
		return false
	}

	// Only the live session's failure is worth acting on. A dead socket
	// fails every request it was holding, so without this check a single
	// drop would enqueue one recovery per in-flight request, and a
	// reconnect that already happened would be undone by a late arrival.
	i.mu.Lock()
	current := i.session
	i.mu.Unlock()
	if current != session {
		return false
	}

	i.redialMu.Lock()
	defer i.redialMu.Unlock()

	now := i.clock.Now()
	if !i.lastRedial.IsZero() && now.Sub(i.lastRedial) < redialCooldown {
		return false
	}
	i.lastRedial = now
	return true
}
