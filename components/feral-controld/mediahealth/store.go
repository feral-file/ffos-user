package mediahealth

import (
	"github.com/feral-file/ffos-user/components/feral-controld/contentpath"

	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	bolt "go.etcd.io/bbolt"
	bolterrors "go.etcd.io/bbolt/errors"
	"go.uber.org/zap"
)

// Store is the on-device observation store: one bbolt file holding every
// record this device has ever made about a source.
//
// Why bbolt: the store is PERMANENT (it survives reboot and, once the ffos
// OTA carry includes its directory, full-image updates; only a factory reset
// clears it) and UNBOUNDED by count — a collector wall can hold hundreds of
// thousands of works over the years. The hint path needs one point read per
// playlist item on every cast, and the status path the same on every poll,
// which a B+tree over an mmap answers in microseconds without loading
// anything at startup. Per-record JSON files (the offline cache's shape) or
// a JSONL log with an in-memory index would both need a startup scan and
// RAM proportional to the record count on a memory-constrained device.
//
// Buckets:
//
//	items    Key(source) → JSON Record
//	recheck  "<recheckAfter unix, zero-padded>|<key>" → "" (due-date index)
//	meta     "schema" → version
//
// The recheck bucket lets the lookahead and the background sweep find due
// records without scanning items; every write of a record replaces its
// recheck entry. Writes go through db.Batch so bursts (a 1,000-item
// preflight) coalesce their fsyncs.
//
// Disk guard: the store never evicts, so the one thing it must not do is
// fill the disk. Writes are refused while the filesystem's free space is
// under Options.MinFreeBytes; the daemon logs that once per episode and
// playback carries on without new observations.
type Store struct {
	db     *bolt.DB
	path   string
	now    func() time.Time
	build  Build
	logger *zap.Logger

	minFreeBytes int64
	freeBytes    func(path string) (int64, error)
	lowDiskWarn  bool

	// gateways parses sources into content ids for the servedBy index.
	gateways *Gateways
	// renderConclusive lets a corroborated render negative (a blank or
	// known-bad frame, from L1 or the wall) conclude broken; false keeps it
	// suspect (advisory). Options.RenderConclusive.
	renderConclusive bool
	// servedByMu guards servedBy, the in-memory mirror of the routes in
	// bucketServedBy that have at least one source on a fallback gateway:
	// route (ContentRef.RouteKey) → entry (identity + servedBySep + owner,
	// see servedByEntryKey) → the gateway origin the owner was served by
	// ("" = its own gateway). Read per
	// kiosk request (GatewayOverride), written as records change. Routes
	// with no fallback stay on disk only, so memory is bounded by the
	// sources that need a fallback, not by every content-addressed source.
	servedByMu sync.RWMutex
	servedBy   map[string]map[string]string
}

// Options tunes Open.
type Options struct {
	// Now is the clock (time.Now when nil).
	Now func() time.Time
	// Gateways parses sources into content ids for the servedBy index
	// (NewGateways defaults when nil). Must be the same set the Probe uses.
	Gateways *Gateways
	// Build is stamped on every verdict this process writes.
	Build Build
	// RenderConclusive (mediaHealth.renderConclusive, default false): a
	// corroborated render negative concludes broken (skipped, counted
	// toward rejecting an all-broken cast) instead of suspect. Off until
	// the office fixture shows no false blanks among its healthy works.
	RenderConclusive bool
	// MinFreeBytes refuses writes when the filesystem has less free space.
	// 0 disables the guard (tests).
	MinFreeBytes int64
	// FreeBytes reports the free space of the filesystem holding path
	// (statfs when nil).
	FreeBytes func(path string) (int64, error)
	Logger    *zap.Logger
}

var (
	bucketItems   = []byte("items")
	bucketRecheck = []byte("recheck")
	bucketMeta    = []byte("meta")
	// bucketServedBy indexes every ok content-addressed source, keyed
	// route + servedBySep + Key(source) (plus the source's request aliases,
	// servedByIdentities), to the gateway origin the kiosk
	// loads it through (Record.ServedBy; "" = its own gateway), so the
	// interceptor's per-request lookup never scans records. The routes
	// with a fallback are mirrored in memory (Store.servedBy).
	bucketServedBy = []byte("servedBy")
	metaSchemaKey  = []byte("schema")
)

// schemaVersion is the bucket layout version, distinct from recordVersion.
const schemaVersion = 1

// ErrLowDisk is returned by a write refused by the disk guard.
var ErrLowDisk = errors.New("media health: store write refused, disk below reserve")

// ErrNewerRecord is returned when a record was written by a newer binary.
var ErrNewerRecord = errors.New("media health: record version is newer than this binary")

// Open opens (creating if needed) the store at path. The parent directory is
// created with 0750. A store written by a newer schema is refused rather
// than rewritten.
func Open(path string, opts Options) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, fmt.Errorf("media health: create store dir: %w", err)
	}
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: 2 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("media health: open store: %w", err)
	}
	s := &Store{
		db:               db,
		path:             path,
		now:              opts.Now,
		build:            opts.Build,
		logger:           opts.Logger,
		minFreeBytes:     opts.MinFreeBytes,
		freeBytes:        opts.FreeBytes,
		gateways:         opts.Gateways,
		renderConclusive: opts.RenderConclusive,
		servedBy:         map[string]map[string]string{},
	}
	if s.gateways == nil {
		s.gateways = defaultGateways
	}
	if s.now == nil {
		s.now = time.Now
	}
	if s.logger == nil {
		s.logger = zap.NewNop()
	}
	if s.freeBytes == nil {
		s.freeBytes = statfsFree
	}
	err = db.Update(func(tx *bolt.Tx) error {
		for _, b := range [][]byte{bucketItems, bucketRecheck, bucketMeta, bucketServedBy} {
			if _, err := tx.CreateBucketIfNotExists(b); err != nil {
				return err
			}
		}
		meta := tx.Bucket(bucketMeta)
		if raw := meta.Get(metaSchemaKey); raw != nil {
			v, perr := strconv.Atoi(string(raw))
			if perr != nil || v > schemaVersion {
				return fmt.Errorf("media health: store schema %q is not readable by this binary", raw)
			}
			return nil
		}
		return meta.Put(metaSchemaKey, []byte(strconv.Itoa(schemaVersion)))
	})
	if err == nil {
		err = s.loadServedBy()
	}
	if err == nil {
		err = s.retireUnconfiguredFallbacks()
	}
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// Close closes the file. Safe to call once; the Store is unusable after.
func (s *Store) Close() error {
	return s.db.Close()
}

