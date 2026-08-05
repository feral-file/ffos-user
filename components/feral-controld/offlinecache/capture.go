package offlinecache

import (
	"context"
	"encoding/json"
	"fmt"
	go_http "net/http"
	go_url "net/url"
	"sort"
	"strings"
	"sync"
	"time"

	dp1playlist "github.com/display-protocol/dp1-go/playlist"
	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

// captureWindowDefault bounds how long capture observes network activity
// after navigation before finalizing the record, when the caller does not
// specify one. Software artworks typically finish their initial resource
// fetches within a few seconds; this is generous enough to tolerate a
// slow origin without hanging a download job forever — anything still in
// flight when the window elapses is simply absent from the record and
// Coverage reflects that.
const captureWindowDefault = 20 * time.Second

// captureFinalizeWindowDefault bounds how long resolveResources' final-
// ization phase may keep STARTING new resource fetches. Unlike
// captureWindowDefault (which only bounds PASSIVE CDP observation),
// finalization makes ACTIVE network calls, one resource at a time. With
// no bound of its own it ran on the caller's unbounded daemon-lifetime
// ctx, so a page whose observation window ends with many stalled or
// slow-responding resources still pending could keep the phase going
// indefinitely — and since capture holds the single download worker's
// slot for its ENTIRE duration (see service.go's captureMu), that starves
// every other queued download.
//
// It is deliberately a gate on STARTING work, not a bound on the work
// itself. An earlier revision passed this deadline down into each fetch
// as well, which meant one healthy multi-hundred-MB asset was canceled
// mid-stream the moment the phase hit 60s and recorded as a permanent
// partial no matter how fast it was actually downloading — the exact
// gigabyte-scale case docs/offline-artwork-capture.md §4.4 documents as
// supported. One deadline cannot both "stop a pile of stalled resources
// from monopolizing the worker" and "let a single huge healthy transfer
// finish"; each transfer now bounds itself on progress instead (see
// resourceTransfer), leaving this free to do only the first job.
//
// Worst-case wall clock for the phase is therefore this window plus one
// in-flight transfer's own ceiling, not (resource count) x anything:
// once it elapses, the loop's phaseCtx.Err() check short-circuits BEFORE
// any further network attempt, so the remaining iterations are pure
// bookkeeping however many resources are left.
const captureFinalizeWindowDefault = 60 * time.Second

// clearObservedOriginsStorageWindow bounds the aggregate wall-clock cost
// of clearObservedOriginsStorage's post-capture per-origin cleanup loop.
// Each individual Storage.clearDataForOrigin call is already bounded by
// CDPSession's own per-send timeout (defaultSendTimeout, 15s in
// production — see cdpsession.go), but that only bounds ONE origin: an
// artwork whose navigation, redirect chain, and subresources touched
// many distinct origins could otherwise cost up to
// (origin count) * defaultSendTimeout in the worst case (e.g. several
// slow/wedged origins), and since capture holds the single download
// worker's slot for its entire duration (see service.go's captureMu),
// that would monopolize it well past captureWindowMs, starving every
// other queued download. Deriving a bounded child context here caps the
// worst case at this constant regardless of origin count: once it
// elapses, the loop's own ctx.Err() check stops attempting further
// clears and logs how many origins were left uncleared, rather than
// letting the loop keep blocking — the SAME "best-effort, only affects
// how clean the NEXT job's starting state is" trade-off
// clearObservedOriginsStorage's own doc already accepts, just now also
// bounded in aggregate time rather than only in eventual outcome.
const clearObservedOriginsStorageWindow = 30 * time.Second

// Capturer drives one headless-Chromium capture of a playlist item's
// source URL and writes the result to the Store.
//
// Byte fetching is deliberately NOT done through CDP's
// Network.getResponseBody: Chromium base64-encodes response bodies over
// the DevTools websocket, which both risks the same large-body limits that
// force replay onto the static-server fallback for big assets (see
// staticserver.go) and pointlessly re-transfers bytes controld's own
// HTTP client can fetch directly and stream to disk. Instead, capture uses
// CDP Network events (requestWillBeSent/responseReceived/loadingFailed)
// only to learn WHICH URLs the page actually requested, their method,
// status, and redirect targets, then re-fetches each successful
// GET/HEAD resource's bytes with the same method (a successful OPTIONS
// gets a stored empty body instead, never a re-fetch — see
// resolveResources' dedicated OPTIONS case; see safeIdempotentMethods'
// doc for why methods other than GET/HEAD/OPTIONS are deliberately left
// unfetched rather than blindly re-issued). This is correct for the
// static JS/CSS/HTML/image/video assets software artworks are built
// from; it is NOT correct for cookie-gated or per-request-dynamic
// resources. That trade-off is accepted here and documented in
// offline-artwork-capture.md.
//
//go:generate mockgen -source=capture.go -destination=../mocks/offlinecache_capture.go -package=mocks -mock_names=Capturer=MockOfflineCacheCapturer
type Capturer interface {
	// Capture navigates to item.Source in a fresh headless Chromium page
	// (acquired from Downloader), observes network activity for up to
	// captureWindowMs (0 uses captureWindowDefault), fetches every
	// discovered resource's bytes, and saves the resulting ItemRecord to
	// the store. It returns the saved record.
	Capture(ctx context.Context, item dp1playlist.PlaylistItem, captureWindowMs int) (*ItemRecord, error)
	// Close tears down the headless Chromium the capturer acquires jobs
	// from (see Downloader.Close's doc). Capturer is the only thing
	// Service holds a reference to — Downloader itself is private to
	// this package's Bootstrap wiring — so this delegating method is
	// what lets Service.Stop reach it on daemon shutdown without
	// main.go needing its own handle to the downloader.
	Close() error
}

type capturer struct {
	downloader Downloader
	dialer     wrapper.WebSocketDialer
	// cdpClient talks to OUR OWN capture Chromium's DevTools endpoint on
	// loopback (:9223/json). It must NOT be the guarded client: that
	// guard exists to keep untrusted playlist sources away from loopback,
	// and pointing it at our own browser refuses every software capture
	// before navigation. Trusted destination, so it is the ordinary
	// timeout-bounded client.
	cdpClient wrapper.HTTPClient
	// fetchClient pulls resource bytes from artwork origins — untrusted
	// input, so this one IS guarded (reserved-address policy enforced in
	// its DialContext, no proxy). Keeping the two apart is the whole
	// point: they have opposite trust properties and one client cannot
	// serve both.
	fetchClient wrapper.HTTPClient
	// guard applies the same reserved-address policy to requests CHROMIUM
	// makes. fetchClient only covers bytes this daemon pulls itself; the
	// page is untrusted code that issues its own requests, and those
	// never touch a Go client at all. See attachSourceGuard.
	guard sourceGuard
	store Store
	json  wrapper.JSON
	// io is used only for DialPageSession's small (/json targets list)
	// HTTP body read — fetchAndStoreBody streams resource bodies
	// straight into the store instead, see maxResourceBytes below.
	io    wrapper.IO
	clock wrapper.Clock
	// maxResourceBytes is the whole cache's configured disk budget
	// (service.maxDiskBytes, "<=0 means unlimited"). Each Capture call's
	// resource fetches are cumulatively bounded so that this capture's
	// bytes PLUS what is already on disk never exceed it — see
	// newDiskBudget and captureDiskBudget's docs for why both the
	// per-resource cap AND the existing-usage subtraction are required
	// to make this a genuine hard bound rather than a best-effort hint.
	maxResourceBytes int64
	logger           *zap.Logger
}

// captureDiskBudget bounds the TOTAL bytes one Capture call is allowed to
// stream to disk across every resource it fetches, measured against the
// WHOLE store's configured maxDiskBytes ceiling INCLUDING what other
// already-cached items are currently consuming — not just this one
// capture's own fetches in isolation.
//
// Two layers of undercounting had to be closed to make maxDiskBytes a
// genuine hard bound rather than a post-hoc "try to make room" heuristic:
//
//  1. A per-resource cap alone (WriteBlob's own maxBytes argument) lets a
//     multi-resource artwork write several budget-sized blobs before
//     service.enforceDiskLimit's post-capture eviction ever runs — three
//     resources each just under maxDiskBytes would transiently occupy ~3x
//     the budget. The cumulative `used` accounting here fixes that within
//     a single capture.
//
//  2. Seeding `limit` at the full maxDiskBytes ignored bytes ALREADY on
//     disk from other items. A store sitting at 9.9GB of a 10GB budget
//     could still admit an 8GB capture (nothing in THIS capture exceeds
//     10GB on its own), pushing the filesystem to ~17.9GB before eviction
//     runs. `limit` is therefore seeded at maxDiskBytes MINUS current
//     DiskUsage (see Capture), so the budget reflects real remaining room.
//
// enforceDiskLimit's cross-item eviction still runs afterward and remains
// the mechanism that reclaims space from STALE items; this budget only
// guarantees a single in-flight capture can never itself push total
// on-disk usage past the ceiling before that eviction gets a chance to.
// Counting existing usage is deliberately conservative (a re-capture of an
// item whose old blobs are about to be replaced/GC'd is charged for both
// briefly), which errs toward staying under budget — the safe direction.
type captureDiskBudget struct {
	// unlimited mirrors maxDiskBytes <= 0 ("no ceiling configured"). Kept
	// as an explicit flag rather than overloading limit<=0, because a
	// bounded budget legitimately reaches limit-used == 0 remaining (store
	// already full), which must reject further writes — NOT be misread as
	// "unlimited" the way a raw 0 would be.
	unlimited bool
	limit     int64 // remaining room at capture start (maxDiskBytes - existing usage), clamped >= 0
	used      int64 // cumulative bytes this Capture call has already written
}

// newCaptureDiskBudget builds a budget with `remaining` bytes of room. A
// non-positive remaining is a real "store already at/over budget" state
// for a bounded budget (reserve then rejects every write) — pass
// unlimited=true explicitly for the "no ceiling configured" case instead.
func newCaptureDiskBudget(remaining int64, unlimited bool) *captureDiskBudget {
	if remaining < 0 {
		remaining = 0
	}
	return &captureDiskBudget{unlimited: unlimited, limit: remaining}
}

// reserve returns the maxBytes a caller should pass to the next
// WriteBlob call: capBytes is the remaining room (0 meaning "unlimited",
// WriteBlob's own convention, only when the budget itself is disabled),
// and ok is false once the budget is already exhausted — the caller must
// skip the write entirely in that case rather than pass a 0 cap, which
// WriteBlob would otherwise read as "unlimited" instead of "none left".
func (b *captureDiskBudget) reserve() (capBytes int64, ok bool) {
	if b.unlimited {
		return 0, true
	}
	remaining := b.limit - b.used
	if remaining <= 0 {
		return 0, false
	}
	return remaining, true
}

// record charges size bytes against the budget after a write succeeds.
// Called with the blob's actual on-disk size (via Store.BlobSize), not
// the reserved cap, so a resource smaller than its reservation does not
// spuriously starve the resources fetched after it. A resource that
// turned out to be a store-level dedup of an already-existing blob still
// charges its full size here rather than zero: this capturer has no way
// to learn "was this actually new bytes on disk" from WriteBlob's return
// value alone, and charging the (possibly-redundant) full size is the
// conservative direction to err in for a hard disk-budget guarantee.
func (b *captureDiskBudget) record(size int64) {
	b.used += size
}

// newDiskBudget builds the per-capture disk budget for one Capture call —
// see newDiskBudgetFromStore's doc for the shared logic both this
// (headless-browser, possibly many resources per item) and
// mediaCapturer.Capture (mediacapture.go, exactly one resource per item)
// build their budget with.
//
// Safe to call DiskUsage here without extra locking: the service runs
// captures one at a time under captureMu (see service.process), so no
// other capture is writing blobs concurrently while this reads usage.
func (c *capturer) newDiskBudget() *captureDiskBudget {
	return newDiskBudgetFromStore(c.store, c.maxResourceBytes, c.logger)
}

// newDiskBudgetFromStore builds a captureDiskBudget seeded with the
// store's REMAINING room (maxDiskBytes minus what is already on disk) so
// the ceiling is a true whole-store bound — see captureDiskBudget's doc
// for why current usage must be subtracted rather than treating the full
// maxDiskBytes as this call's own budget. A DiskUsage read failure is
// treated as "assume the store is full" (zero remaining) rather than
// "assume empty": a bounded cache must fail safe toward not overfilling
// the disk when it cannot confirm how full it already is. maxDiskBytes
// <= 0 means no ceiling was configured, so the budget is genuinely
// unlimited regardless of current usage.
//
// Shared by both capture pipelines (capturer.newDiskBudget and
// mediaCapturer.Capture) so a single item downloaded through either path
// is bounded by the identical whole-store disk ceiling, computed the
// same way.
func newDiskBudgetFromStore(store Store, maxDiskBytes int64, logger *zap.Logger) *captureDiskBudget {
	if maxDiskBytes <= 0 {
		return newCaptureDiskBudget(0, true)
	}
	used, err := store.DiskUsage()
	if err != nil {
		logger.Warn("offline cache capture: disk usage check failed, treating store as full for this capture's budget",
			zap.Error(err))
		return newCaptureDiskBudget(0, false)
	}
	return newCaptureDiskBudget(maxDiskBytes-used, false)
}

// NewCapturer takes TWO http clients on purpose. cdpClient reaches our
// own capture Chromium on loopback and must be unguarded; fetchClient
// reaches untrusted artwork origins and must be guarded. Passing one
// client for both is the bug this signature exists to prevent — doing so
// either blocks every software capture (guarded client on loopback) or
// silently drops the SSRF protection on resource fetches.
func NewCapturer(
	downloader Downloader,
	dialer wrapper.WebSocketDialer,
	cdpClient wrapper.HTTPClient,
	fetchClient wrapper.HTTPClient,
	resolver AddrResolver,
	store Store,
	jsonWrapper wrapper.JSON,
	ioWrapper wrapper.IO,
	clockWrapper wrapper.Clock,
	maxDiskBytes int64,
	logger *zap.Logger,
) Capturer {
	return &capturer{
		downloader:       downloader,
		dialer:           dialer,
		cdpClient:        cdpClient,
		fetchClient:      fetchClient,
		guard:            sourceGuard{resolver: resolver},
		store:            store,
		json:             jsonWrapper,
		io:               ioWrapper,
		clock:            clockWrapper,
		maxResourceBytes: maxDiskBytes,
		logger:           logger,
	}
}

func (c *capturer) Capture(ctx context.Context, item dp1playlist.PlaylistItem, captureWindowMs int) (*ItemRecord, error) {
	// Source is the item's cache identity (see SourceKey) and the URL this
	// capture navigates to; the DP-1 item id is optional per spec and
	// deliberately not required here.
	if item.Source == "" {
		return nil, fmt.Errorf("offline cache: item must have a source")
	}
	window := captureWindowDefault
	if captureWindowMs > 0 {
		window = time.Duration(captureWindowMs) * time.Millisecond
	}

	endpoint, err := c.downloader.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("offline cache: acquire headless chromium: %w", err)
	}
	defer c.downloader.Release()

	session, err := DialPageSession(ctx, endpoint, c.cdpClient, c.dialer, c.json, c.io, c.logger)
	if err != nil {
		return nil, fmt.Errorf("offline cache: dial capture session: %w", err)
	}
	// Stop the page BEFORE dropping the session. Closing the session
	// tears down Fetch interception with it, but nothing else tears down
	// the PAGE: downloader.Release only schedules an idle teardown of the
	// whole browser (DefaultHeadlessIdleTeardown, 30s), so without this
	// the untrusted artwork keeps executing unguarded for that entire
	// window. A timer is all it takes —
	// setTimeout(() => fetch('http://127.0.0.1:1111/...'), 25000) — and
	// no DNS control or other infrastructure is needed, which is why this
	// is NOT covered by the accepted rebinding residual.
	defer func() {
		c.stopPageBeforeDetach(session)
		_ = session.Close()
	}()

	tracker := newCaptureTracker()
	c.attachHandlers(session, tracker)
	// Registered BEFORE Fetch.enable below so no paused request can be
	// delivered with no handler to answer it — an unanswered pause hangs
	// that request until the capture window closes.
	guard := c.attachSourceGuard(ctx, session)

	if _, err := session.Send(ctx, "Network.enable", map[string]interface{}{}); err != nil {
		return nil, fmt.Errorf("offline cache: Network.enable: %w", err)
	}
	if _, err := session.Send(ctx, "Page.enable", map[string]interface{}{}); err != nil {
		return nil, fmt.Errorf("offline cache: Page.enable: %w", err)
	}
	if err := c.resetTargetState(ctx, session, item.Source); err != nil {
		return nil, fmt.Errorf("offline cache: reset chromium state before capture: %w", err)
	}

	// Enabled AFTER resetTargetState so the cache/storage clears above
	// cannot be paused by our own handler, and BEFORE navigate so the
	// very first request of the page is already covered.
	if _, err := session.Send(ctx, "Fetch.enable", fetchEnablePatternAll()); err != nil {
		return nil, fmt.Errorf("offline cache: Fetch.enable on capture session: %w", err)
	}
	// Cross-origin iframes and workers run in their own CDP targets whose
	// requests never reach the root handler above — see this function's
	// doc for why guarding only the root is a complete bypass.
	if err := c.enableGuardedAutoAttach(ctx, session, guard); err != nil {
		return nil, fmt.Errorf("offline cache: arm child-target source guard: %w", err)
	}

	navCtx, navCancel := context.WithTimeout(ctx, window)
	defer navCancel()
	if _, err := session.Send(navCtx, "Page.navigate", map[string]interface{}{"url": item.Source}); err != nil {
		return nil, fmt.Errorf("offline cache: Page.navigate: %w", err)
	}

	// There is no single reliable "artwork fully loaded" signal across
	// arbitrary software artworks (some keep polling/streaming
	// indefinitely), so a bounded wall-clock observation window is the
	// simplest signal that is both testable and safe against a runaway
	// capture holding the single download slot forever.
	if err := waitForObservationWindow(ctx, navCtx); err != nil {
		return nil, err
	}

	// finalizeCtx bounds the fetch-bodies phase below independently of
	// the (already-elapsed) observation window — see
	// captureFinalizeWindowDefault's doc for why finalization needs its
	// own deadline rather than running on ctx, the caller's unbounded
	// daemon-lifetime context, unmodified. Derived from ctx (not
	// context.Background()) so a real shutdown/cancellation still
	// propagates through it immediately rather than waiting out the
	// full finalize window.
	//
	// This is now the ONLY bound on those fetches: Bootstrap hands this
	// capturer an http client with no whole-request timeout (see its
	// bodyClient comment) precisely so a large asset is not killed
	// mid-transfer at 30s, which means removing or widening this
	// deadline no longer has a client-side backstop behind it.
	finalizeCtx, finalizeCancel := context.WithTimeout(ctx, captureFinalizeWindowDefault)
	defer finalizeCancel()
	resources, coverage := c.resolveResources(ctx, finalizeCtx, tracker, c.newDiskBudget())

	// resetTargetState above only reaches item.Source's OWN origin,
	// which cannot cover an origin this navigation redirects to or
	// otherwise loads subresources from — those origins are only known
	// once capture has actually happened. Clearing them now, before
	// this session closes, means the NEXT capture job to touch any of
	// these origins (regardless of ITS OWN item.Source) starts clean,
	// closing the redirect/subresource-origin gap resetTargetState's
	// preemptive, source-only clear leaves open. Best-effort: a failure
	// here must not fail an otherwise-successful capture.
	c.clearObservedOriginsStorage(ctx, session, resources)

	rec := &ItemRecord{
		Item:       item,
		Entry:      item.Source,
		Resources:  resources,
		Coverage:   coverage,
		CapturedAt: c.clock.Now().UTC(),
	}
	if err := c.store.SaveItem(rec); err != nil {
		return nil, fmt.Errorf("offline cache: save item %s: %w", item.Source, err)
	}
	return rec, nil
}

