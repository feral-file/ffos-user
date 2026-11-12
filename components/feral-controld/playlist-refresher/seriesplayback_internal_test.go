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

	first := r.narrowStable(randomGroupPlan(), &mode)
	require.Len(t, first, 1)

	for i := 0; i < 10; i++ {
		again := r.narrowStable(randomGroupPlan(), &mode)
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

	first := r.narrowStable(randomGroupPlan(), &mode)
	require.Len(t, first, 1)

	shrunk := randomGroupPlan()
	shrunk.Segments[0].Group.Editions = []dp1playlist.PlaylistItem{
		{ID: "d", Source: "https://cdn.example.com/d.mp4"},
	}
	again := r.narrowStable(shrunk, &mode)
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

	picked := r.narrowStable(randomGroupPlan(), &random)
	require.Len(t, picked, 1)

	full := r.narrowStable(randomGroupPlan(), &first)
	require.Len(t, full, 1)
	assert.Equal(t, "item-series", full[0].ID,
		"first mode always narrows to the representative, regardless of any remembered random pick")

	backToRandom := r.narrowStable(randomGroupPlan(), &random)
	require.Len(t, backToRandom, 1, "the mode-change memory clear must not leave narrowStable with nothing to return")
}

// TestNarrowStable_PlainItemsPassThrough covers the non-series sibling the
// same loop handles: an ordinary playlist position must come through
// unchanged and untouched by the random-pick memory.
func TestNarrowStable_PlainItemsPassThrough(t *testing.T) {
	r := &refresher{}
	item := dp1playlist.PlaylistItem{ID: "plain", Source: "https://cdn.example.com/plain.png"}
	plan := dp1.CastPlan{Segments: []dp1.CastSegment{{Item: &item}}}

	out := r.narrowStable(plan, nil)
	require.Len(t, out, 1)
	assert.Equal(t, "plain", out[0].ID)
}