// Path is the store file.
func (s *Store) Path() string { return s.path }

// Destroy removes the store directory. The caller must have closed the
// store; factory reset is the one caller.
func Destroy(path string) error {
	dir := filepath.Dir(path)
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("media health: remove store dir: %w", err)
	}
	return nil
}

// Get returns the record for source, nil when none.
func (s *Store) Get(source string) (*Record, error) {
	var rec *Record
	err := s.db.View(func(tx *bolt.Tx) error {
		var derr error
		rec, derr = s.decode(tx.Bucket(bucketItems).Get([]byte(Key(source))))
		return derr
	})
	return rec, err
}

// GetMany returns the records for sources (missing ones absent from the
// map) in one read transaction.
func (s *Store) GetMany(sources []string) (map[string]*Record, error) {
	out := make(map[string]*Record, len(sources))
	err := s.db.View(func(tx *bolt.Tx) error {
		items := tx.Bucket(bucketItems)
		for _, src := range sources {
			if _, done := out[src]; done {
				continue
			}
			rec, derr := s.decode(items.Get([]byte(Key(src))))
			if derr != nil {
				// One unreadable record must not blank a whole playlist's
				// hints; it reads as unknown and is logged once here.
				s.logger.Warn("media health: unreadable record skipped", zap.Error(derr))
				continue
			}
			if rec != nil {
				out[src] = rec
			}
		}
		return nil
	})
	return out, err
}

// Count returns the number of records.
func (s *Store) Count() (int, error) {
	n := 0
	err := s.db.View(func(tx *bolt.Tx) error {
		n = tx.Bucket(bucketItems).Stats().KeyN
		return nil
	})
	return n, err
}

// Due returns up to limit sources whose due time (recheck or transient
// retry) is at or before now, earliest first, skipping any for which skip
// returns true (nil skips nothing). The exclusion is applied INSIDE the
// scan, before the limit, so excluded rows at the head of the index — the
// items on the wall, sources the caller holds — cannot fill the result and
// shadow eligible rows behind them (PR #438 review, F8). skip runs inside
// the store's read transaction: it must be cheap and must not touch the
// store.
func (s *Store) Due(now time.Time, limit int, skip func(source string) bool) ([]string, error) {
	var out []string
	err := s.db.View(func(tx *bolt.Tx) error {
		items := tx.Bucket(bucketItems)
		c := tx.Bucket(bucketRecheck).Cursor()
		bound := []byte(recheckPrefix(now.Add(time.Second)))
		for k, _ := c.First(); k != nil && bytes.Compare(k, bound) < 0 && len(out) < limit; k, _ = c.Next() {
			_, itemKey, found := bytes.Cut(k, []byte{'|'})
			if !found {
				continue
			}
			rec, derr := s.decode(items.Get(itemKey))
			if derr != nil || rec == nil {
				continue
			}
			if skip != nil && skip(rec.Source) {
				continue
			}
			out = append(out, rec.Source)
		}
		return nil
	})
	return out, err
}

// put writes rec under its key and re-indexes its recheck time. prev is the
// record previously stored (nil when none) so the old index entry is removed.
func (s *Store) put(tx *bolt.Tx, prev, rec *Record) error {
	rec.V = recordVersion
	raw, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	key := []byte(Key(rec.Source))
	if err := tx.Bucket(bucketItems).Put(key, raw); err != nil {
		return err
	}
	recheck := tx.Bucket(bucketRecheck)
	if prev != nil {
		if err := recheck.Delete(recheckKey(prev.dueAt(), key)); err != nil {
			return err
		}
	}
	if err := recheck.Put(recheckKey(rec.dueAt(), key), []byte{}); err != nil {
		return err
	}
	return s.indexServedBy(tx, prev, rec)
}

// servedBySep separates a route from the source that needs it in a
// bucketServedBy key.
const servedBySep = "\x00"

// servedByIndexVersion is the bucketServedBy key layout, stored under
// metaServedByIndexKey. An index in any other layout is REBUILT from the
// records at Open, never read or skipped: skipping entries an older
// layout wrote left the records saying "served by ar-io.dev" while the
// kiosk's lookup was empty, so after a deploy the wall went back to the
// failing gateway with nothing in the verdict to show it (office FF1,
// 2026-11-06). The records are the source of truth; the index is a cache.
// Version 5 keys each entry by route, identity AND owning source
// (servedByEntryKey), so an identity two sources share is never deleted
// by one of them on the other's behalf. Version 6 also indexes a
// render-layer broken record (servedByEntry).
const servedByIndexVersion = "6"

var metaServedByIndexKey = []byte("servedByIndex")

// loadServedBy mirrors bucketServedBy into memory, first rebuilding the
// bucket from the records when its layout is not servedByIndexVersion.
func (s *Store) loadServedBy() error {
	err := s.db.Update(func(tx *bolt.Tx) error {
		meta := tx.Bucket(bucketMeta)
		if string(meta.Get(metaServedByIndexKey)) == servedByIndexVersion {
			return nil
		}
		if err := tx.DeleteBucket(bucketServedBy); err != nil && !errors.Is(err, bolterrors.ErrBucketNotFound) {
			return err
		}
		b, err := tx.CreateBucket(bucketServedBy)
		if err != nil {
			return err
		}
		rebuilt := 0
		err = tx.Bucket(bucketItems).ForEach(func(k, v []byte) error {
			rec, derr := s.decode(v)
			if derr != nil {
				return nil
			}
			origin, indexed := servedByEntry(rec)
			if !indexed {
				return nil
			}
			ref, ok := s.gateways.Ref(rec.Source)
			if !ok {
				return nil
			}
			rebuilt++
			for _, id := range servedByIdentities(rec.Source) {
				if err := b.Put([]byte(ref.RouteKey()+servedBySep+servedByEntryKey(id, string(k))), []byte(origin)); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			return err
		}
		if rebuilt > 0 {
			s.logger.Info("media health: gateway-override index rebuilt from records", zap.Int("entries", rebuilt))
		}
		return meta.Put(metaServedByIndexKey, []byte(servedByIndexVersion))
	})
	if err != nil {
		return err
	}
	all := map[string]map[string]string{}
	err = s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketServedBy).ForEach(func(k, v []byte) error {
			if route, entry, ok := strings.Cut(string(k), servedBySep); ok {
				if all[route] == nil {
					all[route] = map[string]string{}
				}
				all[route][entry] = string(v)
			}
			return nil
		})
	})
	if err != nil {
		return err
	}
	s.servedByMu.Lock()
	defer s.servedByMu.Unlock()
	for route, sources := range all {
		if routeHasFallback(sources) {
			s.servedBy[route] = sources
		}
	}
	return nil
}

