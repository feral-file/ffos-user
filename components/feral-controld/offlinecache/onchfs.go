package offlinecache

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"path"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	dp1playlist "github.com/display-protocol/dp1-go/playlist"
	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-controld/contentpath"
	"github.com/feral-file/ffos-user/components/feral-controld/onchfs"
)

// The onchfs materializer is the offline cache's second producer of item
// records (ffos-user#427). The first, capture, loads a work in a headless
// browser from its host; an onchfs work has no host since fxhash closed, so
// this one reads the work's tree from chain (onchfs.Resolver, every inode
// verified against its address) and writes it into the same store, under
// the work's device-local source (contentpath.Onchfs.SourceURL). Replay then
// serves it to the kiosk exactly as it serves a captured work: one path
// serves local artwork, whichever producer filled it.
//
// The record is complete by construction (every file of the tree, verified)
// and written as an Automatic record with every resource fresh for
// onchfsFreshFor: Automatic keeps it out of the app's Downloads list (it is
// not something the user downloaded) and lets the cache's own policies evict
// it like any automatic warm; it is rebuilt on the next cast that needs it.
//
// What the kiosk will ask for, and the resources that answer it:
//   - each file at <origin>/<escaped path>;
//   - each directory holding an index.html at <origin>/<dir>/ (the root at
//     <origin>/), as an onchfs HTTP proxy serves a directory;
//   - the exact source URL, query included (fxhash works read their seed
//     from it), as the entry document. Replay matches method and URL
//     exactly, so this one is stored under the full string.
//
// A sub-resource requested with a query string of its own (`lib.js?v=2`)
// misses: replay matches exactly and the tree has no such URL. Known
// limitation, not seen in the recorded works.

// onchfsFreshFor is how long a materialized record stays replayable. The
// bytes cannot change under their address; expiry only bounds how long an
// unplayed work occupies the disk before the cache may drop it.
const onchfsFreshFor = 30 * 24 * time.Hour

// onchfsRefreshWithin re-materializes a record this close to expiry, so a
// work in rotation never lapses mid-playlist.
const onchfsRefreshWithin = 3 * 24 * time.Hour

// DefaultOnchfsStoreBytes bounds the onchfs records a device keeps when the
// offline cache feature is off (the materializer is then the store's only
// writer and nothing else evicts). With the feature on, the cache's own disk
// policy bounds them along with everything else.
const DefaultOnchfsStoreBytes = 512 << 20

// onchfsJobTimeout bounds one work's read and write. The largest recorded
// works read in under 5 s; this leaves room for a slow node and failover.
const onchfsJobTimeout = 2 * time.Minute

// onchfsConcurrentJobs bounds works read at once, foreground and background
// together; onchfsBackgroundJobs of them may be background reads. The slot
// background work can never take is what keeps a cast's first work from
// waiting behind slow background reads (PR #461 review F13) without letting
// casts add unbounded reads on top (F14). Each read fans out to its own
// chain reads (onchfs.DefaultLimits.ConcurrentReads) and keeps at most
// onchfs.MaxTreeBytes.
const (
	onchfsConcurrentJobs = 3
	onchfsBackgroundJobs = 2
)

// onchfsAdmission hands out read slots: a foreground job takes any free
// slot and goes before every waiting background job; a background job takes
// one only while fewer than onchfsBackgroundJobs are busy and no foreground
// job waits. A job promoted while it waits is re-evaluated at once.
type onchfsAdmission struct {
	mu        sync.Mutex
	cond      *sync.Cond
	running   int
	waitingFG int
}

func newOnchfsAdmission(base context.Context) *onchfsAdmission {
	a := &onchfsAdmission{}
	a.cond = sync.NewCond(&a.mu)
	context.AfterFunc(base, a.wake)
	return a
}

func (a *onchfsAdmission) wake() {
	a.mu.Lock()
	a.cond.Broadcast()
	a.mu.Unlock()
}