func (c *capturer) Close() error {
	return c.downloader.Close()
}

// resetTargetState clears the headless Chromium's HTTP cache and every
// storage bucket (cookies, IndexedDB, Cache Storage, Service Workers,
// local storage, ...) scoped to sourceURL's origin, immediately before
// navigating to it. Downloader deliberately runs one long-lived
// Chromium process with a single persistent --user-data-dir and reuses
// its one page target across every capture job (see downloader.go's
// doc) rather than spinning up a fresh profile per item — cheap and,
// for plain HTTP caching, actually more faithful to the kiosk's own
// long-lived profile (fetchAndStoreBody re-fetches bytes directly over
// HTTP regardless of the browser's cache state, so a same-origin HTTP
// cache hit from an earlier item does not, on its own, cause a resource
// to go unobserved). But a Service Worker, IndexedDB, or Cache Storage
// entry left behind by a PRIOR item sharing the same origin (e.g. two
// playlist items hosted on the same generative-art platform's domain)
// could intercept or seed responses for THIS item without any of it
// ever reaching the network, so resolveResources would never even learn
// those URLs exist. Clearing before every navigation, scoped to this
// item's own origin plus a full cache flush, removes that
// cross-item leakage without the cost of a fresh profile/process per
// job.
//
// This is deliberately only half of the defense: sourceURL's origin is
// the only one knowable BEFORE navigation happens, but a redirect chain
// (see resolveResources' IsRedirect handling) or cross-origin
// subresources can leave THIS item's own state on origins other than
// sourceURL's — see clearObservedOriginsStorage, called after capture,
// for the other half.
func (c *capturer) resetTargetState(ctx context.Context, session CDPSession, sourceURL string) error {
	if _, err := session.Send(ctx, "Network.clearBrowserCache", map[string]interface{}{}); err != nil {
		return fmt.Errorf("Network.clearBrowserCache: %w", err)
	}

	origin, err := requestOrigin(sourceURL)
	if err != nil {
		// item.Source is validated by the caller before Capture reaches
		// here (a malformed URL fails Page.navigate itself, moments
		// later, with a clearer error); skip the origin-scoped clear
		// rather than fail capture entirely just for this defense-in-
		// depth step.
		c.logger.Warn("offline cache: could not determine origin for storage reset, skipping",
			zap.String("source", truncateSourceForLog(sourceURL)), zap.Error(err))
		return nil
	}
	if _, err := session.Send(ctx, "Storage.clearDataForOrigin", map[string]interface{}{
		"origin":       origin,
		"storageTypes": "all",
	}); err != nil {
		return fmt.Errorf("Storage.clearDataForOrigin: %w", err)
	}
	return nil
}

