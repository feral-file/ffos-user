package dp1_test

import (
	encjson "encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/display-protocol/dp1-go/extension/playlists"
	dp1playlist "github.com/display-protocol/dp1-go/playlist"
	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/feral-file/ffos-user/components/feral-controld/dp1"
)

// servePlaylistFromBody wires the HTTP/IO/JSON mocks to hand back body,
// decoded by the real encoding/json (unlike series_test.go's servePlaylist,
// which fakes the decode by assigning Items directly) — so a document's own
// per-item seriesPlayback survives into the raw bytes exactly the way a live
// fetch would, which is the thing feral-file/ffos-user#452 fixes.
// AnyTimes: ResolveSeriesCastPlan's readItemSeriesPlaybackModes reads the
// same bytes a second time, through the same Unmarshal wrapper, only when
// the fetched playlist actually names a series (see its own doc).
func servePlaylistFromBody(ts *testSetup, url string, body string) {
	ts.mockHTTP.EXPECT().Get(url).
		Return(createMockResponse(http.StatusOK, body), nil)
	ts.mockIO.EXPECT().ReadAll(gomock.Any()).Return([]byte(body), nil)
	ts.mockJSON.EXPECT().Unmarshal([]byte(body), gomock.Any()).
		DoAndReturn(func(data []byte, v interface{}) error { return encjson.Unmarshal(data, v) }).
		AnyTimes()
}

// allowRawItemModeDecode lets resolveSeriesCastPlan's per-item
// seriesPlayback read (readItemSeriesPlaybackModes) decode each item's own
// raw JSON fragment for real, instead of panicking as an unexpected mock
// call. Call it AFTER servePlaylistFromBody and serveEditions, never
// before: both of those register an exact-byte match for their own call
// (the whole playlist body, the editions response), and gomock tries
// expectations in registration order — this catch-all must come last so it
// only ever picks up the per-item fragment neither of them matches, rather
// than stealing a call one of them expected to get.
func allowRawItemModeDecode(ts *testSetup) {
	ts.mockJSON.EXPECT().Unmarshal(gomock.Any(), gomock.Any()).
		DoAndReturn(func(data []byte, v interface{}) error { return encjson.Unmarshal(data, v) }).
		AnyTimes()
}

// multiEditionsBody is the catalog's own answer for a `multi` series (N
// editions, all identical copies of one artwork) — the exact shape that
// reproduced feral-file/ffos-user#452 in the field: the Prophecy series
// (aaajiao, contract 0xf4f1f0f18422be62e986b41c4c54c845c09df392, seriesId
// 336) answers with artworkModel "multi", and a playlistUrl display request played all
// 96 editions instead of narrowing to one.
func multiEditionsBody(playlistID string) string {
	return `{"items":[` +
		`{"id":"a","source":"https://example.com/a.mp4","provenance":{"type":"onChain","contract":{"chain":"evm","address":"0x28b51BA8B990c48CB22cB6EF0ad5415fdBA5210C","tokenId":"a"}}},` +
		`{"id":"b","source":"https://example.com/b.mp4","provenance":{"type":"onChain","contract":{"chain":"evm","address":"0x28b51BA8B990c48CB22cB6EF0ad5415fdBA5210C","tokenId":"b"}}}` +
		`],"limit":255,"offset":0,"playlistId":"` + playlistID + `","series":{"chain":"evm","seriesId":"387","artworkModel":"multi"},"total":2}`
}

// multiUniqueEditionsBody is the same shape for a `multi_unique` series
// (editions that genuinely differ), where full/first is a real choice.
func multiUniqueEditionsBody(playlistID string) string {
	return `{"items":[` +
		`{"id":"a","source":"https://example.com/a.mp4","provenance":{"type":"onChain","contract":{"chain":"evm","address":"0x28b51BA8B990c48CB22cB6EF0ad5415fdBA5210C","tokenId":"a"}}},` +
		`{"id":"b","source":"https://example.com/b.mp4","provenance":{"type":"onChain","contract":{"chain":"evm","address":"0x28b51BA8B990c48CB22cB6EF0ad5415fdBA5210C","tokenId":"b"}}}` +
		`],"limit":255,"offset":0,"playlistId":"` + playlistID + `","series":{"chain":"evm","seriesId":"387","artworkModel":"multi_unique"},"total":2}`
}

