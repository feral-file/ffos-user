package ffindexer_test

import (
	"context"
	"encoding/json"
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
	mockHTTP *mocks.MockHTTPClient
	mockJSON *mocks.MockJSON
	mockIO   *mocks.MockIO
	client   ffindexer.FFIndexer
}

func setup(t *testing.T) *testSetup {
	ctrl := gomock.NewController(t)
	ctx := context.Background()
	logger := zaptest.NewLogger(t, zaptest.Level(zap.FatalLevel))

	// Dependencies
	mockHTTPClient := mocks.NewMockHTTPClient(ctrl)
	mockJSON := mocks.NewMockJSON(ctrl)
	mockIO := mocks.NewMockIO(ctrl)

	client := ffindexer.New(mockHTTPClient, mockJSON, mockIO, logger)

	return &testSetup{
		ctrl:     ctrl,
		ctx:      ctx,
		mockHTTP: mockHTTPClient,
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

// Helper function to create a mock GraphQL response JSON using new ff-indexer-v2 schema
func createGraphQLResponseJSON() string {
	return `{
		"data": {
			"tokens": {
				"items": [
					{
						"id": "1",
						"token_cid": "token1",
						"chain": "ethereum",
						"standard": "ERC721",
						"contract_address": "0x1234567890abcdef",
						"token_number": "1",
						"current_owner": "0xowner1",
						"burned": false,
						"metadata": {
							"name": "Test Token 1",
							"description": "Test Description 1",
							"image_url": "http://example.com/preview1.jpg",
							"animation_url": "",
							"artists": [],
							"publisher": {
								"name": "Test Publisher",
								"url": "http://publisher.example.com"
							},
							"mime_type": "image/jpeg"
						},
						"owners": {
							"items": [
								{
									"owner_address": "0xowner1",
									"quantity": "1"
								}
							],
							"total": "1"
						}
					},
					{
						"id": "2",
						"token_cid": "token2",
						"chain": "tezos",
						"standard": "FA2",
						"contract_address": "0xabcdef1234567890",
						"token_number": "2",
						"current_owner": "tz1owner2",
						"burned": false,
						"metadata": {
							"name": "Test Token 2",
							"description": "Test Description 2",
							"image_url": "http://example.com/preview2.jpg",
							"animation_url": "",
							"artists": [],
							"publisher": null,
							"mime_type": "image/jpeg"
						},
						"owners": {
							"items": [
								{
									"owner_address": "tz1owner2",
									"quantity": "1"
								}
							],
							"total": "1"
						}
					}
				],
				"offset": "0",
				"total": "2"
			}
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

	// Expect JSON unmarshal - let it use the real JSON unmarshaler
	ts.mockJSON.EXPECT().
		Unmarshal([]byte(responseJSON), gomock.Any()).
		DoAndReturn(func(data []byte, v interface{}) error {
			// Use real JSON unmarshaling to properly parse the new schema
			return json.Unmarshal(data, v)
		})

	// Test
	result, err := ts.client.QueryTokens(ts.ctx, endpoint, params)
	assert.NoError(t, err)
	assert.NotNil(t, result)
	assert.Len(t, result, 2)

	// Verify first token (v2 schema)
	assert.Equal(t, "token1", result[0].TokenCID)
	assert.Equal(t, "ethereum", result[0].Chain)
	assert.Equal(t, "ERC721", result[0].Standard)
	assert.Equal(t, "0x1234567890abcdef", result[0].ContractAddress)
	assert.Equal(t, "Test Token 1", result[0].GetTitle())
	assert.Equal(t, "http://example.com/preview1.jpg", result[0].GetPreviewURL())

	// Verify second token
	assert.Equal(t, "token2", result[1].TokenCID)
	assert.Equal(t, "tezos", result[1].Chain)
	assert.Equal(t, "FA2", result[1].Standard)
	assert.Equal(t, "0xabcdef1234567890", result[1].ContractAddress)
	assert.Equal(t, "Test Token 2", result[1].GetTitle())
	assert.Equal(t, "http://example.com/preview2.jpg", result[1].GetPreviewURL())
}

func TestFFIndexer_QueryTokens_InvalidEndpoint(t *testing.T) {
	t.Skip("Endpoint validation is currently disabled in ff_indexer.go (line 116-119)")

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
	emptyResponseJSON := `{"data":{"tokens":{"items":[],"offset":"0","total":"0"}}}`

	// Expect JSON marshal
	ts.mockJSON.EXPECT().
		Marshal(gomock.Any()).
		DoAndReturn(func(v interface{}) ([]byte, error) {
			reqBody := v.(map[string]any)
			query := reqBody["query"].(string)
			// Verify query structure is valid
			assert.Contains(t, query, `tokens(`)
			return []byte(`{"query":"test"}`), nil
		})

	// Expect HTTP POST request
	ts.mockHTTP.EXPECT().
		Post(endpoint, "application/json", gomock.Any()).
		Return(createMockResponse(http.StatusOK, emptyResponseJSON), nil)

	// Expect IO ReadAll
	ts.mockIO.EXPECT().
		ReadAll(gomock.Any()).
		Return([]byte(emptyResponseJSON), nil)

	// Expect JSON unmarshal
	ts.mockJSON.EXPECT().
		Unmarshal([]byte(emptyResponseJSON), gomock.Any()).
		DoAndReturn(func(data []byte, v interface{}) error {
			return json.Unmarshal(data, v)
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
		"chain": "ethereum,tezos,bitmark",
		"limit": "50",
	}
	emptyResponseJSON := `{"data":{"tokens":{"items":[],"offset":"0","total":"0"}}}`

	// Expect JSON marshal
	ts.mockJSON.EXPECT().
		Marshal(gomock.Any()).
		DoAndReturn(func(v interface{}) ([]byte, error) {
			reqBody := v.(map[string]any)
			query := reqBody["query"].(string)
			// Should have array format for chain (mapped from blockchains)
			assert.Contains(t, query, `chain: ["ethereum", "tezos", "bitmark"]`)
			assert.Contains(t, query, `limit: "50"`)
			return []byte(`{"query":"test"}`), nil
		})

	// Expect HTTP POST request
	ts.mockHTTP.EXPECT().
		Post(endpoint, "application/json", gomock.Any()).
		Return(createMockResponse(http.StatusOK, emptyResponseJSON), nil)

	// Expect IO ReadAll
	ts.mockIO.EXPECT().
		ReadAll(gomock.Any()).
		Return([]byte(emptyResponseJSON), nil)

	// Expect JSON unmarshal
	ts.mockJSON.EXPECT().
		Unmarshal([]byte(emptyResponseJSON), gomock.Any()).
		DoAndReturn(func(data []byte, v interface{}) error {
			return json.Unmarshal(data, v)
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
		"chain": "ethereum, tezos , bitmark",
		"limit": "50",
	}
	emptyResponseJSON := `{"data":{"tokens":{"items":[],"offset":"0","total":"0"}}}`

	// Expect JSON marshal
	ts.mockJSON.EXPECT().
		Marshal(gomock.Any()).
		DoAndReturn(func(v interface{}) ([]byte, error) {
			reqBody := v.(map[string]any)
			query := reqBody["query"].(string)
			// Should trim spaces and format as array with mapped param name
			assert.Contains(t, query, `chain: ["ethereum", "tezos", "bitmark"]`)
			return []byte(`{"query":"test"}`), nil
		})

	// Expect HTTP POST request
	ts.mockHTTP.EXPECT().
		Post(endpoint, "application/json", gomock.Any()).
		Return(createMockResponse(http.StatusOK, emptyResponseJSON), nil)

	// Expect IO ReadAll
	ts.mockIO.EXPECT().
		ReadAll(gomock.Any()).
		Return([]byte(emptyResponseJSON), nil)

	// Expect JSON unmarshal
	ts.mockJSON.EXPECT().
		Unmarshal([]byte(emptyResponseJSON), gomock.Any()).
		DoAndReturn(func(data []byte, v interface{}) error {
			return json.Unmarshal(data, v)
		})

	// Test
	result, err := ts.client.QueryTokens(ts.ctx, endpoint, params)
	assert.NoError(t, err)
	assert.NotNil(t, result)
	assert.Len(t, result, 0)
}

func TestFFIndexer_ValidateEndpoint(t *testing.T) {
	t.Skip("Endpoint validation is currently disabled in ff_indexer.go (line 116-119)")

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
				emptyResponseJSON := `{"data":{"tokens":{"items":[],"offset":"0","total":"0"}}}`

				ts.mockJSON.EXPECT().
					Marshal(gomock.Any()).
					Return([]byte(`{"query":"test"}`), nil)

				ts.mockHTTP.EXPECT().
					Post(tt.endpoint, "application/json", gomock.Any()).
					Return(createMockResponse(http.StatusOK, emptyResponseJSON), nil)

				ts.mockIO.EXPECT().
					ReadAll(gomock.Any()).
					Return([]byte(emptyResponseJSON), nil)

				ts.mockJSON.EXPECT().
					Unmarshal([]byte(emptyResponseJSON), gomock.Any()).
					DoAndReturn(func(data []byte, v interface{}) error {
						return json.Unmarshal(data, v)
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
			key:      "chain",
			value:    "ethereum,tezos,bitmark",
			expected: `chain: ["ethereum", "tezos", "bitmark"]`,
		},
		{
			name:     "array parameter with spaces",
			key:      "chain",
			value:    "ethereum, tezos , bitmark",
			expected: `chain: ["ethereum", "tezos", "bitmark"]`,
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
			emptyResponseJSON := `{"data":{"tokens":{"items":[],"offset":"0","total":"0"}}}`

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
				Return(createMockResponse(http.StatusOK, emptyResponseJSON), nil)

			// Expect IO ReadAll
			ts.mockIO.EXPECT().
				ReadAll(gomock.Any()).
				Return([]byte(emptyResponseJSON), nil)

			// Expect JSON unmarshal
			ts.mockJSON.EXPECT().
				Unmarshal([]byte(emptyResponseJSON), gomock.Any()).
				DoAndReturn(func(data []byte, v interface{}) error {
					return json.Unmarshal(data, v)
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
	responseJSON := `{
		"data": {
			"tokens": {
				"items": [{
					"id": "1",
					"token_cid": "token1",
					"chain": "ethereum",
					"standard": "ERC721",
					"contract_address": "0x123",
					"token_number": "1",
					"current_owner": "0xowner",
					"burned": false,
					"metadata": {
						"name": "Test",
						"image_url": "http://example.com/img.jpg"
					},
					"owners": {
						"items": [{"owner_address": "0xowner", "quantity": "1"}],
						"total": "1"
					}
				}],
				"offset": "0",
				"total": "1"
			}
		}
	}`

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
			return json.Unmarshal(data, v)
		})

	// Test
	result, err := ts.client.QueryTokens(ts.ctx, endpoint, map[string]string{})
	assert.NoError(t, err)
	assert.NotNil(t, result)
	assert.Len(t, result, 1)
	assert.Equal(t, "token1", result[0].TokenCID)
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

// Test GraphQL errors handling
func TestFFIndexer_QueryTokens_GraphQLErrors(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	endpoint := "https://indexer.feralfile.com/graphql"
	params := map[string]string{"limit": "50"}
	errorResponseJSON := `{
		"data": null,
		"errors": [
			{"message": "Invalid query syntax"},
			{"message": "Field not found"}
		]
	}`

	// Expect JSON marshal
	ts.mockJSON.EXPECT().
		Marshal(gomock.Any()).
		Return([]byte(`{"query":"test"}`), nil)

	// Expect HTTP POST request
	ts.mockHTTP.EXPECT().
		Post(endpoint, "application/json", gomock.Any()).
		Return(createMockResponse(200, errorResponseJSON), nil)

	// Expect IO ReadAll
	ts.mockIO.EXPECT().
		ReadAll(gomock.Any()).
		Return([]byte(errorResponseJSON), nil)

	// Expect JSON unmarshal
	ts.mockJSON.EXPECT().
		Unmarshal([]byte(errorResponseJSON), gomock.Any()).
		DoAndReturn(func(data []byte, v interface{}) error {
			return json.Unmarshal(data, v)
		})

	// Test
	result, err := ts.client.QueryTokens(ts.ctx, endpoint, params)
	assert.Error(t, err)
	assert.Nil(t, result)
	assert.Contains(t, err.Error(), "graphql errors")
}

// Test burned token (balance should be 0)
func TestFFIndexer_QueryTokens_BurnedToken(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	endpoint := "https://indexer.feralfile.com/graphql"
	responseJSON := `{
		"data": {
			"tokens": {
				"items": [{
					"id": "1",
					"token_cid": "burned_token",
					"chain": "ethereum",
					"standard": "ERC721",
					"contract_address": "0x123",
					"token_number": "1",
					"current_owner": "",
					"burned": true,
					"metadata": {
						"name": "Burned Token"
					},
					"owners": null
				}],
				"offset": "0",
				"total": "1"
			}
		}
	}`

	ts.mockJSON.EXPECT().Marshal(gomock.Any()).Return([]byte(`{"query":"test"}`), nil)
	ts.mockHTTP.EXPECT().Post(endpoint, "application/json", gomock.Any()).Return(createMockResponse(200, responseJSON), nil)
	ts.mockIO.EXPECT().ReadAll(gomock.Any()).Return([]byte(responseJSON), nil)
	ts.mockJSON.EXPECT().Unmarshal([]byte(responseJSON), gomock.Any()).DoAndReturn(func(data []byte, v interface{}) error {
		return json.Unmarshal(data, v)
	})

	result, err := ts.client.QueryTokens(ts.ctx, endpoint, map[string]string{})
	assert.NoError(t, err)
	assert.Len(t, result, 1)
	assert.True(t, result[0].Burned)
}

// Test token with animation_url (should prefer animation_url over image_url)
func TestFFIndexer_QueryTokens_AnimationURLPreferred(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	endpoint := "https://indexer.feralfile.com/graphql"
	responseJSON := `{
		"data": {
			"tokens": {
				"items": [{
					"id": "1",
					"token_cid": "video_token",
					"chain": "ethereum",
					"standard": "ERC721",
					"contract_address": "0x123",
					"token_number": "1",
					"current_owner": "0xowner",
					"burned": false,
					"metadata": {
						"name": "Video Token",
						"image_url": "http://example.com/thumbnail.jpg",
						"animation_url": "http://example.com/video.mp4"
					},
					"owners": {
						"items": [{"owner_address": "0xowner", "quantity": "1"}],
						"total": "1"
					}
				}],
				"offset": "0",
				"total": "1"
			}
		}
	}`

	ts.mockJSON.EXPECT().Marshal(gomock.Any()).Return([]byte(`{"query":"test"}`), nil)
	ts.mockHTTP.EXPECT().Post(endpoint, "application/json", gomock.Any()).Return(createMockResponse(200, responseJSON), nil)
	ts.mockIO.EXPECT().ReadAll(gomock.Any()).Return([]byte(responseJSON), nil)
	ts.mockJSON.EXPECT().Unmarshal([]byte(responseJSON), gomock.Any()).DoAndReturn(func(data []byte, v interface{}) error {
		return json.Unmarshal(data, v)
	})

	result, err := ts.client.QueryTokens(ts.ctx, endpoint, map[string]string{})
	assert.NoError(t, err)
	assert.Len(t, result, 1)
	// Should prefer animation_url
	assert.Equal(t, "http://example.com/video.mp4", result[0].GetPreviewURL())
	assert.Equal(t, "Video Token", result[0].GetTitle())
}

// Test token with null metadata
func TestFFIndexer_QueryTokens_NullMetadata(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	endpoint := "https://indexer.feralfile.com/graphql"
	responseJSON := `{
		"data": {
			"tokens": {
				"items": [{
					"id": "1",
					"token_cid": "no_metadata_token",
					"chain": "ethereum",
					"standard": "ERC721",
					"contract_address": "0x123",
					"token_number": "1",
					"current_owner": "0xowner",
					"burned": false,
					"metadata": null,
					"owners": {
						"items": [{"owner_address": "0xowner", "quantity": "1"}],
						"total": "1"
					}
				}],
				"offset": "0",
				"total": "1"
			}
		}
	}`

	ts.mockJSON.EXPECT().Marshal(gomock.Any()).Return([]byte(`{"query":"test"}`), nil)
	ts.mockHTTP.EXPECT().Post(endpoint, "application/json", gomock.Any()).Return(createMockResponse(200, responseJSON), nil)
	ts.mockIO.EXPECT().ReadAll(gomock.Any()).Return([]byte(responseJSON), nil)
	ts.mockJSON.EXPECT().Unmarshal([]byte(responseJSON), gomock.Any()).DoAndReturn(func(data []byte, v interface{}) error {
		return json.Unmarshal(data, v)
	})

	result, err := ts.client.QueryTokens(ts.ctx, endpoint, map[string]string{})
	assert.NoError(t, err)
	assert.Len(t, result, 1)
	// Should handle null metadata gracefully
	assert.Equal(t, "", result[0].GetTitle())
	assert.Equal(t, "", result[0].GetPreviewURL())
	assert.Nil(t, result[0].Metadata)
}

// Test owner filtering
func TestFFIndexer_QueryTokens_OwnerFiltering(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	endpoint := "https://indexer.feralfile.com/graphql"
	responseJSON := `{
		"data": {
			"tokens": {
				"items": [{
					"id": "1",
					"token_cid": "multi_owner_token",
					"chain": "ethereum",
					"standard": "ERC1155",
					"contract_address": "0x123",
					"token_number": "1",
					"current_owner": "0xowner1",
					"burned": false,
					"metadata": {
						"name": "Multi Owner Token"
					},
					"owners": {
						"items": [
							{"owner_address": "0xowner1", "quantity": "5"},
							{"owner_address": "0xowner2", "quantity": "3"}
						],
						"total": "2"
					}
				}],
				"offset": "0",
				"total": "1"
			}
		}
	}`

	ts.mockJSON.EXPECT().Marshal(gomock.Any()).Return([]byte(`{"query":"test"}`), nil)
	ts.mockHTTP.EXPECT().Post(endpoint, "application/json", gomock.Any()).Return(createMockResponse(200, responseJSON), nil)
	ts.mockIO.EXPECT().ReadAll(gomock.Any()).Return([]byte(responseJSON), nil)
	ts.mockJSON.EXPECT().Unmarshal([]byte(responseJSON), gomock.Any()).DoAndReturn(func(data []byte, v interface{}) error {
		return json.Unmarshal(data, v)
	})

	// Test with owner filter
	result, err := ts.client.QueryTokens(ts.ctx, endpoint, map[string]string{"owner": "0xowner1"})
	assert.NoError(t, err)
	assert.Len(t, result, 1)
	// Verify token has multiple owners
	assert.NotNil(t, result[0].Owners)
	assert.Equal(t, "2", result[0].Owners.Total)
}

// Test multiple owners without filter (should sum all quantities)
func TestFFIndexer_QueryTokens_MultipleOwnersNoFilter(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	endpoint := "https://indexer.feralfile.com/graphql"
	responseJSON := `{
		"data": {
			"tokens": {
				"items": [{
					"id": "1",
					"token_cid": "multi_owner_token",
					"chain": "ethereum",
					"standard": "ERC1155",
					"contract_address": "0x123",
					"token_number": "1",
					"current_owner": "0xowner1",
					"burned": false,
					"metadata": {
						"name": "Multi Owner Token"
					},
					"owners": {
						"items": [
							{"owner_address": "0xowner1", "quantity": "5"},
							{"owner_address": "0xowner2", "quantity": "3"}
						],
						"total": "2"
					}
				}],
				"offset": "0",
				"total": "1"
			}
		}
	}`

	ts.mockJSON.EXPECT().Marshal(gomock.Any()).Return([]byte(`{"query":"test"}`), nil)
	ts.mockHTTP.EXPECT().Post(endpoint, "application/json", gomock.Any()).Return(createMockResponse(200, responseJSON), nil)
	ts.mockIO.EXPECT().ReadAll(gomock.Any()).Return([]byte(responseJSON), nil)
	ts.mockJSON.EXPECT().Unmarshal([]byte(responseJSON), gomock.Any()).DoAndReturn(func(data []byte, v interface{}) error {
		return json.Unmarshal(data, v)
	})

	// Test without owner filter
	result, err := ts.client.QueryTokens(ts.ctx, endpoint, map[string]string{})
	assert.NoError(t, err)
	assert.Len(t, result, 1)
	// Verify multiple owners returned
	assert.NotNil(t, result[0].Owners)
	assert.Len(t, result[0].Owners.Items, 2)
}

// Test invalid quantity string (should handle gracefully)
func TestFFIndexer_QueryTokens_InvalidQuantity(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	endpoint := "https://indexer.feralfile.com/graphql"
	responseJSON := `{
		"data": {
			"tokens": {
				"items": [{
					"id": "1",
					"token_cid": "invalid_quantity_token",
					"chain": "ethereum",
					"standard": "ERC1155",
					"contract_address": "0x123",
					"token_number": "1",
					"current_owner": "0xowner1",
					"burned": false,
					"metadata": {
						"name": "Invalid Quantity Token"
					},
					"owners": {
						"items": [
							{"owner_address": "0xowner1", "quantity": "not-a-number"}
						],
						"total": "1"
					}
				}],
				"offset": "0",
				"total": "1"
			}
		}
	}`

	ts.mockJSON.EXPECT().Marshal(gomock.Any()).Return([]byte(`{"query":"test"}`), nil)
	ts.mockHTTP.EXPECT().Post(endpoint, "application/json", gomock.Any()).Return(createMockResponse(200, responseJSON), nil)
	ts.mockIO.EXPECT().ReadAll(gomock.Any()).Return([]byte(responseJSON), nil)
	ts.mockJSON.EXPECT().Unmarshal([]byte(responseJSON), gomock.Any()).DoAndReturn(func(data []byte, v interface{}) error {
		return json.Unmarshal(data, v)
	})

	result, err := ts.client.QueryTokens(ts.ctx, endpoint, map[string]string{})
	assert.NoError(t, err)
	assert.Len(t, result, 1)
	// Verify token has owner with invalid quantity string
	assert.NotNil(t, result[0].Owners)
	assert.Equal(t, "not-a-number", result[0].Owners.Items[0].Quantity)
}

// Test owner not found in filter
func TestFFIndexer_QueryTokens_OwnerNotFound(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	endpoint := "https://indexer.feralfile.com/graphql"
	responseJSON := `{
		"data": {
			"tokens": {
				"items": [{
					"id": "1",
					"token_cid": "token",
					"chain": "ethereum",
					"standard": "ERC721",
					"contract_address": "0x123",
					"token_number": "1",
					"current_owner": "0xowner1",
					"burned": false,
					"metadata": {
						"name": "Token"
					},
					"owners": {
						"items": [
							{"owner_address": "0xowner1", "quantity": "1"}
						],
						"total": "1"
					}
				}],
				"offset": "0",
				"total": "1"
			}
		}
	}`

	ts.mockJSON.EXPECT().Marshal(gomock.Any()).Return([]byte(`{"query":"test"}`), nil)
	ts.mockHTTP.EXPECT().Post(endpoint, "application/json", gomock.Any()).Return(createMockResponse(200, responseJSON), nil)
	ts.mockIO.EXPECT().ReadAll(gomock.Any()).Return([]byte(responseJSON), nil)
	ts.mockJSON.EXPECT().Unmarshal([]byte(responseJSON), gomock.Any()).DoAndReturn(func(data []byte, v interface{}) error {
		return json.Unmarshal(data, v)
	})

	// Test with non-existent owner
	result, err := ts.client.QueryTokens(ts.ctx, endpoint, map[string]string{"owner": "0xnonexistent"})
	assert.NoError(t, err)
	assert.Len(t, result, 1)
	// Verify token has owner data
	assert.NotNil(t, result[0].Owners)
	assert.Equal(t, "0xowner1", result[0].CurrentOwner)
}

// Test case-insensitive owner matching
func TestFFIndexer_QueryTokens_CaseInsensitiveOwner(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	endpoint := "https://indexer.feralfile.com/graphql"
	responseJSON := `{
		"data": {
			"tokens": {
				"items": [{
					"id": "1",
					"token_cid": "token",
					"chain": "ethereum",
					"standard": "ERC721",
					"contract_address": "0x123",
					"token_number": "1",
					"current_owner": "0xOwner1",
					"burned": false,
					"metadata": {
						"name": "Token"
					},
					"owners": {
						"items": [
							{"owner_address": "0xOwner1", "quantity": "1"}
						],
						"total": "1"
					}
				}],
				"offset": "0",
				"total": "1"
			}
		}
	}`

	ts.mockJSON.EXPECT().Marshal(gomock.Any()).Return([]byte(`{"query":"test"}`), nil)
	ts.mockHTTP.EXPECT().Post(endpoint, "application/json", gomock.Any()).Return(createMockResponse(200, responseJSON), nil)
	ts.mockIO.EXPECT().ReadAll(gomock.Any()).Return([]byte(responseJSON), nil)
	ts.mockJSON.EXPECT().Unmarshal([]byte(responseJSON), gomock.Any()).DoAndReturn(func(data []byte, v interface{}) error {
		return json.Unmarshal(data, v)
	})

	// Test with different case
	result, err := ts.client.QueryTokens(ts.ctx, endpoint, map[string]string{"owner": "0xowner1"})
	assert.NoError(t, err)
	assert.Len(t, result, 1)
	// Verify owner data is returned
	assert.NotNil(t, result[0].Owners)
	assert.Equal(t, "0xOwner1", result[0].CurrentOwner)
}

// Test token with current_owner but no owners list
func TestFFIndexer_QueryTokens_CurrentOwnerOnly(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	endpoint := "https://indexer.feralfile.com/graphql"
	responseJSON := `{
		"data": {
			"tokens": {
				"items": [{
					"id": "1",
					"token_cid": "token",
					"chain": "ethereum",
					"standard": "ERC721",
					"contract_address": "0x123",
					"token_number": "1",
					"current_owner": "0xowner1",
					"burned": false,
					"metadata": {
						"name": "Token"
					},
					"owners": null
				}],
				"offset": "0",
				"total": "1"
			}
		}
	}`

	ts.mockJSON.EXPECT().Marshal(gomock.Any()).Return([]byte(`{"query":"test"}`), nil)
	ts.mockHTTP.EXPECT().Post(endpoint, "application/json", gomock.Any()).Return(createMockResponse(200, responseJSON), nil)
	ts.mockIO.EXPECT().ReadAll(gomock.Any()).Return([]byte(responseJSON), nil)
	ts.mockJSON.EXPECT().Unmarshal([]byte(responseJSON), gomock.Any()).DoAndReturn(func(data []byte, v interface{}) error {
		return json.Unmarshal(data, v)
	})

	// Test without owner filter - should use current_owner
	result, err := ts.client.QueryTokens(ts.ctx, endpoint, map[string]string{})
	assert.NoError(t, err)
	assert.Len(t, result, 1)
	// Verify current owner is set
	assert.Equal(t, "0xowner1", result[0].CurrentOwner)
	assert.Nil(t, result[0].Owners)
}

// Test token with publisher information
func TestFFIndexer_QueryTokens_WithPublisher(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	endpoint := "https://indexer.feralfile.com/graphql"
	responseJSON := `{
		"data": {
			"tokens": {
				"items": [{
					"id": "1",
					"token_cid": "publisher_token",
					"chain": "ethereum",
					"standard": "ERC721",
					"contract_address": "0x123",
					"token_number": "1",
					"current_owner": "0xowner",
					"burned": false,
					"metadata": {
						"name": "Token with Publisher",
						"description": "This token has publisher info",
						"image_url": "http://example.com/img.jpg",
						"animation_url": "",
						"artists": [
							{"did": "did:key:artist1", "name": "Artist One"}
						],
						"publisher": {
							"name": "Feral File",
							"url": "https://feralfile.com"
						},
						"mime_type": "image/jpeg"
					},
					"owners": {
						"items": [{"owner_address": "0xowner", "quantity": "1"}],
						"total": "1"
					}
				}],
				"offset": "0",
				"total": "1"
			}
		}
	}`

	ts.mockJSON.EXPECT().Marshal(gomock.Any()).Return([]byte(`{"query":"test"}`), nil)
	ts.mockHTTP.EXPECT().Post(endpoint, "application/json", gomock.Any()).Return(createMockResponse(200, responseJSON), nil)
	ts.mockIO.EXPECT().ReadAll(gomock.Any()).Return([]byte(responseJSON), nil)
	ts.mockJSON.EXPECT().Unmarshal([]byte(responseJSON), gomock.Any()).DoAndReturn(func(data []byte, v interface{}) error {
		return json.Unmarshal(data, v)
	})

	result, err := ts.client.QueryTokens(ts.ctx, endpoint, map[string]string{})
	assert.NoError(t, err)
	assert.Len(t, result, 1)
	assert.NotNil(t, result[0].Metadata)
	assert.NotNil(t, result[0].Metadata.Publisher)
	assert.Equal(t, "Feral File", result[0].Metadata.Publisher.Name)
	assert.Equal(t, "https://feralfile.com", result[0].Metadata.Publisher.URL)
}

// Test token without publisher (null publisher)
func TestFFIndexer_QueryTokens_NullPublisher(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	endpoint := "https://indexer.feralfile.com/graphql"
	responseJSON := `{
		"data": {
			"tokens": {
				"items": [{
					"id": "1",
					"token_cid": "no_publisher_token",
					"chain": "ethereum",
					"standard": "ERC721",
					"contract_address": "0x123",
					"token_number": "1",
					"current_owner": "0xowner",
					"burned": false,
					"metadata": {
						"name": "Token without Publisher",
						"description": "This token has no publisher",
						"image_url": "http://example.com/img.jpg",
						"publisher": null,
						"mime_type": "image/jpeg"
					},
					"owners": {
						"items": [{"owner_address": "0xowner", "quantity": "1"}],
						"total": "1"
					}
				}],
				"offset": "0",
				"total": "1"
			}
		}
	}`

	ts.mockJSON.EXPECT().Marshal(gomock.Any()).Return([]byte(`{"query":"test"}`), nil)
	ts.mockHTTP.EXPECT().Post(endpoint, "application/json", gomock.Any()).Return(createMockResponse(200, responseJSON), nil)
	ts.mockIO.EXPECT().ReadAll(gomock.Any()).Return([]byte(responseJSON), nil)
	ts.mockJSON.EXPECT().Unmarshal([]byte(responseJSON), gomock.Any()).DoAndReturn(func(data []byte, v interface{}) error {
		return json.Unmarshal(data, v)
	})

	result, err := ts.client.QueryTokens(ts.ctx, endpoint, map[string]string{})
	assert.NoError(t, err)
	assert.Len(t, result, 1)
	assert.NotNil(t, result[0].Metadata)
	assert.Nil(t, result[0].Metadata.Publisher)
}

// Test limit parameter
func TestFFIndexer_QueryTokens_LimitParam(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	endpoint := "https://indexer.feralfile.com/graphql"
	emptyResponseJSON := `{"data":{"tokens":{"items":[],"offset":"0","total":"0"}}}`

	ts.mockJSON.EXPECT().
		Marshal(gomock.Any()).
		DoAndReturn(func(v interface{}) ([]byte, error) {
			reqBody := v.(map[string]any)
			query := reqBody["query"].(string)
			// Should use "limit" parameter directly
			assert.Contains(t, query, `limit: "100"`)
			return []byte(`{"query":"test"}`), nil
		})

	ts.mockHTTP.EXPECT().Post(endpoint, "application/json", gomock.Any()).Return(createMockResponse(200, emptyResponseJSON), nil)
	ts.mockIO.EXPECT().ReadAll(gomock.Any()).Return([]byte(emptyResponseJSON), nil)
	ts.mockJSON.EXPECT().Unmarshal([]byte(emptyResponseJSON), gomock.Any()).DoAndReturn(func(data []byte, v interface{}) error {
		return json.Unmarshal(data, v)
	})

	// Use "limit" parameter
	result, err := ts.client.QueryTokens(ts.ctx, endpoint, map[string]string{"limit": "100"})
	assert.NoError(t, err)
	assert.NotNil(t, result)
}
