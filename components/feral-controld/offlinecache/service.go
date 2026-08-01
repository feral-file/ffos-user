package offlinecache

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	dp1playlist "github.com/display-protocol/dp1-go/playlist"
	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

// ErrUnsupportedMediaClass is returned by DownloadItem when the item's
// source classifies as ClassStreaming (see classify.go): a live/VOD HLS
// or DASH manifest has no fixed byte-for-byte content a one-shot
// download or a static blob-store replay could faithfully serve, so it
// is the one class offline caching rejects outright. Every other class
// (software, media, or unknown-but-still-single-file) is downloadable —
// see captureForClass's doc for how each is routed.
var ErrUnsupportedMediaClass = errors.New("offline cache: item is a live/streaming source and cannot be cached offline")

// ErrServiceNotStarted is returned by DownloadItem/DownloadPlaylist when
// the worker goroutine that actually processes queued jobs is not
// running — either Start was never called, or Start's own setup (index
// rebuild from disk) failed and returned an error. main.go treats a
// Start failure as best-effort (it logs and continues rather than
// crashing controld — see main.go's offline-cache section), so without
// this guard DownloadItem/DownloadPlaylist would enqueue jobs onto a
// queue nothing will ever drain and report false success to the mobile
// app.
var ErrServiceNotStarted = errors.New("offline cache: service is not started")

// ErrItemBusy is returned by ClearItem/ClearPlaylist when the item is the
// one job currently inside capturer.Capture (state == StateDownloading —
// see notify's doc). Earlier revisions let a clear proceed unconditionally
// in this case: it would delete the on-disk record and dequeue nothing
// (there was nothing left to dequeue, since the job is no longer in
// s.queue), but the in-flight Capture would still save a fresh record
// once it finished, making the just-cleared item "legitimately reappear"
// moments later — a caller that received ok:true from clear has no
// way to know the item silently came back. Rejecting the clear instead
// (retryable — see offlineCacheErrorResponse's "busy" mapping) is
// simpler and safer than canceling the in-flight capture, which would
// need per-job cancellation plumbed through the single-worker queue.
var ErrItemBusy = errors.New("offline cache: item is currently downloading and cannot be cleared until it finishes")

// ErrQueueFull is returned by DownloadItem/DownloadPlaylist when the
// single-worker capture queue (jobQueue) is already at maxQueueLen.
// jobQueue is deliberately an unbounded-length FIFO in the sense that
// push itself never blocks a caller waiting for room (see its doc), but
// "never blocks" is not the same as "never bounded": distinct playlists
// downloaded in a burst all enqueue independently of the storm gate,
// which only throttles concurrently in-FLIGHT admin commands, not the
// backlog those commands can leave behind for the one serial capture
// worker to work through — without a cap here, that backlog (each
// entry holding a full dp1playlist.PlaylistItem) could grow without
// bound in memory and accumulate arbitrarily stale queued work with no
// way for a caller to know the size of the pile it just added to.
// Retryable — see offlineCacheErrorResponse's "busy"-shaped mapping for
// the same reasoning applied here.
var ErrQueueFull = errors.New("offline cache: download queue is full, try again later")

// dp1MaxPlaylistItems is the DP-1 spec's cap on how many items a single
// playlist may contain, and therefore the largest burst any ONE command
// can produce: downloadPlaylist enqueues up to this many jobs, and
// clearPlaylistCache settles up to this many items, in one shot.
//
// Three separate bounds in this package are derived from it rather than
// each spelling out 1024: defaultMaxQueueLen at 4x (a backlog safety
// valve, deliberately sized for several bursts), MaxStatusSources and
// notifyQueueCapacity at 1x (each absorbing exactly one burst). All three
// exist because of a per-playlist burst, so a future spec change must move
// them together; naming the shared origin is what makes that lockstep
// visible instead of leaving it to prose.
const dp1MaxPlaylistItems = 1024

// defaultMaxQueueLen bounds jobQueue's length (see ErrQueueFull's doc).
// Not currently exposed via config: unlike maxDiskBytes (a real
// per-device hardware constraint operators need to tune), this is a
// pure software safety valve against unbounded backlog growth, not a
// limit callers are meant to ever actually hit in normal use.
//
// One DownloadPlaylist call — in the worst case where every item
// classifies as cacheable (anything but ClassStreaming, see
// ErrUnsupportedMediaClass's doc) — can enqueue a whole playlist's worth
// of jobs in one shot. Sized at 4x that so a full-size playlist with every
// item queued, plus a reasonable burst of other DownloadItem/
// DownloadPlaylist calls queued behind it before the single serial worker
// drains them, is never rejected with ErrQueueFull under realistic use.
// Each queued captureJob only holds item metadata
// (dp1playlist.PlaylistItem), never resource bytes, so even 4096 entries
// is a small, bounded amount of memory — this cap exists purely to stop a
// truly pathological backlog (e.g. a caller bug that re-enqueues in a
// loop) from growing without bound, not to meaningfully constrain
// legitimate traffic.
const defaultMaxQueueLen = 4 * dp1MaxPlaylistItems

// ErrClearedDuringDownload is returned by DownloadItem when a
// ClearItem/ClearPlaylist for the same item landed between the epoch
// sample at the top of the call and the commit inside enqueue (see
// downloadEpoch's doc). The clear wins by design — resurrecting a record
// a clear already reported success for would be the worse outcome — but
// that means NOTHING was queued, and the caller must not be told
// otherwise.
//
// This exists because the alternative was worse than a missing error: the
// commandrouter answers downloadPlaylistItem with a flat
// status:"queued", so swallowing this case reported scheduled work that
// no worker would ever run, and left the mobile app's progress UI
// waiting on an item that could never progress. ErrServiceNotStarted's
// doc names that same "report false success to the mobile app" failure
// as the thing it exists to prevent; this is the same hazard reached by
// a different race.
//
// Retryable: re-issuing the download after the clear has landed queues
// normally — see offlineCacheErrorResponse's "busy" mapping, which this
// joins for exactly that reason.
var ErrClearedDuringDownload = errors.New("offline cache: a clear for this item landed while the download was being queued, so nothing was queued")

// enqueueOutcome distinguishes the three ways enqueue can return without
// an error. It replaces a bool, which conflated two of them: "already
// queued by an earlier call" and "a clear won the race" both meant
// queued=false, yet only the second means the item is NOT going to be
// captured. A caller branching on the bool therefore either reported
// success for work that was never scheduled (DownloadItem) or had to
// know, undocumented, that false could mean either.
type enqueueOutcome int

const (
	// enqueueQueued: this call committed a new job to the queue.
	enqueueQueued enqueueOutcome = iota
	// enqueueAlreadyQueued: an identical job was already queued or
	// downloading, so this call did nothing — but the item genuinely IS
	// scheduled, so a single-item caller should still report success.
	// Only an aggregate count (DownloadPlaylist's queuedCount) needs to
	// exclude it, since nothing new was scheduled.
	enqueueAlreadyQueued
	// enqueueClearWon: a clear landed since the caller sampled its
	// epoch. Nothing was queued and nothing will be — see
	// ErrClearedDuringDownload.
	enqueueClearWon
)

// ItemStatus is one entry of a Status snapshot, shaped for the
// getOfflineCacheStatus command and offline_cache_status notification.
type ItemStatus struct {
	// Source is the item's source URL — the cache identity (see
	// SourceKey) and the field clients match entries against their own
	// playlist items by. The DP-1 item id is deliberately absent from the
	// wire: the DP-1 core schema makes it optional and defines it as a
	// random UUID v4, so nothing durable can key on it (see SourceKey's
	// doc for why observed id churn is the symptom, not the argument).
	Source string    `json:"source"`
	State  ItemState `json:"state"`
	// Percent is coarse (0 or 100): capture is a single bounded-window
	// operation, not chunked, so there is no meaningful mid-download
	// progress to report beyond "queued/downloading" vs "done".
	Percent int   `json:"percent"`
	Bytes   int64 `json:"bytes,omitempty"`
	// CoverageComplete carries omitempty, so a false value is ABSENT from
	// the wire rather than sent as false — clients must read a missing
	// field as false.
	CoverageComplete bool `json:"coverageComplete,omitempty"`
	// Reason is truncated to maxReasonBytes by every constructor of this
	// type (itemStatus, notifyObserver); the untruncated text stays on
	// disk in the ItemRecord. See truncateReason.
	Reason string `json:"reason,omitempty"`
}

// reasonSeparator is what capture.go joins its per-resource failure
// entries with when building Coverage.Reason, and therefore the boundary
// truncateReason cuts on.
const reasonSeparator = "; "

// maxReasonBytes bounds ItemStatus.Reason on the wire.
//
// Coverage.Reason is one free-text entry per failed/unresolved resource,
// each embedding that resource's full URL (capture.go), joined with
// reasonSeparator. Nothing bounds how many resources one artwork loads,
// so a capture that ran with no network — or whose window closed with
// hundreds of requests still pending — can produce a reason of tens of
// KB for a SINGLE item. Multiplied across a page of items, that is the
// dominant term in the response body, which matters because the LAN hub
// buffers the whole body in memory before writing it under a 30s write
// deadline (hub.respondJSON), on a device already carrying OOM pressure
// from the kiosk Chromium.
//
// Truncating here rather than at capture time keeps the on-disk record
// complete for debugging. The wire contract already tells clients the
// reason list is neither exhaustive nor stable in wording, and that only
// coverageComplete is a stable boolean — see
// docs/controld-inbound-controller-messages.md's getOfflineCacheStatus
// section.
const maxReasonBytes = 512

// MaxStatusItems bounds how many ItemStatus entries a single Status call
// can return, and is also the default when StatusRequest.Limit is unset.
// The item count in the store is bounded only by BYTES (maxDiskBytes,
// via enforceDiskLimit) — there is no count cap — so a store full of
// small captures can hold tens of thousands of records, and an
// unpaginated snapshot of them all was previously bounded by nothing at
// all. Sized to be larger than any realistic single-device cache so
// existing clients that never send a cursor keep seeing their whole
// store in one call, while a pathological store degrades into paging
// (nextCursor) rather than into a multi-hundred-MB response.
const MaxStatusItems = 1000

// MaxStatusSources bounds StatusRequest.Sources. No legitimate caller
// needs to ask about more identifiers than a single playlist can hold,
// while an unbounded list would let a single 4 MiB hub body
// (hub.MAX_REQUEST_BODY_BYTES) drive ~100k store reads.
const MaxStatusSources = dp1MaxPlaylistItems

// StatusRequest is the input to Service.Status.
//
// The zero value means "first page of every item this process knows
// about", which is the shape every caller used before paging existed.
type StatusRequest struct {
	// Sources restricts the report to the items with these source URLs.
	// Empty means every item on disk plus everything in flight.
	// Duplicates and empty strings are dropped; the result is always
	// ordered by source KEY (SourceKey — fixed-length, stable), not by
	// the order given here and not lexicographically by the URLs
	// themselves.
	Sources []string
	// Limit caps how many items come back. <=0 or >MaxStatusItems is
	// clamped to MaxStatusItems.
	Limit int
	// Cursor is an opaque paging token: the source KEY of the previous
	// page's last entry (StatusSnapshot.NextCursor), NOT a source URL.
	// This page starts at the first key strictly greater than it. Empty
	// means the first page. Because keys are sorted, a cursor stays
	// valid even if the item it names is evicted between pages.
	Cursor string
	// TotalsOnly returns totals and disk usage with an empty items list,
	// for a caller that only renders a summary. It skips the per-item
	// blob stats recordBytes would otherwise do (the largest syscall
	// cost of a full snapshot) and the whole response body, but NOT the
	// per-item record reads totals are derived from. Implies the first
	// page: Cursor is ignored.
	TotalsOnly bool
}

// StatusTotals summarizes a Status snapshot for quick mobile-app display
// without the caller needing to walk Items itself.
type StatusTotals struct {
	Total       int `json:"total"`
	Ready       int `json:"ready"`
	Downloading int `json:"downloading"`
	Failed      int `json:"failed"`
}

// StatusSnapshot is the response shape for getOfflineCacheStatus.
type StatusSnapshot struct {
	// Items is at most StatusRequest.Limit entries, ordered by source
	// KEY (see StatusRequest.Sources). Never nil, so the wire always
	// carries [] rather than null.
	Items []ItemStatus `json:"items"`
	// Totals and DiskUsedBytes describe the WHOLE requested set, not
	// this page, and are computed on the first page only (empty
	// Cursor) — deriving them costs one store read per item in the set,
	// so recomputing them for every page would turn a full paged walk
	// from O(total) into O(pages x total). They are always both set or
	// both nil; a continuation page leaves them nil and the client
	// carries forward what the first page reported.
	Totals *StatusTotals `json:"totals,omitempty"`
	// DiskUsedBytes is named diskUsed on the wire to match the plan's
	// documented command response shape (section 5).
	DiskUsedBytes *int64 `json:"diskUsed,omitempty"`
	// NextCursor is the source KEY (an opaque token on the wire) to pass
	// back as StatusRequest.Cursor to fetch the next page. Empty means
	// this was the last page.
	NextCursor string `json:"nextCursor,omitempty"`
	// Truncated mirrors "NextCursor is set" as an explicit flag, so a
	// client that ignores paging can still tell it is not looking at the
	// whole set.
	Truncated bool `json:"truncated,omitempty"`
}

// truncateReason bounds reason to roughly maxReasonBytes (plus a short
// marker) while keeping whole entries intact, so a client still sees
// well-formed "<kind>:<url>" tokens it can match on the fixed prefix
// before the first ':'/'(' as documented. The dropped-entry count is
// reported rather than silently swallowed — a truncated list must not
// read as a complete one.
func truncateReason(reason string) string {
	if len(reason) <= maxReasonBytes {
		return reason
	}

	entries := strings.Split(reason, reasonSeparator)
	kept, used := 0, 0
	for _, entry := range entries {
		width := len(entry)
		if kept > 0 {
			width += len(reasonSeparator)
		}
		if used+width > maxReasonBytes {
			break
		}
		used += width
		kept++
	}

	if kept == 0 {
		// The first entry alone is over budget (a pathologically long
		// URL). Cut it on a rune boundary so the JSON body can never
		// carry a partial multi-byte sequence.
		head := truncateUTF8(entries[0], maxReasonBytes) + "…"
		if len(entries) == 1 {
			return head
		}
		return head + reasonSeparator + droppedReasonMarker(len(entries)-1)
	}
	// kept < len(entries) here: every entry fitting would mean the whole
	// reason fit, which the length check above already ruled out.
	return strings.Join(entries[:kept], reasonSeparator) + reasonSeparator + droppedReasonMarker(len(entries)-kept)
}

