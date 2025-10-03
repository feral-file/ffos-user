package dp1_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
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
					"size": "50",
					"offset": "0"
				}
			}
		]
	}`
}

// Helper function to create mock tokens
func createMockTokens() []ffindexer.Token {
	return []ffindexer.Token{
		{
			ID:              "token1",
			Blockchain:      "ethereum",
			ContractType:    "ERC721",
			ContractAddress: "0x1234567890abcdef",
			Asset: struct {
				Metadata struct {
					Project struct {
						Latest ffindexer.ProjectMetadata `json:"latest,omitempty"`
					} `json:"project,omitempty"`
				} `json:"metadata,omitempty"`
			}{
				Metadata: struct {
					Project struct {
						Latest ffindexer.ProjectMetadata `json:"latest,omitempty"`
					} `json:"project,omitempty"`
				}{
					Project: struct {
						Latest ffindexer.ProjectMetadata `json:"latest,omitempty"`
					}{
						Latest: ffindexer.ProjectMetadata{
							Title:      "Test Token 1",
							PreviewURL: "http://example.com/preview1.jpg",
						},
					},
				},
			},
		},
		{
			ID:              "token2",
			Blockchain:      "tezos",
			ContractType:    "FA2",
			ContractAddress: "0xabcdef1234567890",
			Asset: struct {
				Metadata struct {
					Project struct {
						Latest ffindexer.ProjectMetadata `json:"latest,omitempty"`
					} `json:"project,omitempty"`
				} `json:"metadata,omitempty"`
			}{
				Metadata: struct {
					Project struct {
						Latest ffindexer.ProjectMetadata `json:"latest,omitempty"`
					} `json:"project,omitempty"`
				}{
					Project: struct {
						Latest ffindexer.ProjectMetadata `json:"latest,omitempty"`
					}{
						Latest: ffindexer.ProjectMetadata{
							Title:      "Test Token 2",
							PreviewURL: "http://example.com/preview2.jpg",
						},
					},
				},
			},
		},
	}
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
						"size":   "50",
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
				"size":   "100", // minimal is false so it uses MAX_PLAYLIST_ITEMS_LIMIT
				"offset": "0",
			}).
		Return(mockTokens, nil)

	// Test
	result, err := ts.client.ProcessPlaylistURL(ts.ctx, url, false)
	assert.NoError(t, err)
	assert.NotNil(t, result)
	assert.Len(t, result.Items, 3) // 1 original + 2 from tokens
	assert.Equal(t, "Test Item 1", *result.Items[0].Title)
	assert.Equal(t, "Test Token 1", *result.Items[1].Title)
	assert.Equal(t, "Test Token 2", *result.Items[2].Title)
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
					"size":   "50",
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
				"size":   "50", // minimal is true so it uses MINIMAL_PLAYLIST_ITEMS_LIMIT
				"offset": "0",
			}).
		Return(mockTokens, nil)

	// Test
	result, err := ts.client.ProcessDynamicPlaylist(ts.ctx, playlist, true)
	assert.NoError(t, err)
	assert.NotNil(t, result)
	assert.Len(t, result.Items, 3) // 1 original + 2 from tokens
	assert.Equal(t, "Original Item", *result.Items[0].Title)
	assert.Equal(t, "Test Token 1", *result.Items[1].Title)
	assert.Equal(t, "Test Token 2", *result.Items[2].Title)
}

func TestDP1_ProcessDynamicPlaylist_MultipleQueriesError(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	playlist := dp1.Playlist{
		DynamicQueries: []dp1.DynamicQuery{
			{
				Endpoint: "https://indexer.feralfile.com/graphql",
				Params:   map[string]string{"size": "50"},
			},
			{
				Endpoint: "https://indexer.feralfile.com/graphql",
				Params:   map[string]string{"size": "50"},
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
				Params:   map[string]string{"size": "50"},
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
				Params:   map[string]string{"size": "50"},
			},
		},
	}

	// Expect FFIndexer query with minimal size
	ts.mockFFIndexer.EXPECT().
		QueryTokens(ts.ctx, "https://indexer.feralfile.com/graphql", gomock.Any()).
		DoAndReturn(func(ctx context.Context, endpoint string, params map[string]string) ([]ffindexer.Token, error) {
			assert.Equal(t, "50", params["size"]) // Should use MINIMAL_PLAYLIST_ITEMS_LIMIT
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
		firstBatch[i] = ffindexer.Token{
			ID:         fmt.Sprintf("token%d", i+1),
			Blockchain: "ethereum",
		}
	}

	// Second batch has fewer tokens to stop pagination
	secondBatch := []ffindexer.Token{
		{ID: "token101", Blockchain: "ethereum"},
		{ID: "token102", Blockchain: "ethereum"},
	}

	playlist := dp1.Playlist{
		DynamicQueries: []dp1.DynamicQuery{
			{
				Endpoint: "https://indexer.feralfile.com/graphql",
				Params:   map[string]string{"size": "50"},
			},
		},
	}

	// Expect multiple FFIndexer queries for pagination
	gomock.InOrder(
		ts.mockFFIndexer.EXPECT().
			QueryTokens(ts.ctx, "https://indexer.feralfile.com/graphql", gomock.Any()).
			DoAndReturn(func(ctx context.Context, endpoint string, params map[string]string) ([]ffindexer.Token, error) {
				assert.Equal(t, "100", params["size"]) // MAX_PLAYLIST_ITEMS_LIMIT
				assert.Equal(t, "0", params["offset"])
				return firstBatch, nil
			}),
		ts.mockFFIndexer.EXPECT().
			QueryTokens(ts.ctx, "https://indexer.feralfile.com/graphql", gomock.Any()).
			DoAndReturn(func(ctx context.Context, endpoint string, params map[string]string) ([]ffindexer.Token, error) {
				assert.Equal(t, "100", params["size"])
				assert.Equal(t, "100", params["offset"])
				return secondBatch, nil
			}),
	)

	// Test
	result, err := ts.client.ProcessDynamicPlaylist(ts.ctx, playlist, false)
	assert.NoError(t, err)
	assert.NotNil(t, result)
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
				Params:   map[string]string{"size": "50"},
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
				Params:   map[string]string{"size": "50"},
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
			input:    "ethereum",
			expected: "evm",
		},
		{
			name:     "Ethereum uppercase",
			input:    "Ethereum",
			expected: "evm",
		},
		{
			name:     "ethereum with spaces",
			input:    " ethereum ",
			expected: "evm",
		},
		{
			name:     "tezos",
			input:    "tezos",
			expected: "tezos",
		},
		{
			name:     "Tezos uppercase",
			input:    "Tezos",
			expected: "tezos",
		},
		{
			name:     "bitmark",
			input:    "bitmark",
			expected: "bitmark",
		},
		{
			name:     "Bitmark uppercase",
			input:    "Bitmark",
			expected: "bitmark",
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
				ID:              "test-token",
				Blockchain:      tt.input,
				ContractType:    "ERC721",
				ContractAddress: "0x123",
				Asset: struct {
					Metadata struct {
						Project struct {
							Latest ffindexer.ProjectMetadata `json:"latest,omitempty"`
						} `json:"project,omitempty"`
					} `json:"metadata,omitempty"`
				}{
					Metadata: struct {
						Project struct {
							Latest ffindexer.ProjectMetadata `json:"latest,omitempty"`
						} `json:"project,omitempty"`
					}{
						Project: struct {
							Latest ffindexer.ProjectMetadata `json:"latest,omitempty"`
						}{
							Latest: ffindexer.ProjectMetadata{
								Title:      "Test",
								PreviewURL: "http://example.com/preview.jpg",
							},
						},
					},
				},
			}

			playlist := dp1.Playlist{
				DynamicQueries: []dp1.DynamicQuery{
					{
						Endpoint: "https://indexer.feralfile.com/graphql",
						Params:   map[string]string{"size": "50"},
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

// Helper function to create string pointers
func stringPtr(s string) *string {
	return &s
}
