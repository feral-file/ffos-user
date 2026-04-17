package cdp_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/feral-file/ffos-user/components/feral-controld/cdp"
	"github.com/feral-file/ffos-user/components/feral-controld/mocks"
	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"

	"github.com/golang/mock/gomock"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest"
)

type testSetup struct {
	ctrl       *gomock.Controller
	ctx        context.Context
	mockDialer *mocks.MockWebSocketDialer
	mockConn   *mocks.MockWebSocketConn
	mockIO     *mocks.MockIO
	mockJSON   *mocks.MockJSON
	mockHTTP   *mocks.MockHTTPClient
	client     cdp.CDP
}

func setup(t *testing.T) *testSetup {
	ctrl := gomock.NewController(t)
	logger := zaptest.NewLogger(t, zaptest.Level(zap.FatalLevel))
	ctx := context.Background()

	// Dependencies
	mockDialer := mocks.NewMockWebSocketDialer(ctrl)
	mockConn := mocks.NewMockWebSocketConn(ctrl)
	mockIO := mocks.NewMockIO(ctrl)
	mockJSON := mocks.NewMockJSON(ctrl)
	mockHTTPClient := mocks.NewMockHTTPClient(ctrl)

	client := cdp.New("http://localhost:9222", mockDialer, mockIO, mockJSON, mockHTTPClient, logger)

	return &testSetup{
		ctrl:       ctrl,
		ctx:        ctx,
		mockDialer: mockDialer,
		mockConn:   mockConn,
		mockIO:     mockIO,
		mockJSON:   mockJSON,
		mockHTTP:   mockHTTPClient,
		client:     client,
	}
}

func (ts *testSetup) teardown() {
	ts.ctrl.Finish()
}

func TestClient_Init_Success(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	// Mock HTTP response for /json endpoint
	const targetType = "page"
	const targetTitle = "Test Page"
	const targetWebSocketDebuggerURL = "ws://localhost:9222/devtools/page/123"
	responseBody := fmt.Sprintf(`[{"type":"%s","title":"%s","webSocketDebuggerUrl":"%s"}]`, targetType, targetTitle, targetWebSocketDebuggerURL)
	responseBodyBytes := []byte(responseBody)
	mockResponse := &http.Response{
		StatusCode: 200,
		Body:       io.NopCloser(strings.NewReader(responseBody)),
	}

	// Expect http.Do to return mock response
	ts.mockHTTP.EXPECT().
		Do(gomock.Any()).
		Return(mockResponse, nil).
		Times(1)

	// Expect io.ReadAll to return response body
	ts.mockIO.EXPECT().
		ReadAll(mockResponse.Body).
		Return(responseBodyBytes, nil).
		Times(1)

	// Expect json.Unmarshal to unmarshal response body
	ts.mockJSON.EXPECT().
		Unmarshal(responseBodyBytes, gomock.Any()).
		DoAndReturn(func(data []byte, v interface{}) error {
			targets := v.(*[]struct {
				Type                 string `json:"type"`
				Title                string `json:"title"`
				WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
			})
			*targets = []struct {
				Type                 string `json:"type"`
				Title                string `json:"title"`
				WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
			}{
				{
					Type:                 targetType,
					Title:                targetTitle,
					WebSocketDebuggerURL: targetWebSocketDebuggerURL,
				},
			}
			return nil
		}).
		Times(1)

	// Expect dialer to dial and return OK response
	ts.mockDialer.EXPECT().
		DialContext(ts.ctx, targetWebSocketDebuggerURL, nil).
		Return(ts.mockConn, nil, nil).
		Times(1)

	// Expect conn to be closed properly
	ts.mockConn.EXPECT().
		Close().
		Return(nil).
		AnyTimes()

	// Execute the method under test
	err := ts.client.Init(ts.ctx)
	assert.NoError(t, err, "expected no error, got %v", err)
	assert.True(t, ts.client.Initialized(), "expected client to be initialized")

	// Properly close the client to prevent goroutine leaks
	ts.client.Close()
}

