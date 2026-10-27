package dp1_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	dp1playlist "github.com/display-protocol/dp1-go/playlist"
	"github.com/ethereum/go-ethereum/common"
	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest"

	"github.com/feral-file/ffos-user/components/feral-controld/dp1"
	ffindexer "github.com/feral-file/ffos-user/components/feral-controld/ff-indexer"
	"github.com/feral-file/ffos-user/components/feral-controld/mocks"
)

// The wallet walk ff-app casts (lib/domain/extensions/playlist_ext.dart,
// walletDynamicQuery), verbatim apart from the address.
const rasterWalkQuery = `query WalletWalk($after: String) { address(address: "0xabc") { tokensOwned(first: 250, after: $after) { pageInfo { hasNextPage endCursor } nodes { quantity lastAcquiredAt token { id chainId contractAddress tokenId tokenStandard name media { previewHash previewType contentUrl normalizedContentUrl contentType } artworks { id slug title isSeries platform { name } artists { id name slug addresses } } artists { id name slug addresses } } } } } }`

const (
	rasterEndpoint  = "https://api.raster.art/graphql"
	rasterDeviceKey = "device-read-key"
	// Art Blocks' flagship contract, lowercase as Raster returns it.
	artBlocksContract = "0x059edd72cd353df5106d2b9cc5ab83a52287ac3a"
)

type rasterSetup struct {
	ctrl      *gomock.Controller
	mockHTTP  *mocks.MockHTTPClient
	mockFFIdx *mocks.MockFFIndexer
	client    dp1.DP1
}

func setupRaster(t *testing.T, apiKey string) *rasterSetup {
	t.Helper()
	ctrl := gomock.NewController(t)
	logger := zaptest.NewLogger(t, zaptest.Level(zap.FatalLevel))
	mockHTTP := mocks.NewMockHTTPClient(ctrl)
	mockFFIdx := mocks.NewMockFFIndexer(ctrl)
	client := dp1.New(mockFFIdx, mockHTTP, mockHTTP, mocks.NewMockJSON(ctrl), mocks.NewMockIO(ctrl), logger, false, true, apiKey)
	return &rasterSetup{ctrl: ctrl, mockHTTP: mockHTTP, mockFFIdx: mockFFIdx, client: client}
}

// rasterPlaylist is the document ff-app casts for a wallet with no hidden
// works: no items, only the dynamicQuery and its pagination block.
func rasterPlaylist(t *testing.T) dp1.Playlist {
	t.Helper()
	doc := fmt.Sprintf(`{
		"dpVersion": "1.0.0",
		"id": "11111111-2222-4333-8444-555555555555",
		"title": "Wallet",
		"items": [],
		"dynamicQuery": {
			"profile": "graphql-v1",
			"endpoint": %q,
			"method": "POST",
			"headers": {"Content-Type": "application/json", "x-api-key": "key-from-the-document"},
			"query": %q,
			"pagination": {
				"variable": "after",
				"cursorPath": "data.address.tokensOwned.pageInfo.endCursor",
				"hasNextPath": "data.address.tokensOwned.pageInfo.hasNextPage"
			},
			"responseMapping": {
				"itemsPath": "data.address.tokensOwned.nodes",
				"itemSchema": "raster/tokensOwned/1.0"
			}
		}
	}`, rasterEndpoint, rasterWalkQuery)
	var playlist dp1.Playlist
	require.NoError(t, json.Unmarshal([]byte(doc), &playlist))
	return playlist
}

// rasterToken is a complete, playable node. Tests override fields to make
// it unmappable.
func rasterToken(id int, overrides map[string]any) map[string]any {
	token := map[string]any{
		"id":              fmt.Sprintf("raster-%d", id),
		"chainId":         "eip155:1",
		"contractAddress": artBlocksContract,
		"tokenId":         fmt.Sprintf("%d", 78000000+id),
		"tokenStandard":   "ERC721",
		"name":            "",
		"media": map[string]any{
			"previewHash":          "abcd1234",
			"previewType":          "image/2",
			"contentUrl":           fmt.Sprintf("https://generator.artblocks.io/%d", id),
			"normalizedContentUrl": nil,
			"contentType":          "text/html",
		},
		"artworks": []any{map[string]any{
			"id": "aw-1", "slug": "fidenza", "title": "Fidenza", "isSeries": true,
			"platform": map[string]any{"name": "Art Blocks"},
			"artists":  []any{},
		}},
		"artists": []any{},
	}
	for k, v := range overrides {
		token[k] = v
	}
	return token
}

