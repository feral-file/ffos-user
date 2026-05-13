package relayer_test

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/feral-file/ffos-user/components/feral-controld/mocks"
	"github.com/feral-file/ffos-user/components/feral-controld/relayer"
	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"

	"github.com/golang/mock/gomock"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest"
)

// timeoutError implements net.Error interface for testing
type timeoutError struct{}

func (e *timeoutError) Error() string   { return "timeout" }
func (e *timeoutError) Timeout() bool   { return true }
func (e *timeoutError) Temporary() bool { return false }

type testSetup struct {
	ctrl           *gomock.Controller
	ctx            context.Context
	mockDialer     *mocks.MockWebSocketDialer
	mockConn       *mocks.MockWebSocketConn
	mockRandomizer *mocks.MockRandomizer
	mockClock      *mocks.MockClock
	mockOS         *mocks.MockOS
	mockJSON       *mocks.MockJSON
	client         relayer.Relayer
}

func setup(t *testing.T) *testSetup {
	ctrl := gomock.NewController(t)
	logger := zaptest.NewLogger(t, zaptest.Level(zap.FatalLevel))
	ctx := context.Background()

	// Dependencies
	mockDialer := mocks.NewMockWebSocketDialer(ctrl)
	mockConn := mocks.NewMockWebSocketConn(ctrl)
	mockRandomizer := mocks.NewMockRandomizer(ctrl)
	mockClock := mocks.NewMockClock(ctrl)
	mockOS := mocks.NewMockOS(ctrl)
	mockJSON := mocks.NewMockJSON(ctrl)

	client := relayer.New("ws://localhost:8080", "test-api-key", mockDialer, mockRandomizer, mockClock, mockOS, mockJSON, logger)

	return &testSetup{
		ctrl:           ctrl,
		ctx:            ctx,
		mockDialer:     mockDialer,
		mockConn:       mockConn,
		mockRandomizer: mockRandomizer,
		mockClock:      mockClock,
		mockOS:         mockOS,
		mockJSON:       mockJSON,
		client:         client,
	}
}

func (ts *testSetup) teardown() {
	ts.ctrl.Finish()
}

// Helper function to set up mock ticker expectations
func setupMockTicker(ts *testSetup) *mocks.MockTicker {
	// Create a mock ticker
	mockTicker := mocks.NewMockTicker(ts.ctrl)
	tickerChan := make(chan time.Time, 10) // Buffer for multiple ticks

	// Mock the ticker's C() method to return our controllable channel
	mockTicker.EXPECT().
		C().
		Return(tickerChan).
		AnyTimes()

	// Mock the ticker's Stop() method
	mockTicker.EXPECT().
		Stop().
		AnyTimes()

	// Expect clock to create ticker
	ts.mockClock.EXPECT().
		NewTicker(gomock.Any()).
		DoAndReturn(func(d time.Duration) wrapper.Ticker {
			// Send a tick immediately to trigger ping
			go func() {
				time.Sleep(10 * time.Millisecond) // Small delay to let connection establish
				tickerChan <- time.Now()
			}()
			return mockTicker
		}).
		AnyTimes()

	return mockTicker
}

func TestClient_Connect_Success(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	// Set up mock ticker
	setupMockTicker(ts)

	// Expect time to return default time
	ts.mockClock.EXPECT().
		Now().
		Return(time.Time{}).
		AnyTimes()

	// Expect dialer to dial and return OK response
	ts.mockDialer.EXPECT().
		DialContext(ts.ctx, gomock.Any(), nil).
		Return(ts.mockConn, &http.Response{StatusCode: http.StatusOK}, nil)

	// Expect conn to set pong handler
	ts.mockConn.EXPECT().
		SetPongHandler(gomock.Any()).
		Times(1)

	// Expect conn to write ping
	ts.mockConn.EXPECT().
		WriteMessage(websocket.PingMessage, gomock.Any()).
		Return(nil).
		AnyTimes()

	// Expect conn to set read deadline
	ts.mockConn.EXPECT().
		SetReadDeadline(gomock.Any()).
		Return(nil).
		AnyTimes()

	// Expect conn to read message
	ts.mockConn.EXPECT().
		ReadMessage().
		Return(0, []byte{}, nil).
		AnyTimes()

	// Expect cleanup when connection closes
	ts.mockConn.EXPECT().
		WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), gomock.Any()).
		Return(nil).
		AnyTimes()

	// Expect conn to close
	ts.mockConn.EXPECT().
		Close().
		Return(nil).
		AnyTimes()

	// Test
	err := ts.client.Connect(ts.ctx)
	assert.NoError(t, err, "expected no error, got %v", err)
	assert.True(t, ts.client.IsConnected(), "expected client to be connected")

	// Properly close the client to prevent goroutine leaks
	ts.client.Close()
	assert.False(t, ts.client.IsConnected(), "expected client to be disconnected after close")
}

func TestClient_Connect_Async(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	// Set up mock ticker
	setupMockTicker(ts)

	// Expect time to return default time
	ts.mockClock.EXPECT().
		Now().
		Return(time.Time{}).
		AnyTimes()

	// Expect dialer to dial and return OK response
	ts.mockDialer.EXPECT().
		DialContext(ts.ctx, gomock.Any(), nil).
		Return(ts.mockConn, &http.Response{StatusCode: http.StatusOK}, nil).
		Times(1) // Only one connection should succeed

	// Expect conn to set pong handler
	ts.mockConn.EXPECT().
		SetPongHandler(gomock.Any()).
		Times(1)

	// Expect conn to write ping
	ts.mockConn.EXPECT().
		WriteMessage(websocket.PingMessage, gomock.Any()).
		Return(nil).
		AnyTimes()

	// Expect conn to set read deadline
	ts.mockConn.EXPECT().
		SetReadDeadline(gomock.Any()).
		Return(nil).
		AnyTimes()

	// Expect conn to read message
	ts.mockConn.EXPECT().
		ReadMessage().
		Return(0, []byte{}, nil).
		AnyTimes()

	// Expect cleanup when connection closes
	ts.mockConn.EXPECT().
		WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), gomock.Any()).
		Return(nil).
		AnyTimes()

	// Expect conn to close
	ts.mockConn.EXPECT().
		Close().
		Return(nil).
		AnyTimes()

	// Use channels to coordinate the goroutines
	numGoroutines := 5
	errChan := make(chan error, numGoroutines)
	startChan := make(chan struct{})

	// Start multiple goroutines trying to connect concurrently
	for i := range numGoroutines {
		go func(id int) {
			// Wait for all goroutines to be ready
			<-startChan

			err := ts.client.Connect(ts.ctx)
			errChan <- err
		}(i)
	}

	// Start all goroutines at the same time
	close(startChan)

	// Collect results
	var successCount int
	var alreadyConnectedCount int
	var otherErrors []error

	for range numGoroutines {
		err := <-errChan
		if err == nil {
			successCount++
		} else if errors.Is(err, relayer.ErrAlreadyConnected) {
			alreadyConnectedCount++
		} else {
			otherErrors = append(otherErrors, err)
		}
	}

	// Verify results
	assert.Equal(t, 1, successCount, "Expected exactly one successful connection")
	assert.Equal(t, numGoroutines-1, alreadyConnectedCount, "Expected %d already connected errors", numGoroutines-1)
	assert.Empty(t, otherErrors, "Expected no other errors, got: %v", otherErrors)
	assert.True(t, ts.client.IsConnected(), "Expected client to be connected")

	// Properly close the client to prevent goroutine leaks
	ts.client.Close()
	assert.False(t, ts.client.IsConnected(), "expected client to be disconnected after close")
}