func TestClient_Init_Error(t *testing.T) {
	tests := []struct {
		name      string
		setupFunc func(*testSetup)
		wantErr   string
	}{
		{
			name: "already initialized error",
			setupFunc: func(ts *testSetup) {
				// First initialize the client successfully
				responseBody := "fake response body"
				mockResponse := &http.Response{
					StatusCode: 200,
					Body:       io.NopCloser(strings.NewReader(responseBody)),
				}

				// Expect http.Do to return mock response
				ts.mockHTTP.EXPECT().
					Do(gomock.Any()).
					Return(mockResponse, nil).
					Times(1)

				// Expect io.ReadAll to return response body
				ts.mockIO.EXPECT().
					ReadAll(gomock.Any()).
					Return([]byte(responseBody), nil).
					Times(1)

				// Expect json.Unmarshal to unmarshal response body
				ts.mockJSON.EXPECT().
					Unmarshal(gomock.Any(), gomock.Any()).
					DoAndReturn(func(data []byte, v interface{}) error {
						targets := v.(*[]struct {
							Type                 string `json:"type"`
							Title                string `json:"title"`
							WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
						})
						*targets = []struct {
							Type                 string `json:"type"`
							Title                string `json:"title"`
							WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
						}{
							{Type: "page", Title: "Test Page", WebSocketDebuggerURL: "ws://localhost:9222/devtools/page/123"},
						}
						return nil
					}).
					Times(1)

				// Expect dialer to dial and return OK response
				ts.mockDialer.EXPECT().
					DialContext(ts.ctx, gomock.Any(), nil).
					Return(ts.mockConn, nil, nil).
					Times(1)

				// Expect conn to be closed properly
				ts.mockConn.EXPECT().
					Close().
					Return(nil).
					AnyTimes()

				// Initialize once
				err := ts.client.Init(ts.ctx)
				assert.NoError(t, err)
			},
			wantErr: "already initialized",
		},
		{
			name: "HTTP GET error",
			setupFunc: func(ts *testSetup) {
				// Expect http.Do to return error
				ts.mockHTTP.EXPECT().
					Do(gomock.Any()).
					Return(nil, fmt.Errorf("connection refused")).
					Times(1)
			},
			wantErr: "failed to fetch debug targets",
		},
		{
			name: "IO ReadAll error",
			setupFunc: func(ts *testSetup) {
				mockResponse := &http.Response{
					StatusCode: 200,
					Body:       io.NopCloser(strings.NewReader("")),
				}

				// Expect http.Do to return mock response
				ts.mockHTTP.EXPECT().
					Do(gomock.Any()).
					Return(mockResponse, nil).
					Times(1)

				// Expect io.ReadAll to return error
				ts.mockIO.EXPECT().
					ReadAll(gomock.Any()).
					Return(nil, fmt.Errorf("read error")).
					Times(1)
			},
			wantErr: "failed to read targets",
		},
		{
			name: "JSON unmarshal error",
			setupFunc: func(ts *testSetup) {
				responseBody := "invalid json"
				mockResponse := &http.Response{
					StatusCode: 200,
					Body:       io.NopCloser(strings.NewReader(responseBody)),
				}

				// Expect http.Do to return mock response
				ts.mockHTTP.EXPECT().
					Do(gomock.Any()).
					Return(mockResponse, nil).
					Times(1)

				// Expect io.ReadAll to return response body
				ts.mockIO.EXPECT().
					ReadAll(gomock.Any()).
					Return([]byte(responseBody), nil).
					Times(1)

				// Expect json.Unmarshal to return error
				ts.mockJSON.EXPECT().
					Unmarshal(gomock.Any(), gomock.Any()).
					Return(fmt.Errorf("invalid character")).
					Times(1)
			},
			wantErr: "invalid targets format",
		},
		{
			name: "no page target found",
			setupFunc: func(ts *testSetup) {
				responseBody := "fake response body"
				mockResponse := &http.Response{
					StatusCode: 200,
					Body:       io.NopCloser(strings.NewReader(responseBody)),
				}

				// Expect http.Do to return mock response
				ts.mockHTTP.EXPECT().
					Do(gomock.Any()).
					Return(mockResponse, nil).
					Times(1)

				// Expect io.ReadAll to return response body
				ts.mockIO.EXPECT().
					ReadAll(gomock.Any()).
					Return([]byte(responseBody), nil).
					Times(1)

				// Expect json.Unmarshal to unmarshal response body
				ts.mockJSON.EXPECT().Unmarshal(gomock.Any(), gomock.Any()).DoAndReturn(func(data []byte, v interface{}) error {
					targets := v.(*[]struct {
						Type                 string `json:"type"`
						Title                string `json:"title"`
						WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
					})
					*targets = []struct {
						Type                 string `json:"type"`
						Title                string `json:"title"`
						WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
					}{
						{
							Type:                 "unknown",
							Title:                "Not a page",
							WebSocketDebuggerURL: "ws://localhost:9222/devtools/unknown/123",
						},
						{
							Type:                 "unsupported_type",
							Title:                "Not a page",
							WebSocketDebuggerURL: "ws://localhost:9222/devtools/unsupported_type/123",
						},
					}
					return nil
				}).Times(1)
			},
			wantErr: "no page target found",
		},
		{
			name: "multiple page targets found",
			setupFunc: func(ts *testSetup) {
				responseBody := "fake response body"
				mockResponse := &http.Response{
					StatusCode: 200,
					Body:       io.NopCloser(strings.NewReader(responseBody)),
				}

				// Expect http.Do to return mock response
				ts.mockHTTP.EXPECT().
					Do(gomock.Any()).
					Return(mockResponse, nil).
					Times(1)

				// Expect io.ReadAll to return response body
				ts.mockIO.EXPECT().
					ReadAll(gomock.Any()).
					Return([]byte(responseBody), nil).
					Times(1)

				// Expect json.Unmarshal to unmarshal response body
				ts.mockJSON.EXPECT().
					Unmarshal(gomock.Any(), gomock.Any()).
					DoAndReturn(func(data []byte, v interface{}) error {
						targets := v.(*[]struct {
							Type                 string `json:"type"`
							Title                string `json:"title"`
							WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
						})
						*targets = []struct {
							Type                 string `json:"type"`
							Title                string `json:"title"`
							WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
						}{
							{
								Type:                 "page",
								Title:                "Page 1",
								WebSocketDebuggerURL: "ws://localhost:9222/devtools/page/123",
							},
							{
								Type:                 "page",
								Title:                "Page 2",
								WebSocketDebuggerURL: "ws://localhost:9222/devtools/page/456",
							},
						}
						return nil
					}).Times(1)
			},
			wantErr: "multiple page targets found",
		},
		{
			name: "websocket dial error",
			setupFunc: func(ts *testSetup) {
				responseBody := "fake response body"
				mockResponse := &http.Response{
					StatusCode: 200,
					Body:       io.NopCloser(strings.NewReader(responseBody)),
				}

				// Expect http.Do to return mock response
				ts.mockHTTP.EXPECT().
					Do(gomock.Any()).
					Return(mockResponse, nil).
					Times(1)

				// Expect io.ReadAll to return response body
				ts.mockIO.EXPECT().
					ReadAll(gomock.Any()).
					Return([]byte(responseBody), nil).
					Times(1)

				// Expect json.Unmarshal to unmarshal response body
				ts.mockJSON.EXPECT().
					Unmarshal(gomock.Any(), gomock.Any()).
					DoAndReturn(func(data []byte, v interface{}) error {
						targets := v.(*[]struct {
							Type                 string `json:"type"`
							Title                string `json:"title"`
							WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
						})
						*targets = []struct {
							Type                 string `json:"type"`
							Title                string `json:"title"`
							WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
						}{
							{Type: "page", Title: "Test Page", WebSocketDebuggerURL: "ws://localhost:9222/devtools/page/123"},
						}
						return nil
					}).
					Times(1)

				// Expect dialer to dial and return error
				ts.mockDialer.EXPECT().
					DialContext(ts.ctx, gomock.Any(), nil).
					Return(nil, nil, fmt.Errorf("dial failed")).
					Times(1)
			},
			wantErr: "cdp dial error",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts := setup(t)
			defer ts.teardown()

			// Setup error condition
			tt.setupFunc(ts)

			// Execute the method under test
			err := ts.client.Init(ts.ctx)

			// Assert error occurred and contains expected message
			assert.Error(t, err, "expected error, got %v", err)
			assert.Contains(t, err.Error(), tt.wantErr, "expected error message to contain %q, got %q", tt.wantErr, err.Error())

			// Clean up if client was initialized
			ts.client.Close()
		})
	}
}

