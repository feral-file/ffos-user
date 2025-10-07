package ffindexer_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	ffindexer "github.com/feral-file/ffos-user/components/feral-controld/ff-indexer"
	"github.com/feral-file/ffos-user/components/feral-controld/mocks"

	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/assert"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest"
)

type testSetup struct {
	ctrl     *gomock.Controller
	ctx      context.Context
	mockHTTP *mocks.MockHTTP
	mockJSON *mocks.MockJSON
	mockIO   *mocks.MockIO
	client   ffindexer.FFIndexer
}

func setup(t *testing.T) *testSetup {
	ctrl := gomock.NewController(t)
	ctx := context.Background()
	logger := zaptest.NewLogger(t, zaptest.Level(zap.FatalLevel))

	// Dependencies
	mockHTTP := mocks.NewMockHTTP(ctrl)
	mockJSON := mocks.NewMockJSON(ctrl)
	mockIO := mocks.NewMockIO(ctrl)

	client := ffindexer.New(mockHTTP, mockJSON, mockIO, logger)

	return &testSetup{
		ctrl:     ctrl,
		ctx:      ctx,
		mockHTTP: mockHTTP,
		mockJSON: mockJSON,
		mockIO:   mockIO,
		client:   client,
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

// Helper function to create a mock GraphQL response JSON
func createGraphQLResponseJSON() string {
	return `{
		"data": {
			"tokens": [
				{
					"id": "token1",
					"blockchain": "ethereum",
					"contractType": "ERC721",
					"contractAddress": "0x1234567890abcdef",
					"asset": {
						"metadata": {
							"project": {
								"latest": {
									"title": "Test Token 1",
									"previewURL": "http://example.com/preview1.jpg"
								}
							}
						}
					}
				},
				{
					"id": "token2",
					"blockchain": "tezos",
					"contractType": "FA2",
					"contractAddress": "0xabcdef1234567890",
					"asset": {
						"metadata": {
							"project": {
								"latest": {
									"title": "Test Token 2",
									"previewURL": "http://example.com/preview2.jpg"
								}
							}
						}
					}
				}
			]
		}
	}`
}

func TestFFIndexer_QueryTokens_Success(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	endpoint := "https://indexer.feralfile.com/graphql"
	params := map[string]string{
		"limit":  "50",
		"offset": "0",
	}
	responseJSON := createGraphQLResponseJSON()

	// Expect JSON marshal
	ts.mockJSON.EXPECT().
		Marshal(gomock.Any()).
		DoAndReturn(func(v interface{}) ([]byte, error) {
			reqBody := v.(map[string]any)
			// Verify the query contains expected parameters
			query := reqBody["query"].(string)
			assert.Contains(t, query, `limit: "50"`)
			assert.Contains(t, query, `offset: "0"`)
			return []byte(`{"query":"test"}`), nil
		})

	// Expect HTTP POST request
	ts.mockHTTP.EXPECT().
		Post(endpoint, "application/json", gomock.Any()).
		Return(createMockResponse(http.StatusOK, responseJSON), nil)

	// Expect IO ReadAll
	ts.mockIO.EXPECT().
		ReadAll(gomock.Any()).
		Return([]byte(responseJSON), nil)

	// Expect JSON unmarshal
	ts.mockJSON.EXPECT().
		Unmarshal([]byte(responseJSON), gomock.Any()).
		DoAndReturn(func(data []byte, v interface{}) error {
			// Simulate successful unmarshaling by setting the response
			resp := v.(*ffindexer.GraphQLResponse)
			resp.Data.Tokens = []ffindexer.Token{
				{
					ID:              "token1",
					Blockchain:      "ethereum",
					ContractType:    "ERC721",
					ContractAddress: "0x1234567890abcdef",
					Balance:         1,
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
					Balance:         1,
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
			return nil
		})

	// Test
	result, err := ts.client.QueryTokens(ts.ctx, endpoint, params)
	assert.NoError(t, err)
	assert.NotNil(t, result)
	assert.Len(t, result, 2)
	assert.Equal(t, "token1", result[0].ID)
	assert.Equal(t, "ethereum", result[0].Blockchain)
	assert.Equal(t, "Test Token 1", result[0].Asset.Metadata.Project.Latest.Title)
	assert.Equal(t, 1, result[0].Balance)
	assert.Equal(t, "token2", result[1].ID)
	assert.Equal(t, "tezos", result[1].Blockchain)
	assert.Equal(t, "Test Token 2", result[1].Asset.Metadata.Project.Latest.Title)
	assert.Equal(t, 1, result[1].Balance)
}

func TestFFIndexer_QueryTokens_InvalidEndpoint(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	invalidEndpoint := "https://invalid-host.com/graphql"
	params := map[string]string{"limit": "50"}

	// Test
	result, err := ts.client.QueryTokens(ts.ctx, invalidEndpoint, params)
	assert.Error(t, err)
	assert.Nil(t, result)
	assert.Contains(t, err.Error(), "invalid endpoint")
}

func TestFFIndexer_QueryTokens_HTTPError(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	endpoint := "https://indexer.feralfile.com/graphql"
	params := map[string]string{"limit": "50"}

	// Expect JSON marshal
	ts.mockJSON.EXPECT().
		Marshal(gomock.Any()).
		Return([]byte(`{"query":"test"}`), nil)

	// Expect HTTP POST request to fail
	ts.mockHTTP.EXPECT().
		Post(endpoint, "application/json", gomock.Any()).
		Return(nil, errors.New("network error"))

	// Test
	result, err := ts.client.QueryTokens(ts.ctx, endpoint, params)
	assert.Error(t, err)
	assert.Nil(t, result)
	assert.Contains(t, err.Error(), "network error")
}

func TestFFIndexer_QueryTokens_JSONMarshalError(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	endpoint := "https://indexer.feralfile.com/graphql"
	params := map[string]string{"limit": "50"}

	// Expect JSON marshal to fail
	ts.mockJSON.EXPECT().
		Marshal(gomock.Any()).
		Return(nil, errors.New("marshal error"))

	// Test
	result, err := ts.client.QueryTokens(ts.ctx, endpoint, params)
	assert.Error(t, err)
	assert.Nil(t, result)
	assert.Contains(t, err.Error(), "marshal error")
}

func TestFFIndexer_QueryTokens_ReadAllError(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	endpoint := "https://indexer.feralfile.com/graphql"
	params := map[string]string{"limit": "50"}

	// Expect JSON marshal
	ts.mockJSON.EXPECT().
		Marshal(gomock.Any()).
		Return([]byte(`{"query":"test"}`), nil)

	// Expect HTTP POST request
	ts.mockHTTP.EXPECT().
		Post(endpoint, "application/json", gomock.Any()).
		Return(createMockResponse(http.StatusOK, "test"), nil)

	// Expect IO ReadAll to fail
	ts.mockIO.EXPECT().
		ReadAll(gomock.Any()).
		Return(nil, errors.New("read error"))

	// Test
	result, err := ts.client.QueryTokens(ts.ctx, endpoint, params)
	assert.Error(t, err)
	assert.Nil(t, result)
	assert.Contains(t, err.Error(), "read error")
}

func TestFFIndexer_QueryTokens_JSONUnmarshalError(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	endpoint := "https://indexer.feralfile.com/graphql"
	params := map[string]string{"limit": "50"}
	responseJSON := createGraphQLResponseJSON()

	// Expect JSON marshal
	ts.mockJSON.EXPECT().
		Marshal(gomock.Any()).
		Return([]byte(`{"query":"test"}`), nil)

	// Expect HTTP POST request
	ts.mockHTTP.EXPECT().
		Post(endpoint, "application/json", gomock.Any()).
		Return(createMockResponse(http.StatusOK, responseJSON), nil)

	// Expect IO ReadAll
	ts.mockIO.EXPECT().
		ReadAll(gomock.Any()).
		Return([]byte(responseJSON), nil)

	// Expect JSON unmarshal to fail
	ts.mockJSON.EXPECT().
		Unmarshal([]byte(responseJSON), gomock.Any()).
		Return(errors.New("unmarshal error"))

	// Test
	result, err := ts.client.QueryTokens(ts.ctx, endpoint, params)
	assert.Error(t, err)
	assert.Nil(t, result)
	assert.Contains(t, err.Error(), "unmarshal error")
}

func TestFFIndexer_QueryTokens_EmptyParams(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	endpoint := "https://indexer.feralfile.com/graphql"
	params := map[string]string{}
	responseJSON := createGraphQLResponseJSON()

	// Expect JSON marshal
	ts.mockJSON.EXPECT().
		Marshal(gomock.Any()).
		DoAndReturn(func(v interface{}) ([]byte, error) {
			reqBody := v.(map[string]any)
			query := reqBody["query"].(string)
			// Should have empty parameters section
			assert.Contains(t, query, "tokens(\n\t\t\t\n\t\t)")
			return []byte(`{"query":"test"}`), nil
		})

	// Expect HTTP POST request
	ts.mockHTTP.EXPECT().
		Post(endpoint, "application/json", gomock.Any()).
		Return(createMockResponse(http.StatusOK, responseJSON), nil)

	// Expect IO ReadAll
	ts.mockIO.EXPECT().
		ReadAll(gomock.Any()).
		Return([]byte(responseJSON), nil)

	// Expect JSON unmarshal
	ts.mockJSON.EXPECT().
		Unmarshal([]byte(responseJSON), gomock.Any()).
		DoAndReturn(func(data []byte, v interface{}) error {
			resp := v.(*ffindexer.GraphQLResponse)
			resp.Data.Tokens = []ffindexer.Token{}
			return nil
		})

	// Test
	result, err := ts.client.QueryTokens(ts.ctx, endpoint, params)
	assert.NoError(t, err)
	assert.NotNil(t, result)
	assert.Len(t, result, 0)
}

func TestFFIndexer_QueryTokens_ArrayParams(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	endpoint := "https://indexer.feralfile.com/graphql"
	params := map[string]string{
		"blockchains": "ethereum,tezos,bitmark",
		"limit":       "50",
	}
	responseJSON := createGraphQLResponseJSON()

	// Expect JSON marshal
	ts.mockJSON.EXPECT().
		Marshal(gomock.Any()).
		DoAndReturn(func(v interface{}) ([]byte, error) {
			reqBody := v.(map[string]any)
			query := reqBody["query"].(string)
			// Should have array format for blockchains
			assert.Contains(t, query, `blockchains: ["ethereum", "tezos", "bitmark"]`)
			assert.Contains(t, query, `limit: "50"`)
			return []byte(`{"query":"test"}`), nil
		})

	// Expect HTTP POST request
	ts.mockHTTP.EXPECT().
		Post(endpoint, "application/json", gomock.Any()).
		Return(createMockResponse(http.StatusOK, responseJSON), nil)

	// Expect IO ReadAll
	ts.mockIO.EXPECT().
		ReadAll(gomock.Any()).
		Return([]byte(responseJSON), nil)

	// Expect JSON unmarshal
	ts.mockJSON.EXPECT().
		Unmarshal([]byte(responseJSON), gomock.Any()).
		DoAndReturn(func(data []byte, v interface{}) error {
			resp := v.(*ffindexer.GraphQLResponse)
			resp.Data.Tokens = []ffindexer.Token{}
			return nil
		})

	// Test
	result, err := ts.client.QueryTokens(ts.ctx, endpoint, params)
	assert.NoError(t, err)
	assert.NotNil(t, result)
	assert.Len(t, result, 0)
}

func TestFFIndexer_QueryTokens_ArrayParamsWithSpaces(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	endpoint := "https://indexer.feralfile.com/graphql"
	params := map[string]string{
		"blockchains": "ethereum, tezos , bitmark",
		"limit":       "50",
	}
	responseJSON := createGraphQLResponseJSON()

	// Expect JSON marshal
	ts.mockJSON.EXPECT().
		Marshal(gomock.Any()).
		DoAndReturn(func(v interface{}) ([]byte, error) {
			reqBody := v.(map[string]any)
			query := reqBody["query"].(string)
			// Should trim spaces and format as array
			assert.Contains(t, query, `blockchains: ["ethereum", "tezos", "bitmark"]`)
			return []byte(`{"query":"test"}`), nil
		})

	// Expect HTTP POST request
	ts.mockHTTP.EXPECT().
		Post(endpoint, "application/json", gomock.Any()).
		Return(createMockResponse(http.StatusOK, responseJSON), nil)

	// Expect IO ReadAll
	ts.mockIO.EXPECT().
		ReadAll(gomock.Any()).
		Return([]byte(responseJSON), nil)

	// Expect JSON unmarshal
	ts.mockJSON.EXPECT().
		Unmarshal([]byte(responseJSON), gomock.Any()).
		DoAndReturn(func(data []byte, v interface{}) error {
			resp := v.(*ffindexer.GraphQLResponse)
			resp.Data.Tokens = []ffindexer.Token{}
			return nil
		})

	// Test
	result, err := ts.client.QueryTokens(ts.ctx, endpoint, params)
	assert.NoError(t, err)
	assert.NotNil(t, result)
	assert.Len(t, result, 0)
}

func TestFFIndexer_ValidateEndpoint(t *testing.T) {
	tests := []struct {
		name        string
		endpoint    string
		expectError bool
		errorMsg    string
	}{
		{
			name:        "valid endpoint",
			endpoint:    "https://indexer.feralfile.com/graphql",
			expectError: false,
		},
		{
			name:        "valid endpoint with path",
			endpoint:    "https://indexer.feralfile.com/api/graphql",
			expectError: false,
		},
		{
			name:        "invalid host",
			endpoint:    "https://invalid-host.com/graphql",
			expectError: true,
			errorMsg:    "invalid endpoint",
		},
		{
			name:        "invalid URL",
			endpoint:    "not-a-url",
			expectError: true,
			errorMsg:    "invalid endpoint",
		},
		{
			name:        "empty endpoint",
			endpoint:    "",
			expectError: true,
			errorMsg:    "invalid endpoint",
		},
		{
			name:        "HTTP instead of HTTPS",
			endpoint:    "http://indexer.feralfile.com/graphql",
			expectError: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts := setup(t)
			defer ts.teardown()

			params := map[string]string{"limit": "50"}

			if tt.expectError {
				// Test invalid endpoints
				result, err := ts.client.QueryTokens(ts.ctx, tt.endpoint, params)
				assert.Error(t, err)
				assert.Nil(t, result)
				assert.Contains(t, err.Error(), tt.errorMsg)
			} else {
				// For valid endpoints, set up mocks first
				ts.mockJSON.EXPECT().
					Marshal(gomock.Any()).
					Return([]byte(`{"query":"test"}`), nil)

				ts.mockHTTP.EXPECT().
					Post(tt.endpoint, "application/json", gomock.Any()).
					Return(createMockResponse(http.StatusOK, `{"data":{"tokens":[]}}`), nil)

				ts.mockIO.EXPECT().
					ReadAll(gomock.Any()).
					Return([]byte(`{"data":{"tokens":[]}}`), nil)

				ts.mockJSON.EXPECT().
					Unmarshal([]byte(`{"data":{"tokens":[]}}`), gomock.Any()).
					DoAndReturn(func(data []byte, v interface{}) error {
						resp := v.(*ffindexer.GraphQLResponse)
						resp.Data.Tokens = []ffindexer.Token{}
						return nil
					})

				// Test valid endpoints
				result, err := ts.client.QueryTokens(ts.ctx, tt.endpoint, params)
				assert.NoError(t, err)
				assert.NotNil(t, result)
			}
		})
	}
}

func TestFFIndexer_FormatGraphQLParam(t *testing.T) {
	tests := []struct {
		name     string
		key      string
		value    string
		expected string
	}{
		{
			name:     "simple string parameter",
			key:      "limit",
			value:    "50",
			expected: `limit: "50"`,
		},
		{
			name:     "array parameter",
			key:      "blockchains",
			value:    "ethereum,tezos,bitmark",
			expected: `blockchains: ["ethereum", "tezos", "bitmark"]`,
		},
		{
			name:     "array parameter with spaces",
			key:      "blockchains",
			value:    "ethereum, tezos , bitmark",
			expected: `blockchains: ["ethereum", "tezos", "bitmark"]`,
		},
		{
			name:     "single item array",
			key:      "blockchain",
			value:    "ethereum",
			expected: `blockchain: "ethereum"`,
		},
		{
			name:     "empty value",
			key:      "limit",
			value:    "",
			expected: `limit: ""`,
		},
		{
			name:     "parameter with special characters",
			key:      "query",
			value:    "test query with spaces",
			expected: `query: "test query with spaces"`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts := setup(t)
			defer ts.teardown()

			params := map[string]string{tt.key: tt.value}

			// Expect JSON marshal to verify the formatted parameter
			ts.mockJSON.EXPECT().
				Marshal(gomock.Any()).
				DoAndReturn(func(v interface{}) ([]byte, error) {
					reqBody := v.(map[string]any)
					query := reqBody["query"].(string)
					assert.Contains(t, query, tt.expected)
					return []byte(`{"query":"test"}`), nil
				})

			// Expect HTTP POST request
			ts.mockHTTP.EXPECT().
				Post("https://indexer.feralfile.com/graphql", "application/json", gomock.Any()).
				Return(createMockResponse(http.StatusOK, `{"data":{"tokens":[]}}`), nil)

			// Expect IO ReadAll
			ts.mockIO.EXPECT().
				ReadAll(gomock.Any()).
				Return([]byte(`{"data":{"tokens":[]}}`), nil)

			// Expect JSON unmarshal
			ts.mockJSON.EXPECT().
				Unmarshal([]byte(`{"data":{"tokens":[]}}`), gomock.Any()).
				DoAndReturn(func(data []byte, v interface{}) error {
					resp := v.(*ffindexer.GraphQLResponse)
					resp.Data.Tokens = []ffindexer.Token{}
					return nil
				})

			// Test
			result, err := ts.client.QueryTokens(ts.ctx, "https://indexer.feralfile.com/graphql", params)
			assert.NoError(t, err)
			assert.NotNil(t, result)
		})
	}
}

func TestFFIndexer_ExecGraphQLQuery(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	endpoint := "https://indexer.feralfile.com/graphql"
	responseJSON := `{"data":{"tokens":[{"id":"token1"}]}}`

	// Expect JSON marshal
	ts.mockJSON.EXPECT().
		Marshal(gomock.Any()).
		DoAndReturn(func(v interface{}) ([]byte, error) {
			reqBody := v.(map[string]any)
			// Verify it's a proper GraphQL query structure
			assert.Contains(t, reqBody, "query")
			return []byte(`{"query":"test"}`), nil
		})

	// Expect HTTP POST request
	ts.mockHTTP.EXPECT().
		Post(endpoint, "application/json", gomock.Any()).
		Return(createMockResponse(http.StatusOK, responseJSON), nil)

	// Expect IO ReadAll
	ts.mockIO.EXPECT().
		ReadAll(gomock.Any()).
		Return([]byte(responseJSON), nil)

	// Expect JSON unmarshal
	ts.mockJSON.EXPECT().
		Unmarshal([]byte(responseJSON), gomock.Any()).
		DoAndReturn(func(data []byte, v interface{}) error {
			resp := v.(*ffindexer.GraphQLResponse)
			resp.Data.Tokens = []ffindexer.Token{
				{ID: "token1"},
			}
			return nil
		})

	// Test
	result, err := ts.client.QueryTokens(ts.ctx, endpoint, map[string]string{})
	assert.NoError(t, err)
	assert.NotNil(t, result)
	assert.Len(t, result, 1)
	assert.Equal(t, "token1", result[0].ID)
}

func TestFFIndexer_QueryTokens_HTTPStatusCodeError(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	endpoint := "https://indexer.feralfile.com/graphql"
	params := map[string]string{"limit": "50"}

	// Expect JSON marshal
	ts.mockJSON.EXPECT().
		Marshal(gomock.Any()).
		Return([]byte(`{"query":"test"}`), nil)

	// Expect HTTP POST request to return error status
	ts.mockHTTP.EXPECT().
		Post(endpoint, "application/json", gomock.Any()).
		Return(createMockResponse(http.StatusInternalServerError, "Internal Server Error"), nil)

	// Expect IO ReadAll
	ts.mockIO.EXPECT().
		ReadAll(gomock.Any()).
		Return([]byte("Internal Server Error"), nil)

	// Expect JSON unmarshal to fail due to invalid JSON
	ts.mockJSON.EXPECT().
		Unmarshal([]byte("Internal Server Error"), gomock.Any()).
		Return(errors.New("invalid character 'I' looking for beginning of value"))

	// Test
	result, err := ts.client.QueryTokens(ts.ctx, endpoint, params)
	assert.Error(t, err)
	assert.Nil(t, result)
	assert.Contains(t, err.Error(), "invalid character")
}
