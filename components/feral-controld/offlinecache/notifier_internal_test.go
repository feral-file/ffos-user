package offlinecache

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// pushDropped is notifyQueue.push's dropped flag alone, for the many
// assertions below that are not about the pending count it also returns.
func pushDropped(q *notifyQueue, status ItemStatus) bool {
	dropped, _ := q.push(status)
	return dropped
}

// TestNotifyQueue_CoalescesPerItemAndBoundsByDistinctItems pins the three
// properties that make Notifier's bounded buffer safe to hand a max-size
// playlist. They are asserted directly on the queue rather than through a
// wedged transport (notifier_test.go's saturation test covers that end of
// it) so the contract is nailed down without any timing involved:
//
//  1. A newer state for an already-pending item SUPERSEDES the older one
//     rather than queueing behind it. This is what makes the bound a count
//     of items rather than of transitions — the difference between holding
//     1024 slots and needing 3072+ for one playlist.
//  2. It supersedes IN PLACE, keeping the item's original queue position,
//     so an item updating in a tight loop cannot starve items behind it.
//  3. Capacity rejects only genuinely NEW items. An update to something
//     already pending must never be dropped for lack of room, since it
//     consumes no additional slot — dropping it would discard the very
//     latest state while keeping a stale one.
func TestNotifyQueue_CoalescesPerItemAndBoundsByDistinctItems(t *testing.T) {
	t.Run("a newer state supersedes an undelivered one, in place", func(t *testing.T) {
		q := newNotifyQueue(4)

		require.False(t, pushDropped(q, ItemStatus{ItemID: "item-1", State: StateQueued}))
		require.False(t, pushDropped(q, ItemStatus{ItemID: "item-2", State: StateQueued}))
		require.False(t, pushDropped(q, ItemStatus{ItemID: "item-1", State: StateDownloading}))
		require.False(t, pushDropped(q, ItemStatus{ItemID: "item-1", State: StateReady}))

		assert.Equal(t, 2, q.len(), "three pushes for item-1 must occupy exactly one slot")

		first, ok := q.pop()
		require.True(t, ok)
		assert.Equal(t, "item-1", first.ItemID, "item-1 must keep the position of its FIRST pending notification")
		assert.Equal(t, StateReady, first.State, "only the latest state of item-1 may be delivered")

		second, ok := q.pop()
		require.True(t, ok)
		assert.Equal(t, "item-2", second.ItemID)

		_, ok = q.pop()
		assert.False(t, ok, "the queue must be empty once both items are delivered")
	})

	t.Run("capacity rejects new items but never updates to pending ones", func(t *testing.T) {
		q := newNotifyQueue(2)

		require.False(t, pushDropped(q, ItemStatus{ItemID: "item-1", State: StateQueued}))
		require.False(t, pushDropped(q, ItemStatus{ItemID: "item-2", State: StateQueued}))

		dropped, pending := q.push(ItemStatus{ItemID: "item-3", State: StateQueued})
		assert.True(t, dropped, "a third DISTINCT item must be reported as dropped at a capacity of 2")
		assert.Equal(t, 2, pending,
			"push must report the pending count it decided on, sampled under its own lock, so a drop log cannot contradict the drop")

		assert.False(t, pushDropped(q, ItemStatus{ItemID: "item-1", State: StateReady}),
			"an update to an already-pending item consumes no slot and must never be dropped")

		status, ok := q.pop()
		require.True(t, ok)
		assert.Equal(t, StateReady, status.State,
			"the update accepted at capacity must be what gets delivered, not the stale state it replaced")

		// The freed slot admits a new item again, so a full queue is a
		// transient condition rather than a latch.
		assert.False(t, pushDropped(q, ItemStatus{ItemID: "item-3", State: StateQueued}))
	})

	t.Run("a fresh push after delivery re-queues the item at the back", func(t *testing.T) {
		q := newNotifyQueue(4)

		require.False(t, pushDropped(q, ItemStatus{ItemID: "item-1", State: StateQueued}))
		require.False(t, pushDropped(q, ItemStatus{ItemID: "item-2", State: StateQueued}))
		first, ok := q.pop()
		require.True(t, ok)
		require.Equal(t, "item-1", first.ItemID)

		// item-1 is no longer pending, so this is a new entry rather than a
		// coalesce — it must not jump ahead of item-2.
		require.False(t, pushDropped(q, ItemStatus{ItemID: "item-1", State: StateReady}))
		next, ok := q.pop()
		require.True(t, ok)
		assert.Equal(t, "item-2", next.ItemID, "an already-delivered item re-queues behind what is still waiting")
	})
}

// TestNotifyQueue_WakeSignalsOnlyForNewlyQueuedItems pins the signaling
// half of the coalescing contract: wake is a 1-buffered signal channel, and
// a coalesced update deliberately does not re-signal (the entry is already
// queued, so the worker either has not reached it yet or is about to be
// woken by the push that queued it). Without that, a hot item would spin
// the worker on every update for no additional delivery.
func TestNotifyQueue_WakeSignalsOnlyForNewlyQueuedItems(t *testing.T) {
	q := newNotifyQueue(4)

	require.False(t, pushDropped(q, ItemStatus{ItemID: "item-1", State: StateQueued}))
	select {
	case <-q.wake:
	default:
		t.Fatal("queueing a new item must wake the delivery worker")
	}

	require.False(t, pushDropped(q, ItemStatus{ItemID: "item-1", State: StateReady}))
	select {
	case <-q.wake:
		t.Fatal("a coalesced update must not re-signal: its entry is already queued and the worker will read the newer status when it gets there")
	default:
	}
}