func TestClient_Init_Async(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	// Mock HTTP response for /json endpoint
	responseBody := "fake response body"
	responseBodyBytes := []byte(responseBody)
	mockResponse := &http.Response{
		StatusCode: 200,
		Body:       io.NopCloser(strings.NewReader(responseBody)),
	}

	// Expect http.Do to return mock response - only one connection should succeed
	ts.mockHTTP.EXPECT().
		Do(gomock.Any()).
		Return(mockResponse, nil).
		Times(1)

	// Expect io.ReadAll to return response body
	ts.mockIO.EXPECT().
		ReadAll(mockResponse.Body).
		Return(responseBodyBytes, nil).
		Times(1)

	// Expect json.Unmarshal to unmarshal response body
	ts.mockJSON.EXPECT().
		Unmarshal(responseBodyBytes, gomock.Any()).
		DoAndReturn(func(data []byte, v interface{}) error {
			targets := v.(*[]struct {
				Type                 string `json:"type"`
				Title                string `json:"title"`
				WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
			})
			*targets = []struct {
				Type                 string `json:"type"`
				Title                string `json:"title"`
				WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
			}{
				{
					Type:                 "page",
					Title:                "Test Page",
					WebSocketDebuggerURL: "ws://localhost:9222/devtools/page/123",
				},
			}
			return nil
		}).
		Times(1)

	// Expect dialer to dial and return OK response - only one connection should succeed
	ts.mockDialer.EXPECT().
		DialContext(ts.ctx, gomock.Any(), nil).
		Return(ts.mockConn, nil, nil).
		Times(1)

	// Expect conn to be closed properly
	ts.mockConn.EXPECT().
		Close().
		Return(nil).
		AnyTimes()

	// Use channels to coordinate the goroutines
	numGoroutines := 5
	errChan := make(chan error, numGoroutines)
	startChan := make(chan struct{})

	// Start multiple goroutines trying to initialize concurrently
	for i := range numGoroutines {
		go func(id int) {
			// Wait for all goroutines to be ready
			<-startChan

			err := ts.client.Init(ts.ctx)
			errChan <- err
		}(i)
	}

	// Start all goroutines at the same time
	close(startChan)

	// Collect results
	var successCount int
	var alreadyInitializedCount int
	var otherErrors []error

	for range numGoroutines {
		err := <-errChan
		if err == nil {
			successCount++
		} else if errors.Is(err, cdp.ErrAlreadyInitialized) {
			alreadyInitializedCount++
		} else {
			otherErrors = append(otherErrors, err)
		}
	}

	// Verify results
	assert.Equal(t, 1, successCount, "Expected exactly one successful initialization")
	assert.Equal(t, numGoroutines-1, alreadyInitializedCount, "Expected %d already initialized errors", numGoroutines-1)
	assert.Empty(t, otherErrors, "Expected no other errors, got: %v", otherErrors)
	assert.True(t, ts.client.Initialized(), "Expected client to be initialized")

	// Properly close the client to prevent goroutine leaks
	ts.client.Close()
}

func TestClient_Init_ContextCanceled(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	// Create a context that we can cancel
	ctx, cancel := context.WithCancel(context.Background())

	// Mock HTTP response for /json endpoint
	responseBody := "fake response body"
	responseBodyBytes := []byte(responseBody)
	mockResponse := &http.Response{
		StatusCode: 200,
		Body:       io.NopCloser(strings.NewReader(responseBody)),
	}

	// Expect http.Do to return mock response
	ts.mockHTTP.EXPECT().
		Do(gomock.Any()).
		Return(mockResponse, nil).
		Times(1)

	// Expect io.ReadAll to return response body
	ts.mockIO.EXPECT().
		ReadAll(mockResponse.Body).
		Return(responseBodyBytes, nil).
		Times(1)

	// Expect json.Unmarshal to unmarshal response body
	ts.mockJSON.EXPECT().
		Unmarshal(responseBodyBytes, gomock.Any()).
		DoAndReturn(func(data []byte, v interface{}) error {
			targets := v.(*[]struct {
				Type                 string `json:"type"`
				Title                string `json:"title"`
				WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
			})
			*targets = []struct {
				Type                 string `json:"type"`
				Title                string `json:"title"`
				WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
			}{
				{
					Type:                 "page",
					Title:                "Test Page",
					WebSocketDebuggerURL: "ws://localhost:9222/devtools/page/123",
				},
			}
			return nil
		}).
		Times(1)

	// Expect dialer to dial and return connection, but then cancel context immediately
	dialCalled := make(chan struct{})
	ts.mockDialer.EXPECT().
		DialContext(gomock.Any(), gomock.Any(), nil).
		DoAndReturn(func(dialCtx context.Context, url string, headers http.Header) (wrapper.WebSocketConn, *http.Response, error) {
			close(dialCalled)
			return ts.mockConn, nil, nil
		}).
		Times(1)

	// Expect conn to be closed when context is canceled
	ts.mockConn.EXPECT().
		Close().
		Return(nil).
		AnyTimes()

	// Execute the method under test
	err := ts.client.Init(ctx)
	assert.NoError(t, err, "expected no error, got %v", err)
	assert.True(t, ts.client.Initialized(), "expected client to be initialized")

	// Wait for dial to be called
	select {
	case <-dialCalled:
		// Cancel the context
		cancel()
	case <-time.After(1 * time.Second):
		t.Fatal("dial should have been called")
	}

	// Wait for the context cancellation to propagate
	time.Sleep(100 * time.Millisecond)

	// The client should be closed due to context cancellation
	assert.False(t, ts.client.Initialized(), "expected client to be closed after context cancellation")
}

