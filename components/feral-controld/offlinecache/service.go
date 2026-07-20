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

	dp1playlist "github.com/display-protocol/dp1-go/playlist"
	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

// ErrUnsupportedMediaClass is returned by DownloadItem when the item's
// source does not classify as software (see classify.go): offline caching
// only supports software-based DP-1 artworks per the plan's constraints.
var ErrUnsupportedMediaClass = errors.New("offline cache: item is not software-based and cannot be downloaded offline")

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

// ItemStatus is one entry of a Status snapshot, shaped for the
// getOfflineCacheStatus command and offline_cache_status notification.
type ItemStatus struct {
	ItemID string    `json:"itemId"`
	State  ItemState `json:"state"`
	// Percent is coarse (0 or 100): capture is a single bounded-window
	// operation, not chunked, so there is no meaningful mid-download
	// progress to report beyond "queued/downloading" vs "done".
	Percent          int    `json:"percent"`
	Bytes            int64  `json:"bytes,omitempty"`
	CoverageComplete bool   `json:"coverageComplete,omitempty"`
	Reason           string `json:"reason,omitempty"`
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
	Items  []ItemStatus `json:"items"`
	Totals StatusTotals `json:"totals"`
	// DiskUsedBytes is named diskUsed on the wire to match the plan's
	// documented command response shape (section 5).
	DiskUsedBytes int64 `json:"diskUsed"`
}

// ProgressObserver is notified on every item state transition. Keeping this
// as a small consumer-owned interface (rather than importing relayer/ws
// directly) lets the notification-wiring layer push offline_cache_status
// over relayer + hub WS without offlinecache depending on those transport
// packages — see AGENTS.md's "communicate through visible boundaries"
// principle.
//
//go:generate mockgen -source=service.go -destination=../mocks/offlinecache_service.go -package=mocks -mock_names=Service=MockOfflineCacheService,ProgressObserver=MockOfflineCacheProgressObserver
type ProgressObserver interface {
	OnItemStateChanged(status ItemStatus)
}

// Service is the public API used by commandrouter: download one item or a
// whole playlist (software items only), clear caches, and report status.
// It owns a job queue that serializes captures to one at a time — the
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
	// software-classified item it contains. total counts all items in the
	// playlist; queued counts only those actually enqueued.
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
	DownloadPlaylist(ctx context.Context, playlistRaw json.RawMessage) (queued, total int, err error)
	ClearItem(itemID string) error
	ClearPlaylist(playlistID string) error
	// Status reports on itemIDs, or on every item this process knows about
	// (on-disk + in-flight) when itemIDs is empty.
	Status(itemIDs []string) (StatusSnapshot, error)
}

type captureJob struct {
	itemID string
	item   dp1playlist.PlaylistItem
}

// jobQueue is an unbounded FIFO so DownloadItem/DownloadPlaylist never
// block the caller waiting for queue capacity, even for a large playlist.
// wake is a 1-buffered signal channel, not a data channel, so pop() always
// reads from items under the mutex instead of racing two sources of truth.
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

// removeItems drops any not-yet-started job for one of ids. Used by
// ClearItem/ClearPlaylist so a clear that races an item still sitting in
// the queue (as opposed to actively capturing — see service.captureMu)
// does not get silently undone once that queued job eventually runs and
// calls SaveItem again.
func (q *jobQueue) removeItems(ids map[string]bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	kept := q.items[:0]
	for _, j := range q.items {
		if !ids[j.itemID] {
			kept = append(kept, j)
		}
	}
	q.items = kept
}