// acquire waits for a slot for j, or until ctx ends (false).
func (a *onchfsAdmission) acquire(ctx context.Context, j *onchfsJob) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	counted := false
	defer func() {
		if counted {
			a.waitingFG--
		}
	}()
	for ctx.Err() == nil {
		fg := j.foreground.Load()
		if fg && !counted {
			a.waitingFG++
			counted = true
		}
		if (fg && a.running < onchfsConcurrentJobs) ||
			(!fg && a.running < onchfsBackgroundJobs && a.waitingFG == 0) {
			a.running++
			return true
		}
		a.cond.Wait()
	}
	return false
}

func (a *onchfsAdmission) release() {
	a.mu.Lock()
	a.running--
	a.cond.Broadcast()
	a.mu.Unlock()
}

// OnchfsResolver reads a verified onchfs tree (onchfs.Resolver).
type OnchfsResolver interface {
	Resolve(ctx context.Context, root, chain string) (*onchfs.Tree, error)
}

// OnchfsMaterializer writes onchfs works into the store. Safe for concurrent
// use.
type OnchfsMaterializer struct {
	store    Store
	resolver OnchfsResolver
	logger   *zap.Logger
	now      func() time.Time
	// base is the daemon's lifetime: a job outlives the cast that started
	// it and ends with the daemon.
	base context.Context
	// standalone means no offline-cache Service runs on this device, so
	// nothing else evicts records or collects blobs: the materializer keeps
	// its own records under maxBytes and runs GC itself. With a Service
	// running it must NOT run GC — the Service's worker does, and a GC
	// between another producer's WriteBlob and SaveItem would sweep a blob
	// that is about to be referenced.
	standalone bool
	maxBytes   int64
	onReady    func()
	// evict removes one record and reclaims its blobs: the Service's
	// ClearItem when a Service runs (its GC is serialized with captures),
	// otherwise a delete and a GC of our own (we are the only writer).
	evict func(source string) error
	// fence is the Service's clear epoch (nil standalone, where the app's
	// clear commands are disabled): a job never publishes a work cleared
	// after it started (materialize).
	fence ClearFence

	admission *onchfsAdmission

	mu       sync.Mutex
	inflight map[string]*onchfsJob // by SourceKey; guarded by mu
	// protected are the SourceKeys of the playlist last passed to Ensure:
	// reclaiming space never evicts a work the wall is about to show.
	protected map[string]bool // guarded by mu

	// writeMu serializes this producer's writes and, when standalone, its
	// GC: every blob it writes is referenced by a saved record before a GC
	// it runs can look.
	writeMu sync.Mutex
}

type onchfsJob struct {
	done chan struct{}
	err  error
	// foreground marks the work a cast is waiting to show; admission puts
	// it ahead of background work (onchfsAdmission).
	foreground atomic.Bool
	// barrier is the clear epoch when the job was created (fence only).
	barrier uint64
}

// ClearFence is the part of Service a producer outside its worker needs to
// respect clears: ClearItem bumps the source's epoch before it deletes the
// record, so an epoch past a barrier taken earlier means a clear started.
type ClearFence interface {
	ClearBarrier() uint64
	ClearedSinceBarrier(playlistID string, barrier uint64, sources ...string) bool
}

// errOnchfsCleared reports a job whose work was cleared while it was read.
var errOnchfsCleared = errors.New("onchfs: the work was cleared while it was being read")

// OnchfsMaterializerConfig configures NewOnchfsMaterializer.
type OnchfsMaterializerConfig struct {
	Store    Store
	Resolver OnchfsResolver
	Logger   *zap.Logger
	// Base bounds every job; cancel it to stop the materializer.
	Base context.Context
	// Standalone: no Service runs (see OnchfsMaterializer.standalone).
	Standalone bool
	// MaxBytes bounds this producer's records when Standalone
	// (DefaultOnchfsStoreBytes when 0).
	MaxBytes int64
	// OnReady, when set, is called after a job that finished in the
	// background (past the caller's wait) saved a record, so the caller
	// can widen the replay scope to it. It must not block.
	OnReady func()
	// Evict removes one record by source and reclaims its blobs. Pass the
	// Service's ClearItem when a Service runs; nil when Standalone (the
	// materializer then deletes and collects itself).
	Evict func(source string) error
	// Fence is the Service, when one runs: its clear epoch keeps a job
	// from restoring a work cleared while it was being read.
	Fence ClearFence
	// Now is the clock (time.Now when nil).
	Now func() time.Time
}

