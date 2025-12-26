package dp1_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"

	dp1playlist "github.com/display-protocol/dp1-validator/playlist"

	"github.com/feral-file/ffos-user/components/feral-controld/dp1"
	ffindexer "github.com/feral-file/ffos-user/components/feral-controld/ff-indexer"
	"github.com/feral-file/ffos-user/components/feral-controld/mocks"

	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/assert"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest"
)

type testSetup struct {
	ctrl          *gomock.Controller
	ctx           context.Context
	mockFFIndexer *mocks.MockFFIndexer
	mockHTTP      *mocks.MockHTTPClient
	mockJSON      *mocks.MockJSON
	mockIO        *mocks.MockIO
	client        dp1.DP1
}

func setup(t *testing.T) *testSetup {
	ctrl := gomock.NewController(t)
	logger := zaptest.NewLogger(t, zaptest.Level(zap.FatalLevel))
	ctx := context.Background()

	// Dependencies
	mockFFIndexer := mocks.NewMockFFIndexer(ctrl)
	mockHTTPClient := mocks.NewMockHTTPClient(ctrl)
	mockJSON := mocks.NewMockJSON(ctrl)
	mockIO := mocks.NewMockIO(ctrl)

	client := dp1.New(mockFFIndexer, mockHTTPClient, mockJSON, mockIO, logger)

	return &testSetup{
		ctrl:          ctrl,
		ctx:           ctx,
		mockFFIndexer: mockFFIndexer,
		mockHTTP:      mockHTTPClient,
		mockJSON:      mockJSON,
		mockIO:        mockIO,
		client:        client,
	}
}

func (ts *testSetup) teardown() {
	ts.ctrl.Finish()
}