func TestClient_Send_Success(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	// First initialize the client
	responseBody := "fake response body"
	responseBodyBytes := []byte(responseBody)
	mockResponse := &http.Response{
		StatusCode: 200,
		Body:       io.NopCloser(strings.NewReader(responseBody)),
	}

	// Setup initialization expectations
	ts.mockHTTP.EXPECT().
		Do(gomock.Any()).
		Return(mockResponse, nil).
		Times(1)

	// Expect io.ReadAll to return response body
	ts.mockIO.EXPECT().
		ReadAll(mockResponse.Body).
		Return(responseBodyBytes, nil).
		Times(1)

	// Expect json.Unmarshal to unmarshal response body
	ts.mockJSON.EXPECT().
		Unmarshal(gomock.Any(), gomock.Any()).
		DoAndReturn(func(data []byte, v interface{}) error {
			targets := v.(*[]struct {
				Type                 string `json:"type"`
				Title                string `json:"title"`
				WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
			})
			*targets = []struct {
				Type                 string `json:"type"`
				Title                string `json:"title"`
				WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
			}{
				{
					Type:                 "page",
					Title:                "Test Page",
					WebSocketDebuggerURL: "ws://localhost:9222/devtools/page/123",
				},
			}
			return nil
		}).
		Times(1)

	// Expect dialer to dial and return connection
	ts.mockDialer.EXPECT().
		DialContext(ts.ctx, gomock.Any(), nil).
		Return(ts.mockConn, nil, nil).
		Times(1)

	// Expect JSON marshal for the request
	const method = cdp.METHOD_EVALUATE
	const id = 1
	const command = "console.log('test')"
	expectedRequest := map[string]interface{}{
		"id":     id,
		"method": method,
		"params": map[string]interface{}{
			"expression": command,
		},
	}
	requestData := []byte(fmt.Sprintf(`{"id":%d,"method":"%s","params":{"expression":"%s"}}`, id, method, command))

	ts.mockJSON.EXPECT().
		Marshal(expectedRequest).
		Return(requestData, nil).
		Times(1)

	// Expect WriteMessage to send the request
	ts.mockConn.EXPECT().
		WriteMessage(websocket.TextMessage, requestData).
		Return(nil).
		Times(1)

	// Mock response from CDP
	const resultType = cdp.TYPE_STRING
	const resultValue = "{\"key\":\"value\"}"
	cdpResponse := fmt.Sprintf(`{"id":%d,"result":{"result":{"type":"%s","value":"%s"}}}`, id, resultType, resultValue)
	ts.mockConn.EXPECT().
		ReadMessage().
		Return(websocket.TextMessage, []byte(cdpResponse), nil).
		Times(1)

	// Expect JSON unmarshal for the response
	ts.mockJSON.EXPECT().
		Unmarshal([]byte(cdpResponse), gomock.Any()).
		DoAndReturn(func(data []byte, v interface{}) error {
			resp := v.(*struct {
				ID     int `json:"id"`
				Result struct {
					Result struct {
						Type        string      `json:"type"`
						Subtype     *string     `json:"subtype"`
						ClassName   *string     `json:"className"`
						Description *string     `json:"description"`
						Value       interface{} `json:"value"`
					} `json:"result"`
				} `json:"result"`
			})
			resp.ID = 1
			resp.Result.Result.Type = resultType
			resp.Result.Result.Value = resultValue
			return nil
		}).
		Times(1)

	// Expect JSON unmarshal for the response value with type string
	resultValueMap := map[string]interface{}{
		"key": "value",
	}
	ts.mockJSON.EXPECT().
		Unmarshal([]byte(resultValue), gomock.Any()).
		DoAndReturn(func(data []byte, v interface{}) error {
			*v.(*map[string]interface{}) = resultValueMap
			return nil
		}).
		Times(1)

	// Expect conn to be closed properly
	ts.mockConn.EXPECT().
		Close().
		Return(nil).
		AnyTimes()

	// Initialize the client
	err := ts.client.Init(ts.ctx)
	assert.NoError(t, err, "expected no error during init, got %v", err)
	assert.True(t, ts.client.Initialized(), "expected client to be initialized")

	// Send the message
	result, err := ts.client.Send(method,
		map[string]interface{}{
			"expression": command,
		})
	assert.NoError(t, err, "expected no error during send, got %v", err)
	assert.Equal(t, resultValueMap, result, "expected result to be %v, got %v", resultValueMap, result)

	// Properly close the client
	ts.client.Close()
}

