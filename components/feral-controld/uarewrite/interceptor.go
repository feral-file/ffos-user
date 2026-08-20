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
// On its own this bounds only how long WE wait for the reply — Chromium
// keeps the request paused regardless of when we stop waiting. What turns
// this into a real ceiling on how long an asset can stall is the retirement
// in processPaused's error path: a timeout retires (closes) the session, and
// a detached Fetch client releases every request it had paused, so the asset
// fails fast and the player's own retry can make progress against the
// re-armed session. Do not remove that retirement and expect this constant
// to keep meaning anything.
const continueTimeout = 10 * time.Second

const (
	// redialCooldown is the minimum spacing between recovery attach
	// attempts, and redialTimeout bounds one such attempt. Spacing
	// matters because a dead socket fails every request it was holding,
	// so without it one Chromium hiccup becomes a dial storm against a
	// kiosk that is already sick. The value mirrors offlinecache's own
	// replay re-dial (kioskreplay.go) rather than inventing a second
	// cadence for the same event on the same target.
	redialCooldown = 30 * time.Second
	redialTimeout  = 30 * time.Second

	// superviseInterval paces the supervisor's health pass, and
	// probeTimeout bounds its per-tick probe. The supervisor exists
	// because every other recovery trigger here is a Fetch event, and the
	// failure being recovered from is precisely the one that stops Fetch
	// events from arriving: a session that dies IDLE produces no failed
	// request, so nothing event-driven can ever notice it. offlinecache's
	// replay does not need one only because playlist-refresher already
	// touches its session every few minutes; this interceptor has no
	// periodic caller, so it brings its own.
	superviseInterval = 30 * time.Second
	probeTimeout      = 10 * time.Second
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

	// redialMu guards lastRedial and attaching, the cooldown clock and
	// single-flight latch behind claimAttach. Separate from mu: a recovery
	// decision is not a session swap, and it must not serialize against
	// the request path.
	redialMu   sync.Mutex
	lastRedial time.Time
	attaching  bool
	clock      wrapper.Clock

	// dial is DialPageSession behind a seam so recovery paths are testable
	// without a live kiosk. Production never overrides it.
	dial dialFunc

	// superviseOnce starts the supervisor exactly once, on the FIRST
	// AttachOnReconnect call — success or failure. Starting it on failure
	// too is the point: an initial attach against a kiosk that is still
	// booting must leave something behind that keeps trying.
	superviseOnce sync.Once
	// done stops the supervisor; closeOnce keeps Close idempotent.
	done      chan struct{}
	closeOnce sync.Once
}

// dialFunc mirrors offlinecache.DialPageSession's signature.
type dialFunc func(
	ctx context.Context,
	endpoint string,
	httpClient wrapper.HTTPClient,
	dialer wrapper.WebSocketDialer,
	jsonWrapper wrapper.JSON,
	ioWrapper wrapper.IO,
	logger *zap.Logger,
) (offlinecache.CDPSession, error)

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
		dial:       offlinecache.DialPageSession,
		done:       make(chan struct{}),
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
	// First call — success or failure — brings the supervisor up. It has
	// to start even when this attach fails: the kiosk may simply not be
	// up yet (boot ordering), and a failure that leaves nothing behind to
	// retry is how the rewrite stays dead until the next full restart.
	i.superviseOnce.Do(func() { go i.supervise() })

	select {
	case <-i.done:
		// Closed interceptors must not install fresh sessions: a dial
		// racing run()'s shutdown would otherwise leave a connection
		// nobody will ever close.
		return fmt.Errorf("uarewrite: interceptor closed")
	default:
	}

	session, err := i.dial(ctx, i.endpoint, i.httpClient, i.dialer, i.json, i.io, i.logger)
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

