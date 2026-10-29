package dp1

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/display-protocol/dp1-go/extension/playlists"
	dp1playlist "github.com/display-protocol/dp1-go/playlist"
	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func tokenFromJSON(t *testing.T, doc string) rasterOwnedNode {
	t.Helper()
	var node rasterOwnedNode
	require.NoError(t, json.Unmarshal([]byte(doc), &node))
	return node
}

// TestRasterSourceURL pins the gateway table byte for byte: the offline cache
// keys a work by its exact source string, so the phone, the catalog and the
// wall must produce the same one (ff-app decision 12; `ipfs.io` is retired).
func TestRasterSourceURL(t *testing.T) {
	cases := []struct {
		name       string
		normalized string
		content    string
		platform   string
		want       string
	}{
		{"normalized wins over content", "https://a.example/n", "https://a.example/c", "", "https://a.example/n"},
		{"blank normalized falls back to content", "  ", "https://a.example/c", "", "https://a.example/c"},
		{"plain https is returned verbatim", "", "https://CDN.Example/Path%20X?y=1", "", "https://CDN.Example/Path%20X?y=1"},
		{"retired Feral File host is rewritten, path kept", "", "https://cdn.feralfileassets.com/previews/abc/index.html", "Feral File", "https://cdn.artworks.feralfile.io/previews/abc/index.html"},
		{"ipfs on a Feral File work uses the Feral File gateway", "", "ipfs://QmFeral/index.html", "Feral File", "https://ipfs.feralfile.com/ipfs/QmFeral/index.html"},
		{"ipfs platform match is case-insensitive", "", "ipfs://QmFeral", " feral file ", "https://ipfs.feralfile.com/ipfs/QmFeral"},
		{"ipfs elsewhere uses the public gateway", "", "ipfs://QmOther", "fxhash", "https://ipfs.raribleuserdata.com/ipfs/QmOther"},
		{"ipfs with no platform uses the public gateway", "", "ipfs://QmOther", "", "https://ipfs.raribleuserdata.com/ipfs/QmOther"},
		{"doubled ipfs prefix collapses", "", "ipfs://ipfs/QmDoubled", "", "https://ipfs.raribleuserdata.com/ipfs/QmDoubled"},
		{"ipfs query is kept for fxhash seeds", "", "ipfs://QmFx/?fxhash=oo123", "fxhash", "https://ipfs.raribleuserdata.com/ipfs/QmFx/?fxhash=oo123"},
		{"scheme match is case-insensitive", "", "IPFS://QmUpper", "", "https://ipfs.raribleuserdata.com/ipfs/QmUpper"},
		{"arweave", "", "ar://TxId123", "", "https://arweave.net/TxId123"},
		{"onchfs", "", "onchfs://abcdef/index.html", "", "https://onchfs.fxhash2.xyz/abcdef/index.html"},
		{"empty ipfs rest is nothing to play", "", "ipfs://", "", ""},
		{"data URI is not playable by the wall", "", "data:text/html,hi", "", ""},
		{"bare CID is not playable", "", "QmBare", "", ""},
		{"nothing at all", "", "", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := rasterSourceURL(&rasterMedia{NormalizedContentURL: tc.normalized, ContentURL: tc.content}, tc.platform)
			assert.Equal(t, tc.want, got)
			assert.NotContains(t, got, "ipfs.io")
		})
	}
	assert.Equal(t, "", rasterSourceURL(nil, "Feral File"))
}

func TestLookupRasterChain(t *testing.T) {
	cases := []struct {
		id    string
		ok    bool
		kind  string
		index string
		label string
	}{
		{"eip155:1", true, "evm", "eip155:1", "Ethereum"},
		{"EIP155:8453", true, "evm", "eip155:8453", "Base"},
		{"eip155:999999", true, "evm", "eip155:999999", "EVM chain 999999"},
		{"tezos:NetXdQprcVkpaWU", true, "tezos", "tezos:mainnet", "Tezos"},
		{"tezos:mainnet", true, "tezos", "tezos:mainnet", "Tezos"},
		{"tezos", true, "tezos", "tezos:mainnet", "Tezos"},
		{"solana:mainnet", false, "", "", ""},
		{"bitcoin", false, "", "", ""},
		{"", false, "", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.id, func(t *testing.T) {
			chain, ok := lookupRasterChain(tc.id)
			assert.Equal(t, tc.ok, ok)
			if ok {
				assert.Equal(t, rasterChain{kind: tc.kind, indexerChain: tc.index, label: tc.label}, chain)
			}
		})
	}
}