func droppedReasonMarker(dropped int) string {
	return fmt.Sprintf("…(+%d more)", dropped)
}

// truncateUTF8 cuts s to at most maxBytes bytes without splitting a rune.
// Only called when len(s) > maxBytes, so the index it walks back from is
// always inside s.
func truncateUTF8(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	cut := maxBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// ProgressObserver is notified on every item state transition. Keeping this
// as a small consumer-owned interface (rather than importing relayer/ws
// directly) lets the notification-wiring layer push offline_cache_status
// over relayer + hub WS without offlinecache depending on those transport
// packages — see AGENTS.md's "communicate through visible boundaries"
// principle.
//
// Implementations MUST NOT block: OnItemStateChanged is called from the
// single capture worker (stalling it stalls the whole download queue), from
// command admission itself (enqueue emits "queued" per item), and — since
// clears must be ordered against a racing enqueue's notification — from
// inside ClearItem/ClearPlaylist, which have no ctx to escape a wedged
// observer through (see clearReservation.awaitQueuedNotifications). The
// production implementation (notifier.go) satisfies this by enqueuing onto
// a bounded queue drained by its own worker; anything doing transport I/O
// inline would put a network round trip on all three of those paths.
//
//go:generate mockgen -source=service.go -destination=../mocks/offlinecache_service.go -package=mocks -mock_names=Service=MockOfflineCacheService,ProgressObserver=MockOfflineCacheProgressObserver
type ProgressObserver interface {
	OnItemStateChanged(status ItemStatus)
}

// Service is the public API used by commandrouter: download one item or a
// whole playlist (every class but ClassStreaming, see
// ErrUnsupportedMediaClass's doc), clear caches, and report status. It
// owns a job queue that serializes captures to one at a time — the
// device already carries OOM pressure from the kiosk Chromium, and
// downloader.go itself only runs one headless Chromium job at a time, so
// queueing here keeps that invariant visible at the API boundary too.
type Service interface {
	// Start rebuilds the in-memory state index from whatever is already
	// on disk (Store keeps no persisted manifest — see store.go) and
	// launches the background worker goroutine. Call once before any
	// other method; ctx bounds the worker's lifetime.
	Start(ctx context.Context) error
	// Stop cancels in-flight work, blocks until the worker goroutine has
	// exited (so callers can rely on no further ProgressObserver
	// callbacks firing after Stop returns), and then closes the
	// Capturer — which tears down the headless Chromium process
	// Downloader owns. This is the daemon's one shutdown hook for that
	// second Chromium: main.go has no direct handle to the downloader
	// (it is private to this package's Bootstrap wiring), so closing it
	// here is what fulfills Downloader.Close's "called once, on daemon
	// shutdown" contract without leaning on systemd's cgroup kill as the
	// only cleanup path.
	Stop()

	// DownloadItem, like DownloadPlaylist below, returns ErrServiceNotStarted
	// if Start was never called or failed, rather than queueing a job
	// nothing will ever process.
	DownloadItem(ctx context.Context, item dp1playlist.PlaylistItem) error
	// DownloadPlaylist stores playlistRaw exactly as given by the caller
	// (no further marshaling/unmarshaling happens here) and queues every
	// item it contains that does not classify as ClassStreaming (see
	// ErrUnsupportedMediaClass's doc). total counts all items in the
	// playlist; queued counts only those actually enqueued.
	//
	// An item whose Classifier.Classify call itself errors (network
	// blip, unreachable host, etc.) is logged and skipped exactly like a
	// genuinely streaming item, with one exception: if EVERY eligible
	// item hit a classify error (queued==0 but at least one classify
	// call failed), that is materially different from "this playlist
	// really has no cacheable items" and is reported as an error instead
	// of a false ok:true/queuedCount:0 — a transient classifier outage
	// must not look identical to an all-streaming playlist to the
	// controller. A classify failure alongside at least one successful
	// queue is still reported as success (queued reflects only the
	// items that actually got scheduled); that partial case is logged
	// but not surfaced as an error here, since the caller already got
	// real, correct work queued.
	//
	// sourceURL, if non-empty, is recorded so a later displayPlaylist
	// command for the same URL can still find and replay this exact
	// playlist from cache when live DP-1 resolution fails (e.g. no
	// network) — see CachedPlaylistForURL and commandrouter's
	// displayPlaylist branch. Pass "" for an inline dp1_call download,
	// which has no source URL to index by and is simply not
	// URL-recoverable later (only ID-recoverable, same as before).
	//
	// Callers should be aware that playlistRaw is not guaranteed to be
	// byte-identical to whatever a publisher originally served: the
	// commandrouter caller resolves the playlist through dp1.DP1 first
	// (to validate it and materialize any dynamicQuery items), which
	// necessarily re-serializes the Go struct rather than passing through
	// the original wire bytes. Field values (including `source` and any
	// signature fields) survive that round-trip unchanged; only byte-level
	// details like key order and whitespace do not. DP-1 signatures verify
	// against a JCS-canonicalized form (dp1-go's sign package), not raw
	// bytes, so this does not affect signature validity — see
	// docs/offline-artwork-capture.md.
	DownloadPlaylist(ctx context.Context, playlistRaw json.RawMessage, sourceURL string) (queued, total int, err error)
	// ClearItem removes the item with this source URL from the cache: its
	// record, any blob it was the last referent of, and any
	// not-yet-started download queued for it. Because cached records are
	// per-source, this clears the artifact for EVERY playlist item (in
	// any playlist) sharing the source. ErrItemNotFound means the device
	// had NOTHING for the source — not cached, not queued, not otherwise
	// tracked; commandrouter maps it to a non-retryable not_found, so
	// canceling a queued download must not (and does not) report it.
	// ErrItemBusy means this source is the one item currently mid-capture
	// and the caller should retry. See the implementation's doc for the
	// full contract.
	ClearItem(source string) error
	// ClearPlaylist removes a cached playlist's record and every one of its
	// member items, all-or-nothing: if any member is mid-capture the whole
	// call is rejected with ErrItemBusy before anything is deleted.
	// ErrPlaylistNotFound means the playlist itself is not cached.
	ClearPlaylist(playlistID string) error
	// Status reports on req.Sources, or on every item this process knows
	// about (on-disk + in-flight) when req.Sources is empty. The result
	// is one bounded page (see MaxStatusItems and StatusSnapshot's doc);
	// the zero StatusRequest asks for the first page of everything.
	Status(req StatusRequest) (StatusSnapshot, error)
	// CachedPlaylistForURL returns the raw playlist body last downloaded
	// via DownloadPlaylist(ctx, raw, sourceURL) OR indexed via
	// IndexPlaylistForOfflineDisplay below, for commandrouter's
	// displayPlaylist-by-URL fallback when live DP-1 resolution fails
	// (e.g. the device has no network right now). Returns
	// ErrPlaylistNotFound if sourceURL was never downloaded/indexed, or
	// was but has since been cleared — see Store.LoadPlaylistIDForURL's
	// doc for why a stale index entry still fails closed correctly here.
	CachedPlaylistForURL(sourceURL string) (json.RawMessage, error)
	// IndexPlaylistForOfflineDisplay persists playlistRaw and, when
	// sourceURL is non-empty, indexes it by that URL for
	// CachedPlaylistForURL — exactly what DownloadPlaylist already does
	// as a side effect of queuing every eligible item, but WITHOUT
	// classifying or queuing anything itself.
	//
	// This exists for downloadPlaylistItem: that command resolves a
	// whole playlist (possibly by playlistUrl) but only ever queues the
	// ONE requested item, so without this, downloading a single item
	// from a playlistUrl would leave that URL with no offline
	// displayPlaylist-by-URL fallback at all — even though the item
	// itself is now genuinely cached and replayable, the PLAYLIST BODY
	// (the item list/order/metadata the player needs to even know to
	// try displaying it) would never have been persisted. See
	// commandrouter.handleDownloadPlaylistItem's call site.
	//
	// Returns ErrServiceNotStarted, a parse error, or a store save
	// error under the same conditions DownloadPlaylist's own leading
	// validation does; sourceURL indexing failures are logged and
	// swallowed exactly like DownloadPlaylist's own best-effort index
	// write, since the playlist body itself is what CachedPlaylistForURL
	// needs most and is already durably saved by the time indexing runs.
	//
	// sampledEpoch must be the value CurrentPlaylistClearGeneration
	// returned for the resolved playlist's ID BEFORE the caller's
	// resolve+download sequence began. If a ClearPlaylist for that
	// playlist lands during that sequence, this write is skipped rather
	// than resurrecting the cleared record — see
	// CurrentPlaylistClearGeneration and playlistRecordMu's docs.
	IndexPlaylistForOfflineDisplay(playlistRaw json.RawMessage, sourceURL string, sampledEpoch uint64) error
	// CurrentPlaylistClearGeneration returns an opaque, monotonically
	// increasing counter for playlistID that advances every time that
	// playlist is cleared (ClearPlaylist). A caller that will resolve and
	// download a playlist and then index it for offline display
	// (handleDownloadPlaylistItem) samples this BEFORE that sequence and
	// passes it back to IndexPlaylistForOfflineDisplay, so a ClearPlaylist
	// racing the sequence is honored (the index write is skipped) instead
	// of silently resurrecting the just-cleared playlist fallback record.
	// The value is meaningful only for equality against a later sample of
	// the SAME playlist ID; its absolute magnitude carries no meaning.
	CurrentPlaylistClearGeneration(playlistID string) uint64
}

type captureJob struct {
	// sourceKey is SourceKey(item.Source) — the queue/state identity,
	// computed once at enqueue. item carries the raw source alongside for
	// notifications and logs.
	sourceKey string
	item      dp1playlist.PlaylistItem
	// class is the MediaClass sampled at classify time (DownloadItem/
	// DownloadPlaylist) and carried through the queue so process() can
	// route to the right capture pipeline without re-classifying (a
	// second network round trip) or risking a different answer the
	// second time around — see captureForClass's doc.
	class MediaClass
	// queuedNotified is closed by enqueue once this job's
	// state:"queued" notification has actually been emitted, and
	// process() waits on it before emitting state:"downloading".
	//
	// It exists because those two notifications are produced by
	// DIFFERENT goroutines — queued by whichever caller enqueued,
	// downloading by the single capture worker — and enqueue can only
	// emit its notification AFTER committing the job to the queue (see
	// the notify call there for why emitting before the commit produced
	// notifications for work that was never queued). The moment the
	// commit happens, an idle worker can pop this job and reach its own
	// notification, so without this barrier a client could observe
	// downloading before queued: progress running backwards for an item
	// that is in fact progressing normally.
	//
	// nil is tolerated (tests that push jobs directly): the barrier is
	// simply skipped.
	queuedNotified chan struct{}
}

// jobQueue is a FIFO backed by an in-memory slice so DownloadItem/
// DownloadPlaylist never block the caller waiting for queue capacity,
// even for a large playlist — push() only enforces defaultMaxQueueLen
// as a backlog safety valve (see ErrQueueFull's doc), never blocks. wake
// is a 1-buffered signal channel, not a data channel, so pop() always
// reads from items under the mutex instead of racing two sources of
// truth.
type jobQueue struct {
	mu    sync.Mutex
	items []captureJob
	wake  chan struct{}
}

func newJobQueue() *jobQueue {
	return &jobQueue{wake: make(chan struct{}, 1)}
}

func (q *jobQueue) push(j captureJob) {
	q.mu.Lock()
	q.items = append(q.items, j)
	q.mu.Unlock()
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

func (q *jobQueue) pop() (captureJob, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.items) == 0 {
		return captureJob{}, false
	}
	j := q.items[0]
	q.items = q.items[1:]
	return j, true
}

// peek returns the head job without popping it — dequeueAdmitted's
// look-before-pop, so an admission denial leaves the job in the queue
// (still StateQueued, still clearable). Only meaningful while the caller
// holds s.mu: without it a reserveForClear could remove the peeked job
// before any decision about it lands.
func (q *jobQueue) peek() (captureJob, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.items) == 0 {
		return captureJob{}, false
	}
	return q.items[0], true
}

// len reports how many jobs are currently queued (not yet popped) —
// used by enqueue's capacity check (see ErrQueueFull's doc).
func (q *jobQueue) len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.items)
}

// removeItems drops any not-yet-started job whose sourceKey is in keys and
// returns the jobs it dropped. Used by ClearItem/ClearPlaylist so a clear
// that races an item still sitting in the queue (as opposed to actively
// capturing — see service.captureMu) does not get silently undone once
// that queued job eventually runs and calls SaveItem again. The returned
// jobs carry the queuedNotified barriers the clear must wait on before
// announcing not_cached — see clearReservation.awaitQueuedNotifications.
func (q *jobQueue) removeItems(keys map[string]bool) []captureJob {
	q.mu.Lock()
	defer q.mu.Unlock()
	var removed []captureJob
	kept := q.items[:0]
	for _, j := range q.items {
		if keys[j.sourceKey] {
			removed = append(removed, j)
			continue
		}
		kept = append(kept, j)
	}
	q.items = kept
	return removed
}

