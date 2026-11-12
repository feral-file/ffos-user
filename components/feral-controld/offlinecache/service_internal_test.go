package offlinecache

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	dp1playlist "github.com/display-protocol/dp1-go/playlist"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"

	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

// truncateReason is exercised end-to-end through Status in
// service_test.go; these cover the boundaries that are awkward to reach
// through a real capture record.
func TestTruncateReason(t *testing.T) {
	t.Run("short reason is untouched", func(t *testing.T) {
		reason := "csp_blocked; fetch_failed:https://example.com/a.js"
		assert.Equal(t, reason, truncateReason(reason))
	})

	t.Run("reason exactly at the budget is untouched", func(t *testing.T) {
		reason := strings.Repeat("x", maxReasonBytes)
		assert.Equal(t, reason, truncateReason(reason))
	})

	t.Run("keeps whole entries and counts the dropped ones", func(t *testing.T) {
		entries := make([]string, 40)
		for i := range entries {
			entries[i] = fmt.Sprintf("fetch_failed:https://example.com/asset-%02d.js", i)
		}
		got := truncateReason(strings.Join(entries, reasonSeparator))

		// Read the dropped count off the marker rather than hard-coding
		// how many entries happen to fit: what matters is that kept +
		// dropped accounts for every entry.
		marker := got[strings.LastIndex(got, "…"):]
		var dropped int
		_, err := fmt.Sscanf(marker, "…(+%d more)", &dropped)
		require.NoError(t, err)
		kept := strings.Split(got[:strings.LastIndex(got, reasonSeparator)], reasonSeparator)
		assert.Equal(t, len(entries), len(kept)+dropped)

		for _, entry := range kept {
			assert.Contains(t, entries, entry, "kept entries must not be cut mid-token")
		}
		assert.LessOrEqual(t, len(got), maxReasonBytes+len(reasonSeparator)+len(droppedReasonMarker(dropped)))
	})

	t.Run("a single oversized entry is cut on a rune boundary", func(t *testing.T) {
		// Multi-byte runes packed so the budget lands mid-rune.
		entry := "fetch_failed:https://example.com/" + strings.Repeat("é", maxReasonBytes)
		got := truncateReason(entry)

		assert.True(t, utf8.ValidString(got), "truncation must not split a rune")
		assert.True(t, strings.HasSuffix(got, "…"))
		assert.LessOrEqual(t, len(got), maxReasonBytes+len("…"))
	})

	t.Run("a single oversized entry still reports the entries after it", func(t *testing.T) {
		first := "fetch_failed:https://example.com/" + strings.Repeat("a", maxReasonBytes)
		got := truncateReason(strings.Join([]string{first, "csp_blocked", "csp_blocked"}, reasonSeparator))

		assert.True(t, utf8.ValidString(got))
		assert.Contains(t, got, droppedReasonMarker(2))
	})
}

// transientFailReadOS delegates to a real OS wrapper but fails ReadFile
// for one specific path — the same transient-EIO shape
// store_test.go's failReadOS simulates, duplicated here because this
// internal test package cannot import the external one.
type transientFailReadOS struct {
	wrapper.OS
	failPath string
}

func (f *transientFailReadOS) ReadFile(path string) ([]byte, error) {
	if path == f.failPath {
		return nil, fmt.Errorf("simulated transient read error for %s", path)
	}
	return f.OS.ReadFile(path)
}

// transientFailRemoveOS delegates to a real OS wrapper but fails Remove
// for one specific path — used to simulate a sidecar-delete failure
// without touching anything else writeFileAtomic's own cleanup removes
// (its temp files live under a different, uniquely-named path; see
// writeFileAtomic's doc).
type transientFailRemoveOS struct {
	wrapper.OS
	failPath string
}

func (f *transientFailRemoveOS) Remove(path string) error {
	if path == f.failPath {
		return fmt.Errorf("simulated transient remove error for %s", path)
	}
	return f.OS.Remove(path)
}