func TestClient_Send_Error(t *testing.T) {
	tests := []struct {
		name      string
		setupFunc func(*testSetup)
		wantErr   string
	}{
		{
			name: "not initialized error",
			setupFunc: func(ts *testSetup) {
				// Don't initialize the client
			},
			wantErr: "CDP connection is not initialized",
		},
		{
			name: "JSON marshal error",
			setupFunc: func(ts *testSetup) {
				// Initialize the client first
				responseBody := "fake response body"
				mockResponse := &http.Response{
					StatusCode: 200,
					Body:       io.NopCloser(strings.NewReader(responseBody)),
				}

				// Expect HTTP GET to return response body
				ts.mockHTTP.EXPECT().
					Do(gomock.Any()).
					Return(mockResponse, nil).Times(1)

				// Expect io.ReadAll to return response body
				ts.mockIO.EXPECT().
					ReadAll(gomock.Any()).
					Return([]byte(responseBody), nil).Times(1)

				// Expect json.Unmarshal to unmarshal response body
				ts.mockJSON.EXPECT().
					Unmarshal(gomock.Any(), gomock.Any()).
					DoAndReturn(func(data []byte, v interface{}) error {
						targets := v.(*[]struct {
							Type                 string `json:"type"`
							Title                string `json:"title"`
							WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
						})
						*targets = []struct {
							Type                 string `json:"type"`
							Title                string `json:"title"`
							WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
						}{
							{
								Type:                 "page",
								Title:                "Test Page",
								WebSocketDebuggerURL: "ws://localhost:9222/devtools/page/123",
							},
						}
						return nil
					}).Times(1)

				// Expect dialer to dial and return connection
				ts.mockDialer.EXPECT().
					DialContext(ts.ctx, gomock.Any(), nil).
					Return(ts.mockConn, nil, nil).Times(1)

				// Initialize
				err := ts.client.Init(ts.ctx)
				assert.NoError(t, err, "expected no error during init, got %v", err)
				assert.True(t, ts.client.Initialized(), "expected client to be initialized")

				// Expect JSON marshal to fail
				ts.mockJSON.EXPECT().
					Marshal(gomock.Any()).
					Return(nil, fmt.Errorf("marshal error")).
					Times(1)
			},
			wantErr: "failed to marshal CDP message",
		},
		{
			name: "write message error",
			setupFunc: func(ts *testSetup) {
				// Initialize the client first
				responseBody := "fake response body"
				mockResponse := &http.Response{
					StatusCode: 200,
					Body:       io.NopCloser(strings.NewReader(responseBody)),
				}

				// Expect HTTP GET to return response body
				ts.mockHTTP.EXPECT().
					Do(gomock.Any()).
					Return(mockResponse, nil).
					Times(1)

				// Expect io.ReadAll to return response body
				ts.mockIO.EXPECT().
					ReadAll(gomock.Any()).
					Return([]byte(responseBody), nil).
					Times(1)

				// Expect json.Unmarshal to unmarshal response body
				ts.mockJSON.EXPECT().
					Unmarshal(gomock.Any(), gomock.Any()).
					DoAndReturn(func(data []byte, v interface{}) error {
						targets := v.(*[]struct {
							Type                 string `json:"type"`
							Title                string `json:"title"`
							WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
						})
						*targets = []struct {
							Type                 string `json:"type"`
							Title                string `json:"title"`
							WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
						}{
							{
								Type:                 "page",
								Title:                "Test Page",
								WebSocketDebuggerURL: "ws://localhost:9222/devtools/page/123",
							},
						}
						return nil
					}).Times(1)

				// Expect dialer to dial and return connection
				ts.mockDialer.EXPECT().
					DialContext(ts.ctx, gomock.Any(), nil).
					Return(ts.mockConn, nil, nil).
					Times(1)

				// Initialize
				err := ts.client.Init(ts.ctx)
				assert.NoError(t, err, "expected no error during init, got %v", err)
				assert.True(t, ts.client.Initialized(), "expected client to be initialized")

				// Expect JSON marshal to succeed
				ts.mockJSON.EXPECT().
					Marshal(gomock.Any()).
					Return([]byte(`{"test":"data"}`), nil).
					Times(1)

				// Expect WriteMessage to fail
				ts.mockConn.EXPECT().
					WriteMessage(websocket.TextMessage, gomock.Any()).
					Return(fmt.Errorf("write error")).
					Times(1)
			},
			wantErr: "CDP write error",
		},
		{
			name: "read message error",
			setupFunc: func(ts *testSetup) {
				// Initialize the client first
				responseBody := "fake response body"
				mockResponse := &http.Response{
					StatusCode: 200,
					Body:       io.NopCloser(strings.NewReader(responseBody)),
				}

				// Expect HTTP GET to return response body
				ts.mockHTTP.EXPECT().
					Do(gomock.Any()).
					Return(mockResponse, nil).
					Times(1)

				// Expect io.ReadAll to return response body
				ts.mockIO.EXPECT().
					ReadAll(gomock.Any()).
					Return([]byte(responseBody), nil).
					Times(1)

				// Expect json.Unmarshal to unmarshal response body
				ts.mockJSON.EXPECT().
					Unmarshal(gomock.Any(), gomock.Any()).
					DoAndReturn(func(data []byte, v interface{}) error {
						targets := v.(*[]struct {
							Type                 string `json:"type"`
							Title                string `json:"title"`
							WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
						})
						*targets = []struct {
							Type                 string `json:"type"`
							Title                string `json:"title"`
							WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
						}{
							{
								Type:                 "page",
								Title:                "Test Page",
								WebSocketDebuggerURL: "ws://localhost:9222/devtools/page/123",
							},
						}
						return nil
					}).Times(1)

				// Expect dialer to dial and return connection
				ts.mockDialer.EXPECT().
					DialContext(ts.ctx, gomock.Any(), nil).
					Return(ts.mockConn, nil, nil).
					Times(1)

				// Initialize
				err := ts.client.Init(ts.ctx)
				assert.NoError(t, err, "expected no error during init, got %v", err)
				assert.True(t, ts.client.Initialized(), "expected client to be initialized")

				// Expect JSON marshal to succeed
				ts.mockJSON.EXPECT().
					Marshal(gomock.Any()).
					Return([]byte(`{"test":"data"}`), nil).
					Times(1)

				// Expect WriteMessage to succeed
				ts.mockConn.EXPECT().
					WriteMessage(websocket.TextMessage, gomock.Any()).
					Return(nil).
					Times(1)

				// Expect ReadMessage to fail
				ts.mockConn.EXPECT().
					ReadMessage().
					Return(0, nil, fmt.Errorf("read error")).
					Times(1)
			},
			wantErr: "failed to read CDP response",
		},
		{
			name: "JSON unmarshal response error",
			setupFunc: func(ts *testSetup) {
				// Initialize the client first
				responseBody := "fake response body"
				mockResponse := &http.Response{
					StatusCode: 200,
					Body:       io.NopCloser(strings.NewReader(responseBody)),
				}

				// Expect HTTP GET to return response body
				ts.mockHTTP.EXPECT().
					Do(gomock.Any()).
					Return(mockResponse, nil).
					Times(1)

				// Expect io.ReadAll to return response body
				ts.mockIO.EXPECT().
					ReadAll(gomock.Any()).
					Return([]byte(responseBody), nil).
					Times(1)

				// Expect json.Unmarshal to unmarshal response body
				ts.mockJSON.EXPECT().
					Unmarshal(gomock.Any(), gomock.Any()).DoAndReturn(func(data []byte, v interface{}) error {
					targets := v.(*[]struct {
						Type                 string `json:"type"`
						Title                string `json:"title"`
						WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
					})
					*targets = []struct {
						Type                 string `json:"type"`
						Title                string `json:"title"`
						WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
					}{
						{
							Type:                 "page",
							Title:                "Test Page",
							WebSocketDebuggerURL: "ws://localhost:9222/devtools/page/123",
						},
					}
					return nil
				}).Times(1)

				// Expect dialer to dial and return connection
				ts.mockDialer.EXPECT().
					DialContext(ts.ctx, gomock.Any(), nil).
					Return(ts.mockConn, nil, nil).
					Times(1)

				// Initialize
				err := ts.client.Init(ts.ctx)
				assert.NoError(t, err, "expected no error during init, got %v", err)
				assert.True(t, ts.client.Initialized(), "expected client to be initialized")

				// Expect JSON marshal to succeed
				ts.mockJSON.EXPECT().
					Marshal(gomock.Any()).
					Return([]byte(`{"test":"data"}`), nil).
					Times(1)

				// Expect WriteMessage to succeed
				ts.mockConn.EXPECT().
					WriteMessage(websocket.TextMessage, gomock.Any()).
					Return(nil).
					Times(1)

				// Expect ReadMessage to succeed
				ts.mockConn.EXPECT().
					ReadMessage().
					Return(websocket.TextMessage, []byte{}, nil).
					Times(1)

				// Expect JSON unmarshal to fail
				ts.mockJSON.EXPECT().
					Unmarshal(gomock.Any(), gomock.Any()).
					Return(fmt.Errorf("unmarshal error")).
					Times(1)
			},
			wantErr: "failed to parse CDP response",
		},
		{
			name: "CDP error response",
			setupFunc: func(ts *testSetup) {
				// Initialize the client first
				responseBody := "fake response body"
				mockResponse := &http.Response{
					StatusCode: 200,
					Body:       io.NopCloser(strings.NewReader(responseBody)),
				}

				// Expect HTTP GET to return response body
				ts.mockHTTP.EXPECT().
					Do(gomock.Any()).
					Return(mockResponse, nil).
					Times(1)

				// Expect io.ReadAll to return response body
				ts.mockIO.EXPECT().
					ReadAll(gomock.Any()).
					Return([]byte(responseBody), nil).
					Times(1)

				// Expect json.Unmarshal to unmarshal response body
				ts.mockJSON.EXPECT().
					Unmarshal(gomock.Any(), gomock.Any()).
					DoAndReturn(func(data []byte, v interface{}) error {
						targets := v.(*[]struct {
							Type                 string `json:"type"`
							Title                string `json:"title"`
							WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
						})
						*targets = []struct {
							Type                 string `json:"type"`
							Title                string `json:"title"`
							WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
						}{
							{
								Type:                 "page",
								Title:                "Test Page",
								WebSocketDebuggerURL: "ws://localhost:9222/devtools/page/123",
							},
						}
						return nil
					}).Times(1)

				// Expect dialer to dial and return connection
				ts.mockDialer.EXPECT().
					DialContext(ts.ctx, gomock.Any(), nil).
					Return(ts.mockConn, nil, nil).
					Times(1)

				// Initialize
				err := ts.client.Init(ts.ctx)
				assert.NoError(t, err, "expected no error during init, got %v", err)
				assert.True(t, ts.client.Initialized(), "expected client to be initialized")

				// Expect JSON marshal to succeed
				ts.mockJSON.EXPECT().
					Marshal(gomock.Any()).
					Return([]byte(`{"test":"data"}`), nil).
					Times(1)

				// Expect WriteMessage to succeed
				ts.mockConn.EXPECT().
					WriteMessage(websocket.TextMessage, gomock.Any()).
					Return(nil).
					Times(1)

				// Expect ReadMessage to succeed
				ts.mockConn.EXPECT().
					ReadMessage().
					Return(websocket.TextMessage, []byte{}, nil).
					Times(1)

				// Expect JSON unmarshal to succeed and return error response
				ts.mockJSON.EXPECT().
					Unmarshal(gomock.Any(), gomock.Any()).
					DoAndReturn(func(data []byte, v interface{}) error {
						resp := v.(*struct {
							ID     int `json:"id"`
							Result struct {
								Result struct {
									Type        string      `json:"type"`
									Subtype     *string     `json:"subtype"`
									ClassName   *string     `json:"className"`
									Description *string     `json:"description"`
									Value       interface{} `json:"value"`
								} `json:"result"`
							} `json:"result"`
						})
						resp.ID = 1
						resp.Result.Result.Type = cdp.TYPE_OBJECT
						subtype := cdp.SUBTYPE_ERROR
						resp.Result.Result.Subtype = &subtype
						description := "Test error"
						resp.Result.Result.Description = &description
						return nil
					}).
					Times(1)
			},
			wantErr: "CDP error: Test error",
		},
		{
			name: "CDP invalid string response",
			setupFunc: func(ts *testSetup) {
				// Initialize the client first
				responseBody := "fake response body"
				mockResponse := &http.Response{
					StatusCode: 200,
					Body:       io.NopCloser(strings.NewReader(responseBody)),
				}

				// Expect HTTP GET to return response body
				ts.mockHTTP.EXPECT().
					Do(gomock.Any()).
					Return(mockResponse, nil).
					Times(1)

				// Expect io.ReadAll to return response body
				ts.mockIO.EXPECT().
					ReadAll(gomock.Any()).
					Return([]byte(responseBody), nil).
					Times(1)

				// Expect json.Unmarshal to unmarshal response body
				ts.mockJSON.EXPECT().
					Unmarshal(gomock.Any(), gomock.Any()).
					DoAndReturn(func(data []byte, v interface{}) error {
						targets := v.(*[]struct {
							Type                 string `json:"type"`
							Title                string `json:"title"`
							WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
						})
						*targets = []struct {
							Type                 string `json:"type"`
							Title                string `json:"title"`
							WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
						}{
							{
								Type:                 "page",
								Title:                "Test Page",
								WebSocketDebuggerURL: "ws://localhost:9222/devtools/page/123",
							},
						}
						return nil
					}).
					Times(1)

				// Expect dialer to dial and return connection
				ts.mockDialer.EXPECT().
					DialContext(ts.ctx, gomock.Any(), nil).
					Return(ts.mockConn, nil, nil).
					Times(1)

				// Initialize
				err := ts.client.Init(ts.ctx)
				assert.NoError(t, err, "expected no error during init, got %v", err)
				assert.True(t, ts.client.Initialized(), "expected client to be initialized")

				// Expect JSON marshal to succeed
				ts.mockJSON.EXPECT().
					Marshal(gomock.Any()).
					Return([]byte(`{"test":"data"}`), nil).
					Times(1)

				// Expect WriteMessage to succeed
				ts.mockConn.EXPECT().
					WriteMessage(websocket.TextMessage, gomock.Any()).
					Return(nil).
					Times(1)

				// Expect ReadMessage to succeed
				ts.mockConn.EXPECT().
					ReadMessage().
					Return(websocket.TextMessage, []byte{}, nil).
					Times(1)

				// Expect JSON unmarshal to succeed
				const resultValue = "invalid json"
				ts.mockJSON.EXPECT().
					Unmarshal(gomock.Any(), gomock.Any()).
					DoAndReturn(func(data []byte, v interface{}) error {
						resp := v.(*struct {
							ID     int `json:"id"`
							Result struct {
								Result struct {
									Type        string      `json:"type"`
									Subtype     *string     `json:"subtype"`
									ClassName   *string     `json:"className"`
									Description *string     `json:"description"`
									Value       interface{} `json:"value"`
								} `json:"result"`
							} `json:"result"`
						})

						resp.ID = 1
						resp.Result.Result.Type = cdp.TYPE_STRING
						resp.Result.Result.Value = resultValue
						return nil
					}).
					Times(1)

				// Expect JSON unmarshal to fail
				ts.mockJSON.EXPECT().
					Unmarshal([]byte(resultValue), gomock.Any()).
					Return(fmt.Errorf("unmarshal error")).
					Times(1)
			},
			wantErr: "CDP unmarshal error",
		},
		{
			name: "CDP response type mismatch",
			setupFunc: func(ts *testSetup) {
				// Initialize the client first
				responseBody := "fake response body"
				mockResponse := &http.Response{
					StatusCode: 200,
					Body:       io.NopCloser(strings.NewReader(responseBody)),
				}

				// Expect HTTP GET to return response body
				ts.mockHTTP.EXPECT().
					Do(gomock.Any()).
					Return(mockResponse, nil).
					Times(1)

				// Expect io.ReadAll to return response body
				ts.mockIO.EXPECT().
					ReadAll(gomock.Any()).
					Return([]byte(responseBody), nil).
					Times(1)

				// Expect json.Unmarshal to unmarshal response body
				ts.mockJSON.EXPECT().
					Unmarshal(gomock.Any(), gomock.Any()).
					DoAndReturn(func(data []byte, v interface{}) error {
						targets := v.(*[]struct {
							Type                 string `json:"type"`
							Title                string `json:"title"`
							WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
						})
						*targets = []struct {
							Type                 string `json:"type"`
							Title                string `json:"title"`
							WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
						}{
							{
								Type:                 "page",
								Title:                "Test Page",
								WebSocketDebuggerURL: "ws://localhost:9222/devtools/page/123",
							},
						}
						return nil
					}).
					Times(1)

				// Expect dialer to dial and return connection
				ts.mockDialer.EXPECT().
					DialContext(ts.ctx, gomock.Any(), nil).
					Return(ts.mockConn, nil, nil).
					Times(1)

				// Initialize
				err := ts.client.Init(ts.ctx)
				assert.NoError(t, err, "expected no error during init, got %v", err)
				assert.True(t, ts.client.Initialized(), "expected client to be initialized")

				// Expect JSON marshal to succeed
				ts.mockJSON.EXPECT().
					Marshal(gomock.Any()).
					Return([]byte(`{"test":"data"}`), nil).
					Times(1)

				// Expect WriteMessage to succeed
				ts.mockConn.EXPECT().
					WriteMessage(websocket.TextMessage, gomock.Any()).
					Return(nil).
					Times(1)

				// Expect ReadMessage to succeed
				ts.mockConn.EXPECT().
					ReadMessage().
					Return(websocket.TextMessage, []byte{}, nil).
					Times(1)

				// Expect JSON unmarshal to succeed
				const resultValue = "invalid json"
				ts.mockJSON.EXPECT().
					Unmarshal(gomock.Any(), gomock.Any()).
					DoAndReturn(func(data []byte, v interface{}) error {
						resp := v.(*struct {
							ID     int `json:"id"`
							Result struct {
								Result struct {
									Type        string      `json:"type"`
									Subtype     *string     `json:"subtype"`
									ClassName   *string     `json:"className"`
									Description *string     `json:"description"`
									Value       interface{} `json:"value"`
								} `json:"result"`
							} `json:"result"`
						})

						resp.ID = 1
						resp.Result.Result.Type = "undefined"
						resp.Result.Result.Value = resultValue
						return nil
					}).
					Times(1)
			},
			wantErr: "CDP response type mismatch",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts := setup(t)
			defer ts.teardown()

			// Setup error condition
			tt.setupFunc(ts)

			// Expect conn to be closed properly
			ts.mockConn.EXPECT().
				Close().
				Return(nil).
				AnyTimes()

			// Execute the method under test
			result, err := ts.client.Send(cdp.METHOD_EVALUATE,
				map[string]interface{}{
					"expression": "test",
				})

			// Assert error occurred and contains expected message
			assert.Error(t, err, "expected error, got %v", err)
			assert.Contains(t, err.Error(), tt.wantErr, "expected error message to contain %q, got %q", tt.wantErr, err.Error())
			assert.Nil(t, result, "expected nil result on error")

			// Clean up if client was initialized
			ts.client.Close()
		})
	}
}

