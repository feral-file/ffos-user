package offlinecache

import (
	"testing"

	dp1playlist "github.com/display-protocol/dp1-go/playlist"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

	t.Run("dequeue wins: reserveForClear must see busy and must not touch the queue or state", func(t *testing.T) {
		s := &service{queue: newJobQueue(), state: make(map[string]ItemState)}
		s.state["item-1"] = StateQueued
		s.queue.push(captureJob{itemID: "item-1", item: item})

		j, ok := s.dequeueForProcessing()
		require.True(t, ok, "the worker must be able to dequeue the job that was sitting in the queue")
		assert.Equal(t, "item-1", j.itemID)
		assert.Equal(t, StateDownloading, s.state["item-1"],
			"dequeueForProcessing must mark the item downloading in the SAME step as the pop")

		busyID, err := s.reserveForClear(map[string]bool{"item-1": true})
		assert.Equal(t, "item-1", busyID)
		assert.ErrorIs(t, err, ErrItemBusy,
			"a clear landing after the worker already dequeued this exact job must be rejected, never silently succeed")
		assert.Equal(t, StateDownloading, s.state["item-1"],
			"a rejected clear must leave the now-active item's tracked state untouched")
	})

	t.Run("clear wins: dequeueForProcessing must find nothing once reserveForClear already canceled the job", func(t *testing.T) {
		s := &service{queue: newJobQueue(), state: make(map[string]ItemState)}
		s.state["item-1"] = StateQueued
		s.queue.push(captureJob{itemID: "item-1", item: item})

		busyID, err := s.reserveForClear(map[string]bool{"item-1": true})
		require.NoError(t, err)
		assert.Empty(t, busyID)
		_, tracked := s.state["item-1"]
		assert.False(t, tracked, "a winning clear must clear the item's tracked state")

		_, ok := s.dequeueForProcessing()
		assert.False(t, ok,
			"the worker must never dequeue a job reserveForClear already removed, or the clear it just reported as successful would be silently undone")
	})
}

// TestService_Enqueue_ReturnsErrQueueFullAtCapacityAndAdmitsAfterDrain is
// the regression test for ErrQueueFull: without a cap, jobQueue grows
// without bound as distinct playlists are queued in a burst — see that
// error's doc. maxQueueLen is not exposed via NewService (a fixed
// software safety valve, not a per-device tunable — see
// defaultMaxQueueLen's doc), so this is necessarily a whitebox test
// constructing *service directly to set a small cap.
func TestService_Enqueue_ReturnsErrQueueFullAtCapacityAndAdmitsAfterDrain(t *testing.T) {
	s := &service{queue: newJobQueue(), state: make(map[string]ItemState), maxQueueLen: 2}
	s.started.Store(true)

	item1 := dp1playlist.PlaylistItem{ID: "item-1", Source: "https://example.com/item-1"}
	item2 := dp1playlist.PlaylistItem{ID: "item-2", Source: "https://example.com/item-2"}
	item3 := dp1playlist.PlaylistItem{ID: "item-3", Source: "https://example.com/item-3"}

	require.NoError(t, s.enqueue(item1))
	require.NoError(t, s.enqueue(item2))

	err := s.enqueue(item3)
	assert.ErrorIs(t, err, ErrQueueFull, "a third distinct item must be rejected once the queue is already at its 2-item cap")
	assert.Equal(t, 2, s.queue.len(), "a rejected enqueue must not have touched the queue")
	_, tracked := s.state[item3.ID]
	assert.False(t, tracked, "a rejected item must not be left behind in a spurious StateQueued")

	_, ok := s.dequeueForProcessing() // drains item1, freeing one slot
	require.True(t, ok)
	assert.NoError(t, s.enqueue(item3), "capacity freed by a dequeue must admit a new item")
}

// TestService_Enqueue_IdempotentReenqueueDoesNotCountAgainstCapacity pins
// that re-enqueuing an item already StateQueued/StateDownloading is a
// true no-op (enqueue's existing idempotency guarantee) even when the
// queue is otherwise completely full — it must never be rejected as
// ErrQueueFull just because capacity happens to be exhausted by OTHER
// items, since it was never going to consume a new queue slot anyway.
func TestService_Enqueue_IdempotentReenqueueDoesNotCountAgainstCapacity(t *testing.T) {
	s := &service{queue: newJobQueue(), state: make(map[string]ItemState), maxQueueLen: 1}
	s.started.Store(true)
	item := dp1playlist.PlaylistItem{ID: "item-1", Source: "https://example.com/item-1"}

	require.NoError(t, s.enqueue(item))
	assert.NoError(t, s.enqueue(item), "re-enqueuing an already-queued item must be a no-op, not rejected as queue-full")
	assert.Equal(t, 1, s.queue.len(), "the idempotent re-enqueue must not have pushed a second entry")
}
