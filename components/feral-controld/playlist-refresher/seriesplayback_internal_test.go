package refresher

import (
	"testing"

	dp1playlist "github.com/display-protocol/dp1-go/playlist"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/feral-file/ffos-user/components/feral-controld/dp1"
)

// randomGroupPlan builds a fresh CastPlan carrying one resolved
// multi_unique series group (three distinct editions), matching the shape
// resolveSeriesCastPlan produces for a real series. Built fresh per call so
// a test can mutate one copy's Editions without affecting another.
func randomGroupPlan() dp1.CastPlan {
	return dp1.CastPlan{Segments: []dp1.CastSegment{
		{Group: &dp1.ResolvedSeriesGroup{
			Key:            dp1.SeriesGroupKey{Chain: "evm", Address: "0xabc", SeriesID: "42"},
			Representative: dp1playlist.PlaylistItem{ID: "item-series", Source: "https://cdn.example.com/series.html"},
			Editions: []dp1playlist.PlaylistItem{
				{ID: "a", Source: "https://cdn.example.com/a.mp4"},
				{ID: "b", Source: "https://cdn.example.com/b.mp4"},
				{ID: "c", Source: "https://cdn.example.com/c.mp4"},
			},
			Resolved:     true,
			ArtworkModel: dp1.SeriesArtworkModelMultiUnique,
		}},
	}}
}

// TestNarrowStable_RandomModeStaysStableAcrossRefreshes pins the fix for
// feralfile-bot F3 (2026-11-12): repeated periodic resyncs of an unchanged
// multi_unique series under random mode must not re-roll the selected
// edition on every tick — only an actual mode change or the previously
// selected edition disappearing from the catalog's answer may do that.
func TestNarrowStable_RandomModeStaysStableAcrossRefreshes(t *testing.T) {
	r := &refresher{}
	mode := dp1.SeriesPlaybackRandom

	first := r.narrowStable(randomGroupPlan(), &mode, nil)
	require.Len(t, first, 1)

	for i := 0; i < 10; i++ {
		again := r.narrowStable(randomGroupPlan(), &mode, nil)
		require.Len(t, again, 1)
		assert.Equal(t, first[0].Source, again[0].Source,
			"an unchanged random-mode group must keep its pick across refreshes")
	}
}

// TestNarrowStable_RandomModeReselectsWhenEditionDisappears pins the other
// escape hatch F3 asked for: a remembered pick no longer among the group's
// current editions forces a fresh selection rather than being served
// regardless of whether it is still playable.
func TestNarrowStable_RandomModeReselectsWhenEditionDisappears(t *testing.T) {
	r := &refresher{}
	mode := dp1.SeriesPlaybackRandom

	first := r.narrowStable(randomGroupPlan(), &mode, nil)
	require.Len(t, first, 1)

	shrunk := randomGroupPlan()
	shrunk.Segments[0].Group.Editions = []dp1playlist.PlaylistItem{
		{ID: "d", Source: "https://cdn.example.com/d.mp4"},
	}
	again := r.narrowStable(shrunk, &mode, nil)
	require.Len(t, again, 1)
	assert.Equal(t, "https://cdn.example.com/d.mp4", again[0].Source,
		"the previous pick is gone from the editions, so a fresh pick must be made")
}

// TestNarrowStable_ModeChangeForcesFreshPick pins the "setting change"
// escape hatch: resolving to a non-random mode clears any remembered pick
// for that group, so switching back to random later does not replay a
// stale selection from before the switch.
func TestNarrowStable_ModeChangeForcesFreshPick(t *testing.T) {
	r := &refresher{}
	random := dp1.SeriesPlaybackRandom
	first := dp1.SeriesPlaybackFirst

	picked := r.narrowStable(randomGroupPlan(), &random, nil)
	require.Len(t, picked, 1)

	full := r.narrowStable(randomGroupPlan(), &first, nil)
	require.Len(t, full, 1)
	assert.Equal(t, "item-series", full[0].ID,
		"first mode always narrows to the representative, regardless of any remembered random pick")

	backToRandom := r.narrowStable(randomGroupPlan(), &random, nil)
	require.Len(t, backToRandom, 1, "the mode-change memory clear must not leave narrowStable with nothing to return")
}