func TestClient_Connect_Failed(t *testing.T) {
	tests := []struct {
		name         string
		inputError   error
		statusCode   int
		expectedType string
	}{
		{
			name:         "503 with bad handshake should be BusyErr",
			inputError:   websocket.ErrBadHandshake,
			statusCode:   503,
			expectedType: "BusyErr",
		},
		{
			name:         "429 with bad handshake should be BusyErr",
			inputError:   websocket.ErrBadHandshake,
			statusCode:   429,
			expectedType: "BusyErr",
		},
		{
			name:         "400 with bad handshake should be PermanentErr",
			inputError:   websocket.ErrBadHandshake,
			statusCode:   400,
			expectedType: "PermanentErr",
		},
		{
			name:         "connection refused should be BusyErr",
			inputError:   syscall.ECONNREFUSED,
			statusCode:   500,
			expectedType: "BusyErr",
		},
		{
			name:         "connection reset should be TransientErr",
			inputError:   syscall.ECONNRESET,
			statusCode:   500,
			expectedType: "TransientErr",
		},
		{
			name:         "broken pipe should be TransientErr",
			inputError:   syscall.EPIPE,
			statusCode:   500,
			expectedType: "TransientErr",
		},
		{
			name:         "host unreachable should be TransientErr",
			inputError:   syscall.EHOSTUNREACH,
			statusCode:   500,
			expectedType: "TransientErr",
		},
		{
			name:         "network unreachable should be TransientErr",
			inputError:   syscall.ENETUNREACH,
			statusCode:   500,
			expectedType: "TransientErr",
		},
		{
			name:         "protocol not supported should be PermanentErr",
			inputError:   syscall.EPROTONOSUPPORT,
			statusCode:   500,
			expectedType: "PermanentErr",
		},
		{
			name:         "address not available should be PermanentErr",
			inputError:   syscall.EADDRNOTAVAIL,
			statusCode:   500,
			expectedType: "PermanentErr",
		},
		{
			name:         "unsupported protocol scheme should be PermanentErr",
			inputError:   &url.Error{Op: "GET", URL: "ftp://example.com", Err: errors.New("unsupported protocol scheme")},
			statusCode:   500,
			expectedType: "PermanentErr",
		},
		{
			name:         "bad request URI should be PermanentErr",
			inputError:   &url.Error{Op: "GET", URL: "http://example.com", Err: errors.New("bad request uri")},
			statusCode:   500,
			expectedType: "PermanentErr",
		},
		{
			name:         "invalid control character in URL should be PermanentErr",
			inputError:   &url.Error{Op: "GET", URL: "http://example.com", Err: errors.New("invalid control character in URL")},
			statusCode:   500,
			expectedType: "PermanentErr",
		},
		{
			name:         "DNS temporary error should be TransientErr",
			inputError:   &net.DNSError{Name: "example.com", Err: "temporary failure", IsTemporary: true},
			statusCode:   500,
			expectedType: "TransientErr",
		},
		{
			name:         "DNS permanent error should be PermanentErr",
			inputError:   &net.DNSError{Name: "example.com", Err: "permanent failure", IsTemporary: false},
			statusCode:   500,
			expectedType: "PermanentErr",
		},
		{
			name:         "network timeout error should be TransientErr",
			inputError:   &net.OpError{Op: "dial", Net: "tcp", Addr: nil, Err: &timeoutError{}},
			statusCode:   500,
			expectedType: "TransientErr",
		},
		{
			name:         "TLS record header error should be TransientErr",
			inputError:   &tls.RecordHeaderError{},
			statusCode:   500,
			expectedType: "TransientErr",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts := setup(t)
			defer ts.teardown()

			// Expect dialer to dial and return error
			ts.mockDialer.EXPECT().
				DialContext(ts.ctx, gomock.Any(), nil).
				Return(nil, &http.Response{StatusCode: tt.statusCode}, tt.inputError)

			// Expect pong handler to not be set
			ts.mockConn.EXPECT().
				SetPongHandler(gomock.Any()).
				Times(0)

			// Expect background reading never to be called
			ts.mockConn.EXPECT().
				ReadMessage().
				Times(0)

			// Test
			err := ts.client.Connect(ts.ctx)
			assert.Error(t, err, "expected error but got nil")

			switch tt.expectedType {
			case "BusyErr":
				var busyErr relayer.BusyError
				assert.True(t, errors.As(err, &busyErr), "Expected BusyErr but got %T", err)
				assert.Equal(t, tt.inputError, busyErr.Err)
			case "TransientErr":
				var transientErr relayer.TransientError
				assert.True(t, errors.As(err, &transientErr), "Expected TransientErr but got %T", err)
				assert.Equal(t, tt.inputError, transientErr.Err)
			case "PermanentErr":
				var permanentErr relayer.PermanentError
				assert.True(t, errors.As(err, &permanentErr), "Expected PermanentErr but got %T", err)
				assert.Equal(t, tt.inputError, permanentErr.Err)
			default:
				t.Fatalf("Unknown error type: %s", tt.expectedType)
			}
		})
	}
}