type service struct {
	store         Store
	classifier    Classifier
	capturer      Capturer
	mediaCapturer MediaCapturer
	json          wrapper.JSON
	logger        *zap.Logger

	captureWindowMs int
	maxDiskBytes    int64 // <=0 means unlimited
	maxQueueLen     int   // see ErrQueueFull's doc
	observer        ProgressObserver

	// admission gates head-of-queue pops while the device is under
	// resource pressure (nil = always admit). See AdmissionController's
	// doc for the lock-ordering contract its Admit must honor and
	// dequeueAdmitted for the pop semantics.
	admission     AdmissionController
	clock         wrapper.Clock
	maxDefer      time.Duration
	retryInterval time.Duration
	// deferredSourceKey/deferredSince track how long the CURRENT head of
	// the queue has been denied admission, for the maxDefer bound and the
	// admitted-after-deferral log. Head-only tracking is sufficient: the
	// single worker only ever defers on the head. The tracking MUST be
	// reset on every path that retires the tracked job — admitted pop,
	// expiry pop, empty queue, and reserveForClear removing the tracked
	// key — because a stale deferredSince would otherwise be inherited by
	// a LATER job for the same source (clear-then-re-download, exactly the
	// retry the expiry failure reason tells the client to perform) and
	// could expire that fresh job on its first denial. Always written
	// under s.mu; both the worker (dequeueAdmitted) and clear callers
	// (reserveForClear) write it.
	deferredSourceKey string
	deferredSince     time.Time

	queue *jobQueue

	// captureMu fences store.GC() sweeps against an in-flight Capture,
	// from EITHER capture pipeline captureForClass can route a job to.
	// capturer.Capture (capture.go) writes blobs to the store one
	// resource at a time as it observes them, and mediaCapturer.Capture
	// (mediacapture.go) writes its one resource's blob, both calling
	// store.SaveItem only once at the very end — so for the whole span
	// of a capture there can be freshly-written blobs on disk that no
	// saved item record yet references. GC() treats "not referenced by
	// any saved item" as "orphan, delete it" (store.go), so a GC that
	// runs concurrently with that window (from ClearItem/ClearPlaylist,
	// called directly from commandrouter on a different goroutine than
	// the single capture worker) can delete another, unrelated item's
	// in-progress capture out from under it before its record is ever
	// saved. Holding captureMu for the full Capture call and every GC()
	// call closes that window: a clear that races an active capture
	// simply waits for it to finish saving before sweeping.
	captureMu sync.Mutex

	// playlistRecordMu serializes writes to the playlist INDEX record (the
	// offline displayPlaylist-by-URL fallback) against ClearPlaylist's
	// deletion of it. The record is written by savePlaylistAndURLIndex
	// (from DownloadPlaylist after its classify loop, and from
	// IndexPlaylistForOfflineDisplay on the single-item path) and deleted
	// by ClearPlaylist. playlistClearEpoch alone is not enough: reading
	// the epoch and writing the record are two steps, and a ClearPlaylist
	// landing between them would bump-then-delete, then the stale writer
	// would re-save — resurrecting a cleared record. Both the writer's
	// (epoch re-check + SavePlaylist) and ClearPlaylist's (epoch bump +
	// DeletePlaylist) run under this mutex, so they cannot interleave:
	// whichever acquires it first is authoritative, exactly as the
	// item-level dequeueForProcessing/reserveForClear pair does under
	// s.mu. Deliberately a SEPARATE, narrow mutex rather than s.mu: it is
	// held only across a small playlist-metadata store write (never blob
	// bytes), and folding that I/O under s.mu would stall every unrelated
	// in-memory state read/write for its duration.
	//
	// Lock ordering: whenever both are held, playlistRecordMu is always
	// acquired BEFORE s.mu (the epoch read/bump nested inside), never the
	// reverse — no other path takes them in the opposite order, so there
	// is no cycle.
	playlistRecordMu sync.Mutex

	mu    sync.RWMutex
	state map[string]ItemState // sourceKey -> last-known state, seeded from disk on Start

	// sourceByKey maps a tracked sourceKey back to the raw source URL, so
	// status reporting and notifications can always carry the original
	// URL (the wire identity clients match on — see ItemStatus.Source)
	// even for items that exist only in memory: a queued/failed item with
	// no record on disk yet has nowhere else to recover its source from,
	// since the key is a one-way hash. Kept strictly in lockstep with
	// state: every write that adds a state entry records the source here,
	// and reserveForClear deletes both together. Records on disk don't
	// need it — their source is in the record itself (Item.Source).
	sourceByKey map[string]string

	// downloadEpoch[sourceKey] increments every time that item is cleared
	// (ClearItem/ClearPlaylist, via reserveForClear). It exists solely to
	// close the one download window the state map cannot: a download
	// classifies its source (network I/O, up to the shared HTTP client
	// timeout) BEFORE it registers any StateQueued/StateDownloading, so
	// for that whole span the item is untracked and a concurrent clear
	// sails through reserveForClear (nothing downloading, nothing queued),
	// deletes the on-disk record, and reports success — after which the
	// download would finish classifying, enqueue, capture, and SaveItem
	// the record right back, silently undoing a clear the caller was told
	// succeeded. DownloadItem/DownloadPlaylist sample this epoch under mu
	// before classifying and re-check it under mu at the enqueue commit
	// point; a mismatch means a clear landed in between, so the download
	// aborts instead of resurrecting the item. All post-enqueue windows
	// (StateQueued dequeue race, active StateDownloading capture) are
	// already covered by reserveForClear/dequeueForProcessing sharing mu,
	// so this only guards the pre-enqueue classify window.
	//
	// Grows by one uint64 per distinct source ever cleared and is never
	// pruned: an entry must outlive any in-flight download that sampled
	// it, and pruning would reopen the very race it closes. The footprint
	// is negligible for realistic device item counts, and this is the
	// deliberate trade-off (a few KB of long-lived counters vs. a
	// contract-violating resurrection).
	downloadEpoch map[string]uint64

	// playlistClearEpoch is downloadEpoch's playlist-scoped twin, keyed by
	// playlist ID. It closes the same classify-window race for the
	// playlist RECORD (the offline displayPlaylist-by-URL fallback index):
	// DownloadPlaylist persists that record only AFTER its per-item
	// classify loop, so a ClearPlaylist landing during classification
	// deletes the record and reports success, then DownloadPlaylist
	// re-saves it — resurrecting a cleared playlist as an empty offline
	// fallback (its items all correctly abort via downloadEpoch, but the
	// index itself would linger). DownloadPlaylist samples this before
	// classifying and re-checks it before the save; a mismatch means a
	// ClearPlaylist intervened, so the save is skipped. Kept a SEPARATE
	// map from downloadEpoch rather than sharing one keyed by either id:
	// item and playlist IDs are distinct DP-1 namespaces, and a stray
	// collision must not let one invalidate the other. Same
	// grow-without-pruning trade-off as downloadEpoch, for the same
	// reason.
	playlistClearEpoch map[string]uint64

	// started gates DownloadItem/DownloadPlaylist on the worker
	// goroutine actually running — see ErrServiceNotStarted's doc. Three
	// checks exist against it, each cheaper/more advisory than the
	// last, because Stop() can race in at any point during Classify or
	// the observer notification: DownloadItem/DownloadPlaylist do a
	// lock-free started.Load() up front purely to fail fast before
	// paying for Classify's network round trip; enqueue() repeats that
	// lock-free check before deciding whether to notify the observer of
	// "queued", to avoid a spurious notification for a job that is
	// about to be rejected anyway; the one check that is actually
	// authoritative is enqueue()'s final re-read under mu immediately
	// before queue.push (the same lock Start/Stop use to flip this
	// flag), which is what actually prevents a job from being pushed
	// onto a queue the worker has already stopped draining.
	started atomic.Bool

	cancel context.CancelFunc
	doneCh chan struct{}
}

// NewService constructs a Service. observer may be nil (no-op notifications
// — useful in tests and until the notifications todo wires a real one).
// admission's zero value means "no gate" — see AdmissionOptions.
func NewService(
	store Store,
	classifier Classifier,
	capturer Capturer,
	mediaCapturer MediaCapturer,
	jsonWrapper wrapper.JSON,
	captureWindowMs int,
	maxDiskBytes int64,
	observer ProgressObserver,
	admission AdmissionOptions,
	logger *zap.Logger,
) Service {
	clock := admission.Clock
	if clock == nil {
		clock = wrapper.NewClock()
	}
	maxDefer := admission.MaxDefer
	if maxDefer <= 0 {
		maxDefer = DefaultAdmissionMaxDefer
	}
	retryInterval := admission.RetryInterval
	if retryInterval <= 0 {
		retryInterval = defaultAdmissionRetryInterval
	}
	return &service{
		store:              store,
		classifier:         classifier,
		capturer:           capturer,
		mediaCapturer:      mediaCapturer,
		json:               jsonWrapper,
		logger:             logger,
		captureWindowMs:    captureWindowMs,
		maxDiskBytes:       maxDiskBytes,
		maxQueueLen:        defaultMaxQueueLen,
		observer:           observer,
		admission:          admission.Controller,
		clock:              clock,
		maxDefer:           maxDefer,
		retryInterval:      retryInterval,
		queue:              newJobQueue(),
		state:              make(map[string]ItemState),
		sourceByKey:        make(map[string]string),
		downloadEpoch:      make(map[string]uint64),
		playlistClearEpoch: make(map[string]uint64),
	}
}

func (s *service) Start(ctx context.Context) error {
	// Safe only here: nothing has enqueued a job yet and the worker
	// goroutine below has not started, so no WriteBlob call can possibly
	// be mid-write. Any blobs/*.tmp file found at this point is
	// necessarily an orphan left by the daemon's own previous run being
	// killed (SIGKILL, power loss) before WriteBlob's cleanup defer ran
	// — see SweepIncompleteBlobs's doc for why GC/DiskUsage never reclaim
	// these on their own.
	if removed, freed, err := s.store.SweepIncompleteBlobs(); err != nil {
		s.logger.Warn("offline cache: failed to sweep incomplete blobs on startup", zap.Error(err))
	} else if removed > 0 {
		s.logger.Info("offline cache: swept incomplete blobs left by a previous run",
			zap.Int("removed_files", removed), zap.Int64("freed_bytes", freed))
	}

	// One GC pass in this same no-capture-in-flight window, and for the
	// same reason the sweep above cannot wait for a later trigger: GC is
	// otherwise only reached through clears and eviction, and eviction
	// can only free bytes by deleting records ListItemKeys can see. A
	// store carrying records GC must quarantine (a legacy id-keyed cache
	// from before source keying, or any invalid/mismatched filename)
	// counts their blobs in DiskUsage while eviction can never select
	// them as victims — so near maxDiskBytes every new capture would be
	// starved of budget up front, and the post-capture eviction that
	// might have helped is unreachable because captures keep failing or
	// degrading. Running GC here quarantines those records and reclaims
	// their blobs before the first capture ever samples its budget.
	// Best-effort like the sweep: a transiently unreadable record aborts
	// the pass (see GC's doc), and the next clear/eviction retries. Goes
	// through s.gc() like every other GC call in this file — captureMu is
	// uncontended here (the worker does not exist yet), so this keeps
	// gc()'s "never call the store directly" invariant absolute at zero
	// cost.
	if removed, freed, err := s.gc(); err != nil {
		s.logger.Warn("offline cache: startup GC pass failed", zap.Error(err))
	} else if removed > 0 {
		s.logger.Info("offline cache: startup GC reclaimed unreferenced blobs",
			zap.Int("removed_blobs", removed), zap.Int64("freed_bytes", freed))
	}

	keys, err := s.store.ListItemKeys()
	if err != nil {
		return fmt.Errorf("offline cache: rebuild index: %w", err)
	}

	s.mu.Lock()
	for _, key := range keys {
		rec, err := s.store.LoadItem(key)
		if err != nil {
			s.logger.Warn("offline cache: skipping unreadable item record on startup",
				zap.String("source_key", key), zap.Error(err))
			continue
		}
		s.state[key] = stateFromCoverage(rec.Coverage)
		s.sourceByKey[key] = rec.Item.Source
	}
	s.mu.Unlock()

	runCtx, cancel := context.WithCancel(ctx)
	s.cancel = cancel
	s.doneCh = make(chan struct{})
	go s.run(runCtx)
	s.mu.Lock()
	s.started.Store(true)
	s.mu.Unlock()
	return nil
}

func (s *service) Stop() {
	if s.cancel == nil {
		return
	}
	// Flip started under mu before canceling: enqueue() takes the same
	// lock to check-and-push atomically, so once this returns no
	// concurrent DownloadItem/DownloadPlaylist call can observe
	// started==true and still push a job after the worker below is
	// torn down and stops draining the queue.
	s.mu.Lock()
	s.started.Store(false)
	s.mu.Unlock()
	s.cancel()
	<-s.doneCh
	if err := s.capturer.Close(); err != nil {
		s.logger.Warn("offline cache: closing capturer/headless chromium on shutdown failed", zap.Error(err))
	}
}

func (s *service) run(ctx context.Context) {
	defer close(s.doneCh)
	// The ticker only matters while the head of the queue is deferred by
	// the admission gate: pressure clearing produces no queue.wake, so
	// without a periodic re-check a deferred head would wait for the next
	// enqueue that may never come. It ticks while idle too — a harmless
	// spurious loop iteration every retryInterval, accepted as cheaper and
	// simpler than arming/disarming the ticker around deferral episodes.
	// With no gate at all there is nothing to re-evaluate, so no ticker is
	// created (a nil channel in the select below simply never fires).
	var retryTick <-chan time.Time
	if s.admission != nil {
		ticker := s.clock.NewTicker(s.retryInterval)
		defer ticker.Stop()
		retryTick = ticker.C()
	}
	for {
		j, ok, expired, expiredReason := s.dequeueAdmitted()
		if expired {
			s.failDeferred(ctx, j, expiredReason)
			continue
		}
		if !ok {
			select {
			case <-s.queue.wake:
				continue
			case <-retryTick:
				continue
			case <-ctx.Done():
				// enqueue() pushes while still holding s.mu, the same
				// lock Stop() takes to flip started before calling
				// cancel (see enqueue's doc), so any job a caller
				// successfully enqueued is guaranteed to already be
				// sitting in s.queue by the time ctx.Done() fires here.
				// But Go's select does not prefer whichever case became
				// ready first: if that push's wake signal and this
				// cancellation are both ready by the time this select
				// runs, it picks between them uniformly at random, so
				// ctx.Done() can still win the pick. Drain whatever is
				// left before actually returning so that job fails
				// fast (ctx is already canceled, so Capture below
				// will error quickly) instead of sitting stranded in
				// StateQueued forever with no worker left to drain it.
				//
				// The drain deliberately uses the UNGATED
				// dequeueForProcessing, bypassing the admission gate: a
				// hot device must never be able to stall Stop() behind
				// a deferred head, and every drained job fails fast and
				// quietly via process's ctx.Err() early return anyway.
				for {
					j, ok := s.dequeueForProcessing()
					if !ok {
						return
					}
					s.process(ctx, j)
				}
			}
		}
		s.process(ctx, j)
	}
}