// TestService_SavePlaylistAndURLIndex_AbortsWhenStaleSidecarCannotBeInvalidated
// pins the fix for feralfile-bot F2 (round 3 on this fix, 2026-11-12): a
// re-resolve of the IDENTICAL raw playlist bytes — the one case
// playlistBodyDigest alone cannot tell apart from the earlier resolve the
// existing sidecar was saved for — must not let that stale sidecar survive
// next to a freshly-saved body just because invalidating it failed. The
// whole save must abort instead, leaving the prior body+sidecar pair
// exactly as they were rather than quietly accepting a mismatch.
func TestService_SavePlaylistAndURLIndex_AbortsWhenStaleSidecarCannotBeInvalidated(t *testing.T) {
	root := t.TempDir()
	sidecarPath := filepath.Join(root, "playlists", "series-segments", "playlist-1.json")
	failingOS := &transientFailRemoveOS{OS: wrapper.NewOS(), failPath: sidecarPath}
	store := NewStore(root, failingOS, wrapper.NewJSON(), zaptest.NewLogger(t))

	raw := json.RawMessage(`{"id":"playlist-1","items":[]}`)
	require.NoError(t, store.SavePlaylist("playlist-1", raw))
	staleSegments := []SeriesSegment{{Item: &dp1playlist.PlaylistItem{ID: "old"}}}
	require.NoError(t, store.SavePlaylistSeriesSegments("playlist-1", raw, staleSegments))

	svc := &service{store: store, logger: zaptest.NewLogger(t)}

	// The SAME raw bytes resolve again, but this time to no series group at
	// all — exactly the shape a later LoadPlaylistSeriesSegments must never
	// confuse with the earlier resolve's now-stale segments, since both
	// share one digest.
	saved, err := svc.savePlaylistAndURLIndex("playlist-1", raw, "", 0, nil)
	require.Error(t, err, "a failed invalidation must abort the save, not silently continue")
	assert.False(t, saved)

	// Nothing changed: the prior body and its (now only-apparently-stale,
	// but still internally consistent) sidecar are both exactly as they
	// were before this call — never a body that landed next to segments
	// from a resolve that no longer happened.
	loadedSegments, loadErr := store.LoadPlaylistSeriesSegments("playlist-1", raw)
	require.NoError(t, loadErr)
	require.Len(t, loadedSegments, 1)
	require.NotNil(t, loadedSegments[0].Item)
	assert.Equal(t, "old", loadedSegments[0].Item.ID)
}

// transientFailRenameOS delegates to a real OS wrapper but fails Rename
// for one specific destination path, starting from its failFrom'th call
// against that path (1-indexed) — used to simulate SavePlaylist's
// writeFileAtomic failing the way a real disk-full/permission error
// would: the temp file is written, then the final Rename fails, leaving
// the original file at that path completely untouched (see
// writeFileAtomic's doc). Counting lets a test seed an initial, SUCCESSFUL
// save before the simulated failure hits a later one to the same path.
type transientFailRenameOS struct {
	wrapper.OS
	failPath string
	failFrom int
	calls    int
}

func (f *transientFailRenameOS) Rename(oldpath, newpath string) error {
	if newpath == f.failPath {
		f.calls++
		if f.calls >= f.failFrom {
			return fmt.Errorf("simulated transient rename error for %s", newpath)
		}
	}
	return f.OS.Rename(oldpath, newpath)
}