// clearObservedOriginsStorage clears Cache/IndexedDB/Service-Worker/etc.
// storage for every distinct origin actually present among resources —
// i.e. every origin this capture's navigation, its redirect chain, and
// its subresources actually touched, not just sourceURL's own origin
// (resetTargetState's preemptive, pre-navigation clear can only target
// the latter, since a redirect target or subresource origin is not
// knowable before navigating). Running this at the END of a capture,
// rather than only at the start of the NEXT one, means it also fires
// (finishing state cleanup) even if Service.Stop tears the capturer
// down before another job ever starts.
//
// Best-effort by design: called after the record this capture produced
// is already final, so a failure here must never turn an otherwise-
// successful capture into an error — it only affects how clean the
// NEXT job's starting state is, not this one's correctness. The whole
// loop is bounded to clearObservedOriginsStorageWindow in aggregate —
// see that constant's doc for why a per-call timeout alone is not
// enough to bound this function's total cost.
func (c *capturer) clearObservedOriginsStorage(ctx context.Context, session CDPSession, resources []Resource) {
	origins := make(map[string]bool, len(resources))
	for _, res := range resources {
		origin, err := requestOrigin(res.URL)
		if err != nil {
			continue // non-http(s) or unparsable URL; nothing to scope a clear to.
		}
		origins[origin] = true
	}
	if len(origins) == 0 {
		return
	}

	clearCtx, cancel := context.WithTimeout(ctx, clearObservedOriginsStorageWindow)
	defer cancel()
	cleared := 0
	for origin := range origins {
		// Checked before each send (rather than relying solely on
		// session.Send's own per-call ctx.Err() handling) so a deadline
		// that elapses BETWEEN calls skips every remaining origin
		// immediately instead of still issuing one more Send that is
		// certain to fail on an already-expired context.
		if clearCtx.Err() != nil {
			c.logger.Warn("offline cache: post-capture origin storage clear window elapsed, skipping remaining origins",
				zap.Int("origins_cleared", cleared), zap.Int("origins_total", len(origins)))
			return
		}
		cleared++
		if _, err := session.Send(clearCtx, "Storage.clearDataForOrigin", map[string]interface{}{
			"origin":       origin,
			"storageTypes": "all",
		}); err != nil {
			c.logger.Warn("offline cache: post-capture origin storage clear failed",
				zap.String("origin", origin), zap.Error(err))
		}
	}
}

