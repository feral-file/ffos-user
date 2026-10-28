package dp1_test

import (
	"errors"
	"net/http"
	"testing"

	dp1playlist "github.com/display-protocol/dp1-go/playlist"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/golang/mock/gomock"

	"github.com/feral-file/ffos-user/components/feral-controld/dp1"
	ffindexer "github.com/feral-file/ffos-user/components/feral-controld/ff-indexer"
)

// A seriesRegistry item: core 1.1.0 §6's whole-series form, no tokenId and
// no contract address — a Feral File series whose editions sit under more
// than one contract cannot be named by a single address.
//
// Its `seriesId` is left off here, and not because an item lacks one. Feral
// File's V1–V3 series carry a 256-bit id as decimal digits, and the dp1-go
// this daemon builds against (v0.6.0) types the field `*int`, so such an
// item fails to parse before any of this runs:
//
//	json: cannot unmarshal string into Go struct field
//	ProvenanceContract.seriesId of type int
//
// display-protocol/dp1-go#12 makes the field accept either form. Until that
// release is pinned here, a playlist of those series never reaches this
// code. The expansion below does not read `seriesId` in any case — it keys
// on the item's slug — so the gap is in parsing, not in resolving.
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

func TestDP1_ExpandsASeriesIntoItsEditions(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	url := "http://example.com/exhibition.json"
	servePlaylist(ts, url, []dp1playlist.PlaylistItem{seriesItem()})

	// The slug is the only key an item and the index share: the index has no
	// column for a contract address plus a series id.
	ts.mockFFIndexer.EXPECT().
		QueryTokens(gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ interface{}, endpoint string, params map[string]string) ([]ffindexer.Token, error) {
			assert.Equal(t, "https://indexer-v2.feralfile.com/graphql", endpoint)
			assert.Equal(t, "feralfile", params["release_vendor"])
			assert.Equal(t, "scrampled-auw", params["release_vendor_slug"])
			return createMockTokens(), nil
		})

	result, err := ts.client.ProcessPlaylistURL(ts.ctx, url, false)

	require.NoError(t, err)
	require.Len(t, result.Items, 2)
	// Every item the player receives is now an edition it can play.
	for _, item := range result.Items {
		assert.Equal(t, dp1playlist.ProvenanceOnChain, item.Provenance.Type)
		assert.NotEmpty(t, item.Provenance.Contract.TokenID)
	}
}

func TestDP1_KeepsASeriesWhoseEditionsTheIndexCannotReturn(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	url := "http://example.com/exhibition.json"
	servePlaylist(ts, url, []dp1playlist.PlaylistItem{seriesItem()})

	ts.mockFFIndexer.EXPECT().
		QueryTokens(gomock.Any(), gomock.Any(), gomock.Any()).
		Return(nil, errors.New("index unreachable"))

	result, err := ts.client.ProcessPlaylistURL(ts.ctx, url, false)

	// The representative still plays. A screen that goes blank because an
	// index was down is the one outcome this must not produce.
	require.NoError(t, err)
	require.Len(t, result.Items, 1)
	assert.Equal(t, "item-series", result.Items[0].ID)
	assert.Equal(t, dp1playlist.ProvenanceSeriesRegistry, result.Items[0].Provenance.Type)
}

func TestDP1_KeepsASeriesTheIndexKnowsNothingAbout(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	url := "http://example.com/exhibition.json"
	servePlaylist(ts, url, []dp1playlist.PlaylistItem{seriesItem()})

	ts.mockFFIndexer.EXPECT().
		QueryTokens(gomock.Any(), gomock.Any(), gomock.Any()).
		Return([]ffindexer.Token{}, nil)

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

	// No index call at all: a playlist of editions has nothing to expand.
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

	ts.mockFFIndexer.EXPECT().
		QueryTokens(gomock.Any(), gomock.Any(), gomock.Any()).
		Return(createMockTokens(), nil)

	result, err := ts.client.ProcessPlaylistURL(ts.ctx, url, false)

	require.NoError(t, err)
	// The curated items keep their places around the editions.
	require.Len(t, result.Items, 4)
	assert.Equal(t, "before", result.Items[0].ID)
	assert.Equal(t, "after", result.Items[3].ID)
}

func TestDP1_SeriesWithNoSlugStaysAsItIs(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	url := "http://example.com/exhibition.json"
	item := seriesItem()
	item.Slug = ""
	servePlaylist(ts, url, []dp1playlist.PlaylistItem{item})

	// Nothing to key the index query by, so no call is made.
	result, err := ts.client.ProcessPlaylistURL(ts.ctx, url, false)

	require.NoError(t, err)
	require.Len(t, result.Items, 1)
	assert.Equal(t, dp1playlist.ProvenanceSeriesRegistry, result.Items[0].Provenance.Type)
}
