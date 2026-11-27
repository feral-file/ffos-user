package dp1_test

import (
	"encoding/json"
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

const (
	caseyReasRoot    = "1639e8c989cddd2c0f9e951b126a21b1964309854c0a8e0a9b1193a655c6712a"
	caseyReasHost    = "https://cy46rsmjzxosyd46sunre2rbwglegcmfjqfi4cu3cgj2mvogoeva.onchfs.invalid"
	caseyReasEVMHost = "https://cy46rsmjzxosyd46sunre2rbwglegcmfjqfi4cu3cgj2mvogoeva.evm.onchfs.invalid"
)

func TestOnchfsSource(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"indexer form keeps the seed", "onchfs://" + caseyReasRoot + "/?fxhash=0x06ec&fxiteration=1",
			caseyReasHost + "/?fxhash=0x06ec&fxiteration=1"},
		{"bare root", "onchfs://" + caseyReasRoot, caseyReasHost + "/"},
		{"dead fxhash gateway is read as the URI it came from", "https://onchfs.fxhash2.xyz/" + caseyReasRoot + "/?fxhash=0x06ec",
			caseyReasHost + "/?fxhash=0x06ec"},
		{"already device-local is unchanged", caseyReasHost + "/?fxhash=0x06ec", caseyReasHost + "/?fxhash=0x06ec"},
		{"ipfs is not ours", "https://ipfs.example/ipfs/Qm", "https://ipfs.example/ipfs/Qm"},
		{"malformed onchfs is left for the player to refuse", "onchfs://abc", "onchfs://abc"},
		{"a gateway path that is not a root is left alone", "https://onchfs.fxhash2.xyz/about", "https://onchfs.fxhash2.xyz/about"},
		{"look-alike gateway host", "https://onchfs.fxhash2.xyz.evil.example/" + caseyReasRoot, "https://onchfs.fxhash2.xyz.evil.example/" + caseyReasRoot},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, dp1.OnchfsSource(tc.in))
		})
	}
}

// A decoded document carries device-local sources, and re-marshaling it
// (the player echoes dp1_call back in player_status) keeps them: the mapping
// is idempotent.
func TestPlaylistDecodeMapsOnchfsSources(t *testing.T) {
	doc := `{"dpVersion":"1.0.0","id":"p","title":"t","items":[` +
		`{"id":"a","source":"onchfs://` + caseyReasRoot + `/?fxhash=1","duration":10,"license":"open"},` +
		`{"id":"b","source":"https://cdn.example/x.png","duration":10,"license":"open"}]}`
	var p dp1.Playlist
	require.NoError(t, json.Unmarshal([]byte(doc), &p))
	require.Len(t, p.Items, 2)
	assert.Equal(t, caseyReasHost+"/?fxhash=1", p.Items[0].Source)
	assert.Equal(t, "https://cdn.example/x.png", p.Items[1].Source)

	again, err := json.Marshal(p)
	require.NoError(t, err)
	var round dp1.Playlist
	require.NoError(t, json.Unmarshal(again, &round))
	assert.Equal(t, p.Items, round.Items)
}

func TestCastPlanNarrowMapsOnchfsSources(t *testing.T) {
	item := dp1playlist.PlaylistItem{ID: "a", Source: "onchfs://" + caseyReasRoot}
	plan := dp1.CastPlan{Segments: []dp1.CastSegment{{Item: &item}}}
	assert.Equal(t, caseyReasHost+"/", plan.Narrow(nil)[0].Source)
	assert.Equal(t, "onchfs://"+caseyReasRoot, item.Source, "the plan's own item is not written through")
}

// DP-1 hydration (a `dp1/*` dynamicQuery) hands the player the device-local
// source, not the raw URI: dp1-go decodes hydrated items without
// Playlist.UnmarshalJSON, so processDynamicPlaylist's own exit is what maps
// them. The provenance chain names the family (PR #461 review F1).
func TestDynamicHydration_OnchfsWorkLeavesWithADeviceLocalSource(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()
	body := `{"data":{"items":[{"id":"585f79b6-a45f-4c1c-8080-e93a192adccc","title":"w","duration":10,"license":"open",` +
		`"source":"onchfs://` + caseyReasRoot + `/?fxhash=0x06ec&fxiteration=1",` +
		`"provenance":{"type":"onChain","contract":{"chain":"evm","standard":"erc721","address":"0xabc","tokenId":"1"}}}]}}`
	ts.mockHTTP.EXPECT().Do(gomock.Any()).DoAndReturn(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}, nil
	}).Times(1)

	playlist := dp1.Playlist{Playlist: dp1playlist.Playlist{DynamicQuery: &playlists.DynamicQuery{
		Profile:         dp1playlist.ProfileGraphQLV1,
		Endpoint:        "https://example.com/graphql",
		Query:           `query { items(limit: {{limit}}, offset: {{offset}}) { id } }`,
		ResponseMapping: playlists.ResponseMapping{ItemsPath: "data.items", ItemSchema: "dp1/1.0"},
	}}}
	result, err := ts.client.ProcessDynamicPlaylist(t.Context(), playlist, false)
	require.NoError(t, err)
	require.Len(t, result.Items, 1)
	assert.Equal(t, caseyReasEVMHost+"/?fxhash=0x06ec&fxiteration=1", result.Items[0].Source)
}

