package dp1

import (
	"encoding/json"
	"testing"

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