func rasterNode(token map[string]any) map[string]any {
	return map[string]any{"quantity": "1", "lastAcquiredAt": "2026-10-01T00:00:00Z", "token": token}
}

func rasterPage(t *testing.T, nodes []map[string]any, hasNext bool, endCursor string) string {
	t.Helper()
	var cursor any
	if endCursor != "" {
		cursor = endCursor
	}
	body, err := json.Marshal(map[string]any{"data": map[string]any{"address": map[string]any{
		"tokensOwned": map[string]any{
			"pageInfo": map[string]any{"hasNextPage": hasNext, "endCursor": cursor},
			"nodes":    nodes,
		},
	}}})
	require.NoError(t, err)
	return string(body)
}

type graphQLRequest struct {
	Query     string            `json:"query"`
	Variables map[string]string `json:"variables"`
}

func readGraphQLRequest(t *testing.T, req *http.Request) graphQLRequest {
	t.Helper()
	raw, err := io.ReadAll(req.Body)
	require.NoError(t, err)
	var out graphQLRequest
	require.NoError(t, json.Unmarshal(raw, &out))
	return out
}

func respond(status int, body string) (*http.Response, error) {
	return &http.Response{StatusCode: status, Status: fmt.Sprintf("%d", status), Body: io.NopCloser(strings.NewReader(body))}, nil
}

func TestRaster_WalkPagesByCursorAndMapsNodes(t *testing.T) {
	rs := setupRaster(t, rasterDeviceKey)
	page1 := rasterPage(t, []map[string]any{
		rasterNode(rasterToken(1, nil)),
		rasterNode(rasterToken(2, map[string]any{"name": "Named Work"})),
	}, true, "cursor-1")
	page2 := rasterPage(t, []map[string]any{
		rasterNode(rasterToken(3, map[string]any{
			"chainId": "tezos:NetXdQprcVkpaWU", "contractAddress": "KT1RJ6PbjHpwc3M5rw5s2Nbmefwbuwbdxton",
			"tokenId": 42, "tokenStandard": "FA2",
			"media":    map[string]any{"contentUrl": "ipfs://QmTezos", "normalizedContentUrl": ""},
			"artworks": []any{map[string]any{"title": "Teia Work", "platform": map[string]any{"name": "Teia"}}},
		})),
	}, false, "")

	gomock.InOrder(
		rs.mockHTTP.EXPECT().Do(gomock.Any()).DoAndReturn(func(req *http.Request) (*http.Response, error) {
			assert.Equal(t, http.MethodPost, req.Method)
			assert.Equal(t, rasterEndpoint, req.URL.String())
			assert.Equal(t, rasterDeviceKey, req.Header.Get("x-api-key"), "the device's key, never the document's")
			assert.Equal(t, "application/json", req.Header.Get("Content-Type"))
			body := readGraphQLRequest(t, req)
			assert.Equal(t, rasterWalkQuery, body.Query)
			assert.Empty(t, body.Variables, "first page sends no cursor")
			return respond(200, page1)
		}),
		rs.mockHTTP.EXPECT().Do(gomock.Any()).DoAndReturn(func(req *http.Request) (*http.Response, error) {
			body := readGraphQLRequest(t, req)
			assert.Equal(t, rasterWalkQuery, body.Query, "every page carries the query")
			assert.Equal(t, map[string]string{"after": "cursor-1"}, body.Variables)
			return respond(200, page2)
		}),
	)

	result, err := rs.client.ProcessDynamicPlaylist(t.Context(), rasterPlaylist(t), false)
	require.NoError(t, err)
	require.Len(t, result.Items, 3, "Raster's order is kept across pages")

	first := result.Items[0]
	assert.Equal(t, "Fidenza #1", first.Title, "Art Blocks: artwork title plus mint from the token id")
	assert.Equal(t, "https://generator.artblocks.io/1", first.Source)
	require.NotNil(t, first.Duration)
	assert.Equal(t, float64(dp1.DEFAULT_DURATION), *first.Duration)
	assert.Equal(t, "open", first.License)
	require.NotNil(t, first.Provenance)
	assert.Equal(t, dp1playlist.ProvenanceOnChain, first.Provenance.Type)
	assert.Equal(t, &dp1playlist.ProvenanceContract{
		Chain: "evm", Standard: "erc721", Address: artBlocksContract, TokenID: "78000001",
	}, first.Provenance.Contract)

	assert.Equal(t, "Named Work", result.Items[1].Title, "the token's own name wins")

	tezos := result.Items[2]
	assert.Equal(t, "Teia Work", tezos.Title)
	assert.Equal(t, "https://ipfs.raribleuserdata.com/ipfs/QmTezos", tezos.Source)
	assert.Equal(t, &dp1playlist.ProvenanceContract{
		Chain: "tezos", Standard: "fa2", Address: "KT1RJ6PbjHpwc3M5rw5s2Nbmefwbuwbdxton", TokenID: "42",
	}, tezos.Provenance.Contract, "Tezos address verbatim, numeric tokenId as its decimal string")

	assert.Nil(t, result.Verification)
	assert.NotNil(t, result.DynamicQuery, "the resolved playlist keeps its source identity for the refresher")
	assert.NotNil(t, result.DynamicQueryPagination)
}

