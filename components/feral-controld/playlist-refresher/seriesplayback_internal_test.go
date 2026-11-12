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
// new, likely-different pick. It also pins feralfile-bot's F6
// (2026-11-12): currentlyDisplayed is consulted again on EVERY later
// tick too, so a pick already remembered still reconfirms itself against
// the live display rather than skipping that check — an ordinary tick
// where nothing changed on screen must therefore leave the pick stable.
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
	assert.Equal(t, alreadyShown.Source, again[0].Source,
		"an ordinary tick where the display still agrees with the remembered pick must leave it stable")
	assert.Equal(t, 1, calls,
		"currentlyDisplayed must be consulted again even when a pick is already remembered (feralfile-bot F6)")
}

// TestNarrowStable_OverwritesStalePickWhenDisplayDisagrees pins
// feralfile-bot's F6 (2026-11-12) for the case
// TestNarrowStable_SeedsFromWhatIsAlreadyDisplayed does not cover: a pick
// is already remembered, and a LATER tick's live display reports a
// DIFFERENT edition of the same group — the newer accepted cast's own
// choice (run-reviewer, round 1 on this fix, 2026-11-12, flagged the
// overwrite path itself had no test). The remembered pick must be
// overwritten to match the display, not left at the older selection.
func TestNarrowStable_OverwritesStalePickWhenDisplayDisagrees(t *testing.T) {
	r := &refresher{}
	mode := dp1.SeriesPlaybackRandom

	editionA := dp1playlist.PlaylistItem{ID: "a", Source: "https://cdn.example.com/a.mp4"}
	editionB := dp1playlist.PlaylistItem{ID: "b", Source: "https://cdn.example.com/b.mp4"}

	showing := editionA
	currentlyDisplayed := func() []dp1playlist.PlaylistItem {
		return []dp1playlist.PlaylistItem{showing}
	}

	first := r.narrowStable(randomGroupPlan(), &mode, currentlyDisplayed)
	require.Len(t, first, 1)
	assert.Equal(t, editionA.Source, first[0].Source, "sanity: seeded from the initial display")

	key := dp1.SeriesGroupKey{Chain: "evm", Address: "0xabc", SeriesID: "42"}
	require.Equal(t, editionA.Source, r.randomSeriesPicks[key].source, "sanity: the pick was remembered")

	showing = editionB
	second := r.narrowStable(randomGroupPlan(), &mode, currentlyDisplayed)
	require.Len(t, second, 1)
	assert.Equal(t, editionB.Source, second[0].Source,
		"a newer accepted cast's own choice, read from the live display, must replace the remembered pick")
	assert.Equal(t, editionB.Source, r.randomSeriesPicks[key].source,
		"the overwrite must actually be stored, not just returned for this one tick")
}

// TestNarrowStable_RefreshesNonSourceFieldsOfAStablePick pins the fix for
// run-reviewer's own finding on this branch (round 3, 2026-11-12): a
// remembered pick's source staying stable across ticks must not freeze the
// rest of that edition's fields at whatever they were the moment the pick
// was first made — a later catalog ingest changing e.g. duration or
// contentRating on that same edition must be reflected on the very next
// tick, not held stale for as long as the pick is held.
func TestNarrowStable_RefreshesNonSourceFieldsOfAStablePick(t *testing.T) {
	r := &refresher{}
	mode := dp1.SeriesPlaybackRandom

	duration1 := 5.0
	first := randomGroupPlan()
	for i := range first.Segments[0].Group.Editions {
		first.Segments[0].Group.Editions[i].Duration = &duration1
	}
	picked := r.narrowStable(first, &mode, nil)
	require.Len(t, picked, 1)
	require.NotNil(t, picked[0].Duration)
	assert.Equal(t, duration1, *picked[0].Duration)

	duration2 := 20.0
	second := randomGroupPlan()
	for i := range second.Segments[0].Group.Editions {
		second.Segments[0].Group.Editions[i].Duration = &duration2
	}
	again := r.narrowStable(second, &mode, nil)
	require.Len(t, again, 1)
	assert.Equal(t, picked[0].Source, again[0].Source, "the pick itself must still be stable")
	require.NotNil(t, again[0].Duration, "a stable pick must still carry the field the catalog now states")
	assert.Equal(t, duration2, *again[0].Duration,
		"a stable pick's other fields must track the catalog's latest answer, not freeze at pick time")
}

