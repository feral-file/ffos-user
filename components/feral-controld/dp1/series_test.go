package dp1_test

import (
	"context"
	encjson "encoding/json"
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/display-protocol/dp1-go/extension/contentrating"
	dp1playlist "github.com/display-protocol/dp1-go/playlist"
	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/feral-file/ffos-user/components/feral-controld/dp1"
)

// A seriesRegistry item: core 1.1.0 §6's whole-series form, no tokenId and
// no contract address — a Feral File series whose editions sit under more
// than one contract cannot be named by a single address, and after the
// registry landed the item names the registry alone (Sang, 2026-10-28).
//
// It states a `seriesId`, which an earlier version of this fixture left off
// on the grounds that the route is addressed by the playlist's id and the
// item's. That was wrong about the shape: §6 gives `seriesRegistry` two forms
// and `seriesId` present with `tokenId` absent is what makes this one the
// series rather than one named edition of it, so an item without it is not a
// series at all (feralfile-bot F2, 2026-10-29). The catalog reads the value
// itself, from its own copy of the same item.
func seriesItem() dp1playlist.PlaylistItem {
	seriesID := 387
	return dp1playlist.PlaylistItem{
		ID:     "item-series",
		Slug:   "scrampled-auw",
		Title:  "Scrampled",
		Source: "https://cdn.artworks.feralfile.io/previews/scrampled/0.mp4",
		Provenance: &dp1playlist.ProvenanceBlock{
			Type: dp1playlist.ProvenanceSeriesRegistry,
			Contract: &dp1playlist.ProvenanceContract{
				Chain:    "evm",
				SeriesID: &seriesID,
			},
		},
	}
}

func editionItem(id string) dp1playlist.PlaylistItem {
	return dp1playlist.PlaylistItem{
		ID:     id,
		Source: "https://example.com/" + id + ".mp4",
		Provenance: &dp1playlist.ProvenanceBlock{
			Type: dp1playlist.ProvenanceOnChain,
			Contract: &dp1playlist.ProvenanceContract{
				Chain:   "evm",
				Address: "0x28b51BA8B990c48CB22cB6EF0ad5415fdBA5210C",
				TokenID: id,
			},
		},
	}
}

// servePlaylist wires the HTTP/IO/JSON mocks to hand back one playlist.
func servePlaylist(ts *testSetup, url string, items []dp1playlist.PlaylistItem) {
	body := `{"dpVersion":"1.0.0"}`
	ts.mockHTTP.EXPECT().Get(url).
		Return(createMockResponse(http.StatusOK, body), nil)
	ts.mockIO.EXPECT().ReadAll(gomock.Any()).Return([]byte(body), nil)
	ts.mockJSON.EXPECT().Unmarshal([]byte(body), gomock.Any()).
		DoAndReturn(func(_ []byte, v interface{}) error {
			playlist := v.(*dp1.Playlist)
			playlist.ID = "playlist-1"
			playlist.Items = items
			return nil
		})
}

// editionsURL is the route the catalog addresses one series item by.
func editionsURL(playlistID, itemID string) string {
	return fmt.Sprintf(
		"https://catalog.feralfile.io/api/v1/playlists/%s/items/%s/editions?limit=255",
		playlistID, itemID)
}

// serveEditions answers that route with the catalog's own envelope, decoded
// by the real encoding/json so the struct tags are under test too.
func serveEditions(ts *testSetup, playlistID, itemID string, body string) {
	ts.mockHTTP.EXPECT().Get(editionsURL(playlistID, itemID)).
		Return(createMockResponse(http.StatusOK, body), nil)
	ts.mockIO.EXPECT().ReadAll(gomock.Any()).Return([]byte(body), nil)
	ts.mockJSON.EXPECT().Unmarshal([]byte(body), gomock.Any()).
		DoAndReturn(func(data []byte, v interface{}) error { return encjson.Unmarshal(data, v) })
}

// editionsBody is the shape the catalog actually returns, as measured:
// {revisionId, playlistId, itemId, series, total, limit, offset, items}.
// Only `total` and `items` are read here; the rest is carried so a change
// to the envelope that drops them is caught.
func editionsBody(total int, items ...dp1playlist.PlaylistItem) string {
	envelope := map[string]any{
		"revisionId": "editions_e4c3a49b8bf7f92c7b2697b58234f03c47c51868199d359e1bb7fd94ca8a0ba8",
		"playlistId": "playlist-1",
		"itemId":     "item-series",
		"series":     map[string]any{"chain": "evm", "seriesId": "387"},
		"total":      total,
		"limit":      255,
		"offset":     0,
		"items":      items,
	}
	encoded, err := encjson.Marshal(envelope)
	if err != nil {
		panic(err)
	}
	return string(encoded)
}

func TestDP1_ExpandsASeriesIntoItsEditionsFromTheCatalog(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	url := "http://example.com/exhibition.json"
	servePlaylist(ts, url, []dp1playlist.PlaylistItem{seriesItem()})
	serveEditions(ts, "playlist-1", "item-series",
		editionsBody(2, editionItem("a"), editionItem("b")))

	result, err := ts.client.ProcessPlaylistURL(ts.ctx, url, false)

	require.NoError(t, err)
	require.Len(t, result.Items, 2)
	// Every item the player receives is now an edition it can play.
	for _, item := range result.Items {
		assert.Equal(t, dp1playlist.ProvenanceOnChain, item.Provenance.Type)
		assert.NotEmpty(t, item.Provenance.Contract.TokenID)
	}
}