// TestDP1_ProcessPlaylistURLForCast_NarrowsMultiArtworkModelToRepresentative
// is the exact field reproduction of feral-file/ffos-user#452: displaying a
// playlist by its URL (ff-cli display <url>, the normal way a published
// exhibition is display) used to always call the OLD ExpandSeriesItems, which
// ignores artworkModel entirely and expanded every series to every catalog
// edition regardless of mode. A `multi` series must narrow to its
// representative via the SAME resolveSeriesCastPlan path a static dp1_call
// display already used.
func TestDP1_ProcessPlaylistURLForCast_NarrowsMultiArtworkModelToRepresentative(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	url := "https://feed.example/exhibition.json"
	body := `{"dpVersion":"1.1.0","id":"playlist-1","title":"t","items":[` +
		`{"id":"item-series","slug":"scrampled-auw","title":"Scrampled","source":"https://cdn.artworks.feralfile.io/previews/scrampled/0.mp4","provenance":{"type":"seriesRegistry","contract":{"chain":"evm","seriesId":"387"}}}` +
		`]}`
	servePlaylistFromBody(ts, url, body)
	serveEditions(ts, "playlist-1", "item-series", multiEditionsBody("playlist-1"))
	allowRawItemModeDecode(ts)

	result, plan, err := ts.client.ProcessPlaylistURLForCast(ts.ctx, url, nil)

	require.NoError(t, err)
	require.Len(t, result.Items, 1,
		"a multi series (identical editions) must narrow to its representative regardless of mode, not play every catalog edition")
	assert.Equal(t, "item-series", result.Items[0].ID)
	assert.True(t, plan.HasSeriesGroups(),
		"the resolved plan must be returned so a later setSeriesPlayback can re-narrow it without a second catalog call")
}

// TestDP1_ProcessPlaylistURLForCast_HonorsDocumentSeriesPlaybackFromFetchedBytes
// is the other half of #452: before this fix, a fetched document's own
// per-item seriesPlayback was never read at all (only a static dp1_call's
// verbatim bytes were). A multi_unique series — where full/first is a real
// choice — states "first" on itself and must narrow to one edition even
// though nothing device-level overrides it.
func TestDP1_ProcessPlaylistURLForCast_HonorsDocumentSeriesPlaybackFromFetchedBytes(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	url := "https://feed.example/exhibition-2.json"
	body := `{"dpVersion":"1.1.0","id":"playlist-2","title":"t","items":[` +
		`{"id":"item-series","slug":"scrampled-auw","title":"Scrampled","source":"https://cdn.artworks.feralfile.io/previews/scrampled/0.mp4","seriesPlayback":"first","provenance":{"type":"seriesRegistry","contract":{"chain":"evm","seriesId":"387"}}}` +
		`]}`
	servePlaylistFromBody(ts, url, body)
	serveEditions(ts, "playlist-2", "item-series", multiUniqueEditionsBody("playlist-2"))
	allowRawItemModeDecode(ts)

	result, _, err := ts.client.ProcessPlaylistURLForCast(ts.ctx, url, nil)

	require.NoError(t, err)
	require.Len(t, result.Items, 1,
		"the document's own seriesPlayback: first must be read from the FETCHED bytes, the same way a static dp1_call's verbatim bytes already were")
}

// manyEditionsBody builds a catalog `/editions` answer with n editions under
// the given artworkModel — more editions than the 50-item first cut the
// cast path once applied, so a reintroduced cut would show.
func manyEditionsBody(playlistID string, n int, artworkModel string) string {
	items := make([]string, n)
	for i := range items {
		items[i] = fmt.Sprintf(
			`{"id":"e%d","source":"https://example.com/%d.mp4","provenance":{"type":"onChain","contract":{"chain":"evm","address":"0x28b51BA8B990c48CB22cB6EF0ad5415fdBA5210C","tokenId":"%d"}}}`,
			i, i, i)
	}
	return fmt.Sprintf(
		`{"items":[%s],"limit":255,"offset":0,"playlistId":"%s","series":{"chain":"evm","seriesId":"387","artworkModel":"%s"},"total":%d}`,
		strings.Join(items, ","), playlistID, artworkModel, n)
}

// TestDP1_ProcessDynamicPlaylistForCast_ShowsEveryEditionAndThePlanMatches
// keeps run-reviewer's F1 on PR #454 round 2 (a later setSeriesPlayback
// re-narrow of the cached plan must not grow past what the display could
// show) now that the display carries the whole resolved list: the initial
// display shows every edition up to CAST_PLAYLIST_ITEMS_LIMIT, not a first
// 50 (feral-file/feral-file#3636), and a re-narrow in the same mode gives
// back exactly that list.
func TestDP1_ProcessDynamicPlaylistForCast_ShowsEveryEditionAndThePlanMatches(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	const editionCount = 80 // past the old 50-item cut

	ts.mockHTTP.EXPECT().
		Do(gomock.Any()).
		DoAndReturn(func(req *http.Request) (*http.Response, error) {
			assertGraphQLHydration(t, req, "255", "0")
			body := `{"data":{"items":[` +
				`{"id":"fdb2a98f-9ca0-4a76-ae1f-cd8e2f8d4caf","source":"https://cdn.artworks.feralfile.io/0.mp4","provenance":{"type":"seriesRegistry","contract":{"chain":"evm","seriesId":387}}}` +
				`]}}`
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}, nil
		}).
		Times(1)
	serveEditions(ts, "playlist-dyn-1", "fdb2a98f-9ca0-4a76-ae1f-cd8e2f8d4caf", manyEditionsBody("playlist-dyn-1", editionCount, "multi_unique"))
	allowRawItemModeDecode(ts)

	playlist := dp1.Playlist{
		Playlist: dp1playlist.Playlist{
			ID: "playlist-dyn-1",
			DynamicQuery: &playlists.DynamicQuery{
				Profile:  dp1playlist.ProfileGraphQLV1,
				Endpoint: "https://example.com/graphql",
				Query:    `query { items(limit: {{limit}}, offset: {{offset}}) { id source provenance } }`,
				ResponseMapping: playlists.ResponseMapping{
					ItemsPath:  "data.items",
					ItemSchema: "dp1/1.0",
				},
			},
		},
	}

	result, plan, err := ts.client.ProcessDynamicPlaylistForCast(ts.ctx, playlist, nil)
	require.NoError(t, err)
	require.Len(t, result.Items, editionCount, "the initial display request carries every edition, not a first-50 cut")

	renarrowed := plan.Narrow(nil)
	assert.Len(t, renarrowed, editionCount, "a re-narrow in the same mode gives back what the display showed")
}