// TestService_SavePlaylistAndURLIndex_KeepsAValidSidecarWhenSavePlaylistFails
// pins run-reviewer's own finding on this branch (round 3 on this fix,
// 2026-11-12): invalidating the existing sidecar BEFORE attempting the new
// body write destroyed a still-correctly-paired sidecar for the OLD,
// untouched body whenever that body write then failed — a regression the
// fix for feralfile-bot F2 introduced while closing a different gap.
// SavePlaylist's own writeFileAtomic either fully replaces the body or
// leaves it exactly as it was on any failure, so a transient body-write
// error must leave the sidecar — which is still valid for that unchanged
// body — exactly as it was too, not deleted.
func TestService_SavePlaylistAndURLIndex_KeepsAValidSidecarWhenSavePlaylistFails(t *testing.T) {
	root := t.TempDir()
	playlistPath := filepath.Join(root, "playlists", "playlist-1.json")
	failingOS := &transientFailRenameOS{OS: wrapper.NewOS(), failPath: playlistPath, failFrom: 2}
	store := NewStore(root, failingOS, wrapper.NewJSON(), zaptest.NewLogger(t))

	oldRaw := json.RawMessage(`{"id":"playlist-1","items":[]}`)
	require.NoError(t, store.SavePlaylist("playlist-1", oldRaw))
	validSegments := []SeriesSegment{{Item: &dp1playlist.PlaylistItem{ID: "still-valid"}}}
	require.NoError(t, store.SavePlaylistSeriesSegments("playlist-1", oldRaw, validSegments))

	svc := &service{store: store, logger: zaptest.NewLogger(t)}

	// A new resolve with different bytes is attempted, but the body write
	// itself fails transiently — the old body (and its still-matching
	// sidecar) must be left exactly as they were, never left with the old
	// body but no sidecar.
	newRaw := json.RawMessage(`{"id":"playlist-1","items":[{"id":"x"}]}`)
	saved, err := svc.savePlaylistAndURLIndex("playlist-1", newRaw, "", 0, nil)
	require.Error(t, err, "a failed body save must still be reported as an error")
	assert.False(t, saved)

	onDisk, loadErr := store.LoadPlaylist("playlist-1")
	require.NoError(t, loadErr)
	assert.JSONEq(t, string(oldRaw), string(onDisk), "the old body must be untouched by a failed save")

	loadedSegments, segErr := store.LoadPlaylistSeriesSegments("playlist-1", oldRaw)
	require.NoError(t, segErr, "the sidecar for the unchanged old body must still be there")
	require.Len(t, loadedSegments, 1)
	require.NotNil(t, loadedSegments[0].Item)
	assert.Equal(t, "still-valid", loadedSegments[0].Item.ID,
		"a transient SavePlaylist failure must not delete a sidecar that is still correctly paired with the body on disk")
}

// TestService_EvictDownTo_StopsAfterOneVictimWhenGCFails pins the
// eviction loop's behavior at the GC seam (reviewer finding on the
// GC-abort change): when a transiently unreadable record makes gc()
// abort, the loop has ALREADY deleted one healthy victim's record before
// discovering the failure — DeleteItem removes only the JSON, so those
// bytes are not reclaimed until a later successful GC — and must then
// stop rather than keep burning healthy records for zero freed bytes.
// This is the accepted cost of GC's abort-on-transient-error direction:
// one record per eviction attempt, bounded, recoverable (the record's
// blobs are swept once GC succeeds again), versus a sweep over an
// incomplete keep-set destroying referenced blobs unrecoverably.
func TestService_EvictDownTo_StopsAfterOneVictimWhenGCFails(t *testing.T) {
	root := t.TempDir()
	failOS := &transientFailReadOS{OS: wrapper.NewOS()}
	logger := zaptest.NewLogger(t)
	store := NewStore(root, failOS, wrapper.NewJSON(), logger)

	seed := func(id, content string, capturedAt time.Time) {
		blobHash, err := store.WriteBlob(strings.NewReader(content), 0)
		require.NoError(t, err)
		require.NoError(t, store.SaveItem(&ItemRecord{
			Item:       dp1playlist.PlaylistItem{ID: id, Source: "https://example.com/" + id},
			Entry:      "https://example.com/" + id,
			Resources:  []Resource{{URL: "https://example.com/" + id, Status: 200, SHA256: blobHash}},
			Coverage:   Coverage{Complete: true},
			CapturedAt: capturedAt,
		}))
	}
	seed("item-victim", "victim payload", time.Now().Add(-2*time.Hour))
	seed("item-survivor", "survivor payload", time.Now().Add(-1*time.Hour))
	seed("item-flaky", "flaky payload", time.Now())

	svc, ok := NewService(store, nil, nil, nil, wrapper.NewJSON(), 5000, 1, nil, AdmissionOptions{}, logger).(*service)
	require.True(t, ok)

	// The flaky record reads fine during the victim scan setup above;
	// from here on it fails, so oldestEvictableItem skips it (its own
	// LoadItem errors) and gc()'s mark phase aborts on it.
	failOS.failPath = filepath.Join(root, "items", SourceKey("https://example.com/item-flaky")+".json")

	// Target 0 with three items on disk: without the gc-failure stop this
	// loop would evict every readable record trying to get under target.
	svc.evictDownTo(0, "", "", false, nil)

	_, err := store.LoadItem(SourceKey("https://example.com/item-victim"))
	assert.ErrorIs(t, err, ErrItemNotFound, "the oldest readable record is deleted before the GC failure is discovered")
	_, err = store.LoadItem(SourceKey("https://example.com/item-survivor"))
	assert.NoError(t, err, "the loop must stop after the failed GC instead of burning further healthy records")
	for _, content := range []string{"victim payload", "survivor payload", "flaky payload"} {
		sum := sha256.Sum256([]byte(content))
		_, err := store.ReadBlob(hex.EncodeToString(sum[:]))
		assert.NoError(t, err, "no blob bytes may be reclaimed while GC is aborting")
	}
}