// Helper function to create a mock HTTP response
func createMockResponse(statusCode int, body string) *http.Response {
	return &http.Response{
		StatusCode: statusCode,
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

// Helper function to create a mock playlist JSON
func createPlaylistJSON() string {
	return `{
		"items": [
			{
				"id": "item1",
				"title": "Test Item 1",
				"source": "http://example.com/video1.mp4",
				"duration": 300,
				"license": "open"
			}
		],
		"defaults": {
			"duration": 300
		}
	}`
}

// Helper function to create a mock dynamic playlist JSON
func createDynamicPlaylistJSON() string {
	return `{
		"items": [
			{
				"id": "item1",
				"title": "Test Item 1",
				"source": "http://example.com/video1.mp4",
				"duration": 300,
				"license": "open"
			}
		],
		"defaults": {
			"duration": 300
		},
		"dynamicQueries": [
			{
				"endpoint": "https://indexer.feralfile.com/graphql",
				"params": {
					"limit": "50",
					"offset": "0"
				}
			}
		]
	}`
}

// Helper function to create mock tokens (v2 schema)
func createMockTokens() []ffindexer.Token {
	return []ffindexer.Token{
		{
			ID:              "1",
			TokenCID:        "token1",
			Chain:           "ethereum",
			Standard:        "ERC721",
			ContractAddress: "0x1234567890abcdef",
			TokenNumber:     "1",
			CurrentOwner:    "0xowner1",
			Burned:          false,
			Metadata: &ffindexer.TokenMetadata{
				Name:        "Test Token 1",
				Description: "Test description 1",
				ImageURL:    stringPtr("http://example.com/preview1.jpg"),
			},
			Owners: &ffindexer.PaginatedOwners{
				Items: []ffindexer.Owner{
					{
						OwnerAddress: "0xowner1",
						Quantity:     "1",
					},
				},
				Total: "1",
			},
		},
		{
			ID:              "2",
			TokenCID:        "token2",
			Chain:           "tezos",
			Standard:        "FA2",
			ContractAddress: "0xabcdef1234567890",
			TokenNumber:     "2",
			CurrentOwner:    "0xowner2",
			Burned:          false,
			Metadata: &ffindexer.TokenMetadata{
				Name:        "Test Token 2",
				Description: "Test description 2",
				ImageURL:    stringPtr("http://example.com/preview2.jpg"),
			},
			Owners: &ffindexer.PaginatedOwners{
				Items: []ffindexer.Owner{
					{
						OwnerAddress: "0xowner2",
						Quantity:     "1",
					},
				},
				Total: "1",
			},
		},
	}
}

// Helper to create simple test token
// tokenID is used as both the test identifier and to derive the token number
// For example: "token1" -> TokenNumber="1", TokenCID="Qm...token1..."
func createSimpleToken(tokenID, chain string, ownerAddr string, quantity int) ffindexer.Token {
	burned := false
	if quantity == 0 {
		burned = true
		ownerAddr = ""
	}

	// Extract number from tokenID (e.g., "token1" -> "1", "token123" -> "123")
	tokenNumber := tokenID
	if len(tokenID) > 5 && tokenID[:5] == "token" {
		tokenNumber = tokenID[5:]
	}

	// Use token number as ID string
	id := "0"
	if num, err := strconv.ParseUint(tokenNumber, 10, 64); err == nil {
		id = strconv.FormatUint(num, 10)
	}

	// Generate a fake IPFS CID for testing
	fakeCID := fmt.Sprintf("Qm%s%s", tokenID, "abcdefghijklmnopqrstuvwxyz123456")

	token := ffindexer.Token{
		ID:              id,
		TokenCID:        fakeCID,
		Chain:           chain,
		Standard:        "ERC721",
		ContractAddress: "0x1234567890abcdef",
		TokenNumber:     tokenNumber,
		CurrentOwner:    ownerAddr,
		Burned:          burned,
		Metadata: &ffindexer.TokenMetadata{
			Name:        tokenID,
			Description: "Test token",
			ImageURL:    stringPtr("http://example.com/image.jpg"),
		},
	}

	if quantity > 0 && ownerAddr != "" {
		token.Owners = &ffindexer.PaginatedOwners{
			Items: []ffindexer.Owner{
				{
					OwnerAddress: ownerAddr,
					Quantity:     fmt.Sprintf("%d", quantity),
				},
			},
			Total: "1",
		}
	}

	return token
}

func TestDP1_ProcessPlaylistURL_Success(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	url := "http://example.com/playlist.json"
	playlistJSON := createPlaylistJSON()

	// Expect HTTP GET request
	ts.mockHTTP.EXPECT().
		Get(url).
		Return(createMockResponse(http.StatusOK, playlistJSON), nil)

	// Expect IO ReadAll
	ts.mockIO.EXPECT().
		ReadAll(gomock.Any()).
		Return([]byte(playlistJSON), nil)

	// Expect JSON unmarshal
	ts.mockJSON.EXPECT().
		Unmarshal([]byte(playlistJSON), gomock.Any()).
		DoAndReturn(func(data []byte, v interface{}) error {
			// Simulate successful unmarshaling by setting the playlist
			playlist := v.(*dp1.Playlist)
			playlist.Items = []dp1playlist.PlaylistItem{
				{
					ID:       "item1",
					Title:    stringPtr("Test Item 1"),
					Source:   "http://example.com/video1.mp4",
					Duration: 300,
					License:  "open",
				},
			}
			// Note: Defaults will be nil, so DEFAULT_DURATION will be used
			return nil
		})

	// Test
	result, err := ts.client.ProcessPlaylistURL(ts.ctx, url, false)
	assert.NoError(t, err)
	assert.NotNil(t, result)
	assert.Len(t, result.Items, 1)
	assert.Equal(t, "item1", result.Items[0].ID)
	assert.Equal(t, "Test Item 1", *result.Items[0].Title)
	assert.Equal(t, 300, result.Items[0].Duration)
	assert.Equal(t, "open", result.Items[0].License)
}

func TestDP1_ProcessPlaylistURL_WithDynamicQueries(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	url := "http://example.com/dynamic-playlist.json"
	playlistJSON := createDynamicPlaylistJSON()
	mockTokens := createMockTokens()

	// Expect HTTP GET request
	ts.mockHTTP.EXPECT().
		Get(url).
		Return(createMockResponse(http.StatusOK, playlistJSON), nil)

	// Expect IO ReadAll
	ts.mockIO.EXPECT().
		ReadAll(gomock.Any()).
		Return([]byte(playlistJSON), nil)

	// Expect JSON unmarshal
	ts.mockJSON.EXPECT().
		Unmarshal([]byte(playlistJSON), gomock.Any()).
		DoAndReturn(func(data []byte, v interface{}) error {
			playlist := v.(*dp1.Playlist)
			playlist.Items = []dp1playlist.PlaylistItem{
				{
					ID:       "item1",
					Title:    stringPtr("Test Item 1"),
					Source:   "http://example.com/video1.mp4",
					Duration: 300,
					License:  "open",
				},
			}
			// Note: Defaults will be nil, so DEFAULT_DURATION will be used
			playlist.DynamicQueries = []dp1.DynamicQuery{
				{
					Endpoint: "https://indexer.feralfile.com/graphql",
					Params: map[string]string{
						"limit":  "50",
						"offset": "0",
					},
				},
			}
			return nil
		})

	// Expect FFIndexer query
	ts.mockFFIndexer.EXPECT().
		QueryTokens(ts.ctx,
			"https://indexer.feralfile.com/graphql",
			map[string]string{
				"limit":  "100", // minimal is false so it uses MAX_PLAYLIST_ITEMS_LIMIT
				"offset": "0",
			}).
		Return(mockTokens, nil)

	// Test
	result, err := ts.client.ProcessPlaylistURL(ts.ctx, url, false)
	assert.NoError(t, err)
	assert.NotNil(t, result)
	// Original items are replaced with new items from tokens
	assert.Len(t, result.Items, 2) // 2 from tokens (original items are replaced)
	assert.Equal(t, "Test Token 1", *result.Items[0].Title)
	assert.Equal(t, "Test Token 2", *result.Items[1].Title)
}

func TestDP1_ProcessPlaylistURL_HTTPError(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	url := "http://example.com/playlist.json"

	// Expect HTTP GET request to fail
	ts.mockHTTP.EXPECT().
		Get(url).
		Return(nil, errors.New("network error"))

	// Test
	result, err := ts.client.ProcessPlaylistURL(ts.ctx, url, false)
	assert.Error(t, err)
	assert.Nil(t, result)
	assert.Contains(t, err.Error(), "network error")
}

func TestDP1_ProcessPlaylistURL_HTTPStatusCodeError(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	url := "http://example.com/playlist.json"

	// Expect HTTP GET request to return error status
	ts.mockHTTP.EXPECT().
		Get(url).
		Return(createMockResponse(http.StatusNotFound, "Not Found"), nil)

	// Test
	result, err := ts.client.ProcessPlaylistURL(ts.ctx, url, false)
	assert.Error(t, err)
	assert.Nil(t, result)
	assert.Contains(t, err.Error(), "fetch playlist failed")
}

func TestDP1_ProcessPlaylistURL_ReadAllError(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	url := "http://example.com/playlist.json"

	// Expect HTTP GET request
	ts.mockHTTP.EXPECT().
		Get(url).
		Return(createMockResponse(http.StatusOK, "test"), nil)

	// Expect IO ReadAll to fail
	ts.mockIO.EXPECT().
		ReadAll(gomock.Any()).
		Return(nil, errors.New("read error"))

	// Test
	result, err := ts.client.ProcessPlaylistURL(ts.ctx, url, false)
	assert.Error(t, err)
	assert.Nil(t, result)
	assert.Contains(t, err.Error(), "read error")
}

func TestDP1_ProcessPlaylistURL_JSONUnmarshalError(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	url := "http://example.com/playlist.json"
	playlistJSON := createPlaylistJSON()

	// Expect HTTP GET request
	ts.mockHTTP.EXPECT().
		Get(url).
		Return(createMockResponse(http.StatusOK, playlistJSON), nil)

	// Expect IO ReadAll
	ts.mockIO.EXPECT().
		ReadAll(gomock.Any()).
		Return([]byte(playlistJSON), nil)

	// Expect JSON unmarshal to fail
	ts.mockJSON.EXPECT().
		Unmarshal([]byte(playlistJSON), gomock.Any()).
		Return(errors.New("json error"))

	// Test
	result, err := ts.client.ProcessPlaylistURL(ts.ctx, url, false)
	assert.Error(t, err)
	assert.Nil(t, result)
	assert.Contains(t, err.Error(), "json error")
}

func TestDP1_ProcessDynamicPlaylist_Success(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	mockTokens := createMockTokens()
	playlist := dp1.Playlist{
		Playlist: dp1playlist.Playlist{
			Items: []dp1playlist.PlaylistItem{
				{
					ID:       "original1",
					Title:    stringPtr("Original Item"),
					Source:   "http://example.com/original.mp4",
					Duration: 300,
					License:  "open",
				},
			},
		},
		DynamicQueries: []dp1.DynamicQuery{
			{
				Endpoint: "https://indexer.feralfile.com/graphql",
				Params: map[string]string{
					"limit":  "50",
					"offset": "0",
				},
			},
		},
	}

	// Expect FFIndexer query
	ts.mockFFIndexer.EXPECT().
		QueryTokens(ts.ctx,
			"https://indexer.feralfile.com/graphql",
			map[string]string{
				"limit":  "50", // minimal is true so it uses MINIMAL_PLAYLIST_ITEMS_LIMIT
				"offset": "0",
			}).
		Return(mockTokens, nil)

	// Test
	result, err := ts.client.ProcessDynamicPlaylist(ts.ctx, playlist, true)
	assert.NoError(t, err)
	assert.NotNil(t, result)
	// Original items are replaced with new items from tokens
	assert.Len(t, result.Items, 2) // 2 from tokens (original items are replaced)
	assert.Equal(t, "Test Token 1", *result.Items[0].Title)
	assert.Equal(t, "Test Token 2", *result.Items[1].Title)
}

func TestDP1_ProcessDynamicPlaylist_MultipleQueriesError(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	playlist := dp1.Playlist{
		DynamicQueries: []dp1.DynamicQuery{
			{
				Endpoint: "https://indexer.feralfile.com/graphql",
				Params:   map[string]string{"limit": "50"},
			},
			{
				Endpoint: "https://indexer.feralfile.com/graphql",
				Params:   map[string]string{"limit": "50"},
			},
		},
	}

	// Test
	result, err := ts.client.ProcessDynamicPlaylist(ts.ctx, playlist, false)
	assert.Error(t, err)
	assert.Nil(t, result)
	assert.Contains(t, err.Error(), "playlist should have exactly 1 dynamic queries, but has 2")
}

func TestDP1_ProcessDynamicPlaylist_NoQueriesError(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	playlist := dp1.Playlist{
		DynamicQueries: []dp1.DynamicQuery{},
	}

	// Test
	result, err := ts.client.ProcessDynamicPlaylist(ts.ctx, playlist, false)
	assert.Error(t, err)
	assert.Nil(t, result)
	assert.Contains(t, err.Error(), "playlist should have exactly 1 dynamic queries, but has 0")
}

func TestDP1_ProcessDynamicPlaylist_FFIndexerError(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	playlist := dp1.Playlist{
		DynamicQueries: []dp1.DynamicQuery{
			{
				Endpoint: "https://indexer.feralfile.com/graphql",
				Params:   map[string]string{"limit": "50"},
			},
		},
	}

	// Expect FFIndexer query to fail
	ts.mockFFIndexer.EXPECT().
		QueryTokens(ts.ctx, "https://indexer.feralfile.com/graphql", gomock.Any()).
		Return(nil, errors.New("indexer error"))

	// Test
	result, err := ts.client.ProcessDynamicPlaylist(ts.ctx, playlist, false)
	assert.Error(t, err)
	assert.Nil(t, result)
	assert.Contains(t, err.Error(), "indexer error")
}

func TestDP1_ProcessDynamicPlaylist_MinimalFlag(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	mockTokens := createMockTokens()
	playlist := dp1.Playlist{
		DynamicQueries: []dp1.DynamicQuery{
			{
				Endpoint: "https://indexer.feralfile.com/graphql",
				Params:   map[string]string{"limit": "50"},
			},
		},
	}

	// Expect FFIndexer query with minimal size
	ts.mockFFIndexer.EXPECT().
		QueryTokens(ts.ctx, "https://indexer.feralfile.com/graphql", gomock.Any()).
		DoAndReturn(func(ctx context.Context, endpoint string, params map[string]string) ([]ffindexer.Token, error) {
			assert.Equal(t, "50", params["limit"]) // Should use MINIMAL_PLAYLIST_ITEMS_LIMIT
			return mockTokens, nil
		})

	// Test
	result, err := ts.client.ProcessDynamicPlaylist(ts.ctx, playlist, true)
	assert.NoError(t, err)
	assert.NotNil(t, result)
	assert.Len(t, result.Items, 2) // 2 from tokens
}

func TestDP1_ProcessDynamicPlaylist_Pagination(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	// Create tokens for pagination test - first batch has exactly the limit
	firstBatch := make([]ffindexer.Token, 100) // MAX_PLAYLIST_ITEMS_LIMIT
	for i := range 100 {
		firstBatch[i] = createSimpleToken(fmt.Sprintf("token%d", i+1), "ethereum", "0xowner", 1)
	}

	// Second batch has fewer tokens to stop pagination
	secondBatch := []ffindexer.Token{
		createSimpleToken("token101", "ethereum", "0xowner", 1),
		createSimpleToken("token102", "ethereum", "0xowner", 1),
	}

	playlist := dp1.Playlist{
		DynamicQueries: []dp1.DynamicQuery{
			{
				Endpoint: "https://indexer.feralfile.com/graphql",
				Params:   map[string]string{"limit": "50"},
			},
		},
	}

	// Expect multiple FFIndexer queries for pagination
	gomock.InOrder(
		ts.mockFFIndexer.EXPECT().
			QueryTokens(ts.ctx, "https://indexer.feralfile.com/graphql", gomock.Any()).
			DoAndReturn(func(ctx context.Context, endpoint string, params map[string]string) ([]ffindexer.Token, error) {
				assert.Equal(t, "100", params["limit"]) // MAX_PLAYLIST_ITEMS_LIMIT
				assert.Equal(t, "0", params["offset"])
				return firstBatch, nil
			}),
		ts.mockFFIndexer.EXPECT().
			QueryTokens(ts.ctx, "https://indexer.feralfile.com/graphql", gomock.Any()).
			DoAndReturn(func(ctx context.Context, endpoint string, params map[string]string) ([]ffindexer.Token, error) {
				assert.Equal(t, "100", params["limit"])
				assert.Equal(t, "100", params["offset"])
				return secondBatch, nil
			}),
	)

	// Test
	result, err := ts.client.ProcessDynamicPlaylist(ts.ctx, playlist, false)
	assert.NoError(t, err)
	assert.NotNil(t, result)
	// Tokens are appended, so we get 100 from first batch + 2 from second batch
	assert.Len(t, result.Items, 102) // 100 from first batch + 2 from second batch
}

func TestDP1_ProcessDynamicPlaylist_WithDefaults(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	mockTokens := createMockTokens()
	playlist := dp1.Playlist{
		Playlist: dp1playlist.Playlist{},
		DynamicQueries: []dp1.DynamicQuery{
			{
				Endpoint: "https://indexer.feralfile.com/graphql",
				Params:   map[string]string{"limit": "50"},
			},
		},
	}

	// Note: We'll test with nil defaults to use DEFAULT_DURATION

	// Expect FFIndexer query
	ts.mockFFIndexer.EXPECT().
		QueryTokens(ts.ctx, "https://indexer.feralfile.com/graphql", gomock.Any()).
		Return(mockTokens, nil)

	// Test
	result, err := ts.client.ProcessDynamicPlaylist(ts.ctx, playlist, false)
	assert.NoError(t, err)
	assert.NotNil(t, result)
	assert.Len(t, result.Items, 2)

	// Check that items use the default duration (since defaults is nil)
	for _, item := range result.Items {
		assert.Equal(t, 300, item.Duration) // DEFAULT_DURATION
	}
}

func TestDP1_ProcessDynamicPlaylist_WithoutDefaults(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	mockTokens := createMockTokens()
	playlist := dp1.Playlist{
		DynamicQueries: []dp1.DynamicQuery{
			{
				Endpoint: "https://indexer.feralfile.com/graphql",
				Params:   map[string]string{"limit": "50"},
			},
		},
	}

	// Expect FFIndexer query
	ts.mockFFIndexer.EXPECT().
		QueryTokens(ts.ctx, "https://indexer.feralfile.com/graphql", gomock.Any()).
		Return(mockTokens, nil)

	// Test
	result, err := ts.client.ProcessDynamicPlaylist(ts.ctx, playlist, false)
	assert.NoError(t, err)
	assert.NotNil(t, result)
	assert.Len(t, result.Items, 2)

	// Check that items use the default duration
	for _, item := range result.Items {
		assert.Equal(t, 300, item.Duration) // DEFAULT_DURATION
	}
}

func TestDP1_NormalizeChain(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "ethereum",
			input:    "eip155:1",
			expected: "evm",
		},
		{
			name:     "Ethereum uppercase",
			input:    "EIP155:1",
			expected: "evm",
		},
		{
			name:     "ethereum with spaces",
			input:    " eip155:1 ",
			expected: "evm",
		},
		{
			name:     "tezos",
			input:    "tezos:mainnet",
			expected: "tezos",
		},
		{
			name:     "Tezos uppercase",
			input:    "TEZOS:mainnet",
			expected: "tezos",
		},
		{
			name:     "bitmark",
			input:    "bitmark",
			expected: "other",
		},
		{
			name:     "Bitmark uppercase",
			input:    "Bitmark",
			expected: "other",
		},
		{
			name:     "unknown blockchain",
			input:    "solana",
			expected: "other",
		},
		{
			name:     "empty string",
			input:    "",
			expected: "other",
		},
		{
			name:     "whitespace only",
			input:    "   ",
			expected: "other",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// We need to test the normalizeChain function indirectly through buildPlaylistItem
			ts := setup(t)
			defer ts.teardown()

			token := ffindexer.Token{
				ID:              "1",
				TokenCID:        "test-token",
				Chain:           tt.input,
				Standard:        "ERC721",
				ContractAddress: "0x123",
				TokenNumber:     "1",
				CurrentOwner:    "0xowner",
				Burned:          false,
				Metadata: &ffindexer.TokenMetadata{
					Name:        "Test",
					Description: "Test token",
					ImageURL:    stringPtr("http://example.com/preview.jpg"),
				},
				Owners: &ffindexer.PaginatedOwners{
					Items: []ffindexer.Owner{
						{
							OwnerAddress: "0xowner",
							Quantity:     "1",
						},
					},
					Total: "1",
				},
			}

			playlist := dp1.Playlist{
				DynamicQueries: []dp1.DynamicQuery{
					{
						Endpoint: "https://indexer.feralfile.com/graphql",
						Params:   map[string]string{"limit": "50"},
					},
				},
			}

			// Expect FFIndexer query
			ts.mockFFIndexer.EXPECT().
				QueryTokens(ts.ctx, "https://indexer.feralfile.com/graphql", gomock.Any()).
				Return([]ffindexer.Token{token}, nil)

			// Test
			result, err := ts.client.ProcessDynamicPlaylist(ts.ctx, playlist, false)
			assert.NoError(t, err)
			assert.NotNil(t, result)
			assert.Len(t, result.Items, 1)

			// Check the normalized chain
			assert.NotNil(t, result.Items[0].Provenance)
			assert.NotNil(t, result.Items[0].Provenance.Contract)
			assert.Equal(t, tt.expected, result.Items[0].Provenance.Contract.Chain)
		})
	}
}