type service struct {
	store      Store
	classifier Classifier
	capturer   Capturer
	json       wrapper.JSON
	logger     *zap.Logger

	captureWindowMs int
	maxDiskBytes    int64 // <=0 means unlimited
	observer        ProgressObserver

	queue *jobQueue

	// captureMu fences store.GC() sweeps against an in-flight Capture.
	// capturer.Capture (capture.go) writes blobs to the store one
	// resource at a time as it observes them, and only calls
	// store.SaveItem once at the very end — so for the whole span of a
	// capture there can be freshly-written blobs on disk that no saved
	// item record yet references. GC() treats "not referenced by any
	// saved item" as "orphan, delete it" (store.go), so a GC that runs
	// concurrently with that window (from ClearItem/ClearPlaylist, called
	// directly from commandrouter on a different goroutine than the
	// single capture worker) can delete another, unrelated item's
	// in-progress capture out from under it before its record is ever
	// saved. Holding captureMu for the full Capture call and every GC()
	// call closes that window: a clear that races an active capture
	// simply waits for it to finish saving before sweeping.
	captureMu sync.Mutex

	mu    sync.RWMutex
	state map[string]ItemState // itemID -> last-known state, seeded from disk on Start

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
func NewService(
	store Store,
	classifier Classifier,
	capturer Capturer,
	jsonWrapper wrapper.JSON,
	captureWindowMs int,
	maxDiskBytes int64,
	observer ProgressObserver,
	logger *zap.Logger,
) Service {
	return &service{
		store:           store,
		classifier:      classifier,
		capturer:        capturer,
		json:            jsonWrapper,
		logger:          logger,
		captureWindowMs: captureWindowMs,
		maxDiskBytes:    maxDiskBytes,
		observer:        observer,
		queue:           newJobQueue(),
		state:           make(map[string]ItemState),
	}
}

func (s *service) Start(ctx context.Context) error {
	ids, err := s.store.ListItemIDs()
	if err != nil {
		return fmt.Errorf("offline cache: rebuild index: %w", err)
	}

	s.mu.Lock()
	for _, id := range ids {
		rec, err := s.store.LoadItem(id)
		if err != nil {
			s.logger.Warn("offline cache: skipping unreadable item record on startup",
				zap.String("item_id", id), zap.Error(err))
			continue
		}
		s.state[id] = stateFromCoverage(rec.Coverage)
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
	for {
		j, ok := s.queue.pop()
		if !ok {
			select {
			case <-s.queue.wake:
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
				for {
					j, ok := s.queue.pop()
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
	if item.ID == "" || item.Source == "" {
		return errors.New("offline cache: item must have an id and a source")
	}

	class, err := s.classifier.Classify(ctx, item.Source)
	if err != nil {
		return fmt.Errorf("offline cache: classify %s: %w", item.Source, err)
	}
	if class != ClassSoftware {
		return ErrUnsupportedMediaClass
	}

	// Classify above can be slow (network I/O), so the started.Load()
	// fast-fail at the top of this function is only advisory: a Stop()
	// racing this call could tear the worker down in between. enqueue
	// re-checks started under the same lock Stop uses to flip it, so
	// this is the authoritative check that actually prevents pushing a
	// job onto a queue nobody will ever drain.
	if !s.enqueue(item) {
		return ErrServiceNotStarted
	}
	return nil
}

func (s *service) DownloadPlaylist(ctx context.Context, playlistRaw json.RawMessage) (int, int, error) {
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

	if err := s.store.SavePlaylist(playlist.ID, playlistRaw); err != nil {
		return 0, 0, fmt.Errorf("offline cache: save playlist %s: %w", playlist.ID, err)
	}

	total := len(playlist.Items)
	queued := 0
	for _, item := range playlist.Items {
		if item.ID == "" || item.Source == "" {
			continue
		}
		class, err := s.classifier.Classify(ctx, item.Source)
		if err != nil {
			s.logger.Warn("offline cache: classify failed while queuing playlist, skipping item",
				zap.String("item_id", item.ID), zap.Error(err))
			continue
		}
		if class != ClassSoftware {
			continue
		}
		if !s.enqueue(item) {
			// Stop() raced in mid-loop; the remaining items would fail
			// the same way, so stop trying rather than log once per item.
			break
		}
		queued++
	}
	return queued, total, nil
}

// enqueue is idempotent: an item already queued or downloading is left
// alone rather than double-scheduled. Returns false without touching the
// queue if the service was stopped concurrently — see the started field's
// doc comment for why the fresh check in phase 2 below (rather than the
// caller's earlier started.Load(), or even this function's own advisory
// check up front) is the one that actually matters.
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
func (s *service) enqueue(item dp1playlist.PlaylistItem) bool {
	if !s.started.Load() {
		return false
	}
	s.mu.RLock()
	st, tracked := s.state[item.ID]
	s.mu.RUnlock()
	if tracked && (st == StateQueued || st == StateDownloading) {
		return true
	}

	if s.observer != nil {
		s.observer.OnItemStateChanged(ItemStatus{
			ItemID:  item.ID,
			State:   StateQueued,
			Percent: percentForState(StateQueued),
		})
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
		return false
	}
	if st, ok := s.state[item.ID]; ok && (st == StateQueued || st == StateDownloading) {
		s.mu.Unlock()
		return true
	}
	s.state[item.ID] = StateQueued
	s.queue.push(captureJob{itemID: item.ID, item: item})
	s.mu.Unlock()
	return true
}

func (s *service) process(ctx context.Context, j captureJob) {
	s.notify(j.itemID, StateDownloading, Coverage{})

	// Held for the full Capture call, not just its final SaveItem — see
	// captureMu's doc on why the whole blob-writing window must be
	// fenced, not only the save.
	s.captureMu.Lock()
	rec, err := s.capturer.Capture(ctx, j.item, s.captureWindowMs)
	s.captureMu.Unlock()
	if err != nil {
		s.logger.Warn("offline cache: capture failed", zap.String("item_id", j.itemID), zap.Error(err))
		s.notify(j.itemID, StateFailed, Coverage{Reason: err.Error()})
		return
	}

	s.notify(j.itemID, stateFromCoverage(rec.Coverage), rec.Coverage)
	s.enforceDiskLimit(j.itemID)
}

// enforceDiskLimit evicts the oldest-captured item (by CapturedAt),
// excluding justCapturedID, and re-runs GC until usage is back under
// maxDiskBytes or nothing more can be evicted. Blobs are deduped, so an
// item's disk contribution is only knowable after GC reclaims blobs no
// other item still references — hence the delete-then-GC-then-recheck loop
// rather than trying to precompute how much one eviction will free.
func (s *service) enforceDiskLimit(justCapturedID string) {
	if s.maxDiskBytes <= 0 {
		return
	}

	for {
		usage, err := s.store.DiskUsage()
		if err != nil {
			s.logger.Warn("offline cache: disk usage check failed", zap.Error(err))
			return
		}
		if usage <= s.maxDiskBytes {
			return
		}

		victim, ok := s.oldestEvictableItem(justCapturedID)
		if !ok {
			s.logger.Warn("offline cache: over disk budget with nothing left to evict",
				zap.Int64("usage_bytes", usage), zap.Int64("max_bytes", s.maxDiskBytes))
			return
		}
		if err := s.store.DeleteItem(victim); err != nil {
			s.logger.Warn("offline cache: evict item failed", zap.String("item_id", victim), zap.Error(err))
			return
		}
		s.notify(victim, StateNotCached, Coverage{})

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

func (s *service) oldestEvictableItem(excludeID string) (string, bool) {
	ids, err := s.store.ListItemIDs()
	if err != nil {
		return "", false
	}

	var oldestID string
	var oldestAt time.Time
	found := false
	for _, id := range ids {
		if id == excludeID {
			continue
		}
		rec, err := s.store.LoadItem(id)
		if err != nil {
			continue
		}
		if !found || rec.CapturedAt.Before(oldestAt) {
			oldestID, oldestAt, found = id, rec.CapturedAt, true
		}
	}
	return oldestID, found
}

// ClearItem deletes itemID's record and GCs any blob it was the last
// referent of. It returns ErrItemNotFound (wrapped) if itemID has no
// record on disk, matching ClearPlaylist's existing not-cached behavior
// and the not_found contract documented in
// docs/controld-inbound-controller-messages.md — store.DeleteItem itself
// stays a low-level idempotent primitive (Remove-if-exists), so the
// existence check happens here via LoadItem, the same way ClearPlaylist
// already checks via LoadPlaylist.
//
// Any job for itemID still sitting in the queue (not yet started) is
// dropped so it cannot silently resurrect the record this call just
// deleted once it eventually runs. A capture for itemID that is already
// ACTIVE (past the queue, inside capturer.Capture) is a narrower,
// accepted edge case: this call's GC still waits for it via captureMu
// (so it can never corrupt that capture's blobs), but the capture is not
// canceled, so its record will legitimately reappear once it finishes.
// Canceling an in-flight capture would need per-job cancellation plumbed
// through the single-worker queue, which is a larger change than this
// fix's scope; the corruption bug (an unrelated capture's blobs getting
// GC'd out from under it) is what captureMu closes.
func (s *service) ClearItem(itemID string) error {
	if _, err := s.store.LoadItem(itemID); err != nil {
		return fmt.Errorf("offline cache: clear item %s: %w", itemID, err)
	}
	if err := s.store.DeleteItem(itemID); err != nil {
		return fmt.Errorf("offline cache: clear item %s: %w", itemID, err)
	}

	s.mu.Lock()
	delete(s.state, itemID)
	s.mu.Unlock()
	s.queue.removeItems(map[string]bool{itemID: true})

	if _, _, err := s.gc(); err != nil {
		return fmt.Errorf("offline cache: GC after clearing item %s: %w", itemID, err)
	}
	return nil
}

// ClearPlaylist deletes a cached playlist's record and every one of its
// items, GCing shared blobs. See ClearItem's doc for the queued-job and
// active-capture semantics, which apply per item here too.
func (s *service) ClearPlaylist(playlistID string) error {
	raw, err := s.store.LoadPlaylist(playlistID)
	if err != nil {
		return fmt.Errorf("offline cache: load playlist %s: %w", playlistID, err)
	}

	var playlist dp1playlist.Playlist
	if err := s.json.Unmarshal(raw, &playlist); err != nil {
		return fmt.Errorf("offline cache: parse playlist %s: %w", playlistID, err)
	}

	itemIDs := make(map[string]bool, len(playlist.Items))
	s.mu.Lock()
	for _, item := range playlist.Items {
		delete(s.state, item.ID)
		itemIDs[item.ID] = true
	}
	s.mu.Unlock()
	s.queue.removeItems(itemIDs)

	for _, item := range playlist.Items {
		if err := s.store.DeleteItem(item.ID); err != nil {
			s.logger.Warn("offline cache: failed to delete playlist item, continuing",
				zap.String("playlist_id", playlistID), zap.String("item_id", item.ID), zap.Error(err))
		}
	}

	if err := s.store.DeletePlaylist(playlistID); err != nil {
		return fmt.Errorf("offline cache: delete playlist %s: %w", playlistID, err)
	}
	if _, _, err := s.gc(); err != nil {
		return fmt.Errorf("offline cache: GC after clearing playlist %s: %w", playlistID, err)
	}
	return nil
}

func (s *service) Status(itemIDs []string) (StatusSnapshot, error) {
	ids := itemIDs
	if len(ids) == 0 {
		var err error
		ids, err = s.allKnownItemIDs()
		if err != nil {
			return StatusSnapshot{}, fmt.Errorf("offline cache: list known items: %w", err)
		}
	}

	snapshot := StatusSnapshot{Items: make([]ItemStatus, 0, len(ids))}
	for _, id := range ids {
		item := s.itemStatus(id)
		snapshot.Items = append(snapshot.Items, item)
		snapshot.Totals.Total++
		switch item.State {
		case StateReady:
			snapshot.Totals.Ready++
		case StateQueued, StateDownloading:
			snapshot.Totals.Downloading++
		case StateFailed, StateBrokenOnline:
			snapshot.Totals.Failed++
		}
	}

	if usage, err := s.store.DiskUsage(); err == nil {
		snapshot.DiskUsedBytes = usage
	}
	return snapshot, nil
}

// allKnownItemIDs unions on-disk items with anything tracked in memory
// (e.g. a just-queued item that has not written items/<id>.json yet).
func (s *service) allKnownItemIDs() ([]string, error) {
	diskIDs, err := s.store.ListItemIDs()
	if err != nil {
		return nil, err
	}
	seen := make(map[string]struct{}, len(diskIDs))
	ids := make([]string, 0, len(diskIDs))
	for _, id := range diskIDs {
		seen[id] = struct{}{}
		ids = append(ids, id)
	}

	s.mu.RLock()
	var inflight []string
	for id := range s.state {
		if _, ok := seen[id]; !ok {
			inflight = append(inflight, id)
		}
	}
	s.mu.RUnlock()

	sort.Strings(inflight)
	return append(ids, inflight...), nil
}

func (s *service) itemStatus(itemID string) ItemStatus {
	s.mu.RLock()
	trackedState, tracked := s.state[itemID]
	s.mu.RUnlock()

	rec, err := s.store.LoadItem(itemID)
	if err != nil {
		if tracked {
			// Queued/downloading (or failed with no prior successful
			// capture) items have no record on disk yet.
			return ItemStatus{ItemID: itemID, State: trackedState, Percent: percentForState(trackedState)}
		}
		return ItemStatus{ItemID: itemID, State: StateNotCached}
	}

	// A record on disk always wins over in-memory state: e.g. a failed
	// re-download attempt over an existing ready/partial capture should
	// still report that earlier successful capture, not "failed".
	state := stateFromCoverage(rec.Coverage)
	return ItemStatus{
		ItemID:           itemID,
		State:            state,
		Percent:          percentForState(state),
		Bytes:            s.recordBytes(rec),
		CoverageComplete: rec.Coverage.Complete,
		Reason:           rec.Coverage.Reason,
	}
}

func percentForState(state ItemState) int {
	switch state {
	case StateReady, StatePartial:
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

func (s *service) notify(itemID string, state ItemState, coverage Coverage) {
	s.mu.Lock()
	s.state[itemID] = state
	s.mu.Unlock()

	if s.observer == nil {
		return
	}
	s.observer.OnItemStateChanged(ItemStatus{
		ItemID:           itemID,
		State:            state,
		Percent:          percentForState(state),
		CoverageComplete: coverage.Complete,
		Reason:           coverage.Reason,
	})
}