// TestService_NotifyEvicted_NeverDowngradesASourceWithAJobInFlight pins
// eviction's compare-and-set announcement.
//
// A victim can legitimately be a source whose stale record is the oldest
// on disk while a recapture for it sits in the queue. Announcing that
// through the plain notify overwrote StateQueued with StateNotCached and
// broke two contracts: enqueue's idempotency check stopped seeing the job
// as scheduled, so the next request pushed a DUPLICATE job and the
// artwork was captured twice; and reserveForClear stopped counting it as
// settled, so a ClearItem that really did cancel the queued job answered
// a NON-retryable ErrItemNotFound.
//
// Deliberately asserted here rather than by filtering the victim scan:
// eviction must still be free to reclaim an in-flight source's stale
// bytes (DownloadPlaylist enqueues every item, so a refresh marks nearly
// the whole store in-flight — skipping those would starve reclaim exactly
// when the store is full). Only the state downgrade is wrong.
func TestService_NotifyEvicted_NeverDowngradesASourceWithAJobInFlight(t *testing.T) {
	newSvc := func(t *testing.T) (*service, *recordingProgressObserver) {
		t.Helper()
		obs := &recordingProgressObserver{}
		svc := &service{
			queue: newJobQueue(), state: make(map[string]ItemState),
			sourceByKey: make(map[string]string), downloadEpoch: make(map[string]uint64),
			observer: obs, logger: zaptest.NewLogger(t),
		}
		return svc, obs
	}

	const source = "https://example.com/evicted"
	key := SourceKey(source)

	for _, inFlight := range []ItemState{StateQueued, StateDownloading} {
		t.Run("preserves "+string(inFlight), func(t *testing.T) {
			svc, obs := newSvc(t)
			svc.state[key] = inFlight
			svc.sourceByKey[key] = source

			svc.notifyEvicted(source, Coverage{})

			assert.Equal(t, inFlight, svc.state[key],
				"an in-flight source's tracked state must survive the eviction of its stale record")
			assert.Empty(t, obs.states(),
				"and nothing is announced: the queued job's own capture reports the real terminal state")
		})
	}

	t.Run("settles a terminal state", func(t *testing.T) {
		svc, obs := newSvc(t)
		svc.state[key] = StateReady
		svc.sourceByKey[key] = source

		svc.notifyEvicted(source, Coverage{})

		assert.NotContains(t, svc.state, key,
			"eviction must not leave a ghost in whole-store status")
		assert.NotContains(t, svc.sourceByKey, key)
		assert.Equal(t, []ItemState{StateNotCached}, obs.states())
	})

	t.Run("settles an untracked source", func(t *testing.T) {
		svc, obs := newSvc(t)

		svc.notifyEvicted(source, Coverage{})

		assert.NotContains(t, svc.state, key)
		assert.NotContains(t, svc.sourceByKey, key)
		assert.Equal(t, []ItemState{StateNotCached}, obs.states())
	})
}

// recordingProgressObserver is a minimal ProgressObserver for the
// internal test package (service_test.go's equivalent lives in the
// external one and cannot be imported here).
type recordingProgressObserver struct {
	mu       sync.Mutex
	recorded []ItemState
}