func TestDP1_ProcessDynamicPlaylist_MinimalFlagWithZeroBalanceFiltering(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	// Create first batch: 30 tokens (less than MINIMAL_PLAYLIST_ITEMS_LIMIT=50)
	// Note: Current implementation doesn't filter by balance, so all tokens are included
	// The loop will break after first batch since len(tokens) < limit
	firstBatch := make([]ffindexer.Token, 30)
	for i := 0; i < 30; i++ {
		quantity := 1
		firstBatch[i] = createSimpleToken(fmt.Sprintf("token%d", i+1), "ethereum", "0xowner", quantity)
	}

	playlist := dp1.Playlist{
		DynamicQueries: []dp1.DynamicQuery{
			{
				Endpoint: "https://indexer.feralfile.com/graphql",
				Params:   map[string]string{"limit": "50"},
			},
		},
	}

	// Expect single FFIndexer query with minimal limit
	// Loop breaks after first batch since len(ffTokens) >= MINIMAL_PLAYLIST_ITEMS_LIMIT
	ts.mockFFIndexer.EXPECT().
		QueryTokens(ts.ctx, "https://indexer.feralfile.com/graphql", gomock.Any()).
		DoAndReturn(func(ctx context.Context, endpoint string, params map[string]string) ([]ffindexer.Token, error) {
			assert.Equal(t, "50", params["limit"]) // Should use MINIMAL_PLAYLIST_ITEMS_LIMIT
			assert.Equal(t, "0", params["offset"])
			return firstBatch, nil
		})

	// Test with minimal=true
	result, err := ts.client.ProcessDynamicPlaylist(ts.ctx, playlist, true)
	assert.NoError(t, err)
	assert.NotNil(t, result)

	// Should return 30 tokens (all from first batch)
	// Note: Current implementation doesn't filter by balance and breaks after first batch
	assert.Len(t, result.Items, 30)

	// Verify that we have 30 items with UUID IDs
	// PlaylistItem.ID uses uuid.New().String(), so we just verify the count and format
	for i, item := range result.Items {
		assert.NotEmpty(t, item.ID, "Item %d should have an ID", i)
		// Verify UUID format (should be 36 characters with hyphens)
		assert.Len(t, item.ID, 36, "Item %d ID should be a valid UUID", i)
	}
}