// TestDP1_ProcessDynamicPlaylistForCast_CachedPlanStaysBoundedWhenAlreadyShort
// keeps feralfile-bot's F2 on PR #454: re-narrowing a plan cached from a
// display that started short (`first`, one representative) to `full` must
// show what a direct `full` display of the same source would show, never
// depend on which mode started the display. A direct `full` display carries
// every edition up to CAST_PLAYLIST_ITEMS_LIMIT (feral-file/feral-file#3636),
// so the re-narrow does the same.
func TestDP1_ProcessDynamicPlaylistForCast_CachedPlanStaysBoundedWhenAlreadyShort(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	const editionCount = 80 // past the old 50-item cut

	ts.mockHTTP.EXPECT().
		Do(gomock.Any()).
		DoAndReturn(func(req *http.Request) (*http.Response, error) {
			assertGraphQLHydration(t, req, "255", "0")
			body := `{"data":{"items":[` +
				`{"id":"fdb2a98f-9ca0-4a76-ae1f-cd8e2f8d4caf","source":"https://cdn.artworks.feralfile.io/0.mp4","provenance":{"type":"seriesRegistry","contract":{"chain":"evm","seriesId":387}}}` +
				`]}}`
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}, nil
		}).
		Times(1)
	serveEditions(ts, "playlist-dyn-2", "fdb2a98f-9ca0-4a76-ae1f-cd8e2f8d4caf", manyEditionsBody("playlist-dyn-2", editionCount, "multi_unique"))
	allowRawItemModeDecode(ts)

	playlist := dp1.Playlist{
		Playlist: dp1playlist.Playlist{
			ID: "playlist-dyn-2",
			DynamicQuery: &playlists.DynamicQuery{
				Profile:  dp1playlist.ProfileGraphQLV1,
				Endpoint: "https://example.com/graphql",
				Query:    `query { items(limit: {{limit}}, offset: {{offset}}) { id source provenance } }`,
				ResponseMapping: playlists.ResponseMapping{
					ItemsPath:  "data.items",
					ItemSchema: "dp1/1.0",
				},
			},
		},
	}

	first := dp1.SeriesPlaybackFirst
	result, plan, err := ts.client.ProcessDynamicPlaylistForCast(ts.ctx, playlist, &first)
	require.NoError(t, err)
	require.Len(t, result.Items, 1, "device mode first must narrow to the single representative")

	renarrowed := plan.Narrow(dp1Mode(dp1.SeriesPlaybackFull))
	assert.Len(t, renarrowed, editionCount,
		"a re-narrow to full shows what a direct full display shows")
	assert.LessOrEqual(t, len(renarrowed), dp1.CAST_PLAYLIST_ITEMS_LIMIT, "and never past the display request cap")
}

// dp1Mode is a one-line helper so the full-mode re-narrow above can take a
// literal without a throwaway local variable.
func dp1Mode(m dp1.SeriesPlaybackMode) *dp1.SeriesPlaybackMode { return &m }

// TestDP1_ProcessPlaylistURLForCast_FullMultiUniquePlaysEveryEdition pins the
// other side of the same mode: a multi_unique series with no seriesPlayback
// and no device override still display requests every catalog edition under the
// default `full` mode — unchanged from #425/#446, now reachable from a URL
// display too.
func TestDP1_ProcessPlaylistURLForCast_FullMultiUniquePlaysEveryEdition(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	url := "https://feed.example/exhibition-3.json"
	body := `{"dpVersion":"1.1.0","id":"playlist-3","title":"t","items":[` +
		`{"id":"item-series","slug":"scrampled-auw","title":"Scrampled","source":"https://cdn.artworks.feralfile.io/previews/scrampled/0.mp4","provenance":{"type":"seriesRegistry","contract":{"chain":"evm","seriesId":"387"}}}` +
		`]}`
	servePlaylistFromBody(ts, url, body)
	serveEditions(ts, "playlist-3", "item-series", multiUniqueEditionsBody("playlist-3"))
	allowRawItemModeDecode(ts)

	result, _, err := ts.client.ProcessPlaylistURLForCast(ts.ctx, url, nil)

	require.NoError(t, err)
	require.Len(t, result.Items, 2, "multi_unique under the default full mode still display requests every catalog edition")
}
