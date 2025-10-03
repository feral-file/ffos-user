package ws_test

import (
	"context"
	"errors"
	"fmt"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/golang/mock/gomock"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest"

	"github.com/feral-file/ffos-user/components/feral-controld/mocks"
	"github.com/feral-file/ffos-user/components/feral-controld/ws"
)

type testSetup struct {
	ctrl         *gomock.Controller
	ctx          context.Context
	mockUpgrader *mocks.MockWebsocketUpgrader
	mockClock    *mocks.MockClock
	mockConn     *mocks.MockWebSocketConn
	ws           ws.WS
}

func setup(t *testing.T) *testSetup {
	ctrl := gomock.NewController(t)
	logger := zaptest.NewLogger(t, zaptest.Level(zap.FatalLevel))
	ctx := context.Background()

	// Dependencies
	mockUpgrader := mocks.NewMockWebsocketUpgrader(ctrl)
	mockClock := mocks.NewMockClock(ctrl)
	mockConn := mocks.NewMockWebSocketConn(ctrl)

	ws := ws.NewWSHandler(ctx, mockUpgrader, mockClock, logger)

	return &testSetup{
		ctrl:         ctrl,
		ctx:          ctx,
		mockUpgrader: mockUpgrader,
		mockClock:    mockClock,
		mockConn:     mockConn,
		ws:           ws,
	}
}

func (ts *testSetup) teardown() {
	ts.ctrl.Finish()
}

func TestNewConnection_Success(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	// Create a test HTTP request
	req := httptest.NewRequest("GET", "/ws", nil)
	w := httptest.NewRecorder()

	// Mock the upgrader to return a successful connection
	ts.mockUpgrader.EXPECT().
		Upgrade(gomock.Any(), gomock.Any(), gomock.Any()).
		Return(ts.mockConn, nil)

	// Mock connection methods for background goroutine
	ts.mockConn.EXPECT().
		ReadMessage().
		Return(0, nil, errors.New("connection closed")).
		AnyTimes()

	ts.mockConn.EXPECT().
		WriteControl(gomock.Any(), gomock.Any(), gomock.Any()).
		Return(nil).
		AnyTimes()

	ts.mockConn.EXPECT().
		Close().
		Return(nil).
		AnyTimes()

	// Mock clock for close message deadline
	ts.mockClock.EXPECT().
		Now().
		Return(time.Now()).
		AnyTimes()

	connID, err := ts.ws.NewConnection(w, req)

	assert.NoError(t, err)
	assert.Equal(t, "conn-1", connID)
}

func TestNewConnection_UpgradeFailure(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	// Create a test HTTP request
	req := httptest.NewRequest("GET", "/ws", nil)
	w := httptest.NewRecorder()

	// Mock the upgrader to return an error
	expectedErr := errors.New("upgrade failed")
	ts.mockUpgrader.EXPECT().
		Upgrade(gomock.Any(), gomock.Any(), gomock.Any()).
		Return(nil, expectedErr)

	connID, err := ts.ws.NewConnection(w, req)

	assert.Error(t, err)
	assert.Equal(t, expectedErr, err)
	assert.Empty(t, connID)
}

func TestSend_Success(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	// Create a test HTTP request
	req := httptest.NewRequest("GET", "/ws", nil)
	w := httptest.NewRecorder()

	// Mock the upgrader to return a successful connection
	ts.mockUpgrader.EXPECT().
		Upgrade(gomock.Any(), gomock.Any(), gomock.Any()).
		Return(ts.mockConn, nil)

	// Mock connection methods for background goroutine
	ts.mockConn.EXPECT().
		ReadMessage().
		Return(0, nil, errors.New("connection closed")).
		AnyTimes()

	ts.mockConn.EXPECT().
		WriteControl(gomock.Any(), gomock.Any(), gomock.Any()).
		Return(nil).
		AnyTimes()

	ts.mockConn.EXPECT().
		Close().
		Return(nil).
		AnyTimes()

	// Mock clock for close message deadline
	ts.mockClock.EXPECT().
		Now().
		Return(time.Now()).
		AnyTimes()

	// Create connection
	connID, err := ts.ws.NewConnection(w, req)
	assert.NoError(t, err)

	// Mock WriteJSON for Send
	message := map[string]string{"test": "message"}
	ts.mockConn.EXPECT().
		WriteJSON(message).
		Return(nil)

	// Test Send
	err = ts.ws.Send(connID, message)
	assert.NoError(t, err)
}

