package mediahealth

import (
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
	// servedByMu guards servedBy, the in-memory mirror of bucketServedBy:
	// read per kiosk request (GatewayOverride), written after a record's
	// ServedBy changes.
	servedByMu sync.RWMutex
	servedBy   map[string]string
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
	// bucketServedBy indexes content id ("ar:<tx>", "ipfs:<cid>") to the
	// gateway origin the kiosk loads it through (Record.ServedBy), so the
	// interceptor's per-request lookup never scans records. Mirrored in
	// memory (Store.servedBy) at Open.
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
		db:           db,
		path:         path,
		now:          opts.Now,
		build:        opts.Build,
		logger:       opts.Logger,
		minFreeBytes: opts.MinFreeBytes,
		freeBytes:    opts.FreeBytes,
		gateways:     opts.Gateways,
		servedBy:     map[string]string{},
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
		err = db.View(func(tx *bolt.Tx) error {
			return tx.Bucket(bucketServedBy).ForEach(func(k, v []byte) error {
				// Only route keys (content@host): an entry an earlier build
				// keyed by content alone can no longer be matched, and is
				// dropped by the next write for its source.
				if strings.Contains(string(k), "@") {
					s.servedBy[string(k)] = string(v)
				}
				return nil
			})
		})
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

// indexServedBy keeps bucketServedBy and its in-memory mirror in step with
// a record's ServedBy. Keyed by route (ContentRef.RouteKey: the content
// and the gateway it was requested through), so a source on another
// gateway naming the same CID keeps its own override; fragment and query
// variants on one gateway share an entry, and a clear from any of them
// (that gateway served it again, or the content is confirmed missing)
// clears it for all of them — correctly, since it is the same gateway. The mirror is updated inside the
// write transaction: a rolled-back write leaves it ahead of the file until
// the next write for that source, which only affects which gateway the
// kiosk tries, never a verdict.
func (s *Store) indexServedBy(tx *bolt.Tx, prev, rec *Record) error {
	prevBy := ""
	if prev != nil {
		prevBy = prev.ServedBy
	}
	if prevBy == rec.ServedBy {
		return nil
	}
	ref, ok := s.gateways.Ref(rec.Source)
	if !ok {
		return nil
	}
	b := tx.Bucket(bucketServedBy)
	s.servedByMu.Lock()
	defer s.servedByMu.Unlock()
	key := ref.RouteKey()
	// Drop an entry an earlier build keyed by content alone.
	if err := b.Delete([]byte(ref.Key())); err != nil {
		return err
	}
	if rec.ServedBy == "" {
		delete(s.servedBy, key)
		return b.Delete([]byte(key))
	}
	s.servedBy[key] = rec.ServedBy
	return b.Put([]byte(key), []byte(rec.ServedBy))
}

// GatewayOverride answers the kiosk interceptor: for a request URL whose
// content (an IPFS CID or an Arweave transaction, under any gateway, any
// path below it) is recorded as served by a fallback gateway, the same
// path and query on that gateway. Never a URL from playlist content: the
// origin comes from the operator's gateway list via the probe, and only
// the request's own path and query are carried over. A request already on
// the recorded gateway is left alone.
func (s *Store) GatewayOverride(rawURL string) (string, bool) {
	ref, ok := s.gateways.Ref(rawURL)
	if !ok {
		return "", false
	}
	s.servedByMu.RLock()
	origin, found := s.servedBy[ref.RouteKey()]
	s.servedByMu.RUnlock()
	if !found || strings.EqualFold(hostOf(origin), ref.Host) {
		return "", false
	}
	return ref.URLOn(origin), true
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
//   - inline      → nothing: inline items have no origin to observe.
//   - FromStore   → nothing: the verdict came from this store already.
func (s *Store) ApplyProbe(source string, res ProbeResult) (*Record, error) {
	if !probeWritesRecord(res) {
		return s.Get(source)
	}
	return s.update(source, probeMutation(res, s.now()))
}

// probeWritesRecord says whether a probe result changes the record at all:
// inline sources, answers served from the store, and reason-less
// inconclusives (a guard or length refusal — a policy objection, never a
// verdict) write nothing.
func probeWritesRecord(res ProbeResult) bool {
	if res.FromStore || res.Verdict == VerdictInline {
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
		case VerdictAlive:
			// The source's own gateway served it (ServedBy "") or a fallback
			// did (ServedBy set): either way the record says which one the
			// kiosk should load.
			rec.ServedBy = res.ServedBy
			rec.clearIssue()
			rec.State = StateOK
			rec.Layer = LayerL0
			rec.Category, rec.Reason = "", ""
			rec.HTTPStatus = res.Status
			rec.DeclaredType, rec.SniffedType = res.Declared, res.Sniffed
			rec.CheckedAt = now
			rec.RecheckAfter = now.Add(RecheckOK)
			rec.ConsecutiveBroken = 0
			rec.LastError, rec.LastErrorAt = "", nil
			rec.RetryAfter = time.Time{}
		case VerdictDead:
			rec.ServedBy = ""
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
		rec, err := s.applyTx(tx, o.Source, outcomeMutation(o, count, now))
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
func outcomeMutation(o Outcome, count bool, now time.Time) func(rec *Record, existed bool) bool {
	return func(rec *Record, _ bool) bool {
		at := outcomeTime(o, now)
		stale := !rec.CheckedAt.IsZero() && at.Before(rec.CheckedAt)
		switch o.Kind {
		case OutcomeOK:
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
			rec.Plays.ConsecutiveFail = 0
			if !readyIsEvidence(o) {
				// Counted as a play, but proves nothing: any issue stands.
				// An iframe/object/PDF reports ready for whatever document
				// loaded in it — Chromium's own error page included — so its
				// ready proves the mount, not the work. Counted as a play,
				// but it neither sets ok, nor heals a broken verdict, nor
				// pushes the recheck out: the record stays due and L0, the
				// layer that actually reads the bytes, keeps looking. A
				// ready that wrote ok here hid an unresolvable fxhash work
				// for a week (#392, office FF1, 2026-11-05).
				return true
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
			if rec.Plays.LastFailAt == nil || !at.Before(*rec.Plays.LastFailAt) {
				rec.Plays.LastFail = &PlayFailure{Stage: o.Stage, Code: o.Code, MediaType: o.MediaType, Attempt: o.Attempt, At: at}
				rec.Plays.LastFailAt = new(at)
			}
			if o.MediaType != "" {
				rec.MediaType = o.MediaType
			}
			reason := reasonForStage(o.Stage)
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
				(rec.State == StateBroken && rec.Layer == LayerL0 && at.Before(rec.RecheckAfter))
			if !corroborated {
				rec.setIssue(IssuePlayFailed, at)
				return true
			}
			rec.clearIssue()
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
			rec.Reason = reason
			status := 0
			if o.Stage == StageDetect {
				status, _ = strconv.Atoi(o.Code)
			}
			rec.Category = playCategory(reason, status)
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
func playCategory(reason Reason, status int) Category {
	if reason == ReasonRenderDetect && status >= 400 && status < 500 {
		return CategoryOf(ReasonHTTPStatus, status)
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