// waitForObservationWindow blocks until either navCtx's per-navigation
// timeout elapses or ctx (the caller's, possibly long-lived/unbounded,
// context) is done, then returns ctx.Err(). navCtx is always derived
// from ctx via context.WithTimeout, so a cancellation of ctx closes
// BOTH Done() channels — and Go's select, faced with two simultaneously
// ready cases, is free to pick either one. Re-checking ctx.Err()
// explicitly after the select (rather than returning early only from a
// dedicated ctx.Done() case, and falling through to resolveResources/
// SaveItem from the navCtx.Done() case) means the caller gets the
// correct outcome regardless of which case the runtime happens to
// choose. This matters because Capture's caller (Service.Stop during a
// recapture, most notably) must never have a canceled-mid-flight
// capture fall through to SaveItem: that would resolve everything still
// in flight as a failure and could overwrite an item that was already
// fully captured and ready with an incomplete record.
func waitForObservationWindow(ctx, navCtx context.Context) error {
	select {
	case <-navCtx.Done():
	case <-ctx.Done():
	}
	return ctx.Err()
}

// safeIdempotentMethods are the HTTP methods resolveResources treats as
// eligible to acquire a body for, one way or another: GET/HEAD via an
// actual re-fetch (fetchAndStoreBody), OPTIONS via a stored empty body
// instead of a re-fetch (see resolveResources' dedicated OPTIONS case —
// a CORS preflight response body is never exposed to page JS per the
// Fetch spec, and re-issuing the bare request ourselves risks a
// different response than the browser's own preflight got). Everything
// else (POST/PUT/PATCH/DELETE, ...) is deliberately left unfetched —
// see resolveResources' unsupported_method branch for why re-issuing
// one of those here would be unsafe.
var safeIdempotentMethods = map[string]bool{
	go_http.MethodGet:     true,
	go_http.MethodHead:    true,
	go_http.MethodOptions: true,
}

// resolveResources turns the tracker's observed network activity into the
// final Resource list, fetching bytes for every successful (2xx) resource
// captured via a safe method and building the Coverage summary from
// anything that failed, returned a non-2xx/non-redirect status, or used a
// method resolveResources will not re-issue. budget caps the TOTAL bytes
// fetched across every resource in this call — see captureDiskBudget's
// doc.
func (c *capturer) resolveResources(ctx, phaseCtx context.Context, tracker *captureTracker, budget *captureDiskBudget) ([]Resource, Coverage) {
	keys, resources, failures, pendingURLs, overflow := tracker.snapshot()

	result := make([]Resource, 0, len(keys))
	var failureReasons []string
	for _, key := range keys {
		res, ok := resources[key]
		if !ok {
			continue
		}
		// phaseCtx carries captureFinalizeWindowDefault; ctx is the
		// capture's own. Once the phase deadline elapses, stop STARTING
		// further fetches rather than letting fetchAndStoreBody try and
		// fail one-by-one for the same underlying reason — this is what
		// keeps a page with many stalled/slow resources from
		// monopolizing the worker. Checked before EVERY resource, not
		// once before the loop, so whatever was already fetched before
		// the deadline hit is kept; only the remainder becomes an
		// explicit, distinctly-labeled incomplete entry.
		//
		// Deliberately a gate on starting work, NOT the bound on the
		// work itself: an earlier revision passed this same deadline
		// down into each fetch, so a perfectly healthy multi-hundred-MB
		// asset was canceled mid-stream at 60s and recorded as a
		// permanent partial however fast it was downloading. Each
		// transfer bounds itself instead — see resourceTransfer.
		if phaseCtx.Err() != nil {
			failureReasons = append(failureReasons, fmt.Sprintf("finalization_deadline_exceeded:%s", res.URL))
			result = append(result, res)
			continue
		}
		method := res.EffectiveMethod()
		switch {
		case res.IsRedirect():
			// No body to fetch — replay fulfills this from
			// RedirectTo/Status alone (see replay.go's onRequestPaused),
			// and the redirect itself was only ever observed, never
			// re-issued, so method safety does not apply here.
		case res.Status < go_http.StatusOK || res.Status >= go_http.StatusMultipleChoices:
			// A non-2xx, non-redirect response (4xx/5xx, or a 304 the
			// page's own request observed via HTTP cache revalidation)
			// has no faithful replay short of storing the exact error
			// body/status the live origin returned at capture time, and
			// fetchAndStoreBody's unconditional re-GET cannot reproduce
			// a conditional-request 304. Marking it incomplete here (an
			// honest miss on replay, per onRequestPaused's default case
			// below) is preferred over silently reporting Complete=true
			// for a resource that would never actually serve as this
			// status offline.
			failureReasons = append(failureReasons, fmt.Sprintf("http_error(%d):%s", res.Status, res.URL))
		case !safeIdempotentMethods[method]:
			// A successful (2xx) but unsafe-method (POST/PUT/DELETE/...)
			// request: re-issuing it here to read its bytes risks
			// triggering the exact side effect (a mutation, an
			// analytics/provenance call) the original request caused,
			// which capture must never do just to build an offline
			// cache. Left unfetched and reported incomplete instead —
			// replay treats it as an honest miss (see onRequestPaused's
			// default case), never as "ready" bytes for the wrong
			// method.
			failureReasons = append(failureReasons, fmt.Sprintf("unsupported_method(%s):%s", method, res.URL))
		case method == go_http.MethodOptions:
			// A CORS preflight is the one safe-idempotent method whose
			// re-fetch is unsafe in a DIFFERENT way than the unsafe
			// methods above: re-issuing a bare OPTIONS with none of the
			// browser's own Origin/Access-Control-Request-Method/
			// Access-Control-Request-Headers can get a genuinely
			// different response than the live preflight did — many
			// CORS-aware servers only special-case OPTIONS (and return
			// their Access-Control-Allow-* headers) when those
			// preflight-specific request headers are present, and treat
			// a bare OPTIONS as an ordinary request otherwise. Rather
			// than try to reconstruct and replay those request headers
			// faithfully, skip the network re-fetch entirely: a
			// preflight response's body is defined by the Fetch spec to
			// never be exposed to page JS in the first place, so the
			// real status/headers already captured live (via
			// Network.responseReceived, above) are all replay actually
			// needs — storing a canonical empty body here (bypassing
			// the network) can never mismatch what the browser saw,
			// unlike a second, differently-shaped HTTP round-trip could.
			hash, storeErr := c.store.WriteBlob(strings.NewReader(""), c.maxResourceBytes)
			if storeErr != nil {
				failureReasons = append(failureReasons, fmt.Sprintf("fetch_failed:%s", res.URL))
			} else {
				res.SHA256 = hash
			}
		case res.SHA256 == "":
			capBytes, ok := budget.reserve()
			if !ok {
				// The cumulative per-item budget is already exhausted by
				// earlier resources in this same capture — skip the
				// fetch entirely rather than pass WriteBlob a 0 cap,
				// which it would read as "unlimited" instead of "none
				// left" (see captureDiskBudget.reserve's doc). Left
				// unfetched and reported incomplete, same shape as
				// fetch_failed, so replay treats it as an honest miss.
				failureReasons = append(failureReasons, fmt.Sprintf("over_disk_budget:%s", res.URL))
				result = append(result, res)
				continue
			}
			hash, size, fetchErr := c.fetchAndStoreBody(ctx, res.URL, method, capBytes)
			if fetchErr != nil {
				failureReasons = append(failureReasons, fmt.Sprintf("fetch_failed:%s", res.URL))
			} else {
				res.SHA256 = hash
				budget.record(size)
			}
		}
		result = append(result, res)
	}

	for u, reason := range failures {
		failureReasons = append(failureReasons, fmt.Sprintf("loading_failed(%s):%s", reason, u))
	}

	// A request the page made but that never reached responseReceived or
	// loadingFailed before the window closed (e.g. a slow/hanging origin)
	// has no Resource entry above and would otherwise vanish from the
	// record with Coverage.Complete left true — silently promising a
	// resource offline playback does not actually have. Counting it as a
	// failure here is what keeps that promise honest.
	for _, u := range pendingURLs {
		failureReasons = append(failureReasons, fmt.Sprintf("unresolved_at_deadline:%s", u))
	}

	// A capture that hit the tracker ceiling is INCOMPLETE by definition:
	// resources the page asked for were never recorded, so replay would
	// serve an artwork with missing pieces. One bounded marker rather
	// than per-URL reasons — the URLs are exactly what was refused, so
	// naming them here would reintroduce the growth the bound prevents.
	if overflow > 0 {
		failureReasons = append(failureReasons,
			fmt.Sprintf("tracker_limit_exceeded:%d observation(s) dropped past %d resources/%d URL bytes",
				overflow, MaxCaptureResources, MaxCaptureResourceURLBytes))
	}

	coverage := Coverage{Complete: len(failureReasons) == 0}
	if len(failureReasons) > 0 {
		coverage.Reason = strings.Join(failureReasons, "; ")
	}
	return result, coverage
}