func TestSend_ConnectionNotFound(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	message := map[string]string{"test": "message"}
	err := ts.ws.Send("non-existent-conn", message)

	assert.Error(t, err)
	assert.Equal(t, err.Error(), "connection non-existent-conn not found")
}

func TestSend_WriteError(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	// Create a test HTTP request
	req := httptest.NewRequest("GET", "/ws", nil)
	w := httptest.NewRecorder()

	// Mock the upgrader to return a successful connection
	ts.mockUpgrader.EXPECT().
		Upgrade(gomock.Any(), gomock.Any(), gomock.Any()).
		Return(ts.mockConn, nil)

	// Mock connection methods for background goroutine
	ts.mockConn.EXPECT().
		ReadMessage().
		Return(0, nil, errors.New("connection closed")).
		AnyTimes()

	ts.mockConn.EXPECT().
		WriteControl(gomock.Any(), gomock.Any(), gomock.Any()).
		Return(nil).
		AnyTimes()

	ts.mockConn.EXPECT().
		Close().
		Return(nil).
		AnyTimes()

	// Mock clock for close message deadline
	ts.mockClock.EXPECT().
		Now().
		Return(time.Now()).
		AnyTimes()

	// Create connection
	connID, err := ts.ws.NewConnection(w, req)
	assert.NoError(t, err)

	// Mock WriteJSON to return an error
	message := map[string]string{"test": "message"}
	writeErr := errors.New("write failed")
	ts.mockConn.EXPECT().
		WriteJSON(message).
		Return(writeErr)

	// Test Send
	err = ts.ws.Send(connID, message)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "failed to send message")
}

func TestSendAll_Success(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	// Create additional mock connection for second connection
	mockConn2 := mocks.NewMockWebSocketConn(ts.ctrl)

	// Create two test HTTP requests
	req1 := httptest.NewRequest("GET", "/ws", nil)
	w1 := httptest.NewRecorder()
	req2 := httptest.NewRequest("GET", "/ws", nil)
	w2 := httptest.NewRecorder()

	// Mock the upgrader to return successful connections
	ts.mockUpgrader.EXPECT().
		Upgrade(gomock.Any(), gomock.Any(), gomock.Any()).
		Return(ts.mockConn, nil).
		Times(1)

	ts.mockUpgrader.EXPECT().
		Upgrade(gomock.Any(), gomock.Any(), gomock.Any()).
		Return(mockConn2, nil).
		Times(1)

	// Mock connection methods for background goroutines
	ts.mockConn.EXPECT().
		ReadMessage().
		Return(0, nil, errors.New("connection closed")).
		AnyTimes()

	ts.mockConn.EXPECT().
		WriteControl(gomock.Any(), gomock.Any(), gomock.Any()).
		Return(nil).
		AnyTimes()

	ts.mockConn.EXPECT().
		Close().
		Return(nil).
		AnyTimes()

	mockConn2.EXPECT().
		ReadMessage().
		Return(0, nil, errors.New("connection closed")).
		AnyTimes()

	mockConn2.EXPECT().
		WriteControl(gomock.Any(), gomock.Any(), gomock.Any()).
		Return(nil).
		AnyTimes()

	mockConn2.EXPECT().
		Close().
		Return(nil).
		AnyTimes()

	// Mock clock for close message deadline
	ts.mockClock.EXPECT().
		Now().
		Return(time.Now()).
		AnyTimes()

	// Mock WriteJSON for both connections
	message := map[string]string{"test": "broadcast"}
	ts.mockConn.EXPECT().
		WriteJSON(message).
		Return(nil)

	mockConn2.EXPECT().
		WriteJSON(message).
		Return(nil)

	// Create two connections
	_, err := ts.ws.NewConnection(w1, req1)
	assert.NoError(t, err)

	_, err = ts.ws.NewConnection(w2, req2)
	assert.NoError(t, err)

	// Test SendAll
	err = ts.ws.SendAll(message)
	assert.NoError(t, err)
}

