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
	// digitalSoul is Han Saglam's Digital Soul: the return of artpiece() on
	// Ethereum (feral-file#3599). The host is base32 of the contract's 20
	// bytes, then the chain id.
	digitalSoulContract = "0x4f32740cf412d647a622ba27c36fa4b76f71601f"
	digitalSoulHost     = "https://j4zhidhucllepjrcxit4g35ew5xxcya7.1.web3.invalid"
)

func TestWeb3Source(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"method only", "web3://" + digitalSoulContract + ":1/artpiece", digitalSoulHost + "/artpiece"},
		{"an omitted chain id is chain 1", "web3://" + digitalSoulContract + "/artpiece", digitalSoulHost + "/artpiece"},
		{"the w3 alias is the same work", "w3://" + digitalSoulContract + ":1/artpiece", digitalSoulHost + "/artpiece"},
		{"the query survives", "web3://" + digitalSoulContract + ":1/artpiece?seed=0x06ec", digitalSoulHost + "/artpiece?seed=0x06ec"},
		{"already device-local is unchanged", digitalSoulHost + "/artpiece", digitalSoulHost + "/artpiece"},
		{"a name is not a contract the device reads", "web3://example.eth/artpiece", "web3://example.eth/artpiece"},
		{"malformed web3 is left for the player to refuse", "web3://0xabc/artpiece", "web3://0xabc/artpiece"},
		{"https is not ours", "https://cdn.example/x.html", "https://cdn.example/x.html"},
		{"onchfs is the other scheme's", "onchfs://" + caseyReasRoot, caseyReasHost + "/"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// DeviceLocalSource is the seam every caller outside dp1 uses;
			// it must agree with the scheme-specific function.
			assert.Equal(t, tc.want, dp1.DeviceLocalSource(tc.in))
		})
	}
}

// Every exit an item can take out of this package carries the device-local
// source, so the player, the offline-cache key, media health and the replay
// scope see one identity per work however it was resolved.
func TestPlaylistDecodeMapsWeb3Sources(t *testing.T) {
	doc := `{"dpVersion":"1.0.0","id":"p","title":"t","items":[` +
		`{"id":"a","source":"web3://` + digitalSoulContract + `:1/artpiece","duration":10,"license":"open"},` +
		`{"id":"b","source":"onchfs://` + caseyReasRoot + `","duration":10,"license":"open"},` +
		`{"id":"c","source":"https://cdn.example/x.png","duration":10,"license":"open"}]}`
	var p dp1.Playlist
	require.NoError(t, json.Unmarshal([]byte(doc), &p))
	require.Len(t, p.Items, 3)
	assert.Equal(t, digitalSoulHost+"/artpiece", p.Items[0].Source)
	assert.Equal(t, caseyReasHost+"/", p.Items[1].Source, "the onchfs mapping still runs beside web3")
	assert.Equal(t, "https://cdn.example/x.png", p.Items[2].Source)

	// The player echoes dp1_call back in player_status, so the mapping has
	// to be idempotent or a work would gain a second identity per round.
	again, err := json.Marshal(p)
	require.NoError(t, err)
	var round dp1.Playlist
	require.NoError(t, json.Unmarshal(again, &round))
	assert.Equal(t, digitalSoulHost+"/artpiece", round.Items[0].Source)
}

func TestCastPlanNarrowMapsWeb3Sources(t *testing.T) {
	item := dp1playlist.PlaylistItem{ID: "a", Source: "web3://" + digitalSoulContract + ":1/artpiece"}
	plan := dp1.CastPlan{Segments: []dp1.CastSegment{{Item: &item}}}
	assert.Equal(t, digitalSoulHost+"/artpiece", plan.Narrow(nil)[0].Source)
	assert.Equal(t, "web3://"+digitalSoulContract+":1/artpiece", item.Source,
		"the plan's own item is not written through")
}

// DP-1 hydration decodes items without Playlist.UnmarshalJSON, so
// processDynamicPlaylist's own exit is what maps them. Unlike onchfs, web3
// needs nothing from the item's provenance: the URL carries the chain.
func TestDynamicHydration_Web3WorkLeavesWithADeviceLocalSource(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()
	body := `{"data":{"items":[{"id":"585f79b6-a45f-4c1c-8080-e93a192adccc","title":"w","duration":10,"license":"open",` +
		`"source":"web3://` + digitalSoulContract + `/artpiece"}]}}`
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
	assert.Equal(t, digitalSoulHost+"/artpiece", result.Items[0].Source)
}