// fetchAndStoreBody fetches url's body and streams it to the store,
// bounded by capBytes (the caller's per-call reservation from a
// captureDiskBudget — see its doc for why this is no longer simply
// c.maxResourceBytes on every call). It returns the blob's actual
// on-disk size alongside its hash so the caller can charge the budget
// for the real bytes written, not the (possibly much larger) reserved
// cap.
func (c *capturer) fetchAndStoreBody(ctx context.Context, url, method string, capBytes int64) (string, int64, error) {
	req, err := c.fetchClient.NewRequest(method, url, nil)
	if err != nil {
		return "", 0, err
	}
	// Bounded per-transfer rather than by the finalization phase's
	// deadline (see captureFinalizeWindowDefault): a stalled origin is
	// cut off promptly while a large asset that keeps delivering bytes
	// runs to completion.
	transfer := beginResourceTransfer(ctx)
	defer transfer.Close()

	resp, err := c.fetchClient.Do(req.WithContext(transfer.Context()))
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < go_http.StatusOK || resp.StatusCode >= go_http.StatusMultipleChoices {
		return "", 0, fmt.Errorf("unexpected status %d fetching %s", resp.StatusCode, url)
	}
	// resp.Body is streamed straight into the store (hashed while it is
	// copied to disk, never buffered whole in memory first).
	hash, err := c.store.WriteBlob(transfer.Body(resp.Body), capBytes)
	if err != nil {
		return "", 0, err
	}
	size, err := c.store.BlobSize(hash)
	if err != nil {
		// The write itself already succeeded; a stat failure here is a
		// bookkeeping problem, not a fetch failure — fall back to
		// charging capBytes (the conservative, over-estimating
		// direction) against the budget rather than losing accounting
		// for this resource's contribution entirely.
		c.logger.Warn("offline cache capture: failed to stat written blob for disk-budget accounting",
			zap.String("url", url), zap.String("sha256", hash), zap.Error(err))
		return hash, capBytes, nil
	}
	return hash, size, nil
}

// attachHandlers wires the Network domain events capture needs. Handlers
// run on the CDPSession's read pump goroutine (see cdpsession.go's On
// doc), so they only mutate the tracker (mutex-guarded) and never block or
// call Send.
func (c *capturer) attachHandlers(session CDPSession, tracker *captureTracker) {
	session.On("Network.requestWillBeSent", func(params json.RawMessage) {
		var evt struct {
			RequestID string `json:"requestId"`
			Request   struct {
				URL    string `json:"url"`
				Method string `json:"method"`
			} `json:"request"`
			RedirectResponse *struct {
				URL     string            `json:"url"`
				Status  int               `json:"status"`
				Headers map[string]string `json:"headers"`
			} `json:"redirectResponse"`
		}
		if err := c.json.Unmarshal(params, &evt); err != nil {
			c.logger.Debug("offline cache capture: failed to parse requestWillBeSent", zap.Error(err))
			return
		}
		// CDP's redirectResponse carries the URL that redirected and its
		// status; the new URL for the same requestId is this event's
		// request.url — capturing that pairing here is the only way to
		// reconstruct a redirect chain, since CDP does not send a separate
		// "final" event naming the hop that was skipped. Its headers are
		// captured too (not just the final response's): a cors-mode
		// fetch() applies its CORS check to EVERY hop of a cross-origin
		// redirect chain, not only the final response, so replay must be
		// able to serve the same allowlisted headers back on the
		// redirect fulfill itself (see replay.go's onRequestPaused).
		//
		// The redirected-from hop's method must be read from the tracker
		// BEFORE trackRequest below overwrites it with this event's own
		// (post-redirect) method: redirectResponse describes the PREVIOUS
		// hop's response, tracked by the SAME requestId at the time of
		// its own earlier requestWillBeSent event, not this one's.
		if evt.RedirectResponse != nil {
			redirectMethod := tracker.methodForRequest(evt.RequestID)
			tracker.recordResource(evt.RedirectResponse.URL, evt.RedirectResponse.Status, "", evt.Request.URL, filterReplayableHeaders(evt.RedirectResponse.Headers), redirectMethod)
		}
		tracker.trackRequest(evt.RequestID, evt.Request.URL, evt.Request.Method)
	})

	session.On("Network.responseReceived", func(params json.RawMessage) {
		var evt struct {
			RequestID string `json:"requestId"`
			Response  struct {
				URL      string            `json:"url"`
				Status   int               `json:"status"`
				MimeType string            `json:"mimeType"`
				Headers  map[string]string `json:"headers"`
			} `json:"response"`
		}
		if err := c.json.Unmarshal(params, &evt); err != nil {
			c.logger.Debug("offline cache capture: failed to parse responseReceived", zap.Error(err))
			return
		}
		// The request's method was tracked by this same requestId's
		// requestWillBeSent event, which always precedes its terminal
		// responseReceived/loadingFailed.
		method := tracker.methodForRequest(evt.RequestID)
		tracker.recordResource(evt.Response.URL, evt.Response.Status, evt.Response.MimeType, "", filterReplayableHeaders(evt.Response.Headers), method)
		tracker.resolveRequest(evt.RequestID)
	})

	session.On("Network.loadingFailed", func(params json.RawMessage) {
		var evt struct {
			RequestID     string `json:"requestId"`
			ErrorText     string `json:"errorText"`
			BlockedReason string `json:"blockedReason,omitempty"`
		}
		if err := c.json.Unmarshal(params, &evt); err != nil {
			c.logger.Debug("offline cache capture: failed to parse loadingFailed", zap.Error(err))
			return
		}
		url := tracker.urlForRequest(evt.RequestID)
		reason := evt.ErrorText
		// blockedReason=="csp" is the CSP-broken-online case (e.g. a CDN
		// serving a restrictive Content-Security-Policy that blocks the
		// artwork's own script): the artwork fails to load even with a
		// live network connection, which status reporting should
		// distinguish from a capture defect.
		if evt.BlockedReason == "csp" {
			reason = ReasonCSPBlocked
		}
		tracker.recordFailure(url, reason)
		tracker.resolveRequest(evt.RequestID)
	})
}