func TestRasterNodeToItem_TitleRules(t *testing.T) {
	const contract = "0x059edd72cd353df5106d2b9cc5ab83a52287ac3a"
	cases := []struct {
		name string
		doc  string
		want string
	}{
		{
			"token name wins",
			`{"token":{"chainId":"eip155:1","contractAddress":"` + contract + `","tokenId":"78000001","name":" Fidenza #1 ","media":{"contentUrl":"https://a.example/1"},"artworks":[{"title":"Other","platform":{"name":"Art Blocks"}}]}}`,
			"Fidenza #1",
		},
		{
			"Art Blocks: artwork title plus mint from the token id",
			`{"token":{"chainId":"eip155:1","contractAddress":"` + contract + `","tokenId":"78000123","name":"","media":{"contentUrl":"https://a.example/1"},"artworks":[{"title":"Fidenza","platform":{"name":"Art Blocks"}}]}}`,
			"Fidenza #123",
		},
		{
			"Art Blocks platform match tolerates the one-word spelling",
			`{"token":{"chainId":"eip155:1","contractAddress":"` + contract + `","tokenId":"1000005","media":{"contentUrl":"https://a.example/1"},"artworks":[{"title":"Chromie Squiggle","platform":{"name":"ArtBlocks Curated"}}]}}`,
			"Chromie Squiggle #5",
		},
		{
			"other platform: artwork title alone, a Feral File token id is not a mint",
			`{"token":{"chainId":"eip155:1","contractAddress":"` + contract + `","tokenId":"1234567890123456789","media":{"contentUrl":"https://a.example/1"},"artworks":[{"title":"Series Work","platform":{"name":"Feral File"}}]}}`,
			"Series Work",
		},
		{
			"no name, no artwork: chain label, short contract, token id",
			`{"token":{"chainId":"eip155:8453","contractAddress":"` + contract + `","tokenId":"7","media":{"contentUrl":"https://a.example/1"},"artworks":[]}}`,
			"Base 0x059e…ac3a #7",
		},
		{
			"short Tezos contract is not elided",
			`{"token":{"chainId":"tezos:NetXdQprcVkpaWU","contractAddress":"KT1Short","tokenId":"7","media":{"contentUrl":"https://a.example/1"}}}`,
			"Tezos KT1Short #7",
		},
		{
			"unknown EVM network gets a readable label",
			`{"token":{"chainId":"eip155:424242","contractAddress":"` + contract + `","tokenId":"7","media":{"contentUrl":"https://a.example/1"}}}`,
			"EVM chain 424242 0x059e…ac3a #7",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			item, reason := rasterNodeToItem(tokenFromJSON(t, tc.doc))
			require.Empty(t, reason)
			assert.Equal(t, tc.want, item.Title)
		})
	}
}

func TestRasterNodeToItem_ProvenanceAndStandard(t *testing.T) {
	mixedCase := "0x059EDD72Cd353dF5106D2B9cC5ab83a52287aC3a"
	item, reason := rasterNodeToItem(tokenFromJSON(t,
		`{"token":{"chainId":"eip155:1","contractAddress":"`+mixedCase+`","tokenId":1,"tokenStandard":"ERC-1155","media":{"contentUrl":"https://a.example/1"}}}`))
	require.Empty(t, reason)
	require.NotNil(t, item.Provenance)
	assert.Equal(t, "evm", item.Provenance.Contract.Chain)
	assert.Equal(t, "erc1155", item.Provenance.Contract.Standard)
	assert.Equal(t, "0x059edd72cd353df5106d2b9cc5ab83a52287ac3a", item.Provenance.Contract.Address, "EVM provenance address is lowercased")
	assert.Equal(t, "1", item.Provenance.Contract.TokenID, "numeric tokenId becomes its decimal string")

	for standard, want := range map[string]string{"ERC721": "erc721", "fa2": "fa2", "FA2": "fa2", "CRYPTOPUNKS": "other", "": "other"} {
		assert.Equal(t, want, rasterTokenStandard(standard), standard)
	}

	// The item id is the legacy indexer path's UUID: checksummed EVM address,
	// `eip155:1`, decimal token id. Changing any of the three inputs changes
	// every wallet item's id on the fleet.
	assert.Equal(t, generateTokenUUID("0x059EDD72Cd353dF5106D2B9cC5ab83a52287aC3a", "eip155:1", "1"), item.ID)
	lower, _ := rasterNodeToItem(tokenFromJSON(t,
		`{"token":{"chainId":"eip155:1","contractAddress":"0x059edd72cd353df5106d2b9cc5ab83a52287ac3a","tokenId":"1","media":{"contentUrl":"https://a.example/1"}}}`))
	assert.Equal(t, item.ID, lower.ID, "address case as Raster sends it does not change the id")
}

