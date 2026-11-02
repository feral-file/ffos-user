package dp1_test

import (
	encjson "encoding/json"
	"fmt"
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
	got := dp1.NarrowEditions(dp1.SeriesPlaybackFull, editionItem("representative"), editions)
	assert.Len(t, got, 3)
}

// feralfile-bot F1, 2026-10-30: first must cast the DOCUMENT's own
// representative, not editions[0] — the catalog's ordering has no
// guarantee of matching what the item itself named as representative, so
// the representative here is deliberately absent from editions.
func TestNarrowEditions_FirstKeepsOnlyTheRepresentative(t *testing.T) {
	editions := []dp1playlist.PlaylistItem{editionItem("a"), editionItem("b"), editionItem("c")}
	got := dp1.NarrowEditions(dp1.SeriesPlaybackFirst, editionItem("representative"), editions)
	require.Len(t, got, 1)
	assert.Equal(t, "representative", got[0].ID)
}

// FF1 has no re-roll-per-loop behavior (AC4 of feral-file/ffos-user#425):
// random has nothing to randomize at cast time and narrows to the same
// single representative first would.
func TestNarrowEditions_RandomResolvesToFirstAtCastTime(t *testing.T) {
	editions := []dp1playlist.PlaylistItem{editionItem("a"), editionItem("b")}
	got := dp1.NarrowEditions(dp1.SeriesPlaybackRandom, editionItem("representative"), editions)
	require.Len(t, got, 1)
	assert.Equal(t, "representative", got[0].ID)
}

// feralfile-bot F4, 2026-10-30: first/random used to hand back representative
// completely unchanged — still carrying the document's own series marker
// (contract.seriesId, no tokenId) — so a cast built from that output still
// satisfied isSeriesItem and re-triggered catalog expansion on any later
// re-entry (setSeriesPlayback's re-cast, a recently-played replay). The
// caller's own representative — kept inside ResolvedSeriesGroup for a later
// re-narrow under a different mode — must come through unmutated.
func TestNarrowEditions_FirstClearsTheSeriesMarkerWithoutMutatingTheCallersRepresentative(t *testing.T) {
	representative := seriesItem()
	editions := []dp1playlist.PlaylistItem{editionItem("a")}

	got := dp1.NarrowEditions(dp1.SeriesPlaybackFirst, representative, editions)

	require.Len(t, got, 1)
	assert.Nil(t, got[0].Provenance.Contract.SeriesID, "the narrowed copy must no longer look like an unresolved series")
	require.NotNil(t, representative.Provenance.Contract.SeriesID, "the caller's own representative must be unaffected by narrowing a copy of it")
	assert.Equal(t, 387, *representative.Provenance.Contract.SeriesID)
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
	assert.Equal(t, "item-series", narrowed[0].ID, "first must cast the item's own representative, not the catalog's first-returned edition")
}

