package netlog

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// DefaultDir is the on-device ring location. Under ~/.logs deliberately
	// (plan branch B1): uploadLogs already walks it recursively, so segments
	// ride every support bundle with zero uploader change, and the whole path
	// ships on the package rail. log-rotation.sh cannot copy-truncate these
	// files — it rotates only the exact paths listed in log_paths.conf, and
	// this subdirectory is not one of them (verified against the script).
	// Cost, accepted for the first release: /home is discarded on OTA
	// subvolume promotion (plan branch B2 is the deferred alternative).
	DefaultDir = "/home/feralfile/.logs/netlog"

	// DefaultMaxTotalBytes caps the whole ring. Far below uploadLogs'
	// 128 MB bundle budget on purpose: the recorder must never evict other
	// logs from a support bundle (its per-file size also stays far under the
	// uploader's oversized-file skip).
	DefaultMaxTotalBytes = 8 << 20

	// defaultMaxSegmentBytes bounds one segment file. Small segments make
	// eviction granular (dropping the oldest loses minutes, not days) and
	// keep the active file's loss window on crash small.
	defaultMaxSegmentBytes = 512 << 10

	// defaultMaxSegmentAge prunes segments older than the log-rotation
	// regime's own 7-day retention — the ring must not become the one place
	// stale data outlives every other log.
	defaultMaxSegmentAge = 7 * 24 * time.Hour

	// segmentPrefix/segmentExt name segments netlog-<seq>.jsonl. The name is
	// load-bearing twice over: log-rotation.sh must keep ignoring it (see
	// DefaultDir), and reopen-after-crash orders segments by the numeric seq,
	// not mtime, so a clock step cannot shuffle the ring's order.
	segmentPrefix = "netlog-"
	segmentExt    = ".jsonl"
)

// Ring is the crash-tolerant JSONL segment store. Single-writer by contract:
// only the recorder's worker goroutine appends (mu still guards against a
// misuse, cheaply). Every append is fsynced — events are transitions, not
// ticks, so durability per record costs nothing measurable, and the record
// being written is usually the one describing the outage that may kill the
// power next.
type Ring struct {
	mu sync.Mutex

	dir             string
	maxTotalBytes   int64
	maxSegmentBytes int64
	maxSegmentAge   time.Duration

	active     *os.File
	activeSize int64
	activeSeq  int
	// closed marks an explicit Close. It exists to tell "sealed on purpose"
	// apart from "the last roll failed to open a successor" (ENOSPC on a
	// /home the unbounded daemon logs can genuinely fill, EROFS, EMFILE):
	// the former is final, the latter must stay retryable — a device that
	// flaps hard enough to fill its disk is precisely the device whose
	// timeline is wanted once log rotation frees space again.
	closed bool
}

// OpenRing opens (creating if needed) the ring at dir. maxTotalBytes <= 0
// uses DefaultMaxTotalBytes. On reopen after a crash it appends to the newest
// segment; a torn final record is tolerated by writing a fresh newline first,
// so the next record never concatenates onto half a line (readers skip the
// unparsable line, losing exactly one record).
func OpenRing(dir string, maxTotalBytes int64) (*Ring, error) {
	if maxTotalBytes <= 0 {
		maxTotalBytes = DefaultMaxTotalBytes
	}
	r := &Ring{
		dir:             dir,
		maxTotalBytes:   maxTotalBytes,
		maxSegmentBytes: defaultMaxSegmentBytes,
		maxSegmentAge:   defaultMaxSegmentAge,
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("netlog: create ring dir: %w", err)
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	r.pruneLocked()
	segs, err := r.segmentsLocked()
	if err != nil {
		return nil, err
	}
	if len(segs) == 0 {
		return r, r.openSegmentLocked(1)
	}
	newest := segs[len(segs)-1]
	f, err := os.OpenFile(filepath.Join(dir, newest.name), os.O_WRONLY|os.O_APPEND, 0o600) //nolint:gosec // G304: dir is the fixed ring directory; name passed parseSegmentName
	if err != nil {
		return nil, fmt.Errorf("netlog: reopen segment: %w", err)
	}
	r.active, r.activeSize, r.activeSeq = f, newest.size, newest.seq
	if err := r.isolateTornTailLocked(filepath.Join(dir, newest.name)); err != nil {
		// Best-effort: a failed tail check degrades to possibly losing the
		// first appended record to the same unparsable line, never to a
		// write failure.
		_ = err
	}
	return r, nil
}

// Append writes one record as a JSONL line, rolling and evicting as needed.
func (r *Ring) Append(rec Record) error {
	line, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("netlog: marshal record: %w", err)
	}
	line = append(line, '\n')

	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.ensureActiveLocked(); err != nil {
		return err
	}
	if r.activeSize+int64(len(line)) > r.maxSegmentBytes && r.activeSize > 0 {
		if err := r.rollLocked(); err != nil {
			return err
		}
	}
	n, err := r.active.Write(line)
	r.activeSize += int64(n)
	if err != nil {
		return fmt.Errorf("netlog: append: %w", err)
	}
	// fsync per record: see Ring's doc for why this trade-off is accepted.
	if err := r.active.Sync(); err != nil {
		return fmt.Errorf("netlog: sync: %w", err)
	}
	r.enforceCapLocked()
	return nil
}