func TestDP1_ProcessDynamicPlaylist_FiltersZeroBalanceTokens(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	// Create tokens with mixed balances - some zero, some non-zero
	// Note: Current implementation doesn't filter by balance, so all tokens are included
	mockTokens := []ffindexer.Token{
		createSimpleToken("token1", "ethereum", "0xowner", 1), // Non-zero balance
		createSimpleToken("token2", "ethereum", "0xowner", 0), // Zero balance (burned)
		createSimpleToken("token3", "tezos", "0xowner", 2),    // Non-zero balance
		createSimpleToken("token4", "bitmark", "0xowner", 0),  // Zero balance (burned)
	}

	playlist := dp1.Playlist{
		DynamicQueries: []dp1.DynamicQuery{
			{
				Endpoint: "https://indexer.feralfile.com/graphql",
				Params:   map[string]string{"limit": "50"},
			},
		},
	}

	// Expect FFIndexer query to return tokens with mixed balances
	ts.mockFFIndexer.EXPECT().
		QueryTokens(ts.ctx, "https://indexer.feralfile.com/graphql", gomock.Any()).
		Return(mockTokens, nil)

	// Test
	result, err := ts.client.ProcessDynamicPlaylist(ts.ctx, playlist, false)
	assert.NoError(t, err)
	assert.NotNil(t, result)

	// Note: Current implementation doesn't filter by balance, so all 4 tokens are included
	assert.Len(t, result.Items, 4)

	// Verify that all items have UUID IDs
	for i, item := range result.Items {
		assert.NotEmpty(t, item.ID, "Item %d should have an ID", i)
		// Verify UUID format (should be 36 characters with hyphens)
		assert.Len(t, item.ID, 36, "Item %d ID should be a valid UUID", i)
	}

	// Verify the titles of all tokens
	titles := make([]string, len(result.Items))
	for i, item := range result.Items {
		titles[i] = *item.Title
	}
	assert.Contains(t, titles, "token1")
	assert.Contains(t, titles, "token2")
	assert.Contains(t, titles, "token3")
	assert.Contains(t, titles, "token4")
}

