package provisioning

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestTransitionObserverSeesEveryChange pins the netlog observer contract:
// it fires on every REAL state/reason change — including reason-only changes
// on the silent OfflineRetrying legs that the Notifier's narration dedupe
// deliberately hides — and stays quiet on true no-ops.
func TestTransitionObserverSeesEveryChange(t *testing.T) {
	h := newHarness(t)
	type obs struct{ from, to, fromReason, toReason string }
	var seen []obs
	h.m.transitionObserver = func(from, to, fromReason, toReason string) {
		seen = append(seen, obs{from, to, fromReason, toReason})
	}
	ctx := context.Background()

	h.m.transition(ctx, StateOnline, Detail{Message: "connected"})
	require.Len(t, seen, 1, "state change must be observed")
	assert.Equal(t, string(StateOnline), seen[0].to)

	h.m.transition(ctx, StateOnline, Detail{})
	assert.Len(t, seen, 1, "a true no-op (same state, same reason) must not be observed")

	// Reason-only change on a silent leg: the Notifier dedupe hides this from
	// the screen; the recorder must still see it.
	h.m.transition(ctx, StateOfflineRetrying, Detail{Reason: "first-reason"})
	h.m.transition(ctx, StateOfflineRetrying, Detail{Reason: "second-reason"})
	require.Len(t, seen, 3)
	assert.Equal(t, "first-reason", seen[2].fromReason)
	assert.Equal(t, "second-reason", seen[2].toReason)
	assert.Equal(t, string(StateOfflineRetrying), seen[2].from)
	assert.Equal(t, string(StateOfflineRetrying), seen[2].to)
}

// TestTransitionObserverSeesDirectJoinEdge: applyJoin sets StateJoining
// without going through transition(); the observer must still see the edge.
func TestTransitionObserverSeesDirectJoinEdge(t *testing.T) {
	h := newHarness(t)
	var tos []string
	h.m.transitionObserver = func(_, to, _, _ string) { tos = append(tos, to) }
	ctx := context.Background()

	h.m.transition(ctx, StateAPActive, Detail{Reason: "test-raise"})
	h.m.applyJoin(ctx, "VenueWifi", "password1", false)

	assert.Contains(t, tos, string(StateJoining), "the direct join edge must be observed")
}