// NewOnchfsMaterializer returns a materializer over cfg.Store.
func NewOnchfsMaterializer(cfg OnchfsMaterializerConfig) *OnchfsMaterializer {
	m := &OnchfsMaterializer{
		store: cfg.Store, resolver: cfg.Resolver, logger: cfg.Logger, base: cfg.Base,
		standalone: cfg.Standalone, maxBytes: cfg.MaxBytes, onReady: cfg.OnReady, now: cfg.Now,
		inflight: map[string]*onchfsJob{},
		evict:    cfg.Evict, fence: cfg.Fence,
	}
	if m.evict == nil {
		m.evict = m.evictStandalone
	}
	if m.now == nil {
		m.now = time.Now
	}
	if m.maxBytes <= 0 {
		m.maxBytes = DefaultOnchfsStoreBytes
	}
	if m.base == nil {
		m.base = context.Background()
	}
	m.admission = newOnchfsAdmission(m.base)
	return m
}

// Ensure makes every onchfs item among items replayable from the store. It
// waits for the first onchfs item in playlist order, the one a cast shows
// first, at most wait (and no longer than ctx); every other item, and the
// first one if it takes longer, continues in the background and calls
// OnReady when it saves a record. Items whose record is already fresh cost
// one record read. A zero wait starts the work and returns at once. It
// returns how many onchfs items were replayable when it returned.
//
// Errors are logged, not returned: an item that cannot be read stays
// unplayable, which the player handles like any failed source, and must not
// fail the rest of the cast.
func (m *OnchfsMaterializer) Ensure(ctx context.Context, items []dp1playlist.PlaylistItem, wait time.Duration) int {
	ready := 0
	var first *onchfsJob
	seen := map[string]bool{}
	protected := map[string]bool{}
	for _, item := range items {
		if contentpath.IsOnchfsSourceURL(item.Source) {
			protected[SourceKey(item.Source)] = true
		}
	}
	m.mu.Lock()
	m.protected = protected
	m.mu.Unlock()
	for _, item := range items {
		if !contentpath.IsOnchfsSourceURL(item.Source) || seen[item.Source] {
			continue
		}
		isFirst := len(seen) == 0
		seen[item.Source] = true
		if m.fresh(item.Source, onchfsRefreshWithin) {
			ready++
			continue
		}
		// The first work is what the cast waits for; it must not queue
		// behind background reads of the rest (PR #461 review F13).
		job := m.start(item, isFirst && wait > 0)
		if isFirst {
			first = job
		}
	}
	if first == nil || wait <= 0 {
		return ready
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-first.done:
		if first.err == nil {
			ready++
		}
	case <-timer.C:
	case <-ctx.Done():
	}
	return ready
}

// fresh reports whether source's record exists and stays replayable for at
// least margin.
//
// It also checks every blob the record names is on disk. A record whose blobs
// went missing (swept by a store without the publish guard, or removed by
// hand) would otherwise stay "fresh" and unplayable until it expired; this
// way the next cast rebuilds it.
func (m *OnchfsMaterializer) fresh(source string, margin time.Duration) bool {
	rec, err := m.store.LoadItem(SourceKey(source))
	if err != nil || rec == nil || !rec.Coverage.Complete || !rec.freshForPlayback(m.now().Add(margin)) {
		return false
	}
	for _, res := range rec.Resources {
		if res.SHA256 == "" {
			continue
		}
		if _, err := m.store.BlobSize(res.SHA256); err != nil {
			return false
		}
	}
	return true
}