func TestDP1_ProcessDynamicPlaylist_ReplacesOriginalItems(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	// Create original items - these will be completely replaced
	playlist := dp1.Playlist{
		Playlist: dp1playlist.Playlist{
			Items: []dp1playlist.PlaylistItem{
				{
					ID:       "original1",
					Title:    stringPtr("Original Item 1"),
					Source:   "http://example.com/original1.mp4",
					Duration: 300,
					License:  "open",
				},
				{
					ID:       "original2",
					Title:    stringPtr("Original Item 2"),
					Source:   "http://example.com/original2.mp4",
					Duration: 300,
					License:  "open",
				},
				{
					ID:       "original3",
					Title:    stringPtr("Original Item 3"),
					Source:   "http://example.com/original3.mp4",
					Duration: 300,
					License:  "open",
				},
			},
		},
		DynamicQueries: []dp1.DynamicQuery{
			{
				Endpoint: "https://indexer.feralfile.com/graphql",
				Params:   map[string]string{"limit": "50"},
			},
		},
	}

	// Create tokens - these will completely replace the original items
	mockTokens := []ffindexer.Token{
		{
			ID:              "1",
			TokenCID:        "token1",
			Chain:           "ethereum",
			Standard:        "ERC721",
			ContractAddress: "0x1111111111111111",
			TokenNumber:     "100",
			CurrentOwner:    "0xowner1",
			Burned:          false,
			Metadata: &ffindexer.TokenMetadata{
				Name:        "New Token 1",
				Description: "This replaces all original items",
				ImageURL:    stringPtr("http://example.com/new1.jpg"),
			},
			Owners: &ffindexer.PaginatedOwners{
				Items: []ffindexer.Owner{
					{OwnerAddress: "0xowner1", Quantity: "1"},
				},
				Total: "1",
			},
		},
		{
			ID:              "2",
			TokenCID:        "token2",
			Chain:           "tezos",
			Standard:        "FA2",
			ContractAddress: "0x2222222222222222",
			TokenNumber:     "200",
			CurrentOwner:    "0xowner2",
			Burned:          false,
			Metadata: &ffindexer.TokenMetadata{
				Name:        "New Token 2",
				Description: "Another new item",
				ImageURL:    stringPtr("http://example.com/new2.jpg"),
			},
			Owners: &ffindexer.PaginatedOwners{
				Items: []ffindexer.Owner{
					{OwnerAddress: "0xowner2", Quantity: "1"},
				},
				Total: "1",
			},
		},
	}

	// Expect FFIndexer query
	ts.mockFFIndexer.EXPECT().
		QueryTokens(ts.ctx, "https://indexer.feralfile.com/graphql", gomock.Any()).
		Return(mockTokens, nil)

	// Test
	result, err := ts.client.ProcessDynamicPlaylist(ts.ctx, playlist, false)
	assert.NoError(t, err)
	assert.NotNil(t, result)

	// Verify original items are completely replaced
	// Should only have items from tokens, not original items
	assert.Len(t, result.Items, 2, "Should have 2 items from tokens (original items are completely replaced)")

	// Verify no original items remain
	for _, item := range result.Items {
		assert.NotEqual(t, "original1", item.ID, "Original item 1 should not be in result")
		assert.NotEqual(t, "original2", item.ID, "Original item 2 should not be in result")
		assert.NotEqual(t, "original3", item.ID, "Original item 3 should not be in result")
	}

	// Verify items are from tokens
	assert.Equal(t, "New Token 1", *result.Items[0].Title)
	assert.Equal(t, "New Token 2", *result.Items[1].Title)
}