// dequeueForProcessing pops the next queued job and marks it
// StateDownloading in the SAME s.mu critical section — never as two
// separate steps. reserveForClear (ClearItem/ClearPlaylist's guard)
// takes this identical lock around its own busy-check-then-cancel
// sequence, so whichever of the two critical sections runs first is
// authoritative: either this dequeue wins and reserveForClear then
// correctly observes StateDownloading and rejects with ErrItemBusy, or
// reserveForClear wins and removes the job from the queue before this
// function could ever have popped it. Splitting the pop and the state
// write into two separate steps (as an earlier revision did, with
// process() setting StateDownloading a few instructions later) left a
// window where a clear's busy-check could observe the job still
// StateQueued, and this dequeue could then happen — and the queue
// removal that follows the busy-check would find nothing left to
// remove — letting the clear report success while the now-active
// capture "resurrects" the record it just deleted (see ClearItem's
// doc).
func (s *service) dequeueForProcessing() (captureJob, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, ok := s.queue.pop()
	if !ok {
		return captureJob{}, false
	}
	s.state[j.sourceKey] = StateDownloading
	return j, true
}

// dequeueAdmitted is dequeueForProcessing's admission-gated twin, used by
// run()'s steady state (the shutdown drain keeps using the ungated
// original — see run's drain comment). The admission check and the pop are
// ONE s.mu critical section for the same reason dequeueForProcessing fuses
// its pop with the state write: a denied head stays in the queue in
// StateQueued, so a racing reserveForClear can still remove it without
// ErrItemBusy, and an admitted pop can never race a clear or pop a job of
// a different class than the one just admitted (the clear would have had
// to run inside this critical section to swap the head).
//
// Return shape: ok means j was popped (marked StateDownloading) and must
// be processed. expired means the head outlived maxDefer: it was popped
// and marked StateFailed atomically here — mirroring the pop+state-write
// invariant — and the caller owns the observer notification via
// failDeferred (state writes happen under s.mu, observer callouts never do
// — see notifyObserver's doc); expiredReason carries the gate's last
// denial cause for that notification. Neither flag set means the queue is
// empty or its head is deferred; the caller waits and retries either way.
func (s *service) dequeueAdmitted() (j captureJob, ok bool, expired bool, expiredReason string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	head, exists := s.queue.peek()
	if !exists {
		// An emptied queue retires whatever was being tracked (the head
		// was popped, cleared, or drained) — see the field doc on why
		// stale tracking must never outlive its job.
		s.deferredSourceKey = ""
		return captureJob{}, false, false, ""
	}

	if s.admission != nil {
		if decision := s.admission.Admit(head.class); !decision.Allowed {
			now := s.clock.Now()
			if s.deferredSourceKey != head.sourceKey {
				// A new head started deferring (first denial, or the
				// previous head was cleared/expired out from under the
				// tracking): restart the clock.
				s.deferredSourceKey = head.sourceKey
				s.deferredSince = now
				s.logger.Info("offline cache: deferring download under system pressure",
					zap.String("source", head.item.Source),
					zap.String("class", string(head.class)),
					zap.String("reason", decision.Reason))
			}
			if now.Sub(s.deferredSince) <= s.maxDefer {
				return captureJob{}, false, false, ""
			}
			// Deferred past the bound: fail it so the FIFO cannot be
			// wedged forever behind one item on a persistently hot
			// device. Pop + terminal state in this same critical
			// section, exactly like the ok path below.
			popped, _ := s.queue.pop()
			s.state[popped.sourceKey] = StateFailed
			s.deferredSourceKey = ""
			s.logger.Warn("offline cache: failing download deferred past the admission bound",
				zap.String("source", popped.item.Source),
				zap.String("class", string(popped.class)),
				zap.Duration("deferred_for", now.Sub(s.deferredSince)),
				zap.String("reason", decision.Reason))
			return popped, false, true, decision.Reason
		}
	}

	popped, _ := s.queue.pop()
	s.state[popped.sourceKey] = StateDownloading
	if s.deferredSourceKey == popped.sourceKey {
		s.logger.Info("offline cache: admitting download after deferral",
			zap.String("source", popped.item.Source),
			zap.Duration("deferred_for", s.clock.Now().Sub(s.deferredSince)))
	}
	s.deferredSourceKey = ""
	return popped, true, false, ""
}

// failDeferred emits the observer notification for a job dequeueAdmitted
// expired (its StateFailed was already written there, under s.mu). The
// queuedNotified barrier is honored with the same ctx escape process uses,
// so failed can never reach a client before its own queued did — and a
// canceled ctx means shutdown is tearing everything down and ordering no
// longer matters. gateReason is the gate's last denial cause, embedded so
// the client sees WHAT was over threshold, not just that something was.
func (s *service) failDeferred(ctx context.Context, j captureJob, gateReason string) {
	if j.queuedNotified != nil {
		select {
		case <-j.queuedNotified:
		case <-ctx.Done():
		}
	}
	s.notifyObserver(j.item.Source, StateFailed, Coverage{
		Reason: "download deferred too long under sustained system pressure (" + gateReason + "); re-issue the download once the device recovers",
	})
}

// reserveForClear is dequeueForProcessing's counterpart on the
// clear side — see that function's doc for why both must share this
// exact lock and why the check-then-mutate sequence below must be one
// critical section, not two. keys are sourceKeys. If ANY key in keys is
// already StateDownloading, nothing is mutated and that key is returned
// alongside ErrItemBusy (map iteration order is unspecified, so which
// one is named when several are busy simultaneously is not guaranteed
// — every caller's contract only promises all-or-nothing, never a
// specific culprit). err is returned as exactly ErrItemBusy, not
// wrapped with the busy key baked in, so each caller can format its own
// source/playlistID-scoped message the same way it already did before
// this shared helper existed. Otherwise every key's queued job (if any)
// is dropped and its tracked state cleared, atomically with the check
// that just passed.
//
// The returned clearReservation describes what the reservation actually
// took away from clients — see its own doc.
func (s *service) reserveForClear(keys map[string]bool) (res clearReservation, busyKey string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for key := range keys {
		if s.state[key] == StateDownloading {
			return clearReservation{}, key, ErrItemBusy
		}
	}
	res.settled = make(map[string]bool, len(keys))
	for key := range keys {
		if st, tracked := s.state[key]; tracked && st != StateNotCached {
			res.settled[key] = true
		}
		delete(s.state, key)
		delete(s.sourceByKey, key)
		// Invalidate any download of this source that is still in its
		// pre-enqueue classify window: bumping the epoch here (under the
		// same mu enqueue commits under) is what a racing
		// DownloadItem/DownloadPlaylist observes as "a clear landed after
		// I sampled," making it abort rather than resurrect this record
		// once classification finishes — see downloadEpoch's doc. Done
		// only after the busy check above passed, so a rejected
		// (ErrItemBusy) clear never perturbs an in-flight download's
		// epoch.
		s.downloadEpoch[key]++
	}
	for _, j := range s.queue.removeItems(keys) {
		if j.queuedNotified != nil {
			res.queuedNotified = append(res.queuedNotified, j.queuedNotified)
		}
	}
	// Retire the admission-deferral tracking if this clear just removed
	// the tracked head: a later re-download of the same source is a NEW
	// job and must start with a fresh deferral clock, not inherit this
	// one's (see the deferredSourceKey field doc for the instant-expiry
	// failure this prevents). Same s.mu section as the removal, so the
	// worker can never observe the removed job with tracking still
	// attached.
	if keys[s.deferredSourceKey] {
		s.deferredSourceKey = ""
	}
	return res, "", nil
}

// clearReservation is what a reserveForClear call took away from clients,
// handed to ClearItem/ClearPlaylist so they can report and announce the
// outcome truthfully.
type clearReservation struct {
	// settled names the ids the reservation actually moved TO not_cached:
	// those that had a tracked state other than not_cached (queued, or a
	// terminal ready/partial/failed/broken_online). Callers use it for the
	// two things that must not treat "no record on disk" as "this clear did
	// nothing":
	//   - ClearItem's not_found decision — a first-time download still
	//     sitting in the queue has no record yet, but canceling it is real
	//     work.
	//   - which ids warrant a not_cached notification, ALONGSIDE the store's
	//     own "a record really was removed" signal (see Store.DeleteItem):
	//     an id that was neither tracked nor on disk was already not_cached
	//     to every client, so there is no transition to announce and
	//     emitting one would be pure noise (a playlist clear would otherwise
	//     fan out one notification per never-cached member item).
	//
	// Tracked state is the right source of truth for the first, rather than
	// the queue: it is exactly what this service last pushed to observers,
	// it is seeded from the on-disk records at Start (see start's rebuild
	// loop), and enqueue writes StateQueued in the SAME critical section as
	// its queue.push (see its phase 2), so "state[id] == StateQueued" and "a
	// job for id is still in the queue" cannot disagree.
	settled map[string]bool
	// queuedNotified holds the barriers of the queued jobs this reservation
	// dropped — see awaitQueuedNotifications for why a clear must wait on
	// them.
	queuedNotified []chan struct{}
}

// awaitQueuedNotifications blocks until every queued job this reservation
// dropped has finished emitting its own state:"queued" notification.
//
// enqueue commits a job under s.mu but emits its "queued" notification
// AFTER releasing the lock (it must never call the observer under that
// lock — see notify's doc). A clear only needs s.mu, so it can slip in
// between: it would cancel the job and announce not_cached, and only then
// would the enqueuing goroutine emit "queued" — leaving every client on a
// terminal-looking queued entry for work that no longer exists and for
// which no further notification is ever coming. That is precisely the
// failure mode enqueue's own notify-ordering comment exists to prevent, so
// the clear side must respect the same barrier rather than reintroduce it
// from the other direction.
//
// This is the identical mechanism (and identical justification) as
// process()'s wait before emitting "downloading" — see
// captureJob.queuedNotified. enqueue closes the channel unconditionally
// once it has pushed, so a job that is in the queue is always guaranteed to
// have its barrier closed; waiting here can only be as slow as one observer
// callback, the same bound the capture worker already accepts. Unlike
// process()'s wait, this one has no ctx to escape through — ClearItem/
// ClearPlaylist take none — so it relies on ProgressObserver honoring its
// "must not block" contract (see that interface's doc).
//
// This closes the enqueue-vs-clear inversion specifically, NOT notification
// ordering in general. One window remains: a clear cannot pass
// reserveForClear's busy check until process's notify has already written
// the capture's terminal state (dequeueForProcessing holds StateDownloading
// from the pop until then, and Capture's SaveItem runs before it), but
// notify releases s.mu before calling the observer — so a clear can slip in
// there, delete the record that capture just saved, emit not_cached, and
// have the capture's own ready/failed land afterwards. The item really is
// gone; the client is simply left on a stale terminal state until its next
// getOfflineCacheStatus.
//
// That is deliberately not fixed here, for two reasons: the window is a few
// instructions wide rather than a whole observer callback (nothing analogous
// to enqueue's post-unlock transport call sits inside it), and the outcome
// is no worse than before clears notified at all — the client kept that
// terminal state regardless. Closing it would mean ordering against
// process's notify too, which has no barrier to reuse. Do not read this
// barrier as making the observer stream totally ordered.
//
// Callers must call this only after releasing s.mu.
func (r clearReservation) awaitQueuedNotifications() {
	for _, ch := range r.queuedNotified {
		<-ch
	}
}

// currentEpoch reads a source's clear-epoch under mu. Callers sample it
// before the network-bound Classify step and hand the sample to enqueue,
// which re-reads and compares under the commit lock — see downloadEpoch's
// doc for the resurrection race this closes.
func (s *service) currentEpoch(sourceKey string) uint64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.downloadEpoch[sourceKey]
}

// currentPlaylistEpoch is currentEpoch's playlist-scoped twin — see
// playlistClearEpoch's doc. DownloadPlaylist samples it before its
// classify loop and re-checks before persisting the playlist record.
func (s *service) currentPlaylistEpoch(playlistID string) uint64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.playlistClearEpoch[playlistID]
}

// CurrentPlaylistClearGeneration is the exported sampling entry point for
// callers that resolve+download+index a playlist across the Service
// boundary (handleDownloadPlaylistItem) — see its interface doc. It is
// just currentPlaylistEpoch under its contract-facing name.
func (s *service) CurrentPlaylistClearGeneration(playlistID string) uint64 {
	return s.currentPlaylistEpoch(playlistID)
}

// markPlaylistCleared bumps a playlist's clear-epoch so a DownloadPlaylist
// still in its classify window will not resurrect the record this clear is
// about to delete — see playlistClearEpoch's doc. Under the same mu
// DownloadPlaylist re-checks the sampled epoch under.
func (s *service) markPlaylistCleared(playlistID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.playlistClearEpoch[playlistID]++
}

// stateFromCoverage classifies a finished capture's Coverage into an
// ItemState. A capture whose only failures were CSP-blocked page assets is
// reported as StateBrokenOnline rather than StatePartial: a CSP block
// means the artwork never renders even with a live network connection, so
// "partial" (implying a normal artwork playable with a few pieces missing)
// would misrepresent it to the mobile app. Any capture with at least one
// non-CSP failure keeps the more general StatePartial classification.
func stateFromCoverage(c Coverage) ItemState {
	if c.Complete {
		return StateReady
	}
	if isBrokenOnlineCoverage(c.Reason) {
		return StateBrokenOnline
	}
	return StatePartial
}

// isBrokenOnlineCoverage reports whether every failure recorded in reason
// is a CSP block. Coverage.Reason is a semicolon-joined free-text list
// (see capture.go's resolveResources, which wraps this as
// "loading_failed(csp_blocked):<url>"), so this is necessarily a
// best-effort substring match rather than a structured check; an empty
// reason or any non-CSP failure is not broken-online.
func isBrokenOnlineCoverage(reason string) bool {
	if reason == "" {
		return false
	}
	for _, part := range strings.Split(reason, "; ") {
		if !strings.Contains(part, ReasonCSPBlocked) {
			return false
		}
	}
	return true
}