func TestRaster_MinimalResolveMakesOneRequest(t *testing.T) {
	rs := setupRaster(t, rasterDeviceKey)
	rs.mockHTTP.EXPECT().Do(gomock.Any()).DoAndReturn(func(*http.Request) (*http.Response, error) {
		return respond(200, rasterPage(t, []map[string]any{rasterNode(rasterToken(1, nil))}, true, "more"))
	}).Times(1)

	result, err := rs.client.ProcessDynamicPlaylist(t.Context(), rasterPlaylist(t), true)
	require.NoError(t, err)
	assert.Len(t, result.Items, 1)
}

func TestRaster_CastStopsAtItemCapWithinTheFirstPage(t *testing.T) {
	rs := setupRaster(t, rasterDeviceKey)
	nodes := make([]map[string]any, 0, 300)
	for i := range 300 {
		nodes = append(nodes, rasterNode(rasterToken(i+1, nil)))
	}
	rs.mockHTTP.EXPECT().Do(gomock.Any()).DoAndReturn(func(*http.Request) (*http.Response, error) {
		return respond(200, rasterPage(t, nodes, true, "more"))
	}).Times(1)

	result, err := rs.client.ProcessDynamicPlaylistForCast(t.Context(), rasterPlaylist(t))
	require.NoError(t, err)
	// CAST_PLAYLIST_ITEMS_LIMIT reached on page one, then the no-displayAt
	// trim to the minimal size, exactly as the other dynamic paths behave.
	assert.Len(t, result.Items, dp1.MINIMAL_PLAYLIST_ITEMS_LIMIT)
	assert.Equal(t, "Fidenza #1", result.Items[0].Title)
}

func TestRaster_RefreshIsNotCappedBelowTheWallet(t *testing.T) {
	rs := setupRaster(t, rasterDeviceKey)
	nodes := make([]map[string]any, 0, 300)
	for i := range 300 {
		nodes = append(nodes, rasterNode(rasterToken(i+1, nil)))
	}
	rs.mockHTTP.EXPECT().Do(gomock.Any()).DoAndReturn(func(*http.Request) (*http.Response, error) {
		return respond(200, rasterPage(t, nodes, false, ""))
	}).Times(1)

	result, err := rs.client.ProcessDynamicPlaylist(t.Context(), rasterPlaylist(t), false)
	require.NoError(t, err)
	assert.Len(t, result.Items, 300)
}

func TestRaster_GraphQLErrorFailsTheResolve(t *testing.T) {
	rs := setupRaster(t, rasterDeviceKey)
	rs.mockHTTP.EXPECT().Do(gomock.Any()).DoAndReturn(func(*http.Request) (*http.Response, error) {
		return respond(200, `{"data":null,"errors":[{"message":"query cost 60000 exceeds budget 50000"}]}`)
	})

	result, err := rs.client.ProcessDynamicPlaylist(t.Context(), rasterPlaylist(t), false)
	require.Error(t, err)
	assert.Nil(t, result)
	assert.ErrorIs(t, err, dp1.ErrRasterQuery)
	assert.Contains(t, err.Error(), "query cost 60000 exceeds budget 50000")
}