func TestSendAll_PartialFailure(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	// Create additional mock connection for second connection
	mockConn2 := mocks.NewMockWebSocketConn(ts.ctrl)

	// Create two test HTTP requests
	req1 := httptest.NewRequest("GET", "/ws", nil)
	w1 := httptest.NewRecorder()
	req2 := httptest.NewRequest("GET", "/ws", nil)
	w2 := httptest.NewRecorder()

	// Mock the upgrader to return successful connections
	ts.mockUpgrader.EXPECT().
		Upgrade(gomock.Any(), gomock.Any(), gomock.Any()).
		Return(ts.mockConn, nil).
		Times(1)

	ts.mockUpgrader.EXPECT().
		Upgrade(gomock.Any(), gomock.Any(), gomock.Any()).
		Return(mockConn2, nil).
		Times(1)

	// Mock connection methods for background goroutines
	ts.mockConn.EXPECT().
		ReadMessage().
		Return(0, nil, errors.New("connection closed")).
		AnyTimes()

	ts.mockConn.EXPECT().
		WriteControl(gomock.Any(), gomock.Any(), gomock.Any()).
		Return(nil).
		AnyTimes()

	ts.mockConn.EXPECT().
		Close().
		Return(nil).
		AnyTimes()

	mockConn2.EXPECT().
		ReadMessage().
		Return(0, nil, errors.New("connection closed")).
		AnyTimes()

	mockConn2.EXPECT().
		WriteControl(gomock.Any(), gomock.Any(), gomock.Any()).
		Return(nil).
		AnyTimes()

	mockConn2.EXPECT().
		Close().
		Return(nil).
		AnyTimes()

	// Mock clock for close message deadline
	ts.mockClock.EXPECT().
		Now().
		Return(time.Now()).
		AnyTimes()

	// Mock WriteJSON - first succeeds, second fails
	message := map[string]string{"test": "broadcast"}
	writeErr := errors.New("write failed")
	ts.mockConn.EXPECT().
		WriteJSON(message).
		Return(nil)

	mockConn2.EXPECT().
		WriteJSON(message).
		Return(writeErr)

	// Create two connections
	_, err := ts.ws.NewConnection(w1, req1)
	assert.NoError(t, err)

	_, err = ts.ws.NewConnection(w2, req2)
	assert.NoError(t, err)

	// Test SendAll - should return the last error
	err = ts.ws.SendAll(message)
	assert.Error(t, err)
	assert.Equal(t, writeErr, err)

	// Mock connection methods for background goroutines after SendAll
	ts.mockConn.EXPECT().
		ReadMessage().
		Return(0, nil, errors.New("connection closed")).
		AnyTimes()

	ts.mockConn.EXPECT().
		WriteControl(gomock.Any(), gomock.Any(), gomock.Any()).
		Return(nil).
		AnyTimes()

	ts.mockConn.EXPECT().
		Close().
		Return(nil).
		AnyTimes()

	mockConn2.EXPECT().
		ReadMessage().
		Return(0, nil, errors.New("connection closed")).
		AnyTimes()

	mockConn2.EXPECT().
		WriteControl(gomock.Any(), gomock.Any(), gomock.Any()).
		Return(nil).
		AnyTimes()

	mockConn2.EXPECT().
		Close().
		Return(nil).
		AnyTimes()
}

func TestClose_SingleConnection(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	// Create a test HTTP request
	req := httptest.NewRequest("GET", "/ws", nil)
	w := httptest.NewRecorder()

	// Mock the upgrader to return a successful connection
	ts.mockUpgrader.EXPECT().
		Upgrade(gomock.Any(), gomock.Any(), gomock.Any()).
		Return(ts.mockConn, nil)

	// Mock connection methods for background goroutine
	ts.mockConn.EXPECT().
		ReadMessage().
		Return(0, nil, errors.New("connection closed")).
		AnyTimes()

	// Mock clock for close message deadline
	now := time.Now()
	ts.mockClock.EXPECT().
		Now().
		Return(now).
		MinTimes(1)

	// Mock close operations
	ts.mockConn.EXPECT().
		WriteControl(websocket.CloseMessage, gomock.Any(), now.Add(2*time.Second)).
		Return(nil)

	ts.mockConn.EXPECT().
		Close().
		Return(nil)

	// Create connection
	_, err := ts.ws.NewConnection(w, req)
	assert.NoError(t, err)

	// Test Close
	ts.ws.Close()
}