// captureTracker accumulates network activity observed during one capture
// session. All access is mutex-guarded because events are delivered on the
// CDPSession's read-pump goroutine while resolveResources reads the final
// state from the caller's goroutine after the observation window closes.
// MaxCaptureResources bounds how many distinct resources one capture may
// track, and MaxCaptureResourceURLBytes bounds the length of each URL kept.
//
// Both exist because a capture's input is a PERMITTED artwork, and passing
// the source guard says only that the origin is public — not that the page
// is well behaved. Nothing else bounds this: the disk budget caps BYTES
// FETCHED, but the tracker also holds URLs for resources it never fetches
// (failures and still-pending requests), so a page issuing a stream of
// distinct long URLs grew daemon memory during the capture window and then
// grew ItemRecord.Resources and the on-disk coverage without limit.
//
// The limits are deliberately generous rather than tight: a rich software
// artwork legitimately pulls hundreds of resources, and signed CDN links
// legitimately run long, so these are an anti-abuse ceiling and must never
// be reached by real work. Hitting either is recorded rather than silent —
// see trackerOverflow — because a capture that quietly dropped resources
// would replay as an artwork with missing pieces and no explanation.
const (
	MaxCaptureResources        = 4096
	MaxCaptureResourceURLBytes = 4096
)

type captureTracker struct {
	mu sync.Mutex

	// overflow counts observations refused by the bounds above: distinct
	// resources past MaxCaptureResources, and URLs past
	// MaxCaptureResourceURLBytes. Counted rather than dropped silently so
	// Coverage can say the capture is incomplete AND why.
	overflow int

	// requestURL maps a CDP requestId to its current URL so
	// Network.loadingFailed (which only carries the id) can be attributed
	// to a URL recorded by an earlier requestWillBeSent.
	requestURL map[string]string
	// requestMethod mirrors requestURL, tracking each requestId's current
	// hop's HTTP method — needed to attribute the right method to a
	// redirectResponse (which describes the PREVIOUS hop) and to a
	// responseReceived/loadingFailed event (which only carries the id),
	// since neither event otherwise repeats the method.
	requestMethod map[string]string
	// resources is keyed by resourceKey(method, url), not url alone — see
	// Resource.Method's doc for why method is part of this identity.
	resources map[string]Resource
	order     []string // resourceKeys in first-seen order, for deterministic output
	failures  map[string]string
	// pending holds every requestId seen via Network.requestWillBeSent
	// that has not yet reached a terminal event (Network.responseReceived
	// or Network.loadingFailed). A request still pending when the capture
	// window closes was asked for by the page but its outcome was never
	// observed — resolveResources must count that as an incomplete
	// capture (see its doc), or a slow/hanging resource could silently
	// vanish from the record while Coverage.Complete still reports true.
	pending map[string]struct{}
}

func newCaptureTracker() *captureTracker {
	return &captureTracker{
		requestURL:    make(map[string]string),
		requestMethod: make(map[string]string),
		resources:     make(map[string]Resource),
		failures:      make(map[string]string),
		pending:       make(map[string]struct{}),
	}
}

// isIgnoredCaptureURL excludes page-internal URL schemes that are always
// regenerated fresh on every page load (a new random blob: UUID or an
// inline data: payload, never a stable network resource) and would
// otherwise be recorded as false misses if captured and looked up again
// on a later load.
func isIgnoredCaptureURL(url string) bool {
	return strings.HasPrefix(url, "blob:") || strings.HasPrefix(url, "data:")
}

func (t *captureTracker) recordResource(url string, status int, contentType, redirectTo string, headers map[string]string, method string) {
	if url == "" || isIgnoredCaptureURL(url) {
		return
	}
	key := resourceKey(method, url)
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(url) > MaxCaptureResourceURLBytes {
		t.overflow++
		return
	}
	if _, exists := t.resources[key]; !exists {
		// Bound checked only for a NEW key: an update to a resource
		// already tracked costs no additional entry, and refusing it
		// would leave a stale first-observation record in place.
		if len(t.resources) >= MaxCaptureResources {
			t.overflow++
			return
		}
		t.order = append(t.order, key)
	}
	// Method is stored as "" for GET (see Resource.Method's doc), never
	// the resourceKey's normalized/uppercased form, so the on-disk record
	// stays free of redundant text for the overwhelmingly common case.
	storedMethod := ""
	if normalized := strings.ToUpper(method); normalized != "" && normalized != go_http.MethodGet {
		storedMethod = normalized
	}
	t.resources[key] = Resource{URL: url, Status: status, ContentType: contentType, RedirectTo: redirectTo, Headers: headers, Method: storedMethod}
}

func (t *captureTracker) recordFailure(url, reason string) {
	if url == "" || isIgnoredCaptureURL(url) {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(url) > MaxCaptureResourceURLBytes {
		t.overflow++
		return
	}
	if _, exists := t.failures[url]; !exists && len(t.failures) >= MaxCaptureResources {
		t.overflow++
		return
	}
	t.failures[url] = reason
}

// trackRequest records a Network.requestWillBeSent observation and marks
// requestID pending. Called on every hop of a redirect chain (CDP reuses
// the same requestId across hops, only changing the URL), which is exactly
// why this must (re-)mark it pending rather than only doing so on first
// sight: a request is not resolved until its FINAL hop reaches a terminal
// event, so an earlier hop's resolveRequest call (from that hop's own
// redirect responseReceived) must not be mistaken for the whole chain being
// done. In practice each hop's requestWillBeSent fires strictly after the
// previous hop's responseReceived, so this re-marking is what keeps the
// invariant "pending means the CURRENT url has no terminal event yet" true
// throughout the chain.
func (t *captureTracker) trackRequest(requestID, url, method string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	// Same ceiling on the in-flight maps. These are keyed by requestId
	// rather than URL, so they are bounded by concurrent requests rather
	// than distinct ones in practice — but a page that never lets its
	// requests terminate keeps every entry alive, and the URL strings are
	// the part that carries weight.
	if len(url) > MaxCaptureResourceURLBytes {
		t.overflow++
		return
	}
	if _, exists := t.requestURL[requestID]; !exists && len(t.requestURL) >= MaxCaptureResources {
		t.overflow++
		return
	}
	t.requestURL[requestID] = url
	t.requestMethod[requestID] = method
	t.pending[requestID] = struct{}{}
}

func (t *captureTracker) urlForRequest(requestID string) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.requestURL[requestID]
}

// methodForRequest returns the method most recently tracked for
// requestID — see requestMethod's doc for why callers must read this
// before a same-event trackRequest call would overwrite it.
func (t *captureTracker) methodForRequest(requestID string) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.requestMethod[requestID]
}

// resolveRequest marks requestID as having reached a terminal event
// (Network.responseReceived or Network.loadingFailed), removing it from
// the "still in flight when the window closes" set snapshot exposes.
func (t *captureTracker) resolveRequest(requestID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.pending, requestID)
}

// snapshot returns copies of the tracker's state for lock-free use after
// the observation window closes. The first return value is resourceKeys
// (not plain URLs — see resources' doc) in first-seen order, for
// deterministic Resource ordering in the final record. pendingURLs is
// sorted for deterministic Coverage.Reason ordering (map iteration order
// is not); it excludes blob:/data: URLs for the same reason
// recordResource/recordFailure do — those schemes never reach a Network
// terminal event since they resolve in-page, so treating one as "still
// pending" would be a permanent false incompleteness on every capture
// rather than a real signal.
func (t *captureTracker) snapshot() ([]string, map[string]Resource, map[string]string, []string, int) {
	t.mu.Lock()
	defer t.mu.Unlock()

	resourceKeys := append([]string{}, t.order...)
	resources := make(map[string]Resource, len(t.resources))
	for k, v := range t.resources {
		resources[k] = v
	}
	failures := make(map[string]string, len(t.failures))
	for k, v := range t.failures {
		failures[k] = v
	}
	pendingURLs := make([]string, 0, len(t.pending))
	for reqID := range t.pending {
		if u := t.requestURL[reqID]; u != "" && !isIgnoredCaptureURL(u) {
			pendingURLs = append(pendingURLs, u)
		}
	}
	sort.Strings(pendingURLs)
	return resourceKeys, resources, failures, pendingURLs, t.overflow
}

