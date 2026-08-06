package offlinecache

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	dp1playlist "github.com/display-protocol/dp1-go/playlist"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

// TestService_DequeueForProcessingAndReserveForClear_AreMutuallyExclusive
// is the whitebox regression test for the ClearItem/ClearPlaylist vs.
// worker-dequeue race a review round flagged: an earlier revision split
// "check whether itemID is busy" and "remove itemID's queued job, clear
// its tracked state" into two separate steps (a busy-check loop, then a
// later, separately-locked mutate step). Between those two steps, the
// worker's run() loop could pop the exact same job and transition it to
// StateDownloading — so the busy-check would already have passed, the
// subsequent queue removal would find nothing left to remove (already
// popped), and the clear would report success while the now-active
// capture's SaveItem "resurrected" the record moments later.
//
// dequeueForProcessing and reserveForClear now share s.mu across their
// whole check-then-mutate sequence (see both methods' docs), so
// whichever one actually executes first is authoritative. This test
// calls them directly, in each order, on a service instance stripped
// down to just the fields they touch (mu/state/queue) — no goroutines,
// no timing — to pin the CONTRACT deterministically rather than relying
// on chance interleaving to reproduce the bug.
func TestService_DequeueForProcessingAndReserveForClear_AreMutuallyExclusive(t *testing.T) {
	item := dp1playlist.PlaylistItem{ID: "item-1", Source: "https://example.com/item-1"}
	key := SourceKey(item.Source)

	t.Run("dequeue wins: reserveForClear must see busy and must not touch the queue or state", func(t *testing.T) {
		s := &service{queue: newJobQueue(), state: make(map[string]ItemState), sourceByKey: make(map[string]string), downloadEpoch: make(map[string]uint64)}
		s.state[key] = StateQueued
		s.queue.push(captureJob{sourceKey: key, item: item})

		j, ok := s.dequeueForProcessing()
		require.True(t, ok, "the worker must be able to dequeue the job that was sitting in the queue")
		assert.Equal(t, key, j.sourceKey)
		assert.Equal(t, StateDownloading, s.state[key],
			"dequeueForProcessing must mark the item downloading in the SAME step as the pop")

		res, busyID, err := s.reserveForClear(map[string]bool{key: true})
		assert.Equal(t, key, busyID)
		assert.ErrorIs(t, err, ErrItemBusy,
			"a clear landing after the worker already dequeued this exact job must be rejected, never silently succeed")
		assert.Empty(t, res.settled,
			"a rejected clear settled nothing: reporting the item as settled would make ClearItem announce a not_cached that never happened")
		assert.Equal(t, StateDownloading, s.state[key],
			"a rejected clear must leave the now-active item's tracked state untouched")
	})

	t.Run("clear wins: dequeueForProcessing must find nothing once reserveForClear already canceled the job", func(t *testing.T) {
		s := &service{queue: newJobQueue(), state: make(map[string]ItemState), sourceByKey: make(map[string]string), downloadEpoch: make(map[string]uint64)}
		s.state[key] = StateQueued
		s.queue.push(captureJob{sourceKey: key, item: item})

		res, busyID, err := s.reserveForClear(map[string]bool{key: true})
		require.NoError(t, err)
		assert.Empty(t, busyID)
		assert.True(t, res.settled[key],
			"a winning clear must report the queued item as settled: it is what tells ClearItem this was a real cancellation and not a not_found no-op")
		_, tracked := s.state[key]
		assert.False(t, tracked, "a winning clear must clear the item's tracked state")

		_, ok := s.dequeueForProcessing()
		assert.False(t, ok,
			"the worker must never dequeue a job reserveForClear already removed, or the clear it just reported as successful would be silently undone")
	})
}

// blockingObserver holds the FIRST notification it receives inside
// OnItemStateChanged until released, recording every state it is handed in
// call order. That models the real observer's cost honestly: the notifier
// sends over the relayer and the hub WebSocket, so a notification is not
// instantaneous, and the window between enqueue committing a job and
// enqueue actually emitting its "queued" is exactly what the barrier under
// test has to cover.
type blockingObserver struct {
	release chan struct{}
	mu      sync.Mutex
	states  []ItemState
	blocked bool
}