func TestClient_Send_Async(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	// First initialize the client
	responseBody := "fake response body"
	responseBodyBytes := []byte(responseBody)
	mockResponse := &http.Response{
		StatusCode: 200,
		Body:       io.NopCloser(strings.NewReader(responseBody)),
	}

	// Setup initialization expectations
	ts.mockHTTP.EXPECT().
		Do(gomock.Any()).
		Return(mockResponse, nil).
		Times(1)

	// Expect io.ReadAll to return response body
	ts.mockIO.EXPECT().
		ReadAll(mockResponse.Body).
		Return(responseBodyBytes, nil).
		Times(1)

	// Expect json.Unmarshal to unmarshal response body
	ts.mockJSON.EXPECT().
		Unmarshal(responseBodyBytes, gomock.Any()).
		DoAndReturn(func(data []byte, v interface{}) error {
			targets := v.(*[]struct {
				Type                 string `json:"type"`
				Title                string `json:"title"`
				WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
			})
			*targets = []struct {
				Type                 string `json:"type"`
				Title                string `json:"title"`
				WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
			}{
				{
					Type:                 "page",
					Title:                "Test Page",
					WebSocketDebuggerURL: "ws://localhost:9222/devtools/page/123",
				},
			}
			return nil
		}).
		Times(1)

	// Expect dialer to dial and return connection
	ts.mockDialer.EXPECT().
		DialContext(ts.ctx, gomock.Any(), nil).
		Return(ts.mockConn, nil, nil).
		Times(1)

	// Initialize the client
	err := ts.client.Init(ts.ctx)
	assert.NoError(t, err, "expected no error during init, got %v", err)
	assert.True(t, ts.client.Initialized(), "expected client to be initialized")

	// Test concurrent sends
	numGoroutines := 1000
	resultChan := make(chan struct {
		goroutineID int
		result      interface{}
		err         error
	}, numGoroutines)

	// Use sync.Map to track requestID by goroutine ID
	var requestIDMap sync.Map
	// Channel to track the order of requests for ReadMessage correlation
	requestIDQueue := make(chan int, numGoroutines)

	// Expect JSON marshal for any request structure
	ts.mockJSON.EXPECT().
		Marshal(gomock.Any()).
		DoAndReturn(func(msg interface{}) ([]byte, error) {
			// Extract the request to get ID and command
			reqMap := msg.(map[string]interface{})
			requestID := reqMap["id"].(int)
			params := reqMap["params"].(map[string]interface{})
			expression := params["expression"].(string)

			// Extract goroutine ID from the expression (we embed it in the command)
			// Format: "console.log('test {goroutineID}')"
			var goroutineID int
			_, _ = fmt.Sscanf(expression, "console.log('test %d')", &goroutineID)

			// Store requestID by goroutine ID
			requestIDMap.Store(goroutineID, requestID)

			// Queue the request ID for ReadMessage correlation
			requestIDQueue <- requestID

			// Return the marshaled data
			return []byte(fmt.Sprintf(`{"id":%d,"method":"%s","params":{"expression":"%s"}}`,
				requestID, cdp.METHOD_EVALUATE, expression)), nil
		}).
		Times(numGoroutines)

	// Expect WriteMessage for any data
	ts.mockConn.EXPECT().
		WriteMessage(websocket.TextMessage, gomock.Any()).
		Return(nil).
		Times(numGoroutines)

	// Expect ReadMessage to return responses
	ts.mockConn.EXPECT().
		ReadMessage().
		DoAndReturn(func() (int, []byte, error) {
			// Get the request ID that corresponds to this ReadMessage call
			requestID := <-requestIDQueue
			// Create response with the same ID as the request
			respData := []byte(fmt.Sprintf(`{"id":%d,"result":{"result":{"type":"string","value":"{}"}}}`, requestID))
			return websocket.TextMessage, respData, nil
		}).
		Times(numGoroutines)

	// Expect JSON unmarshal calls (2 per Send operation)
	ts.mockJSON.EXPECT().
		Unmarshal(gomock.Any(), gomock.Any()).
		DoAndReturn(func(data []byte, v interface{}) error {

			// Check if this is a CDP response structure (the first unmarshal call)
			if _, ok := v.(*struct {
				ID     int `json:"id"`
				Result struct {
					Result struct {
						Type        string      `json:"type"`
						Subtype     *string     `json:"subtype"`
						ClassName   *string     `json:"className"`
						Description *string     `json:"description"`
						Value       interface{} `json:"value"`
					} `json:"result"`
				} `json:"result"`
			}); ok {
				// Parse the actual response to get the ID
				var rawResp struct {
					ID int `json:"id"`
				}
				if _, err := fmt.Sscanf(string(data), `{"id":%d,`, &rawResp.ID); err == nil {
					// This is the CDP response unmarshal
					resp := v.(*struct {
						ID     int `json:"id"`
						Result struct {
							Result struct {
								Type        string      `json:"type"`
								Subtype     *string     `json:"subtype"`
								ClassName   *string     `json:"className"`
								Description *string     `json:"description"`
								Value       interface{} `json:"value"`
							} `json:"result"`
						} `json:"result"`
					})

					resp.ID = rawResp.ID
					resp.Result.Result.Type = cdp.TYPE_STRING
					resp.Result.Result.Value = "{}"
					return nil
				}
			} else if _, ok := v.(*map[string]interface{}); ok {
				// This is the string value unmarshal (second call)
				*v.(*map[string]interface{}) = map[string]interface{}{}
				return nil
			}
			return fmt.Errorf("unexpected unmarshal type")
		}).
		Times(numGoroutines * 2) // Two unmarshal calls per Send operation

	// Expect conn to be closed properly
	ts.mockConn.EXPECT().
		Close().
		Return(nil).
		AnyTimes()

	// Start concurrent goroutines
	startChan := make(chan struct{})
	for i := 1; i <= numGoroutines; i++ {
		go func(goroutineID int) {
			// Wait for all goroutines to be ready
			<-startChan

			command := fmt.Sprintf("console.log('test %d')", goroutineID)
			result, err := ts.client.Send(cdp.METHOD_EVALUATE,
				map[string]interface{}{
					"expression": command,
				})

			resultChan <- struct {
				goroutineID int
				result      interface{}
				err         error
			}{
				goroutineID: goroutineID,
				result:      result,
				err:         err,
			}
		}(i)
	}

	// Start all goroutines at the same time
	close(startChan)

	// Collect and verify results
	results := make(map[int]interface{})
	errors := make(map[int]error)

	for range numGoroutines {
		res := <-resultChan
		if res.err != nil {
			errors[res.goroutineID] = res.err
		} else {
			results[res.goroutineID] = res.result
		}
	}

	// Verify no errors occurred
	assert.Empty(t, errors, "Expected no errors, got: %v", errors)

	// Verify all requests got responses
	assert.Len(t, results, numGoroutines, "Expected %d results, got %d", numGoroutines, len(results))

	// Verify each response is an empty map (our expected result)
	for goroutineID := 1; goroutineID <= numGoroutines; goroutineID++ {
		assert.Contains(t, results, goroutineID, "Missing result for goroutine %d", goroutineID)
		assert.Equal(t, map[string]interface{}{}, results[goroutineID], "Incorrect result for goroutine %d", goroutineID)
	}

	// Check for duplicate request IDs using requestIDMap
	idSet := make(map[int]bool)
	requestIDMap.Range(func(key, value interface{}) bool {
		requestID := value.(int)
		if idSet[requestID] {
			t.Errorf("Duplicate request ID found: %d", requestID)
		}
		idSet[requestID] = true
		return true
	})

	// Verify we got the expected number of unique IDs
	assert.Len(t, idSet, numGoroutines, "Should have %d unique request IDs", numGoroutines)

	// Properly close the client
	ts.client.Close()
}

