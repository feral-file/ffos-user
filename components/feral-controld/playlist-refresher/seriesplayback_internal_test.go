package refresher

import (
	"testing"
	"time"

	"github.com/display-protocol/dp1-go/extension/contentrating"
	dp1playlist "github.com/display-protocol/dp1-go/playlist"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/feral-file/ffos-user/components/feral-controld/contentpolicy"
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

	first := r.narrowStable(randomGroupPlan(), &mode, nil, nil)
	require.Len(t, first, 1)

	for i := 0; i < 10; i++ {
		again := r.narrowStable(randomGroupPlan(), &mode, nil, nil)
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

	first := r.narrowStable(randomGroupPlan(), &mode, nil, nil)
	require.Len(t, first, 1)

	shrunk := randomGroupPlan()
	shrunk.Segments[0].Group.Editions = []dp1playlist.PlaylistItem{
		{ID: "d", Source: "https://cdn.example.com/d.mp4"},
	}
	again := r.narrowStable(shrunk, &mode, nil, nil)
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

	picked := r.narrowStable(randomGroupPlan(), &random, nil, nil)
	require.Len(t, picked, 1)

	full := r.narrowStable(randomGroupPlan(), &first, nil, nil)
	require.Len(t, full, 1)
	assert.Equal(t, "item-series", full[0].ID,
		"first mode always narrows to the representative, regardless of any remembered random pick")

	backToRandom := r.narrowStable(randomGroupPlan(), &random, nil, nil)
	require.Len(t, backToRandom, 1, "the mode-change memory clear must not leave narrowStable with nothing to return")
}

// TestNarrowStable_PlainItemsPassThrough covers the non-series sibling the
// same loop handles: an ordinary playlist position must come through
// unchanged and untouched by the random-pick memory.
func TestNarrowStable_PlainItemsPassThrough(t *testing.T) {
	r := &refresher{}
	item := dp1playlist.PlaylistItem{ID: "plain", Source: "https://cdn.example.com/plain.png"}
	plan := dp1.CastPlan{Segments: []dp1.CastSegment{{Item: &item}}}

	out := r.narrowStable(plan, nil, nil, nil)
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

	r.narrowStable(randomGroupPlan(), &mode, nil, nil)
	require.Len(t, r.randomSeriesPicks, 1, "the group's pick must be remembered")

	plainItem := dp1playlist.PlaylistItem{ID: "plain", Source: "https://cdn.example.com/plain.png"}
	unrelated := dp1.CastPlan{Segments: []dp1.CastSegment{{Item: &plainItem}}}
	r.narrowStable(unrelated, &mode, nil, nil)

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

	out := r.narrowStable(randomGroupPlan(), &mode, currentlyDisplayed, nil)
	require.Len(t, out, 1)
	assert.Equal(t, alreadyShown.Source, out[0].Source,
		"a fresh group must seed from the edition the player already reports on screen")
	assert.Equal(t, 1, calls, "currentlyDisplayed must be consulted when no pick is yet remembered")

	calls = 0
	again := r.narrowStable(randomGroupPlan(), &mode, currentlyDisplayed, nil)
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

	first := r.narrowStable(randomGroupPlan(), &mode, currentlyDisplayed, nil)
	require.Len(t, first, 1)
	assert.Equal(t, editionA.Source, first[0].Source, "sanity: seeded from the initial display")

	key := dp1.SeriesGroupKey{Chain: "evm", Address: "0xabc", SeriesID: "42"}
	require.Equal(t, editionA.Source, r.randomSeriesPicks[key].source, "sanity: the pick was remembered")

	showing = editionB
	second := r.narrowStable(randomGroupPlan(), &mode, currentlyDisplayed, nil)
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
	picked := r.narrowStable(first, &mode, nil, nil)
	require.Len(t, picked, 1)
	require.NotNil(t, picked[0].Duration)
	assert.Equal(t, duration1, *picked[0].Duration)

	duration2 := 20.0
	second := randomGroupPlan()
	for i := range second.Segments[0].Group.Editions {
		second.Segments[0].Group.Editions[i].Duration = &duration2
	}
	again := r.narrowStable(second, &mode, nil, nil)
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

	picked := r.narrowStable(randomGroupPlan(), &mode, nil, nil)
	require.Len(t, picked, 1)
	require.NotEqual(t, "item-series", picked[0].ID, "sanity: multi_unique must pick a real edition, not the representative")

	reclassified := randomGroupPlan()
	reclassified.Segments[0].Group.ArtworkModel = dp1.SeriesArtworkModelMulti
	again := r.narrowStable(reclassified, &mode, nil, nil)
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

	out := r.narrowStable(plan, &mode, currentlyDisplayed, nil)
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

	out := r.narrowStable(plan, &mode, currentlyDisplayed, nil)
	require.Len(t, out, 2)
	assert.Equal(t, 1, calls, "currentlyDisplayed must be called at most once per narrowStable call")
}

// TestNarrowStable_MatchesPickToItsOwnOccurrenceNotAnEarlierExplicitItem pins
// feralfile-bot's F7 (2026-11-12): a playlist explicitly lists edition A
// ahead of a multi_unique series that also contains A (alongside B). The
// live random cast picked B for the series' own slot, so displayed is
// [A, B] — A from the explicit item's own position, B from the group's.
// Matching by whole-list Source scan mistook the explicit A for the
// group's pick and overwrote the remembered B with it; matching by
// occurrence must read displayed[1] (the group's own slot) and keep B.
func TestNarrowStable_MatchesPickToItsOwnOccurrenceNotAnEarlierExplicitItem(t *testing.T) {
	r := &refresher{}
	mode := dp1.SeriesPlaybackRandom

	explicitA := dp1playlist.PlaylistItem{ID: "explicit-a", Source: "https://cdn.example.com/a.mp4"}
	plan := dp1.CastPlan{Segments: append(
		[]dp1.CastSegment{{Item: &explicitA}},
		randomGroupPlan().Segments...,
	)}

	editionA := dp1playlist.PlaylistItem{ID: "explicit-a", Source: "https://cdn.example.com/a.mp4"}
	editionB := dp1playlist.PlaylistItem{ID: "b", Source: "https://cdn.example.com/b.mp4"}
	currentlyDisplayed := func() []dp1playlist.PlaylistItem {
		return []dp1playlist.PlaylistItem{editionA, editionB}
	}

	out := r.narrowStable(plan, &mode, currentlyDisplayed, nil)
	require.Len(t, out, 2)
	assert.Equal(t, explicitA.Source, out[0].Source, "the explicit item passes through unchanged")
	assert.Equal(t, editionB.Source, out[1].Source,
		"the group's own occurrence shows B, so the group's pick must be B — not the explicit item's coincidentally-shared Source")

	key := dp1.SeriesGroupKey{Chain: "evm", Address: "0xabc", SeriesID: "42"}
	assert.Equal(t, editionB.Source, r.randomSeriesPicks[key].source,
		"the remembered pick must be the group's own live choice, not the unrelated earlier item")
}

// TestNarrowStable_MatchesPickToItsOwnOccurrenceNotALaterExplicitItem is
// F7's mirror sibling: the coincidentally-shared-Source item sits AFTER the
// series instead of before it. The group's own occurrence (index 0) must
// still be read from the group's own slot, not from the later explicit
// item that happens to share a Source with one of its editions.
func TestNarrowStable_MatchesPickToItsOwnOccurrenceNotALaterExplicitItem(t *testing.T) {
	r := &refresher{}
	mode := dp1.SeriesPlaybackRandom

	explicitA := dp1playlist.PlaylistItem{ID: "explicit-a", Source: "https://cdn.example.com/a.mp4"}
	plan := dp1.CastPlan{Segments: append(
		randomGroupPlan().Segments,
		dp1.CastSegment{Item: &explicitA},
	)}

	editionB := dp1playlist.PlaylistItem{ID: "b", Source: "https://cdn.example.com/b.mp4"}
	explicitAShown := dp1playlist.PlaylistItem{ID: "explicit-a", Source: "https://cdn.example.com/a.mp4"}
	currentlyDisplayed := func() []dp1playlist.PlaylistItem {
		return []dp1playlist.PlaylistItem{editionB, explicitAShown}
	}

	out := r.narrowStable(plan, &mode, currentlyDisplayed, nil)
	require.Len(t, out, 2)
	assert.Equal(t, editionB.Source, out[0].Source,
		"the group's own occurrence (index 0) shows B, so the group's pick must be B")
	assert.Equal(t, explicitA.Source, out[1].Source, "the explicit item passes through unchanged")

	key := dp1.SeriesGroupKey{Chain: "evm", Address: "0xabc", SeriesID: "42"}
	assert.Equal(t, editionB.Source, r.randomSeriesPicks[key].source,
		"the remembered pick must be the group's own live choice, not the unrelated later item")
}

// TestNarrowStable_OccurrenceCountsPastContentPolicyFiltering pins
// feralfile-bot's F7 (2026-11-13): a curated playlist lists a mature item
// ahead of a multi_unique series, which the default content policy removes
// AFTER narrowStable runs (processPlayingPlaylist's own ProjectLocked call
// happens later) — so the group's live display is [B, explicit A], never
// [mature, B, explicit A]. An occurrence computed by raw position (len(items)
// counting the still-present mature item) reads displayed[1] (explicit A)
// instead of the group's own slot, overwriting a correct remembered B with
// an unrelated item — reproducible with no catalog or mode change, unlike
// the whole-list-scan F7 the occurrence check itself fixed on 2026-11-12.
// displayFilter corrects this: occurrence must count only the items the
// SAME content policy would also keep, landing on displayed[0] (B).
func TestNarrowStable_OccurrenceCountsPastContentPolicyFiltering(t *testing.T) {
	r := &refresher{}
	mode := dp1.SeriesPlaybackRandom
	mature := contentrating.RatingMature

	matureItem := dp1playlist.PlaylistItem{ID: "mature", Source: "https://cdn.example.com/mature.mp4", ContentRating: &mature}
	explicitA := dp1playlist.PlaylistItem{ID: "explicit-a", Source: "https://cdn.example.com/a.mp4"}
	plan := dp1.CastPlan{Segments: append(
		append([]dp1.CastSegment{{Item: &matureItem}}, randomGroupPlan().Segments...),
		dp1.CastSegment{Item: &explicitA},
	)}

	editionB := dp1playlist.PlaylistItem{ID: "b", Source: "https://cdn.example.com/b.mp4"}
	// What the player actually shows: content policy already removed the
	// mature item from whatever cast produced this display.
	currentlyDisplayed := func() []dp1playlist.PlaylistItem {
		return []dp1playlist.PlaylistItem{editionB, explicitA}
	}

	policy := contentpolicy.Default()
	displayFilter := func(item dp1playlist.PlaylistItem) bool {
		return policy.Allows(item, contentpolicy.ContextCurated)
	}

	out := r.narrowStable(plan, &mode, currentlyDisplayed, displayFilter)
	require.Len(t, out, 3)
	assert.Equal(t, matureItem.Source, out[0].Source,
		"the mature item still passes through narrowStable unchanged; content policy removes it in a later, separate pass")
	assert.Equal(t, editionB.Source, out[1].Source,
		"occurrence must count past the mature item content policy will filter, landing on displayed's own slot for the group — not shifted by one")
	assert.Equal(t, explicitA.Source, out[2].Source, "the trailing explicit item passes through unchanged")

	key := dp1.SeriesGroupKey{Chain: "evm", Address: "0xabc", SeriesID: "42"}
	assert.Equal(t, editionB.Source, r.randomSeriesPicks[key].source,
		"the remembered pick must be the group's own live choice, not the later explicit item an unfiltered occurrence would have read")
}

// TestNarrowStable_NilDisplayFilterCountsEveryItem pins the no-content-
// policy-context fallback: when displayFilter is nil (no store, or the
// context behind this tick's cast is unknown — processPlayingPlaylist's own
// projection guard skips filtering in exactly that case too), occurrence
// must count every earlier item unfiltered, exactly as narrowStable always
// did before displayFilter existed — nothing will actually be removed on
// cast, so occurrence must not pretend otherwise.
func TestNarrowStable_NilDisplayFilterCountsEveryItem(t *testing.T) {
	r := &refresher{}
	mode := dp1.SeriesPlaybackRandom
	mature := contentrating.RatingMature

	matureItem := dp1playlist.PlaylistItem{ID: "mature", Source: "https://cdn.example.com/mature.mp4", ContentRating: &mature}
	plan := dp1.CastPlan{Segments: append(
		[]dp1.CastSegment{{Item: &matureItem}},
		randomGroupPlan().Segments...,
	)}

	editionA := dp1playlist.PlaylistItem{ID: "a", Source: "https://cdn.example.com/a.mp4"}
	editionB := dp1playlist.PlaylistItem{ID: "b", Source: "https://cdn.example.com/b.mp4"}
	// The mature item was NOT filtered here (no content policy consulted),
	// so displayed still carries it at index 0 — the group's own slot is
	// index 1, exactly len(items) without any filtering.
	currentlyDisplayed := func() []dp1playlist.PlaylistItem {
		return []dp1playlist.PlaylistItem{matureItem, editionB}
	}

	out := r.narrowStable(plan, &mode, currentlyDisplayed, nil)
	require.Len(t, out, 2)
	assert.Equal(t, matureItem.Source, out[0].Source)
	assert.NotEqual(t, editionA.Source, out[1].Source, "sanity: a is not the live pick")
	assert.Equal(t, editionB.Source, out[1].Source,
		"with displayFilter nil, occurrence stays the raw position, matching an unfiltered display")
}

// TestNarrowStable_OccurrenceCountsPastScheduleFiltering pins feralfile-bot's
// F7 follow-up (2026-11-13): a playlist carries a future-displayAt item
// ahead of a multi_unique series, which playlistschedule.Scheduler strips
// out of the active set AFTER narrowStable runs (PrepareWithSource is
// called on narrowStable's own output, later in processPlayingPlaylist) —
// so the group's live display is [B, explicit A], never [future, B,
// explicit A]. Without accounting for displayAt scheduling, occurrence
// read by raw position (counting the future item that was never actually
// shown) lands on displayed[1] (explicit A) instead of the group's own
// slot, overwriting a correct remembered B with an unrelated item —
// reproducible with no catalog, mode, or content-policy change, the
// scheduling-specific sibling of the content-policy version above.
func TestNarrowStable_OccurrenceCountsPastScheduleFiltering(t *testing.T) {
	r := &refresher{}
	mode := dp1.SeriesPlaybackRandom

	futureDisplayAt := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	futureItem := dp1playlist.PlaylistItem{ID: "future", Source: "https://cdn.example.com/future.mp4", DisplayAt: &futureDisplayAt}
	explicitA := dp1playlist.PlaylistItem{ID: "explicit-a", Source: "https://cdn.example.com/a.mp4"}
	plan := dp1.CastPlan{Segments: append(
		append([]dp1.CastSegment{{Item: &futureItem}}, randomGroupPlan().Segments...),
		dp1.CastSegment{Item: &explicitA},
	)}

	editionB := dp1playlist.PlaylistItem{ID: "b", Source: "https://cdn.example.com/b.mp4"}
	// What the player actually shows: the scheduler already stripped the
	// future item from the active set before this cohort ever reached the
	// player, so displayed never carried it at all.
	currentlyDisplayed := func() []dp1playlist.PlaylistItem {
		return []dp1playlist.PlaylistItem{editionB, explicitA}
	}

	out := r.narrowStable(plan, &mode, currentlyDisplayed, nil)
	require.Len(t, out, 3)
	assert.Equal(t, futureItem.Source, out[0].Source,
		"the future item still passes through narrowStable unchanged; displayAt scheduling removes it in a later, separate pass")
	assert.Equal(t, editionB.Source, out[1].Source,
		"occurrence must count past the future-scheduled item displayAt scheduling will filter, landing on displayed's own slot for the group — not shifted by one")
	assert.Equal(t, explicitA.Source, out[2].Source, "the trailing explicit item passes through unchanged")

	key := dp1.SeriesGroupKey{Chain: "evm", Address: "0xabc", SeriesID: "42"}
	assert.Equal(t, editionB.Source, r.randomSeriesPicks[key].source,
		"the remembered pick must be the group's own live choice, not the later explicit item an unfiltered occurrence would have read")
}

// TestNarrowStable_PreservesRememberedPickWhenGroupItselfIsScheduledOut pins
// the Action's second half: a group with no active displayed occurrence at
// all (its own segment is itself future-scheduled) must not consult
// currentlyDisplayed — there is no slot for it to read — and must instead
// preserve whatever was remembered, exactly like "nothing currently
// displayed" already did for a missing player status.
func TestNarrowStable_PreservesRememberedPickWhenGroupItselfIsScheduledOut(t *testing.T) {
	r := &refresher{}
	mode := dp1.SeriesPlaybackRandom

	key := dp1.SeriesGroupKey{Chain: "evm", Address: "0xabc", SeriesID: "42"}
	r.randomSeriesPicks = map[dp1.SeriesGroupKey]randomSeriesPick{
		key: {mode: dp1.SeriesPlaybackRandom, artworkModel: dp1.SeriesArtworkModelMultiUnique, source: "https://cdn.example.com/b.mp4"},
	}

	futureDisplayAt := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	plan := randomGroupPlan()
	plan.Segments[0].Group.Representative.DisplayAt = &futureDisplayAt
	for i := range plan.Segments[0].Group.Editions {
		plan.Segments[0].Group.Editions[i].DisplayAt = &futureDisplayAt
	}

	// currentlyDisplayed must not be trusted for a group with no active
	// occurrence this tick: if narrowStable read it anyway, this
	// deliberately wrong item would be adopted and the assertion below
	// would catch it.
	wrongPick := dp1playlist.PlaylistItem{ID: "a", Source: "https://cdn.example.com/a.mp4"}
	currentlyDisplayed := func() []dp1playlist.PlaylistItem {
		return []dp1playlist.PlaylistItem{wrongPick}
	}

	out := r.narrowStable(plan, &mode, currentlyDisplayed, nil)
	require.Len(t, out, 1)
	assert.Equal(t, "https://cdn.example.com/b.mp4", out[0].Source,
		"a group with no active displayed occurrence must keep its remembered pick, not read an unrelated displayed item")
}

// TestNarrowStable_ScheduleActiveSegmentsExcludesContentWithheldItemsFromCohort
// pins feralfile-bot's second F7 follow-up (2026-11-13): scheduleActiveSegments
// used to decide the "latest scheduled" cohort over EVERY segment's raw
// displayAt, including one content policy withholds before the real
// scheduler ever sees it. A mature item with a newer past displayAt than a
// legitimately-displayable, older-past-displayAt item could still win that
// cohort and mark the earlier item inactive — demoting an item the real,
// already-policy-filtered scheduler keeps active, and shifting the
// occurrence count for a random group that follows both. Policy removing
// the mature item BEFORE the real scheduler runs is exactly the ordering
// scheduleActiveSegments must mirror.
func TestNarrowStable_ScheduleActiveSegmentsExcludesContentWithheldItemsFromCohort(t *testing.T) {
	r := &refresher{}
	mode := dp1.SeriesPlaybackRandom
	mature := contentrating.RatingMature

	olderPast := time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339)
	newerPast := time.Now().Add(-1 * time.Hour).UTC().Format(time.RFC3339)
	scheduledOld := dp1playlist.PlaylistItem{ID: "scheduled-old", Source: "https://cdn.example.com/a.mp4", DisplayAt: &olderPast}
	matureNew := dp1playlist.PlaylistItem{ID: "mature-new", Source: "https://cdn.example.com/m.mp4", DisplayAt: &newerPast, ContentRating: &mature}

	editionA := dp1playlist.PlaylistItem{ID: "a", Source: "https://cdn.example.com/a.mp4"}
	editionB := dp1playlist.PlaylistItem{ID: "b", Source: "https://cdn.example.com/b.mp4"}
	key := dp1.SeriesGroupKey{Chain: "evm", Address: "0xabc", SeriesID: "42"}
	plan := dp1.CastPlan{Segments: []dp1.CastSegment{
		{Item: &scheduledOld},
		{Item: &matureNew},
		{Group: &dp1.ResolvedSeriesGroup{
			Key:            key,
			Representative: dp1playlist.PlaylistItem{ID: "item-series", Source: "https://cdn.example.com/series.html"},
			Editions:       []dp1playlist.PlaylistItem{editionA, editionB},
			Resolved:       true,
			ArtworkModel:   dp1.SeriesArtworkModelMultiUnique,
		}},
	}}
	// Already selected on an earlier tick.
	r.randomSeriesPicks = map[dp1.SeriesGroupKey]randomSeriesPick{
		key: {mode: dp1.SeriesPlaybackRandom, artworkModel: dp1.SeriesArtworkModelMultiUnique, source: editionB.Source},
	}

	// What the player actually shows: content policy already removed the
	// mature item before the real scheduler ever ran, so the older-past item
	// is the real scheduler's only competitor for "latest" and stays active.
	currentlyDisplayed := func() []dp1playlist.PlaylistItem {
		return []dp1playlist.PlaylistItem{scheduledOld, editionB}
	}

	policy := contentpolicy.Default()
	displayFilter := func(item dp1playlist.PlaylistItem) bool {
		return policy.Allows(item, contentpolicy.ContextCurated)
	}

	out := r.narrowStable(plan, &mode, currentlyDisplayed, displayFilter)
	require.Len(t, out, 3)
	assert.Equal(t, scheduledOld.Source, out[0].Source,
		"the older-past item still passes through narrowStable unchanged")
	assert.Equal(t, matureNew.Source, out[1].Source,
		"the mature item still passes through narrowStable unchanged; content policy removes it in a later, separate pass")
	assert.Equal(t, editionB.Source, out[2].Source,
		"the remembered pick B must be preserved: the mature item must not out-rank the older-past item for the scheduling cohort, "+
			"or occurrence misreads the explicit older-past item's position as the group's own live pick")
	assert.Equal(t, editionB.Source, r.randomSeriesPicks[key].source,
		"the remembered pick must stay B, not be overwritten by a coincidental Source match on the older-past item")
}