func TestRasterNodeToItem_SkipReasons(t *testing.T) {
	cases := []struct {
		name   string
		doc    string
		reason string
	}{
		{"no token", `{"token":null}`, "no token"},
		{"unsupported chain", `{"token":{"chainId":"solana:mainnet","contractAddress":"abc","tokenId":"1","media":{"contentUrl":"https://a.example/1"}}}`, `unsupported chain "solana:mainnet"`},
		{"no contract", `{"token":{"chainId":"eip155:1","contractAddress":"","tokenId":"1","media":{"contentUrl":"https://a.example/1"}}}`, "no contract address"},
		{"malformed EVM contract", `{"token":{"chainId":"eip155:1","contractAddress":"0x1234","tokenId":"1","media":{"contentUrl":"https://a.example/1"}}}`, "malformed EVM contract address"},
		{"no token id", `{"token":{"chainId":"eip155:1","contractAddress":"0x059edd72cd353df5106d2b9cc5ab83a52287ac3a","tokenId":null,"media":{"contentUrl":"https://a.example/1"}}}`, "no token id"},
		{"no media", `{"token":{"chainId":"eip155:1","contractAddress":"0x059edd72cd353df5106d2b9cc5ab83a52287ac3a","tokenId":"1","media":null}}`, "no playable media URL"},
		{"unplayable scheme", `{"token":{"chainId":"eip155:1","contractAddress":"0x059edd72cd353df5106d2b9cc5ab83a52287ac3a","tokenId":"1","media":{"contentUrl":"oogy://thing"}}}`, "no playable media URL"},
		{"source with whitespace fails the item contract", `{"token":{"chainId":"eip155:1","contractAddress":"0x059edd72cd353df5106d2b9cc5ab83a52287ac3a","tokenId":"1","media":{"contentUrl":"https://a.example/with space"}}}`, "mapped item invalid: source is not an absolute URI"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, reason := rasterNodeToItem(tokenFromJSON(t, tc.doc))
			assert.Equal(t, tc.reason, reason)
		})
	}
}

func TestRasterScalar(t *testing.T) {
	assert.Equal(t, "42", rasterScalar(json.RawMessage(`42`)))
	assert.Equal(t, "42", rasterScalar(json.RawMessage(` "42" `)))
	assert.Equal(t, "", rasterScalar(json.RawMessage(`null`)))
	assert.Equal(t, "", rasterScalar(nil))
	assert.Equal(t, "", rasterScalar(json.RawMessage(`""`)))
}

func TestGraphQLErrorMessage(t *testing.T) {
	var root any
	require.NoError(t, json.Unmarshal([]byte(`{"data":{},"errors":[{"message":"boom"}]}`), &root))
	msg, failed := graphqlErrorMessage(root)
	assert.True(t, failed)
	assert.Equal(t, "boom", msg)

	require.NoError(t, json.Unmarshal([]byte(`{"data":{},"errors":[{}]}`), &root))
	msg, failed = graphqlErrorMessage(root)
	assert.True(t, failed)
	assert.Equal(t, "graphql error", msg)

	require.NoError(t, json.Unmarshal([]byte(`{"data":{},"errors":[]}`), &root))
	_, failed = graphqlErrorMessage(root)
	assert.False(t, failed, "an empty errors array is not a failure")
}