// The chain family comes from the item's provenance; an explicit URI
// authority wins over it, and an item without provenance stays unlabeled.
func TestPlaylistDecodeTakesTheChainFamilyFromProvenance(t *testing.T) {
	item := func(source, chain string) string {
		prov := ""
		if chain != "" {
			prov = `,"provenance":{"type":"onChain","contract":{"chain":"` + chain + `","standard":"erc721","address":"0xabc","tokenId":"1"}}`
		}
		return `{"id":"a","source":"` + source + `","duration":10,"license":"open"` + prov + `}`
	}
	doc := `{"dpVersion":"1.0.0","id":"p","title":"t","items":[` +
		item("onchfs://"+caseyReasRoot, "evm") + `,` +
		item("onchfs://"+caseyReasRoot, "tezos") + `,` +
		item("onchfs://"+caseyReasRoot, "") + `,` +
		item("onchfs://eip155:8453/"+caseyReasRoot, "tezos") + `]}`
	var p dp1.Playlist
	require.NoError(t, json.Unmarshal([]byte(doc), &p))
	require.Len(t, p.Items, 4)
	assert.Equal(t, caseyReasEVMHost+"/", p.Items[0].Source)
	assert.Equal(t, "https://cy46rsmjzxosyd46sunre2rbwglegcmfjqfi4cu3cgj2mvogoeva.tezos.onchfs.invalid/", p.Items[1].Source)
	assert.Equal(t, caseyReasHost+"/", p.Items[2].Source)
	assert.Equal(t, "https://cy46rsmjzxosyd46sunre2rbwglegcmfjqfi4cu3cgj2mvogoeva.base.onchfs.invalid/", p.Items[3].Source, "the URI's own authority wins")
}

func TestOnchfsSourceVariantsAndSameWork(t *testing.T) {
	raw := "onchfs://" + caseyReasRoot + "/?fxhash=1"
	variants := dp1.OnchfsSourceVariants(raw)
	require.Len(t, variants, 5)
	assert.Equal(t, caseyReasHost+"/?fxhash=1", variants[0])
	assert.Contains(t, variants, caseyReasEVMHost+"/?fxhash=1")
	assert.Equal(t, []string{"https://cy46rsmjzxosyd46sunre2rbwglegcmfjqfi4cu3cgj2mvogoeva.base.onchfs.invalid/"},
		dp1.OnchfsSourceVariants("onchfs://eip155:8453/"+caseyReasRoot), "an explicit authority has one form")
	assert.Nil(t, dp1.OnchfsSourceVariants("https://cdn.example/x.png"))

	assert.True(t, dp1.SameOnchfsWork(caseyReasEVMHost+"/?fxhash=1", raw))
	assert.True(t, dp1.SameOnchfsWork(caseyReasEVMHost+"/?fxhash=1", caseyReasHost+"/?fxhash=1"))
	assert.False(t, dp1.SameOnchfsWork(caseyReasEVMHost+"/?fxhash=2", raw), "another iteration is another work")
	assert.False(t, dp1.SameOnchfsWork("https://cdn.example/x.png", "https://cdn.example/x.png"), "not onchfs")
}

// Catalog editions leave the plan mapped, not only Narrow's output: the
// refresher re-narrows the plan itself (narrowStable) and matches random
// picks by source, so a raw onchfs:// edition there reached the player on
// every 5-minute refresh (pre-staging review of #459, 2026-11-16).
func TestResolveSeriesCastPlan_MapsOnchfsEditionsInThePlan(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	a, b := editionItem("a"), editionItem("b")
	a.Source = "onchfs://" + caseyReasRoot + "/?fxhash=1"
	b.Source = "onchfs://" + caseyReasRoot + "/?fxhash=2"
	serveEditions(ts, "playlist-1", itemSeriesID, editionsBody(2, a, b))
	ts.mockJSON.EXPECT().Unmarshal(gomock.Any(), gomock.Any()).AnyTimes().
		DoAndReturn(func(data []byte, v interface{}) error { return json.Unmarshal(data, v) })

	playlist := dp1.Playlist{Playlist: dp1playlist.Playlist{ID: "playlist-1", Items: []dp1playlist.PlaylistItem{seriesItem()}}}
	plan := ts.client.ResolveSeriesCastPlan(ts.ctx, playlist, castPlanBody(seriesItemRaw("full")), nil, 0)

	require.True(t, plan.HasSeriesGroups())
	editions := plan.Segments[0].Group.Editions
	require.Len(t, editions, 2)
	for _, e := range editions {
		assert.Equal(t, caseyReasEVMHost+"/?fxhash="+e.Source[len(e.Source)-1:], e.Source,
			"the plan's own editions carry the device-local source")
	}
}