func (o *blockingObserver) OnItemStateChanged(status ItemStatus) {
	o.mu.Lock()
	o.states = append(o.states, status.State)
	first := !o.blocked
	o.blocked = true
	o.mu.Unlock()
	if first {
		<-o.release
	}
}

func (o *blockingObserver) recorded() []ItemState {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]ItemState(nil), o.states...)
}

// TestService_ClearReservation_WaitsForARacingEnqueuesQueuedNotification is
// the whitebox regression test for the notification inversion a review
// round flagged in the clear-side not_cached push: enqueue commits a job to
// the queue under s.mu but emits its state:"queued" notification only AFTER
// releasing that lock (it must never call the observer while holding it).
// reserveForClear needs the same lock and nothing more, so it can cancel
// that job and announce not_cached in between — and the client would then
// receive not_cached FOLLOWED BY queued, leaving it parked forever on a
// queued entry for work that no longer exists.
//
// The fix reuses the barrier that already exists for the symmetric
// worker-side inversion (captureJob.queuedNotified, which process() waits
// on before emitting "downloading"). This test drives the two calls
// directly, with the observer wedged open inside the queued notification,
// so the ordering is pinned by construction rather than by timing luck.
func TestService_ClearReservation_WaitsForARacingEnqueuesQueuedNotification(t *testing.T) {
	obs := &blockingObserver{release: make(chan struct{})}
	s := &service{
		queue: newJobQueue(), state: make(map[string]ItemState),
		sourceByKey:   make(map[string]string),
		downloadEpoch: make(map[string]uint64), maxQueueLen: 4, observer: obs,
	}
	s.started.Store(true)
	item := dp1playlist.PlaylistItem{ID: "item-1", Source: "https://example.com/item-1"}
	key := SourceKey(item.Source)

	enqueueDone := make(chan struct{})
	go func() {
		defer close(enqueueDone)
		_, err := s.enqueue(item, 0, ClassSoftware)
		assert.NoError(t, err)
	}()
	// Wait for the commit, not for the notification: this is precisely the
	// window in which a clear can observe the job and cancel it while its
	// "queued" is still unsent.
	require.Eventually(t, func() bool { return s.queue.len() == 1 }, time.Second, time.Millisecond,
		"the enqueue goroutine must commit its job before this test can race it")

	res, _, err := s.reserveForClear(map[string]bool{key: true})
	require.NoError(t, err)
	require.Len(t, res.queuedNotified, 1, "the clear must have dropped the queued job, barrier included")

	waited := make(chan struct{})
	go func() {
		res.awaitQueuedNotifications()
		close(waited)
	}()
	select {
	case <-waited:
		t.Fatal("the clear announced not_cached while the racing enqueue's state:\"queued\" was still unsent; the client would end up parked on queued for a canceled job")
	case <-time.After(100 * time.Millisecond):
	}

	close(obs.release)
	select {
	case <-waited:
	case <-time.After(2 * time.Second):
		t.Fatal("the clear never resumed after the queued notification completed")
	}
	s.notifyObserver("item-1", StateNotCached, Coverage{})
	<-enqueueDone

	assert.Equal(t, []ItemState{StateQueued, StateNotCached}, obs.recorded(),
		"the client must see the cancellation settle AFTER the queued entry it settles, never before it")
}

// TestService_ReserveForClear_DoesNotSettleAnAlreadyNotCachedItem pins the
// other side of the settled contract: an item the disk-budget eviction
// already reported as not_cached (enforceDiskLimit's notify leaves that
// state tracked) transitioned to nothing, so a clear for it must stay a
// not_found no-op rather than re-announcing a state the client already has.
func TestService_ReserveForClear_DoesNotSettleAnAlreadyNotCachedItem(t *testing.T) {
	s := &service{queue: newJobQueue(), state: make(map[string]ItemState), sourceByKey: make(map[string]string), downloadEpoch: make(map[string]uint64)}
	key := SourceKey("https://example.com/item-1")
	s.state[key] = StateNotCached

	res, busyID, err := s.reserveForClear(map[string]bool{key: true})
	require.NoError(t, err)
	assert.Empty(t, busyID)
	assert.Empty(t, res.settled)
	assert.Empty(t, res.queuedNotified)
}