func (o *recordingProgressObserver) OnItemStateChanged(status ItemStatus) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.recorded = append(o.recorded, status.State)
}

func (o *recordingProgressObserver) states() []ItemState {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]ItemState(nil), o.recorded...)
}

// TestService_HasReplayableItem pins the preflight's cache-rescue
// predicate (#305 review F4): a prior successful capture on disk —
// complete (ready) or partial — rescues an all-dead cast, while
// broken-online (fails even with network), not-cached, and unknown
// sources do not. Empty input reports false.
func TestService_HasReplayableItem(t *testing.T) {
	logger := zaptest.NewLogger(t)
	store := NewStore(t.TempDir(), wrapper.NewOS(), wrapper.NewJSON(), logger)
	svc := &service{
		store: store, state: make(map[string]ItemState),
		sourceByKey: make(map[string]string), logger: logger,
	}

	save := func(source string, cov Coverage) {
		t.Helper()
		require.NoError(t, store.SaveItem(&ItemRecord{
			Item:     dp1playlist.PlaylistItem{Source: source},
			Coverage: cov,
		}))
	}
	const (
		ready   = "https://example.com/ready"
		partial = "https://example.com/partial"
		broken  = "https://example.com/broken"
	)
	save(ready, Coverage{Complete: true})
	save(partial, Coverage{Complete: false, Reason: "one asset failed: timeout"})
	save(broken, Coverage{Complete: false, Reason: string(ReasonCSPBlocked)})

	assert.True(t, svc.HasReplayableItem(ready), "a complete capture rescues")
	assert.True(t, svc.HasReplayableItem("https://example.com/missing", partial),
		"a partial capture rescues, even alongside unknown sources")
	assert.False(t, svc.HasReplayableItem(broken),
		"broken-online fails even with network and must not rescue")
	assert.False(t, svc.HasReplayableItem("https://example.com/missing", ""),
		"unknown and empty sources do not rescue")
	assert.False(t, svc.HasReplayableItem(), "empty input reports false")
}

// countingStore wraps a Store and counts LoadItem calls, for pinning
// HasReplayableItem's read bound.
type countingStore struct {
	Store
	loads int
}

func (c *countingStore) LoadItem(sourceKey string) (*ItemRecord, error) {
	c.loads++
	return c.Store.LoadItem(sourceKey)
}

// TestService_HasReplayableItem_BoundedAndDeduped pins the rescue scan's
// cost and evidence contract: duplicates collapse to one record read,
// inline sources are never looked up (they are never cached), and past
// maxRescueLookups unique sources the scan stops WITHOUT granting the
// rescue — the rescue is an exception to a proven rejection and needs a
// confirmed hit (#308 round 5), while the bound keeps the rejection path
// from grinding unbounded serial filesystem reads.
func TestService_HasReplayableItem_BoundedAndDeduped(t *testing.T) {
	logger := zaptest.NewLogger(t)
	cs := &countingStore{Store: NewStore(t.TempDir(), wrapper.NewOS(), wrapper.NewJSON(), logger)}
	svc := &service{
		store: cs, state: make(map[string]ItemState),
		sourceByKey: make(map[string]string), logger: logger,
	}

	dups := make([]string, 40)
	for i := range dups {
		dups[i] = "https://example.com/same"
	}
	assert.False(t, svc.HasReplayableItem(dups...))
	assert.Equal(t, 1, cs.loads, "duplicates collapse to one record read")

	cs.loads = 0
	assert.False(t, svc.HasReplayableItem("data:text/plain,hi", "data:image/png;base64,aGk="))
	assert.Zero(t, cs.loads, "inline sources are never looked up")

	cs.loads = 0
	many := make([]string, maxRescueLookups+10)
	for i := range many {
		many[i] = fmt.Sprintf("https://example.com/unique/%d", i)
	}
	assert.False(t, svc.HasReplayableItem(many...),
		"an exhausted bound with no hit is NO EVIDENCE — the rescue must not be granted, or an unproven rescue would go on to mutate replay scope")
	assert.Equal(t, maxRescueLookups, cs.loads, "the scan stops at the bound")
}
