package dp1_test

import (
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
// Its `seriesId` is left off here, and that is faithful rather than
// convenient: the expansion keys on the playlist's id and the item's, which
// is what the catalog addresses a series by, and never reads `seriesId`.
// The catalog reads it, from its own copy of the same item.
func seriesItem() dp1playlist.PlaylistItem {
	return dp1playlist.PlaylistItem{
		ID:     "item-series",
		Slug:   "scrampled-auw",
		Title:  "Scrampled",
		Source: "https://cdn.artworks.feralfile.io/previews/scrampled/0.mp4",
		Provenance: &dp1playlist.ProvenanceBlock{
			Type: dp1playlist.ProvenanceSeriesRegistry,
			Contract: &dp1playlist.ProvenanceContract{
				Chain: "evm",
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
