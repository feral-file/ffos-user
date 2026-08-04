package offlinecache

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	go_http "net/http"
	"net/url"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

// largeAssetThreshold is the point at which replay redirects the kiosk to
// the static server instead of fulfilling inline via CDP
// Fetch.fulfillRequest. Fulfilling requires base64-encoding the body into
// a single JSON string over the DevTools websocket, which hits a V8
// string-length ceiling (~0x1fffffe8 characters, roughly 400-536MB
// depending on encoding overhead) on Chromium's side; this stays
// comfortably under that so the CDP path never gets close to it.
const largeAssetThreshold = 200 * 1024 * 1024 // 200MB

// MissPolicy controls what replay does when a request has no matching
// captured resource for the currently enabled item.
type MissPolicy string

const (
	// MissPolicyFailClosed fails the request as a network error. This is
	// the recommended default: it makes offline behavior deterministic and
	// surfaces a partial capture honestly instead of quietly depending on
	// connectivity that offline mode is supposed to remove.
	//
	// It holds for as long as replay is actually intercepting. There is
	// exactly one path that knowingly suspends it — retireOnSaturation,
	// when BOTH admission bounds are exhausted and the CDP connection can
	// no longer answer anything — and it is a suspension of interception
	// itself, not a per-request policy decision: the ROOT session is closed,
	// so Chromium releases what it had paused across every target on that
	// connection and the page runs unintercepted against the live network
	// until SyncPlaylist re-arms scope. That re-arm is nudged rather than
	// waited for — see SetOnScopeLost — but that is best-effort
	// acceleration, not a bound on the window. See
	// retireOnSaturation's doc for why the alternative on offer there is a
	// hung page rather than an enforced one, and
	// docs/offline-artwork-capture.md for the operator-visible signal.
	MissPolicyFailClosed MissPolicy = "fail_closed"
	// MissPolicyPassThrough lets the request continue to the real network.
	// Only sensible when the device is known to be online (progressive
	// capture); exposed as a config toggle rather than hardcoded.
	MissPolicyPassThrough MissPolicy = "pass_through"
)

// Replayer serves a cached item's captured resources back to the kiosk
// Chromium via CDP Fetch domain interception, keyed on the exact URL the
// item's DP-1 source pointed at — the source itself is never rewritten
// (see ItemRecord.Item's doc), which preserves the signed-playlist
// invariant end to end. A live request that misses on that exact URL is
// retried once with a small, explicit allowlist of known player-appended
// UI-only query params stripped (see stripPlayerAppendedParams) before
// falling through to the miss policy — this does not weaken the exact-URL
// guarantee for anything else, since it only ever removes allowlisted
// params, never resources.
//
//go:generate mockgen -source=replay.go -destination=../mocks/offlinecache_replay.go -package=mocks -mock_names=Replayer=MockOfflineCacheReplayer
type Replayer interface {
	// Attach registers the Fetch.requestPaused handler on session for the
	// CDP target identified by sessionID. The empty sessionID is the
	// top-level kiosk page target; a non-empty sessionID is a flat-mode
	// child target (e.g. a cross-origin, out-of-process iframe embedding
	// the artwork — see kiosktargets.go). A cross-origin iframe runs in
	// its own renderer process with its own CDP target, so its requests
	// never surface on the top-level page's session; without attaching to
	// that child target too, offline replay would silently miss every
	// request the iframe makes and fall through to the (offline) network.
	//
	// Attaching the top-level target (empty sessionID) is a connection
	// generation boundary: it supersedes and closes the previous top-level
	// session AND drops every child target from the prior connection
	// (their sessionIds are meaningless against the new socket). It does
	// NOT enable Fetch by itself — EnableForItem/EnableForPlaylist does
	// that — so call it once per CDP connection (including after a
	// reconnect; see cdp.CDP's onConnect hook) and then scope interception
	// on/off per displayed item/playlist.
	//
	// Attaching a child target (non-empty sessionID) is additive and, if a
	// replay scope is already enabled, immediately enables Fetch on that
	// new target too — a child iframe can attach at any time (mid-playback,
	// on an iframe navigation) after EnableForPlaylist already ran, so
	// attach-time enablement is what keeps interception armed for it.
	Attach(sessionID string, session CDPSession)
	// AttachChild is Attach's reconnect-race-safe counterpart for the
	// flat-mode child-target path specifically: it behaves exactly like
	// Attach(sessionID, session) with a non-empty sessionID, EXCEPT it
	// first verifies — atomically, under the same lock Attach's
	// top-level generation swap uses — that root is still the CURRENT
	// top-level session (i.e. the most recent Attach("", root) call
	// this replayer has seen). It exists because kiosktargets.go's
	// Target.attachedToTarget handler runs on its own goroutine, off
	// the CDP read pump that delivered the event: a kiosk reconnect can
	// supersede root (via a fresh Attach("", newRoot) call) at any
	// point between that event being read and this goroutine actually
	// running. Without this guard, a delayed handler would call plain
	// Attach with a child session bound to the now-DEAD old root,
	// injecting it into the CURRENT generation's target set — the next
	// EnableForPlaylist/Disable fan-out would then try (and fail or
	// stall on the dead socket's send timeout) to Fetch.enable/disable
	// it, degrading or delaying live replay scope for an iframe that no
	// longer exists the moment its top-level page was replaced.
	// The returned bool is the caller's authorization to RESUME the
	// paused child target, and is false for either of two reasons, both
	// leading to the same action: root was already superseded (nothing
	// was attached, and resuming a dead connection's target is
	// pointless), or Fetch could not be armed on the child. The second
	// is a correctness requirement, not an optimization — see
	// attachChild's doc for why resuming an unarmed child silently
	// defeats fail_closed.
	AttachChild(root CDPSession, sessionID string, session CDPSession) (resumeChild bool)
	// Detach drops a child target that has gone away
	// (Target.detachedFromTarget). It unregisters that target's handlers
	// so a long-lived kiosk connection that sees many iframe navigations
	// does not accumulate dead per-target state. Detaching the empty
	// (top-level) sessionID is a no-op: the top-level target only changes
	// via a full reconnect, handled by Attach's generation boundary above.
	Detach(sessionID string)
	// DetachChild is Detach's counterpart to AttachChild, for the exact
	// same reconnect-race reason: a Target.detachedFromTarget delivered
	// on a superseded root must never touch the CURRENT generation's
	// target set — see AttachChild's doc. In practice this guards
	// against the (extremely unlikely, since CDP mints a fresh sessionId
	// per connection) case of a stale event's sessionId colliding with a
	// live child's; the guard is kept symmetric with AttachChild rather
	// than relying on that being merely unlikely.
	DetachChild(root CDPSession, sessionID string)
	// EnableForItem loads the captured record for source (a raw item
	// source URL — the cache identity, see SourceKey) and enables Fetch
	// interception scoped to it. Call before displaying a single cached
	// item. Equivalent to EnableForPlaylist(ctx, []string{source}, false):
	// a lone item is by definition not a "mixed" scope.
	EnableForItem(ctx context.Context, source string) error
	// EnableForPlaylist loads every source's captured record and enables
	// Fetch interception scoped to the union of their captured
	// resources. A DP-1 playlist plays multiple items in sequence inside
	// the same kiosk page/CDP target without Go being told exactly which
	// one is on screen at any instant (feral-player advances the
	// playlist client-side), so replay must recognize any cached item's
	// URLs for as long as that playlist is showing rather than trying to
	// track a single "current" item from the daemon side.
	//
	// mixed must be true whenever sources is not the complete set of
	// items the displayed playlist actually contains (i.e. the caller
	// filtered out some uncached items before calling this — see
	// KioskReplay.SyncPlaylist). It relaxes the miss policy to
	// pass-through for the duration of this scope: with Fetch.enable
	// patterned on "*", any request Chromium makes while ANY item from
	// the playlist is on screen goes through this handler, including
	// requests that belong to a sibling item this call was never told
	// about because it isn't cached. Failing those as fail_closed would
	// misreport a live, reachable resource as offline-missing just
	// because it happens to share a CDP target with a cached item.
	// mixed=false (a single item, or a playlist where every item is
	// cached) keeps the configured missPolicy's strict guarantee, since
	// every resource the page can legitimately need is then known.
	EnableForPlaylist(ctx context.Context, sources []string, mixed bool) error
	// Disable turns interception off; all requests pass through
	// untouched. Call when moving to an uncached/online item. If the
	// underlying Fetch.disable CDP call itself fails, scope is NOT
	// cleared to a fail-closed state (see the implementation's doc
	// comment) — the returned error should be logged/retried by the
	// caller, but must never be treated as "safe to ignore, disable
	// probably still happened."
	Disable(ctx context.Context) error
	// RootAttached reports whether a top-level session is installed. A
	// failed Enable/Disable retires the session it failed on when the
	// failure was a transport one (see the implementation's
	// retireIfDeadLocked), so a caller that sees a scope call fail can
	// use this to distinguish "the connection is fine, the command was
	// rejected" from "the socket is dead and something has to re-dial" —
	// see KioskReplay.SyncPlaylist, which is that something.
	RootAttached() bool
}

