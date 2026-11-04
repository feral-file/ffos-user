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
}

// Options tunes Open.
type Options struct {
	// Now is the clock (time.Now when nil).
	Now func() time.Time
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
	metaSchemaKey = []byte("schema")
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
		for _, b := range [][]byte{bucketItems, bucketRecheck, bucketMeta} {
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
// retry) is at or before now, earliest first, skipping any in exclude. The
// exclusion is applied INSIDE the scan so a handful of permanently excluded
// rows at the head of the index (the items on the wall) cannot shadow
// eligible rows behind them.
func (s *Store) Due(now time.Time, limit int, exclude map[string]struct{}) ([]string, error) {
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
			if _, skip := exclude[rec.Source]; skip {
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
	return recheck.Put(recheckKey(rec.dueAt(), key), []byte{})
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
		switch res.Verdict {
		case VerdictAlive:
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
			if rec.State == StateBroken && rec.Layer == LayerL0 {
				rec.ConsecutiveBroken++
			} else {
				rec.ConsecutiveBroken = 1
			}
			rec.State = StateBroken
			rec.Layer = LayerL0
			rec.Reason = res.Reason
			rec.Category = CategoryOf(res.Reason, res.Status)
			rec.HTTPStatus = res.Status
			rec.DeclaredType, rec.SniffedType = res.Declared, res.Sniffed
			rec.CheckedAt = now
			rec.RecheckAfter = now.Add(brokenRecheck(rec.ConsecutiveBroken))
			rec.LastError = res.Detail
			rec.LastErrorAt = new(now)
			rec.RetryAfter = time.Time{}
		default:
			// Transient: hold the next attempt, never the verdict. An
			// expired broken verdict stays expired (no skip authority) and
			// the source is left alone for RetryTransient.
			rec.LastError = transientDetail(res)
			rec.LastErrorAt = new(now)
			rec.RetryAfter = now.Add(RetryTransient)
		}
		return true
	}
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
func outcomeMutation(o Outcome, count bool, now time.Time) func(rec *Record, existed bool) bool {
	return func(rec *Record, _ bool) bool {
		switch o.Kind {
		case OutcomeOK:
			rec.Plays.OK++
			rec.Plays.ConsecutiveFail = 0
			rec.Plays.LastOKAt = new(now)
			rec.State = StateOK
			rec.Layer = LayerPlay
			rec.Category, rec.Reason = "", ""
			rec.CheckedAt = now
			rec.RecheckAfter = now.Add(RecheckOK)
			rec.ConsecutiveBroken = 0
			rec.LastError, rec.LastErrorAt = "", nil
			if o.MediaType != "" {
				rec.MediaType = o.MediaType
			}
			return true
		case OutcomeSkip:
			rec.Plays.Skipped++
			return true
		case OutcomeFail:
			fail := &PlayFailure{Stage: o.Stage, Code: o.Code, MediaType: o.MediaType, Attempt: o.Attempt, At: now}
			rec.Plays.LastFail = fail
			rec.Plays.LastFailAt = new(now)
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
			rec.Plays.ConsecutiveFail++
			corroborated := rec.Plays.ConsecutiveFail >= 2 ||
				(rec.State == StateBroken && rec.Layer == LayerL0 && now.Before(rec.RecheckAfter))
			if !corroborated {
				return true
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
			rec.CheckedAt = now
			rec.RecheckAfter = now.Add(RecheckBroken)
			rec.LastError = fmt.Sprintf("player: %s %s", o.Stage, o.Code)
			rec.LastErrorAt = new(now)
			return true
		}
		return false
	}
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