// feralfile-bot F4, 2026-10-30, end to end: recastForMode and
// CMD_PLAY_RECENTLY_PLAYED both rebuild a fresh dp1_call whose sole item IS
// a previous narrow's own output, then re-enter ResolveSeriesCastPlan on it.
// No mockHTTP expectation is registered for that second call below — if the
// narrowed representative still satisfied isSeriesItem, the unexpected
// catalog call would fail this test outright, exactly the regression
// clearing the series marker on the narrowed copy prevents.
func TestResolveSeriesCastPlan_ANarrowedRepresentativeNeverReExpands(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	raw := castPlanBody(seriesItemRaw("first"))
	serveEditions(ts, "playlist-1", "item-series", editionsBody(3, editionItem("a"), editionItem("b"), editionItem("c")))
	ts.mockJSON.EXPECT().Unmarshal(gomock.Any(), gomock.Any()).AnyTimes().
		DoAndReturn(func(data []byte, v interface{}) error { return encjson.Unmarshal(data, v) })

	playlist := dp1.Playlist{Playlist: dp1playlist.Playlist{ID: "playlist-1", Items: []dp1playlist.PlaylistItem{seriesItem()}}}
	plan := ts.client.ResolveSeriesCastPlan(ts.ctx, playlist, raw, nil, 0)
	narrowed := plan.Narrow(nil)
	require.Len(t, narrowed, 1, "the document's own first narrows the initial cast to the representative")

	rebuilt := dp1.Playlist{Playlist: dp1playlist.Playlist{ID: "playlist-1", Items: narrowed}}
	again := ts.client.ResolveSeriesCastPlan(ts.ctx, rebuilt, nil, nil, 0)

	assert.False(t, again.HasSeriesGroups(), "a narrowed representative must never satisfy isSeriesItem again")
	require.Len(t, again.Narrow(nil), 1, "and must cast through unchanged, not re-fetch and re-expand to full")
	assert.Equal(t, "item-series", again.Narrow(nil)[0].ID)
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

	// Wrapped as its own one-edition group now (feralfile-bot F5,
	// 2026-11-02; unresolvedSeriesGroup), not a bare Item, so the document's
	// mode survives into the cache — but it is NOT a "resolved" group
	// (Resolved stays false), so HasSeriesGroups must still read false: a
	// lone unresolved series is not worth tracking for a re-cast, and
	// treating it as tracked reintroduced a synchronous catalog retry on the
	// next setSeriesPlayback call (feralfile-bot F1 on this fix,
	// 2026-11-02). The marker is cleared unconditionally at construction
	// (feralfile-bot F1 pass 2 on this fix, 2026-11-02), not only for
	// first/random: there is only ever this one item to show, under any
	// mode, so there is nothing lost by never letting it look like an
	// unresolved series again.
	assert.False(t, plan.HasSeriesGroups())
	narrowed := plan.Narrow(nil)
	require.Len(t, narrowed, 1)
	assert.Equal(t, "item-series", narrowed[0].ID)
	assert.Nil(t, narrowed[0].Provenance.Contract.SeriesID, "full must not carry the series marker either — there is no catalog data behind this fallback to re-resolve from")
}

// feralfile-bot F5, 2026-11-02, end to end: an unresolved series item (the
// catalog failed) used to come through as a plain Item segment, still
// carrying the document's series marker. commandrouter's recastForMode
// rebuilds a fresh dp1_call from exactly this kind of Narrow output and
// re-enters Process with it — so a marker-carrying fallback there still
// satisfied isSeriesItem and was resolved a second time, and because that
// rebuild's JSON round trip has already dropped the document's own
// seriesPlayback (not a typed field), the second resolution silently
// defaulted to full instead of the document's actual first, re-fetching the
// catalog along the way. No mockHTTP expectation is registered for a second
// fetch of item-b below — an unexpected call there would fail this test
// outright, exactly the regression wrapping the fallback as its own group
// prevents.
func TestResolveSeriesCastPlan_AnUnresolvedRepresentativeNeverReExpandsEither(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	seriesIDA, seriesIDB := 387, 512
	itemA := dp1playlist.PlaylistItem{
		ID: "item-a", Source: "https://cdn/a.mp4",
		Provenance: &dp1playlist.ProvenanceBlock{
			Type:     dp1playlist.ProvenanceSeriesRegistry,
			Contract: &dp1playlist.ProvenanceContract{Chain: "evm", SeriesID: &seriesIDA},
		},
	}
	itemB := dp1playlist.PlaylistItem{
		ID: "item-b", Source: "https://cdn/b.mp4",
		Provenance: &dp1playlist.ProvenanceBlock{
			Type:     dp1playlist.ProvenanceSeriesRegistry,
			Contract: &dp1playlist.ProvenanceContract{Chain: "evm", SeriesID: &seriesIDB},
		},
	}
	raw := castPlanBody(
		map[string]any{"id": "item-a", "source": "https://cdn/a.mp4", "seriesPlayback": "first"},
		map[string]any{"id": "item-b", "source": "https://cdn/b.mp4", "seriesPlayback": "first"},
	)

	serveEditions(ts, "playlist-1", "item-a", editionsBody(3, editionItem("a1"), editionItem("a2"), editionItem("a3")))
	ts.mockHTTP.EXPECT().Get(editionsURL("playlist-1", "item-b")).
		Return(nil, assert.AnError)
	ts.mockJSON.EXPECT().Unmarshal(gomock.Any(), gomock.Any()).AnyTimes().
		DoAndReturn(func(data []byte, v interface{}) error { return encjson.Unmarshal(data, v) })

	playlist := dp1.Playlist{Playlist: dp1playlist.Playlist{ID: "playlist-1", Items: []dp1playlist.PlaylistItem{itemA, itemB}}}
	plan := ts.client.ResolveSeriesCastPlan(ts.ctx, playlist, raw, nil, 0)

	// A's genuine resolution alone makes the MIXED plan worth tracking —
	// unaffected by F1's fix, which only changes the LONE-unresolved case
	// (see TestResolveSeriesCastPlan_CatalogUnreachableKeepsTheRepresentative).
	require.True(t, plan.HasSeriesGroups())
	narrowed := plan.Narrow(nil)
	require.Len(t, narrowed, 2, "A's representative and B's still-unresolved representative must both survive the first narrow")
	assert.Equal(t, "item-a", narrowed[0].ID)
	assert.Equal(t, "item-b", narrowed[1].ID)

	// The internal rebuild: no raw bytes, exactly as recastForMode's re-entry
	// has none, and critically no second mockHTTP expectation for item-b.
	rebuilt := dp1.Playlist{Playlist: dp1playlist.Playlist{ID: "playlist-1", Items: narrowed}}
	again := ts.client.ResolveSeriesCastPlan(ts.ctx, rebuilt, nil, nil, 0)

	require.Len(t, again.Narrow(nil), 2, "must cast both through unchanged, not re-fetch and re-expand either")
	assert.Equal(t, "item-a", again.Narrow(nil)[0].ID)
	assert.Equal(t, "item-b", again.Narrow(nil)[1].ID)
}