// Capture-side source guarding. fetchClient covers the bytes THIS daemon
// pulls; it cannot cover the requests the page itself makes, because
// those never pass through a Go client. capture.go hands an untrusted
// artwork to Chromium via Page.navigate, and Chromium then follows
// redirects and loads subresources on its own — so without interception
// a page can simply fetch http://127.0.0.1:1111/api/cast and drive the
// unauthenticated hub, or read the kiosk's DevTools on :9222.
const (
	// captureGuardConcurrency bounds how many paused requests are being
	// decided at once. Each decision may perform a DNS lookup, and the
	// page is hostile-by-assumption, so this is what stops a page that
	// issues thousands of requests from spawning thousands of resolving
	// goroutines.
	captureGuardConcurrency = 16
	// captureGuardDecisionTimeout bounds one decision, so a resolver that
	// hangs cannot pin a slot for the whole capture window.
	captureGuardDecisionTimeout = 5 * time.Second
	// captureTargetSetupConcurrency bounds how many child targets are
	// being armed at once. Lower than the request bound because each
	// setup is several sequential CDP round trips rather than one
	// decision, and a page has no legitimate reason to open many targets
	// at once.
	captureTargetSetupConcurrency = 8
)

// attachSourceGuard arms Fetch interception on the capture session and
// answers every paused request with continue or fail.
//
// Handler discipline: CDPSession.On runs handlers on the read pump, and a
// handler must never Send inline (see that interface's doc) — the reply it
// waits for can only arrive on the pump it is blocking. Every decision is
// therefore handed to a goroutine, exactly as replay.go does for the kiosk.
//
// Saturation fails CLOSED: when every slot is busy the request is left
// unanswered rather than admitted, so a page cannot get an unchecked
// request through by flooding. The cost is that request stalling until the
// bounded capture window ends, which degrades one capture's fidelity — the
// right trade against admitting an unchecked request to loopback.
//
// KNOWN LIMITATION, deliberately not papered over: this is a URL-time
// check, so DNS rebinding is NOT closed — Chromium resolves the host
// itself after we continue the request, and may get a different answer
// than we did. Closing that requires taking Chromium's egress away
// entirely (a loopback filtering proxy it must dial through). CDP Fetch
// also does not intercept WebSocket handshakes, so ws:// egress is
// likewise uncovered. Both are recorded in
// docs/offline-artwork-capture.md §9.
func (c *capturer) attachSourceGuard(ctx context.Context, session CDPSession) *captureGuard {
	g := &captureGuard{
		c:           c,
		slots:       make(chan struct{}, captureGuardConcurrency),
		targetSlots: make(chan struct{}, captureTargetSetupConcurrency),
	}
	g.arm(ctx, session)
	return g
}

// captureGuard carries the state shared by the root session and every
// child target: one concurrency bound across ALL of them, so a page
// cannot multiply its decision budget by spawning iframes.
type captureGuard struct {
	c     *capturer
	slots chan struct{}
	// targetSlots bounds CHILD-TARGET SETUP, which is separate work from
	// deciding a paused request: each attach performs several CDP calls
	// (Fetch.enable, setAutoAttach, resume), and a hostile page can create
	// targets faster than those complete. Without its own bound, one
	// goroutine per attach event is unbounded — the request semaphore
	// above does not constrain it at all.
	targetSlots         chan struct{}
	saturatedOnce       sync.Once
	targetSaturatedOnce sync.Once
}

// arm registers the Fetch.requestPaused handler on one session. Called
// for the root page and again for every auto-attached child target.
func (g *captureGuard) arm(ctx context.Context, session CDPSession) {
	c := g.c
	slots := g.slots
	saturatedOnce := &g.saturatedOnce

	session.On("Fetch.requestPaused", func(params json.RawMessage) {
		var evt struct {
			RequestID string `json:"requestId"`
			Request   struct {
				URL string `json:"url"`
			} `json:"request"`
		}
		if err := c.json.Unmarshal(params, &evt); err != nil {
			// No requestId means no way to answer this pause at all.
			c.logger.Warn("offline cache capture: unparseable Fetch.requestPaused; request left blocked",
				zap.Error(err))
			return
		}
		select {
		case slots <- struct{}{}:
			go func() {
				defer func() { <-slots }()
				c.decidePausedRequest(ctx, session, evt.RequestID, evt.Request.URL)
			}()
		default:
			saturatedOnce.Do(func() {
				c.logger.Warn("offline cache capture: source-guard slots saturated; further requests blocked for this capture",
					zap.Int("concurrency", captureGuardConcurrency))
			})
		}
	})
}

// decidePausedRequest allows or blocks one paused request.
func (c *capturer) decidePausedRequest(ctx context.Context, session CDPSession, requestID, rawURL string) {
	decideCtx, cancel := context.WithTimeout(ctx, captureGuardDecisionTimeout)
	defer cancel()

	if err := c.pausedRequestAllowed(decideCtx, rawURL); err != nil {
		c.logger.Warn("offline cache capture: blocked page request",
			zap.String("url", truncateSourceForLog(rawURL)), zap.Error(err))
		// Answered on its OWN context, not decideCtx. The dominant reason
		// the check above fails is decideCtx expiring inside addrsFor (a
		// hanging resolver — the case its 5s bound exists for), and
		// sending the verdict on that same expired context would return
		// immediately without ever failing the request, leaking the pause
		// instead of closing it.
		verdictCtx, cancelVerdict := context.WithTimeout(ctx, captureGuardDecisionTimeout)
		defer cancelVerdict()
		if _, err := session.Send(verdictCtx, "Fetch.failRequest", map[string]interface{}{
			"requestId":   requestID,
			"errorReason": "AccessDenied",
		}); err != nil {
			c.logger.Warn("offline cache capture: Fetch.failRequest failed",
				zap.String("request_id", requestID), zap.Error(err))
		}
		return
	}
	if _, err := session.Send(decideCtx, "Fetch.continueRequest", map[string]interface{}{
		"requestId": requestID,
	}); err != nil {
		c.logger.Warn("offline cache capture: Fetch.continueRequest failed",
			zap.String("request_id", requestID), zap.Error(err))
	}
}

// pausedRequestAllowed applies the source policy to one page request.
//
// ALLOWLIST, not a denylist — the same rule sourceGuard.check states and
// for the same reason. An earlier version of this function continued
// every scheme that was not http(s), intending only to let data: and
// blob: through; that silently admitted file:, ftp:, chrome: and anything
// else Chromium might hand us, which is precisely the shape of mistake a
// denylist makes. data: and blob: are the ONLY non-dialing exceptions:
// they carry their own bytes and open no socket, so refusing them would
// break ordinary artwork for no security gain.
func (c *capturer) pausedRequestAllowed(ctx context.Context, rawURL string) error {
	u, err := go_url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("%w: unparseable request URL: %s", ErrUnsafeSource, truncateSourceForLog(err.Error()))
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
		// Checked against the address policy below.
	case "data", "blob", "about":
		// Non-dialing: these carry their own bytes or name no resource at
		// all, so they open no socket. "about" is also what the
		// post-capture teardown navigates to, and refusing it here would
		// block the very thing that stops the page.
		return nil
	default:
		return fmt.Errorf("%w: request scheme %q is not permitted", ErrUnsafeSource, truncateSourceForLog(u.Scheme))
	}
	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("%w: request URL has no host", ErrUnsafeSource)
	}
	_, err = c.guard.addrsFor(ctx, host)
	return err
}