func TestDP1_KeepsASeriesWhenTheCatalogIsUnreachable(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	url := "http://example.com/exhibition.json"
	servePlaylist(ts, url, []dp1playlist.PlaylistItem{seriesItem()})
	ts.mockHTTP.EXPECT().Get(editionsURL("playlist-1", "item-series")).
		Return(nil, errors.New("catalog unreachable"))

	result, err := ts.client.ProcessPlaylistURL(ts.ctx, url, false)

	// The representative still plays. A screen that goes blank because the
	// catalog was down is the one outcome this must not produce.
	require.NoError(t, err)
	require.Len(t, result.Items, 1)
	assert.Equal(t, "item-series", result.Items[0].ID)
	assert.Equal(t, dp1playlist.ProvenanceSeriesRegistry, result.Items[0].Provenance.Type)
}

func TestDP1_KeepsASeriesThePlaylistTheCatalogDoesNotHold(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	url := "http://example.com/exhibition.json"
	servePlaylist(ts, url, []dp1playlist.PlaylistItem{seriesItem()})
	// A cast of something never published, or an older channel revision:
	// the catalog 404s and the body is never read.
	ts.mockHTTP.EXPECT().Get(editionsURL("playlist-1", "item-series")).
		Return(createMockResponse(http.StatusNotFound, `{"status":404}`), nil)

	result, err := ts.client.ProcessPlaylistURL(ts.ctx, url, false)

	require.NoError(t, err)
	require.Len(t, result.Items, 1)
	assert.Equal(t, "item-series", result.Items[0].ID)
}

func TestDP1_KeepsASeriesTheCatalogReturnsNoEditionsFor(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	url := "http://example.com/exhibition.json"
	servePlaylist(ts, url, []dp1playlist.PlaylistItem{seriesItem()})
	serveEditions(ts, "playlist-1", "item-series", editionsBody(0))

	result, err := ts.client.ProcessPlaylistURL(ts.ctx, url, false)

	require.NoError(t, err)
	require.Len(t, result.Items, 1)
	assert.Equal(t, "item-series", result.Items[0].ID)
}

func TestDP1_LeavesEditionItemsAlone(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	url := "http://example.com/exhibition.json"
	servePlaylist(ts, url, []dp1playlist.PlaylistItem{
		editionItem("a"),
		editionItem("b"),
	})

	// No catalog call at all: a playlist of editions has nothing to expand.
	result, err := ts.client.ProcessPlaylistURL(ts.ctx, url, false)

	require.NoError(t, err)
	require.Len(t, result.Items, 2)
	assert.Equal(t, []string{"a", "b"}, []string{result.Items[0].ID, result.Items[1].ID})
}

func TestDP1_ExpandsOnlyTheSeriesInAMixedPlaylist(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	url := "http://example.com/exhibition.json"
	servePlaylist(ts, url, []dp1playlist.PlaylistItem{
		editionItem("before"),
		seriesItem(),
		editionItem("after"),
	})
	serveEditions(ts, "playlist-1", "item-series",
		editionsBody(2, editionItem("a"), editionItem("b")))

	result, err := ts.client.ProcessPlaylistURL(ts.ctx, url, false)

	require.NoError(t, err)
	// The curated items keep their places around the editions.
	require.Len(t, result.Items, 4)
	assert.Equal(t, "before", result.Items[0].ID)
	assert.Equal(t, "after", result.Items[3].ID)
}

func TestDP1_SeriesWithNoItemIDStaysAsItIs(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	url := "http://example.com/exhibition.json"
	item := seriesItem()
	item.ID = ""
	servePlaylist(ts, url, []dp1playlist.PlaylistItem{item})

	// Nothing to address the catalog with, so no call is made.
	result, err := ts.client.ProcessPlaylistURL(ts.ctx, url, false)

	require.NoError(t, err)
	require.Len(t, result.Items, 1)
	assert.Equal(t, dp1playlist.ProvenanceSeriesRegistry, result.Items[0].Provenance.Type)
}

// TestDP1_SeriesStaysUnexpandedWhenThePlaylistHasNoID pins a known,
// documented degrade case round 2 of run-reviewer found undocumented: an
// inline dp1_call playlist — the shape docs/controld-inbound-controller-
// messages.md's own "Inline DP1" and "Dynamic DP1" examples show, and
// which DP-1 core's own schema does not require an "id" for — gives
// seriesEditions nothing to address the catalog by, so a seriesRegistry
// item arriving that way silently keeps its representative rather than
// expanding. On the device this is indistinguishable from "catalog
// unreachable" (see the PR's Out of scope line for the accepted
// limitation this test pins as deliberate rather than silently
// discovered).
func TestDP1_SeriesStaysUnexpandedWhenThePlaylistHasNoID(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	url := "http://example.com/exhibition.json"
	body := `{"dpVersion":"1.0.0"}`
	ts.mockHTTP.EXPECT().Get(url).Return(createMockResponse(http.StatusOK, body), nil)
	ts.mockIO.EXPECT().ReadAll(gomock.Any()).Return([]byte(body), nil)
	ts.mockJSON.EXPECT().Unmarshal([]byte(body), gomock.Any()).
		DoAndReturn(func(_ []byte, v interface{}) error {
			playlist := v.(*dp1.Playlist)
			// Deliberately left unset: this is the shape an inline
			// dp1_call payload actually arrives in, unlike servePlaylist's
			// helper default which always assigns one.
			playlist.Items = []dp1playlist.PlaylistItem{seriesItem()}
			return nil
		})

	// No catalog call is made: seriesEditions refuses to address the
	// catalog with an empty playlist id, so there is nothing to mock —
	// a stray *ts.mockHTTP* expectation for the editions route would make
	// this test fail with "missing call(s)" if the guard ever stopped
	// holding.
	result, err := ts.client.ProcessPlaylistURL(ts.ctx, url, false)

	require.NoError(t, err)
	require.Len(t, result.Items, 1)
	assert.Equal(t, dp1playlist.ProvenanceSeriesRegistry, result.Items[0].Provenance.Type)
}