func TestClose_MultipleConnections(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	// Create additional mock connection for second connection
	mockConn2 := mocks.NewMockWebSocketConn(ts.ctrl)

	// Create two test HTTP requests
	req1 := httptest.NewRequest("GET", "/ws", nil)
	w1 := httptest.NewRecorder()
	req2 := httptest.NewRequest("GET", "/ws", nil)
	w2 := httptest.NewRecorder()

	// Mock the upgrader to return successful connections
	ts.mockUpgrader.EXPECT().
		Upgrade(gomock.Any(), gomock.Any(), gomock.Any()).
		Return(ts.mockConn, nil).
		Times(1)

	ts.mockUpgrader.EXPECT().
		Upgrade(gomock.Any(), gomock.Any(), gomock.Any()).
		Return(mockConn2, nil).
		Times(1)

	// Mock connection methods for background goroutines
	ts.mockConn.EXPECT().
		ReadMessage().
		Return(0, nil, errors.New("connection closed")).
		AnyTimes()

	ts.mockConn.EXPECT().
		WriteControl(gomock.Any(), gomock.Any(), gomock.Any()).
		Return(nil).
		Times(1)

	ts.mockConn.EXPECT().
		Close().
		Return(nil).
		Times(1)

	mockConn2.EXPECT().
		ReadMessage().
		Return(0, nil, errors.New("connection closed")).
		AnyTimes()

	mockConn2.EXPECT().
		WriteControl(gomock.Any(), gomock.Any(), gomock.Any()).
		Return(nil).
		Times(1)

	mockConn2.EXPECT().
		Close().
		Return(nil).
		Times(1)

	// Mock clock for close message deadline
	now := time.Now()
	ts.mockClock.EXPECT().
		Now().
		Return(now).
		AnyTimes()

	// Create two connections
	_, err := ts.ws.NewConnection(w1, req1)
	assert.NoError(t, err)

	_, err = ts.ws.NewConnection(w2, req2)
	assert.NoError(t, err)

	// Test Close
	ts.ws.Close()
}

func TestBackground_ConnectionCleanupOnReadError(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	// Create a test HTTP request
	req := httptest.NewRequest("GET", "/ws", nil)
	w := httptest.NewRecorder()

	// Mock the upgrader to return a successful connection
	ts.mockUpgrader.EXPECT().
		Upgrade(gomock.Any(), gomock.Any(), gomock.Any()).
		Return(ts.mockConn, nil)

	// Mock ReadMessage to return an error (simulating connection close)
	readErr := errors.New("connection closed")
	ts.mockConn.EXPECT().
		ReadMessage().
		Return(0, nil, readErr)

	// Mock close operations
	now := time.Now()
	ts.mockClock.EXPECT().
		Now().
		Return(now)

	ts.mockConn.EXPECT().
		WriteControl(websocket.CloseMessage, gomock.Any(), now.Add(2*time.Second)).
		Return(nil)

	ts.mockConn.EXPECT().
		Close().
		Return(nil)

	// Create connection
	connID, err := ts.ws.NewConnection(w, req)
	assert.NoError(t, err)

	// Wait for background goroutine to process the read error
	time.Sleep(50 * time.Millisecond)

	// Verify connection was removed by trying to send to it
	err = ts.ws.Send(connID, "test")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "connection conn-1 not found")
}

func TestBackground_ContextCancellation(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	// Create a cancellable context
	ctx, cancel := context.WithCancel(context.Background())
	logger := zaptest.NewLogger(t, zaptest.Level(zap.FatalLevel))
	ws := ws.NewWSHandler(ctx, ts.mockUpgrader, ts.mockClock, logger)

	// Create a test HTTP request
	req := httptest.NewRequest("GET", "/ws", nil)
	w := httptest.NewRecorder()

	// Mock the upgrader to return a successful connection
	ts.mockUpgrader.EXPECT().
		Upgrade(gomock.Any(), gomock.Any(), gomock.Any()).
		Return(ts.mockConn, nil)

	// Mock ReadMessage to block until context is canceled
	ts.mockConn.EXPECT().
		ReadMessage().
		DoAndReturn(func() (int, []byte, error) {
			// Block until context is canceled
			<-ctx.Done()
			return 0, nil, errors.New("context canceled")
		})

	// Mock close operations - these may not be called if context cancellation happens first
	now := time.Now()
	ts.mockClock.EXPECT().
		Now().
		Return(now).
		AnyTimes()

	ts.mockConn.EXPECT().
		WriteControl(websocket.CloseMessage, gomock.Any(), now.Add(2*time.Second)).
		Return(nil).
		AnyTimes()

	ts.mockConn.EXPECT().
		Close().
		Return(nil).
		AnyTimes()

	// Create connection
	connID, err := ws.NewConnection(w, req)
	assert.NoError(t, err)

	// Cancel the context
	cancel()

	// Wait for background goroutine to process the cancellation
	time.Sleep(50 * time.Millisecond)

	// Verify connection was removed by trying to send to it
	err = ws.Send(connID, "test")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "connection conn-1 not found")
}