// ScopeLostRegistrar is the one-method view of the replayer that startup
// wiring needs, kept separate from Replayer so main.go gets only the
// setter rather than Attach/EnableForPlaylist/Disable as well — see
// Runtime.ScopeLost.
type ScopeLostRegistrar interface {
	// SetOnScopeLost registers a callback fired when replay stops
	// enforcing scope for a reason only a re-arm can fix — today only
	// admission saturation (see retireOnSaturation), which closes the
	// root session and lets the page run unintercepted until something
	// calls SyncPlaylist again.
	//
	// It exists because that "something" is otherwise
	// playlist-refresher's periodic pass, up to PLAYLIST_REFRESH_INTERVAL
	// away, and a fail_closed scope is enforced by nothing for that whole
	// window. main.go wires this to PlaylistRefresher.ForceRefresh, the
	// same recovery the kiosk-reconnect path already uses for the
	// identical "scope was invalidated and the loop has not noticed"
	// problem — see that interface's doc.
	//
	// Optional: a nil handler (never set) simply means recovery waits for
	// the periodic pass, which is the pre-existing behavior.
	//
	// The handler MUST NOT block. It runs on the retirement goroutine,
	// which still holds the saturationRetiring latch — so a handler that
	// never returns does not merely delay this recovery, it permanently
	// prevents any future saturation on any session from ever being
	// retired again.
	SetOnScopeLost(fn func())
}

type replayer struct {
	store        Store
	staticServer StaticServer
	missPolicy   MissPolicy
	json         wrapper.JSON
	logger       *zap.Logger

	// kioskShellOrigin is the parsed origin (scheme+host) of the bundled
	// local player (constant.WEBAPP_URL, served on loopback by
	// feral-player.service), injected via NewReplayer. Every request to
	// this origin passes through untouched — see processRequestPaused for
	// why this exemption is load-bearing for recovery navigations. nil
	// when the injected URL was empty or unparsable, which simply means
	// no exemption (safe: requests then fall through to normal
	// interception, they never escape to the network by accident).
	kioskShellOrigin *url.URL

	// transitionMu serializes target-set mutations (Attach, which swaps
	// and closes the top-level CDP session on kiosk reconnect and adds
	// child targets, and Detach) against EnableForPlaylist/Disable's own
	// snapshot-targets-then-Send sequences. It is a SEPARATE, coarser
	// lock from mu below — deliberately, so a slow Fetch.enable/disable
	// round-trip (bounded by cdpsession's own send timeout, but still up
	// to several seconds) here never blocks mu's fast, always-brief hot
	// path (processRequestPaused's per-request target/resources read).
	// Without this lock, EnableForPlaylist/Disable could snapshot the
	// target set under mu, release mu, and then have a concurrent Attach
	// swap AND close a session before this call's own Fetch.enable/
	// disable Send runs against it — the Send then fails against an
	// already-dying session while r.resources/r.mixedScope (already
	// committed) still claim a scope is active, silently leaving a
	// target's Fetch domain never actually enabled. Holding transitionMu
	// across the whole snapshot-through-Send sequence closes that window.
	//
	// IMPORTANT: transitionMu can be held across a CDP Send (which blocks
	// on the read pump delivering the reply), so it must NEVER be
	// acquired from a CDP event handler running ON that read pump — that
	// would deadlock the pump against the very reply the Send is waiting
	// for (see CDPSession.On's doc). kiosktargets.go therefore hands
	// Attach/Detach off to a fresh goroutine rather than calling them
	// inline from its Target.attachedToTarget/detachedFromTarget handlers.
	transitionMu sync.Mutex

	mu sync.RWMutex
	// targets maps a CDP target's sessionId to its session view. The
	// empty key is the top-level kiosk page; non-empty keys are flat-mode
	// child targets (cross-origin iframes). A single displayed artwork can
	// span several targets at once (the page plus its out-of-process
	// iframe), and Fetch is a per-target domain, so interception must be
	// enabled — and paused requests answered — on each target
	// independently. nil-safe: constructed empty in NewReplayer.
	targets map[string]CDPSession
	// enabled records whether replay has asked Chromium to keep Fetch
	// interception on for the current scope. It stays true through a
	// failed Disable (which cannot prove the domain is actually off), so
	// a child target that attaches after such a failure is still armed.
	// It is the trigger for attach-time Fetch.enable on a newly attached
	// child (see attachChild).
	enabled bool
	// resources is the flat resourceKey(method,url)->resource lookup for
	// every item currently in scope, i.e. the union of each enabled
	// item's Resources. A flat map (rather than per-item scoping) is what
	// lets EnableForPlaylist serve a multi-item playlist correctly: nil
	// when disabled. Keyed by method as well as URL — see
	// Resource.Method's doc for why a paused request must never be
	// fulfilled from a resource captured for a different method to the
	// same URL. The one deliberate exception is processRequestPaused's
	// HEAD fallback (fulfillHeadFromGet): a HEAD is answered from the
	// SAME URL's GET resource, never a different URL's, because HEAD is
	// itself defined as "GET without a body" — this substitutes method
	// only, never risks serving the wrong resource. Shared across every
	// target: all targets of one displayed playlist replay from the same
	// captured resource union.
	resources map[string]Resource
	// mixedScope mirrors EnableForPlaylist's mixed parameter for the
	// currently-enabled scope; read alongside resources under the same
	// lock so a miss decision always sees a consistent (resources, mixed)
	// pair. See EnableForPlaylist's doc for why this exists.
	mixedScope bool

	// admission is a non-blocking counting semaphore (buffered channel;
	// send=acquire, receive=release) bounding concurrent
	// processRequestPaused work — see onRequestPaused's doc on
	// requestPausedAdmission for why. Sized once at construction time;
	// never resized.
	admission chan struct{}
	// overflowAdmission is the same shape for the overflow path, which
	// is cheap per goroutine but not instantaneous — see
	// OverflowAdmission's doc.
	overflowAdmission chan struct{}
	// saturationRetiring keeps one saturation episode to one retirement
	// goroutine — see retireOnSaturation.
	saturationRetiring atomic.Bool
	// onScopeLost is SetOnScopeLost's handler. Atomic rather than
	// mutex-guarded because it is read from the retirement goroutine
	// while main.go may still be setting it during startup wiring.
	// Reading it is cheap and lock-order-free; it is the INVOCATION that
	// must stay outside transitionMu — see notifyScopeLost.
	onScopeLost atomic.Pointer[func()]
}

func (r *replayer) SetOnScopeLost(fn func()) {
	if fn == nil {
		r.onScopeLost.Store(nil)
		return
	}
	r.onScopeLost.Store(&fn)
}

// notifyScopeLost invokes the registered handler, if any.
//
// Callers must NOT hold transitionMu. The handler wired today cannot
// deadlock on it — ForceRefresh is a non-blocking send on a buffered
// channel, and the SyncPlaylist it schedules runs on the refresher's own
// goroutine — but the recovery this triggers does take transitionMu to
// re-attach and re-arm, so any handler that ever waits for that recovery
// to finish would deadlock against it. Keeping the call outside the lock
// makes that a property of this seam rather than of one caller's current
// implementation.
func (r *replayer) notifyScopeLost() {
	if fn := r.onScopeLost.Load(); fn != nil {
		(*fn)()
	}
}

// NewReplayer constructs a Replayer. staticServer's BaseURL is used to let
// the static-asset follow-up request (from a large-asset 302) pass
// through untouched instead of being re-intercepted. kioskShellURL is the
// bundled local player's own URL (constant.WEBAPP_URL) — requests to its
// origin are likewise never intercepted, so daemon-driven recovery
// navigations (boot recovery, refreshArtwork escalation, watchdog
// navigate) still work while a fail-closed scope is armed. An empty or
// unparsable kioskShellURL disables that exemption rather than failing
// construction.
func NewReplayer(store Store, staticServer StaticServer, missPolicy MissPolicy, kioskShellURL string, jsonWrapper wrapper.JSON, logger *zap.Logger) Replayer {
	if missPolicy == "" {
		missPolicy = MissPolicyFailClosed
	}
	var shellOrigin *url.URL
	if kioskShellURL != "" {
		if parsed, err := url.Parse(kioskShellURL); err == nil && parsed.Scheme != "" && parsed.Host != "" {
			shellOrigin = parsed
		} else if logger != nil {
			logger.Warn("offline cache replay: kiosk shell URL unparsable, shell-origin pass-through disabled",
				zap.String("url", kioskShellURL), zap.Error(err))
		}
	}
	return &replayer{
		store:             store,
		staticServer:      staticServer,
		missPolicy:        missPolicy,
		kioskShellOrigin:  shellOrigin,
		json:              jsonWrapper,
		logger:            logger,
		targets:           make(map[string]CDPSession),
		admission:         make(chan struct{}, RequestPausedAdmission),
		overflowAdmission: make(chan struct{}, OverflowAdmission),
	}
}