// An edition of a V1–V3 series carries a 256-bit `seriesId` as decimal
// digits, which is what the catalog returns. The dp1-go this daemon builds
// against (v0.6.0) types the field `*int`, so decoding that answer fails:
//
//	json: cannot unmarshal string into Go struct field
//	ProvenanceContract.seriesId of type int
//
// display-protocol/dp1-go#12 makes the field accept either form. Until that
// release is pinned here, `full` playback of those series is unreachable —
// and this test is what says so, rather than the behavior looking like a
// catalog fault. When #12 lands this test flips to asserting the expansion.
func TestDP1_A256BitSeriesIDOnAnEditionIsNotYetReadable(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	url := "http://example.com/exhibition.json"
	servePlaylist(ts, url, []dp1playlist.PlaylistItem{seriesItem()})

	const bigSeriesID = "8520038953287439433536834341059117372268365319568716555346026339118809853347"
	body := fmt.Sprintf(`{"total":1,"items":[{"id":"a","source":"https://example.com/a.mp4",`+
		`"provenance":{"type":"onChain","contract":{"chain":"evm","standard":"erc721",`+
		`"address":"0xe59B7D60d7d7183E09d8ABb6DF8B20EB4CF66632","tokenId":%[1]q,"seriesId":%[1]q}}}]}`,
		bigSeriesID)
	ts.mockHTTP.EXPECT().Get(editionsURL("playlist-1", "item-series")).
		Return(createMockResponse(http.StatusOK, body), nil)
	ts.mockIO.EXPECT().ReadAll(gomock.Any()).Return([]byte(body), nil)
	ts.mockJSON.EXPECT().Unmarshal([]byte(body), gomock.Any()).
		DoAndReturn(func(data []byte, v interface{}) error { return encjson.Unmarshal(data, v) })

	result, err := ts.client.ProcessPlaylistURL(ts.ctx, url, false)

	// The decode fails, so the series keeps its representative rather than
	// the screen losing the work.
	require.NoError(t, err)
	require.Len(t, result.Items, 1)
	assert.Equal(t, "item-series", result.Items[0].ID)
}

// TestDP1_CastCapHoldsAcrossPlainAndExpandedAppends is the regression test
// for the first run-reviewer round's finding: the cast cap
// (CAST_PLAYLIST_ITEMS_LIMIT, only passed as maxItems by
// ProcessPlaylistURLForCast) was checked after a successful series
// expansion's append, but not after a plain item's or a kept-unexpanded
// representative's — so a series followed by enough plain items overran the
// limit. The series here expands to 2 editions first (well under the cap),
// then 300 plain items follow one at a time; before the fix every one of
// those 300 appends skipped the check.
func TestDP1_CastCapHoldsAcrossPlainAndExpandedAppends(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	url := "http://example.com/exhibition.json"
	items := []dp1playlist.PlaylistItem{seriesItem()}
	for i := 0; i < 300; i++ {
		items = append(items, editionItem(fmt.Sprintf("plain-%d", i)))
	}
	servePlaylist(ts, url, items)
	serveEditions(ts, "playlist-1", "item-series",
		editionsBody(2, editionItem("a"), editionItem("b")))

	result, err := ts.client.ProcessPlaylistURLForCast(ts.ctx, url)

	require.NoError(t, err)
	assert.Len(t, result.Items, dp1.CAST_PLAYLIST_ITEMS_LIMIT)
}

// TestDP1_CastCapHoldsWhenASeriesKeepsItsRepresentative is the same
// regression, for the other previously-unchecked branch: a series that
// fails to expand keeps its single representative item rather than a
// batch of editions, and that append must be capped too.
func TestDP1_CastCapHoldsWhenASeriesKeepsItsRepresentative(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	url := "http://example.com/exhibition.json"
	items := []dp1playlist.PlaylistItem{seriesItem()}
	for i := 0; i < 300; i++ {
		items = append(items, editionItem(fmt.Sprintf("plain-%d", i)))
	}
	servePlaylist(ts, url, items)
	ts.mockHTTP.EXPECT().Get(editionsURL("playlist-1", "item-series")).
		Return(nil, errors.New("catalog unreachable"))

	result, err := ts.client.ProcessPlaylistURLForCast(ts.ctx, url)

	require.NoError(t, err)
	assert.Len(t, result.Items, dp1.CAST_PLAYLIST_ITEMS_LIMIT)
}

func TestDP1_TheCatalogEndpointIsHTTPSAndAllowlisted(t *testing.T) {
	// The base URL is a constant, not something a playlist supplies, but the
	// guard is what keeps it that way: the answer decides what the screen
	// plays, so the set of servers that may give it stays closed.
	assert.True(t, dp1.CATALOG_HOSTS["catalog.feralfile.io"])
	assert.True(t, dp1.CATALOG_HOSTS["art-catalog-service.bitmark-development.workers.dev"])
	assert.False(t, dp1.CATALOG_HOSTS["catalog.feralfile.io.attacker.example"])
	assert.False(t, dp1.CATALOG_HOSTS["catalog.feralfile.io:8788"])
}

