package dp1_test

import (
	encjson "encoding/json"
	"testing"

	dp1playlist "github.com/display-protocol/dp1-go/playlist"
	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/feral-file/ffos-user/components/feral-controld/dp1"
)

func mode(m dp1.SeriesPlaybackMode) *dp1.SeriesPlaybackMode { return &m }

func TestResolveSeriesPlaybackMode_DeviceWinsOverDocument(t *testing.T) {
	got := dp1.ResolveSeriesPlaybackMode(mode(dp1.SeriesPlaybackFull), mode(dp1.SeriesPlaybackFirst))
	assert.Equal(t, dp1.SeriesPlaybackFirst, got)
}

func TestResolveSeriesPlaybackMode_DocumentWinsWhenNoDeviceOverride(t *testing.T) {
	got := dp1.ResolveSeriesPlaybackMode(mode(dp1.SeriesPlaybackFirst), nil)
	assert.Equal(t, dp1.SeriesPlaybackFirst, got)
}

// Owner decision, 2026-10-30: content published before this field existed —
// or any series item that simply states nothing — keeps casting exactly as
// it does today (feral-file/ffos-user#421's always-expand-every-edition
// behavior), so the default must be full, not first.
func TestResolveSeriesPlaybackMode_DefaultsToFullWhenNeitherStatesAnything(t *testing.T) {
	got := dp1.ResolveSeriesPlaybackMode(nil, nil)
	assert.Equal(t, dp1.SeriesPlaybackFull, got)
}

func TestParseSeriesPlaybackMode_AcceptsTheThreeKnownValues(t *testing.T) {
	for _, s := range []string{"first", "full", "random"} {
		got, ok := dp1.ParseSeriesPlaybackMode(s)
		require.True(t, ok, s)
		assert.Equal(t, dp1.SeriesPlaybackMode(s), got)
	}
}

func TestParseSeriesPlaybackMode_RejectsAnUnrecognizedValue(t *testing.T) {
	_, ok := dp1.ParseSeriesPlaybackMode("shuffle")
	assert.False(t, ok)
}

func TestNarrowEditions_FullKeepsEveryEdition(t *testing.T) {
	editions := []dp1playlist.PlaylistItem{editionItem("a"), editionItem("b"), editionItem("c")}
	got := dp1.NarrowEditions(dp1.SeriesPlaybackFull, editions)
	assert.Len(t, got, 3)
}

func TestNarrowEditions_FirstKeepsOnlyTheRepresentative(t *testing.T) {
	editions := []dp1playlist.PlaylistItem{editionItem("a"), editionItem("b"), editionItem("c")}
	got := dp1.NarrowEditions(dp1.SeriesPlaybackFirst, editions)
	require.Len(t, got, 1)
	assert.Equal(t, "a", got[0].ID)
}

// FF1 has no re-roll-per-loop behavior (AC4 of feral-file/ffos-user#425):
// random has nothing to randomize at cast time and narrows to the same
// single edition first would.
func TestNarrowEditions_RandomResolvesToFirstAtCastTime(t *testing.T) {
	editions := []dp1playlist.PlaylistItem{editionItem("a"), editionItem("b")}
	got := dp1.NarrowEditions(dp1.SeriesPlaybackRandom, editions)
	require.Len(t, got, 1)
	assert.Equal(t, "a", got[0].ID)
}

// castPlanBody is the raw dp1_call bytes a static cast would have been
// decoded from — the ONE thing ResolveSeriesCastPlan reads seriesPlayback
// from that the typed dp1playlist.PlaylistItem has no field for.
func castPlanBody(items ...map[string]any) []byte {
	b, err := encjson.Marshal(map[string]any{
		"dpVersion": "1.0.0",
		"id":        "playlist-1",
		"items":     items,
	})
	if err != nil {
		panic(err)
	}
	return b
}

func seriesItemRaw(seriesPlayback string) map[string]any {
	m := map[string]any{"id": "item-series", "source": "https://cdn.artworks.feralfile.io/previews/scrampled/0.mp4"}
	if seriesPlayback != "" {
		m["seriesPlayback"] = seriesPlayback
	}
	return m
}