// TestService_Enqueue_ReturnsErrQueueFullAtCapacityAndAdmitsAfterDrain is
// the regression test for ErrQueueFull: without a cap, jobQueue grows
// without bound as distinct playlists are queued in a burst — see that
// error's doc. maxQueueLen is not exposed via NewService (a fixed
// software safety valve, not a per-device tunable — see
// defaultMaxQueueLen's doc), so this is necessarily a whitebox test
// constructing *service directly to set a small cap.
func TestService_Enqueue_ReturnsErrQueueFullAtCapacityAndAdmitsAfterDrain(t *testing.T) {
	s := &service{queue: newJobQueue(), state: make(map[string]ItemState), sourceByKey: make(map[string]string), downloadEpoch: make(map[string]uint64), maxQueueLen: 2}
	s.started.Store(true)

	item1 := dp1playlist.PlaylistItem{ID: "item-1", Source: "https://example.com/item-1"}
	item2 := dp1playlist.PlaylistItem{ID: "item-2", Source: "https://example.com/item-2"}
	item3 := dp1playlist.PlaylistItem{ID: "item-3", Source: "https://example.com/item-3"}

	// epoch 0 for every call: no item is cleared in this test, so the
	// sampled-vs-current epoch always matches and the clear-abort path is
	// never taken (see downloadEpoch's doc).
	queued1, err := s.enqueue(item1, 0, ClassSoftware)
	require.NoError(t, err)
	assert.Equal(t, enqueueQueued, queued1, "the first distinct item must be reported as newly queued")
	queued2, err := s.enqueue(item2, 0, ClassSoftware)
	require.NoError(t, err)
	assert.Equal(t, enqueueQueued, queued2, "the second distinct item must be reported as newly queued")

	queued3, err := s.enqueue(item3, 0, ClassSoftware)
	assert.ErrorIs(t, err, ErrQueueFull, "a third distinct item must be rejected once the queue is already at its 2-item cap")
	assert.NotEqual(t, enqueueQueued, queued3, "a rejected enqueue must not report itself as newly queued")
	assert.Equal(t, 2, s.queue.len(), "a rejected enqueue must not have touched the queue")
	_, tracked := s.state[SourceKey(item3.Source)]
	assert.False(t, tracked, "a rejected item must not be left behind in a spurious StateQueued")

	_, ok := s.dequeueForProcessing() // drains item1, freeing one slot
	require.True(t, ok)
	queued3Retry, err := s.enqueue(item3, 0, ClassSoftware)
	assert.NoError(t, err)
	assert.Equal(t, enqueueQueued, queued3Retry, "capacity freed by a dequeue must admit a new item and report it as newly queued")
}

// TestService_Enqueue_IdempotentReenqueueDoesNotCountAgainstCapacity pins
// that re-enqueuing an item already StateQueued/StateDownloading is a
// true no-op (enqueue's existing idempotency guarantee) even when the
// queue is otherwise completely full — it must never be rejected as
// ErrQueueFull just because capacity happens to be exhausted by OTHER
// items, since it was never going to consume a new queue slot anyway.
func TestService_Enqueue_IdempotentReenqueueDoesNotCountAgainstCapacity(t *testing.T) {
	s := &service{queue: newJobQueue(), state: make(map[string]ItemState), sourceByKey: make(map[string]string), downloadEpoch: make(map[string]uint64), maxQueueLen: 1}
	s.started.Store(true)
	item := dp1playlist.PlaylistItem{ID: "item-1", Source: "https://example.com/item-1"}

	firstQueued, err := s.enqueue(item, 0, ClassSoftware)
	require.NoError(t, err)
	assert.Equal(t, enqueueQueued, firstQueued, "the first enqueue of a distinct item must be reported as newly queued")

	reenqueued, err := s.enqueue(item, 0, ClassSoftware)
	assert.NoError(t, err, "re-enqueuing an already-queued item must be a no-op, not rejected as queue-full")
	assert.Equal(t, enqueueAlreadyQueued, reenqueued, "re-enqueuing an already-queued item must NOT report itself as newly queued, or a caller aggregating counts (DownloadPlaylist's queuedCount) would overcount idempotent retries")
	assert.Equal(t, 1, s.queue.len(), "the idempotent re-enqueue must not have pushed a second entry")
}