func (s *service) DownloadItem(ctx context.Context, item dp1playlist.PlaylistItem) error {
	if !s.started.Load() {
		return ErrServiceNotStarted
	}
	// Source is the cache identity (see SourceKey); the DP-1 core schema
	// makes the item id optional and defines it as a random UUID v4, so a
	// conforming playlist may omit it or change it freely — it is
	// deliberately NOT required here.
	if item.Source == "" {
		return errors.New("offline cache: item must have a source")
	}

	// Sample the clear-epoch BEFORE the (network-bound) classify below,
	// then hand it to enqueue: a ClearItem/ClearPlaylist for this source
	// that lands anywhere in the classify window bumps the epoch, so
	// enqueue aborts instead of resurrecting a record the clear just
	// deleted and reported success for — see downloadEpoch's doc.
	epoch := s.currentEpoch(SourceKey(item.Source))

	class, err := s.classifier.Classify(ctx, item.Source)
	if err != nil {
		return fmt.Errorf("offline cache: classify %s: %w", item.Source, err)
	}
	// ClassStreaming is the only rejected class — see
	// ErrUnsupportedMediaClass's doc. Every other class (software,
	// media, or unknown-but-still-single-file) is enqueued and routed
	// by captureForClass once the worker dequeues it.
	if class == ClassStreaming {
		return ErrUnsupportedMediaClass
	}

	// Classify above can be slow (network I/O), so the started.Load()
	// fast-fail at the top of this function is only advisory: a Stop()
	// racing this call could tear the worker down in between. enqueue
	// re-checks started under the same lock Stop uses to flip it, so
	// this is the authoritative check that actually prevents pushing a
	// job onto a queue nobody will ever drain. It also returns
	// ErrQueueFull if the queue is already at capacity — see that
	// error's doc.
	//
	// enqueueAlreadyQueued is success for this caller: a repeated
	// download of an item already queued/downloading is an idempotent
	// no-op, and the item genuinely is scheduled. Only enqueueClearWon
	// means nothing was queued and nothing will be, which is the one
	// outcome that must not reach the caller as success — see
	// ErrClearedDuringDownload's doc.
	outcome, err := s.enqueue(item, epoch, class)
	if err != nil {
		return err
	}
	if outcome == enqueueClearWon {
		return ErrClearedDuringDownload
	}
	return nil
}

// classifyPhaseTimeout bounds the whole classification step of one
// DownloadPlaylist call.
//
// Classification is a real network round trip per item (a HEAD, with a
// ranged-GET fallback — see classify.go) on the daemon-wide 30s client,
// and it runs BEFORE the command can answer. Serially, a playlist of
// unreachable sources therefore held the command — and its heavy
// storm-gate weight — for (item count) x up to 30s, i.e. hours, while
// the LAN hub gave up on the response at its own 30s write deadline and
// the work carried on regardless (hub.go passes the daemon context, not
// the request's, so the client hanging up cancels nothing).
//
// Bounding the phase — rather than moving classification into the
// background — keeps every documented response field honest:
// queuedCount still means "actually queued", and the all-failed case is
// still distinguishable from "no cacheable items". An item not
// classified before this deadline is treated exactly like a classify
// failure, which is already a logged, skipped, excluded-from-queuedCount
// outcome. A caller that wants those items simply retries.
const classifyPhaseTimeout = 10 * time.Second

// classifyConcurrency caps how many classify probes are in flight at
// once. Enough to make a large playlist finish well inside
// classifyPhaseTimeout when origins are healthy, small enough not to
// fan out a burst at one origin (a playlist's items commonly share a
// host).
const classifyConcurrency = 8

// classifyPlaylistItems classifies every eligible item concurrently and
// returns those worth queuing, in playlist order, plus how many failed
// classification outright. Order is preserved deliberately: the enqueue
// loop that consumes this drives capture order, and a playlist's item
// order is the artist's, not an implementation detail to be scrambled by
// whichever probe happened to answer first.
func (s *service) classifyPlaylistItems(ctx context.Context, items []dp1playlist.PlaylistItem) ([]queuedItem, int) {
	ctx, cancel := context.WithTimeout(ctx, classifyPhaseTimeout)
	defer cancel()

	results := make([]*queuedItem, len(items))
	failed := make([]bool, len(items))

	var wg sync.WaitGroup
	slots := make(chan struct{}, classifyConcurrency)
	// Dedup repeated sources up front: two playlist items sharing a
	// source ARE the same cache entry (see ItemRecord's per-source doc),
	// so probing the duplicate would be a wasted network round trip and
	// its job would only be rejected by enqueue's already-queued check
	// anyway. First occurrence wins, preserving playlist order.
	seen := make(map[string]bool, len(items))
	for i, item := range items {
		if item.Source == "" {
			continue
		}
		if key := SourceKey(item.Source); seen[key] {
			continue
		} else {
			seen[key] = true
		}
		wg.Add(1)
		go func(i int, item dp1playlist.PlaylistItem) {
			defer wg.Done()
			slots <- struct{}{}
			defer func() { <-slots }()

			// Sampled per item immediately before its own classify, so
			// a clear landing during this (network-bound) window is
			// still caught at enqueue — see downloadEpoch's doc. The
			// concurrency does not change that contract: each item's
			// epoch still brackets only its own classify.
			epoch := s.currentEpoch(SourceKey(item.Source))
			class, err := s.classifier.Classify(ctx, item.Source)
			if err != nil {
				failed[i] = true
				s.logger.Warn("offline cache: classify failed while queuing playlist, skipping item",
					zap.String("source", item.Source), zap.Error(err))
				return
			}
			// ClassStreaming is the only excluded class — see
			// ErrUnsupportedMediaClass's doc. Silently skipped here (not
			// counted as a failure) since a live/streaming item is a
			// legitimate, correctly-classified exclusion, not a
			// classification failure.
			if class == ClassStreaming {
				return
			}
			results[i] = &queuedItem{item: item, epoch: epoch, class: class}
		}(i, item)
	}
	wg.Wait()

	toQueue := make([]queuedItem, 0, len(items))
	failures := 0
	for i := range items {
		if failed[i] {
			failures++
			continue
		}
		if results[i] != nil {
			toQueue = append(toQueue, *results[i])
		}
	}
	return toQueue, failures
}

// queuedItem pairs an item with the clear-epoch sampled just before its
// classify and the class that classify produced, so the enqueue loop can
// detect a ClearItem/ClearPlaylist that landed in between and route the
// job without re-classifying — see downloadEpoch's and captureJob.class's
// docs.
type queuedItem struct {
	item  dp1playlist.PlaylistItem
	epoch uint64
	class MediaClass
}

func (s *service) DownloadPlaylist(ctx context.Context, playlistRaw json.RawMessage, sourceURL string) (int, int, error) {
	if !s.started.Load() {
		return 0, 0, ErrServiceNotStarted
	}
	var playlist dp1playlist.Playlist
	if err := s.json.Unmarshal(playlistRaw, &playlist); err != nil {
		return 0, 0, fmt.Errorf("offline cache: parse playlist: %w", err)
	}
	if playlist.ID == "" {
		return 0, 0, errors.New("offline cache: playlist has no id")
	}

	// Sample the playlist's clear-epoch before the classify loop so a
	// ClearPlaylist landing during classification is detected before the
	// save below and cannot resurrect the cleared playlist record — see
	// playlistClearEpoch's doc.
	plEpoch := s.currentPlaylistEpoch(playlist.ID)

	// Classification only decides WHICH items are eligible — it must
	// never itself start any work. Enqueuing is deliberately deferred
	// to its own loop below, after the playlist is safely persisted,
	// so a SavePlaylist failure (rare, but a real I/O error path) can
	// never leave already-accepted capture jobs running in the
	// background for a call this method is about to report as failed.
	total := len(playlist.Items)
	// Pair each queued item with the clear-epoch sampled just before its
	// classify, so a ClearItem/ClearPlaylist landing during this (per
	// item, network-bound) loop or the SavePlaylist below is detected at
	// enqueue and cannot be silently undone — see downloadEpoch's doc.
	toQueue, classifyFailed := s.classifyPlaylistItems(ctx, playlist.Items)
	if len(toQueue) == 0 && classifyFailed > 0 {
		// Distinguish "classification itself is broken" from "this
		// playlist genuinely has no cacheable items" — the latter is a
		// normal, successful ok:true/queuedCount:0 outcome, but the
		// former must not look identical to it (see this method's doc).
		// Deliberately returns BEFORE saving the playlist/URL index
		// below: loadCachedPlaylistForURL's/CachedPlaylistForURL's doc
		// promises that an offline-fallback playlist record can only
		// exist if downloadPlaylist actually succeeded for it, never for
		// a download this method is about to report as failed to the
		// caller — saving it anyway here would let displayPlaylist's
		// offline fallback later serve a playlist body whose items were
		// never actually queued because classification itself was down,
		// not because it genuinely has none.
		return 0, total, fmt.Errorf(
			"offline cache: classify playlist %s items: %d of %d item(s) failed classification, none queued",
			playlist.ID, classifyFailed, total)
	}

	// savePlaylistAndURLIndex re-checks plEpoch under playlistRecordMu and
	// skips the write if a ClearPlaylist for this exact playlist landed
	// since plEpoch was sampled (the check and the write are one critical
	// section, so a clear cannot slip between them — see
	// playlistRecordMu's doc). saved==false is that "clear won" case:
	// nothing has been enqueued yet and the per-item epoch checks in the
	// enqueue loop below would abort every item anyway, so it is reported
	// as a clean 0-queued/no-error outcome.
	saved, err := s.savePlaylistAndURLIndex(playlist.ID, playlistRaw, sourceURL, plEpoch)
	if err != nil {
		// Nothing has been enqueued yet at this point — a save failure
		// here starts no background work at all, matching this error
		// return exactly.
		return 0, total, err
	}
	if !saved {
		return 0, total, nil
	}

	queued := 0
	for _, q := range toQueue {
		outcome, err := s.enqueue(q.item, q.epoch, q.class)
		if err != nil {
			if errors.Is(err, ErrServiceNotStarted) {
				// Stop() raced in mid-loop; the remaining items would
				// fail the same way, so stop trying rather than log
				// once per item. The playlist/URL index are already
				// durably saved above, and whatever DID get queued
				// before the race is real work already in flight, so
				// this is reported as success (matching the pre-
				// existing "Stop() raced" tolerance), not an error.
				break
			}
			// ErrQueueFull (or any other unexpected enqueue error): the
			// caller needs to know the remaining items were NOT queued
			// — unlike the Stop()-raced case above, this is an ongoing
			// condition a retry can plausibly resolve, so it must not
			// be silently swallowed into a success report.
			return queued, total, fmt.Errorf("offline cache: queue playlist %s items: %w", playlist.ID, err)
		}
		// Only count jobs THIS call actually put in the queue.
		// enqueueAlreadyQueued (item already queued/downloading from an
		// earlier call) and enqueueClearWon (a concurrent clear won)
		// both scheduled nothing new, so counting either would inflate
		// queuedCount above what this request truly added for a retried
		// or clear-racing playlist. Unlike DownloadItem, a clear-won
		// item is NOT surfaced as an error here: a playlist download is
		// an aggregate whose other items may well have queued fine, and
		// queuedCount already tells the caller exactly how many did.
		if outcome == enqueueQueued {
			queued++
		}
	}
	return queued, total, nil
}

// savePlaylistAndURLIndex is the shared save+index step DownloadPlaylist
// and IndexPlaylistForOfflineDisplay both need — the only difference
// between the two callers is whether classification/queuing happens
// around it, not this part.
//
// sampledEpoch is the playlist's clear-generation the caller read BEFORE
// the network-bound work (classify / DP-1 resolution) that preceded this
// save. Under playlistRecordMu, this re-reads the current generation and,
// if it advanced, skips the write entirely (saved=false, err=nil): a
// ClearPlaylist landed since the caller committed to this download, and
// re-saving would resurrect the record it deleted. Holding
// playlistRecordMu across the re-check AND the store writes is what makes
// it a genuine guard rather than a racy check-then-write — see that
// mutex's doc. saved=true means the record was persisted.
func (s *service) savePlaylistAndURLIndex(playlistID string, playlistRaw json.RawMessage, sourceURL string, sampledEpoch uint64) (bool, error) {
	s.playlistRecordMu.Lock()
	defer s.playlistRecordMu.Unlock()

	if s.currentPlaylistEpoch(playlistID) != sampledEpoch {
		return false, nil
	}
	if err := s.store.SavePlaylist(playlistID, playlistRaw); err != nil {
		return false, fmt.Errorf("offline cache: save playlist %s: %w", playlistID, err)
	}
	if sourceURL != "" {
		// Best-effort: the playlist itself is already safely saved above
		// regardless of whether this index write succeeds, so a failure
		// here only degrades the displayPlaylist-by-URL offline fallback
		// (CachedPlaylistForURL) rather than the download itself.
		if err := s.store.SavePlaylistURLIndex(sourceURL, playlistID); err != nil {
			s.logger.Warn("offline cache: failed to index playlist by source URL",
				zap.String("playlist_id", playlistID), zap.Error(err))
		}
	}
	// Bound the metadata this just wrote. A playlist body is persisted
	// even when nothing in the playlist is cacheable, and eviction only
	// ever walks items and their blobs, so without this a series of
	// distinct downloads grows playlists/ without limit and outside any
	// budget — see Store.MaxPlaylistRecords.
	if pruned, err := s.store.PrunePlaylistRecords(MaxPlaylistRecords); err != nil {
		s.logger.Warn("offline cache: failed to prune old playlist records", zap.Error(err))
	} else if pruned > 0 {
		s.logger.Info("offline cache: pruned old playlist records past the retention bound",
			zap.Int("pruned", pruned), zap.Int("keep", MaxPlaylistRecords))
	}
	return true, nil
}