// An edition replaces the series item in the playlist, so it has to carry the
// fields that belong to THIS playlist instance rather than to the series: the
// catalog merged each edition with its own copy of the item, which can predate
// a schedule or a rating the document in hand carries.
//
// All three fields are pinned together because they are one mechanism, not
// three bugs — every consumer that reads them runs AFTER expansion
// (commandrouter's scheduled-cast scan, playlistschedule's timer,
// contentpolicy's filter), and `contentRating` is the one whose loss fails
// open: a mature series would expand into unrated editions.
func TestDP1_ExpandedEditionsInheritTheSeriesItemsInstanceFields(t *testing.T) {
	const displayAt = "2026-11-01T18:00:00Z"
	mature := contentrating.Rating(contentrating.RatingMature)
	reasons := []string{"violence"}

	tests := []struct {
		name   string
		series func() dp1playlist.PlaylistItem
		verify func(t *testing.T, edition dp1playlist.PlaylistItem)
	}{
		{
			name: "displayAt keeps a scheduled series recognizable as scheduled",
			series: func() dp1playlist.PlaylistItem {
				item := seriesItem()
				at := displayAt
				item.DisplayAt = &at
				return item
			},
			verify: func(t *testing.T, edition dp1playlist.PlaylistItem) {
				require.NotNil(t, edition.DisplayAt)
				assert.Equal(t, displayAt, *edition.DisplayAt)
			},
		},
		{
			name: "contentRating keeps a mature series mature after expansion",
			series: func() dp1playlist.PlaylistItem {
				item := seriesItem()
				rating := mature
				item.ContentRating = &rating
				item.ContentReasons = &reasons
				return item
			},
			verify: func(t *testing.T, edition dp1playlist.PlaylistItem) {
				require.NotNil(t, edition.ContentRating)
				assert.Equal(t, mature, *edition.ContentRating)
				require.NotNil(t, edition.ContentReasons)
				assert.Equal(t, reasons, *edition.ContentReasons)
			},
		},
		{
			name:   "a series item stating nothing leaves the edition as the catalog sent it",
			series: seriesItem,
			verify: func(t *testing.T, edition dp1playlist.PlaylistItem) {
				assert.Nil(t, edition.DisplayAt)
				assert.Nil(t, edition.ContentRating)
				assert.Nil(t, edition.ContentReasons)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts := setup(t)
			defer ts.teardown()

			url := "http://example.com/exhibition.json"
			servePlaylist(ts, url, []dp1playlist.PlaylistItem{tt.series()})
			serveEditions(ts, "playlist-1", "item-series",
				editionsBody(2, editionItem("a"), editionItem("b")))

			result, err := ts.client.ProcessPlaylistURL(ts.ctx, url, false)

			require.NoError(t, err)
			require.Len(t, result.Items, 2)
			// Every edition, not just the first: the scan that reads these
			// fields stops at the first item that carries one.
			for _, edition := range result.Items {
				tt.verify(t, edition)
			}
		})
	}
}

// The propagated value is a copy per edition, so a caller that rewrites one
// edition's schedule cannot silently move its siblings'.
func TestDP1_EachExpandedEditionOwnsItsCopyOfTheInstanceFields(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	item := seriesItem()
	at := "2026-11-01T18:00:00Z"
	item.DisplayAt = &at

	url := "http://example.com/exhibition.json"
	servePlaylist(ts, url, []dp1playlist.PlaylistItem{item})
	serveEditions(ts, "playlist-1", "item-series",
		editionsBody(2, editionItem("a"), editionItem("b")))

	result, err := ts.client.ProcessPlaylistURL(ts.ctx, url, false)

	require.NoError(t, err)
	require.Len(t, result.Items, 2)
	require.NotNil(t, result.Items[0].DisplayAt)
	require.NotNil(t, result.Items[1].DisplayAt)
	assert.NotSame(t, result.Items[0].DisplayAt, result.Items[1].DisplayAt)
}

// labeledEditionItem is editionItem with its own series id, which is the shape
// a real catalog answer has: the catalog passes each edition's own provenance
// through wholesale, so an edition whose on-chain provenance names its series
// arrives carrying `seriesId`. editionItem() omits it, so any test built only
// on that helper cannot see this at all.
func labeledEditionItem(id string, seriesID int) dp1playlist.PlaylistItem {
	item := editionItem(id)
	item.Provenance.Contract.SeriesID = &seriesID
	return item
}

// What the daemon does today with an edition that arrives already labeled with
// its series id: nothing. It is passed through untouched.
//
// This is recorded rather than asserted as desirable, because it is the open
// question the 2026-10-29 review round stopped on. ff-player#320 groups a run
// by this key and narrows it using the device override alone, which is `auto`
// (read as `first`) on an unconfigured device — so the run this test produces
// can play as a single edition there instead of the whole series. Whether the
// daemon should strip the field, or the player should default an expanded run
// to `full`, is the owner's call; see the note on seriesEditions.
//
// The test exists so that call is made against the real shape. An earlier
// version of it used editionItem(), which never sets SeriesID, and so passed
// no matter what the code did.
func TestDP1_AnEditionsOwnSeriesIDSurvivesExpansion(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	const seriesID = 387

	url := "http://example.com/exhibition.json"
	servePlaylist(ts, url, []dp1playlist.PlaylistItem{seriesItem()})
	serveEditions(ts, "playlist-1", "item-series",
		editionsBody(2, labeledEditionItem("a", seriesID), labeledEditionItem("b", seriesID)))

	result, err := ts.client.ProcessPlaylistURL(ts.ctx, url, false)

	require.NoError(t, err)
	require.Len(t, result.Items, 2)
	// Both editions, so the run really does share one key — that sameness is
	// what the player groups on.
	for _, edition := range result.Items {
		require.NotNil(t, edition.Provenance.Contract.SeriesID)
		assert.Equal(t, seriesID, *edition.Provenance.Contract.SeriesID)
	}
}