// TestNarrowStable_PlainItemsPassThrough covers the non-series sibling the
// same loop handles: an ordinary playlist position must come through
// unchanged and untouched by the random-pick memory.
func TestNarrowStable_PlainItemsPassThrough(t *testing.T) {
	r := &refresher{}
	item := dp1playlist.PlaylistItem{ID: "plain", Source: "https://cdn.example.com/plain.png"}
	plan := dp1.CastPlan{Segments: []dp1.CastSegment{{Item: &item}}}

	out := r.narrowStable(plan, nil, nil)
	require.Len(t, out, 1)
	assert.Equal(t, "plain", out[0].ID)
}

// TestNarrowStable_PrunesPicksForGroupsNoLongerDisplayed pins the fix for
// run-reviewer's own finding on this branch (round 1, 2026-11-12): a
// remembered random pick must not outlive the group it belongs to once
// that group stops appearing in the plan at all (the device moves on to a
// different playlist/exhibition) — the mode-change delete alone only
// covers a group still present under a new mode, leaving this sibling an
// unbounded, process-lifetime leak.
func TestNarrowStable_PrunesPicksForGroupsNoLongerDisplayed(t *testing.T) {
	r := &refresher{}
	mode := dp1.SeriesPlaybackRandom

	r.narrowStable(randomGroupPlan(), &mode, nil)
	require.Len(t, r.randomSeriesPicks, 1, "the group's pick must be remembered")

	plainItem := dp1playlist.PlaylistItem{ID: "plain", Source: "https://cdn.example.com/plain.png"}
	unrelated := dp1.CastPlan{Segments: []dp1.CastSegment{{Item: &plainItem}}}
	r.narrowStable(unrelated, &mode, nil)

	assert.Empty(t, r.randomSeriesPicks, "a group absent from the current plan must not be remembered forever")
}

// TestNarrowStable_SeedsFromWhatIsAlreadyDisplayed pins the fix for
// run-reviewer's own finding on this branch (round 2, 2026-11-12): the
// FIRST time narrowStable ever sees a random-mode group — most commonly
// the first periodic resync after a live cast made its own independent
// pick through commandrouter's separate Narrow call, which this package's
// memory never saw — it must seed from whatever the player already
// reports as on screen (currentlyDisplayed) instead of rolling a brand
// new, likely-different pick.
func TestNarrowStable_SeedsFromWhatIsAlreadyDisplayed(t *testing.T) {
	r := &refresher{}
	mode := dp1.SeriesPlaybackRandom

	alreadyShown := dp1playlist.PlaylistItem{ID: "b", Source: "https://cdn.example.com/b.mp4"}
	calls := 0
	currentlyDisplayed := func() []dp1playlist.PlaylistItem {
		calls++
		return []dp1playlist.PlaylistItem{alreadyShown}
	}

	out := r.narrowStable(randomGroupPlan(), &mode, currentlyDisplayed)
	require.Len(t, out, 1)
	assert.Equal(t, alreadyShown.Source, out[0].Source,
		"a fresh group must seed from the edition the player already reports on screen")
	assert.Equal(t, 1, calls, "currentlyDisplayed must be consulted when no pick is yet remembered")

	calls = 0
	again := r.narrowStable(randomGroupPlan(), &mode, currentlyDisplayed)
	require.Len(t, again, 1)
	assert.Equal(t, alreadyShown.Source, again[0].Source, "the seeded pick must then stay stable")
	assert.Zero(t, calls, "a group with an already-remembered pick must never consult currentlyDisplayed")
}

// TestNarrowStable_SeedSourceCalledAtMostOnce pins narrowStable's own
// memoization: a plan carrying several unseeded random groups must call
// currentlyDisplayed at most once per narrowStable call (it may be a live
// network round trip), not once per group.
func TestNarrowStable_SeedSourceCalledAtMostOnce(t *testing.T) {
	r := &refresher{}
	mode := dp1.SeriesPlaybackRandom

	second := randomGroupPlan()
	second.Segments[0].Group.Key = dp1.SeriesGroupKey{Chain: "evm", Address: "0xdef", SeriesID: "99"}
	second.Segments[0].Group.Editions = []dp1playlist.PlaylistItem{
		{ID: "x", Source: "https://cdn.example.com/x.mp4"},
	}
	plan := dp1.CastPlan{Segments: append(randomGroupPlan().Segments, second.Segments...)}

	calls := 0
	currentlyDisplayed := func() []dp1playlist.PlaylistItem {
		calls++
		return nil
	}

	out := r.narrowStable(plan, &mode, currentlyDisplayed)
	require.Len(t, out, 2)
	assert.Equal(t, 1, calls, "currentlyDisplayed must be called at most once per narrowStable call")
}
