package offlinecache

import (
	"crypto/sha256"
	"encoding/hex"
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
	svc.evictDownTo(0, "", "", false)

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

		assert.Equal(t, StateNotCached, svc.state[key],
			"an ordinary victim must still be recorded and announced as not_cached")
		assert.Equal(t, []ItemState{StateNotCached}, obs.states())
	})

	t.Run("settles an untracked source", func(t *testing.T) {
		svc, obs := newSvc(t)

		svc.notifyEvicted(source, Coverage{})

		assert.Equal(t, StateNotCached, svc.state[key])
		assert.Equal(t, source, svc.sourceByKey[key],
			"the source must be recorded alongside the state, so status can still report an identity")
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
// cost contract (#305 review): duplicates collapse to one record read,
// inline sources are never looked up (they are never cached), and past
// maxRescueLookups unique sources the scan fails OPEN — uncertainty
// favors the cast, and "rescued" is just the pre-#304 behavior — instead
// of grinding an unbounded number of serial filesystem reads on the
// rejection path.
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
	assert.True(t, svc.HasReplayableItem(many...),
		"an exhausted bound fails open rather than rejecting")
	assert.Equal(t, maxRescueLookups, cs.loads, "the scan stops at the bound")
}