// enableGuardedAutoAttach extends the source guard to CHILD CDP targets.
//
// Why this is required, not defense in depth: a cross-origin iframe
// (OOPIF) and a worker each run in their OWN CDP target, and their
// requests are never delivered to the root page session's
// Fetch.requestPaused handler. Guarding only the root therefore leaves a
// complete bypass — an artwork embeds a cross-origin iframe, and that
// iframe fetches http://127.0.0.1:1111/api/cast unguarded.
//
// Ordering contract, and the reason waitForDebuggerOnStart is set: a new
// child is PAUSED before it issues even its first request. On
// Target.attachedToTarget we arm the guard and Fetch.enable on it FIRST,
// then resume it. Without the pause, the child's opening request races
// our Fetch.enable and can reach the network before interception exists.
//
// Differs from replay's equivalent (kiosktargets.go) in two ways, both
// deliberate. No target-type filter: replay scopes to iframes because it
// has nothing cached for a worker, but a worker can dial, so a SECURITY
// boundary cannot skip it. And it recurses — setAutoAttach is reissued on
// each child — because an artwork can nest iframes, and a boundary that
// stops at depth one is a boundary with a documented way around it.
//
// A child whose interception could not be armed is NOT resumed. It stays
// frozen, which costs that subframe's content; resuming it would let it
// run entirely outside the guard, which is the failure this exists to
// prevent.
func (c *capturer) enableGuardedAutoAttach(ctx context.Context, root CDPSession, guard *captureGuard) error {
	c.armAutoAttachHandler(ctx, root, root, guard)
	return c.sendAutoAttach(ctx, root)
}

// armAutoAttachHandler registers the child-attach handler on session.
// root is kept separately because flat-mode sessions are all routed over
// the root connection, so ForSession is always called on it — including
// for grandchildren, whose events arrive on a child's session.
func (c *capturer) armAutoAttachHandler(ctx context.Context, root, session CDPSession, guard *captureGuard) {
	session.On("Target.attachedToTarget", func(params json.RawMessage) {
		// Off the read pump: this handler Sends (Fetch.enable, resume),
		// and CDPSession.On forbids that inline. Bounded, because a
		// hostile page can create targets faster than the several CDP
		// calls per attach complete.
		select {
		case guard.targetSlots <- struct{}{}:
			go func() {
				defer func() { <-guard.targetSlots }()
				c.handleCaptureTargetAttached(ctx, root, guard, params)
			}()
		default:
			// Fail closed: the child was created with
			// waitForDebuggerOnStart, so declining to set it up leaves it
			// PAUSED and it never runs. Dropping the event is therefore
			// safe in the direction that matters — the opposite of
			// admitting an unguarded target.
			guard.targetSaturatedOnce.Do(func() {
				c.logger.Warn("offline cache capture: child-target setup saturated; further targets left paused for this capture",
					zap.Int("concurrency", captureTargetSetupConcurrency))
			})
		}
	})
}

func (c *capturer) sendAutoAttach(ctx context.Context, session CDPSession) error {
	if _, err := session.Send(ctx, "Target.setAutoAttach", map[string]interface{}{
		"autoAttach":             true,
		"waitForDebuggerOnStart": true,
		"flatten":                true,
		// No "filter": every child type must be covered, workers included.
	}); err != nil {
		return fmt.Errorf("Target.setAutoAttach: %w", err)
	}
	return nil
}

// handleCaptureTargetAttached arms the guard on a newly attached child,
// extends auto-attach into it, and only then resumes it.
func (c *capturer) handleCaptureTargetAttached(ctx context.Context, root CDPSession, guard *captureGuard, params json.RawMessage) {
	var evt targetAttachedEvent
	if err := c.json.Unmarshal(params, &evt); err != nil {
		c.logger.Warn("offline cache capture: failed to parse Target.attachedToTarget", zap.Error(err))
		return
	}
	if evt.SessionID == "" {
		// Without a sessionId there is nothing to route commands to, so
		// the child can be neither guarded nor resumed.
		c.logger.Warn("offline cache capture: Target.attachedToTarget without sessionId; child left paused",
			zap.String("type", evt.TargetInfo.Type))
		return
	}

	// Flat mode routes every session over the root connection, so the
	// view is always taken from root — this is what makes the recursion
	// below work for grandchildren too.
	child := root.ForSession(evt.SessionID)
	guard.arm(ctx, child)
	if _, err := child.Send(ctx, "Fetch.enable", fetchEnablePatternAll()); err != nil {
		// Deliberately NOT resumed: a child running without interception
		// is exactly the bypass this closes.
		//
		// This is the NORMAL path for workers, not an error path. Chromium
		// does not implement the Fetch domain on worker targets at all —
		// measured on the FF1, a type:worker session answers
		// "cdp error -32601: 'Fetch.enable' wasn't found". So a worker is
		// contained by staying paused forever rather than by having its
		// requests intercepted, which is safe but has a real functional
		// cost worth knowing before anyone "fixes" this: artwork whose
		// rendering depends on a worker is captured with that worker
		// never running. Resuming it anyway to recover that rendering
		// would hand every artwork worker an unguarded network — the
		// exact trade this fail-closed branch refuses.
		// TestCapturer_RealBrowser_LoopbackRequestsNeverLeaveTheBrowser
		// pins it against a real browser.
		c.logger.Warn("offline cache capture: Fetch.enable on child target failed; leaving it paused",
			zap.String("session_id", evt.SessionID),
			zap.String("type", evt.TargetInfo.Type), zap.Error(err))
		return
	}

	// Recurse before resuming, so a nested target created by this child's
	// first paint is already covered.
	//
	// A failure here is NOT survivable, for the same reason a failed
	// Fetch.enable above is not: without auto-attach on this child, the
	// targets IT creates are neither paused nor intercepted, so resuming
	// it would reopen the loopback bypass one level down. An earlier
	// version logged this and resumed anyway — fail-open, and inconsistent
	// with the Fetch.enable path immediately above it.
	c.armAutoAttachHandler(ctx, root, child, guard)
	if err := c.sendAutoAttach(ctx, child); err != nil {
		c.logger.Warn("offline cache capture: could not extend auto-attach into child target; leaving it paused",
			zap.String("session_id", evt.SessionID),
			zap.String("type", evt.TargetInfo.Type), zap.Error(err))
		return
	}

	if _, err := child.Send(ctx, "Runtime.runIfWaitingForDebugger", map[string]interface{}{}); err != nil {
		c.logger.Debug("offline cache capture: resuming child target failed",
			zap.String("session_id", evt.SessionID), zap.Error(err))
	}
}

// captureTeardownWindow bounds the post-capture navigation that stops the
// page. Short: it is one CDP round trip, and a browser too wedged to
// answer it is about to be torn down by the idle teardown anyway.
const captureTeardownWindow = 5 * time.Second

// stopPageBeforeDetach navigates the capture target to about:blank while
// interception is STILL armed, which is the only moment it can be done
// safely.
//
// Why it is required: Capture's session close removes Fetch interception,
// but nothing removes the page. downloader.Release schedules an idle
// teardown of the browser (30s by default), so between those two events
// the untrusted artwork runs with no guard at all — its timers still
// fire, its pending requests still complete, and any target left paused
// by a saturated guard is released when the Fetch domain goes away with
// the client. Navigating away discards all three in one step.
//
// Uses its own context rather than the capture's: the page must be
// stopped whether capture succeeded, timed out, or was canceled, and the
// capture context is expired in exactly the cases that leave the most
// dangerous page running.
//
// about:blank commits without a network load, so this navigation is never
// itself paused by our Fetch handler. If anyone widens this teardown to a
// real URL, that stops being true — and the decision would then run on
// decideCtx, derived from the possibly-expired capture context, which is
// the asymmetry this function exists to avoid for its own Send.
func (c *capturer) stopPageBeforeDetach(session CDPSession) {
	ctx, cancel := context.WithTimeout(context.Background(), captureTeardownWindow)
	defer cancel()
	if _, err := session.Send(ctx, "Page.navigate", map[string]interface{}{"url": "about:blank"}); err != nil {
		// Not fatal to the capture, whose bytes are already saved — but
		// it does mean an untrusted page may keep running until the idle
		// teardown, so it is a Warn rather than a Debug.
		c.logger.Warn("offline cache capture: could not stop the captured page before detaching; it may keep running until idle teardown",
			zap.Error(err))
	}
}