func TestResolveSeriesCastPlan_ReadsTheItemsOwnSeriesPlaybackFromRawBytes(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	raw := castPlanBody(seriesItemRaw("first"))
	serveEditions(ts, "playlist-1", "item-series", editionsBody(3, editionItem("a"), editionItem("b"), editionItem("c")))
	ts.mockJSON.EXPECT().Unmarshal(gomock.Any(), gomock.Any()).AnyTimes().
		DoAndReturn(func(data []byte, v interface{}) error { return encjson.Unmarshal(data, v) })

	playlist := dp1.Playlist{Playlist: dp1playlist.Playlist{ID: "playlist-1", Items: []dp1playlist.PlaylistItem{seriesItem()}}}
	plan := ts.client.ResolveSeriesCastPlan(ts.ctx, playlist, raw, nil, 0)

	require.True(t, plan.HasSeriesGroups())
	narrowed := plan.Narrow(nil)
	require.Len(t, narrowed, 1, "the item's own seriesPlayback:first must narrow the cast to the representative")
	assert.Equal(t, "a", narrowed[0].ID)
}

func TestResolveSeriesCastPlan_DeviceOverrideWinsOverTheDocument(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	raw := castPlanBody(seriesItemRaw("first"))
	serveEditions(ts, "playlist-1", "item-series", editionsBody(3, editionItem("a"), editionItem("b"), editionItem("c")))
	ts.mockJSON.EXPECT().Unmarshal(gomock.Any(), gomock.Any()).AnyTimes().
		DoAndReturn(func(data []byte, v interface{}) error { return encjson.Unmarshal(data, v) })

	playlist := dp1.Playlist{Playlist: dp1playlist.Playlist{ID: "playlist-1", Items: []dp1playlist.PlaylistItem{seriesItem()}}}
	full := mode(dp1.SeriesPlaybackFull)
	plan := ts.client.ResolveSeriesCastPlan(ts.ctx, playlist, raw, full, 0)

	narrowed := plan.Narrow(full)
	assert.Len(t, narrowed, 3, "a device override of full must win over the document's own first")
}

func TestResolveSeriesCastPlan_NoDocumentModeDefaultsToFull(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	raw := castPlanBody(seriesItemRaw(""))
	serveEditions(ts, "playlist-1", "item-series", editionsBody(2, editionItem("a"), editionItem("b")))
	ts.mockJSON.EXPECT().Unmarshal(gomock.Any(), gomock.Any()).AnyTimes().
		DoAndReturn(func(data []byte, v interface{}) error { return encjson.Unmarshal(data, v) })

	playlist := dp1.Playlist{Playlist: dp1playlist.Playlist{ID: "playlist-1", Items: []dp1playlist.PlaylistItem{seriesItem()}}}
	plan := ts.client.ResolveSeriesCastPlan(ts.ctx, playlist, raw, nil, 0)

	assert.Len(t, plan.Narrow(nil), 2)
}

// rawPlaylist may be nil: an internally rebuilt re-cast (setSeriesPlayback's
// re-narrow) has no wire form. Every item must then read as absent, falling
// through to the device override or the built-in default, not error out.
func TestResolveSeriesCastPlan_NilRawPlaylistFallsThroughToDeviceOrDefault(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	serveEditions(ts, "playlist-1", "item-series", editionsBody(2, editionItem("a"), editionItem("b")))

	playlist := dp1.Playlist{Playlist: dp1playlist.Playlist{ID: "playlist-1", Items: []dp1playlist.PlaylistItem{seriesItem()}}}
	plan := ts.client.ResolveSeriesCastPlan(ts.ctx, playlist, nil, nil, 0)

	assert.Len(t, plan.Narrow(nil), 2, "no raw bytes and no device override must fall through to the full default")
}

func TestResolveSeriesCastPlan_CatalogUnreachableKeepsTheRepresentative(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	ts.mockHTTP.EXPECT().Get(editionsURL("playlist-1", "item-series")).
		Return(nil, assert.AnError)

	playlist := dp1.Playlist{Playlist: dp1playlist.Playlist{ID: "playlist-1", Items: []dp1playlist.PlaylistItem{seriesItem()}}}
	plan := ts.client.ResolveSeriesCastPlan(ts.ctx, playlist, nil, nil, 0)

	assert.False(t, plan.HasSeriesGroups())
	narrowed := plan.Narrow(nil)
	require.Len(t, narrowed, 1)
	assert.Equal(t, "item-series", narrowed[0].ID)
	assert.Equal(t, dp1playlist.ProvenanceSeriesRegistry, narrowed[0].Provenance.Type)
}