// Close tears down the session and stops the supervisor. Safe to call when
// never attached, and idempotent.
func (i *Interceptor) Close() error {
	i.closeOnce.Do(func() { close(i.done) })

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

// recoverableSendFailure reports whether err is worth recovering the session
// over. Two shapes qualify:
//
//   - ErrCDPTransport: the session package classifies at the source (see its
//     doc) — the socket itself is unusable. shutdown() now wraps the pending-
//     call error the same way, so an in-flight call killed by session death
//     classifies identically to one issued after it.
//   - context.DeadlineExceeded: the write went out but no reply came back
//     inside our own bound (every ctx on this path is created locally, so the
//     deadline is necessarily ours). The socket may technically be open, but
//     a Fetch client that cannot get replies is not intercepting anything —
//     and worse, Chromium is still holding whatever requests it paused for
//     us. Only retiring the session makes Chromium release them.
//
// A CDP error REPLY stays excluded: the peer answered, the socket is healthy,
// and re-dialing a kiosk that is answering fine is pointless churn.
func recoverableSendFailure(err error) bool {
	return errors.Is(err, offlinecache.ErrCDPTransport) || errors.Is(err, context.DeadlineExceeded)
}

// recoverFrom is the request-path accelerator: it retires the failed session
// immediately and kicks one paced attach, so recovery lands in seconds when
// traffic is flowing. It is NOT the guarantee — the supervisor is. A session
// that dies idle produces no failed request to arrive here, and an attach
// that fails here is not retried here; both are the supervisor's next tick.
func (i *Interceptor) recoverFrom(session offlinecache.CDPSession, err error) {
	if !recoverableSendFailure(err) {
		return
	}
	if !i.retire(session) {
		// Superseded: a dead socket fails every request it was holding, so
		// late arrivals from the old session must not disturb the live one.
		return
	}
	// Off the request goroutine: the dial is bounded but slow against a
	// sick kiosk, and this path is reached from the CDP read pump's child
	// goroutine.
	go i.tryAttach("request-path failure")
}

// retire removes session if — and only if — it is still the installed one,
// and closes it. Closing is what makes retirement matter beyond bookkeeping:
// a detached Fetch client's paused requests are released by Chromium, so a
// wedged session's assets fail fast instead of hanging on a pause nobody can
// answer. Returns false when session was already superseded, in which case
// the live session is left untouched.
//
// After retire, i.session == nil is the honest "not armed" signal the
// supervisor keys on.
func (i *Interceptor) retire(session offlinecache.CDPSession) bool {
	i.mu.Lock()
	if i.session != session {
		i.mu.Unlock()
		return false
	}
	i.session = nil
	i.mu.Unlock()

	if err := session.Close(); err != nil {
		i.logger.Debug("uarewrite: closing retired session", zap.Error(err))
	}
	return true
}

// supervise is the recovery guarantee: a paced loop that keeps interception
// armed until Close, regardless of whether any Fetch traffic is flowing.
// Every other trigger in this file is an accelerator on top of it.
func (i *Interceptor) supervise() {
	ticker := i.clock.NewTicker(superviseInterval)
	defer ticker.Stop()

	for {
		select {
		case <-i.done:
			return
		case <-ticker.C():
			i.ensureArmed()
		}
	}
}

// ensureArmed is one supervisor pass: attach if nothing is installed,
// otherwise probe the installed session and retire it if it is dead.
//
// The probe IS Fetch.enable with the policy's own patterns, not a synthetic
// ping. Re-enabling with identical patterns is idempotent in CDP, it answers
// the exact question the supervisor cares about ("is interception armed on a
// session that can still hear me"), and on a session that died idle it fails
// deterministically — sendForSession refuses closed sessions with
// ErrCDPTransport at entry, so the shutdown-race classification gap never
// reaches this path. NOTE: this assumes the session carries only OUR Fetch
// patterns; under the future shared seam, re-enabling would clobber the
// union, which is one more reason this whole type dissolves into that seam.
func (i *Interceptor) ensureArmed() {
	i.mu.Lock()
	session := i.session
	i.mu.Unlock()

	if session == nil {
		i.tryAttach("supervisor: no session installed")
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()

	if _, err := session.Send(ctx, "Fetch.enable", map[string]interface{}{
		"patterns": i.policy.FetchPatterns(),
	}); err != nil {
		if !recoverableSendFailure(err) {
			// The socket answered and refused. Re-dialing would loop the
			// same refusal; leave the session installed and say so.
			i.logger.Warn("uarewrite: supervisor probe refused on a live session", zap.Error(err))
			return
		}
		i.logger.Warn("uarewrite: supervisor found the rewrite session dead", zap.Error(err))
		i.retire(session)
		i.tryAttach("supervisor: probe failed")
	}
}

// tryAttach performs one paced, single-flight attach attempt. A failure is
// only logged: the supervisor's next tick is the retry, which is what makes
// "keeps trying until it succeeds or is closed" actually true — the previous
// design claimed a next-failed-request retry that could not exist, because a
// dead Fetch session produces no further failed requests.
//
// The onConnect hook deliberately does NOT come through here: a primary CDP
// reconnect is authoritative evidence of a fresh kiosk target, and making it
// wait out a cooldown from an unrelated failed dial would delay arming at
// exactly the moment arming is known to be possible.
func (i *Interceptor) tryAttach(trigger string) {
	if !i.claimAttach() {
		return
	}
	defer i.releaseAttach()

	ctx, cancel := context.WithTimeout(context.Background(), redialTimeout)
	defer cancel()

	if err := i.AttachOnReconnect(ctx); err != nil {
		i.logger.Warn("uarewrite: recovery attach failed; supervisor retries next tick",
			zap.String("trigger", trigger), zap.Error(err))
	}
}

// claimAttach is the paced single-flight gate in front of tryAttach's dial:
// at most one attempt in flight, at most one per cooldown window, shared by
// the supervisor and the request-path accelerator so their combined rate can
// never exceed one dial per window.
func (i *Interceptor) claimAttach() bool {
	i.redialMu.Lock()
	defer i.redialMu.Unlock()

	if i.attaching {
		return false
	}
	now := i.clock.Now()
	if !i.lastRedial.IsZero() && now.Sub(i.lastRedial) < redialCooldown {
		return false
	}
	i.lastRedial = now
	i.attaching = true
	return true
}

func (i *Interceptor) releaseAttach() {
	i.redialMu.Lock()
	i.attaching = false
	i.redialMu.Unlock()
}