func TestClient_RetryableConnect_FailsThenSucceeds(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	// Expect time.Sleep to return 100ms
	ts.mockClock.EXPECT().
		Sleep(gomock.Any()).
		DoAndReturn(func(d time.Duration) {
			time.Sleep(100 * time.Millisecond)
		}).
		AnyTimes()

	// Set up mock ticker
	setupMockTicker(ts)

	// Expect time to return default time
	ts.mockClock.EXPECT().
		Now().
		Return(time.Time{}).
		AnyTimes()

	// Expect randomizer to return 1 second for all calls
	ts.mockRandomizer.EXPECT().
		Duration(gomock.Any(), gomock.Any()).
		Return(1 * time.Second).
		AnyTimes()

	// Setup ordered expectations - first two calls fail, third succeeds
	gomock.InOrder(
		// First attempt: Transient error (should retry)
		ts.mockDialer.EXPECT().
			DialContext(ts.ctx, gomock.Any(), nil).
			Return(nil, &http.Response{StatusCode: 500}, syscall.ECONNREFUSED),

		// Second attempt: Busy error (should retry)
		ts.mockDialer.EXPECT().
			DialContext(ts.ctx, gomock.Any(), nil).
			Return(nil, &http.Response{StatusCode: 503}, websocket.ErrBadHandshake),

		// Third attempt: Success
		ts.mockDialer.EXPECT().
			DialContext(ts.ctx, gomock.Any(), nil).
			Return(ts.mockConn, &http.Response{StatusCode: http.StatusOK}, nil),
	)

	// Expect conn to set pong handler
	ts.mockConn.EXPECT().
		SetPongHandler(gomock.Any()).
		Times(1)

	// Expect conn to send ping
	ts.mockConn.EXPECT().
		WriteMessage(websocket.PingMessage, gomock.Any()).
		Return(nil).
		AnyTimes()

	// Expect conn to set read deadline
	ts.mockConn.EXPECT().
		SetReadDeadline(gomock.Any()).
		Return(nil).
		AnyTimes()

	// Expect conn to read message
	ts.mockConn.EXPECT().
		ReadMessage().
		Return(0, []byte{}, nil).
		AnyTimes()

	// Expect cleanup when connection closes
	ts.mockConn.EXPECT().
		WriteControl(websocket.CloseMessage, gomock.Any(), gomock.Any()).
		Return(nil).
		AnyTimes()

	// Expect conn to close
	ts.mockConn.EXPECT().
		Close().
		Return(nil).
		AnyTimes()

	// Test
	err := ts.client.RetryableConnect(ts.ctx)
	assert.NoError(t, err, "expected no error, got %v", err)
	assert.True(t, ts.client.IsConnected(), "expected client to be connected after retries")

	// Properly close the client to prevent goroutine leaks
	ts.client.Close()
	assert.False(t, ts.client.IsConnected(), "expected client to be disconnected after close")
}

func TestClient_RetryableConnect_PermanentError_NoRetry(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	// Permanent error should not be retried
	ts.mockDialer.EXPECT().
		DialContext(ts.ctx, gomock.Any(), nil).
		Return(nil, &http.Response{StatusCode: 400}, websocket.ErrBadHandshake).
		Times(1) // Should only be called once

	// Test - should fail immediately without retries
	err := ts.client.RetryableConnect(ts.ctx)
	assert.Error(t, err)

	// Should be a PermanentErr
	var permanentErr relayer.PermanentError
	assert.True(t, errors.As(err, &permanentErr), "Expected PermanentErr")

	assert.False(t, ts.client.IsConnected(), "expected client to remain disconnected")
}

func TestClient_RetryableConnect_ContextCanceled(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	// Create a context that we'll cancel during the test
	ctx, cancel := context.WithCancel(context.Background())

	// Expect time.Sleep to return 100ms
	ts.mockClock.EXPECT().
		Sleep(gomock.Any()).
		DoAndReturn(func(d time.Duration) {
			time.Sleep(100 * time.Millisecond)
		}).
		AnyTimes()

	// Expect randomizer to return 1 second for all calls
	ts.mockRandomizer.EXPECT().
		Duration(gomock.Any(), gomock.Any()).
		Return(1 * time.Second).
		AnyTimes()

	// Setup to fail first, then allow the retry which should detect the canceled context
	callCount := 0
	ts.mockDialer.EXPECT().
		DialContext(gomock.Any(), gomock.Any(), nil).
		DoAndReturn(func(dialCtx context.Context, url string, headers http.Header) (wrapper.WebSocketConn, *http.Response, error) {
			callCount++

			if callCount == 1 {
				// Cancel context after first failure to simulate cancellation during retry delay
				cancel()
				return nil, &http.Response{StatusCode: 500}, syscall.ECONNREFUSED
			}

			// Second call should detect the canceled context
			// Return the context error that should be detected
			return nil, &http.Response{StatusCode: 500}, dialCtx.Err()
		}).
		Times(2) // Allow for both calls

	// Test - should fail with context canceled
	err := ts.client.RetryableConnect(ctx)
	assert.Error(t, err)

	// Check if it's a context cancellation error (can be wrapped)
	assert.True(t,
		errors.Is(err, context.Canceled) || err.Error() == "context canceled",
		"Expected context.Canceled error, got: %v", err)
	assert.False(t, ts.client.IsConnected(), "expected client to remain disconnected")
	assert.Equal(t, 2, callCount, "expected exactly 2 dial attempts")
}

func TestClient_RetryableConnect_UnknownError_RetryLimitExceeded(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	// Expect time.Sleep to return 100ms for faster testing
	ts.mockClock.EXPECT().
		Sleep(gomock.Any()).
		DoAndReturn(func(d time.Duration) {
			time.Sleep(100 * time.Millisecond)
		}).
		Times(10)

	// Expect randomizer to return 1 second for all calls
	ts.mockRandomizer.EXPECT().
		Duration(gomock.Any(), gomock.Any()).
		Return(1 * time.Second).
		Times(10)

	// Create an unknown error (not categorized as any of our custom error types)
	unknownError := errors.New("some unknown network error")

	// Expect dialer to fail 11 times with unknown error (10 retries + 1 initial attempt)
	ts.mockDialer.EXPECT().
		DialContext(ts.ctx, gomock.Any(), nil).
		Return(nil, &http.Response{StatusCode: 500}, unknownError).
		Times(11)

	// Test - should fail after 10 retries with unknown error
	err := ts.client.RetryableConnect(ts.ctx)
	assert.Error(t, err)
	assert.Equal(t, unknownError, err, "Expected the original unknown error to be returned")

	assert.False(t, ts.client.IsConnected(), "expected client to remain disconnected")
}