// start begins (or joins) the job for item.
//
// A foreground job (the work a cast is waiting for) goes ahead of background
// work for a read slot, and a background job already queued for the work is
// promoted when a cast asks for it; see onchfsAdmission for the bound both
// share.
func (m *OnchfsMaterializer) start(item dp1playlist.PlaylistItem, foreground bool) *onchfsJob {
	key := SourceKey(item.Source)
	m.mu.Lock()
	if job, ok := m.inflight[key]; ok {
		m.mu.Unlock()
		if foreground && !job.foreground.Swap(true) {
			m.admission.wake()
		}
		return job
	}
	job := &onchfsJob{done: make(chan struct{})}
	job.foreground.Store(foreground)
	if m.fence != nil {
		job.barrier = m.fence.ClearBarrier()
	}
	m.inflight[key] = job
	m.mu.Unlock()

	go func() {
		defer func() {
			m.mu.Lock()
			delete(m.inflight, key)
			m.mu.Unlock()
			close(job.done)
		}()
		if !m.admission.acquire(m.base, job) {
			job.err = m.base.Err()
			return
		}
		defer m.admission.release()
		ctx, cancel := context.WithTimeout(m.base, onchfsJobTimeout)
		defer cancel()
		started := m.now()
		job.err = m.materialize(ctx, item, job.barrier)
		label := truncateSourceForLog(item.Source)
		if job.err != nil {
			m.logger.Warn("onchfs: work not materialized", zap.String("source", label), zap.Error(job.err))
			return
		}
		m.logger.Info("onchfs: work materialized", zap.String("source", label), zap.Duration("took", m.now().Sub(started)))
		if m.onReady != nil {
			m.onReady()
		}
	}()
	return job
}

// materialize reads item's tree and writes it as a record.
func (m *OnchfsMaterializer) materialize(ctx context.Context, item dp1playlist.PlaylistItem, barrier uint64) error {
	o, ok := contentpath.ParseOnchfsSourceURL(item.Source)
	if !ok {
		return fmt.Errorf("onchfs: not a device-local onchfs source")
	}
	tree, err := m.resolver.Resolve(ctx, o.Root, o.Chain)
	if err != nil {
		return err
	}
	now := m.now()
	resources, blobs, err := onchfsResources(o, item.Source, tree, now.Add(onchfsFreshFor))
	if err != nil {
		return err
	}
	rec := &ItemRecord{
		Automatic:    true,
		LastPlayedAt: now,
		Item:         item,
		Entry:        entryURL(item.Source),
		Resources:    resources,
		Coverage:     Coverage{Complete: true},
		CapturedAt:   now,
	}
	m.writeMu.Lock()
	defer m.writeMu.Unlock()
	key := SourceKey(item.Source)
	if m.cleared(item.Source, barrier) {
		return errOnchfsCleared
	}
	err = m.write(rec, blobs, barrier)
	// The cache shares one disk budget. When it is full, older onchfs works
	// make room for this one, oldest played first, never one the current
	// playlist shows (PR #461 review F6). Capture reclaims for itself
	// inside the Service; this producer has to do the same or a full cache
	// would leave every new onchfs work unplayable.
	for evicted := 0; isDiskPressure(err) && evicted < onchfsMaxEvictions; evicted++ {
		victim, ok := m.oldestEvictable(key)
		if !ok {
			break
		}
		if evictErr := m.evict(victim); evictErr != nil {
			m.logger.Warn("onchfs: evicting a record to make room failed", zap.Error(evictErr))
			break
		}
		m.logger.Info("onchfs: evicted an older work to make room", zap.String("source", truncateSourceForLog(victim)))
		err = m.write(rec, blobs, barrier)
	}
	if err != nil {
		if m.standalone {
			// Blobs a failed publication wrote are referenced by nothing;
			// collect them now. With a Service running its GC does.
			if _, _, gcErr := m.store.GC(); gcErr != nil {
				m.logger.Warn("onchfs: collecting blobs after a failed write failed", zap.Error(gcErr))
			}
		}
		return err
	}
	if m.standalone {
		m.prune(key)
	}
	return nil
}

// cleared reports a clear of source that started after barrier.
func (m *OnchfsMaterializer) cleared(source string, barrier uint64) bool {
	return m.fence != nil && m.fence.ClearedSinceBarrier("", barrier, source)
}

// onchfsMaxEvictions bounds one write's reclaim loop.
const onchfsMaxEvictions = 64

// isDiskPressure reports a write the cache's disk budget refused.
func isDiskPressure(err error) bool {
	return errors.Is(err, errCacheDiskPressure) || errors.Is(err, ErrBlobTooLarge)
}