// retireUnconfiguredFallbacks drops, at Open, every recorded fallback that
// is no longer on the operator's gateway lists, and makes its source due
// now so the next probe finds its current gateway. The override must only
// ever target a configured origin: ar-io.dev was removed from the
// defaults after it answered whole-file loads with 402, and records still
// "served by" it would otherwise keep routing the kiosk there for the 7 d
// an ok verdict lasts. Bounded by the index (sources with a fallback), not
// the store.
func (s *Store) retireUnconfiguredFallbacks() error {
	s.servedByMu.RLock()
	var stale []string
	for _, sources := range s.servedBy {
		for entry, origin := range sources {
			if origin != "" && !s.gateways.Configured(origin) {
				if id, owner := splitServedByEntry(entry); id == owner && !slices.Contains(stale, owner) {
					stale = append(stale, owner)
				}
			}
		}
	}
	s.servedByMu.RUnlock()
	if len(stale) == 0 {
		return nil
	}
	now := s.now()
	err := s.db.Update(func(tx *bolt.Tx) error {
		items := tx.Bucket(bucketItems)
		for _, key := range stale {
			prev, derr := s.decode(items.Get([]byte(key)))
			if derr != nil || prev == nil {
				continue
			}
			rec := *prev
			rec.ServedBy = ""
			rec.RecheckAfter = now
			rec.RetryAfter = time.Time{}
			if err := s.put(tx, prev, &rec); err != nil {
				return err
			}
		}
		return nil
	})
	if err == nil {
		s.logger.Info("media health: fallback gateways no longer configured were retired", zap.Int("sources", len(stale)))
	}
	return err
}

// servedByEntry is what the index holds for a record: every ok
// content-addressed source is indexed, with the fallback origin that
// serves it or "" for its own gateway. Anything else (no record, no
// verdict, broken) is absent, so the kiosk lookup can tell a source KNOWN
// to load from its own gateway from a request nothing ties to a source
// (PR #438 review, F25 round 4). A suspect (L1) record is indexed like an
// ok one: L1 is advisory and never gates playback, and its verdict says
// nothing about which gateway serves the bytes — dropping the route when a
// work turns suspect would send the wall back to the gateway that 404s.
//
// A render-layer broken record is indexed too: a render verdict (the work
// did not paint) says nothing about which gateway serves its bytes, and the
// work is retried at the end of its window — through the gateway that
// served it, not the one that 404s.
func servedByEntry(rec *Record) (origin string, indexed bool) {
	if rec == nil || (rec.State != StateOK && rec.State != StateSuspect && !rec.renderLayerBroken()) {
		return "", false
	}
	return rec.ServedBy, true
}

func routeHasFallback(sources map[string]string) bool {
	for _, o := range sources {
		if o != "" {
			return true
		}
	}
	return false
}

// indexServedBy keeps bucketServedBy and its in-memory mirror in step with
// a record (servedByEntry). One entry per (route, source): a route stays
// routed while ANY source on it still needs the fallback, a source
// recovering keeps its own entry ("" — known to load from its own gateway)
// so it is never mistaken for an untied request, and a route is held in
// memory only while a source on it needs a fallback — the moment one does,
// the route's own-gateway sources are read in from the bucket (PR #438
// review, F25). The mirror is updated inside the write transaction: a
// rolled-back write leaves it ahead of the file until the next write for
// that source, which only affects which gateway the kiosk tries, never a
// verdict.
func (s *Store) indexServedBy(tx *bolt.Tx, prev, rec *Record) error {
	prevOrigin, prevIndexed := servedByEntry(prev)
	origin, indexed := servedByEntry(rec)
	if prevIndexed == indexed && prevOrigin == origin {
		return nil
	}
	ref, ok := s.gateways.Ref(rec.Source)
	if !ok {
		return nil
	}
	b := tx.Bucket(bucketServedBy)
	route, owner := ref.RouteKey(), Key(rec.Source)
	var entries []string
	for _, id := range servedByIdentities(rec.Source) {
		entries = append(entries, servedByEntryKey(id, owner))
	}
	prefix := []byte(route + servedBySep)
	for _, entry := range entries {
		key := append(append([]byte(nil), prefix...), entry...)
		if indexed {
			if err := b.Put(key, []byte(origin)); err != nil {
				return err
			}
		} else if err := b.Delete(key); err != nil {
			return err
		}
	}
	s.servedByMu.Lock()
	defer s.servedByMu.Unlock()
	sources, inMemory := s.servedBy[route]
	switch {
	case !inMemory && origin == "":
		// A route with no fallback stays on disk only.
		return nil
	case !inMemory:
		// The route's first fallback: bring its own-gateway sources along
		// (the cursor sees this transaction's Put).
		sources = map[string]string{}
		c := b.Cursor()
		for k, v := c.Seek(prefix); k != nil && bytes.HasPrefix(k, prefix); k, v = c.Next() {
			sources[string(k[len(prefix):])] = string(v)
		}
		s.servedBy[route] = sources
	case !indexed:
		for _, entry := range entries {
			delete(sources, entry)
		}
	default:
		for _, entry := range entries {
			sources[entry] = origin
		}
	}
	if !routeHasFallback(sources) {
		delete(s.servedBy, route)
	}
	return nil
}