// feralfile-bot F1 pass 2 on this fix, 2026-11-02: the sibling of the test
// above, under the (common, default) full mode instead of first. A, the
// genuinely resolved series, alone keeps this MIXED plan tracked
// (HasSeriesGroups true) — B's still-unresolved fallback rides along inside
// it. First/random already cleared B's marker (the test above); full did
// not, until unresolvedSeriesGroup started clearing it unconditionally. No
// mockHTTP expectation is registered for a second fetch of item-b below.
func TestResolveSeriesCastPlan_AnUnresolvedRepresentativeNeverReExpandsUnderFullEither(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	seriesIDA, seriesIDB := 387, 512
	itemA := dp1playlist.PlaylistItem{
		ID: "item-a", Source: "https://cdn/a.mp4",
		Provenance: &dp1playlist.ProvenanceBlock{
			Type:     dp1playlist.ProvenanceSeriesRegistry,
			Contract: &dp1playlist.ProvenanceContract{Chain: "evm", SeriesID: &seriesIDA},
		},
	}
	itemB := dp1playlist.PlaylistItem{
		ID: "item-b", Source: "https://cdn/b.mp4",
		Provenance: &dp1playlist.ProvenanceBlock{
			Type:     dp1playlist.ProvenanceSeriesRegistry,
			Contract: &dp1playlist.ProvenanceContract{Chain: "evm", SeriesID: &seriesIDB},
		},
	}

	serveEditions(ts, "playlist-1", "item-a", editionsBody(3, editionItem("a1"), editionItem("a2"), editionItem("a3")))
	ts.mockHTTP.EXPECT().Get(editionsURL("playlist-1", "item-b")).
		Return(nil, assert.AnError)

	playlist := dp1.Playlist{Playlist: dp1playlist.Playlist{ID: "playlist-1", Items: []dp1playlist.PlaylistItem{itemA, itemB}}}
	// No raw bytes: neither item states a mode, so both default to full.
	plan := ts.client.ResolveSeriesCastPlan(ts.ctx, playlist, nil, nil, 0)
	require.True(t, plan.HasSeriesGroups(), "A's genuine resolution alone keeps this mixed plan worth tracking")

	narrowed := plan.Narrow(nil)
	require.Len(t, narrowed, 4, "full keeps every one of A's 3 editions, plus B's single unresolved representative")
	assert.Nil(t, narrowed[3].Provenance.Contract.SeriesID, "B's fallback must not carry the series marker under full either — there is nothing else to show")

	rebuilt := dp1.Playlist{Playlist: dp1playlist.Playlist{ID: "playlist-1", Items: narrowed}}
	again := ts.client.ResolveSeriesCastPlan(ts.ctx, rebuilt, nil, nil, 0)

	assert.False(t, again.HasSeriesGroups(), "neither A's editions nor B's cleared fallback must look like a series again")
	require.Len(t, again.Narrow(nil), 4, "must cast all four through unchanged, not re-fetch and re-expand B")
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

// feralfile-bot F6, 2026-11-02: a plain playlist (no series items at all)
// was still handed the series-expansion item cap as its maxItems, so
// CastPlan.Narrow truncated it exactly like a series expansion would —
// silently dropping every item past CAST_PLAYLIST_ITEMS_LIMIT from content
// that never expanded anything to bound. The cap must apply only to a
// narrowed series expansion, never to an ordinary pass-through.
func TestResolveSeriesCastPlan_APlainPlaylistLongerThanTheCastCapIsNotTruncated(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	items := make([]dp1playlist.PlaylistItem, dp1.CAST_PLAYLIST_ITEMS_LIMIT+1)
	for i := range items {
		items[i] = editionItem(fmt.Sprintf("plain-%d", i))
	}
	playlist := dp1.Playlist{Playlist: dp1playlist.Playlist{ID: "playlist-1", Items: items}}

	// maxItems set exactly as the live-cast call site does, so this
	// reproduces the handler's real call shape rather than only the
	// zero-maxItems case TestResolveSeriesCastPlan_LeavesAPlainPlaylistAlone
	// already covers.
	plan := ts.client.ResolveSeriesCastPlan(ts.ctx, playlist, nil, nil, dp1.CAST_PLAYLIST_ITEMS_LIMIT)

	assert.False(t, plan.HasSeriesGroups())
	narrowed := plan.Narrow(nil)
	require.Len(t, narrowed, dp1.CAST_PLAYLIST_ITEMS_LIMIT+1, "no series was expanded, so nothing here is subject to the cast cap")
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

// feralfile-bot F3, 2026-10-30: a series whose initial resolution already
// fills the item-count cap by itself (full, 255 editions) used to make the
// resolve walk stop right there, permanently discarding the ordinary tail
// item it had not visited yet. A later re-narrow under a mode with room to
// spare (first) could then never recover it from the cache. The cap must
// apply only when flattening (Narrow), never while resolving the plan.
func TestResolveSeriesCastPlan_PreservesTrailingSegmentsForALaterNarrowerMode(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	editions := make([]dp1playlist.PlaylistItem, dp1.CAST_PLAYLIST_ITEMS_LIMIT)
	for i := range editions {
		editions[i] = editionItem(fmt.Sprintf("e%d", i))
	}
	serveEditions(ts, "playlist-1", "item-series", editionsBody(len(editions), editions...))
	ts.mockJSON.EXPECT().Unmarshal(gomock.Any(), gomock.Any()).AnyTimes().
		DoAndReturn(func(data []byte, v interface{}) error { return encjson.Unmarshal(data, v) })

	tail := editionItem("tail")
	playlist := dp1.Playlist{Playlist: dp1playlist.Playlist{
		ID:    "playlist-1",
		Items: []dp1playlist.PlaylistItem{seriesItem(), tail},
	}}

	// No raw bytes and no device override: the series item states nothing,
	// so ResolveSeriesPlaybackMode's own default (full) applies, and full
	// alone already reaches the cap — exactly the shape that used to cut
	// the walk short before it ever reached the tail item.
	plan := ts.client.ResolveSeriesCastPlan(ts.ctx, playlist, nil, nil, dp1.CAST_PLAYLIST_ITEMS_LIMIT)

	full := plan.Narrow(nil)
	require.Len(t, full, dp1.CAST_PLAYLIST_ITEMS_LIMIT, "full narrows to exactly the cap, leaving no room for the tail")

	first := mode(dp1.SeriesPlaybackFirst)
	narrowed := plan.Narrow(first)
	require.Len(t, narrowed, 2, "first leaves room for both the representative and the tail — the SAME plan must still carry it")
	assert.Equal(t, "item-series", narrowed[0].ID)
	assert.Equal(t, "tail", narrowed[1].ID)
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
