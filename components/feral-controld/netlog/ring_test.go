package netlog

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// readRing parses every line of every segment, returning parsed records and
// the count of unparsable (torn) lines — readers must tolerate those.
func readRing(t *testing.T, dir string) (recs []Record, torn int) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	for _, e := range entries {
		if _, ok := parseSegmentName(e.Name()); !ok {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name())) //nolint:gosec // G304: test reads its own t.TempDir
		require.NoError(t, err)
		for _, line := range strings.Split(string(data), "\n") {
			if strings.TrimSpace(line) == "" {
				continue
			}
			var rec Record
			if err := json.Unmarshal([]byte(line), &rec); err != nil {
				torn++
				continue
			}
			recs = append(recs, rec)
		}
	}
	return recs, torn
}

func note(s string) Record {
	return Record{Stamp: Stamp{Wall: time.Unix(1700000000, 0)}, Kind: KindNote, Note: s}
}

func TestRingAppendAndReadBack(t *testing.T) {
	dir := t.TempDir()
	r, err := OpenRing(dir, 0)
	require.NoError(t, err)
	defer func() { _ = r.Close() }()

	require.NoError(t, r.Append(note("a")))
	require.NoError(t, r.Append(note("b")))

	recs, torn := readRing(t, dir)
	assert.Equal(t, 0, torn)
	require.Len(t, recs, 2)
	assert.Equal(t, "a", recs[0].Note)
	assert.Equal(t, "b", recs[1].Note)
}

// TestRingSizeCapEvictsOldest pins the eviction contract: the ring stays
// under its total cap by deleting the OLDEST sealed segments, and the active
// segment is never deleted.
func TestRingSizeCapEvictsOldest(t *testing.T) {
	dir := t.TempDir()
	r, err := OpenRing(dir, 2048)
	require.NoError(t, err)
	defer func() { _ = r.Close() }()
	r.maxSegmentBytes = 512

	for i := 0; i < 100; i++ {
		require.NoError(t, r.Append(note(strings.Repeat("x", 64))))
	}

	segs, err := r.segmentsLocked()
	require.NoError(t, err)
	require.NotEmpty(t, segs)
	var total int64
	for _, s := range segs {
		total += s.size
	}
	// Cap is enforced after each roll; the active segment may still grow past
	// it by at most one segment's worth.
	assert.LessOrEqual(t, total, int64(2048+512), "ring must stay near its cap")
	assert.Greater(t, segs[0].seq, 1, "oldest segments must have been evicted")
	assert.Equal(t, r.activeSeq, segs[len(segs)-1].seq, "active segment must survive eviction")
}

// TestRingReopenToleratesTornTail: a crash mid-write leaves a half line; on
// reopen the next record must start on a fresh line, losing exactly the torn
// record and nothing else.
func TestRingReopenToleratesTornTail(t *testing.T) {
	dir := t.TempDir()
	r, err := OpenRing(dir, 0)
	require.NoError(t, err)
	require.NoError(t, r.Append(note("before-crash")))
	require.NoError(t, r.Close())

	// Simulate the torn write: append half a JSON record with no newline.
	f, err := os.OpenFile(filepath.Join(dir, segmentName(1)), os.O_WRONLY|os.O_APPEND, 0o600) //nolint:gosec // G304: test writes its own t.TempDir
	require.NoError(t, err)
	_, err = f.WriteString(`{"kind":"note","note":"torn`)
	require.NoError(t, err)
	require.NoError(t, f.Close())

	r2, err := OpenRing(dir, 0)
	require.NoError(t, err)
	defer func() { _ = r2.Close() }()
	require.NoError(t, r2.Append(note("after-crash")))

	recs, torn := readRing(t, dir)
	assert.Equal(t, 1, torn, "exactly the torn line is lost")
	require.Len(t, recs, 2)
	assert.Equal(t, "before-crash", recs[0].Note)
	assert.Equal(t, "after-crash", recs[1].Note)
}

func TestRingRollSealsSegment(t *testing.T) {
	dir := t.TempDir()
	r, err := OpenRing(dir, 0)
	require.NoError(t, err)
	defer func() { _ = r.Close() }()

	// Empty roll mints nothing (flap protection).
	require.NoError(t, r.Roll())
	segs, _ := r.segmentsLocked()
	assert.Len(t, segs, 1)

	require.NoError(t, r.Append(note("episode")))
	require.NoError(t, r.Roll())
	segs, _ = r.segmentsLocked()
	assert.Len(t, segs, 2, "roll after writes must seal and open fresh")
	assert.Equal(t, 2, r.activeSeq)
}