// Only the whole-series form of `seriesRegistry` provenance is a series. The
// same type carrying a `tokenId` names one specific edition (core 1.1.0 §6),
// and an item with no `seriesId` names nothing the catalog can resolve —
// neither is expanded, and neither costs a catalog round trip.
//
// The cost is why this is a defect rather than a tidiness point: expansion
// fetches one series at a time, so a playlist of explicitly-named editions
// used to serialize one doomed request per item in front of playback, each
// answered `item-not-a-series` (feralfile-bot F2, 2026-10-29). No editions
// route is mocked below, so any attempt to call it fails the test.
func TestDP1_OnlyTheWholeSeriesFormIsExpanded(t *testing.T) {
	tests := []struct {
		name string
		item func() dp1playlist.PlaylistItem
	}{
		{
			name: "seriesRegistry with a tokenId names one edition, not the series",
			item: func() dp1playlist.PlaylistItem {
				item := seriesItem()
				item.Provenance.Contract.TokenID = "42"
				return item
			},
		},
		{
			name: "seriesRegistry with no seriesId has no series to resolve",
			item: func() dp1playlist.PlaylistItem {
				item := seriesItem()
				item.Provenance.Contract.SeriesID = nil
				return item
			},
		},
		{
			name: "seriesRegistry with no contract block at all",
			item: func() dp1playlist.PlaylistItem {
				item := seriesItem()
				item.Provenance.Contract = nil
				return item
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts := setup(t)
			defer ts.teardown()

			url := "http://example.com/exhibition.json"
			servePlaylist(ts, url, []dp1playlist.PlaylistItem{tt.item()})

			result, err := ts.client.ProcessPlaylistURL(ts.ctx, url, false)

			require.NoError(t, err)
			require.Len(t, result.Items, 1)
			assert.Equal(t, "item-series", result.Items[0].ID)
		})
	}
}

// durationOf is a *float64 for a literal, which the item struct needs.
func durationOf(seconds float64) *float64 {
	return &seconds
}

// editionWithDuration is an edition as the catalog returns it: the duration it
// carries was inherited from the catalog's OWN copy of the series item, not
// stated by the edition, which is why every edition of a series shows the same
// one.
func editionWithDuration(id string, seconds float64) dp1playlist.PlaylistItem {
	item := editionItem(id)
	item.Duration = durationOf(seconds)
	return item
}

// The catalog's merge of duration, license, display and override stands as it
// arrived, even when the series item in hand states something else.
//
// This is the limitation the expansion accepts, and it is pinned rather than
// merely documented because the branch tried the other reading twice. The
// catalog merges each edition on top of ITS stored copy of the series item, so
// those four fields come back as that copy stated them — and an inline
// dp1_call payload, or a playlist edited after the catalog ingested it, can
// carry values the copy never had (feralfile-bot F3). Re-basing them onto the
// item in hand needs to know which editions stated a value for themselves, and
// the /editions answer does not say; inferring it from how many editions share
// a value overwrote deliberate per-edition overrides instead (feralfile-bot
// F4 — see TestDP1_EditionsThatShareAnOverrideKeepIt). So the daemon leaves
// them alone: the editions play the catalog's settings until its next ingest,
// the same staleness an unreachable catalog degrades to.
func TestDP1_PlaybackSettingsStandAsTheCatalogMergedThem(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	item := seriesItem()
	item.Duration = durationOf(99)
	item.License = "open"
	item.Display = &dp1playlist.DisplayPrefs{Scaling: "fit", Background: "#000000"}
	item.Override = encjson.RawMessage(`{"ff":{"hint":"instance"}}`)

	url := "http://example.com/exhibition.json"
	servePlaylist(ts, url, []dp1playlist.PlaylistItem{item})
	catalogCopy := func(id string) dp1playlist.PlaylistItem {
		edition := editionWithDuration(id, 5)
		edition.License = "token"
		edition.Display = &dp1playlist.DisplayPrefs{Scaling: "cover"}
		edition.Override = encjson.RawMessage(`{"ff":{"hint":"catalog"}}`)
		return edition
	}
	serveEditions(ts, "playlist-1", "item-series",
		editionsBody(2, catalogCopy("a"), catalogCopy("b")))

	result, err := ts.client.ProcessPlaylistURL(ts.ctx, url, false)

	require.NoError(t, err)
	require.Len(t, result.Items, 2)
	for _, edition := range result.Items {
		require.NotNil(t, edition.Duration)
		assert.Equal(t, float64(5), *edition.Duration)
		assert.Equal(t, "token", edition.License)
		require.NotNil(t, edition.Display)
		assert.Equal(t, "cover", edition.Display.Scaling)
		assert.JSONEq(t, `{"ff":{"hint":"catalog"}}`, string(edition.Override))
	}
	// Still per edition, not shared: DisplayPrefs is a struct of pointers, so
	// two editions sharing one would let an edit to one edition's render
	// preferences reach its siblings.
	assert.NotSame(t, result.Items[0].Display, result.Items[1].Display)
	assert.NotSame(t, result.Items[0].Duration, result.Items[1].Duration)
}

// An edition that states a playback setting for ITSELF keeps it, whatever the
// series item in hand says — the same rule as the test above, read from the
// other side: nothing is re-based, so nothing can overwrite a per-edition
// override. Pinned separately because this is the direction a future re-base
// would break first.
func TestDP1_AnEditionsOwnPlaybackSettingSurvivesExpansion(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	item := seriesItem()
	item.Duration = durationOf(99)

	url := "http://example.com/exhibition.json"
	servePlaylist(ts, url, []dp1playlist.PlaylistItem{item})
	serveEditions(ts, "playlist-1", "item-series",
		editionsBody(2, editionWithDuration("a", 5), editionWithDuration("b", 7)))

	result, err := ts.client.ProcessPlaylistURL(ts.ctx, url, false)

	require.NoError(t, err)
	require.Len(t, result.Items, 2)
	require.NotNil(t, result.Items[0].Duration)
	require.NotNil(t, result.Items[1].Duration)
	assert.Equal(t, float64(5), *result.Items[0].Duration)
	assert.Equal(t, float64(7), *result.Items[1].Duration)
}