// pagedHTTP answers successive Do calls with the queued bodies and records
// how many requests were made.
type pagedHTTP struct {
	wrapper.HTTPClient
	bodies []string
	calls  int
}

func (p *pagedHTTP) Do(*http.Request) (*http.Response, error) {
	if p.calls >= len(p.bodies) {
		return nil, fmt.Errorf("unexpected request %d", p.calls+1)
	}
	body := p.bodies[p.calls]
	p.calls++
	return &http.Response{StatusCode: 200, Status: "200 OK", Body: io.NopCloser(strings.NewReader(body))}, nil
}

// walkNode is one tokensOwned node on an EVM contract whose lowercase address
// is `0x` + 40 times hexDigit, so tests can steer the work-id tie-break.
func walkNode(hexDigit, tokenID, acquired string) string {
	return fmt.Sprintf(`{"lastAcquiredAt":%q,"token":{"id":"r-%s-%s","chainId":"eip155:1","contractAddress":"0x%s","tokenId":%q,"tokenStandard":"ERC721","name":"w-%s-%s","media":{"contentUrl":"https://a.example/%s/%s"}}}`,
		acquired, hexDigit, tokenID, strings.Repeat(hexDigit, 40), tokenID, hexDigit, tokenID, hexDigit, tokenID)
}

func walkPage(hasNext bool, cursor string, nodes ...string) string {
	return fmt.Sprintf(`{"data":{"address":{"tokensOwned":{"pageInfo":{"hasNextPage":%t,"endCursor":%q},"nodes":[%s]}}}}`,
		hasNext, cursor, strings.Join(nodes, ","))
}

func walkPlaylist() Playlist {
	return Playlist{
		Playlist: dp1playlist.Playlist{DynamicQuery: &playlists.DynamicQuery{
			Profile:  dp1playlist.ProfileGraphQLV1,
			Endpoint: "https://api.raster.art/graphql",
			Query:    "query WalletWalk($after: String) { x }",
			ResponseMapping: playlists.ResponseMapping{
				ItemsPath:  "data.address.tokensOwned.nodes",
				ItemSchema: RasterTokensOwnedItemSchema,
			},
		}},
		DynamicQueryPagination: &DynamicQueryPagination{
			Variable:    "after",
			CursorPath:  "data.address.tokensOwned.pageInfo.endCursor",
			HasNextPath: "data.address.tokensOwned.pageInfo.hasNextPage",
		},
	}
}

func walkTitles(t *testing.T, d *dp1, maxItems int) []string {
	t.Helper()
	result, err := d.processRasterWalk(t.Context(), walkPlaylist(), false, maxItems)
	require.NoError(t, err)
	titles := make([]string, 0, len(result.Items))
	for _, item := range result.Items {
		titles = append(titles, item.Title)
	}
	return titles
}

const (
	t3 = "2026-10-03T00:00:00Z"
	t2 = "2026-10-02T00:00:00Z"
	t1 = "2026-10-01T00:00:00Z"
)

// TestRasterWalk_OrderIsTheAppsWhateverRasterReturns pins the order the app
// shows (newest acquisition first, then work id descending) and that it does
// not depend on the order Raster lists a page in.
func TestRasterWalk_OrderIsTheAppsWhateverRasterReturns(t *testing.T) {
	nodes := []string{
		walkNode("a", "1", t2),
		walkNode("c", "1", t2),
		walkNode("b", "1", t2),
		walkNode("1", "9", t3),
		walkNode("2", "10", t1),
		walkNode("2", "9", t1),
	}
	want := []string{"w-1-9", "w-c-1", "w-b-1", "w-a-1", "w-2-9", "w-2-10"}

	forward := &dp1{rasterHTTP: &pagedHTTP{bodies: []string{walkPage(false, "", nodes...)}}, logger: zap.NewNop()}
	assert.Equal(t, want, walkTitles(t, forward, 0), "work id compares as a string, as the app's does: :9 sorts above :10")

	reversed := append([]string(nil), nodes...)
	slices.Reverse(reversed)
	backward := &dp1{rasterHTTP: &pagedHTTP{bodies: []string{walkPage(false, "", reversed...)}}, logger: zap.NewNop()}
	assert.Equal(t, want, walkTitles(t, backward, 0), "the same wallet in any page order resolves identically")
}