// TestRingSegmentCountCapEvictsOldest pins the count cap alongside the byte
// cap: one segment is minted per closed outage episode, and the cap must be
// sized so a flapping device's episodes survive until the 6-hourly
// self-upload — a 64-entry first cut evicted classified episodes before any
// bundle shipped them (the retention arithmetic lives on defaultMaxSegments).
func TestRingSegmentCountCapEvictsOldest(t *testing.T) {
	dir := t.TempDir()
	r, err := OpenRing(dir, 0)
	require.NoError(t, err)
	defer func() { _ = r.Close() }()
	r.maxSegments = 5

	// Mint 12 episode-shaped segments (one small record each, sealed by Roll).
	for i := 0; i < 12; i++ {
		require.NoError(t, r.Append(note("episode")))
		require.NoError(t, r.Roll())
	}

	segs, err := r.segmentsLocked()
	require.NoError(t, err)
	assert.LessOrEqual(t, len(segs), 5, "count cap must evict")
	assert.Equal(t, r.activeSeq, segs[len(segs)-1].seq, "active segment must survive")
	assert.Greater(t, segs[0].seq, 1, "oldest segments must be the ones evicted")

	// The default must hold well more than the episodes accumulated between
	// self-uploads on a fast flapper (~120 at a 3-minute cycle over 6h).
	assert.GreaterOrEqual(t, defaultMaxSegments, 512,
		"default count cap must not evict a flapping device's episodes between self-uploads")
}

// TestRingRecoversAfterFailedRoll: a transient filesystem error at roll time
// (ENOSPC/EROFS-shaped: the ring dir made unwritable) must not silence the
// recorder forever — the next Append after the condition clears reopens a
// successor segment. Explicit Close stays final.
func TestRingRecoversAfterFailedRoll(t *testing.T) {
	dir := t.TempDir()
	r, err := OpenRing(dir, 0)
	require.NoError(t, err)
	defer func() { _ = r.Close() }()
	require.NoError(t, r.Append(note("episode")))

	// Make creating the successor fail, then roll.
	require.NoError(t, os.Chmod(dir, 0o500)) //nolint:gosec // G302: directory perms, deliberately unwritable to force the roll failure
	rollErr := r.Roll()
	if rollErr == nil {
		// Running as root (or on a platform ignoring the chmod) the roll
		// succeeds and there is nothing to recover from — skip rather than
		// assert a failure that did not happen.
		require.NoError(t, os.Chmod(dir, 0o750)) //nolint:gosec // G302: restoring directory perms
		t.Skip("chmod did not make the ring dir unwritable in this environment")
	}

	// While the condition persists, appends fail loudly (never silently).
	require.Error(t, r.Append(note("during-outage")))

	// Condition clears: the very next append must recover.
	require.NoError(t, os.Chmod(dir, 0o750)) //nolint:gosec // G302: directory perms (0750 matches the ring's own MkdirAll)
	require.NoError(t, r.Append(note("after-recovery")))

	recs, _ := readRing(t, dir)
	var notes []string
	for _, rec := range recs {
		notes = append(notes, rec.Note)
	}
	assert.Contains(t, notes, "episode")
	assert.Contains(t, notes, "after-recovery")

	// Explicit Close is final: no resurrection.
	require.NoError(t, r.Close())
	assert.Error(t, r.Append(note("post-close")))
}

// TestRingAgePrune: segments older than the retention window are removed at
// open time (the ring must not outlive the device's own log-rotation regime).
func TestRingAgePrune(t *testing.T) {
	dir := t.TempDir()
	stale := filepath.Join(dir, segmentName(1))
	fresh := filepath.Join(dir, segmentName(2))
	require.NoError(t, os.WriteFile(stale, []byte("{}\n"), 0o600))
	require.NoError(t, os.WriteFile(fresh, []byte("{}\n"), 0o600))
	old := time.Now().Add(-8 * 24 * time.Hour)
	require.NoError(t, os.Chtimes(stale, old, old))

	r, err := OpenRing(dir, 0)
	require.NoError(t, err)
	defer func() { _ = r.Close() }()

	_, err = os.Stat(stale)
	assert.True(t, os.IsNotExist(err), "stale segment must be pruned at open")
	_, err = os.Stat(fresh)
	assert.NoError(t, err, "fresh segment must survive the prune")
	assert.Equal(t, 2, r.activeSeq, "newest surviving segment becomes active")
}