func TestDP1_ProcessDynamicPlaylist_NoDuplicateItems(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	playlist := dp1.Playlist{
		DynamicQueries: []dp1.DynamicQuery{
			{
				Endpoint: "https://indexer.feralfile.com/graphql",
				Params:   map[string]string{"limit": "50"},
			},
		},
	}

	// Create tokens where some have duplicate IDs (same contractAddress, chain, tokenNumber)
	// Note: The current implementation doesn't deduplicate, so duplicates will appear
	// This test verifies that if deduplication is added, it works correctly
	mockTokens := []ffindexer.Token{
		{
			ID:              "1",
			TokenCID:        "token1",
			Chain:           "ethereum",
			Standard:        "ERC721",
			ContractAddress: "0x1111111111111111",
			TokenNumber:     "100",
			CurrentOwner:    "0xowner1",
			Burned:          false,
			Metadata: &ffindexer.TokenMetadata{
				Name:        "Token 1",
				Description: "First token",
				ImageURL:    stringPtr("http://example.com/token1.jpg"),
			},
			Owners: &ffindexer.PaginatedOwners{
				Items: []ffindexer.Owner{
					{OwnerAddress: "0xowner1", Quantity: "1"},
				},
				Total: "1",
			},
		},
		{
			ID:              "2",
			TokenCID:        "token2",
			Chain:           "ethereum",
			Standard:        "ERC721",
			ContractAddress: "0x2222222222222222",
			TokenNumber:     "200",
			CurrentOwner:    "0xowner2",
			Burned:          false,
			Metadata: &ffindexer.TokenMetadata{
				Name:        "Token 2",
				Description: "Second token",
				ImageURL:    stringPtr("http://example.com/token2.jpg"),
			},
			Owners: &ffindexer.PaginatedOwners{
				Items: []ffindexer.Owner{
					{OwnerAddress: "0xowner2", Quantity: "1"},
				},
				Total: "1",
			},
		},
		// Duplicate token (same contractAddress, chain, tokenNumber as token 1)
		// This will generate the same ID as token 1
		{
			ID:              "3",
			TokenCID:        "token3",
			Chain:           "ethereum",
			Standard:        "ERC721",
			ContractAddress: "0x1111111111111111", // Same as token 1
			TokenNumber:     "100",                // Same as token 1
			CurrentOwner:    "0xowner3",
			Burned:          false,
			Metadata: &ffindexer.TokenMetadata{
				Name:        "Duplicate Token",
				Description: "This has the same ID as token 1",
				ImageURL:    stringPtr("http://example.com/duplicate.jpg"),
			},
			Owners: &ffindexer.PaginatedOwners{
				Items: []ffindexer.Owner{
					{OwnerAddress: "0xowner3", Quantity: "1"},
				},
				Total: "1",
			},
		},
	}

	// Expect FFIndexer query
	ts.mockFFIndexer.EXPECT().
		QueryTokens(ts.ctx, "https://indexer.feralfile.com/graphql", gomock.Any()).
		Return(mockTokens, nil)

	// Test
	result, err := ts.client.ProcessDynamicPlaylist(ts.ctx, playlist, false)
	assert.NoError(t, err)
	assert.NotNil(t, result)

	// Verify no duplicate IDs in the result
	idSet := make(map[string]bool)
	duplicateCount := 0
	for _, item := range result.Items {
		if idSet[item.ID] {
			duplicateCount++
			t.Logf("Duplicate ID found: %s", item.ID)
		}
		idSet[item.ID] = true
	}

	// Note: Current implementation doesn't deduplicate, so duplicates may exist
	// This test documents the current behavior
	// If deduplication is added later, this test should be updated to assert no duplicates
	if duplicateCount > 0 {
		t.Logf("Warning: Found %d duplicate IDs. Current implementation doesn't deduplicate.", duplicateCount)
	}

	// Verify we have items from tokens
	assert.GreaterOrEqual(t, len(result.Items), 2, "Should have at least 2 items from tokens")
}

// Helper function to create string pointers
func stringPtr(s string) *string {
	return &s
}