func TestClient_RetryableConnect_UnknownError_RetryLimitNotExceeded(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	// Expect time.Sleep to return 100ms for faster testing
	ts.mockClock.EXPECT().
		Sleep(gomock.Any()).
		DoAndReturn(func(d time.Duration) {
			time.Sleep(100 * time.Millisecond)
		}).
		Times(5) // 5 retries

	// Set up mock ticker (only when connection succeeds)
	setupMockTicker(ts)

	// Expect time to return default time (only when ticker is created)
	ts.mockClock.EXPECT().
		Now().
		Return(time.Time{}).
		Times(1) // 1 time when ticker is created

	// Expect randomizer to return 1 second for all calls
	ts.mockRandomizer.EXPECT().
		Duration(gomock.Any(), gomock.Any()).
		Return(1 * time.Second).
		Times(5) // 5 retries

	// Create an unknown error (not categorized as any of our custom error types)
	unknownError := errors.New("some unknown network error")

	// Expect dialer to fail 5 times with unknown error, then succeed
	gomock.InOrder(
		// First 5 attempts: Unknown error (should retry)
		ts.mockDialer.EXPECT().
			DialContext(ts.ctx, gomock.Any(), nil).
			Return(nil, &http.Response{StatusCode: 500}, unknownError),
		ts.mockDialer.EXPECT().
			DialContext(ts.ctx, gomock.Any(), nil).
			Return(nil, &http.Response{StatusCode: 500}, unknownError),
		ts.mockDialer.EXPECT().
			DialContext(ts.ctx, gomock.Any(), nil).
			Return(nil, &http.Response{StatusCode: 500}, unknownError),
		ts.mockDialer.EXPECT().
			DialContext(ts.ctx, gomock.Any(), nil).
			Return(nil, &http.Response{StatusCode: 500}, unknownError),
		ts.mockDialer.EXPECT().
			DialContext(ts.ctx, gomock.Any(), nil).
			Return(nil, &http.Response{StatusCode: 500}, unknownError),
		// 6th attempt: Success
		ts.mockDialer.EXPECT().
			DialContext(ts.ctx, gomock.Any(), nil).
			Return(ts.mockConn, &http.Response{StatusCode: http.StatusOK}, nil),
	)

	// Expect conn to set pong handler
	ts.mockConn.EXPECT().
		SetPongHandler(gomock.Any()).
		Times(1)

	// Expect conn to send ping
	ts.mockConn.EXPECT().
		WriteMessage(websocket.PingMessage, gomock.Any()).
		Return(nil).
		AnyTimes()

	// Expect conn to set read deadline
	ts.mockConn.EXPECT().
		SetReadDeadline(gomock.Any()).
		Return(nil).
		AnyTimes()

	// Expect conn to read message
	ts.mockConn.EXPECT().
		ReadMessage().
		Return(0, []byte{}, nil).
		AnyTimes()

	// Expect cleanup when connection closes
	ts.mockConn.EXPECT().
		WriteControl(websocket.CloseMessage, gomock.Any(), gomock.Any()).
		Return(nil).
		AnyTimes()

	// Expect conn to close
	ts.mockConn.EXPECT().
		Close().
		Return(nil).
		AnyTimes()

	// Test - should succeed after retrying unknown errors
	err := ts.client.RetryableConnect(ts.ctx)
	assert.NoError(t, err, "expected no error after retrying unknown errors")
	assert.True(t, ts.client.IsConnected(), "expected client to be connected after retries")

	// Properly close the client to prevent goroutine leaks
	ts.client.Close()
	assert.False(t, ts.client.IsConnected(), "expected client to be disconnected after close")
}

func TestClient_SendMessage_Success(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	// Set up mock ticker
	setupMockTicker(ts)

	// Expect time.Now() to return default time
	ts.mockClock.EXPECT().
		Now().
		Return(time.Time{}).
		AnyTimes()

	// Expect dialer to dial and return OK response
	ts.mockDialer.EXPECT().
		DialContext(ts.ctx, gomock.Any(), nil).
		Return(ts.mockConn, &http.Response{StatusCode: http.StatusOK}, nil).
		Times(1) // Only one connection should succeed

	// Expect conn to set pong handler
	ts.mockConn.EXPECT().
		SetPongHandler(gomock.Any()).
		Times(1)

	// Expect conn to write ping
	ts.mockConn.EXPECT().
		WriteMessage(websocket.PingMessage, gomock.Any()).
		Return(nil).
		AnyTimes()

	// Expect conn to set read deadline
	ts.mockConn.EXPECT().
		SetReadDeadline(gomock.Any()).
		Return(nil).
		AnyTimes()

	// Expect conn to read message
	ts.mockConn.EXPECT().
		ReadMessage().
		Return(0, []byte{}, nil).
		AnyTimes()

	// Expect cleanup when connection closes
	ts.mockConn.EXPECT().
		WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), gomock.Any()).
		Return(nil).
		AnyTimes()

	// Expect conn to close
	ts.mockConn.EXPECT().
		Close().
		Return(nil).
		AnyTimes()

	// Expect JSON marshal for sending message
	ts.mockJSON.EXPECT().
		Marshal(gomock.Any()).
		Return([]byte("{}"), nil).
		AnyTimes()

	// Expect conn to write message once
	ts.mockConn.EXPECT().
		WriteMessage(gomock.Any(), gomock.Any()).
		Return(nil).
		Times(1)

	// Connect
	err := ts.client.Connect(ts.ctx)
	assert.NoError(t, err, "expected no error, got %v", err)

	// Send message
	err = ts.client.Send(ts.ctx,
		map[string]interface{}{
			"command": "test",
		})
	assert.NoError(t, err, "expected no error, got %v", err)

	// Properly close the client to prevent goroutine leaks
	ts.client.Close()
	assert.False(t, ts.client.IsConnected(), "expected client to be disconnected after close")
}

func TestClient_SendMessage_NotConnected(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	// Test
	err := ts.client.Send(ts.ctx,
		map[string]interface{}{
			"command": "test",
		})
	assert.Error(t, err, "expected error, got nil")
	assert.True(t, errors.Is(err, relayer.ErrNotConnected), "expected ErrNotConnected, got %v", err)
}