// fetchEnablePatternAll is Fetch.enable's params: pattern "*" so every
// request the target makes is paused and routed through
// onRequestPaused. Shared by EnableForPlaylist and attach-time child
// enablement so both arm interception identically.
func fetchEnablePatternAll() map[string]interface{} {
	return map[string]interface{}{
		"patterns": []map[string]interface{}{{"urlPattern": "*"}},
	}
}

func (r *replayer) Attach(sessionID string, session CDPSession) {
	// See transitionMu's doc: held for this whole swap so a concurrent
	// EnableForPlaylist/Disable cannot be caught mid-flight holding a
	// reference to a session this call is about to supersede/close, and
	// so attach-time Fetch.enable (child path) is serialized against
	// scope transitions.
	r.transitionMu.Lock()
	defer r.transitionMu.Unlock()

	if sessionID == "" {
		r.attachRoot(session)
		return
	}
	// Result ignored: this entry point does not drive target resumption
	// (kiosktargets.go uses AttachChild for that) — see attachChild's doc.
	_ = r.attachChild(sessionID, session)
}

// AttachChild is the reconnect-race-safe entry point for kiosktargets.go
// — see the Replayer interface doc for why the plain Attach above is
// unsafe for that specific caller. Holding transitionMu across BOTH the
// root-currency check and attachChild's own mutation is what makes this
// atomic with respect to a concurrent Attach("", newRoot) call: that
// call also takes transitionMu for its whole attachRoot swap (see
// Attach's own doc), so there is no window between "root is still
// current" and "attach the child" where a reconnect could land.
func (r *replayer) AttachChild(root CDPSession, sessionID string, session CDPSession) bool {
	r.transitionMu.Lock()
	defer r.transitionMu.Unlock()

	if !r.isCurrentRootLocked(root) {
		return false
	}
	return r.attachChild(sessionID, session)
}

// isCurrentRootLocked reports whether root is the top-level session most
// recently installed by Attach("", root) (i.e. attachRoot has not since
// superseded it with a later reconnect). Caller must hold transitionMu —
// see AttachChild/DetachChild's docs for why that is what makes this
// check atomic with respect to a concurrent attachRoot swap, rather than
// a separate query a caller could race against.
func (r *replayer) isCurrentRootLocked(root CDPSession) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	current, ok := r.targets[""]
	return ok && current == root
}

// attachRoot handles a fresh top-level connection (initial attach or a
// reconnect after a kiosk/OOM restart). A new top-level session is a
// generation boundary: every child target from the prior connection
// belonged to a now-dead socket and its sessionId is meaningless against
// this one, so the entire target set is replaced. Scope is reset too —
// the new socket starts with Fetch disabled, and whichever caller next
// runs SyncPlaylist (main.go's onConnect ForceRefresh, or the next
// displayPlaylist) re-establishes it. Caller holds transitionMu.
func (r *replayer) attachRoot(session CDPSession) {
	r.mu.Lock()
	previous := r.targets[""]
	r.targets = map[string]CDPSession{"": session}
	r.resources = nil
	r.mixedScope = false
	r.enabled = false
	r.mu.Unlock()

	// The handler closes over (sessionID="", session) — this call's
	// specific arguments, not a re-read of r.targets — and
	// processRequestPaused uses that bound session for every
	// Fetch.fulfillRequest/continueRequest/failRequest it sends (see its
	// doc). This guarantees a Fetch.requestPaused event delivered on THIS
	// session's read pump is always answered on THIS SAME session, even
	// if a later reconnect has since replaced the top-level target before
	// this event's handler goroutine runs. Binding to a re-read of
	// r.targets instead would let a delayed event from a superseded
	// connection answer on the REPLACEMENT connection using a requestId
	// that only ever existed on the old one.
	session.On("Fetch.requestPaused", func(params json.RawMessage) {
		r.onRequestPaused("", session, params)
	})

	// The old connection is normally already dead by reconnect time, but
	// closing it deterministically stops its read-pump goroutine and
	// releases its socket instead of relying on that connection's own
	// read error to eventually notice — avoiding a goroutine/fd leak
	// across repeated reconnects. Its child views shared that same socket,
	// so dropping them from the map above is enough; they need no
	// individual Close.
	if previous != nil && previous != session {
		if err := previous.Close(); err != nil {
			r.logger.Debug("offline cache replay: closing superseded CDP session", zap.Error(err))
		}
	}
}

// attachChild adds a flat-mode child target (a cross-origin iframe). It is
// purely additive to the target set and, if a scope is already enabled,
// arms Fetch on the new target immediately: a child can attach at any
// point after EnableForPlaylist last ran (an iframe is created only once
// the kiosk navigates to the artwork, which happens AFTER SyncPlaylist in
// the display path, and can also re-attach mid-playback on an iframe
// navigation), so relying solely on the next EnableForPlaylist pass would
// leave the iframe's own requests un-intercepted in between. Caller holds
// transitionMu; this may Send (Fetch.enable), so it must not run on the
// CDP read pump — see kiosktargets.go.
// Returns whether the caller may resume this child target. Arming Fetch
// is the whole precondition for resuming it: a child resumed with
// interception NOT armed runs completely outside replay, so every request
// it makes goes straight to the network — silently, while the scope still
// claims fail_closed and status still reports the item cached. Leaving it
// paused instead is the fail-closed-consistent outcome: a visibly stalled
// iframe rather than an invisible violation of the guarantee offline mode
// exists to make. Under a policy that already permits the network for a
// miss (pass_through, or a mixed scope), that trade is not worth making,
// so resuming is allowed there.
func (r *replayer) attachChild(sessionID string, session CDPSession) (resumable bool) {
	r.mu.Lock()
	if existing, ok := r.targets[sessionID]; ok && existing != session {
		// Same sessionId re-reported with a different view: drop the
		// stale view's handlers before replacing so they stop receiving
		// this target's events. This closes before the new view's On
		// registers just below, so an event for this sessionId arriving
		// in that tiny window would be dropped — acceptable because CDP
		// does not reuse a sessionId for a different target, so this
		// branch is a defensive guard for a report that should not occur,
		// not a normal path.
		if err := existing.Close(); err != nil {
			r.logger.Debug("offline cache replay: closing superseded child target session", zap.Error(err))
		}
	}
	r.targets[sessionID] = session
	enabled := r.enabled
	r.mu.Unlock()

	session.On("Fetch.requestPaused", func(params json.RawMessage) {
		r.onRequestPaused(sessionID, session, params)
	})

	if !enabled {
		// No scope is active, so there is nothing to arm and nothing for
		// this child to escape: replay is not intercepting anything right
		// now, on any target.
		return true
	}

	if _, err := session.Send(context.Background(), "Fetch.enable", fetchEnablePatternAll()); err != nil {
		r.logger.Warn("offline cache replay: Fetch.enable on newly attached child target failed",
			zap.String("session_id", sessionID), zap.Error(err))
		// A transport failure means this session is finished — retire it
		// rather than leaving a corpse in the target set. Note that for a
		// CHILD this does NOT release anything Chromium has paused:
		// flatSession.Close is a per-target detach that only unregisters
		// handlers (see its doc, and retireOnSaturation for where that
		// distinction is load-bearing). Benign here — under fail_closed
		// this child is deliberately left paused anyway, and the socket
		// carrying it is already dead — but do not read this as a release.
		r.retireIfDeadLocked(sessionID, session, err, "Fetch.enable (child attach)")

		r.mu.RLock()
		networkAlreadyPermitted := r.missPolicy == MissPolicyPassThrough || r.mixedScope
		r.mu.RUnlock()
		if !networkAlreadyPermitted {
			r.logger.Warn("offline cache replay: leaving child target paused, its requests would bypass offline replay",
				zap.String("session_id", sessionID))
		}
		return networkAlreadyPermitted
	}
	return true
}