func TestRaster_LaterPageFailureYieldsNoPartialPlaylist(t *testing.T) {
	rs := setupRaster(t, rasterDeviceKey)
	gomock.InOrder(
		rs.mockHTTP.EXPECT().Do(gomock.Any()).DoAndReturn(func(*http.Request) (*http.Response, error) {
			return respond(200, rasterPage(t, []map[string]any{rasterNode(rasterToken(1, nil))}, true, "cursor-1"))
		}),
		rs.mockHTTP.EXPECT().Do(gomock.Any()).DoAndReturn(func(*http.Request) (*http.Response, error) {
			return respond(502, `bad gateway`)
		}),
	)

	result, err := rs.client.ProcessDynamicPlaylist(t.Context(), rasterPlaylist(t), false)
	require.Error(t, err)
	assert.Nil(t, result, "page one's items are not returned when page two fails")
	assert.ErrorIs(t, err, dp1.ErrRasterQuery)
	assert.Contains(t, err.Error(), "page 2")
	assert.NotContains(t, err.Error(), "bad gateway", "the upstream body is not echoed")
}

func TestRaster_UnmappableNodesAreSkippedNotFatal(t *testing.T) {
	rs := setupRaster(t, rasterDeviceKey)
	page := rasterPage(t, []map[string]any{
		rasterNode(rasterToken(1, map[string]any{"contractAddress": nil})),
		rasterNode(rasterToken(2, map[string]any{"media": nil})),
		rasterNode(rasterToken(3, map[string]any{"media": map[string]any{"contentUrl": "", "normalizedContentUrl": nil}})),
		rasterNode(rasterToken(4, map[string]any{"chainId": "solana:mainnet"})),
		rasterNode(rasterToken(5, map[string]any{"media": map[string]any{"contentUrl": "data:text/html,hi"}})),
		rasterNode(map[string]any{"token": nil}),
		rasterNode(rasterToken(7, nil)),
	}, false, "")
	rs.mockHTTP.EXPECT().Do(gomock.Any()).DoAndReturn(func(*http.Request) (*http.Response, error) {
		return respond(200, page)
	})

	result, err := rs.client.ProcessDynamicPlaylist(t.Context(), rasterPlaylist(t), false)
	require.NoError(t, err)
	require.Len(t, result.Items, 1)
	assert.Equal(t, "Fidenza #7", result.Items[0].Title)
}

func TestRaster_UnknownVendorItemSchemaIsRefusedByName(t *testing.T) {
	rs := setupRaster(t, rasterDeviceKey)
	playlist := rasterPlaylist(t)
	playlist.DynamicQuery.ResponseMapping.ItemSchema = "objkt/collection/2.0"

	result, err := rs.client.ProcessDynamicPlaylist(t.Context(), playlist, false)
	require.Error(t, err)
	assert.Nil(t, result)
	assert.ErrorIs(t, err, dp1.ErrUnsupportedItemSchema)
	assert.Contains(t, err.Error(), `"objkt/collection/2.0"`)
}

func TestRaster_DocumentWithoutPaginationIsRefused(t *testing.T) {
	rs := setupRaster(t, rasterDeviceKey)
	playlist := rasterPlaylist(t)
	playlist.DynamicQueryPagination = nil

	_, err := rs.client.ProcessDynamicPlaylist(t.Context(), playlist, false)
	require.ErrorIs(t, err, dp1.ErrRasterQuery)
	assert.Contains(t, err.Error(), "pagination")
}

func TestRaster_EndpointOffTheRasterAllowlistIsRefused(t *testing.T) {
	for _, endpoint := range []string{
		"https://evil.example/graphql",
		"http://api.raster.art/graphql",
		"https://user:pw@api.raster.art/graphql",
	} {
		t.Run(endpoint, func(t *testing.T) {
			rs := setupRaster(t, rasterDeviceKey)
			playlist := rasterPlaylist(t)
			playlist.DynamicQuery.Endpoint = endpoint

			_, err := rs.client.ProcessDynamicPlaylist(t.Context(), playlist, false)
			require.ErrorIs(t, err, dp1.ErrRasterQuery, "no request is made, so the device key cannot leak")
		})
	}
}