// TestNarrowStable_ArtworkModelChangeForcesFreshPick pins the fix for
// run-reviewer's own finding on this branch (round 4, 2026-11-12): a
// memoized pick must not survive the group's own ArtworkModel changing
// from multi_unique (which has a real distinct-editions choice to make) to
// multi (which does not — dp1.NarrowEditions always collapses it to the
// representative). Reusing the stale pick would keep showing one specific
// edition instead of the representative a fresh NarrowEditions call
// requires.
func TestNarrowStable_ArtworkModelChangeForcesFreshPick(t *testing.T) {
	r := &refresher{}
	mode := dp1.SeriesPlaybackRandom

	picked := r.narrowStable(randomGroupPlan(), &mode, nil)
	require.Len(t, picked, 1)
	require.NotEqual(t, "item-series", picked[0].ID, "sanity: multi_unique must pick a real edition, not the representative")

	reclassified := randomGroupPlan()
	reclassified.Segments[0].Group.ArtworkModel = dp1.SeriesArtworkModelMulti
	again := r.narrowStable(reclassified, &mode, nil)
	require.Len(t, again, 1)
	assert.Equal(t, "item-series", again[0].ID,
		"a group reclassified to multi must collapse to the representative, not replay the multi_unique-era pick")
}

// TestNarrowStable_SingleMultiNeverSeedsOrMemoizesEvenWhenDisplayedSourceMatches
// pins feralfile-bot's F5 (2026-11-12) for the path
// TestNarrowStable_ArtworkModelChangeForcesFreshPick does not cover
// (run-reviewer, round 3 on this fix, 2026-11-12: that test only reaches
// the gate via RECLASSIFICATION, never from the first tick): a single/multi
// group's own one edition can share its Source with what the player
// already displays from the very FIRST call, with no reclassification
// involved at all. The top-level HasDistinctEditions gate must still route
// it straight to the representative and never let it reach the
// seed/memoize path below — reusing or memoizing that coincidental match
// would make it look like a genuine distinct random pick.
func TestNarrowStable_SingleMultiNeverSeedsOrMemoizesEvenWhenDisplayedSourceMatches(t *testing.T) {
	r := &refresher{}
	mode := dp1.SeriesPlaybackRandom

	plan := dp1.CastPlan{Segments: []dp1.CastSegment{
		{Group: &dp1.ResolvedSeriesGroup{
			Key:            dp1.SeriesGroupKey{Chain: "evm", Address: "0xabc", SeriesID: "42"},
			Representative: dp1playlist.PlaylistItem{ID: "item-series", Source: "https://cdn.example.com/series.html"},
			Editions: []dp1playlist.PlaylistItem{
				{ID: "a", Source: "https://cdn.example.com/a.mp4"},
			},
			Resolved:     true,
			ArtworkModel: dp1.SeriesArtworkModelSingle,
		}},
	}}

	displayed := dp1playlist.PlaylistItem{ID: "a", Source: "https://cdn.example.com/a.mp4"}
	currentlyDisplayed := func() []dp1playlist.PlaylistItem {
		return []dp1playlist.PlaylistItem{displayed}
	}

	out := r.narrowStable(plan, &mode, currentlyDisplayed)
	require.Len(t, out, 1)
	assert.Equal(t, "item-series", out[0].ID,
		"a single/multi group must always collapse to the representative, even when the player happens to show its own edition")
	assert.Empty(t, r.randomSeriesPicks,
		"a single/multi group must never be memoized, even when its edition's Source coincidentally matches the display")
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