func TestClient_Close_Success(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	// Set up mock ticker
	setupMockTicker(ts)

	// Expect time.Now() to return default time
	ts.mockClock.EXPECT().
		Now().
		Return(time.Time{}).
		AnyTimes()

	// Expect dialer to dial and return OK response
	ts.mockDialer.EXPECT().
		DialContext(ts.ctx, gomock.Any(), nil).
		Return(ts.mockConn, &http.Response{StatusCode: http.StatusOK}, nil).
		Times(1)

	// Expect conn to set pong handler
	ts.mockConn.EXPECT().
		SetPongHandler(gomock.Any()).
		Times(1)

	// Expect conn to write ping
	ts.mockConn.EXPECT().
		WriteMessage(websocket.PingMessage, gomock.Any()).
		Return(nil).
		AnyTimes()

	// Expect conn to set read deadline
	ts.mockConn.EXPECT().
		SetReadDeadline(gomock.Any()).
		Return(nil).
		AnyTimes()

	// Expect conn to read message
	ts.mockConn.EXPECT().
		ReadMessage().
		Return(0, []byte{}, nil).
		AnyTimes()

	// Expect cleanup when connection closes
	ts.mockConn.EXPECT().
		WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), gomock.Any()).
		Return(nil).
		Times(1)

	// Expect conn to close
	ts.mockConn.EXPECT().
		Close().
		Return(nil).
		Times(1)

	// Connect
	err := ts.client.Connect(ts.ctx)
	assert.NoError(t, err, "expected no error, got %v", err)
	assert.True(t, ts.client.IsConnected(), "expected client to be connected")

	// Close
	ts.client.Close()
	assert.False(t, ts.client.IsConnected(), "expected client to be disconnected after close")
}

func TestClient_Close_Error(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	// 1. Close not connected client
	ts.client.Close()
	assert.False(t, ts.client.IsConnected(), "expected client to be disconnected after close")

	// 2. Close already closed client
	// Set up mock ticker
	setupMockTicker(ts)

	// Expect time.Now() to return default time
	ts.mockClock.EXPECT().
		Now().
		Return(time.Time{}).
		AnyTimes()

	// Expect dialer to dial and return OK response
	ts.mockDialer.EXPECT().
		DialContext(ts.ctx, gomock.Any(), nil).
		Return(ts.mockConn, &http.Response{StatusCode: http.StatusOK}, nil).
		Times(1)

	// Expect conn to set pong handler
	ts.mockConn.EXPECT().
		SetPongHandler(gomock.Any()).
		Times(1)

	// Expect conn to write ping
	ts.mockConn.EXPECT().
		WriteMessage(websocket.PingMessage, gomock.Any()).
		Return(nil).
		AnyTimes()

	// Expect conn to set read deadline
	ts.mockConn.EXPECT().
		SetReadDeadline(gomock.Any()).
		Return(nil).
		AnyTimes()

	// Expect conn to read message
	ts.mockConn.EXPECT().
		ReadMessage().
		Return(0, []byte{}, nil).
		AnyTimes()

	// Expect write control to return error
	ts.mockConn.EXPECT().
		WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), gomock.Any()).
		Return(errors.New("test error 1")).
		Times(1)

	// Expect conn to close to return error
	ts.mockConn.EXPECT().
		Close().
		Return(nil).
		Times(1)

	// Connect
	err := ts.client.Connect(ts.ctx)
	assert.NoError(t, err, "expected no error, got %v", err)
	assert.True(t, ts.client.IsConnected(), "expected client to be connected")

	// Close
	ts.client.Close()
	assert.False(t, ts.client.IsConnected(), "expected client to be disconnected after close")
}

func TestClient_Close_Async(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	// Set up mock ticker
	setupMockTicker(ts)

	// Expect time.Now() to return default time
	ts.mockClock.EXPECT().
		Now().
		Return(time.Time{}).
		AnyTimes()

	// Expect dialer to dial and return OK response
	ts.mockDialer.EXPECT().
		DialContext(ts.ctx, gomock.Any(), nil).
		Return(ts.mockConn, &http.Response{StatusCode: http.StatusOK}, nil).
		Times(1)

	// Expect conn to set pong handler
	ts.mockConn.EXPECT().
		SetPongHandler(gomock.Any()).
		Times(1)

	// Expect conn to read message
	ts.mockConn.EXPECT().
		ReadMessage().
		Return(0, []byte{}, nil).
		AnyTimes()

	// Track the number of WriteControl and Close calls to ensure they're reasonable
	var writeControlCalls, closeCalls int32

	// Expect cleanup when connection closes - should happen at least once but not excessively
	ts.mockConn.EXPECT().
		WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), gomock.Any()).
		DoAndReturn(func(messageType int, data []byte, deadline time.Time) error {
			atomic.AddInt32(&writeControlCalls, 1)
			return nil
		}).
		AnyTimes()

	// Expect conn to close - should happen at least once but not excessively
	ts.mockConn.EXPECT().
		Close().
		DoAndReturn(func() error {
			atomic.AddInt32(&closeCalls, 1)
			return nil
		}).
		AnyTimes()

	// Connect
	err := ts.client.Connect(ts.ctx)
	assert.NoError(t, err, "expected no error, got %v", err)
	assert.True(t, ts.client.IsConnected(), "expected client to be connected")

	// Close the client in different goroutines
	const numberOfGoroutines = 10
	startChan := make(chan struct{})
	closedChan := make(chan struct{}, numberOfGoroutines)

	// Channel to capture any panics that might occur during Close()
	panicChan := make(chan interface{}, numberOfGoroutines)

	for range numberOfGoroutines {
		go func() {
			defer func() {
				if r := recover(); r != nil {
					panicChan <- r
				}
				closedChan <- struct{}{}
			}()

			<-startChan
			ts.client.Close()
		}()
	}

	// Close the start channel to unblock all goroutines simultaneously
	close(startChan)

	// Wait for all goroutines to finish
	for range numberOfGoroutines {
		<-closedChan
	}

	// Check for any panics
	close(panicChan)
	var panics []interface{}
	for panic := range panicChan {
		panics = append(panics, panic)
	}
	assert.Empty(t, panics, "Expected no panics during concurrent close, but got: %v", panics)

	// Verify final state
	assert.False(t, ts.client.IsConnected(), "expected client to be disconnected after close")

	// Verify that cleanup methods were called a reasonable number of times
	writeControlCallCount := atomic.LoadInt32(&writeControlCalls)
	closeCallCount := atomic.LoadInt32(&closeCalls)

	assert.Equal(t, int(writeControlCallCount), 1, "WriteControl should be called once")
	assert.Equal(t, int(closeCallCount), 1, "Close should be called once")
}

func TestClient_SendNotification_Success(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	t.Log("TODO: Implement this test")
}

func TestClient_SendNotification_Error(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	t.Log("TODO: Implement this test")
}