func TestConcurrentNewConnections(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	const numConnections = 100
	var wg sync.WaitGroup
	connectionIDs := make([]string, numConnections)
	connectionErrors := make([]error, numConnections)

	// Mock the upgrader to return successful connections
	ts.mockUpgrader.EXPECT().
		Upgrade(gomock.Any(), gomock.Any(), gomock.Any()).
		Return(ts.mockConn, nil).
		Times(numConnections)

	// Mock connection methods for background goroutines
	ts.mockConn.EXPECT().
		ReadMessage().
		Return(0, nil, errors.New("connection closed")).
		AnyTimes()

	ts.mockConn.EXPECT().
		WriteControl(gomock.Any(), gomock.Any(), gomock.Any()).
		Return(nil).
		AnyTimes()

	ts.mockConn.EXPECT().
		Close().
		Return(nil).
		AnyTimes()

	// Mock clock for close message deadline
	ts.mockClock.EXPECT().
		Now().
		Return(time.Now()).
		AnyTimes()

	// Create connections concurrently
	for i := range numConnections {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			req := httptest.NewRequest("GET", "/ws", nil)
			w := httptest.NewRecorder()

			connID, err := ts.ws.NewConnection(w, req)
			connectionIDs[index] = connID
			connectionErrors[index] = err
		}(i)
	}

	wg.Wait()

	// Verify all connections were created successfully
	for i := range numConnections {
		assert.NoError(t, connectionErrors[i], "Connection %d failed", i)
		assert.NotEmpty(t, connectionIDs[i], "Connection %d has empty ID", i)
		assert.Contains(t, connectionIDs[i], "conn-", "Connection %d has invalid ID format", i)
	}

	// Verify all connection IDs are unique
	uniqueIDs := make(map[string]bool)
	for _, connID := range connectionIDs {
		assert.False(t, uniqueIDs[connID], "Duplicate connection ID: %s", connID)
		uniqueIDs[connID] = true
	}

	assert.Equal(t, numConnections, len(uniqueIDs), "Expected %d unique connections", numConnections)
}

func TestConcurrentSendToMultipleConnections(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	const numConnections = 10
	const messagesPerConnection = 5

	// Create multiple mock connections
	mockConns := make([]*mocks.MockWebSocketConn, numConnections)
	for i := range numConnections {
		mockConns[i] = mocks.NewMockWebSocketConn(ts.ctrl)
	}

	var wg sync.WaitGroup
	connectionIDs := make([]string, numConnections)

	// Mock the upgrader to return different connections
	for i := range numConnections {
		ts.mockUpgrader.EXPECT().
			Upgrade(gomock.Any(), gomock.Any(), gomock.Any()).
			Return(mockConns[i], nil).
			Times(1)
	}

	// Mock connection methods for background goroutines - make them block longer
	for i := range numConnections {
		mockConns[i].EXPECT().
			ReadMessage().
			DoAndReturn(func() (int, []byte, error) {
				// Block for a longer time to prevent premature cleanup
				time.Sleep(100 * time.Millisecond)
				return 0, nil, errors.New("connection closed")
			}).
			AnyTimes()

		mockConns[i].EXPECT().
			WriteControl(gomock.Any(), gomock.Any(), gomock.Any()).
			Return(nil).
			AnyTimes()

		mockConns[i].EXPECT().
			Close().
			Return(nil).
			AnyTimes()
	}

	// Mock clock for close message deadline
	ts.mockClock.EXPECT().
		Now().
		Return(time.Now()).
		AnyTimes()

	// Create connections concurrently
	for i := range numConnections {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			req := httptest.NewRequest("GET", "/ws", nil)
			w := httptest.NewRecorder()

			connID, err := ts.ws.NewConnection(w, req)
			assert.NoError(t, err)
			connectionIDs[index] = connID
		}(i)
	}

	wg.Wait()

	// Now send messages concurrently to all connections
	message := map[string]string{"test": "concurrent message"}

	// Mock WriteJSON for all connections
	for i := range numConnections {
		mockConns[i].EXPECT().
			WriteJSON(message).
			Return(nil).
			Times(messagesPerConnection)
	}

	// Send messages concurrently
	for i := range numConnections {
		for range messagesPerConnection {
			wg.Add(1)
			go func(connIndex int) {
				defer wg.Done()
				err := ts.ws.Send(connectionIDs[connIndex], message)
				assert.NoError(t, err, "Failed to send message to connection %d", connIndex)
			}(i)
		}
	}

	wg.Wait()
}