func TestResolveSeriesCastPlan_LeavesAPlainPlaylistAlone(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	playlist := dp1.Playlist{Playlist: dp1playlist.Playlist{ID: "playlist-1", Items: []dp1playlist.PlaylistItem{editionItem("a"), editionItem("b")}}}
	plan := ts.client.ResolveSeriesCastPlan(ts.ctx, playlist, nil, nil, 0)

	assert.False(t, plan.HasSeriesGroups())
	narrowed := plan.Narrow(nil)
	require.Len(t, narrowed, 2)
	assert.Equal(t, "a", narrowed[0].ID)
	assert.Equal(t, "b", narrowed[1].ID)
}

// The full edition set is kept on the plan regardless of which mode it
// narrows to — AC5 of feral-file/ffos-user#425: a later device-setting
// change re-narrows the SAME plan without a second catalog call.
func TestResolveSeriesCastPlan_KeepsTheFullEditionSetForANetworkFreeRenarrow(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	raw := castPlanBody(seriesItemRaw("first"))
	// Fetched exactly ONCE: resolving the plan (narrowed to "first" below)
	// must not re-fetch when Narrow is called again with a different mode.
	serveEditions(ts, "playlist-1", "item-series", editionsBody(3, editionItem("a"), editionItem("b"), editionItem("c")))
	ts.mockJSON.EXPECT().Unmarshal(gomock.Any(), gomock.Any()).AnyTimes().
		DoAndReturn(func(data []byte, v interface{}) error { return encjson.Unmarshal(data, v) })

	playlist := dp1.Playlist{Playlist: dp1playlist.Playlist{ID: "playlist-1", Items: []dp1playlist.PlaylistItem{seriesItem()}}}
	plan := ts.client.ResolveSeriesCastPlan(ts.ctx, playlist, raw, nil, 0)

	require.Len(t, plan.Narrow(nil), 1, "the document's own first narrows the initial cast")
	full := mode(dp1.SeriesPlaybackFull)
	require.Len(t, plan.Narrow(full), 3, "re-narrowing the SAME plan under a new device mode needs no second catalog call")
}

// feralfile-bot's F1 evidence on ff-player#320 was exactly two different
// contracts sharing one numeric seriesId being collapsed together. AC8 of
// feral-file/ffos-user#425 requires grouping by chain+address+seriesId.
func TestResolveSeriesCastPlan_GroupsByChainAddressAndSeriesIdNotSeriesIdAlone(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	seriesID := 387
	itemA := dp1playlist.PlaylistItem{
		ID: "series-a", Source: "https://cdn/a.mp4",
		Provenance: &dp1playlist.ProvenanceBlock{
			Type:     dp1playlist.ProvenanceSeriesRegistry,
			Contract: &dp1playlist.ProvenanceContract{Chain: "evm", Address: "0xAAA", SeriesID: &seriesID},
		},
	}
	itemB := dp1playlist.PlaylistItem{
		ID: "series-b", Source: "https://cdn/b.mp4",
		Provenance: &dp1playlist.ProvenanceBlock{
			Type:     dp1playlist.ProvenanceSeriesRegistry,
			Contract: &dp1playlist.ProvenanceContract{Chain: "evm", Address: "0xBBB", SeriesID: &seriesID},
		},
	}
	ts.mockHTTP.EXPECT().Get(editionsURL("playlist-1", "series-a")).
		Return(nil, assert.AnError)
	ts.mockHTTP.EXPECT().Get(editionsURL("playlist-1", "series-b")).
		Return(nil, assert.AnError)

	playlist := dp1.Playlist{Playlist: dp1playlist.Playlist{ID: "playlist-1", Items: []dp1playlist.PlaylistItem{itemA, itemB}}}
	// Both unreachable, so this only pins that the two are addressed
	// (and would be cached) as DISTINCT groups despite the shared seriesId —
	// the catalog calls above are matched by item id, which is already
	// address-scoped per playlist item, and each must fire independently.
	ts.client.ResolveSeriesCastPlan(ts.ctx, playlist, nil, nil, 0)
}