func TestRaster_WalkPastThePageCapIsAnError(t *testing.T) {
	rs := setupRaster(t, rasterDeviceKey)
	calls := 0
	rs.mockHTTP.EXPECT().Do(gomock.Any()).DoAndReturn(func(*http.Request) (*http.Response, error) {
		calls++
		return respond(200, rasterPage(t, []map[string]any{rasterNode(rasterToken(calls, nil))}, true, fmt.Sprintf("cursor-%d", calls)))
	}).Times(200)

	result, err := rs.client.ProcessDynamicPlaylist(t.Context(), rasterPlaylist(t), false)
	require.ErrorIs(t, err, dp1.ErrRasterQuery)
	assert.Nil(t, result)
	assert.Contains(t, err.Error(), "200 pages")
}

func TestRaster_RepeatedCursorStopsTheWalk(t *testing.T) {
	rs := setupRaster(t, rasterDeviceKey)
	gomock.InOrder(
		rs.mockHTTP.EXPECT().Do(gomock.Any()).DoAndReturn(func(*http.Request) (*http.Response, error) {
			return respond(200, rasterPage(t, []map[string]any{rasterNode(rasterToken(1, nil))}, true, "same"))
		}),
		rs.mockHTTP.EXPECT().Do(gomock.Any()).DoAndReturn(func(*http.Request) (*http.Response, error) {
			return respond(200, rasterPage(t, []map[string]any{rasterNode(rasterToken(2, nil))}, true, "same"))
		}),
	)

	result, err := rs.client.ProcessDynamicPlaylist(t.Context(), rasterPlaylist(t), false)
	require.NoError(t, err)
	assert.Len(t, result.Items, 2)
}

func TestRaster_MissingPaginationPathFailsThePage(t *testing.T) {
	rs := setupRaster(t, rasterDeviceKey)
	rs.mockHTTP.EXPECT().Do(gomock.Any()).DoAndReturn(func(*http.Request) (*http.Response, error) {
		return respond(200, `{"data":{"address":{"tokensOwned":{"nodes":[]}}}}`)
	})

	_, err := rs.client.ProcessDynamicPlaylist(t.Context(), rasterPlaylist(t), false)
	require.ErrorIs(t, err, dp1.ErrRasterQuery)
	assert.Contains(t, err.Error(), "hasNextPage")
}

func TestRaster_NoConfiguredKeySendsNoKeyHeader(t *testing.T) {
	rs := setupRaster(t, "")
	rs.mockHTTP.EXPECT().Do(gomock.Any()).DoAndReturn(func(req *http.Request) (*http.Response, error) {
		_, present := req.Header["X-Api-Key"]
		assert.False(t, present, "the document's own x-api-key is dropped and nothing replaces it")
		return respond(200, rasterPage(t, nil, false, ""))
	})

	result, err := rs.client.ProcessDynamicPlaylist(t.Context(), rasterPlaylist(t), false)
	require.NoError(t, err)
	assert.Empty(t, result.Items)
}

// TestRaster_ItemIDMatchesTheLegacyIndexerPath pins the id-stability promise:
// a token resolved through the retired `dynamicQueries` path (ff-indexer-v2,
// which stores EVM addresses EIP-55 checksummed and names Tezos
// `tezos:mainnet`) and the same token resolved through Raster (lowercase EVM
// address, `tezos:NetXdQprcVkpaWU`) carry the same item id.
func TestRaster_ItemIDMatchesTheLegacyIndexerPath(t *testing.T) {
	rs := setupRaster(t, rasterDeviceKey)
	name := "Fidenza #1"
	rs.mockFFIdx.EXPECT().QueryTokens(gomock.Any(), "https://indexer-v2.feralfile.com/graphql", gomock.Any()).
		Return([]ffindexer.Token{
			{Chain: "eip155:1", Standard: "erc721", ContractAddress: common.HexToAddress(artBlocksContract).Hex(), TokenNumber: "78000001",
				Display: &ffindexer.Display{Name: &name}},
			{Chain: "tezos:mainnet", Standard: "fa2", ContractAddress: "KT1RJ6PbjHpwc3M5rw5s2Nbmefwbuwbdxton", TokenNumber: "42",
				Display: &ffindexer.Display{Name: &name}},
		}, nil)
	legacy, err := rs.client.ProcessDynamicPlaylist(t.Context(), dp1.Playlist{
		DynamicQueries: []dp1.LegacyDynamicQuery{{Endpoint: "https://indexer-v2.feralfile.com/graphql", Params: map[string]string{"owners": "0xabc"}}},
	}, false)
	require.NoError(t, err)
	require.Len(t, legacy.Items, 2)

	rs.mockHTTP.EXPECT().Do(gomock.Any()).DoAndReturn(func(*http.Request) (*http.Response, error) {
		return respond(200, rasterPage(t, []map[string]any{
			rasterNode(rasterToken(1, nil)),
			rasterNode(rasterToken(2, map[string]any{
				"chainId": "tezos:NetXdQprcVkpaWU", "contractAddress": "KT1RJ6PbjHpwc3M5rw5s2Nbmefwbuwbdxton", "tokenId": "42", "tokenStandard": "FA2",
			})),
		}, false, ""))
	})
	raster, err := rs.client.ProcessDynamicPlaylist(t.Context(), rasterPlaylist(t), false)
	require.NoError(t, err)
	require.Len(t, raster.Items, 2)

	assert.Equal(t, legacy.Items[0].ID, raster.Items[0].ID, "EVM token id is stable across the switch")
	assert.Equal(t, legacy.Items[1].ID, raster.Items[1].ID, "Tezos token id is stable across the switch")
}