func TestConcurrentSendAll(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	const numConnections = 5
	const numBroadcasts = 3

	// Create multiple mock connections
	mockConns := make([]*mocks.MockWebSocketConn, numConnections)
	for i := range numConnections {
		mockConns[i] = mocks.NewMockWebSocketConn(ts.ctrl)
	}

	var wg sync.WaitGroup
	connectionIDs := make([]string, numConnections)

	// Mock the upgrader to return different connections
	for i := range numConnections {
		ts.mockUpgrader.EXPECT().
			Upgrade(gomock.Any(), gomock.Any(), gomock.Any()).
			Return(mockConns[i], nil).
			Times(1)
	}

	// Mock connection methods for background goroutines - make them block longer
	for i := range numConnections {
		mockConns[i].EXPECT().
			ReadMessage().
			DoAndReturn(func() (int, []byte, error) {
				// Block for a longer time to prevent premature cleanup
				time.Sleep(100 * time.Millisecond)
				return 0, nil, errors.New("connection closed")
			}).
			AnyTimes()

		mockConns[i].EXPECT().
			WriteControl(gomock.Any(), gomock.Any(), gomock.Any()).
			Return(nil).
			AnyTimes()

		mockConns[i].EXPECT().
			Close().
			Return(nil).
			AnyTimes()
	}

	// Mock clock for close message deadline
	ts.mockClock.EXPECT().
		Now().
		Return(time.Now()).
		AnyTimes()

	// Create connections concurrently
	for i := range numConnections {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			req := httptest.NewRequest("GET", "/ws", nil)
			w := httptest.NewRecorder()

			connID, err := ts.ws.NewConnection(w, req)
			assert.NoError(t, err)
			connectionIDs[index] = connID
		}(i)
	}

	wg.Wait()

	// Mock WriteJSON for all connections for all broadcasts
	for i := range numConnections {
		mockConns[i].EXPECT().
			WriteJSON(gomock.Any()).
			Return(nil).
			Times(numBroadcasts)
	}

	// Send broadcasts concurrently
	for i := range numBroadcasts {
		wg.Add(1)
		go func(broadcastIndex int) {
			defer wg.Done()
			message := map[string]string{"broadcast": fmt.Sprintf("message-%d", broadcastIndex)}
			err := ts.ws.SendAll(message)
			assert.NoError(t, err, "Failed to broadcast message %d", broadcastIndex)
		}(i)
	}

	wg.Wait()
}