func TestClient_Close_Success(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	// First initialize the client
	responseBody := "fake response body"
	responseBodyBytes := []byte(responseBody)
	mockResponse := &http.Response{
		StatusCode: 200,
		Body:       io.NopCloser(strings.NewReader(responseBody)),
	}

	// Setup initialization expectations
	ts.mockHTTP.EXPECT().
		Do(gomock.Any()).
		Return(mockResponse, nil).
		Times(1)

	ts.mockIO.EXPECT().
		ReadAll(mockResponse.Body).
		Return(responseBodyBytes, nil).
		Times(1)

	ts.mockJSON.EXPECT().
		Unmarshal(responseBodyBytes, gomock.Any()).
		DoAndReturn(func(data []byte, v interface{}) error {
			targets := v.(*[]struct {
				Type                 string `json:"type"`
				Title                string `json:"title"`
				WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
			})
			*targets = []struct {
				Type                 string `json:"type"`
				Title                string `json:"title"`
				WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
			}{
				{
					Type:                 "page",
					Title:                "Test Page",
					WebSocketDebuggerURL: "ws://localhost:9222/devtools/page/123",
				},
			}
			return nil
		}).
		Times(1)

	// Expect dialer to dial and return connection
	ts.mockDialer.EXPECT().
		DialContext(ts.ctx, gomock.Any(), nil).
		Return(ts.mockConn, nil, nil).
		Times(1)

	// Expect conn to be closed successfully
	ts.mockConn.EXPECT().
		Close().
		Return(nil).
		Times(1)

	// Initialize the client
	err := ts.client.Init(ts.ctx)
	assert.NoError(t, err, "expected no error during init, got %v", err)
	assert.True(t, ts.client.Initialized(), "expected client to be initialized")

	// Close the client
	ts.client.Close()
	assert.False(t, ts.client.Initialized(), "expected client to not be initialized after close")

	// Close again should be no-op
	ts.client.Close()
	assert.False(t, ts.client.Initialized(), "expected client to remain not initialized")
}