func TestClient_ReceiveMessage_Success(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	// Setup payload
	messageID := "test_id"
	command := "test"
	p := map[string]any{
		"messageID": messageID,
		"message": map[string]any{
			"command": command,
		},
	}

	// Setup handler
	handler1Called := make(chan struct{})
	handler2Called := make(chan struct{})
	handler1 := func(ctx context.Context, payload relayer.Payload) error {
		assert.Equal(t, messageID, payload.MessageID, "expected messageID to be %s but got %s", messageID, payload.MessageID)
		assert.NotNil(t, payload.Message.Command, "expected command to be not nil")
		assert.Equal(t, command, *payload.Message.Command, "expected command to be %s but got %s", command, *payload.Message.Command)
		handler1Called <- struct{}{}
		return nil
	}
	handler2 := func(ctx context.Context, payload relayer.Payload) error {
		handler2Called <- struct{}{}
		return nil
	}
	// Add handlers
	ts.client.OnRelayerMessage(handler1)
	ts.client.OnRelayerMessage(handler2)

	// Remove handler2
	ts.client.RemoveRelayerMessage(handler2)

	// Set up mock ticker
	setupMockTicker(ts)

	// Expect time.Now() to return default time
	ts.mockClock.EXPECT().
		Now().
		Return(time.Time{}).
		AnyTimes()

	// Expect dialer to dial and return OK response
	ts.mockDialer.EXPECT().
		DialContext(ts.ctx, gomock.Any(), nil).
		Return(ts.mockConn, &http.Response{StatusCode: http.StatusOK}, nil).
		Times(1)

	// Expect conn to set pong handler
	ts.mockConn.EXPECT().
		SetPongHandler(gomock.Any()).
		Times(1)

	// Expect conn to write ping
	ts.mockConn.EXPECT().
		WriteMessage(websocket.PingMessage, gomock.Any()).
		Return(nil).
		AnyTimes()

	// Expect conn to set read deadline
	ts.mockConn.EXPECT().
		SetReadDeadline(gomock.Any()).
		Return(nil).
		AnyTimes()

	// Expect conn to read message with payload
	pb, err := json.Marshal(p)
	assert.NoError(t, err, "expected no error, got %v", err)
	ts.mockConn.EXPECT().
		ReadMessage().
		Return(0, pb, nil).
		AnyTimes()

	// Expect cleanup when connection closes
	ts.mockConn.EXPECT().
		WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), gomock.Any()).
		Return(nil).
		AnyTimes()

	// Expect conn to close
	ts.mockConn.EXPECT().
		Close().
		Return(nil).
		AnyTimes()

	// Connect
	err = ts.client.Connect(ts.ctx)
	assert.NoError(t, err, "expected no error, got %v", err)
	assert.True(t, ts.client.IsConnected(), "expected client to be connected")

	// Wait for the test done
	select {
	case <-handler1Called:
		// Good
	case <-time.After(100 * time.Millisecond):
		t.Fatal("handler1 was not called")
	}

	select {
	case <-handler2Called:
		t.Fatalf("handler2 should not be called")
	case <-time.After(100 * time.Millisecond):
		// Good
	}

	// Properly close the client to prevent goroutine leaks
	ts.client.Close()
	assert.False(t, ts.client.IsConnected(), "expected client to be disconnected after close")
}

func TestClient_ReceiveMessage_Error(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	// Create a second mock connection for the reconnection
	mockConn2 := mocks.NewMockWebSocketConn(ts.ctrl)

	// Set up mock ticker
	setupMockTicker(ts)

	// Expect time.Now() to return default time
	ts.mockClock.EXPECT().
		Now().
		Return(time.Time{}).
		AnyTimes()

	// Expect randomizer to return 1 second for retry delay
	ts.mockRandomizer.EXPECT().
		Duration(gomock.Any(), gomock.Any()).
		Return(1 * time.Second).
		AnyTimes()

	// Setup ordered expectations for initial connection
	ts.mockDialer.EXPECT().
		DialContext(ts.ctx, gomock.Any(), nil).
		Return(ts.mockConn, &http.Response{StatusCode: http.StatusOK}, nil).
		Times(1)

	// Expect conn to set pong handler
	ts.mockConn.EXPECT().
		SetPongHandler(gomock.Any()).
		Times(1)

	// Expect conn to write ping
	ts.mockConn.EXPECT().
		WriteMessage(websocket.PingMessage, gomock.Any()).
		Return(nil).
		AnyTimes()

	// Expect conn to set read deadline
	ts.mockConn.EXPECT().
		SetReadDeadline(gomock.Any()).
		Return(nil).
		AnyTimes()

	// Expect conn to read message with error (this triggers reconnection)
	readErrorOccurred := make(chan struct{})
	ts.mockConn.EXPECT().
		ReadMessage().
		DoAndReturn(func() (int, []byte, error) {
			close(readErrorOccurred)
			return 0, []byte{}, errors.New("test read error")
		}).
		Times(1)

	// Expect cleanup when first connection closes due to error
	ts.mockConn.EXPECT().
		WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), gomock.Any()).
		Return(nil).
		Times(1)

	ts.mockConn.EXPECT().
		Close().
		Return(nil).
		Times(1)

	// Setup expectations for reconnection attempt (second connection)
	ts.mockDialer.EXPECT().
		DialContext(ts.ctx, gomock.Any(), nil).
		Return(mockConn2, &http.Response{StatusCode: http.StatusOK}, nil).
		Times(1)

	// Expect second conn to set pong handler
	mockConn2.EXPECT().
		SetPongHandler(gomock.Any()).
		Times(1)

	// Expect second conn to write ping
	mockConn2.EXPECT().
		WriteMessage(websocket.PingMessage, gomock.Any()).
		Return(nil).
		AnyTimes()

	// Expect second conn to set read deadline
	mockConn2.EXPECT().
		SetReadDeadline(gomock.Any()).
		Return(nil).
		AnyTimes()

	// Expect second conn to read message successfully
	reconnectedReadCalled := make(chan struct{})
	once := sync.Once{}
	mockConn2.EXPECT().
		ReadMessage().
		DoAndReturn(func() (int, []byte, error) {
			once.Do(func() {
				close(reconnectedReadCalled)
			})
			return 0, []byte{}, nil
		}).
		MinTimes(1)

	// Expect cleanup when second connection closes
	mockConn2.EXPECT().
		WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), gomock.Any()).
		Return(nil).
		AnyTimes()

	mockConn2.EXPECT().
		Close().
		Return(nil).
		AnyTimes()

	// Connect initially
	err := ts.client.Connect(ts.ctx)
	assert.NoError(t, err, "expected no error, got %v", err)
	assert.True(t, ts.client.IsConnected(), "expected client to be connected")

	// Wait for the read error to occur
	select {
	case <-readErrorOccurred:
		// Good, read error occurred
	case <-time.After(1 * time.Second):
		t.Fatal("read error should have occurred")
	}

	// Wait for reconnection to complete
	select {
	case <-reconnectedReadCalled:
		// Good, reconnection successful
	case <-time.After(1 * time.Second):
		t.Fatal("reconnection should have completed")
	}

	// Verify client is still connected after reconnection
	assert.True(t, ts.client.IsConnected(), "expected client to be connected after reconnection")

	// Properly close the client to prevent goroutine leaks
	ts.client.Close()
	assert.False(t, ts.client.IsConnected(), "expected client to be disconnected after close")
}