// GatewayOverride answers the kiosk interceptor: for a request URL whose
// content (an IPFS CID or an Arweave transaction, under any gateway, any
// path below it) is recorded as served by a fallback gateway, the same
// path and query on that gateway. Never a URL from playlist content: the
// origin comes from the operator's gateway list via the probe, and only
// the request's own path and query are carried over. A request already on
// the recorded gateway is left alone.
//
// referrer is the request's Referer header ("" when absent): a work's own
// files are requested by the work's document, so they follow the gateway
// recorded for THAT source (routeOrigin).
func (s *Store) GatewayOverride(rawURL, referrer string) (string, bool) {
	ref, ok := s.gateways.Ref(rawURL)
	if !ok {
		return "", false
	}
	s.servedByMu.RLock()
	origin := s.routeOriginLocked(ref.RouteKey(), rawURL, referrer)
	s.servedByMu.RUnlock()
	if origin == "" || strings.EqualFold(hostOf(origin), ref.Host) {
		return "", false
	}
	return ref.URLOn(origin), true
}

// routeOriginLocked picks the gateway for one request on a route ("" =
// the request's own gateway). Two sources under one CID can need
// DIFFERENT fallbacks — the first gateway serves a.html but answers 404
// for b.html, which the second serves — and one can need none while the
// other does, so the choice is per source, not per route (PR #438 review,
// F25 rounds 3–4):
//
//  1. the request IS an ok source (as written, or with the player's own
//     query parameters stripped — navigationKey — including the URL the
//     kiosk redirects a bare CID with a query to, which the index holds
//     as an alias of the source: servedByIdentities): that source's
//     gateway, its own included;
//  2. its Referer is an ok source: the gateway recorded for the document
//     that embeds it (a same-origin subresource carries the full document
//     URL under Chromium's default referrer policy);
//  3. otherwise the lexically smallest fallback on the route, a stable
//     best-effort for a request nothing ties to a source.
//
// Caller holds servedByMu.
func (s *Store) routeOriginLocked(route, rawURL, referrer string) string {
	sources := s.servedBy[route]
	if len(sources) == 0 {
		return ""
	}
	for _, u := range []string{rawURL, navigationKey(rawURL), referrer, navigationKey(referrer)} {
		if u == "" {
			continue
		}
		if o, ok := originForIdentity(sources, Key(u)); ok {
			return o
		}
	}
	origin := ""
	for _, o := range sources {
		if o != "" && (origin == "" || o < origin) {
			origin = o
		}
	}
	return origin
}

// servedByIdentities are the bucketServedBy keys one source is indexed
// under: Key(source), and — when the kiosk redirects it (a bare CID with a
// query, contentpath.SlashBeforeQuery) — the key of the URL it actually
// requests instead, as written and without the player's query params
// (navigationKey), so the redirected navigation and the files it loads
// (by Referer) find this source's own entry. The alias is computed from
// the source forward, never reconstructed from the request: the redirect
// also normalizes the path (`/ipfs//<cid>?q` → `/ipfs/<cid>/?q`), and a
// reconstruction cannot recover the original spelling, so the lookup fell
// through to another file's fallback (PR #438 review, F25 rounds 5–6).
//
// Several sources can share an identity — `/ipfs/<cid>?q`, `/ipfs//<cid>?q`
// and the already-slashed `/ipfs/<cid>/?q` all request the same URL — so
// every entry is owned (servedByEntryKey): a source writes and deletes
// only its own, and a shared identity survives while any owner is ok
// (PR #438 review, F25 round 7). originForIdentity resolves between
// owners.
func servedByIdentities(source string) []string {
	ids := []string{Key(source)}
	slashed, ok := contentpath.SlashBeforeQuery(source)
	if !ok {
		return ids
	}
	for _, u := range []string{slashed, navigationKey(slashed)} {
		if u == "" {
			continue
		}
		if k := Key(u); !slices.Contains(ids, k) {
			ids = append(ids, k)
		}
	}
	return ids
}

// servedByEntryKey is one index entry within a route: the identity a
// request is matched by, and the source (Key) that owns the entry.
func servedByEntryKey(identity, owner string) string {
	return identity + servedBySep + owner
}

func splitServedByEntry(entry string) (identity, owner string) {
	identity, owner, _ = strings.Cut(entry, servedBySep)
	return identity, owner
}

// originForIdentity picks the gateway for a request matched by identity
// among the route's entries (a route holds the few sources under one
// content id on one gateway, so the scan is short). Between owners: the
// source that IS this URL first (its own verdict is about exactly this
// request); otherwise the own gateway if any owner was served there (the
// gateway answered this very URL for it); otherwise the smallest origin,
// for a stable choice.
func originForIdentity(entries map[string]string, identity string) (string, bool) {
	found, own := false, false
	best := ""
	for entry, origin := range entries {
		id, owner := splitServedByEntry(entry)
		if id != identity {
			continue
		}
		if owner == identity {
			return origin, true
		}
		switch {
		case origin == "":
			own = true
		case !found || (best != "" && origin < best):
			best = origin
		}
		found = true
	}
	if own {
		return "", true
	}
	return best, found
}

func recheckPrefix(t time.Time) string {
	return fmt.Sprintf("%020d", t.Unix())
}

func recheckKey(t time.Time, key []byte) []byte {
	return []byte(recheckPrefix(t) + "|" + string(key))
}

// decode parses a stored record. A newer record version is an error (the
// caller treats it as unknown and never rewrites it); nil raw is nil record.
func (s *Store) decode(raw []byte) (*Record, error) {
	if raw == nil {
		return nil, nil
	}
	var rec Record
	if err := json.Unmarshal(raw, &rec); err != nil {
		return nil, fmt.Errorf("media health: decode record: %w", err)
	}
	if rec.V > recordVersion {
		return nil, ErrNewerRecord
	}
	if rec.LegacyDNSFailSince != nil && rec.UnreachableSince == nil {
		rec.UnreachableSince, rec.UnreachableFailures = rec.LegacyDNSFailSince, rec.LegacyDNSFailures
	}
	rec.LegacyDNSFailSince, rec.LegacyDNSFailures = nil, 0
	return &rec, nil
}