func (r *replayer) Detach(sessionID string) {
	// The top-level target is never detached this way — it only changes
	// through attachRoot's generation boundary. Ignore a stray root
	// detach so it can never wipe live interception.
	if sessionID == "" {
		return
	}
	r.transitionMu.Lock()
	defer r.transitionMu.Unlock()
	r.detachLocked(sessionID)
}

// DetachChild is Detach's reconnect-race-safe counterpart, symmetric
// with AttachChild — see both docs. Caller (kiosktargets.go) passes the
// root session the Target.detachedFromTarget event was delivered on;
// this only proceeds if that root is still current, atomically with
// respect to a concurrent reconnect (transitionMu is the same lock
// attachRoot's swap uses).
func (r *replayer) DetachChild(root CDPSession, sessionID string) {
	if sessionID == "" {
		return
	}
	r.transitionMu.Lock()
	defer r.transitionMu.Unlock()

	if !r.isCurrentRootLocked(root) {
		return
	}
	r.detachLocked(sessionID)
}

// detachLocked is Detach/DetachChild's shared body: drop sessionID from
// the target set and close its session view. Caller must hold
// transitionMu.
func (r *replayer) detachLocked(sessionID string) {
	r.mu.Lock()
	session := r.targets[sessionID]
	delete(r.targets, sessionID)
	r.mu.Unlock()

	if session != nil {
		// flatSession.Close unregisters this target's handlers only; it
		// does not touch the shared socket (see its doc).
		if err := session.Close(); err != nil {
			r.logger.Debug("offline cache replay: closing detached child target session", zap.Error(err))
		}
	}
}

// snapshotTargets returns every currently-attached target's session.
// Caller must hold mu (read or write). The returned slice is a copy, so
// callers can Send to each without holding mu across the round-trip.
// snapshotTargets copies the target set so a Send loop can run without
// holding mu. Keyed by sessionID, not a bare slice: a Send failure has to
// be attributable back to the exact map entry it came from so
// retireIfDeadLocked can forget that one session (and only if it has not
// since been superseded).
func (r *replayer) snapshotTargets() map[string]CDPSession {
	sessions := make(map[string]CDPSession, len(r.targets))
	for id, s := range r.targets {
		sessions[id] = s
	}
	return sessions
}

func (r *replayer) EnableForItem(ctx context.Context, source string) error {
	return r.EnableForPlaylist(ctx, []string{source}, false)
}

func (r *replayer) EnableForPlaylist(ctx context.Context, sources []string, mixed bool) error {
	resources := make(map[string]Resource)
	for _, source := range sources {
		rec, err := r.store.LoadItem(SourceKey(source))
		if err != nil {
			return fmt.Errorf("offline cache replay: load item %s: %w", source, err)
		}
		for _, res := range rec.Resources {
			resources[resourceKey(res.Method, res.URL)] = res
		}
	}

	// See transitionMu's doc: held across the ENTIRE snapshot-targets
	// through Fetch.enable Sends below so a concurrent Attach cannot
	// swap/close a session in between — otherwise this call could send
	// Fetch.enable to a connection already being torn down while
	// r.resources/r.mixedScope (committed moments later, unaffected by
	// this lock) claim a scope is active, silently leaving a target
	// without interception enabled. Setting r.enabled=true under mu
	// BEFORE releasing it also closes the race with a child attaching
	// concurrently: attachChild then observes enabled and arms Fetch on
	// itself, so the new child is covered whether it attached just before
	// or just after this snapshot.
	r.transitionMu.Lock()
	defer r.transitionMu.Unlock()

	r.mu.Lock()
	sessions := r.snapshotTargets()
	r.resources = resources
	r.mixedScope = mixed
	r.enabled = true
	r.mu.Unlock()

	if len(sessions) == 0 {
		return fmt.Errorf("offline cache replay: no CDP session attached")
	}
	// Fetch is a per-target domain: enable it on every attached target
	// (the top-level page and each cross-origin iframe) so a paused
	// request on any of them routes through onRequestPaused. Report the
	// first failure but still try the rest — a single wedged target must
	// not leave the others un-armed.
	var firstErr error
	for sessionID, session := range sessions {
		if _, err := session.Send(ctx, "Fetch.enable", fetchEnablePatternAll()); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("offline cache replay: Fetch.enable: %w", err)
			}
			r.retireIfDeadLocked(sessionID, session, err, "Fetch.enable")
		}
	}
	return firstErr
}

// retireIfDeadLocked closes and forgets a session whose Send failed for a
// transport reason, so a socket that can never work again is not left
// installed as though it were live. Caller holds transitionMu (both call
// sites are inside its critical section, which is also what makes the
// close safe against a concurrent Attach swapping the same session).
//
// This exists because a gorilla/websocket connection is unusable after a
// write error or an exceeded write deadline, yet nothing here used to
// notice: a failed Send was reported to the caller and the dead session
// stayed in r.targets. Every later SyncPlaylist then sent into the same
// corpse, and because the socket was never Close()d, Chromium could still
// consider this client attached with Fetch armed at pattern "*" — CDP
// releases paused requests when a client DISCONNECTS, so a half-dead
// socket that is never closed can leave requests paused forever and hang
// playback. Closing is what makes Chromium let go; forgetting is what
// lets KioskReplay.SyncPlaylist notice the root is gone and re-dial.
//
// Retirement is driven by ErrCDPTransport, which cdpsession.go stamps on
// exactly the failures that leave the connection unusable — never on a
// CDP error REPLY (the peer answered; the command was merely rejected)
// and never on the caller's own expired context. Inferring "dead" by
// exclusion here instead would let a marshal bug or a rejected
// Fetch.enable tear down a perfectly healthy connection.
func (r *replayer) retireIfDeadLocked(sessionID string, session CDPSession, err error, op string) {
	if !errors.Is(err, ErrCDPTransport) {
		return
	}

	r.retireLocked(sessionID, session, "transport failure on "+op)
}

// retireLocked drops a session from the target set and closes it. Caller
// holds transitionMu.
//
// Closing is the load-bearing half. CDP releases every request a client
// has paused when that client DISCONNECTS, so closing is what hands the
// page back its pending requests; merely forgetting the session would
// leave them paused with nobody left to answer.
func (r *replayer) retireLocked(sessionID string, session CDPSession, reason string) {
	r.mu.Lock()
	// Only forget it if it is still the session we were given: a
	// concurrent Attach may already have superseded this entry, and
	// deleting then would drop the healthy replacement.
	if current, ok := r.targets[sessionID]; !ok || current != session {
		r.mu.Unlock()
		return
	}
	delete(r.targets, sessionID)
	r.mu.Unlock()

	r.logger.Warn("offline cache replay: retiring CDP session",
		zap.String("session_id", sessionID), zap.String("reason", reason))
	if closeErr := session.Close(); closeErr != nil {
		r.logger.Warn("offline cache replay: failed to close retired CDP session",
			zap.String("session_id", sessionID), zap.Error(closeErr))
	}
}

// RootAttached reports whether a top-level session is currently
// installed. KioskReplay.SyncPlaylist uses it to tell "the scope call
// failed but the connection is fine" from "the root was retired as dead
// and a re-dial is needed" — see retireIfDeadLocked.
func (r *replayer) RootAttached() bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.targets[""]
	return ok
}

// Disable clears local scope only AFTER Fetch.disable actually succeeds.
// Clearing resources/mixedScope
// optimistically before the CDP call, as this used to do, meant a failed
// Fetch.disable left Chromium's Fetch interception (pattern "*", so it
// matches every request on the page) potentially still live while local
// state said "disabled, nothing cached, strict policy" — turning every
// subsequent request on whatever is now on screen into a fail_closed
// Fetch.failRequest, i.e. breaking normal online playback outright the
// moment a device moved to (or refreshed) an uncached/online item. On
// failure this instead forces mixedScope=true, the same "cannot
// distinguish a miss from a legitimate live request" relaxation
// EnableForPlaylist's mixed parameter already uses, so at worst a stale
// cached resource keeps being replayed (still correct bytes) while
// everything else passes through to the network — never a network
// failure the page didn't actually have. The caller (KioskReplay.
// SyncPlaylist) propagates the error so it can be logged and retried on
// the next sync pass.
func (r *replayer) Disable(ctx context.Context) error {
	// See transitionMu's doc: same reasoning as EnableForPlaylist —
	// held across the snapshot-through-Fetch.disable-Send sequence so a
	// concurrent Attach cannot swap/close a session this call is about to
	// send Fetch.disable to.
	r.transitionMu.Lock()
	defer r.transitionMu.Unlock()

	r.mu.RLock()
	sessions := r.snapshotTargets()
	r.mu.RUnlock()

	if len(sessions) == 0 {
		// Fetch was never enabled without a session to enable it on, so
		// there is nothing that could still be active — always safe to
		// clear unconditionally.
		r.mu.Lock()
		r.resources = nil
		r.mixedScope = false
		r.enabled = false
		r.mu.Unlock()
		return nil
	}

	// Disable on every target. If ANY target's Fetch.disable fails we
	// cannot prove interception is off everywhere, so we take the same
	// conservative relaxation Disable already uses for a single target:
	// force mixedScope=true (miss => pass-through) rather than clearing
	// scope, and keep enabled=true, so at worst a stale cached resource
	// keeps being replayed (correct bytes) while everything else passes
	// through — never a fail_closed network error the page did not
	// actually have. Try every target even after one fails so the others
	// still get disabled.
	var firstErr error
	for sessionID, session := range sessions {
		if _, err := session.Send(ctx, "Fetch.disable", map[string]interface{}{}); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("offline cache replay: Fetch.disable: %w", err)
			}
			r.retireIfDeadLocked(sessionID, session, err, "Fetch.disable")
		}
	}
	if firstErr != nil {
		r.mu.Lock()
		r.mixedScope = true
		r.mu.Unlock()
		return firstErr
	}

	r.mu.Lock()
	r.resources = nil
	r.mixedScope = false
	r.enabled = false
	r.mu.Unlock()
	return nil
}