// TestRasterWalk_CapReadsOnThroughATieGroup: when the page ends inside the cap
// item's timestamp, a tied token on the next page may sort above it, so the
// walk must read that page before cutting.
func TestRasterWalk_CapReadsOnThroughATieGroup(t *testing.T) {
	fake := &pagedHTTP{bodies: []string{
		walkPage(true, "c1", walkNode("1", "1", t3), walkNode("a", "1", t2)),
		walkPage(true, "c2", walkNode("f", "1", t2), walkNode("2", "1", t1)),
	}}
	d := &dp1{rasterHTTP: fake, logger: zap.NewNop()}
	assert.Equal(t, []string{"w-1-1", "w-f-1"}, walkTitles(t, d, 2),
		"the tied 0xfff… token from page two outranks page one's 0xaaa…")
	assert.Equal(t, 2, fake.calls, "page two ends older than the cap item, so the walk stops there")
}

func TestRasterWalk_CapStopsWhenThePageEndsOlder(t *testing.T) {
	fake := &pagedHTTP{bodies: []string{
		walkPage(true, "c1", walkNode("1", "1", t3), walkNode("a", "1", t2), walkNode("b", "1", t1)),
	}}
	d := &dp1{rasterHTTP: fake, logger: zap.NewNop()}
	assert.Equal(t, []string{"w-1-1", "w-a-1"}, walkTitles(t, d, 2))
	assert.Equal(t, 1, fake.calls, "nothing unread can make the cut, so no second request")
}

func TestRasterAcquiredUs(t *testing.T) {
	assert.Equal(t, int64(0), rasterAcquiredUs(""))
	assert.Equal(t, int64(0), rasterAcquiredUs("yesterday"))
	assert.Equal(t, rasterAcquiredUs("2026-10-01T00:00:00.000001Z"), rasterAcquiredUs("2026-10-01T00:00:00.0000019Z"),
		"microsecond precision, like the app's sortKeyUs")
	assert.Equal(t, rasterAcquiredUs("2026-10-01T07:00:00+07:00"), rasterAcquiredUs("2026-10-01T00:00:00Z"))
}

func TestRasterWorkIDStandard(t *testing.T) {
	for in, want := range map[string]string{
		"ERC721": "erc721", "erc1155": "erc1155", " FA2 ": "fa2",
		"CRYPTOPUNKS": "cryptopunks", "ORDINAL_INSCRIPTION": "ordinal", "ERC-1155": "other", "": "other",
	} {
		assert.Equal(t, want, rasterWorkIDStandard(in), in)
	}
}

func TestRasterNodeToWalkItem_WorkID(t *testing.T) {
	evm, reason := rasterNodeToWalkItem(tokenFromJSON(t,
		`{"lastAcquiredAt":"2026-10-01T00:00:00Z","token":{"chainId":"eip155:8453","contractAddress":"0x059EDD72Cd353dF5106D2B9cC5ab83a52287aC3a","tokenId":"7","tokenStandard":"ERC721","media":{"contentUrl":"https://a.example/1"}}}`))
	require.Empty(t, reason)
	assert.Equal(t, "eip155:8453:erc721:0x059edd72cd353df5106d2b9cc5ab83a52287ac3a:7", evm.workID)

	tezos, reason := rasterNodeToWalkItem(tokenFromJSON(t,
		`{"token":{"chainId":"tezos:NetXdQprcVkpaWU","contractAddress":"KT1RJ6PbjHpwc3M5rw5s2Nbmefwbuwbdxton","tokenId":"42","tokenStandard":"FA2","media":{"contentUrl":"https://a.example/1"}}}`))
	require.Empty(t, reason)
	assert.Equal(t, "tezos:mainnet:fa2:KT1RJ6PbjHpwc3M5rw5s2Nbmefwbuwbdxton:42", tezos.workID)
	assert.Equal(t, int64(0), tezos.acquiredUs, "no date sorts as oldest, like the app's sortKeyUs 0")
}