// IndexPlaylistForOfflineDisplay: see the Service interface doc for why
// this exists (downloadPlaylistItem's single-item scope) and what it
// deliberately does NOT do (classify or queue anything). sampledEpoch is
// the playlist's clear-generation the CALLER read (via
// CurrentPlaylistClearGeneration) before the resolve+download sequence
// this index write follows, so a ClearPlaylist racing that sequence is
// honored rather than undone here — see savePlaylistAndURLIndex and
// playlistRecordMu's docs.
func (s *service) IndexPlaylistForOfflineDisplay(playlistRaw json.RawMessage, sourceURL string, sampledEpoch uint64) error {
	if !s.started.Load() {
		return ErrServiceNotStarted
	}
	var playlist dp1playlist.Playlist
	if err := s.json.Unmarshal(playlistRaw, &playlist); err != nil {
		return fmt.Errorf("offline cache: parse playlist: %w", err)
	}
	if playlist.ID == "" {
		return errors.New("offline cache: playlist has no id")
	}
	// saved==false (a ClearPlaylist won the race) is not an error for this
	// best-effort index path — the caller only logs failures.
	_, err := s.savePlaylistAndURLIndex(playlist.ID, playlistRaw, sourceURL, sampledEpoch)
	return err
}

// enqueue is idempotent: an item already queued or downloading is left
// alone rather than double-scheduled. Returns ErrServiceNotStarted
// without touching the queue if the service was stopped concurrently —
// see the started field's doc comment for why the fresh check in phase
// 2 below (rather than the caller's earlier started.Load(), or even
// this function's own advisory check up front) is the one that actually
// matters. Returns ErrQueueFull if the queue is already at maxQueueLen
// — see that error's doc.
//
// The queued bool distinguishes "this call actually pushed a new job"
// (true) from every no-op nil-error outcome (false: already
// queued/downloading, or a concurrent clear won the race — see the
// cleared checks below). Callers that report an aggregate count back to
// a client (DownloadPlaylist's queuedCount) MUST gate on this bool
// rather than on err == nil: counting every nil return as "queued"
// overstates the number of jobs actually newly scheduled whenever a
// request is retried while items are still in flight, or races a
// ClearPlaylist/ClearItem for the same id.
//
// This runs in two locked phases with the observer notification in
// between, rather than one critical section covering everything, for two
// reasons:
//   - Calling out to s.observer while holding s.mu would block every
//     other state read/write in the service for as long as the observer
//     takes, which this package has no bound on (it may be a websocket
//     send).
//   - Ordering: the "queued" notification must reach the observer before
//     the worker can possibly dequeue this job and report "downloading"
//     for the same item (mobile status displays assume that sequence).
//     Phase 2's push is what first makes the job visible to the worker,
//     so notifying in between the phases — rather than after phase 2, as
//     an earlier version of this function did — guarantees that ordering
//     by construction instead of by coincidence.
//
// The queue-full check is duplicated in both phases for the same reason
// the started check is: phase 1's read is only advisory (racing an
// unbounded number of concurrent callers all past phase 1 at once could
// still let more than maxQueueLen through if it were the only check),
// so it exists purely to skip the observer's spurious "queued"
// notification in the COMMON case where the queue is nowhere near full,
// not to be the authoritative guard. Phase 2's check, taken under the
// same lock as the actual push, is what actually enforces the bound.
func (s *service) enqueue(item dp1playlist.PlaylistItem, epoch uint64, class MediaClass) (enqueueOutcome, error) {
	if !s.started.Load() {
		return enqueueClearWon, ErrServiceNotStarted
	}
	key := SourceKey(item.Source)
	s.mu.RLock()
	st, tracked := s.state[key]
	queueFull := s.queue.len() >= s.maxQueueLen
	cleared := s.downloadEpoch[key] != epoch
	s.mu.RUnlock()
	if tracked && (st == StateQueued || st == StateDownloading) {
		return enqueueAlreadyQueued, nil
	}
	// A clear landed since this download sampled its epoch (see
	// downloadEpoch's doc): honor the clear and abort rather than
	// resurrect the record it deleted. Advisory only (RLock) — a clear
	// can still land between here and the commit below, so phase 2
	// re-checks authoritatively under the same mu reserveForClear bumps
	// the epoch under. This early exit just avoids the wasted lock
	// acquisition in the common case.
	if cleared {
		return enqueueClearWon, nil
	}
	if queueFull {
		return enqueueClearWon, ErrQueueFull
	}

	// Phase 2: push happens before mu is released, not after. Stop()
	// must take this same mu to flip started false before it can call
	// cancel() (see Stop's doc), so pushing here — while re-checking
	// started fresh, since phase 1's check above and the observer call
	// just above could have raced a Stop() in between — makes "job is
	// in the queue" happen-before any subsequent Stop() via mutex
	// handoff. Without this fresh check-and-push done atomically, a
	// Stop() landing after phase 1 but before this point could tear the
	// worker down and strand the job in s.queue forever with nothing
	// left to drain it.
	s.mu.Lock()
	if !s.started.Load() {
		s.mu.Unlock()
		return enqueueClearWon, ErrServiceNotStarted
	}
	if st, ok := s.state[key]; ok && (st == StateQueued || st == StateDownloading) {
		s.mu.Unlock()
		return enqueueAlreadyQueued, nil
	}
	// Authoritative clear check: this read and the queue.push below are
	// one critical section under the same mu reserveForClear bumps the
	// epoch under, so a clear either fully precedes this push (epoch
	// mismatch → abort, record stays deleted) or fully follows it (item
	// is now StateQueued → reserveForClear removes the queued job). There
	// is no interleaving that both passes this check and leaves a job the
	// clear failed to remove — see downloadEpoch's doc.
	if s.downloadEpoch[key] != epoch {
		s.mu.Unlock()
		return enqueueClearWon, nil
	}
	if s.queue.len() >= s.maxQueueLen {
		s.mu.Unlock()
		return enqueueClearWon, ErrQueueFull
	}
	s.state[key] = StateQueued
	s.sourceByKey[key] = item.Source
	queuedNotified := make(chan struct{})
	s.queue.push(captureJob{sourceKey: key, item: item, class: class, queuedNotified: queuedNotified})
	s.mu.Unlock()

	// Notified only AFTER the commit above, never before it. An earlier
	// revision fired this between phase 1 and phase 2, so a clear
	// landing in that window produced an offline_cache_status
	// state:"queued" for an item that was then never queued — a
	// terminal-looking progress entry the mobile app would wait on
	// forever, since no further notification for that item was ever
	// coming. Emitting after the push means the notification can only
	// ever describe work that really is scheduled. Deliberately outside
	// s.mu: the observer sends over the relayer/hub WS, and notify's doc
	// explains why that must never run under this lock.
	s.notifyObserver(item.Source, StateQueued, Coverage{})
	// Releases process()'s barrier — see captureJob.queuedNotified.
	// Deliberately after the notify above, and unconditional, so the
	// worker is never left waiting on a job whose enqueuer has already
	// moved on.
	close(queuedNotified)
	return enqueueQueued, nil
}

func (s *service) process(ctx context.Context, j captureJob) {
	// Shutdown drain: run() empties whatever is left in the queue
	// through here after ctx is already canceled (see its ctx.Done()
	// branch). Those jobs were never started and never will be, so this
	// records the terminal state and returns WITHOUT notifying.
	//
	// The notifications are what made this unbounded. Each drained job
	// used to emit downloading and then failed, and Notifier's relayer
	// send is synchronous on this goroutine with its own 5s deadline
	// built from context.Background() — so cancellation could not
	// shorten it. Stop() blocks on doneCh until the drain finishes, and
	// the queue holds up to maxQueueLen jobs, which put shutdown at up
	// to 2 x 5s x backlog against a wedged relayer. systemd would reach
	// TimeoutStopSec and SIGKILL long before that, skipping the
	// capturer.Close() that Stop runs afterwards — the daemon's one
	// hook for tearing down the second headless Chromium, which exists
	// precisely so shutdown does not depend on the cgroup kill.
	//
	// Nothing is lost by staying quiet here: the queue is in-memory
	// only, every one of these items is about to become "not cached" on
	// a store the next Start rebuilds from disk, and both transports are
	// moments from being torn down anyway. This deliberately does NOT
	// cover a capture already in flight when cancellation lands — that
	// one is a real attempt with a real outcome, and its attempt-level
	// failed notification is a contract pinned by
	// TestService_Stop_DuringInFlightRecaptureLeavesReadyStatusUntouched.
	if ctx.Err() != nil {
		// The same terminal state the old notify-driven path left
		// behind, so Status keeps answering identically for a Stop()ed
		// service that outlives the process (tests, chiefly).
		s.mu.Lock()
		s.state[j.sourceKey] = StateFailed
		s.mu.Unlock()
		return
	}

	// Wait for this job's own queued notification to have gone out
	// before announcing downloading, so the two never reach a client in
	// the wrong order — see captureJob.queuedNotified. The ctx escape
	// keeps shutdown from ever blocking on it: a canceled ctx means this
	// capture is about to fail anyway, so ordering no longer matters.
	if j.queuedNotified != nil {
		select {
		case <-j.queuedNotified:
		case <-ctx.Done():
		}
	}

	// s.state[j.sourceKey] is already StateDownloading —
	// dequeueForProcessing set it atomically with the pop that produced j
	// (see that function's doc). Only the observer notification remains
	// to do here, and it deliberately runs without s.mu held (see
	// notify's doc on why the observer call is never made under that
	// lock).
	s.notifyObserver(j.item.Source, StateDownloading, Coverage{})

	// Make room BEFORE the capture starts, not only after it succeeds:
	// both capture pipelines seed their disk budget with the store's
	// CURRENT remaining room (newDiskBudgetFromStore), and
	// enforceDiskLimit below only ever runs after a successful capture,
	// so a store already sitting at maxDiskBytes would otherwise starve
	// every new capture of budget before eviction ever got a chance to
	// run — freezing the cache on its oldest contents forever instead of
	// rolling over to newer ones. Must run before captureMu is taken:
	// the eviction loop's GC goes through s.gc(), which locks captureMu
	// itself. That ordering is safe against a concurrent capture because
	// this single worker goroutine is the only place captures run.
	s.reclaimDiskForCapture(j.sourceKey)

	// Held for the full Capture call, not just its final SaveItem — see
	// captureMu's doc on why the whole blob-writing window must be
	// fenced, not only the save.
	s.captureMu.Lock()
	rec, err := s.captureForClass(ctx, j)
	s.captureMu.Unlock()
	if err != nil {
		s.logger.Warn("offline cache: capture failed", zap.String("source", j.item.Source), zap.Error(err))
		s.notify(j.item.Source, StateFailed, Coverage{Reason: err.Error()})
		return
	}

	s.notify(j.item.Source, stateFromCoverage(rec.Coverage), rec.Coverage)
	s.enforceDiskLimit(j.sourceKey, j.item.Source)
}

// captureForClass routes a queued job to the capture pipeline matching
// its classified media type, sampled once at classify time and carried
// on the job (see captureJob.class's doc): ClassSoftware needs the
// headless-Chromium pipeline (capturer) to run arbitrary code and
// observe its unenumerable dependency graph; every other class routes
// to the browser-free direct-download mediaCapturer — see classify.go's
// MediaClass doc for why those never need a browser. ClassStreaming
// never reaches here at all: DownloadItem/DownloadPlaylist reject it
// before ever enqueuing a job (see ErrUnsupportedMediaClass's doc).
func (s *service) captureForClass(ctx context.Context, j captureJob) (*ItemRecord, error) {
	if j.class == ClassSoftware {
		return s.capturer.Capture(ctx, j.item, s.captureWindowMs)
	}
	return s.mediaCapturer.Capture(ctx, j.item)
}

// enforceDiskLimit evicts oldest-first (via evictDownTo, preferring
// anything OTHER than the just-captured item) until usage is back under
// maxDiskBytes or nothing more can be evicted. justCapturedKey/Source
// identify that item (key for store ops, source for the eviction
// notification).
//
// The just-captured item is only PREFERRED to survive, never permanently
// exempted: capture enforces a per-resource size cap
// (maxResourceBytes), never a per-item total, so a single multi-resource
// artwork can still exceed the WHOLE cache's maxDiskBytes budget on its
// own even after every older item has already been evicted. Leaving it
// alone in that case would mean maxDiskBytes is not actually a hard
// ceiling — exactly the "flip offlineCache.enabled on and it can still
// fill the disk" failure mode this budget exists to prevent (see
// OptionsFromConfig's DefaultMaxDiskBytes doc). So once
// oldestEvictableItem has nothing else left to offer, evictDownTo's
// last-resort mode deliberately evicts the just-captured item too.
func (s *service) enforceDiskLimit(justCapturedKey, justCapturedSource string) {
	if s.maxDiskBytes <= 0 {
		return
	}
	s.evictDownTo(s.maxDiskBytes, justCapturedKey, justCapturedSource, true)
}

// captureReclaimHeadroomDivisor sizes the free-space floor
// reclaimDiskForCapture guarantees before each capture:
// maxDiskBytes/8, i.e. 1.25 GiB at the DefaultMaxDiskBytes of 10 GiB.
// The value is a trade-off between two failure directions: a floor too
// small starves large multi-resource artworks of budget on a full
// store (they capture partial and need further rollover passes to
// converge), while a floor too large preemptively evicts cached items
// a bulk playlist download is going to want again minutes later —
// at 1/8th, a full store keeps ~87.5% of its working set across a
// capture while still guaranteeing room for a typical artwork.
const captureReclaimHeadroomDivisor = 8

// reclaimDiskForCapture makes room for a capture that has NOT started
// yet, evicting oldest-first until at least
// maxDiskBytes/captureReclaimHeadroomDivisor of the byte budget is
// free. It exists because both capture pipelines seed their disk
// budget with the store's REMAINING room (newDiskBudgetFromStore) and
// enforceDiskLimit only runs after a capture SUCCEEDS: without a
// pre-capture reclaim, a store that once reached maxDiskBytes rejected
// every subsequent capture up front, and with no successful captures
// the post-capture eviction never ran again — the cache froze on its
// oldest contents instead of rolling over to newer ones
// (feral-file/ffos-user#229 review finding).
//
// sourceKey (the item about to be captured) is never evicted here, with
// no last-resort fallback: for a RECAPTURE of an already-ready item,
// evicting its existing record now would flip it to not_cached while
// the replacement download is still in flight, and "this item alone
// exceeds the budget" cannot be known before its capture has even
// started — enforceDiskLimit still owns that verdict afterwards.
// Running out of victims before reaching the target is likewise a
// normal, quiet outcome here (a store whose remaining contents are
// this item itself plus playlist metadata no item eviction can reclaim),
// not an over-budget alarm: the capture then simply runs with whatever
// room is actually left, exactly as it would have before this reclaim
// existed.
func (s *service) reclaimDiskForCapture(sourceKey string) {
	if s.maxDiskBytes <= 0 {
		return
	}
	// The protected item's source is irrelevant here: with the
	// last-resort mode off, evictDownTo can never evict (and so never
	// needs to announce) the protected item itself.
	s.evictDownTo(s.maxDiskBytes-s.maxDiskBytes/captureReclaimHeadroomDivisor, sourceKey, "", false)
}