// TestNarrowStable_PreservesRememberedPickWhenItIsContentWithheld pins
// feralfile-bot's F8 (2026-11-13): a random group's remembered pick that
// content policy itself withholds has no slot in the real, already-filtered
// displayed list at all — the projection drops it entirely rather than
// leaving a placeholder — so occurrence lands on whatever follows it
// instead, and a coincidental Source match there must not be adopted as the
// group's live pick. The remembered, still-withheld pick must be preserved
// so content policy continues withholding the SAME thing next tick too.
func TestNarrowStable_PreservesRememberedPickWhenItIsContentWithheld(t *testing.T) {
	r := &refresher{}
	mode := dp1.SeriesPlaybackRandom
	mature := contentrating.RatingMature

	editionA := dp1playlist.PlaylistItem{ID: "a", Source: "https://cdn.example.com/a.mp4"}
	editionB := dp1playlist.PlaylistItem{ID: "b", Source: "https://cdn.example.com/b.mp4", ContentRating: &mature}
	// Shares its Source with editionA, the exact shape that let a wrongly
	// computed occurrence be mistaken for the group's own live pick.
	explicitA := dp1playlist.PlaylistItem{ID: "explicit-a", Source: editionA.Source}
	key := dp1.SeriesGroupKey{Chain: "evm", Address: "0xabc", SeriesID: "42"}
	plan := dp1.CastPlan{Segments: []dp1.CastSegment{
		{Group: &dp1.ResolvedSeriesGroup{
			Key:            key,
			Representative: dp1playlist.PlaylistItem{ID: "item-series", Source: "https://cdn.example.com/series.html"},
			Editions:       []dp1playlist.PlaylistItem{editionA, editionB},
			Resolved:       true,
			ArtworkModel:   dp1.SeriesArtworkModelMultiUnique,
		}},
		{Item: &explicitA},
	}}
	// B was selected on an earlier tick; content policy has always withheld it.
	r.randomSeriesPicks = map[dp1.SeriesGroupKey]randomSeriesPick{
		key: {mode: dp1.SeriesPlaybackRandom, artworkModel: dp1.SeriesArtworkModelMultiUnique, source: editionB.Source},
	}

	// What the player actually shows: B has no slot at all (withheld with
	// no placeholder), leaving only the trailing explicit item.
	currentlyDisplayed := func() []dp1playlist.PlaylistItem {
		return []dp1playlist.PlaylistItem{explicitA}
	}

	policy := contentpolicy.Default()
	displayFilter := func(item dp1playlist.PlaylistItem) bool {
		return policy.Allows(item, contentpolicy.ContextCurated)
	}

	out := r.narrowStable(plan, &mode, currentlyDisplayed, displayFilter)
	require.Len(t, out, 2)
	assert.Equal(t, editionB.Source, out[0].Source,
		"the content-withheld remembered pick B must be preserved, not overwritten by the following explicit item's coincidental Source match")
	assert.Equal(t, explicitA.Source, out[1].Source, "the trailing explicit item passes through unchanged")
	assert.Equal(t, editionB.Source, r.randomSeriesPicks[key].source,
		"the remembered pick must stay B so content policy continues withholding the same thing next tick")
}