// Roll closes the active segment and starts a fresh one. The recorder calls
// it when an outage episode closes so a whole episode lands in sealed
// segments — the natural upload unit.
func (r *Ring) Roll() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.ensureActiveLocked(); err != nil {
		return err
	}
	if r.activeSize == 0 {
		return nil // nothing to seal; avoid minting empty segments on flap
	}
	return r.rollLocked()
}

// ensureActiveLocked recovers from a previously failed roll (see the closed
// field's doc): when the ring is not explicitly closed but has no active
// segment, it retries opening the successor so one transient filesystem
// error never silences the recorder for the rest of the process lifetime.
func (r *Ring) ensureActiveLocked() error {
	if r.closed {
		return fmt.Errorf("netlog: ring closed")
	}
	if r.active != nil {
		return nil
	}
	if err := r.openSegmentLocked(r.activeSeq + 1); err != nil {
		return fmt.Errorf("netlog: reopen after failed roll: %w", err)
	}
	return nil
}

// Close seals the ring. Idempotent.
func (r *Ring) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closed = true
	if r.active == nil {
		return nil
	}
	err := r.active.Sync()
	if cerr := r.active.Close(); err == nil {
		err = cerr
	}
	r.active = nil
	return err
}

// Dir exposes the ring location (wiring/logging convenience).
func (r *Ring) Dir() string { return r.dir }

// --- internals (all *Locked helpers require r.mu held) ---

type segmentInfo struct {
	name string
	seq  int
	size int64
	mod  time.Time
}

// segmentsLocked lists ring segments sorted by seq ascending.
func (r *Ring) segmentsLocked() ([]segmentInfo, error) {
	entries, err := os.ReadDir(r.dir)
	if err != nil {
		return nil, fmt.Errorf("netlog: list segments: %w", err)
	}
	var segs []segmentInfo
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		seq, ok := parseSegmentName(e.Name())
		if !ok {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		segs = append(segs, segmentInfo{name: e.Name(), seq: seq, size: info.Size(), mod: info.ModTime()})
	}
	sort.Slice(segs, func(i, j int) bool { return segs[i].seq < segs[j].seq })
	return segs, nil
}

func parseSegmentName(name string) (int, bool) {
	if !strings.HasPrefix(name, segmentPrefix) || !strings.HasSuffix(name, segmentExt) {
		return 0, false
	}
	seq, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(name, segmentPrefix), segmentExt))
	if err != nil || seq <= 0 {
		return 0, false
	}
	return seq, true
}

func segmentName(seq int) string {
	return fmt.Sprintf("%s%06d%s", segmentPrefix, seq, segmentExt)
}

func (r *Ring) openSegmentLocked(seq int) error {
	f, err := os.OpenFile(filepath.Join(r.dir, segmentName(seq)), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("netlog: open segment: %w", err)
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return fmt.Errorf("netlog: stat segment: %w", err)
	}
	r.active, r.activeSize, r.activeSeq = f, info.Size(), seq
	return nil
}

func (r *Ring) rollLocked() error {
	_ = r.active.Sync()
	_ = r.active.Close()
	r.active = nil
	if err := r.openSegmentLocked(r.activeSeq + 1); err != nil {
		return err
	}
	r.pruneLocked()
	r.enforceCapLocked()
	return nil
}

// isolateTornTailLocked appends a lone newline when the active segment does
// not end with one (a crash mid-write), so the next record starts on a fresh
// line. Readers tolerate the resulting unparsable line.
func (r *Ring) isolateTornTailLocked(path string) error {
	if r.activeSize == 0 {
		return nil
	}
	f, err := os.Open(path) //nolint:gosec // G304: path is the ring directory's own active segment
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	buf := make([]byte, 1)
	if _, err := f.ReadAt(buf, r.activeSize-1); err != nil {
		return err
	}
	if buf[0] == '\n' {
		return nil
	}
	n, err := r.active.Write([]byte("\n"))
	r.activeSize += int64(n)
	return err
}

// enforceCapLocked deletes oldest sealed segments until the ring fits the
// total cap. The active segment is never deleted — the cap arithmetic
// guarantees headroom because maxSegmentBytes is a small fraction of the cap.
func (r *Ring) enforceCapLocked() {
	segs, err := r.segmentsLocked()
	if err != nil {
		return
	}
	var total int64
	for _, s := range segs {
		total += s.size
	}
	for _, s := range segs {
		if total <= r.maxTotalBytes {
			return
		}
		if s.seq == r.activeSeq {
			return
		}
		if err := os.Remove(filepath.Join(r.dir, s.name)); err == nil {
			total -= s.size
		}
	}
}

// pruneLocked removes sealed segments older than maxSegmentAge (the ring's
// own 7-day retention, matching the device's log-rotation regime).
func (r *Ring) pruneLocked() {
	segs, err := r.segmentsLocked()
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-r.maxSegmentAge)
	for _, s := range segs {
		if s.seq == r.activeSeq && r.active != nil {
			continue
		}
		if s.mod.Before(cutoff) {
			_ = os.Remove(filepath.Join(r.dir, s.name))
		}
	}
}