func TestClient_ReadMessage_ErrorAfterClose_DoesNotReconnect(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	setupMockTicker(ts)

	ts.mockClock.EXPECT().
		Now().
		Return(time.Time{}).
		AnyTimes()

	ts.mockDialer.EXPECT().
		DialContext(ts.ctx, gomock.Any(), nil).
		Return(ts.mockConn, &http.Response{StatusCode: http.StatusOK}, nil).
		Times(1)

	ts.mockConn.EXPECT().
		SetPongHandler(gomock.Any()).
		Times(1)

	ts.mockConn.EXPECT().
		WriteMessage(websocket.PingMessage, gomock.Any()).
		Return(nil).
		AnyTimes()

	ts.mockConn.EXPECT().
		SetReadDeadline(gomock.Any()).
		Return(nil).
		AnyTimes()

	readStarted := make(chan struct{})
	releaseRead := make(chan struct{})
	readReturned := make(chan struct{})
	ts.mockConn.EXPECT().
		ReadMessage().
		DoAndReturn(func() (int, []byte, error) {
			close(readStarted)
			<-releaseRead
			defer close(readReturned)
			return 0, nil, &websocket.CloseError{Code: websocket.CloseAbnormalClosure, Text: "unexpected EOF"}
		}).
		Times(1)

	ts.mockConn.EXPECT().
		WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), gomock.Any()).
		Return(nil).
		Times(1)

	releaseOnce := sync.Once{}
	ts.mockConn.EXPECT().
		Close().
		DoAndReturn(func() error {
			releaseOnce.Do(func() { close(releaseRead) })
			return nil
		}).
		Times(1)

	err := ts.client.Connect(ts.ctx)
	assert.NoError(t, err, "expected no error, got %v", err)

	select {
	case <-readStarted:
	case <-time.After(1 * time.Second):
		t.Fatal("ReadMessage should have started")
	}

	ts.client.Close()

	select {
	case <-readReturned:
	case <-time.After(1 * time.Second):
		t.Fatal("read loop should return after close")
	}

	assert.False(t, ts.client.IsConnected(), "expected client to remain disconnected after close")
}

func TestClient_Ping_Success(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	// Set up mock ticker so ping fires quickly
	setupMockTicker(ts)

	// Expect time.Now() to return default time
	ts.mockClock.EXPECT().
		Now().
		Return(time.Time{}).
		AnyTimes()

	// Expect dialer to dial and return OK response
	ts.mockDialer.EXPECT().
		DialContext(ts.ctx, gomock.Any(), nil).
		Return(ts.mockConn, &http.Response{StatusCode: http.StatusOK}, nil)

	// Expect conn to set pong handler
	ts.mockConn.EXPECT().
		SetPongHandler(gomock.Any()).
		Times(1)

	// Expect conn to send ping - add channel to synchronize
	pingCalled := make(chan struct{})
	once := sync.Once{}
	ts.mockConn.EXPECT().
		WriteMessage(websocket.PingMessage, []byte("ping")).
		DoAndReturn(func(messageType int, data []byte) error {
			once.Do(func() {
				close(pingCalled)
			})
			return nil
		}).
		MinTimes(1)

	// Expect conn to set read deadline
	ts.mockConn.EXPECT().
		SetReadDeadline(time.Time{}.Add(relayer.PONG_WAIT)).
		Return(nil).
		MinTimes(1) // Ping method should call this at least once

	// Expect conn to read message
	ts.mockConn.EXPECT().
		ReadMessage().
		Return(0, []byte{}, nil).
		MinTimes(1)

	// Expect cleanup when connection closes
	ts.mockConn.EXPECT().
		WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), gomock.Any()).
		Return(nil).
		AnyTimes()

	// Expect conn to close
	ts.mockConn.EXPECT().
		Close().
		Return(nil).
		AnyTimes()

	// Connect
	err := ts.client.Connect(ts.ctx)
	assert.NoError(t, err, "expected no error, got %v", err)
	assert.True(t, ts.client.IsConnected(), "expected client to be connected")

	// Wait for ping to be called before closing
	select {
	case <-pingCalled:
		// Good, ping was sent
	case <-time.After(500 * time.Millisecond):
		t.Fatal("ping should have been sent within 500ms")
	}

	// Properly close the client to prevent goroutine leaks
	ts.client.Close()
	assert.False(t, ts.client.IsConnected(), "expected client to be disconnected after close")
}

func TestClient_Ping_Error(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	// Set up mock ticker so ping fires quickly
	setupMockTicker(ts)

	// Expect time.Now() to return default time
	ts.mockClock.EXPECT().
		Now().
		Return(time.Time{}).
		AnyTimes()

	// Expect dialer to dial and return OK response
	ts.mockDialer.EXPECT().
		DialContext(ts.ctx, gomock.Any(), nil).
		Return(ts.mockConn, &http.Response{StatusCode: http.StatusOK}, nil)

	// Expect conn to set pong handler
	ts.mockConn.EXPECT().
		SetPongHandler(gomock.Any()).
		Times(1)

	// Expect conn to write ping
	pingCalled := make(chan struct{})
	once := sync.Once{}
	ts.mockConn.EXPECT().
		WriteMessage(websocket.PingMessage, gomock.Any()).
		DoAndReturn(func(messageType int, data []byte) error {
			once.Do(func() {
				close(pingCalled)
			})
			return errors.New("test write error")
		}).
		MinTimes(1)

	// Expect conn call read message
	ts.mockConn.EXPECT().
		ReadMessage().
		Return(0, []byte{}, nil).
		AnyTimes()

	// Expect cleanup when connection closes
	ts.mockConn.EXPECT().
		WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), gomock.Any()).
		Return(nil).
		AnyTimes()

	// Expect conn to close
	ts.mockConn.EXPECT().
		Close().
		Return(nil).
		AnyTimes()

	// Connect
	err := ts.client.Connect(ts.ctx)
	assert.NoError(t, err, "expected no error, got %v", err)
	assert.True(t, ts.client.IsConnected(), "expected client to be connected")

	// Wait for ping to be called before closing
	select {
	case <-pingCalled:
		// Good, ping was sent
	case <-time.After(500 * time.Millisecond):
		t.Fatal("ping should have been sent within 500ms")
	}

	// Properly close the client to prevent goroutine leaks
	ts.client.Close()
	assert.False(t, ts.client.IsConnected(), "expected client to be disconnected after close")
}