// A series item that states no playback setting leaves the editions exactly as
// the catalog sent them — as does one that states them, above; this pins the
// common case, where the catalog's copy of the series item IS the item in hand
// and the two readings cannot be told apart.
func TestDP1_ASeriesItemStatingNoPlaybackSettingsLeavesTheEditionsAlone(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	url := "http://example.com/exhibition.json"
	servePlaylist(ts, url, []dp1playlist.PlaylistItem{seriesItem()})
	serveEditions(ts, "playlist-1", "item-series",
		editionsBody(2, editionWithDuration("a", 5), editionWithDuration("b", 5)))

	result, err := ts.client.ProcessPlaylistURL(ts.ctx, url, false)

	require.NoError(t, err)
	require.Len(t, result.Items, 2)
	for _, edition := range result.Items {
		require.NotNil(t, edition.Duration)
		assert.Equal(t, float64(5), *edition.Duration)
	}
}

// A series whose editions genuinely differ — the `multi_unique` shape — comes
// through edition by edition, with no edition's value moved onto another's.
//
// This set is what two rounds of re-basing rules disagreed about: an
// all-or-nothing agreement check left all three on the catalog's values, a
// plurality check moved the two that matched onto the item in hand. Neither
// could tell which of the three stated a duration for itself, and the answer
// does not say, so all three are now left as they arrived (run-reviewer round
// 1 F1, feralfile-bot F4).
func TestDP1_EditionsThatDifferAreLeftAsTheyArrived(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	item := seriesItem()
	item.Duration = durationOf(99)

	url := "http://example.com/exhibition.json"
	servePlaylist(ts, url, []dp1playlist.PlaylistItem{item})
	// Two editions carry 5 and one carries 7. Which of those the catalog
	// supplied and which an edition stated is exactly what the answer does
	// not say.
	serveEditions(ts, "playlist-1", "item-series", editionsBody(3,
		editionWithDuration("a", 5),
		editionWithDuration("b", 7),
		editionWithDuration("c", 5)))

	result, err := ts.client.ProcessPlaylistURL(ts.ctx, url, false)

	require.NoError(t, err)
	require.Len(t, result.Items, 3)
	durations := make([]float64, 0, 3)
	for _, edition := range result.Items {
		require.NotNil(t, edition.Duration)
		durations = append(durations, *edition.Duration)
	}
	assert.Equal(t, []float64{5, 7, 5}, durations)
}

// Editions that share an override keep it, even when they are most of the
// answer and the series item in hand states something else.
//
// The catalog gives an edition the series' value unless the edition states its
// own, and a curator applying one override to most of a series is ordinary
// curation — so "most editions carry this value" is not evidence that the
// catalog supplied it. Reading it as evidence overwrote exactly those editions
// with the series item's value: series duration 5, two editions stating 20 and
// one carrying 5, expanded to 5/5/5, which silently shortens playback
// (feralfile-bot F4, 2026-10-29, reproduced against the real catalog merger).
//
// All four fields that merge covers are asserted, not only the duration the
// finding named, because one rule read them all.
func TestDP1_EditionsThatShareAnOverrideKeepIt(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	// The series item in hand and the catalog's copy of it agree: 5 is the
	// series' duration, and the two editions below departed from it.
	item := seriesItem()
	item.Duration = durationOf(5)
	item.License = "token"
	item.Display = &dp1playlist.DisplayPrefs{Scaling: "cover"}
	item.Override = encjson.RawMessage(`{"ff":{"hint":"series"}}`)

	stated := func(id string) dp1playlist.PlaylistItem {
		edition := editionWithDuration(id, 20)
		edition.License = "open"
		edition.Display = &dp1playlist.DisplayPrefs{Scaling: "fit"}
		edition.Override = encjson.RawMessage(`{"ff":{"hint":"edition"}}`)
		return edition
	}
	inherited := func(id string) dp1playlist.PlaylistItem {
		edition := editionWithDuration(id, 5)
		edition.License = "token"
		edition.Display = &dp1playlist.DisplayPrefs{Scaling: "cover"}
		edition.Override = encjson.RawMessage(`{"ff":{"hint":"series"}}`)
		return edition
	}

	url := "http://example.com/exhibition.json"
	servePlaylist(ts, url, []dp1playlist.PlaylistItem{item})
	serveEditions(ts, "playlist-1", "item-series",
		editionsBody(3, stated("a"), stated("b"), inherited("c")))

	result, err := ts.client.ProcessPlaylistURL(ts.ctx, url, false)

	require.NoError(t, err)
	require.Len(t, result.Items, 3)
	for i, edition := range result.Items {
		want := map[bool]struct {
			duration float64
			license  string
			scaling  string
			hint     string
		}{
			true:  {20, "open", "fit", "edition"},
			false: {5, "token", "cover", "series"},
		}[i < 2]
		require.NotNil(t, edition.Duration)
		assert.Equal(t, want.duration, *edition.Duration)
		assert.Equal(t, want.license, edition.License)
		require.NotNil(t, edition.Display)
		assert.Equal(t, want.scaling, edition.Display.Scaling)
		assert.JSONEq(t, `{"ff":{"hint":"`+want.hint+`"}}`, string(edition.Override))
	}
}