func TestRaster_StaticSnapshotCastIsUntouched(t *testing.T) {
	// A hidden-works snapshot carries items and no dynamicQuery; it never
	// reaches the resolver at all.
	rs := setupRaster(t, rasterDeviceKey)
	_, err := rs.client.ProcessDynamicPlaylist(t.Context(), dp1.Playlist{Playlist: dp1playlist.Playlist{
		Items: []dp1playlist.PlaylistItem{{ID: "x", Source: "https://cdn.example/a"}},
	}}, false)
	require.Error(t, err, "a static playlist has no dynamic query configuration")
	assert.False(t, errors.Is(err, dp1.ErrRasterQuery))
}

func TestPlaylist_PaginationSurvivesTheJSONRoundTrip(t *testing.T) {
	original := rasterPlaylist(t)
	require.Equal(t, &dp1.DynamicQueryPagination{
		Variable:    "after",
		CursorPath:  "data.address.tokensOwned.pageInfo.endCursor",
		HasNextPath: "data.address.tokensOwned.pageInfo.hasNextPage",
	}, original.DynamicQueryPagination, "decoded from inside dynamicQuery")

	// Pointer and value receivers both encode: player_status holds a pointer,
	// the DP1 interface passes values.
	for name, value := range map[string]any{"pointer": &original, "value": original} {
		t.Run(name, func(t *testing.T) {
			encoded, err := json.Marshal(value)
			require.NoError(t, err)

			var wire map[string]json.RawMessage
			require.NoError(t, json.Unmarshal(encoded, &wire))
			_, sibling := wire["pagination"]
			assert.False(t, sibling, "pagination is written back inside dynamicQuery, never as a top-level key")
			var dq map[string]json.RawMessage
			require.NoError(t, json.Unmarshal(wire["dynamicQuery"], &dq))
			assert.JSONEq(t, `{"variable":"after","cursorPath":"data.address.tokensOwned.pageInfo.endCursor","hasNextPath":"data.address.tokensOwned.pageInfo.hasNextPage"}`, string(dq["pagination"]))

			var decoded dp1.Playlist
			require.NoError(t, json.Unmarshal(encoded, &decoded))
			assert.Equal(t, original.DynamicQueryPagination, decoded.DynamicQueryPagination)
			assert.Equal(t, original.DynamicQuery, decoded.DynamicQuery)
			assert.Equal(t, original.ID, decoded.ID)
		})
	}
}

func TestPlaylist_WithoutPaginationEncodesAsBefore(t *testing.T) {
	doc := `{"dpVersion":"1.0.0","id":"p1","title":"T","items":[{"id":"i1","source":"https://cdn.example/a","duration":300}],"dynamicQuery":{"profile":"graphql-v1","endpoint":"https://example.com/graphql","query":"q","responseMapping":{"itemsPath":"data.items","itemSchema":"dp1/1.0"}}}`
	var playlist dp1.Playlist
	require.NoError(t, json.Unmarshal([]byte(doc), &playlist))
	assert.Nil(t, playlist.DynamicQueryPagination)

	encoded, err := json.Marshal(playlist)
	require.NoError(t, err)
	assert.NotContains(t, string(encoded), "pagination")

	// The hook must encode byte for byte what the plain struct would.
	type plain struct {
		dp1playlist.Playlist
		DynamicQueries []dp1.LegacyDynamicQuery `json:"dynamicQueries,omitempty"`
	}
	expected, err := json.Marshal(plain{Playlist: playlist.Playlist})
	require.NoError(t, err)
	assert.Equal(t, string(expected), string(encoded))
}