// TestService_CaptureFailure_TruncatesSourceInLog pins the terminal
// capture-failure log boundary. The source is attacker-controlled input
// arriving from an unauthenticated LAN hub, and this field is newly
// exposed by the source-key conversion — the old log carried item_id. An
// untruncated source here lets one oversized playlist item flood the
// rotated logs on a device whose disk budget this subsystem is otherwise
// careful about.
func TestService_CaptureFailure_TruncatesSourceInLog(t *testing.T) {
	core, observed := observer.New(zap.WarnLevel)
	huge := "https://example.com/" + strings.Repeat("a", maxLoggedSourceBytes*3)

	// Drive the REAL failure path rather than re-typing its log call.
	// An earlier version of this test built the zap.String itself, which
	// meant it passed just as well with service.go's call site reverted
	// to the untruncated form — it asserted that the helper works, which
	// TestTruncateSourceForLog_BoundsOversizedSources already covers,
	// and nothing about the call site it claims to pin.
	s := &service{
		logger:      zap.New(core),
		state:       map[string]ItemState{},
		sourceByKey: map[string]string{},
		clock:       wrapper.NewClock(),
		capturer:    failingCapturer{},
	}
	s.process(context.Background(), captureJob{
		sourceKey: SourceKey(huge),
		item:      dp1playlist.PlaylistItem{Source: huge},
		class:     ClassSoftware,
	})

	entries := observed.FilterMessageSnippet("capture failed").All()
	require.Len(t, entries, 1)
	logged, ok := entries[0].ContextMap()["source"].(string)
	require.True(t, ok)
	assert.Less(t, len(logged), len(huge), "an oversized source must not reach the log intact")
	assert.Contains(t, logged, "bytes]", "the marker must say it was shortened, so nobody hunts a URL that never existed")
}

// Every attacker-controlled source log in the package goes through the
// same helper. Fixing only the one boundary a reviewer happened to name
// would leave the others unbounded for the identical reason.
func TestTruncateSourceForLog_BoundsOversizedSources(t *testing.T) {
	short := "https://example.com/a.png"
	assert.Equal(t, short, truncateSourceForLog(short), "a normal source is untouched")

	huge := strings.Repeat("z", maxLoggedSourceBytes*2)
	got := truncateSourceForLog(huge)
	assert.Less(t, len(got), len(huge))
	assert.Contains(t, got, "bytes]")
}

// failingCapturer makes the software capture path fail, so the terminal
// capture-failure log this file pins is actually reached.
type failingCapturer struct{}

func (failingCapturer) Capture(context.Context, dp1playlist.PlaylistItem, int) (*ItemRecord, error) {
	return nil, errors.New("simulated capture failure")
}
func (failingCapturer) Close() error { return nil }

// errAfterHook is returned by the stub store below so ClearPlaylist
// unwinds immediately after its first critical section. The point is to
// observe that section and nothing else — no reserveForClear, no blob
// deletes, no gc.
var errAfterHook = errors.New("stop the clear here")

// lockProbeStore serves LoadPlaylist by running a hook and then failing.
// Store is embedded as an interface and left nil deliberately: this test
// must exercise exactly one store method, so any other call is a test
// bug and a nil panic names it immediately.
type lockProbeStore struct {
	Store
	onLoadPlaylist func()
}

func (s *lockProbeStore) LoadPlaylist(string) (json.RawMessage, error) {
	s.onLoadPlaylist()
	return nil, errAfterHook
}