// evictDownTo is the shared eviction loop behind enforceDiskLimit
// (post-capture, target = the hard ceiling itself) and
// reclaimDiskForCapture (pre-capture, target = ceiling minus a
// headroom floor). It evicts the oldest-captured item (by CapturedAt,
// via oldestEvictableItem), preferring anything OTHER than
// protectedKey, and re-runs GC until usage is back under targetBytes or
// nothing more can be evicted. Blobs are deduped, so an item's disk
// contribution is only knowable after GC reclaims blobs no other item
// still references — hence the delete-then-GC-then-recheck loop rather
// than trying to precompute how much one eviction will free.
//
// protectedSource is protectedKey's raw source URL, needed only for the
// last-resort eviction notification below (the key is a one-way hash,
// and the record it could have been read back from is what is being
// deleted); ordinary victims carry their own source out of
// oldestEvictableItem.
//
// evictProtectedAsLastResort selects what "nothing else left" means:
//   - true (enforceDiskLimit): protectedKey is only PREFERRED to
//     survive, never permanently exempted — see enforceDiskLimit's doc
//     for why the just-captured item must ultimately be evictable too.
//     The fallback fires at most once per call (protectedKey is cleared
//     right after), so a store that is over budget with truly nothing
//     left on disk still terminates instead of looping forever trying
//     to re-evict an item that is already gone.
//   - false (reclaimDiskForCapture): missing the target is a normal
//     outcome, so the loop returns quietly without touching
//     protectedKey and without the over-budget warning — the hard
//     ceiling is enforceDiskLimit's job, not this reclaim's.
func (s *service) evictDownTo(targetBytes int64, protectedKey, protectedSource string, evictProtectedAsLastResort bool) {
	for {
		usage, err := s.store.DiskUsage()
		if err != nil {
			s.logger.Warn("offline cache: disk usage check failed", zap.Error(err))
			return
		}
		if usage <= targetBytes {
			return
		}

		victimKey, victimSource, ok := s.oldestEvictableItem(protectedKey)
		reason := ""
		if !ok {
			if !evictProtectedAsLastResort {
				return
			}
			if protectedKey == "" {
				// Already gave up the just-captured item's protection
				// above and there is STILL nothing left to evict:
				// nothing more this function can do.
				s.logger.Warn("offline cache: over disk budget with nothing left to evict",
					zap.Int64("usage_bytes", usage), zap.Int64("max_bytes", s.maxDiskBytes))
				return
			}
			victimKey, victimSource = protectedKey, protectedSource
			protectedKey = ""
			reason = "offline cache: evicted the just-captured item itself; it alone exceeds the disk budget with no older item left to evict"
		}
		if _, err := s.store.DeleteItem(victimKey); err != nil {
			s.logger.Warn("offline cache: evict item failed", zap.String("source", victimSource), zap.Error(err))
			return
		}
		s.notifyEvicted(victimSource, Coverage{Reason: reason})

		if _, _, err := s.gc(); err != nil {
			s.logger.Warn("offline cache: GC during eviction failed", zap.Error(err))
			return
		}
	}
}

// gc runs store.GC() under captureMu — see captureMu's doc. Every GC()
// call in this file must go through here rather than calling the store
// directly, or the fence it provides against an in-flight Capture is
// silently bypassed.
func (s *service) gc() (int, int64, error) {
	s.captureMu.Lock()
	defer s.captureMu.Unlock()
	return s.store.GC()
}

// oldestEvictableItem picks the eviction victim: the oldest-captured
// record other than excludeKey. It returns both the victim's key (for
// the store delete) and its source (for the eviction notification —
// already in hand here from the record read, where the caller could not
// recover it after the delete).
func (s *service) oldestEvictableItem(excludeKey string) (key, source string, ok bool) {
	keys, err := s.store.ListItemKeys()
	if err != nil {
		return "", "", false
	}

	var oldestAt time.Time
	for _, candidate := range keys {
		if candidate == excludeKey {
			continue
		}
		rec, err := s.store.LoadItem(candidate)
		if err != nil {
			continue
		}
		if !ok || rec.CapturedAt.Before(oldestAt) {
			key, source, oldestAt, ok = candidate, rec.Item.Source, rec.CapturedAt, true
		}
	}
	return key, source, ok
}

// ClearItem deletes the record for source (the raw source URL, hashed to
// its key here at the boundary) and GCs any blob it was the last
// referent of. Because the record is per-SOURCE (see ItemRecord's doc),
// this clears the cached artifact for EVERY playlist item — in any
// playlist — that shares this source, not just the one the caller had in
// hand. It returns ErrItemNotFound (wrapped) only when the source had
// nothing to clear at all — no record on disk AND nothing tracked in
// memory — matching the not_found contract documented in
// docs/controld-inbound-controller-messages.md.
//
// Any job for this source still sitting in the queue (not yet started)
// is dropped, atomically with the busy check, so it cannot silently
// resurrect the record this call just deleted once it eventually runs
// — see reserveForClear's doc for why the busy check and the queue
// removal must be one critical section rather than two separate steps.
// A capture for this source that is already ACTIVE (past the queue,
// inside capturer.Capture) is rejected outright with ErrItemBusy instead
// — see that error's doc for why: this call's GC would otherwise still
// wait for the active capture via captureMu (so it could never corrupt
// that capture's blobs), but the capture itself is not canceled, so a
// delete-then-let-GC-run approach would let the item's record
// legitimately reappear once the capture finishes.
//
// reserveForClear runs FIRST and unconditionally: for a source this
// service has never heard of, that is a harmless no-op (nothing tracked,
// nothing queued), and for one whose only trace is a not-yet-started
// first-time download it cancels that queued job — closing a related gap
// where such an item could never be canceled via ClearItem at all before,
// only a re-download of an item that already has a record.
//
// That cancellation is why "no record on disk" alone is NOT reported as
// not_found: a first-time download has no record yet, so answering
// not_found would tell the client its clear did nothing while the
// download it asked to cancel is in fact gone and the item has genuinely
// settled at not_cached. Worse, the router maps not_found to a
// NON-retryable error (see offlineCacheErrorResponse), so the client
// would keep showing the stale queued entry with no reason to re-issue.
// reserveForClear's settled set is what distinguishes that case from a
// truly unknown id.
//
// Whether a record existed is then decided by DeleteItem's own removed
// flag, NOT by a LoadItem probe in front of the delete (which is what this
// function used to do, and what ClearPlaylist still does one level up for
// the PLAYLIST record via LoadPlaylist — it needs that record's contents,
// not just its existence). The record is about to be deleted either way,
// so reading and unmarshaling it to learn only whether it exists is wasted
// work — and a record too corrupt to unmarshal would fail the probe,
// leaving the client permanently unable to clear the very record most in
// need of clearing. Do not reintroduce that probe.
//
// Both success paths emit not_cached to the observer, since the clear is
// a per-item state transition and that notification is the documented
// push mechanism for those (see the offline_cache_status notification in
// docs/controld-inbound-controller-messages.md); without it a connected
// controller keeps displaying queued/ready until it happens to poll
// getOfflineCacheStatus. It is notifyObserver, NOT notify: notify would
// write s.state[key] = not_cached, re-adding the very entry
// reserveForClear just deleted, and s.state doubles as the in-memory
// half of the known-item set (see allKnownItemKeys), so every cleared item
// would linger forever in whole-store Status pages — and grow s.state
// without bound — purely as a side effect of having been cleared.
func (s *service) ClearItem(source string) error {
	key := SourceKey(source)
	res, _, err := s.reserveForClear(map[string]bool{key: true})
	if err != nil {
		return fmt.Errorf("offline cache: clear item %s: %w", source, err)
	}
	removed, err := s.store.DeleteItem(key)
	if err != nil {
		// Knowingly silent on the observer: the record survived, so the
		// item is still whatever the record says (see itemStatus) and
		// announcing not_cached would be a lie. A queued job canceled just
		// above stays canceled, so a client that had a queued entry for it
		// keeps one until it polls — the honest alternative is re-emitting
		// the record-derived status, which is more machinery than this
		// already-failing I/O path warrants. The error itself is the
		// caller's signal to retry.
		return fmt.Errorf("offline cache: clear item %s: %w", source, err)
	}
	if !removed && !res.settled[key] {
		return fmt.Errorf("offline cache: clear item %s: %w", source, ErrItemNotFound)
	}
	// Ordered against a racing enqueue's own "queued" notification, then
	// emitted as soon as the record is gone and ahead of GC: the record is
	// what every client-visible state is derived from (see itemStatus), so
	// the item is already not_cached here whether or not the blob sweep
	// below succeeds.
	res.awaitQueuedNotifications()
	s.notifyObserver(source, StateNotCached, Coverage{})

	if !removed {
		// Cancel-only clear: this call deleted no record, so it orphaned no
		// blobs, and s.gc() would block on captureMu behind any unrelated
		// in-flight capture (see gc's doc) — a wait with nothing to collect.
		// Blobs left behind by an EARLIER failed capture are not this call's
		// to reclaim; the next clear or disk-limit eviction sweeps them.
		return nil
	}
	if _, _, err := s.gc(); err != nil {
		return fmt.Errorf("offline cache: GC after clearing item %s: %w", source, err)
	}
	return nil
}

// ClearPlaylist deletes a cached playlist's record and every one of its
// items, GCing shared blobs. See ClearItem's doc for the queued-job and
// active-capture semantics, which apply per item here too: if any item in
// the playlist is currently downloading, the whole call is rejected with
// ErrItemBusy before anything is deleted, rather than clearing everything
// else and leaving one item partially cleared (deleted here, then
// resurrected once its capture finishes) — an all-or-nothing outcome is
// far easier for a caller to reason about and retry than a partial one.
// The busy check and the queue removal below both go through
// reserveForClear as ONE call across every member item, not a
// check-loop followed by a separate mutate-loop: see that function's
// doc for why splitting them would leave a window where the worker
// could dequeue and start capturing one of these exact items in
// between, after this call's own check already passed.
func (s *service) ClearPlaylist(playlistID string) error {
	raw, err := s.store.LoadPlaylist(playlistID)
	if err != nil {
		return fmt.Errorf("offline cache: load playlist %s: %w", playlistID, err)
	}

	var playlist dp1playlist.Playlist
	if err := s.json.Unmarshal(raw, &playlist); err != nil {
		return fmt.Errorf("offline cache: parse playlist %s: %w", playlistID, err)
	}

	// One clear target per distinct SOURCE, not per playlist item: items
	// sharing a source share one record (see ItemRecord's doc), so
	// deduping here is what keeps the delete/notify loop below from
	// double-announcing a source the playlist happens to list twice. An
	// item with no source has nothing cached to clear and is skipped.
	// Note the flip side of per-source records: a source this playlist
	// shares with ANOTHER cached playlist is cleared for that playlist
	// too — records are shared with no refcount, by design.
	keys := make(map[string]bool, len(playlist.Items))
	sourceOf := make(map[string]string, len(playlist.Items))
	for _, item := range playlist.Items {
		if item.Source == "" {
			continue
		}
		key := SourceKey(item.Source)
		keys[key] = true
		sourceOf[key] = item.Source
	}
	res, busyKey, err := s.reserveForClear(keys)
	if err != nil {
		return fmt.Errorf("offline cache: clear playlist %s: item %s: %w", playlistID, sourceOf[busyKey], err)
	}
	// Before any not_cached is emitted below — see
	// awaitQueuedNotifications' doc for the inversion it prevents.
	res.awaitQueuedNotifications()

	// Every step below (each item's delete, the playlist record's own
	// delete, and GC) still runs even if an earlier one failed — one bad
	// item, or a GC hiccup, must not leave everything else undeleted too
	// — but every failure is collected rather than logged-and-swallowed.
	// store.DeleteItem already treats "record does not exist" as success
	// (Remove-if-exists), so any error it returns here is a genuine
	// deletion failure (permissions, I/O); reporting ok:true to the
	// caller while that item's record (and therefore its blobs) may
	// still be on disk would misreport what this call actually did.
	//
	// Each item that this call actually settled at not_cached gets the same
	// observer notification ClearItem sends, on the same two signals it
	// uses (a record really was removed, or in-memory state was taken away)
	// and for the same reason — see ClearItem's doc for why it is
	// notifyObserver rather than notify, and why silence would strand a
	// connected controller on a stale queued/ready entry. Both signals are
	// needed, not just settled: an item can be on disk yet untracked (a
	// record Start's rebuild could not read, or one left behind by an
	// earlier clear whose DeleteItem failed after its tracked state was
	// already dropped), and that item still transitions here. An item that
	// was neither on disk nor tracked emits nothing, which is what keeps a
	// mostly-uncached playlist from fanning out one no-op notification per
	// member item. A failed delete emits nothing either: the record — and
	// therefore a ready/partial status — is still on disk, so announcing
	// not_cached would be a lie the next getOfflineCacheStatus contradicts.
	var errs []error
	// Iterate the playlist's own item order (deduped via done) rather
	// than the keys map, so deletions and notifications keep a
	// deterministic, playlist-shaped order.
	done := make(map[string]bool, len(keys))
	for _, item := range playlist.Items {
		if item.Source == "" {
			continue
		}
		key := SourceKey(item.Source)
		if done[key] {
			continue
		}
		done[key] = true
		removed, err := s.store.DeleteItem(key)
		if err != nil {
			errs = append(errs, fmt.Errorf("delete item %s: %w", item.Source, err))
			continue
		}
		if removed || res.settled[key] {
			s.notifyObserver(item.Source, StateNotCached, Coverage{})
		}
	}
	// Bump the playlist clear-epoch and delete the record as ONE critical
	// section under playlistRecordMu, so a DownloadPlaylist /
	// IndexPlaylistForOfflineDisplay still in its classify/resolve window
	// cannot slip its (epoch-check + SavePlaylist) between the bump and
	// the delete and resurrect the record — see playlistRecordMu's doc.
	// The per-item reserveForClear above already invalidated the items
	// themselves; this covers the playlist index record.
	func() {
		s.playlistRecordMu.Lock()
		defer s.playlistRecordMu.Unlock()
		s.markPlaylistCleared(playlistID)
		if err := s.store.DeletePlaylist(playlistID); err != nil {
			errs = append(errs, fmt.Errorf("delete playlist record: %w", err))
		}
	}()
	if _, _, err := s.gc(); err != nil {
		errs = append(errs, fmt.Errorf("GC: %w", err))
	}
	if len(errs) > 0 {
		return fmt.Errorf("offline cache: clear playlist %s: %w", playlistID, errors.Join(errs...))
	}
	return nil
}