func TestConcurrentCloseAndSend(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	const numConnections = 20

	// Create multiple mock connections
	mockConns := make([]*mocks.MockWebSocketConn, numConnections)
	for i := range numConnections {
		mockConns[i] = mocks.NewMockWebSocketConn(ts.ctrl)
	}

	var wg sync.WaitGroup
	connectionIDs := make([]string, numConnections)

	// Mock the upgrader to return different connections
	for i := range numConnections {
		ts.mockUpgrader.EXPECT().
			Upgrade(gomock.Any(), gomock.Any(), gomock.Any()).
			Return(mockConns[i], nil).
			Times(1)
	}

	// Mock connection methods for background goroutines
	for i := range numConnections {
		mockConns[i].EXPECT().
			ReadMessage().
			Return(0, nil, errors.New("connection closed")).
			AnyTimes()

		mockConns[i].EXPECT().
			WriteControl(gomock.Any(), gomock.Any(), gomock.Any()).
			Return(nil).
			AnyTimes()

		mockConns[i].EXPECT().
			Close().
			Return(nil).
			AnyTimes()
	}

	// Mock clock for close message deadline
	now := time.Now()
	ts.mockClock.EXPECT().
		Now().
		Return(now).
		AnyTimes()

	// Create connections concurrently
	for i := range numConnections {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			req := httptest.NewRequest("GET", "/ws", nil)
			w := httptest.NewRecorder()

			connID, err := ts.ws.NewConnection(w, req)
			assert.NoError(t, err)
			connectionIDs[index] = connID
		}(i)
	}

	wg.Wait()

	// Mock WriteJSON for some connections (before close)
	for i := range numConnections / 2 {
		mockConns[i].EXPECT().
			WriteJSON(gomock.Any()).
			Return(nil).
			AnyTimes()
	}

	// Mock close operations for all connections
	for i := range numConnections {
		mockConns[i].EXPECT().
			WriteControl(websocket.CloseMessage, gomock.Any(), now.Add(2*time.Second)).
			Return(nil).
			AnyTimes()

		mockConns[i].EXPECT().
			Close().
			Return(nil).
			AnyTimes()
	}

	// Concurrently send messages and close connections
	message := map[string]string{"test": "concurrent close and send"}

	// Start sending messages
	for i := range numConnections {
		wg.Add(1)
		go func(connIndex int) {
			defer wg.Done()
			// Try to send multiple messages
			for range 5 {
				err := ts.ws.Send(connectionIDs[connIndex], message)
				if err != nil {
					// Expected to fail after close
					assert.Contains(t, err.Error(), "not found")
				}
			}
		}(i)
	}

	// Close all connections after a short delay
	wg.Add(1)
	go func() {
		defer wg.Done()
		time.Sleep(10 * time.Millisecond) // Let some sends start
		ts.ws.Close()
	}()

	wg.Wait()
}

func TestConcurrentNewConnectionAndClose(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	const numConnections = 50

	// Create multiple mock connections
	mockConns := make([]*mocks.MockWebSocketConn, numConnections)
	for i := range numConnections {
		mockConns[i] = mocks.NewMockWebSocketConn(ts.ctrl)
	}

	var wg sync.WaitGroup
	connectionIDs := make([]string, numConnections)
	connectionErrors := make([]error, numConnections)

	// Mock the upgrader to return different connections
	for i := range numConnections {
		ts.mockUpgrader.EXPECT().
			Upgrade(gomock.Any(), gomock.Any(), gomock.Any()).
			Return(mockConns[i], nil).
			Times(1)
	}

	// Mock connection methods for background goroutines
	for i := range numConnections {
		mockConns[i].EXPECT().
			ReadMessage().
			Return(0, nil, errors.New("connection closed")).
			AnyTimes()

		mockConns[i].EXPECT().
			WriteControl(gomock.Any(), gomock.Any(), gomock.Any()).
			Return(nil).
			AnyTimes()

		mockConns[i].EXPECT().
			Close().
			Return(nil).
			AnyTimes()
	}

	// Mock clock for close message deadline
	now := time.Now()
	ts.mockClock.EXPECT().
		Now().
		Return(now).
		AnyTimes()

	// Mock close operations for all connections
	for i := range numConnections {
		mockConns[i].EXPECT().
			WriteControl(websocket.CloseMessage, gomock.Any(), now.Add(2*time.Second)).
			Return(nil).
			AnyTimes()

		mockConns[i].EXPECT().
			Close().
			Return(nil).
			AnyTimes()
	}

	// Concurrently create connections and close
	for i := range numConnections {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			req := httptest.NewRequest("GET", "/ws", nil)
			w := httptest.NewRecorder()

			connID, err := ts.ws.NewConnection(w, req)
			connectionIDs[index] = connID
			connectionErrors[index] = err
		}(i)
	}

	// Close connections after a short delay
	wg.Add(1)
	go func() {
		defer wg.Done()
		time.Sleep(5 * time.Millisecond) // Let some connections start
		ts.ws.Close()
	}()

	wg.Wait()

	// Verify that some connections were created successfully
	successCount := 0
	for i := range numConnections {
		if connectionErrors[i] == nil {
			successCount++
			assert.NotEmpty(t, connectionIDs[i])
		}
	}

	// At least some connections should have been created before close
	assert.Greater(t, successCount, 0, "Expected at least some connections to be created")
}