// A schedule the catalog still holds but the document in hand no longer states
// is cleared, not carried.
//
// The catalog answers from a snapshot of the series item taken at its last
// ingest, so a curator who removes a series' `displayAt` keeps getting editions
// that carry the old timestamp until the next ingest. Unlike a content rating,
// a stale schedule has no safe direction to fail in: it makes the cast
// preflight read an unscheduled playlist as scheduled and skip the dead-link
// probe, and it arms a Daily timer that delays a cast nothing intends to delay
// (run-reviewer round 1, F2).
func TestDP1_AStaleCatalogScheduleDoesNotSurviveExpansion(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	staleSchedule := func(id string) dp1playlist.PlaylistItem {
		edition := editionItem(id)
		at := "2026-11-01T18:00:00Z"
		edition.DisplayAt = &at
		return edition
	}

	url := "http://example.com/exhibition.json"
	// The series item in hand states no schedule at all.
	servePlaylist(ts, url, []dp1playlist.PlaylistItem{seriesItem()})
	serveEditions(ts, "playlist-1", "item-series",
		editionsBody(2, staleSchedule("a"), staleSchedule("b")))

	result, err := ts.client.ProcessPlaylistURL(ts.ctx, url, false)

	require.NoError(t, err)
	require.Len(t, result.Items, 2)
	for _, edition := range result.Items {
		assert.Nil(t, edition.DisplayAt)
	}
}

// A content rating is the opposite case, and deliberately so: it is only ever
// re-applied, never cleared. Clearing is the permissive direction — a `mature`
// series whose rating reached the catalog but not the copy in hand has to stay
// mature, or contentpolicy.FilterLocked stops refusing it.
func TestDP1_ACatalogContentRatingSurvivesASeriesItemThatStatesNone(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	rated := func(id string) dp1playlist.PlaylistItem {
		edition := editionItem(id)
		rating := contentrating.Rating(contentrating.RatingMature)
		edition.ContentRating = &rating
		return edition
	}

	url := "http://example.com/exhibition.json"
	servePlaylist(ts, url, []dp1playlist.PlaylistItem{seriesItem()})
	serveEditions(ts, "playlist-1", "item-series",
		editionsBody(2, rated("a"), rated("b")))

	result, err := ts.client.ProcessPlaylistURL(ts.ctx, url, false)

	require.NoError(t, err)
	require.Len(t, result.Items, 2)
	for _, edition := range result.Items {
		require.NotNil(t, edition.ContentRating)
		assert.Equal(t, contentrating.Rating(contentrating.RatingMature), *edition.ContentRating)
	}
}

// The walk spends at most maxSeriesPerPlaylist catalog calls, whatever
// maxItems says.
//
// maxItems bounds the RESULT, and is 0 — unbounded — on four of
// expandSeriesItems' six call sites, CMD_DOWNLOAD_PLAYLIST's among them. So a
// document carrying more series items than a conforming playlist could ever
// show used to buy one sequential 30s-timeout catalog round trip per item,
// with nothing to stop it (run-reviewer round 2, F1). The 300 items below are
// served through the uncapped ProcessPlaylistURL; the mock allows exactly 255
// editions requests and gomock fails the test on the 256th.
func TestDP1_TheExpansionWalkSpendsABoundedNumberOfCatalogCalls(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	// One item id for all of them, so the 255 permitted calls share one URL
	// and one expectation can count them.
	items := make([]dp1playlist.PlaylistItem, 0, 300)
	for i := 0; i < 300; i++ {
		items = append(items, seriesItem())
	}

	url := "http://example.com/exhibition.json"
	servePlaylist(ts, url, items)

	// Every permitted call answers "no editions", so each series keeps its
	// representative and the result length is not what bounds the walk.
	empty := editionsBody(0)
	ts.mockHTTP.EXPECT().Get(editionsURL("playlist-1", "item-series")).
		Return(createMockResponse(http.StatusOK, empty), nil).Times(255)
	ts.mockIO.EXPECT().ReadAll(gomock.Any()).Return([]byte(empty), nil).Times(255)
	ts.mockJSON.EXPECT().Unmarshal([]byte(empty), gomock.Any()).
		DoAndReturn(func(data []byte, v interface{}) error { return encjson.Unmarshal(data, v) }).
		Times(255)

	result, err := ts.client.ProcessPlaylistURL(ts.ctx, url, false)

	require.NoError(t, err)
	// Nothing is dropped: the items past the budget keep their representative,
	// exactly as an unreachable catalog leaves them.
	require.Len(t, result.Items, 300)
	for _, item := range result.Items {
		assert.Equal(t, dp1playlist.ProvenanceSeriesRegistry, item.Provenance.Type)
	}
}

// A caller whose context is already done buys no catalog calls at all: there
// is no one left waiting for the answer. No editions route is mocked, so any
// attempt fails the test.
func TestDP1_ADoneContextStopsTheExpansionWalk(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	ctx, cancel := context.WithCancel(ts.ctx)
	cancel()

	url := "http://example.com/exhibition.json"
	servePlaylist(ts, url, []dp1playlist.PlaylistItem{seriesItem()})

	result, err := ts.client.ProcessPlaylistURL(ctx, url, false)

	require.NoError(t, err)
	require.Len(t, result.Items, 1)
	assert.Equal(t, "item-series", result.Items[0].ID)
}