// evictStandalone removes a record and collects its blobs, for a device
// with no Service (the materializer is then the store's only writer).
func (m *OnchfsMaterializer) evictStandalone(source string) error {
	if _, err := m.store.DeleteItem(SourceKey(source)); err != nil {
		return err
	}
	_, _, err := m.store.GC()
	return err
}

// oldestEvictable is the least recently played onchfs record other than
// keep and the current playlist's works.
func (m *OnchfsMaterializer) oldestEvictable(keep string) (string, bool) {
	m.mu.Lock()
	protected := m.protected
	m.mu.Unlock()
	for _, r := range m.onchfsRecords() {
		if r.key != keep && !protected[r.key] {
			return r.source, true
		}
	}
	return "", false
}

// onchfsRecord is one onchfs record as the eviction policies see it.
type onchfsRecord struct {
	key, source string
	at          time.Time
	bytes       int64
}

// onchfsRecords lists this producer's records, least recently played first.
func (m *OnchfsMaterializer) onchfsRecords() []onchfsRecord {
	keys, err := m.store.ListItemKeys()
	if err != nil {
		m.logger.Warn("onchfs: listing records failed", zap.Error(err))
		return nil
	}
	var out []onchfsRecord
	for _, key := range keys {
		rec, err := m.store.LoadItem(key)
		if err != nil || rec == nil || !contentpath.IsOnchfsSourceURL(rec.Item.Source) {
			continue
		}
		r := onchfsRecord{key: key, source: rec.Item.Source, at: rec.LastPlayedAt}
		if r.at.IsZero() {
			r.at = rec.CapturedAt
		}
		seen := map[string]bool{}
		for _, res := range rec.Resources {
			if res.SHA256 == "" || seen[res.SHA256] {
				continue
			}
			seen[res.SHA256] = true
			if n, err := m.store.BlobSize(res.SHA256); err == nil {
				r.bytes += n
			}
		}
		out = append(out, r)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].at.Before(out[j].at) })
	return out
}

// write stores every blob and saves the record. Through a publishing store
// (BootstrapCore's guard) the two happen while no GC can run, so a GC can
// never sweep blobs a record is about to reference (PR #461 review F3).
// Afterwards every blob the record names is checked and rewritten if
// missing: the guard is what makes this safe, the check is what repairs a
// store without one.
//
// A clear that started after barrier undoes the publication (PR #461 review
// F16). ClearItem bumps the source's epoch before deleting the record, so a
// check after SaveItem covers every order: a clear that started before the
// check is seen and the record just saved is deleted again; one that starts
// after it deletes the saved record itself.
func (m *OnchfsMaterializer) write(rec *ItemRecord, blobs map[string][]byte, barrier uint64) error {
	publish := func() error {
		for _, body := range blobs {
			if _, err := m.store.WriteBlob(bytes.NewReader(body), 0); err != nil {
				return fmt.Errorf("onchfs: writing a blob: %w", err)
			}
		}
		if err := m.store.SaveItem(rec); err != nil {
			return fmt.Errorf("onchfs: saving the record: %w", err)
		}
		if m.cleared(rec.Item.Source, barrier) {
			if _, err := m.store.DeleteItem(SourceKey(rec.Item.Source)); err != nil {
				return fmt.Errorf("onchfs: undoing a publication a clear raced: %w", err)
			}
			return errOnchfsCleared
		}
		for sha, body := range blobs {
			if _, err := m.store.BlobSize(sha); err == nil {
				continue
			}
			if _, err := m.store.WriteBlob(bytes.NewReader(body), 0); err != nil {
				return fmt.Errorf("onchfs: rewriting a missing blob: %w", err)
			}
		}
		return nil
	}
	if p, ok := m.store.(publisher); ok {
		return p.Publish(publish)
	}
	return publish()
}