// onRequestPaused is registered (bound to a specific session — see
// Attach's doc) as a CDPSession event handler, which runs on that
// session's read-pump goroutine. It must not call session.Send
// synchronously (see cdpsession.go's On doc: the pump could not then
// deliver that Send's own reply, deadlocking against itself), so the
// actual response decision — which itself calls Send — is handed off to
// a goroutine per paused request.
//
// RequestPausedAdmission bounds how many of those goroutines may run
// processRequestPaused's full path (cache lookup + potentially a
// blob-store read for fulfillFromBlob) at once: with Fetch.enable
// patterned on "*", a request-heavy software artwork (many concurrent
// XHR/fetch calls) previously fanned out an unbounded number of these
// per burst, each a candidate to hold a full resource's bytes plus a
// pending CDP send — a resource-exhaustion risk on a disk/RAM-
// constrained kiosk device (feral-file/ffos-user#229 review finding).
// The admission channel is a non-blocking counting semaphore (send to
// acquire, receive to release); acquiring it costs nothing beyond the
// channel op, so the common case (well under the bound) is exactly as
// cheap as before.
//
// A request that finds the semaphore full is NOT queued behind it —
// this handler itself runs on the read pump and must return
// immediately regardless of backlog, so blocking here would stall
// every other CDP reply/event on this connection, not just this one
// request — nor is it silently dropped, which would strand the kiosk
// page's fetch/XHR pending forever. Instead it is resolved right away
// via resolveOverflow, which answers exactly like a cache miss (per
// the configured missPolicy) without ever touching the resources
// lookup or a blob read. This keeps every overflow goroutine O(1)-cheap
// and short-lived, so sustained backlog pressure can lose cache hits
// (an accepted, explicit trade-off — see resolveOverflow's doc).
// Exported (like ClassifyProbeRangeBytes in classify.go) so tests can
// assert against it precisely.
const RequestPausedAdmission = 64

// OverflowAdmission bounds the overflow path itself, which was
// previously an unbounded `go resolveOverflow(...)` per excess event.
// "O(1)-cheap and short-lived" was true of the work each one does, but
// not of how long it can take to do it: resolveOverflow answers via
// CDPSession.Send, which blocks on that session's writeMu — a plain
// mutex, taken BEFORE the send's own deadline context is consulted, so a
// goroutine can queue on it for (queue position) x (write deadline)
// regardless of its own ceiling. With Fetch armed at pattern "*"
// Chromium pauses EVERY request, so a burst against a backpressured
// socket accumulated goroutines at page-load rate on a device already
// carrying OOM pressure from the kiosk (feral-file/ffos-user#229 review
// finding).
//
// Sized well above RequestPausedAdmission because overflow is the wide,
// cheap path: it should absorb a large burst before shedding. Beyond it
// an event is dropped with a warning, which strands that one request
// pending in the page — the same last-resort trade Notifier's bounded WS
// queue already makes for the same reason, and strictly better than the
// alternatives available here: blocking would stall the read pump (and
// with it every CDP reply on this connection, including the ones the
// queued sends are waiting for), and growing without bound is the defect
// itself.
const OverflowAdmission = 512

func (r *replayer) onRequestPaused(sessionID string, session CDPSession, params json.RawMessage) {
	select {
	case r.admission <- struct{}{}:
		go func() {
			defer func() { <-r.admission }()
			r.processRequestPaused(sessionID, session, params)
		}()
	default:
		select {
		case r.overflowAdmission <- struct{}{}:
			go func() {
				defer func() { <-r.overflowAdmission }()
				r.resolveOverflow(sessionID, session, params)
			}()
		default:
			r.retireOnSaturation()
		}
	}
}

// retireOnSaturation gives up once BOTH admission bounds are saturated,
// retiring the ROOT session — deliberately, whichever target's event
// happened to trip it.
//
// Saturation is a property of the SOCKET, not of a target: both
// semaphores are per-replayer and every flat-mode child target shares the
// one connection's write path, so 576 outstanding sends means that
// connection has stopped serving replay for everything on it. Retiring
// the tripping target instead would be a no-op at best and a hang at
// worst — flatSession.Close is a per-target detach that only unregisters
// handlers (it must never close the shared socket, see its doc), so
// Chromium would keep the connection, keep Fetch armed at "*" on that
// iframe, and keep its requests paused, while this replayer no longer has
// a handler to answer them or an entry in targets to re-arm. Closing the
// root closes the actual websocket, which is what makes Chromium release
// every paused request on EVERY target on it, and what makes
// RootAttached() false so SyncPlaylist knows to re-dial.
//
// An earlier revision simply dropped the event with a warning. That was
// wrong in a way the warning hid: a paused request that is never
// continued, fulfilled or failed stays paused forever, so dropping did
// not shed load — it hung that resource, and with it any page waiting on
// it. There is no third option that answers the request, either, since
// answering means Send and Send is exactly what has run out of capacity.
//
// Retiring is the honest way out: the page gets every pending request
// back and proceeds against the live network, and SyncPlaylist re-dials
// and re-arms scope (see RootAttached and SetOnScopeLost).
// That does mean a window where a fail_closed scope is not enforced —
// accepted here, unlike the unarmed-child case, because there is no
// working offline path left to protect: 576 concurrent CDP sends
// outstanding on one socket is a connection that has stopped serving
// replay altogether, and the alternative on offer is a hung page rather
// than a correct one.
//
// Runs on its own goroutine because this is called from the CDP read
// pump, which must never take transitionMu (see its doc — a Send held
// under that lock could not have its reply read). The CAS keeps one
// saturation episode to one retirement goroutine rather than spawning
// one per dropped event, which would reintroduce the unbounded growth
// the admission bounds exist to stop.
func (r *replayer) retireOnSaturation() {
	if !r.saturationRetiring.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer r.saturationRetiring.Store(false)
		func() {
			r.transitionMu.Lock()
			defer r.transitionMu.Unlock()
			r.mu.RLock()
			root, ok := r.targets[""]
			r.mu.RUnlock()
			if !ok {
				// Already retired by another path (a transport failure, or
				// a reconnect swap). Whatever did it left recovery owing
				// either way, so still notify below.
				return
			}
			r.retireLocked("", root,
				"admission saturated; releasing paused requests to the network rather than hanging them")
		}()
		// Deliberately after transitionMu is released — see
		// notifyScopeLost's doc. Without a handler wired this does nothing
		// and recovery waits for playlist-refresher's periodic pass.
		r.notifyScopeLost()
	}()
}