func TestResolveSeriesCastPlan_MapsWeb3EditionsInThePlan(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	// Art Blocks' generator: one contract, two editions, which is exactly
	// the case where a per-contract origin means the two share one origin.
	const generator = "0x953d288708bb771f969fcfd9ba0819ef506ac718"
	a, b := editionItem("a"), editionItem("b")
	a.Source = "web3://" + generator + ":1/getTokenHtml/" + digitalSoulContract + "/1"
	b.Source = "web3://" + generator + ":1/getTokenHtml/" + digitalSoulContract + "/2"
	serveEditions(ts, "playlist-1", "item-series", editionsBody(2, a, b))
	ts.mockJSON.EXPECT().Unmarshal(gomock.Any(), gomock.Any()).AnyTimes().
		DoAndReturn(func(data []byte, v interface{}) error { return json.Unmarshal(data, v) })

	playlist := dp1.Playlist{Playlist: dp1playlist.Playlist{ID: "playlist-1", Items: []dp1playlist.PlaylistItem{seriesItem()}}}
	plan := ts.client.ResolveSeriesCastPlan(ts.ctx, playlist, castPlanBody(seriesItemRaw("full")), nil, 0)

	require.True(t, plan.HasSeriesGroups())
	editions := plan.Segments[0].Group.Editions
	require.Len(t, editions, 2)
	for _, e := range editions {
		assert.True(t, strings.HasPrefix(e.Source, "https://"),
			"the plan's own editions carry the device-local source, got %q", e.Source)
		assert.Contains(t, e.Source, ".1.web3.invalid/getTokenHtml/")
	}
	assert.NotEqual(t, editions[0].Source, editions[1].Source, "two editions are two works")
}

// A web3 work has exactly one device-local source, because its URL carries
// its chain id; onchfs has several, because the chain label comes from the
// item's provenance which a caller holding the raw URI cannot know.
func TestWeb3SourceVariantsAndSameWork(t *testing.T) {
	raw := "web3://" + digitalSoulContract + ":1/artpiece"
	variants := dp1.DeviceLocalSourceVariants(raw)
	require.Len(t, variants, 1)
	assert.Equal(t, digitalSoulHost+"/artpiece", variants[0])

	assert.True(t, dp1.SameDeviceLocalWork(raw, digitalSoulHost+"/artpiece"))
	assert.True(t, dp1.SameDeviceLocalWork("web3://"+digitalSoulContract+"/artpiece", raw),
		"an omitted chain id is chain 1")
	assert.True(t, dp1.SameWeb3Work(raw, digitalSoulHost+"/artpiece"))
	assert.False(t, dp1.SameDeviceLocalWork(raw, digitalSoulHost+"/other"))
	assert.False(t, dp1.SameDeviceLocalWork(raw, "https://cdn.example/x.png"))
	assert.False(t, dp1.SameDeviceLocalWork("https://cdn.example/x.png", "https://cdn.example/x.png"),
		"an ordinary source is not a device-local work")
	assert.Nil(t, dp1.DeviceLocalSourceVariants("https://cdn.example/x.png"))
}

// CastPlan.Full() is the second flatten of a plan, beside Narrow(), and the
// offline-cache download path takes it
// (commandrouter.resolveOfflineCachePlaylist). It is an exit as much as
// Narrow is, so it maps too: the two flattens of one plan must not be able to
// disagree about a work's identity, because the offline-cache key is sha256 of
// the exact source string.
func TestCastPlanFullMapsDeviceLocalSources(t *testing.T) {
	onchfsItem := dp1playlist.PlaylistItem{ID: "a", Source: "onchfs://" + caseyReasRoot}
	web3Item := dp1playlist.PlaylistItem{ID: "b", Source: "web3://" + digitalSoulContract + ":1/artpiece"}
	plan := dp1.CastPlan{Segments: []dp1.CastSegment{{Item: &onchfsItem}, {Item: &web3Item}}}

	full := plan.Full()
	require.Len(t, full, 2)
	assert.Equal(t, caseyReasHost+"/", full[0].Source)
	assert.Equal(t, digitalSoulHost+"/artpiece", full[1].Source)

	narrow := plan.Narrow(nil)
	require.Len(t, narrow, 2)
	assert.Equal(t, narrow[0].Source, full[0].Source, "the two flattens of one plan must agree")
	assert.Equal(t, narrow[1].Source, full[1].Source, "the two flattens of one plan must agree")

	assert.Equal(t, "onchfs://"+caseyReasRoot, onchfsItem.Source, "the plan's own items are not written through")
	assert.Equal(t, "web3://"+digitalSoulContract+":1/artpiece", web3Item.Source, "the plan's own items are not written through")
}