func TestClient_Ping_ContextCanceled(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	// Create a context that we can cancel
	ctx, cancel := context.WithCancel(context.Background())

	// Set up mock ticker so ping fires quickly
	setupMockTicker(ts)

	// Expect time.Now() to return default time
	ts.mockClock.EXPECT().
		Now().
		Return(time.Time{}).
		AnyTimes()

	// Expect dialer to dial and return OK response
	ts.mockDialer.EXPECT().
		DialContext(ctx, gomock.Any(), nil).
		Return(ts.mockConn, &http.Response{StatusCode: http.StatusOK}, nil)

	// Expect conn to set pong handler
	ts.mockConn.EXPECT().
		SetPongHandler(gomock.Any()).
		Times(1)

	// Track ping calls to ensure they stop after context cancellation
	var pingCallCount int32
	firstPingCalled := make(chan struct{})
	pingAfterCancel := make(chan struct{})

	ts.mockConn.EXPECT().
		WriteMessage(websocket.PingMessage, []byte("ping")).
		DoAndReturn(func(messageType int, data []byte) error {
			count := atomic.AddInt32(&pingCallCount, 1)
			if count == 1 {
				close(firstPingCalled)
				cancel()
			} else if count >= 2 {
				select {
				case pingAfterCancel <- struct{}{}:
				default:
				}
			}
			return nil
		}).
		AnyTimes()

	// Expect conn to set read deadline
	ts.mockConn.EXPECT().
		SetReadDeadline(gomock.Any()).
		Return(nil).
		AnyTimes()

	// Expect conn to read message
	ts.mockConn.EXPECT().
		ReadMessage().
		Return(0, []byte{}, nil).
		AnyTimes()

	// Expect cleanup when connection closes
	ts.mockConn.EXPECT().
		WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), gomock.Any()).
		Return(nil).
		AnyTimes()

	// Expect conn to close
	ts.mockConn.EXPECT().
		Close().
		Return(nil).
		AnyTimes()

	// Connect
	err := ts.client.Connect(ctx)
	assert.NoError(t, err, "expected no error, got %v", err)
	assert.True(t, ts.client.IsConnected(), "expected client to be connected")

	// Wait for first ping to be called
	select {
	case <-firstPingCalled:
		// Good, first ping was sent
	case <-time.After(500 * time.Millisecond):
		t.Fatal("first ping should have been sent within 500ms")
	}

	// Wait a bit to ensure no more pings are sent after cancellation
	select {
	case <-pingAfterCancel:
		t.Fatal("ping should not be called after context cancellation")
	case <-time.After(300 * time.Millisecond):
		// Good, no pings after cancellation
	}

	// Verify that ping calls stopped (should be 1 total calls)
	finalPingCount := atomic.LoadInt32(&pingCallCount)
	assert.Equal(t, int(finalPingCount), 1, "expected ping calls to stop after context cancellation, got %d calls", finalPingCount)

	// Properly close the client to prevent goroutine leaks
	ts.client.Close()
	assert.False(t, ts.client.IsConnected(), "expected client to be disconnected after close")
}

func TestClient_ReadMessage_PermanentError_ExitsProgram(t *testing.T) {
	ts := setup(t)
	defer ts.ctrl.Finish()

	// Set up mock ticker
	setupMockTicker(ts)

	// Expect time.Now() to return default time
	ts.mockClock.EXPECT().
		Now().
		Return(time.Time{}).
		AnyTimes()

	// Setup ordered expectations for initial connection
	ts.mockDialer.EXPECT().
		DialContext(ts.ctx, gomock.Any(), nil).
		Return(ts.mockConn, &http.Response{StatusCode: http.StatusOK}, nil).
		Times(1)

	// Expect conn to set pong handler
	ts.mockConn.EXPECT().
		SetPongHandler(gomock.Any()).
		Times(1)

	// Expect conn to write ping
	ts.mockConn.EXPECT().
		WriteMessage(websocket.PingMessage, gomock.Any()).
		Return(nil).
		AnyTimes()

	// Expect conn to set read deadline
	ts.mockConn.EXPECT().
		SetReadDeadline(gomock.Any()).
		Return(nil).
		AnyTimes()

	// Expect conn to read message with error (this triggers reconnection attempt)
	readErrorOccurred := make(chan struct{})
	ts.mockConn.EXPECT().
		ReadMessage().
		DoAndReturn(func() (int, []byte, error) {
			close(readErrorOccurred)
			return 0, []byte{}, errors.New("test read error")
		}).
		Times(1)

	// Expect cleanup when first connection closes due to error
	ts.mockConn.EXPECT().
		WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), gomock.Any()).
		Return(nil).
		Times(1)

	ts.mockConn.EXPECT().
		Close().
		Return(nil).
		Times(1)

	// Expect reconnection attempt to fail with PermanentError
	permanentErr := relayer.PermanentError{Err: errors.New("permanent connection error")}
	ts.mockDialer.EXPECT().
		DialContext(ts.ctx, gomock.Any(), nil).
		Return(nil, nil, permanentErr).
		Times(1)

	// Expect os.Exit(1) to be called when reconnection fails with PermanentError
	exitCalled := make(chan struct{})
	ts.mockOS.EXPECT().
		Exit(1).
		DoAndReturn(func(code int) {
			close(exitCalled)
		}).
		Times(1)

	// Test - Connect (this automatically starts background message reading)
	err := ts.client.Connect(ts.ctx)
	assert.NoError(t, err, "expected no error during initial connection, got %v", err)

	// Wait for ReadMessage error to occur
	select {
	case <-readErrorOccurred:
		// ReadMessage error occurred, continue
	case <-time.After(5 * time.Second):
		t.Fatal("Expected ReadMessage error to occur within timeout")
	}

	// Wait for os.Exit(1) to be called
	select {
	case <-exitCalled:
		// Exit was called as expected
	case <-time.After(5 * time.Second):
		t.Fatal("Expected os.Exit(1) to be called within timeout")
	}
}