func (s *service) CachedPlaylistForURL(sourceURL string) (json.RawMessage, error) {
	playlistID, err := s.store.LoadPlaylistIDForURL(sourceURL)
	if err != nil {
		return nil, err
	}
	return s.store.LoadPlaylist(playlistID)
}

func (s *service) Status(req StatusRequest) (StatusSnapshot, error) {
	// keys drive the walk (sorted, fixed-length — the paging domain);
	// requestedSource preserves the caller's own source strings so an
	// entry can still report a source for a key the store has never seen
	// (a not_cached answer for an explicitly asked-about source). For the
	// whole-store case it stays nil: every known key recovers its source
	// from the record on disk or from sourceByKey (in-flight items).
	keys, requestedSource := sortedUniqueSourceKeys(req.Sources)
	if len(keys) == 0 {
		var err error
		keys, err = s.allKnownItemKeys()
		if err != nil {
			return StatusSnapshot{}, fmt.Errorf("offline cache: list known items: %w", err)
		}
	}

	limit := req.Limit
	if limit <= 0 || limit > MaxStatusItems {
		limit = MaxStatusItems
	}
	// TotalsOnly is defined as a first-page request (see its doc):
	// honoring a cursor as well would ask for the totals of a set while
	// skipping part of it, which is not a meaningful answer. The command
	// layer rejects the combination outright; this keeps a direct Go
	// caller from getting a silently partial total.
	cursor := req.Cursor
	if req.TotalsOnly {
		cursor = ""
	}
	// Every key here sorts strictly above "", so an empty cursor selects
	// the whole set without any special-casing below.
	withTotals := cursor == ""

	snapshot := StatusSnapshot{Items: []ItemStatus{}}
	if !req.TotalsOnly {
		snapshot.Items = make([]ItemStatus, 0, min(limit, len(keys)))
	}
	var totals StatusTotals

	countTotals := func(item ItemStatus) {
		totals.Total++
		switch item.State {
		case StateReady:
			totals.Ready++
		case StateQueued, StateDownloading:
			totals.Downloading++
		case StateFailed, StateBrokenOnline:
			totals.Failed++
		}
	}

	// lastKey mirrors the key of the last entry appended to
	// snapshot.Items, for NextCursor — the entry itself only carries the
	// source, and paging runs on keys.
	lastKey := ""
	for _, key := range keys {
		// Only reachable on a continuation page (see withTotals): keys
		// already delivered cost nothing here, which is what keeps a
		// paged walk from re-reading the whole store per page.
		if key <= cursor {
			continue
		}
		pageFull := req.TotalsOnly || len(snapshot.Items) == limit
		if pageFull {
			if !withTotals {
				// Nothing left to compute for this page, and keys is
				// sorted, so everything after this point is out of
				// scope too.
				snapshot.Truncated = true
				break
			}
			if !req.TotalsOnly {
				snapshot.Truncated = true
			}
			// Cheap read: this key is only here to be counted, so skip
			// the per-blob stats recordBytes would do.
			if item, ok := s.itemStatus(key, requestedSource[key], false); ok {
				countTotals(item)
			}
			continue
		}

		item, ok := s.itemStatus(key, requestedSource[key], true)
		if !ok {
			// No source recoverable for this key (see itemStatus): the
			// entry would be unmatchable by any client, so it is
			// dropped from the page rather than reported with an empty
			// identity.
			continue
		}
		snapshot.Items = append(snapshot.Items, item)
		lastKey = key
		if withTotals {
			countTotals(item)
		}
	}

	if snapshot.Truncated {
		snapshot.NextCursor = lastKey
	}
	if withTotals {
		snapshot.Totals = &totals
		usage, err := s.store.DiskUsage()
		if err != nil {
			// Reported as 0 rather than omitted so the first page's
			// shape never depends on a transient store error; the
			// warning is what makes the 0 diagnosable.
			s.logger.Warn("offline cache: failed to measure disk usage for status", zap.Error(err))
		}
		snapshot.DiskUsedBytes = &usage
	}
	return snapshot, nil
}

// sortedUniqueSourceKeys normalizes a caller-supplied source list into
// the same key ordering allKnownItemKeys produces, which is what lets
// StatusRequest.Cursor mean "the first key strictly greater than this"
// for both the explicit-sources and whole-store cases. The second return
// maps each key back to the caller's own source string, so Status can
// still report a source for a key the store has never seen. Empty
// sources are dropped rather than reported as not_cached entries nobody
// asked for; duplicate sources collapse onto one key.
func sortedUniqueSourceKeys(sources []string) ([]string, map[string]string) {
	if len(sources) == 0 {
		return nil, nil
	}
	byKey := make(map[string]string, len(sources))
	keys := make([]string, 0, len(sources))
	for _, source := range sources {
		if source == "" {
			continue
		}
		key := SourceKey(source)
		if _, dup := byKey[key]; dup {
			continue
		}
		byKey[key] = source
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys, byKey
}

// allKnownItemKeys unions on-disk items with anything tracked in memory
// (e.g. a just-queued item that has not written its record yet).
// The union is sorted as a whole, not as an on-disk run followed by an
// in-flight run: Status pages by "key strictly greater than the cursor",
// which silently skips entries unless the sequence is globally ordered.
func (s *service) allKnownItemKeys() ([]string, error) {
	diskKeys, err := s.store.ListItemKeys()
	if err != nil {
		return nil, err
	}
	seen := make(map[string]struct{}, len(diskKeys))
	keys := make([]string, 0, len(diskKeys))
	for _, key := range diskKeys {
		seen[key] = struct{}{}
		keys = append(keys, key)
	}

	s.mu.RLock()
	for key := range s.state {
		if _, ok := seen[key]; !ok {
			seen[key] = struct{}{}
			keys = append(keys, key)
		}
	}
	s.mu.RUnlock()

	sort.Strings(keys)
	return keys, nil
}

// itemStatus builds one snapshot entry for key. sourceHint is the
// caller-supplied source for the key (from an explicit StatusRequest.
// Sources entry), used only when neither the record on disk nor
// sourceByKey can supply one; ok=false means no source could be
// recovered at all — a record too corrupt to read for a key nobody
// asked about explicitly — and the entry should be dropped rather than
// reported without the one field clients match on (GC's quarantine pass
// owns that record's fate).
//
// withBytes controls whether the Bytes field is populated, which costs
// one store stat per unique blob this item references (recordBytes) —
// by far the most expensive part of a large snapshot. Callers that only
// need the item's state for totals pass false, and must not put the
// result on the wire: its Bytes would read as 0 rather than as "not
// measured".
func (s *service) itemStatus(key, sourceHint string, withBytes bool) (ItemStatus, bool) {
	s.mu.RLock()
	trackedState, tracked := s.state[key]
	if src, ok := s.sourceByKey[key]; ok {
		// Tracked entries always know their source — sourceByKey is kept
		// in lockstep with state (see its doc) — and it wins over the
		// hint: both are the same string whenever both exist. The ok
		// check (rather than an unconditional overwrite) only matters if
		// a future edit ever breaks that lockstep: an explicit-source
		// query should then still fall back to the caller's own hint
		// instead of emitting an entry with no identity at all.
		sourceHint = src
	}
	s.mu.RUnlock()

	rec, err := s.store.LoadItem(key)
	if err != nil {
		// No record to read the source back from, so sourceHint is the
		// only identity this entry can carry. Checked once for BOTH
		// branches below: an entry with an empty source is unmatchable by
		// any client (it is the field they key on — see ItemStatus.Source),
		// so dropping it from the page is the honest outcome, and the
		// tracked branch must not be the one place that emits one anyway.
		// Unreachable today for a tracked key — sourceByKey is in lockstep
		// with state — which is exactly why the guard belongs here rather
		// than duplicated into the untracked branch alone: it holds if a
		// future edit ever breaks that lockstep.
		if sourceHint == "" {
			return ItemStatus{}, false
		}
		if tracked {
			// Queued/downloading (or failed with no prior successful
			// capture) items have no record on disk yet.
			return ItemStatus{Source: sourceHint, State: trackedState, Percent: percentForState(trackedState)}, true
		}
		return ItemStatus{Source: sourceHint, State: StateNotCached}, true
	}

	// A record on disk always wins over in-memory state: e.g. a failed
	// re-download attempt over an existing ready/partial capture should
	// still report that earlier successful capture, not "failed".
	state := stateFromCoverage(rec.Coverage)
	status := ItemStatus{
		Source:           rec.Item.Source,
		State:            state,
		Percent:          percentForState(state),
		CoverageComplete: rec.Coverage.Complete,
		Reason:           truncateReason(rec.Coverage.Reason),
	}
	if withBytes {
		status.Bytes = s.recordBytes(rec)
	}
	return status, true
}

// percentForState is coarse (0 or 100): capture is a single bounded-window
// operation, not chunked, so there is no meaningful mid-capture progress
// to report. StateBrokenOnline belongs at 100, not 0, alongside
// StateReady/StatePartial: it means the capture window already finished
// (see stateFromCoverage) — the artwork itself is what is broken (a
// CSP-blocked asset that would fail even online), not the download.
// StateFailed is the only terminal-but-not-"done" case: it has no prior
// successful capture on disk at all (see ItemStatus's doc), so there is
// nothing captured to call 100% of.
func percentForState(state ItemState) int {
	switch state {
	case StateReady, StatePartial, StateBrokenOnline:
		return 100
	default:
		return 0
	}
}

func (s *service) recordBytes(rec *ItemRecord) int64 {
	var total int64
	seen := make(map[string]struct{}, len(rec.Resources))
	for _, res := range rec.Resources {
		if res.SHA256 == "" {
			continue
		}
		if _, dup := seen[res.SHA256]; dup {
			continue
		}
		seen[res.SHA256] = struct{}{}
		if size, err := s.store.BlobSize(res.SHA256); err == nil {
			total += size
		}
	}
	return total
}

// notifyEvicted is notify's eviction-only variant: it records the victim
// as not_cached UNLESS that source still has a job scheduled, in which
// case the tracked state is left alone and nothing is announced.
//
// Eviction is the one notify caller that writes a state DOWNWARD for an
// item it is not itself processing, and a victim can legitimately be a
// source whose stale record is the oldest on disk while a recapture for
// it sits in the queue. Letting the plain notify run there overwrote
// StateQueued with StateNotCached and broke two contracts at once:
// enqueue's idempotency check stopped seeing the job as scheduled, so
// the next request for that source pushed a DUPLICATE job and the same
// artwork was captured twice (the cross-request "one capture" contract
// in docs/offline-artwork-capture.md §5); and reserveForClear stopped
// counting it as settled, so a ClearItem that really did cancel the
// queued job answered a NON-retryable ErrItemNotFound.
//
// The check and the write are ONE critical section under the same s.mu
// enqueue commits StateQueued under, so there is no window where a
// concurrent enqueue can slip between them — which is why this is a
// compare-and-set here rather than a filter in oldestEvictableItem's
// victim scan. Filtering the scan instead would also be actively harmful:
// DownloadPlaylist enqueues every classified item, so a playlist refresh
// marks nearly every record in the store in-flight, and skipping them all
// would leave the pre-capture reclaim with no victims exactly when the
// store is full — starving each capture of budget, which the software
// path records as an all-resources-over-budget partial that OVERWRITES
// the previously complete record. Evicting the stale bytes is correct;
// only the state downgrade is not.
//
// The item's own capture still announces its real terminal state when it
// finishes, so nothing is lost by staying quiet here: the record this
// call deleted is one the queued job is about to replace.
func (s *service) notifyEvicted(source string, coverage Coverage) {
	key := SourceKey(source)
	s.mu.Lock()
	if st, tracked := s.state[key]; tracked && (st == StateQueued || st == StateDownloading) {
		s.mu.Unlock()
		return
	}
	s.state[key] = StateNotCached
	s.sourceByKey[key] = source
	s.mu.Unlock()
	s.notifyObserver(source, StateNotCached, coverage)
}

func (s *service) notify(source string, state ItemState, coverage Coverage) {
	key := SourceKey(source)
	s.mu.Lock()
	s.state[key] = state
	s.sourceByKey[key] = source
	s.mu.Unlock()
	s.notifyObserver(source, state, coverage)
}

// notifyObserver is notify's callout-only half, split out so
// dequeueForProcessing/process can write s.state atomically (in
// dequeueForProcessing's own critical section, paired with the queue
// pop) and still send the exact same observer notification notify
// would have sent, without notify's redundant second state write.
// Never called while s.mu is held: the observer may be a websocket
// send with no bound on how long it takes, and holding s.mu across
// that would block every other state read/write (reserveForClear,
// dequeueForProcessing, ...) for as long as it runs.
//
// Takes the raw source, not a key: every caller has it in hand (the
// job's item, the clear request, the eviction victim's record), and it
// is what the notification carries on the wire (ItemStatus.Source).
func (s *service) notifyObserver(source string, state ItemState, coverage Coverage) {
	if s.observer == nil {
		return
	}
	s.observer.OnItemStateChanged(ItemStatus{
		Source:           source,
		State:            state,
		Percent:          percentForState(state),
		CoverageComplete: coverage.Complete,
		// Truncated for the same reason itemStatus truncates: this
		// ItemStatus goes straight out over the relayer and the hub
		// WebSocket, both of which bound how long a single write may
		// take, not how large it may be.
		Reason: truncateReason(coverage.Reason),
	})
}