func TestPlaylist_NonObjectPaginationIsIgnoredNotFatal(t *testing.T) {
	doc := `{"items":[],"dynamicQuery":{"profile":"graphql-v1","endpoint":"https://example.com/graphql","query":"q","pagination":"cursor","responseMapping":{"itemsPath":"data.items","itemSchema":"dp1/1.0"}}}`
	var playlist dp1.Playlist
	require.NoError(t, json.Unmarshal([]byte(doc), &playlist), "a non-Raster document is not refused for a stray pagination value")
	assert.Nil(t, playlist.DynamicQueryPagination)
}

// TestNewRasterHTTPClient_DoesNotFollowRedirects pins the credential
// boundary: the host allowlist checks only the URL the document names, so the
// client that carries x-api-key must hand a 3xx back instead of following it.
func TestNewRasterHTTPClient_DoesNotFollowRedirects(t *testing.T) {
	var leaked atomic.Int32
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != "" {
			leaked.Add(1)
		}
		w.WriteHeader(200)
	}))
	defer elsewhere.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL+"/graphql", http.StatusTemporaryRedirect)
	}))
	defer origin.Close()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, origin.URL+"/graphql", strings.NewReader("{}"))
	require.NoError(t, err)
	req.Header.Set("x-api-key", rasterDeviceKey)
	resp, err := dp1.NewRasterHTTPClient().Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	assert.Equal(t, http.StatusTemporaryRedirect, resp.StatusCode, "the 3xx is returned, not followed")
	assert.Equal(t, int32(0), leaked.Load(), "the key never reached the redirect target")
}

// TestRaster_RedirectedResponseFailsThePage covers the injected-client
// backstop: a client that did follow a redirect is reported, not trusted.
func TestRaster_RedirectedResponseFailsThePage(t *testing.T) {
	rs := setupRaster(t, rasterDeviceKey)
	rs.mockHTTP.EXPECT().Do(gomock.Any()).DoAndReturn(func(req *http.Request) (*http.Response, error) {
		moved := req.Clone(req.Context())
		moved.URL, _ = moved.URL.Parse("https://elsewhere.example/graphql")
		resp, _ := respond(200, rasterPage(t, nil, false, ""))
		resp.Request = moved
		return resp, nil
	})

	_, err := rs.client.ProcessDynamicPlaylist(t.Context(), rasterPlaylist(t), false)
	require.ErrorIs(t, err, dp1.ErrRasterQuery)
	assert.Contains(t, err.Error(), "redirected")
}

// TestRaster_NumericTokenIDsKeepTheirDigits: Raster may send tokenId as a
// JSON number; past 2^53 a float64 decode would round it and change both the
// provenance and the item id.
func TestRaster_NumericTokenIDsKeepTheirDigits(t *testing.T) {
	rs := setupRaster(t, rasterDeviceKey)
	page := `{"data":{"address":{"tokensOwned":{"pageInfo":{"hasNextPage":false,"endCursor":null},"nodes":[
		{"token":{"id":1,"chainId":"eip155:1","contractAddress":"` + artBlocksContract + `","tokenId":9007199254740993,"tokenStandard":"ERC721","media":{"contentUrl":"https://a.example/1"}}},
		{"token":{"id":2,"chainId":"eip155:1","contractAddress":"` + artBlocksContract + `","tokenId":115792089237316195423570985008687907853269984665640564039457584007913129639935,"tokenStandard":"ERC721","media":{"contentUrl":"https://a.example/2"}}}
	]}}}}`
	rs.mockHTTP.EXPECT().Do(gomock.Any()).DoAndReturn(func(*http.Request) (*http.Response, error) {
		return respond(200, page)
	})

	result, err := rs.client.ProcessDynamicPlaylist(t.Context(), rasterPlaylist(t), false)
	require.NoError(t, err)
	require.Len(t, result.Items, 2)
	assert.Equal(t, "9007199254740993", result.Items[0].Provenance.Contract.TokenID)
	assert.Equal(t, "115792089237316195423570985008687907853269984665640564039457584007913129639935", result.Items[1].Provenance.Contract.TokenID)
	assert.Equal(t, "Ethereum 0x059e…ac3a #9007199254740993", result.Items[0].Title)
}