// prune keeps this producer's records under maxBytes, oldest played first,
// never the one just written, then collects unreferenced blobs. Only when
// standalone (see OnchfsMaterializer.standalone); called under writeMu.
// Failures are logged: the cost of a failed prune is disk, never playback.
func (m *OnchfsMaterializer) prune(keep string) {
	records := m.onchfsRecords()
	var total int64
	for _, r := range records {
		total += r.bytes
	}
	if total <= m.maxBytes {
		return
	}
	// Works the current playlist shows are kept even past the budget: the
	// wall is about to play them, and deleting one only makes the next pass
	// read it again (PR #461 review F8). The budget then binds once the
	// playlist changes.
	m.mu.Lock()
	protected := m.protected
	m.mu.Unlock()
	for _, r := range records {
		if total <= m.maxBytes {
			break
		}
		if r.key == keep || protected[r.key] {
			continue
		}
		if _, err := m.store.DeleteItem(r.key); err != nil {
			m.logger.Warn("onchfs: pruning a record failed", zap.Error(err))
			continue
		}
		total -= r.bytes
	}
	if _, _, err := m.store.GC(); err != nil {
		m.logger.Warn("onchfs: collecting blobs failed", zap.Error(err))
	}
}

// onchfsResources maps a verified tree to the resources replay answers with
// (see the comment at the top of this file), and the blobs they name by
// sha256. Bodies are the plain bytes: a declared gzip content-encoding was
// decoded when the tree was read, and Content-Encoding is never replayed.
func onchfsResources(o contentpath.Onchfs, source string, tree *onchfs.Tree, freshUntil time.Time) ([]Resource, map[string][]byte, error) {
	origin := o.Origin()
	blobs := map[string][]byte{}
	byPath := map[string]Resource{}
	paths := make([]string, 0, len(tree.Files))
	for p := range tree.Files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		f := tree.Files[p]
		sum := sha256.Sum256(f.Plain)
		sha := hex.EncodeToString(sum[:]) // the key WriteBlob stores it under
		blobs[sha] = f.Plain
		byPath[p] = Resource{
			URL: origin + "/" + escapeTreePath(p), Status: 200, SHA256: sha, ContentType: f.ContentType,
			// The player probes a work's content type with a cross-origin
			// HEAD from its own origin before mounting it.
			Headers:    map[string]string{"Access-Control-Allow-Origin": "*"},
			FreshUntil: freshUntil,
		}
	}
	resources := make([]Resource, 0, len(paths)+4)
	urls := map[string]bool{}
	add := func(r Resource) {
		if !urls[r.URL] {
			urls[r.URL] = true
			resources = append(resources, r)
		}
	}
	// Each file under the spelling a browser requests first, for every file,
	// so a primary URL always wins; then under the other spellings a
	// request may use (treePathSpellings), wherever no file claimed them.
	for _, p := range paths {
		add(byPath[p])
	}
	// A single-file tree is keyed "" and served at the root.
	if f, ok := byPath[""]; ok {
		f.URL = origin + "/"
		add(f)
	}
	dirIndexes := make([]string, 0)
	for _, p := range paths {
		if path.Base(p) != "index.html" {
			continue
		}
		dirIndexes = append(dirIndexes, p)
		alias := byPath[p]
		alias.URL = origin + "/"
		if dir := path.Dir(p); dir != "." {
			alias.URL += escapeTreePath(dir) + "/"
		}
		add(alias)
	}
	for _, p := range paths {
		for _, spelling := range treePathSpellings(p)[1:] {
			alt := byPath[p]
			alt.URL = origin + "/" + spelling
			add(alt)
		}
	}
	for _, p := range dirIndexes {
		if dir := path.Dir(p); dir != "." {
			for _, spelling := range treePathSpellings(dir)[1:] {
				alt := byPath[p]
				alt.URL = origin + "/" + spelling + "/"
				add(alt)
			}
		}
	}

	entry := entryURL(source)
	target, err := url.PathUnescape(o.Path)
	if err != nil {
		return nil, nil, fmt.Errorf("onchfs: entry path %q: %w", o.Path, err)
	}
	rootFile, singleFile := byPath[""]
	switch {
	case target == "" && singleFile:
		// The root is a file, not a directory (PR #461 review F4).
		rootFile.URL = entry
		add(rootFile)
	case hasPath(byPath, entryFile(target)):
		res := byPath[entryFile(target)]
		res.URL = entry
		add(res)
	case target != "" && !strings.HasSuffix(target, "/") && hasPath(byPath, target+"/index.html"):
		// A directory named without its slash: redirect to the slashed form
		// (query kept), as an onchfs proxy does, so relative URLs inside its
		// index.html resolve against the directory. The redirect's target,
		// query included, is stored too: replay matches exact URLs, and
		// without it the followed redirect would miss (review F5).
		slashed := slashedEntry(entry)
		add(Resource{URL: entry, Status: 301, RedirectTo: slashed, FreshUntil: freshUntil,
			Headers: map[string]string{"Access-Control-Allow-Origin": "*"}})
		index := byPath[target+"/index.html"]
		index.URL = slashed
		add(index)
	default:
		return nil, nil, fmt.Errorf("%w: the tree has no %q to open", onchfs.ErrNotFound, entryFile(target))
	}
	return resources, blobs, nil
}