// processRequestPaused always responds using session — the SAME CDP
// connection this specific event was delivered on (bound by Attach's
// closure) — never by re-reading r.session, which a concurrent Attach
// may have already swapped to a different (replacement) connection by
// the time this goroutine runs. An earlier version re-read r.session
// here, which meant a Fetch.requestPaused event delayed just long
// enough to still be in flight when a kiosk reconnect swapped sessions
// would answer using the NEW connection's Send, carrying a requestId
// that only ever existed on the OLD connection — Chromium's DevTools
// protocol has no cross-connection request-ID namespace, so that call
// would at best fail silently and at worst (if IDs happened to collide)
// resolve the wrong in-flight request on the new connection.
func (r *replayer) processRequestPaused(sessionID string, session CDPSession, params json.RawMessage) {
	var evt struct {
		RequestID string `json:"requestId"`
		Request   struct {
			URL    string `json:"url"`
			Method string `json:"method"`
		} `json:"request"`
	}
	if err := r.json.Unmarshal(params, &evt); err != nil {
		r.logger.Warn("offline cache replay: failed to parse Fetch.requestPaused", zap.Error(err))
		return
	}

	r.mu.RLock()
	current, stillAttached := r.targets[sessionID]
	resources := r.resources
	mixed := r.mixedScope
	r.mu.RUnlock()
	// Drop the event outright once this target's session is no longer the
	// attached one: either it detached (Detach dropped it) or a reconnect
	// superseded the whole connection (attachRoot replaced the map and
	// closed the old socket — see its doc), so the frame this request
	// belonged to is gone and responding on the dead session would only
	// ever fail. This is a pure efficiency short-circuit, not the
	// correctness fix itself — session is already the bound value every
	// Send below uses, so even without this check a stale event could
	// never reach the WRONG (new) session; it would just attempt, and
	// benignly fail on, its own closed one.
	if !stillAttached || session != current {
		return
	}

	ctx := context.Background() // decoupled from any caller; this runs off the CDP event pump, not a request context

	// The static server's own URL is replay's own follow-up (a large-asset
	// 302 target): it must always pass through, or intercepting it too
	// (Fetch.enable's pattern is "*") would loop back into this handler.
	// Pass-through here means Fetch.continueRequest — i.e. the request
	// escapes offline isolation and hits the real loopback socket — so the
	// match MUST be an exact loopback-origin + /blobs/ path check, never a
	// prefix test: a crafted URL like http://127.0.0.1:8082@evil.example/
	// has BaseURL() as a string prefix yet resolves (per RFC 3986 userinfo
	// parsing) to host evil.example, which a prefix test would wrongly wave
	// through under fail_closed. See isStaticServerFollowUp.
	if r.staticServer != nil && r.isStaticServerFollowUp(evt.Request.URL) {
		r.continueRequest(ctx, session, evt.RequestID)
		return
	}

	// The kiosk shell (ff-player, constant.WEBAPP_URL) must likewise always
	// pass through: it is served by feral-player.service on loopback, so it
	// is reachable offline and is by construction never in the captured
	// resource map (capture navigates to item.Source, never to the shell).
	// Without this exemption, a fail-closed non-mixed scope — armed exactly
	// when a displayed playlist is fully cached, online or not — would
	// Fetch.failRequest every daemon-driven recovery navigation
	// (playersession.navigateAndVerify's Page.navigate to WEBAPP_URL for
	// boot recovery, refreshArtwork escalation, and watchdog-requested
	// navigations): Chromium renders its error page, the command handler
	// never installs, and only a Chromium restart recovers. Matched by
	// origin equality (never a prefix test — see isStaticServerFollowUp's
	// userinfo hazard) over the WHOLE origin, not just "/": the shell's
	// own JS/CSS bundle requests after the navigation must escape
	// interception too.
	if r.isKioskShellRequest(evt.Request.URL) {
		r.continueRequest(ctx, session, evt.RequestID)
		return
	}

	// Looking up a key on a nil map is safe in Go (returns zero value,
	// ok=false), so no explicit nil-check is needed here when disabled.
	// Keyed by method+URL, not URL alone — see resources' doc for why a
	// paused request must only ever match a resource captured for the
	// SAME method.
	resource, found := resources[resourceKey(evt.Request.Method, evt.Request.URL)]
	if !found {
		// The live kiosk (ff-player's ArtworkPlayer.tsx) unconditionally
		// appends "&display_mode=fit|crop" to a software/iframe artwork's
		// URL right before navigating to it — a UI-local rendering hint
		// chosen from the device's own display settings, added AFTER the
		// DP-1 item.Source was already finalized. capture.go always
		// navigates to the bare item.Source (see its doc) and therefore
		// never observes or stores this parameter, so the FIRST request a
		// freshly displayed item's own top-level document makes is
		// GUARANTEED to miss the exact-URL lookup above — not a partial-
		// capture edge case, but every single iframe-type item, every
		// time. Under MissPolicyFailClosed (the default, and the only
		// policy in effect once mixed=false, i.e. the whole displayed
		// scope is cached — exactly the case offline mode exists for),
		// that unconditional miss fails the navigation itself
		// (net::ERR_FAILED), so the artwork never even starts loading.
		// See docs/offline-artwork-capture.md §6 for the field evidence.
		//
		// Retrying with the known player-appended params stripped closes
		// that gap without weakening the miss policy for anything else:
		// this is a second, narrower lookup attempt, not a change to how
		// resources are stored or keyed, and it only ever removes params
		// from an explicit allowlist (never a blanket query-string drop),
		// so a genuinely different URL can still only ever miss, never
		// be mismatched to the wrong cached resource.
		if normalizedURL := stripPlayerAppendedParams(evt.Request.URL); normalizedURL != evt.Request.URL {
			resource, found = resources[resourceKey(evt.Request.Method, normalizedURL)]
		}
	}
	// ff-player's content-type probe (see headProbeQueryParams' doc)
	// issues a HEAD to the item's source before ever issuing the GET a
	// native <img>/<video>/<audio> element actually renders from — a
	// HEAD-keyed resource is never captured (capture.go/mediacapture.go
	// only ever store a GET), so this is not a rare edge case but an
	// UNCONDITIONAL miss on the two lookups above for every offline
	// media item, every time. Answering it from the same URL's GET
	// resource (headLookupURL strips the probe's own cache-busting
	// params first) is what lets the player determine the correct
	// Content-Type and select its native renderer instead of degrading
	// to the iframe fallback — see docs/offline-artwork-capture.md §6.
	if !found && evt.Request.Method == go_http.MethodHead {
		if getResource, ok := resources[resourceKey(go_http.MethodGet, headLookupURL(evt.Request.URL))]; ok {
			r.fulfillHeadFromGet(ctx, session, evt.RequestID, getResource, mixed)
			return
		}
	}
	if !found {
		r.handleMiss(ctx, session, evt.RequestID, mixed)
		return
	}

	switch {
	case resource.IsRedirect():
		r.fulfill(ctx, session, evt.RequestID, statusOrDefault(resource.Status, go_http.StatusFound), "", nil, resource.RedirectTo, resource.Headers)
	case resource.SHA256 != "":
		r.fulfillFromBlob(ctx, session, evt.RequestID, resource, mixed)
	default:
		// Captured but with no body (e.g. its fetch failed at capture
		// time) — nothing to serve, so treat exactly like a miss.
		r.handleMiss(ctx, session, evt.RequestID, mixed)
	}
}