func TestClient_Close_Error(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	// Test 1: Close not initialized client (should be no-op)
	ts.client.Close()
	assert.False(t, ts.client.Initialized(), "expected client to remain not initialized")

	// Test 2: Close with error during conn.Close()
	// First initialize the client
	responseBody := "fake response body"
	responseBodyBytes := []byte(responseBody)
	mockResponse := &http.Response{
		StatusCode: 200,
		Body:       io.NopCloser(strings.NewReader(responseBody)),
	}

	// Setup initialization expectations
	ts.mockHTTP.EXPECT().
		Do(gomock.Any()).
		Return(mockResponse, nil).
		Times(1)

	ts.mockIO.EXPECT().
		ReadAll(mockResponse.Body).
		Return(responseBodyBytes, nil).
		Times(1)

	ts.mockJSON.EXPECT().
		Unmarshal(responseBodyBytes, gomock.Any()).
		DoAndReturn(func(data []byte, v interface{}) error {
			targets := v.(*[]struct {
				Type                 string `json:"type"`
				Title                string `json:"title"`
				WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
			})
			*targets = []struct {
				Type                 string `json:"type"`
				Title                string `json:"title"`
				WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
			}{
				{
					Type:                 "page",
					Title:                "Test Page",
					WebSocketDebuggerURL: "ws://localhost:9222/devtools/page/123",
				},
			}
			return nil
		}).
		Times(1)

	ts.mockDialer.EXPECT().
		DialContext(ts.ctx, gomock.Any(), nil).
		Return(ts.mockConn, nil, nil).
		Times(1)

	// Expect conn.Close() to return an error
	ts.mockConn.EXPECT().
		Close().
		Return(fmt.Errorf("close error")).
		Times(1)

	// Initialize the client
	err := ts.client.Init(ts.ctx)
	assert.NoError(t, err, "expected no error during init, got %v", err)
	assert.True(t, ts.client.Initialized(), "expected client to be initialized")

	// Close the client (should handle the error gracefully)
	ts.client.Close()
	assert.False(t, ts.client.Initialized(), "expected client to not be initialized after close despite error")
}