// TestClearPlaylist_HoldsPlaylistRecordMuAcrossBumpAndSnapshot pins the
// half of the invariant that the ordering test in service_test.go
// cannot: that the clear-epoch bump and the membership snapshot happen
// in ONE critical section, not merely in that order.
//
// Both halves are load-bearing and neither implies the other:
//
//   - Bump before load (the ordering test). Without it, a save that
//     lands in between passes its own epoch re-check and enqueues a
//     source this clear will never invalidate.
//   - Both the bump AND the load under playlistRecordMu (this test).
//     This is what catches a regression to the ORIGINAL bug shape —
//     bump and load BOTH outside the mutex — where a save's re-check
//     runs before the bump lands, passes legitimately, and its atomic
//     write becomes visible only after an unlocked snapshot. That
//     snapshot misses the source the save enqueues, whose per-item epoch
//     was sampled during its classify and is never bumped by
//     reserveForClear: the same orphaned-item failure ClearPlaylist's own
//     comment describes. That shape still bumps before reading, so the
//     ordering test passes it and this is the only test in the package
//     that fails it.
//
// Be precise about the weaker rearrangements — the obvious stronger
// claim ("each operation must individually be under the lock") is wrong,
// and was believed twice during review before being traced out:
//
//   - Hoisting the BUMP alone (load still inside) is safe, and this test
//     passes it. A save arriving in the gap re-checks against the
//     already-bumped epoch and declines to write. Releasing and
//     re-acquiring between the two is safe for the same reason.
//   - Hoisting the LOAD alone (bump still inside) is not a live hole
//     either, but only because savePlaylistAndURLIndex holds the mutex
//     across its re-check AND its write and fsStore.SavePlaylist writes
//     atomically. It degrades to the residual ClearPlaylist already
//     documents, not to a fresh one.
//
// So this test does not pin "the load must be locked" as a standalone
// hazard. It pins both operations inside the mutex so the guarantee stops
// depending on those two assumptions holding elsewhere in the file, and
// so the edit that hoists both is caught.
//
// TryLock is what makes this sound rather than timing-dependent: it
// reports false whenever the mutex is held by ANY goroutine, so probing
// it from inside the LoadPlaylist hook is a direct observation of "the
// lock is held at the instant membership is read". No second goroutine,
// no sleep, and no window in either direction — unlike an attempt to
// prove blocking by racing a competing save, where a goroutine that has
// not yet reached the lock is indistinguishable from one parked on it.
func TestClearPlaylist_HoldsPlaylistRecordMuAcrossBumpAndSnapshot(t *testing.T) {
	var (
		heldAtSnapshot  bool
		epochAtSnapshot uint64
		probed          bool
	)
	s := &service{
		playlistClearEpoch: make(map[string]uint64),
		downloadEpoch:      make(map[string]uint64),
	}
	s.store = &lockProbeStore{onLoadPlaylist: func() {
		probed = true
		// Held by this same goroutine — Go mutexes are not reentrant, so
		// TryLock reports false either way, which is exactly the signal
		// wanted here.
		if s.playlistRecordMu.TryLock() {
			s.playlistRecordMu.Unlock()
			return
		}
		heldAtSnapshot = true
		s.mu.RLock()
		epochAtSnapshot = s.playlistClearEpoch["pl-lock"]
		s.mu.RUnlock()
	}}

	err := s.ClearPlaylist("pl-lock")
	require.ErrorIs(t, err, errAfterHook, "the stub store must be what ended the call")

	require.True(t, probed, "test bug: ClearPlaylist never loaded the playlist")
	require.True(t, heldAtSnapshot,
		"the bump and the membership read must BOTH be under playlistRecordMu; with both "+
			"outside it (the original bug shape, which still bumps first and so passes the "+
			"ordering test) a save's re-check runs before the bump, passes, and its write "+
			"lands after this snapshot — leaving the source it enqueues cached with no "+
			"playlist referencing it")
	require.NotZero(t, epochAtSnapshot,
		"the bump must already be visible inside that same critical section")
}