// isStaticServerFollowUp reports whether rawURL is genuinely one of
// replay's own static-server redirect targets (URLFor emits
// http://<loopback-addr>/blobs/<sha>?...), and therefore safe to pass
// through untouched instead of being intercepted. It exists to replace a
// naive strings.HasPrefix(rawURL, BaseURL()) test, which is unsafe as a
// TRUST gate: HasPrefix matches any string that merely starts with the
// base URL, including one where "127.0.0.1:8082" is actually the RFC 3986
// userinfo of a different host (http://127.0.0.1:8082@evil.example/...).
// Because a match here yields Fetch.continueRequest — the one branch that
// lets a paused request leave the offline sandbox and reach the network —
// this validates the parsed components instead:
//   - the URL must parse,
//   - its scheme+host must EQUAL the static server's (url.Host excludes
//     userinfo, so the lookalike above is rejected: its Host is
//     evil.example, not the loopback addr), and
//   - its path must be under the blobs route the server actually serves.
//
// Anything failing these falls through to normal interception (miss /
// blob fulfillment), which is the safe default — the worst case for a
// false negative is replay intercepting its own redirect and treating it
// as a miss, never a request silently escaping to an attacker-chosen host.
func (r *replayer) isStaticServerFollowUp(rawURL string) bool {
	base, err := url.Parse(r.staticServer.BaseURL())
	if err != nil {
		return false
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	return u.Scheme == base.Scheme &&
		u.Host == base.Host &&
		strings.HasPrefix(u.Path, blobsRoutePrefix)
}

// isKioskShellRequest reports whether rawURL targets the bundled local
// player's own origin (constant.WEBAPP_URL, injected via NewReplayer) —
// see processRequestPaused for why those requests must always pass
// through. Origin equality on parsed components for the same reason
// isStaticServerFollowUp documents: a match yields Fetch.continueRequest,
// the one branch that lets a paused request escape offline isolation, so
// a prefix test would let a userinfo lookalike
// (http://127.0.0.1:8080@evil.example/) through under fail_closed. The
// whole origin is exempt (no path restriction): everything on it is
// served locally by feral-player.service, so it is both offline-reachable
// and never attacker-controlled.
func (r *replayer) isKioskShellRequest(rawURL string) bool {
	if r.kioskShellOrigin == nil {
		return false
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	return u.Scheme == r.kioskShellOrigin.Scheme && u.Host == r.kioskShellOrigin.Host
}

// playerAppendedQueryParams lists query parameters ff-player's
// ArtworkPlayer.tsx appends to a software (iframe) artwork's URL for its
// own live rendering behavior, chosen from the device's current display
// settings — never part of the signed DP-1 item.Source and never seen by
// capture.go's navigation (see processRequestPaused's doc for the
// resulting mismatch this exists to close). This is an explicit,
// deliberately narrow allowlist rather than a blanket "ignore all extra
// query params" normalization: stripping an unknown param could silently
// match an incoming request to the WRONG cached resource (e.g. an
// artwork whose own logic legitimately varies content by an unrelated
// query key), turning a should-be-miss into a wrong-bytes-served bug.
// Only params confirmed (by reading ff-player's source) to be
// content-inert UI hints belong here. If a future ff-player version
// starts appending another such param, add it here — this list is the
// single point of coordination between the two repos for this specific
// mismatch class.
var playerAppendedQueryParams = []string{"display_mode"}

// headProbeQueryParams are the query params ff-player's own
// getContentTypeFromURL appends to the HEAD request it issues to probe
// a media item's Content-Type BEFORE choosing which native element
// (<img>/<video>/<audio>/...) to render it with — never part of the
// signed DP-1 item.Source, and never captured (mediacapture.go downloads
// only the bare item.Source with a GET — see its doc). "v" is a
// cache-busting timestamp that is by construction different on every
// single probe, so a HEAD lookup against the exact live URL is
// GUARANTEED to miss, unconditionally, exactly like
// playerAppendedQueryParams' display_mode case is for a fresh software
// item's first request (see that var's doc). Left unanswered, the
// player's probe fails offline and it falls back to rendering the item
// in a plain iframe instead of its correct native element. Stripped only
// as part of headLookupURL's HEAD-specific retry (never for the
// ordinary GET/exact-URL lookup path), since a genuine artwork query
// param happening to be named "v" must still only ever miss for a GET,
// never be silently dropped.
var headProbeQueryParams = []string{"v", "x-request"}

// stripPlayerAppendedParams removes any playerAppendedQueryParams pairs
// from rawURL's query string — see stripQueryParams' doc for the exact
// preserved-encoding contract this and stripHeadProbeParams both rely
// on.
func stripPlayerAppendedParams(rawURL string) string {
	return stripQueryParams(rawURL, playerAppendedQueryParams)
}

// stripHeadProbeParams removes any headProbeQueryParams pairs from
// rawURL's query string — see headProbeQueryParams' doc for why these
// are only ever stripped for a HEAD lookup retry, never a GET one.
func stripHeadProbeParams(rawURL string) string {
	return stripQueryParams(rawURL, headProbeQueryParams)
}

// stripQueryParams removes any pairs whose key is in params from
// rawURL's query string, preserving everything else — scheme, host,
// path, fragment, and the exact byte encoding/order of every surviving
// query parameter — untouched. This deliberately avoids a
// parse-then-net/url.Values.Encode() round trip: Values.Encode() sorts
// keys alphabetically and re-percent-encodes them, which would silently
// reorder a URL like "?edition_number=0&blockchain=bitmark" (captured
// verbatim, in DP-1 source order) and break the exact-string
// resourceKey match this function exists to restore. Returns rawURL
// unchanged if none of params are present.
//
// Empty pairs are dropped along the way, but ONLY on a query string this
// function is already rewriting. ff-player builds its iframe URL with
// `url.search += "&display_mode=..."`, which on a query-less source (every
// Art Blocks generator URL, and any bare item.Source) emits
// "...?&display_mode=fit" — an empty leading pair. Keeping it would leave
// a dangling "?" after the strip, and "https://host/path?" is not the
// captured key "https://host/path", so a fully cached artwork missed and
// fail-closed to Chromium's error page. ff-player has been fixed to emit
// the right separator; this stays because fielded devices update the
// player bundle and the daemon on separate cadences, and because emitting
// a URL that can never match a captured key is wrong on its own terms.
func stripQueryParams(rawURL string, params []string) string {
	before, after, ok := strings.Cut(rawURL, "?")
	if !ok {
		return rawURL
	}
	base := before
	query := after
	fragment := ""
	if fIdx := strings.IndexByte(query, '#'); fIdx >= 0 {
		fragment = query[fIdx:]
		query = query[:fIdx]
	}

	pairs := strings.Split(query, "&")
	kept := make([]string, 0, len(pairs))
	changed := false
	for _, pair := range pairs {
		key := pair
		if eqIdx := strings.IndexByte(pair, '='); eqIdx >= 0 {
			key = pair[:eqIdx]
		}
		if slices.Contains(params, key) {
			changed = true
			continue
		}
		// Deliberately does NOT set changed: an empty pair alone is not a
		// reason to rewrite a URL whose params we are not stripping, so a
		// query string we do not otherwise touch still returns verbatim
		// below and can only ever miss, never be rewritten into a
		// different resource's key.
		if pair == "" {
			continue
		}
		kept = append(kept, pair)
	}
	if !changed {
		return rawURL
	}
	if len(kept) == 0 {
		return base + fragment
	}
	return base + "?" + strings.Join(kept, "&") + fragment
}

func statusOrDefault(status, fallback int) int {
	if status == 0 {
		return fallback
	}
	return status
}

func (r *replayer) fulfillFromBlob(ctx context.Context, session CDPSession, requestID string, resource Resource, mixed bool) {
	size, err := r.store.BlobSize(resource.SHA256)
	if err != nil {
		r.logger.Warn("offline cache replay: blob size lookup failed, treating as miss",
			zap.String("url", resource.URL), zap.Error(err))
		r.handleMiss(ctx, session, requestID, mixed)
		return
	}

	if size > largeAssetThreshold {
		// Only ever redirect here if the static server is DEFINITELY
		// listening (see StaticServer.IsListening's doc): redirecting
		// regardless would either produce a dead redirect (nobody
		// home, if it never bound) or, worse, one silently absorbed by
		// some unrelated loopback process that happens to occupy the
		// same port — neither of which this method's caller (Chromium,
		// mid-navigation) has any way to distinguish from a genuinely
		// broken cached asset. Falling through to the normal miss path
		// instead is the same honest "not actually available offline"
		// signal replay already gives for any other unreplayable
		// resource.
		if r.staticServer == nil || !r.staticServer.IsListening() {
			r.logger.Warn("offline cache replay: static server unavailable for an oversized cached asset, treating as a miss",
				zap.String("url", resource.URL), zap.Int64("size_bytes", size))
			r.handleMiss(ctx, session, requestID, mixed)
			return
		}
		location := r.staticServer.URLFor(resource.SHA256, resource.ContentType, resource.Headers)
		// The 302 fulfilled here is itself one hop of a redirect chain a
		// cors-mode fetch() will CORS-check independently of the final
		// static-server response (see filterReplayableHeaders' doc) —
		// resource.Headers is threaded through both so neither hop is
		// missing the headers Chromium's own enforcement needs.
		r.fulfill(ctx, session, requestID, go_http.StatusFound, "", nil, location, resource.Headers)
		return
	}

	data, err := r.store.ReadBlob(resource.SHA256)
	if err != nil {
		r.logger.Warn("offline cache replay: blob read failed, treating as miss",
			zap.String("url", resource.URL), zap.Error(err))
		r.handleMiss(ctx, session, requestID, mixed)
		return
	}
	r.fulfill(ctx, session, requestID, inlineFulfillStatus(resource.Status), resource.ContentType, data, "", resource.Headers)
}

// inlineFulfillStatus normalizes a captured resource's status for the
// inline-blob fulfill path (used for anything under largeAssetThreshold).
// Capture only ever has the complete body to hand back here — a 206 was
// what the browser's own request observed live, but fetchAndStoreBody
// (capture.go) always issues an unranged GET and stores the whole asset,
// so replaying that stored 206 verbatim with the full body and no
// Content-Range header is not a valid partial-content response and can
// break range-aware <video>/<audio> elements. 200 is the honest status
// for what is actually being returned; only staticServer's redirect path
// (>largeAssetThreshold) serves real Range/Content-Range semantics via
// http.ServeContent.
func inlineFulfillStatus(status int) int {
	if status == go_http.StatusPartialContent {
		return go_http.StatusOK
	}
	return statusOrDefault(status, go_http.StatusOK)
}

// fulfill issues Fetch.fulfillRequest. body/location are mutually
// exclusive in practice (a redirect has no body; a served asset has no
// Location), but both are threaded through one helper to keep the CDP
// params-building logic in a single place. extraHeaders is the captured
// Resource.Headers subset (see filterReplayableHeaders' doc) — carrying
// it through here rather than only setting Content-Type/Location is what
// lets a replayed cross-origin resource still pass Chromium's own CORS
// enforcement.
func (r *replayer) fulfill(ctx context.Context, session CDPSession, requestID string, status int, contentType string, body []byte, location string, extraHeaders map[string]string) {
	// Must stay a non-nil slice even when empty: a nil Go slice marshals
	// to JSON `null`, and Chromium rejects Fetch.fulfillRequest with
	// "Invalid parameters" when responseHeaders is present-but-null. A
	// headerless cached resource (e.g. a shader file captured with no
	// Content-Type and no extra headers) would then never be fulfilled —
	// its paused request just stalls forever, hanging the artwork on a
	// perpetual "loading" — even though its bytes are cached. An empty
	// `[]` is accepted. This bites hardest for cross-origin iframe
	// (OOPIF) sub-resources, which only started reaching this path once
	// child-target interception was added (see kiosktargets.go).
	headers := []map[string]interface{}{}
	if contentType != "" {
		headers = append(headers, map[string]interface{}{"name": "Content-Type", "value": contentType})
	}
	if location != "" {
		headers = append(headers, map[string]interface{}{"name": "Location", "value": location})
	}
	// Sorted so the emitted header order (and therefore this call's CDP
	// params) is deterministic across runs — map iteration order is not,
	// which would otherwise make test assertions on the exact params
	// flaky for no functional reason.
	names := make([]string, 0, len(extraHeaders))
	for name := range extraHeaders {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		headers = append(headers, map[string]interface{}{"name": name, "value": extraHeaders[name]})
	}

	params := map[string]interface{}{
		"requestId":       requestID,
		"responseCode":    status,
		"responseHeaders": headers,
	}
	if len(body) > 0 {
		params["body"] = base64.StdEncoding.EncodeToString(body)
	}

	if _, err := session.Send(ctx, "Fetch.fulfillRequest", params); err != nil {
		r.logger.Warn("offline cache replay: Fetch.fulfillRequest failed",
			zap.String("request_id", requestID), zap.Error(err))
	}
}

// headLookupURL strips both playerAppendedQueryParams and
// headProbeQueryParams from rawURL, for the HEAD-specific retry only
// (see processRequestPaused's HEAD-fallback branch). Stripping both
// (rather than just headProbeQueryParams) is defensive, not currently
// load-bearing: display_mode is appended only ahead of a software
// item's iframe navigation, never a media probe, but a HEAD lookup
// paying for both strips costs nothing extra and keeps this in lockstep
// automatically if that ever changes. Both stripXParams helpers are
// no-ops when their params are absent, so this returns rawURL unchanged
// when neither is present.
func headLookupURL(rawURL string) string {
	return stripHeadProbeParams(stripPlayerAppendedParams(rawURL))
}

// fulfillHeadFromGet answers a HEAD request using the SAME URL's GET
// resource — see resources' doc for why this is the one deliberate
// exception to the "never fulfill from a different method's resource"
// rule: HEAD is itself defined as "GET without a body", so this
// substitutes method only, never risks serving bytes for the wrong
// resource. mixed is forwarded to handleMiss for the SHA256=="" case
// below so that fallback's pass-through-vs-fail-closed behavior matches
// every other miss the caller could have taken instead.
func (r *replayer) fulfillHeadFromGet(ctx context.Context, session CDPSession, requestID string, resource Resource, mixed bool) {
	if resource.IsRedirect() {
		// A HEAD should observe the exact same redirect a GET to this
		// URL would — no special-casing needed here beyond reusing the
		// ordinary redirect fulfillment.
		r.fulfill(ctx, session, requestID, statusOrDefault(resource.Status, go_http.StatusFound), "", nil, resource.RedirectTo, resource.Headers)
		return
	}
	if resource.SHA256 == "" {
		// The GET itself never got a body at capture time (its own
		// fetch failed) — nothing to answer the probe with either.
		r.handleMiss(ctx, session, requestID, mixed)
		return
	}
	// Empty body is what makes this an honest HEAD response rather than
	// a GET answered on the wrong method: status/Content-Type/headers
	// mirror the stored GET resource exactly, but body is nil.
	r.fulfill(ctx, session, requestID, statusOrDefault(resource.Status, go_http.StatusOK), resource.ContentType, nil, "", resource.Headers)
}

func (r *replayer) continueRequest(ctx context.Context, session CDPSession, requestID string) {
	if _, err := session.Send(ctx, "Fetch.continueRequest", map[string]interface{}{"requestId": requestID}); err != nil {
		r.logger.Warn("offline cache replay: Fetch.continueRequest failed",
			zap.String("request_id", requestID), zap.Error(err))
	}
}

func (r *replayer) handleMiss(ctx context.Context, session CDPSession, requestID string, mixed bool) {
	// mixed means the enabled scope is a strict subset of a playlist's
	// items (see EnableForPlaylist's doc): a miss here cannot be
	// distinguished from "belongs to an uncached sibling item that must
	// still reach the live network", so pass-through is the only choice
	// that does not break that sibling's playback. The configured
	// missPolicy's fail_closed guarantee only holds when the full set of
	// resources the page can request is actually known.
	if mixed || r.missPolicy == MissPolicyPassThrough {
		r.continueRequest(ctx, session, requestID)
		return
	}
	// Fetch.failRequest reports a genuine network-error condition to the
	// page, which is the honest "offline and not cached" shape, distinct
	// from fulfilling with an HTTP error status.
	if _, err := session.Send(ctx, "Fetch.failRequest", map[string]interface{}{
		"requestId":   requestID,
		"errorReason": "Failed",
	}); err != nil {
		r.logger.Warn("offline cache replay: Fetch.failRequest failed",
			zap.String("request_id", requestID), zap.Error(err))
	}
}

// resolveOverflow answers a Fetch.requestPaused event that arrived while
// RequestPausedAdmission's semaphore was full — see onRequestPaused's
// doc for why this path exists and why it must stay cheap. It resolves
// EXACTLY like a genuine cache miss (handleMiss, keyed on the configured
// missPolicy) and deliberately never touches r.resources or reads a
// blob: doing either here would defeat the whole point of admission by
// letting overflow traffic perform the same I/O the semaphore exists to
// bound. A request answered this way is therefore always treated as a
// miss even when resources actually has a matching entry — losing that
// hit deterministically under sustained backlog pressure is the accepted
// trade-off for guaranteeing this path can never itself accumulate
// unbounded work.
//
// This still needs its own goroutine (never call Send from the read
// pump — see processRequestPaused's doc), but unlike the admitted path
// it does only one bounded Send and returns, so even a large burst of
// concurrent overflow events cannot reproduce the original leak: each
// instance is short-lived and holds no resource bytes.
func (r *replayer) resolveOverflow(sessionID string, session CDPSession, params json.RawMessage) {
	var evt struct {
		RequestID string `json:"requestId"`
		Request   struct {
			URL string `json:"url"`
		} `json:"request"`
	}
	if err := r.json.Unmarshal(params, &evt); err != nil {
		r.logger.Warn("offline cache replay: failed to parse Fetch.requestPaused during admission overflow", zap.Error(err))
		return
	}

	r.mu.RLock()
	current, stillAttached := r.targets[sessionID]
	mixed := r.mixedScope
	r.mu.RUnlock()
	// Same stale-session guard as processRequestPaused — see its doc.
	if !stillAttached || session != current {
		return
	}

	ctx := context.Background()

	// The static server's own follow-up must still pass through even
	// under backlog pressure — see processRequestPaused's identical
	// check for why this cannot be collapsed into an ordinary miss.
	if r.staticServer != nil && r.isStaticServerFollowUp(evt.Request.URL) {
		r.continueRequest(ctx, session, evt.RequestID)
		return
	}

	// So must the kiosk shell's own requests: failing a recovery
	// navigation because it happened to land during backlog pressure
	// would reintroduce (nondeterministically) the exact wedge the
	// processRequestPaused exemption exists to prevent. Both checks are
	// pure in-memory URL parses — no store I/O — so they preserve this
	// path's cheapness contract.
	if r.isKioskShellRequest(evt.Request.URL) {
		r.continueRequest(ctx, session, evt.RequestID)
		return
	}

	r.logger.Warn("offline cache replay: Fetch.requestPaused admission full, resolving via miss policy without a cache lookup",
		zap.String("url", evt.Request.URL))
	r.handleMiss(ctx, session, evt.RequestID, mixed)
}