func hasPath(byPath map[string]Resource, p string) bool {
	_, ok := byPath[p]
	return ok
}

// entryFile is the file a path inside the tree opens: itself, or the
// index.html of a directory ("" is the root).
func entryFile(target string) string {
	if target == "" || strings.HasSuffix(target, "/") {
		return target + "index.html"
	}
	return target
}

// entryURL is the URL the kiosk requests for source: the fragment never
// reaches the network.
func entryURL(source string) string {
	before, _, _ := strings.Cut(source, "#")
	return before
}

// slashedEntry inserts "/" before the query of an entry URL.
func slashedEntry(entry string) string {
	before, query, hasQuery := strings.Cut(entry, "?")
	if hasQuery {
		return before + "/?" + query
	}
	return before + "/"
}

// escapeTreePath is the URL path a browser requests for a tree path: the
// first of treePathSpellings.
func escapeTreePath(p string) string { return treePathSpellings(p)[0] }

// treePathSpellings lists the URL paths a request for tree path p may use,
// the browser's own first.
//
// Replay matches URLs exactly, so a file must be stored under the spelling
// the kiosk requests. A browser resolves `<script src="lib!v1.js">` to
// `/lib!v1.js`: it percent-encodes a path only in the WHATWG URL
// standard's path percent-encode set (controls, space, non-ASCII, and
// " # < > ? ` { }), not everything Go's url.PathEscape escapes (`!` among
// them), so a file stored under PathEscape's spelling missed (PR #461
// review F9). Two more spellings are kept as fallbacks: one that also
// escapes ^ and |, which browsers have treated differently over time, and
// url.PathEscape's, for a work that spells its own references escaped.
// They never displace another file's primary spelling (onchfsResources
// adds every primary first).
//
// Every spelling must decode back to p (PR #461 review F11). A URL path is
// percent-decoded to find its file, so `/a%21b.html` names `a!b.html`; a
// file literally named `a%21b.html` is `/a%2521b.html`. A literal % is
// therefore always encoded: leaving it as is let a file claim a URL that
// names another one.
func treePathSpellings(p string) []string {
	join := func(escape func(string) string) string {
		segs := strings.Split(p, "/")
		for i, s := range segs {
			segs[i] = escape(s)
		}
		return strings.Join(segs, "/")
	}
	spellings := []string{
		join(func(s string) string { return browserPathEscape(s, false) }),
		join(func(s string) string { return browserPathEscape(s, true) }),
		join(url.PathEscape),
	}
	out := spellings[:1]
	for _, s := range spellings[1:] {
		if !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	return out
}

// browserPathEscape percent-encodes one path segment as the WHATWG path
// percent-encode set does, plus % itself (see treePathSpellings);
// caretPipe also encodes ^ and |.
func browserPathEscape(s string, caretPipe bool) string {
	const upperhex = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c <= 0x20 || c >= 0x7f || strings.IndexByte("\"#<>?`{}%", c) >= 0 || (caretPipe && (c == '^' || c == '|')) {
			b.WriteByte('%')
			b.WriteByte(upperhex[c>>4])
			b.WriteByte(upperhex[c&15])
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}