// Expansion may tighten an edition's rating, never loosen it. A `general`
// series can legitimately contain one `mature` edition — the catalog's merger
// preserves each edition's own rating (EDITION_OWN_FIELDS) — and copying the
// series item's looser rating over it downgraded the edition, after which the
// default content policy ADMITTED a mature work on a device configured to hide
// it (feralfile-bot F5, 2026-10-29).
//
// The table covers the comparison, not that one pair: the sibling shape is an
// edition carrying a label this build does not know, which contentpolicy reads
// as UNRATED and the operator's blockUnratedCurated gate withholds — so a
// series item's `general` must not overwrite that either.
func TestDP1_ExpansionNeverLoosensAnEditionsContentRating(t *testing.T) {
	general := contentrating.Rating(contentrating.RatingGeneral)
	mature := contentrating.Rating(contentrating.RatingMature)
	unknown := contentrating.Rating("explicit")

	tests := []struct {
		name          string
		seriesRating  *contentrating.Rating
		editionRating *contentrating.Rating
		want          contentrating.Rating
		wantReasons   []string
	}{
		{
			name:          "a mature edition under a general series stays mature",
			seriesRating:  &general,
			editionRating: &mature,
			want:          mature,
			// The edition's own reason survives with the rating it
			// justifies; the series item's would describe the `general` the
			// edition did not keep.
			wantReasons: []string{"edition-violence"},
		},
		{
			name:          "a rating this build cannot read is not overwritten by general either",
			seriesRating:  &general,
			editionRating: &unknown,
			want:          unknown,
			wantReasons:   []string{"edition-violence"},
		},
		{
			name:          "a mature series still tightens a general edition",
			seriesRating:  &mature,
			editionRating: &general,
			want:          mature,
			wantReasons:   []string{"series-violence"},
		},
		{
			name:          "an equal rating stays the item in hand's, reasons included",
			seriesRating:  &mature,
			editionRating: &mature,
			want:          mature,
			wantReasons:   []string{"series-violence"},
		},
		{
			name:          "an edition that states no rating inherits the series item's",
			seriesRating:  &general,
			editionRating: nil,
			want:          general,
			wantReasons:   []string{"series-violence"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts := setup(t)
			defer ts.teardown()

			item := seriesItem()
			item.ContentRating = tt.seriesRating
			seriesReasons := []string{"series-violence"}
			item.ContentReasons = &seriesReasons

			edition := func(id string) dp1playlist.PlaylistItem {
				e := editionItem(id)
				e.ContentRating = tt.editionRating
				if tt.editionRating != nil {
					editionReasons := []string{"edition-violence"}
					e.ContentReasons = &editionReasons
				}
				return e
			}

			url := "http://example.com/exhibition.json"
			servePlaylist(ts, url, []dp1playlist.PlaylistItem{item})
			serveEditions(ts, "playlist-1", "item-series",
				editionsBody(2, edition("a"), edition("b")))

			result, err := ts.client.ProcessPlaylistURL(ts.ctx, url, false)

			require.NoError(t, err)
			require.Len(t, result.Items, 2)
			for _, got := range result.Items {
				require.NotNil(t, got.ContentRating)
				assert.Equal(t, tt.want, *got.ContentRating)
				require.NotNil(t, got.ContentReasons)
				assert.Equal(t, tt.wantReasons, *got.ContentReasons)
			}
		})
	}
}

// Reasons travel with the rating that was applied, cleared first and then
// re-applied the way DisplayAt is — so no edition is left carrying a
// justification written for a rating it does not have.
//
// DP-1's content-rating extension does not require `contentRating` and
// `contentReasons` to be stated together, so both halves below are reachable
// shapes rather than hypotheticals. Writing the reasons only when the series
// item stated some left each one open (run-reviewer round 5, F1): the tests
// above never exercise them, because they always set the two fields together.
func TestDP1_ExpandedEditionReasonsFollowTheRatingThatWasApplied(t *testing.T) {
	general := contentrating.Rating(contentrating.RatingGeneral)
	mature := contentrating.Rating(contentrating.RatingMature)

	tests := []struct {
		name        string
		series      func(dp1playlist.PlaylistItem) dp1playlist.PlaylistItem
		edition     func(dp1playlist.PlaylistItem) dp1playlist.PlaylistItem
		wantRating  contentrating.Rating
		wantReasons *[]string
	}{
		{
			name: "a tightening series item with no reasons clears the edition's own",
			series: func(item dp1playlist.PlaylistItem) dp1playlist.PlaylistItem {
				rating := mature
				item.ContentRating = &rating
				return item
			},
			edition: func(e dp1playlist.PlaylistItem) dp1playlist.PlaylistItem {
				rating := general
				reasons := []string{"written-for-general"}
				e.ContentRating = &rating
				e.ContentReasons = &reasons
				return e
			},
			// The edition is mature now; "written-for-general" justified the
			// rating it no longer carries, so it must not survive.
			wantRating:  mature,
			wantReasons: nil,
		},
		{
			name: "reasons on a series item that states no rating do not reach the edition",
			series: func(item dp1playlist.PlaylistItem) dp1playlist.PlaylistItem {
				reasons := []string{"series-reason"}
				item.ContentReasons = &reasons
				return item
			},
			edition: func(e dp1playlist.PlaylistItem) dp1playlist.PlaylistItem {
				rating := mature
				reasons := []string{"edition-reason"}
				e.ContentRating = &rating
				e.ContentReasons = &reasons
				return e
			},
			// The edition's own rating survives, so its own reasons are the
			// ones that describe it; the series item has no rating of its own
			// for "series-reason" to justify.
			wantRating:  mature,
			wantReasons: &[]string{"edition-reason"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts := setup(t)
			defer ts.teardown()

			url := "http://example.com/exhibition.json"
			servePlaylist(ts, url, []dp1playlist.PlaylistItem{tt.series(seriesItem())})
			serveEditions(ts, "playlist-1", "item-series",
				editionsBody(2, tt.edition(editionItem("a")), tt.edition(editionItem("b"))))

			result, err := ts.client.ProcessPlaylistURL(ts.ctx, url, false)

			require.NoError(t, err)
			require.Len(t, result.Items, 2)
			for _, got := range result.Items {
				require.NotNil(t, got.ContentRating)
				assert.Equal(t, tt.wantRating, *got.ContentRating)
				if tt.wantReasons == nil {
					assert.Nil(t, got.ContentReasons)
					continue
				}
				require.NotNil(t, got.ContentReasons)
				assert.Equal(t, *tt.wantReasons, *got.ContentReasons)
			}
		})
	}
}