// update loads source's record, lets fn mutate it (creating it when
// absent), and writes it back — one Batch transaction, refused under the
// disk guard. fn returning false skips the write.
func (s *Store) update(source string, fn func(rec *Record, existed bool) bool) (*Record, error) {
	if err := s.checkDisk(); err != nil {
		return nil, err
	}
	var out *Record
	err := s.db.Batch(func(tx *bolt.Tx) error {
		var terr error
		out, terr = s.applyTx(tx, source, fn)
		return terr
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// applyTx is one read-modify-write of source's record inside tx.
func (s *Store) applyTx(tx *bolt.Tx, source string, fn func(rec *Record, existed bool) bool) (*Record, error) {
	items := tx.Bucket(bucketItems)
	prev, derr := s.decode(items.Get([]byte(Key(source))))
	if errors.Is(derr, ErrNewerRecord) {
		return nil, derr
	}
	var rec Record
	existed := derr == nil && prev != nil
	if existed {
		rec = *prev
	} else {
		prev = nil
		rec = Record{Source: source}
	}
	if !fn(&rec, existed) {
		return &rec, nil
	}
	rec.Build = s.build
	if err := s.put(tx, prev, &rec); err != nil {
		return nil, err
	}
	return &rec, nil
}

// ProbeWrite pairs a source with the probe result to record for it.
type ProbeWrite struct {
	Source string
	Result ProbeResult
}

// ApplyProbes records many probe results in ONE write transaction, the
// last write per source winning. The cast pass uses it for the fan-out of
// a shared verdict onto wire-identical siblings and for malformed data:
// URIs: one ApplyProbe per source costs one db.Batch wait each (bbolt
// coalesces concurrent callers, not a sequential loop), which turned a
// 2,048-item playlist repeating one source into a 37 s pass against a 10 s
// ceiling (PR #438 review, round 3, F12). Results that write nothing
// (inline, from-store, reason-less inconclusive) are skipped.
func (s *Store) ApplyProbes(writes []ProbeWrite) error {
	if err := s.checkDisk(); err != nil {
		return err
	}
	bySource := make(map[string]ProbeResult, len(writes))
	order := make([]string, 0, len(writes))
	for _, w := range writes {
		if !probeWritesRecord(w.Result) {
			continue
		}
		if _, dup := bySource[w.Source]; !dup {
			order = append(order, w.Source)
		}
		bySource[w.Source] = w.Result
	}
	if len(order) == 0 {
		return nil
	}
	now := s.now()
	return s.db.Update(func(tx *bolt.Tx) error {
		for _, src := range order {
			if _, err := s.applyTx(tx, src, probeMutation(bySource[src], now)); err != nil {
				return err
			}
		}
		return nil
	})
}

// checkDisk enforces the free-space reserve, logging the first refusal of
// an episode and the recovery.
func (s *Store) checkDisk() error {
	if s.minFreeBytes <= 0 {
		return nil
	}
	free, err := s.freeBytes(s.path)
	if err != nil {
		// Unknown free space is not evidence of pressure.
		return nil
	}
	if free < s.minFreeBytes {
		if !s.lowDiskWarn {
			s.lowDiskWarn = true
			s.logger.Warn("media health: disk below reserve; observations are not being recorded",
				zap.Int64("free_bytes", free), zap.Int64("reserve_bytes", s.minFreeBytes))
		}
		return ErrLowDisk
	}
	if s.lowDiskWarn {
		s.lowDiskWarn = false
		s.logger.Info("media health: disk back above reserve; observations resume")
	}
	return nil
}

// ApplyProbe folds an L0 probe result into source's record and returns the
// record as stored (or as it stands, when nothing was written).
//
// Rules:
//   - alive       → State ok, Layer l0, recheck in RecheckOK; consecutive
//     broken reset. The play stats are kept.
//   - dead        → State broken, Layer l0, recheck in RecheckBroken (or
//     RecheckBrokenBackoff after BrokenBackoffAfter consecutive).
//   - inconclusive with a Reason (transient status, timeout, transport)
//     → LastError only, recheck in RetryTransient when the record has no
//     verdict, otherwise untouched. A guard/too-long refusal (no Reason)
//     writes nothing at all: it is policy, not an observation.
//   - inline      → as alive once the payload was checked (Sniffed set): the
//     bytes are the work, and a checked payload is what L0 vouches for.
//   - FromStore   → nothing: the verdict came from this store already.
func (s *Store) ApplyProbe(source string, res ProbeResult) (*Record, error) {
	if !probeWritesRecord(res) {
		return s.Get(source)
	}
	return s.update(source, probeMutation(res, s.now()))
}

// probeWritesRecord says whether a probe result changes the record at all:
// answers served from the store, an inline source whose payload was not
// checked, and reason-less inconclusives (a guard or length refusal — a
// policy objection, never a verdict) write nothing. A checked inline
// payload is recorded ok: before it, a well-formed data: work never had a
// state and read `unknown` forever, however often it played.
func probeWritesRecord(res ProbeResult) bool {
	if res.FromStore || (res.Verdict == VerdictInline && res.Sniffed == "") {
		return false
	}
	return res.Verdict != VerdictInconclusive || res.Reason != ""
}

// probeMutation is the record update one probe result applies.
func probeMutation(res ProbeResult, now time.Time) func(rec *Record, existed bool) bool {
	return func(rec *Record, _ bool) bool {
		if res.WireURL != "" {
			rec.WireURL = res.WireURL
			rec.Host = hostOf(res.WireURL)
		}
		if res.Unreachable == "" {
			// Any answer ends an unreachable streak.
			rec.UnreachableSince, rec.UnreachableFailures = nil, 0
		}
		switch res.Verdict {
		case VerdictAlive, VerdictInline:
			// The source's own gateway served it (ServedBy "") or a fallback
			// did (ServedBy set): either way the record says which one the
			// kiosk should load — for a suspect record too, whose verdict
			// below is left to L1.
			rec.ServedBy = res.ServedBy
			rec.clearIssue()
			rec.HTTPStatus = res.Status
			rec.DeclaredType, rec.SniffedType = res.Declared, res.Sniffed
			rec.RecheckAfter = now.Add(RecheckOK)
			rec.ConsecutiveBroken = 0
			rec.LastError, rec.LastErrorAt = "", nil
			rec.RetryAfter = time.Time{}
			if rec.State == StateSuspect {
				// Valid bytes say nothing about a frame that rendered
				// blank or as an error page: the L0 metadata and recheck
				// are refreshed, the L1 verdict stands until a play
				// success or a real L1 render heals it (PR #443 review,
				// F23).
				return true
			}
			if rec.State != StateOK || (rec.Layer != LayerL1 && rec.Layer != LayerPlay) {
				// Valid bytes say less than render evidence: an ok record a
				// frame or a play proved keeps its layer (l1, play — the
				// app's "verified on this device"); its L0 metadata above
				// is refreshed all the same.
				rec.Layer = LayerL0
			}
			rec.State = StateOK
			rec.Category, rec.Reason = "", ""
			rec.CheckedAt = now
		case VerdictDead:
			concludeBrokenL0(rec, res.Reason, res.Status, res.Detail, now)
			rec.DeclaredType, rec.SniffedType = res.Declared, res.Sniffed
		default:
			// Transient: hold the next attempt, never the verdict. An
			// expired broken verdict stays expired (no skip authority) and
			// the source is left alone for RetryTransient — or for the
			// shorter pause the origin itself asked for (Retry-After on a
			// 429/503): a host saying "30 s" should not cost the source an
			// hour. RetryTransient stays the ceiling.
			hold := RetryTransient
			if res.RetryAfter > 0 && res.RetryAfter < hold {
				hold = res.RetryAfter
			}
			rec.LastError = transientDetail(res)
			rec.LastErrorAt = new(now)
			rec.RetryAfter = now.Add(hold)
			// The honest report of this attempt (Issue). An answer the
			// vocabulary does not name (a 406, 408, 416) still disproves a
			// probe-side issue — the source responded — so it clears one
			// (PR #438 review, F23); a play_failed stands until a real
			// playback success. An attempt with no answer that says nothing
			// about the source (an outage) leaves the issue as it is.
			if issue := issueForProbe(res); issue != "" {
				rec.setIssue(issue, now)
			} else if res.Status > 0 && res.Unreachable == "" && rec.Issue.probeSide() {
				rec.clearIssue()
			}
			if res.Unreachable != "" && res.NetworkHealthy {
				if rec.UnreachableSince == nil {
					rec.UnreachableSince = new(now)
				}
				rec.UnreachableFailures++
				if rec.UnreachableFailures >= UnreachableDeadMinFailures && now.Sub(*rec.UnreachableSince) >= UnreachableDeadAfter {
					// The streak is the corroboration: a day of attempts
					// with no usable answer, each while other origins
					// answered. A slow-but-alive origin never gets here —
					// one answer in the day ends the streak. Re-concluded
					// on every recheck that still fails, which advances the
					// broken backoff like any repeated broken verdict.
					reason, status := ReasonUnreachable, 0
					switch res.Unreachable {
					case UnreachableDNS:
						reason = ReasonDNS
					case UnreachableServer:
						status = res.Status
					}
					concludeBrokenL0(rec, reason, status, rec.LastError, now)
				}
			}
			// An unreachable result while the device reached nothing else
			// neither counts nor ends the streak: it says nothing about
			// this source.
		}
		return true
	}
}

// concludeBrokenL0 writes an L0 broken verdict: the shared tail of a dead
// probe and a concluded DNS streak.
func concludeBrokenL0(rec *Record, reason Reason, status int, detail string, now time.Time) {
	// A verdict replaces any issue: the app shows the category instead.
	rec.clearIssue()
	rec.dropRoute()
	if rec.State == StateBroken && rec.Layer == LayerL0 {
		rec.ConsecutiveBroken++
	} else {
		rec.ConsecutiveBroken = 1
	}
	rec.State = StateBroken
	rec.Layer = LayerL0
	rec.Reason = reason
	rec.Category = CategoryOf(reason, status)
	rec.HTTPStatus = status
	rec.CheckedAt = now
	rec.RecheckAfter = now.Add(brokenRecheck(rec.ConsecutiveBroken))
	rec.LastError = detail
	rec.LastErrorAt = new(now)
	rec.RetryAfter = time.Time{}
}

func brokenRecheck(consecutive int) time.Duration {
	if consecutive >= BrokenBackoffAfter {
		return RecheckBrokenBackoff
	}
	return RecheckBroken
}

func transientDetail(res ProbeResult) string {
	if res.Detail != "" {
		return res.Detail
	}
	if res.Err != nil {
		return res.Err.Error()
	}
	return string(res.Reason)
}

// ApplyOutcome folds one player-reported outcome into source's record.
// count says whether a failing outcome may advance the corroboration
// counters: the Service passes false while the device's own connectivity
// verdict is offline or unknown, and for every failing entry after the
// first of one play (Service.samePlay) — the player reports several stages
// per attempt and retries in place, and one attempt must not corroborate
// itself.
//
// Rules (the corroboration contract):
//   - kind ok    → State ok, Layer play, recheck in RecheckOK; consecutive
//     fail reset. A play success is the strongest positive evidence and
//     heals a broken verdict — whatever count says: a work that rendered
//     (from cache, from a LAN origin) rendered.
//   - kind skip  → Plays.Skipped only.
//   - kind fail, device-local (deviceLocal: webgl, or detect/network) or
//     count=false → Plays.LastFail only; never a verdict and never a count.
//   - kind fail, otherwise → Plays.Fail and ConsecutiveFail advance. The
//     record becomes broken (Layer play) when ConsecutiveFail reaches 2 —
//     two distinct plays — or when an L0 broken verdict is already standing
//     (corroborated either way). A single failure is recorded, not concluded.
func (s *Store) ApplyOutcome(o Outcome, count bool) (*Record, error) {
	if err := s.checkDisk(); err != nil {
		return nil, err
	}
	now := s.now()
	// A ring entry (Seq > 0) is applied at most once for the life of the
	// store: its digest is written to the consumed ring in the SAME
	// transaction as the record update, so a controld restart under a
	// surviving player page — the whole ring drained again by a fresh
	// process with no cursor — re-applies nothing. In-process dedup
	// (Drainer) is not enough: it let a restart turn one failed play into
	// a corroborated broken verdict (PR #438 review, round 4, F15). The
	// inferred fallback (no seq) carries no identity and is bounded by
	// the service's once-per-mount rule instead.
	var digest string
	if o.Seq > 0 {
		digest = outcomeDigest(o)
	}
	var out *Record
	err := s.db.Batch(func(tx *bolt.Tx) error {
		meta := tx.Bucket(bucketMeta)
		var consumed []string
		if digest != "" {
			if raw := meta.Get(metaConsumedKey); raw != nil {
				// An unreadable ring reads as empty: the cost is one
				// possible double count, never a lost observation.
				_ = json.Unmarshal(raw, &consumed)
			}
			if slices.Contains(consumed, digest) {
				rec, derr := s.decode(tx.Bucket(bucketItems).Get([]byte(Key(o.Source))))
				if derr != nil {
					return derr
				}
				out = rec
				return nil
			}
		}
		rec, err := s.applyTx(tx, o.Source, outcomeMutation(o, count, s.contentAddressed(o.Source), s.renderConclusive, now))
		if err != nil {
			return err
		}
		out = rec
		if digest == "" {
			return nil
		}
		consumed = append(consumed, digest)
		if len(consumed) > consumedCap {
			consumed = consumed[len(consumed)-consumedCap:]
		}
		raw, err := json.Marshal(consumed)
		if err != nil {
			return err
		}
		return meta.Put(metaConsumedKey, raw)
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// metaConsumedKey holds the digests of the last consumedCap ring entries
// applied, in order, under bucketMeta.
var metaConsumedKey = []byte("consumed")

// consumedCap is four rings' worth: the player keeps 64 entries, so a
// restart can never show an entry older than the ring that is remembered
// here, whatever was applied in between.
const consumedCap = 256

// outcomeDigest is the stored identity of one ring entry: the same fields
// the Drainer keys on (seq, source, kind, stage, attempt, at), hashed so
// the consumed ring holds no source URLs.
func outcomeDigest(o Outcome) string {
	id := identityOf(o)
	sum := sha256.Sum256([]byte(fmt.Sprintf("%d\x00%s\x00%s\x00%s\x00%d\x00%s", id.Seq, id.Source, id.Kind, id.Stage, id.Attempt, id.At)))
	return hex.EncodeToString(sum[:16])
}

// outcomeMutation is the record update one play outcome applies.
//
// The outcome is dated by the player's own timestamp (Outcome.At), not by
// the drain: the ring is retained by the page, and a daemon restart drains
// entries that may predate a verdict written since (an L0 probe at 12:00,
// then two failed plays from 11:00 drained at 12:05). An outcome older
// than the record's current verdict (CheckedAt) is stale: it still counts
// in the play history the app shows, but it never changes the state,
// never advances the corroboration run, and never refreshes a verdict —
// older evidence does not supersede newer (PR #438 review, round 5, F17).
// contentAddressed reports whether source names an IPFS/Arweave content id
// (whatever gateway it was published on): a gateway's 404 for one says the
// gateway could not serve it, not that the content is gone.
func (s *Store) contentAddressed(source string) bool {
	_, ok := s.gateways.Ref(source)
	return ok
}

func outcomeMutation(o Outcome, count, contentAddressed, renderConclusive bool, now time.Time) func(rec *Record, existed bool) bool {
	return func(rec *Record, _ bool) bool {
		at := outcomeTime(o, now)
		stale := !rec.CheckedAt.IsZero() && at.Before(rec.CheckedAt)
		if o.frame != nil {
			// A capture of the wall: record what it saw. A frame is not a
			// play of its own — the player's `ready` for the same mount
			// already counted that.
			rec.Wall = &WallObservation{
				Verdict: RenderVerdict(o.Code), PHash: fmt.Sprintf("%016x", o.frame.PHash),
				Variance: o.frame.Variance, Label: o.frame.Label, Clipped: o.frame.Clipped, At: at,
			}
		}
		switch o.Kind {
		case OutcomeOK:
			if o.Stage == StageFrame {
				rec.renderedFrame(at, LayerPlay)
				return true
			}
			if o.errorPage {
				// The embed showed the error page the content-type check
				// already reported: not a success, and it must not erase the
				// failure run before it. Without this, a work refused with
				// 429 on every play (12 plays on the office FF1) reset its
				// own run each time and was never marked broken.
				return true
			}
			rec.Plays.OK++
			rec.Plays.LastOKAt = laterOf(rec.Plays.LastOKAt, at)
			if o.MediaType != "" {
				rec.MediaType = o.MediaType
			}
			if stale {
				return true
			}
			if !readyIsEvidence(o) && rec.frameRunOpen() {
				// An opaque embed's ready says nothing about a blank frame:
				// the frame run (and its play_failed) stands (frameRunOpen).
				return true
			}
			rec.Plays.ConsecutiveFail = 0
			if rec.Issue == IssuePlayFailed {
				// play_failed reports a failure run short of the verdict;
				// the run just ended, so the issue goes with it. This holds
				// for an iframe ready too: it cannot heal a verdict, but it
				// does reset the run (the next failure counts from one), and
				// an issue kept past that never cleared at all — no probe
				// answer or kiosk 200 clears a play issue, and the verdict
				// at two can no longer come. PENDING #37 showed play_failed
				// through ten good plays (office FF1, 2026-11-09). An error
				// page's ready returned above and keeps the run and issue.
				rec.clearIssue()
			}
			if !readyIsEvidence(o) {
				// Counted as a play, but proves nothing about the work: no
				// ok, no heal, no recheck push (a probe-side issue stands).
				// An iframe/object/PDF reports ready for whatever document
				// loaded in it — Chromium's own error page included — so its
				// ready proves the mount, not the work. Counted as a play,
				// but it neither sets ok, nor heals a broken verdict, nor
				// pushes the recheck out: the record stays due and L0, the
				// layer that actually reads the bytes, keeps looking. A
				// ready that wrote ok here hid an unresolvable fxhash work
				// for a week (#392, office FF1, 2026-11-05). For the same
				// reason it neither heals an L1 suspect nor resets the L1
				// blank run below: a blank frame reports ready too.
				return true
			}
			if rec.L1 != nil {
				// A fresh successful play ends an L1 blank run too: a
				// recovered work must earn a new long confirmation before
				// it can be suspect again (PR #443 review, F25). Only a play
				// that proves a render counts (readyIsEvidence, above).
				rec.L1.ConsecutiveBlank = 0
			}
			rec.clearIssue()
			rec.State = StateOK
			rec.Layer = LayerPlay
			rec.Category, rec.Reason = "", ""
			rec.CheckedAt = at
			rec.RecheckAfter = at.Add(RecheckOK)
			rec.ConsecutiveBroken = 0
			rec.LastError, rec.LastErrorAt = "", nil
			return true
		case OutcomeSkip:
			rec.Plays.Skipped++
			return true
		case OutcomeFail:
			if o.Stage == StageFrame && rec.fetchRunOpen() {
				// The play on screen is the one whose content-type check
				// failed (a clean play's ready would have ended that run),
				// so this frame is of its error page: the same play, not a
				// second failure — and the fetch reason stands (both layers
				// failing report the fetch one). Recorded under `wall`
				// above; the run and LastFail are left as they are.
				return true
			}
			if rec.Plays.LastFailAt == nil || !at.Before(*rec.Plays.LastFailAt) {
				rec.Plays.LastFail = &PlayFailure{Stage: o.Stage, Code: o.Code, MediaType: o.MediaType, Attempt: o.Attempt, At: at}
				rec.Plays.LastFailAt = new(at)
			}
			if o.MediaType != "" {
				rec.MediaType = o.MediaType
			}
			reason := outcomeReason(o)
			if reason == "" || deviceLocal(o) || !count {
				// Recorded for the app and the support bundle; the source
				// is not the thing that failed (or nothing could be told
				// about it while the device was offline).
				return true
			}
			rec.Plays.Fail++
			if stale {
				// History only: a failure from before the current verdict
				// cannot corroborate against it.
				return true
			}
			rec.Plays.ConsecutiveFail++
			corroborated := rec.Plays.ConsecutiveFail >= 2 ||
				(rec.State == StateBroken && rec.Layer == LayerL0 && at.Before(rec.RecheckAfter)) ||
				(o.Stage == StageFrame && rec.l1Negative())
			if !corroborated {
				rec.setIssue(IssuePlayFailed, at)
				return true
			}
			rec.clearIssue()
			if o.Stage == StageFrame {
				// Two plays whose frame on the wall read blank (or known
				// bad), or one over a standing L1 negative: a render
				// verdict — broken when render verdicts are conclusive,
				// suspect (advisory) otherwise.
				rec.concludeRender(reason, LayerPlay, at, renderConclusive)
				return true
			}
			// Backoff, as for L0: a work the player has re-concluded broken
			// on BrokenBackoffAfter consecutive rechecks is retried weekly,
			// not daily — each daily retry of a dead iframe costs the wall
			// three 45 s attempts. Only a re-conclusion AFTER the previous
			// verdict expired advances it (repeat-one can replay a broken
			// work inside its window; those plays must not burn the week).
			switch {
			case rec.State == StateBroken && rec.Layer == LayerPlay && !at.Before(rec.RecheckAfter):
				rec.ConsecutiveBroken++
			case rec.State == StateBroken && rec.Layer == LayerPlay:
				// Still inside the window: keep the count.
			default:
				rec.ConsecutiveBroken = 1
			}
			rec.State = StateBroken
			rec.Layer = LayerPlay
			rec.dropRoute()
			rec.Reason = reason
			status := 0
			if o.Stage == StageDetect {
				status, _ = strconv.Atoi(o.Code)
			}
			rec.Category = playCategory(reason, status, contentAddressed)
			rec.HTTPStatus = status
			rec.CheckedAt = at
			rec.RecheckAfter = at.Add(brokenRecheck(rec.ConsecutiveBroken))
			rec.LastError = fmt.Sprintf("player: %s %s", o.Stage, o.Code)
			rec.LastErrorAt = new(at)
			return true
		}
		return false
	}
}

// outcomeTime is when the player observed the outcome: its own RFC 3339
// timestamp when present, parseable and not after now (a player clock
// ahead of the device must not date a verdict into the future, which
// would stretch its recheck window); otherwise the drain time.
func outcomeTime(o Outcome, now time.Time) time.Time {
	if o.At == "" {
		return now
	}
	at, err := time.Parse(time.RFC3339Nano, o.At)
	if err != nil || at.After(now) {
		return now
	}
	return at
}

func laterOf(have *time.Time, t time.Time) *time.Time {
	if have != nil && have.After(t) {
		return have
	}
	return new(t)
}

// playCategory maps a play-layer conclusion to its category: a detect-stage
// 4xx is the kiosk's own answer, so 401/403 are source_blocked and
// 404/410-class source_missing; a 5xx means the origin is up but failing,
// which (corroborated across plays) is render_failed, like every other
// stage.
//
// The exception is a 404/410-class answer for a content-addressed source:
// that is one gateway's answer, and the play layer has no second gateway
// to confirm it (L0's confirmAbsence does, and an L0 verdict that confirmed
// absence is source_missing). The verdict stands — the work does not play
// here — but it reads source_unreachable, the same as L0's unconfirmed
// gateway 404 (arweave.net 404ing an on-chain transaction to the player,
// office FF1, 2026-11-09).
func playCategory(reason Reason, status int, contentAddressed bool) Category {
	if reason == ReasonRenderDetect && status >= 400 && status < 500 {
		category := CategoryOf(ReasonHTTPStatus, status)
		if category == CategorySourceMissing && contentAddressed {
			return CategorySourceUnreachable
		}
		return category
	}
	return CategoryOf(reason, status)
}

// ApplyKioskAnswer folds a 2xx DOCUMENT the kiosk itself loaded for source
// (the navigation observer): the origin served the kiosk, so a probe-side
// issue (no answer, DNS, 5xx, rate limit, access denied) is no longer the
// truth and the unreachable streak ends. It never writes ok — a 200 proves
// the document arrived, not that the work rendered — and a play_failed
// issue stands: that one is about rendering. Writes only when something
// changes; a source with no record is left alone (nothing to correct).
func (s *Store) ApplyKioskAnswer(source string) error {
	rec, err := s.Get(source)
	if err != nil || rec == nil {
		return err
	}
	if !rec.Issue.probeSide() && rec.UnreachableSince == nil {
		return nil
	}
	_, err = s.update(source, func(rec *Record, existed bool) bool {
		if !existed {
			return false
		}
		changed := false
		if rec.Issue.probeSide() {
			rec.clearIssue()
			changed = true
		}
		if rec.UnreachableSince != nil {
			rec.UnreachableSince, rec.UnreachableFailures = nil, 0
			changed = true
		}
		return changed
	})
	return err
}
