package ws_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
	"github.com/feral-file/ffos-user/components/feral-controld/ws"
)

func TestNewWSHandler(t *testing.T) {
	logger := zap.NewNop()
	clock := wrapper.NewClock()
	upgrader := wrapper.NewWebsocketUpgrader(&websocket.Upgrader{
		ReadBufferSize:  ws.BUFFER_SIZE,
		WriteBufferSize: ws.BUFFER_SIZE,
		CheckOrigin: func(r *http.Request) bool {
			return true
		},
	})
	ctx := context.Background()
	handler := ws.NewWSHandler(ctx, upgrader, clock, logger)

	assert.NotNil(t, handler)
}

func TestNewConnection(t *testing.T) {
	logger := zap.NewNop()
	clock := wrapper.NewClock()
	upgrader := wrapper.NewWebsocketUpgrader(&websocket.Upgrader{
		ReadBufferSize:  ws.BUFFER_SIZE,
		WriteBufferSize: ws.BUFFER_SIZE,
		CheckOrigin: func(r *http.Request) bool {
			return true
		},
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	handler := ws.NewWSHandler(ctx, upgrader, clock, logger)

	connIDChan := make(chan string, 1)

	// Create a test server
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		connID, err := handler.NewConnection(w, r)
		require.NoError(t, err)
		assert.NotEmpty(t, connID)
		connIDChan <- connID
	}))
	defer server.Close()

	// Convert http:// to ws://
	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")

	// Connect as a client
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	require.NoError(t, err)
	defer func() {
		_ = conn.Close()
	}()

	// Wait for connection ID
	select {
	case connID := <-connIDChan:
		assert.NotEmpty(t, connID)
	case <-time.After(1 * time.Second):
		t.Fatal("timeout waiting for connection ID")
	}
}

func TestSend(t *testing.T) {
	logger := zap.NewNop()
	clock := wrapper.NewClock()
	upgrader := wrapper.NewWebsocketUpgrader(&websocket.Upgrader{
		ReadBufferSize:  ws.BUFFER_SIZE,
		WriteBufferSize: ws.BUFFER_SIZE,
		CheckOrigin: func(r *http.Request) bool {
			return true
		},
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	handler := ws.NewWSHandler(ctx, upgrader, clock, logger)

	var connID string
	connIDChan := make(chan string, 1)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var err error
		connID, err = handler.NewConnection(w, r)
		require.NoError(t, err)
		connIDChan <- connID
	}))
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	require.NoError(t, err)
	defer func() {
		_ = conn.Close()
	}()

	// Wait for connection ID
	select {
	case connID = <-connIDChan:
	case <-time.After(1 * time.Second):
		t.Fatal("timeout waiting for connection ID")
	}

	// Give a moment for the connection to be fully established
	time.Sleep(50 * time.Millisecond)

	// Send a message
	testMsg := map[string]string{"test": "message"}
	err = handler.Send(connID, testMsg)
	assert.NoError(t, err)

	// Read the message
	var received map[string]string
	err = conn.ReadJSON(&received)
	assert.NoError(t, err)
	assert.Equal(t, testMsg, received)

	// Cleanup: close connection and wait for cleanup
	_ = conn.Close()
	cancel()
	time.Sleep(50 * time.Millisecond) // Give time for background goroutine to exit
}

func TestSendAll(t *testing.T) {
	logger := zap.NewNop()
	clock := wrapper.NewClock()
	upgrader := wrapper.NewWebsocketUpgrader(&websocket.Upgrader{
		ReadBufferSize:  ws.BUFFER_SIZE,
		WriteBufferSize: ws.BUFFER_SIZE,
		CheckOrigin: func(r *http.Request) bool {
			return true
		},
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	handler := ws.NewWSHandler(ctx, upgrader, clock, logger)

	connCount := 0
	countChan := make(chan int, 2)

	// Create shared server
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, err := handler.NewConnection(w, r)
		require.NoError(t, err)
		connCount++
		countChan <- connCount
	}))
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")

	// Create two connections
	conn1, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	require.NoError(t, err)
	defer func() {
		_ = conn1.Close()
	}()

	// Wait for first connection
	select {
	case <-countChan:
	case <-time.After(1 * time.Second):
		t.Fatal("timeout waiting for first connection")
	}

	conn2, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	require.NoError(t, err)
	defer func() {
		_ = conn2.Close()
	}()

	// Wait for second connection
	select {
	case <-countChan:
	case <-time.After(1 * time.Second):
		t.Fatal("timeout waiting for second connection")
	}

	// Give a moment for connections to be fully established
	time.Sleep(50 * time.Millisecond)

	// Send message to all
	testMsg := map[string]string{"broadcast": "message"}
	err = handler.SendAll(testMsg)
	assert.NoError(t, err)

	// Read from both connections
	var received1, received2 map[string]string
	err = conn1.ReadJSON(&received1)
	assert.NoError(t, err)
	assert.Equal(t, testMsg, received1)

	err = conn2.ReadJSON(&received2)
	assert.NoError(t, err)
	assert.Equal(t, testMsg, received2)

	// Cleanup: close connections and wait for cleanup
	_ = conn1.Close()
	_ = conn2.Close()
	cancel()
	time.Sleep(50 * time.Millisecond) // Give time for background goroutines to exit
}

func TestSendToNonExistentConnection(t *testing.T) {
	logger := zap.NewNop()
	clock := wrapper.NewClock()
	upgrader := wrapper.NewWebsocketUpgrader(&websocket.Upgrader{
		ReadBufferSize:  ws.BUFFER_SIZE,
		WriteBufferSize: ws.BUFFER_SIZE,
		CheckOrigin: func(r *http.Request) bool {
			return true
		},
	})
	ctx := context.Background()
	handler := ws.NewWSHandler(ctx, upgrader, clock, logger)

	// Try to send to non-existent connection
	err := handler.Send("non-existent", map[string]string{"test": "message"})
	assert.Error(t, err)
	assert.Equal(t, err.Error(), "connection non-existent not found")
}

func TestConnectionAutoCloseOnClientDisconnect(t *testing.T) {
	logger := zap.NewNop()
	clock := wrapper.NewClock()
	upgrader := wrapper.NewWebsocketUpgrader(&websocket.Upgrader{
		ReadBufferSize:  ws.BUFFER_SIZE,
		WriteBufferSize: ws.BUFFER_SIZE,
		CheckOrigin: func(r *http.Request) bool {
			return true
		},
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	handler := ws.NewWSHandler(ctx, upgrader, clock, logger)

	var connID string
	connIDChan := make(chan string, 1)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var err error
		connID, err = handler.NewConnection(w, r)
		require.NoError(t, err)
		connIDChan <- connID
	}))
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	require.NoError(t, err)

	// Wait for connection ID
	select {
	case connID = <-connIDChan:
	case <-time.After(1 * time.Second):
		t.Fatal("timeout waiting for connection ID")
	}

	// Close the client connection
	_ = conn.Close()

	// Give time for the server to detect the closure
	time.Sleep(100 * time.Millisecond)

	// Try to send to the closed connection - should fail
	err = handler.Send(connID, map[string]string{"test": "message"})
	assert.Error(t, err)

	// Cleanup
	cancel()
	time.Sleep(50 * time.Millisecond) // Give time for background goroutine to exit
}

func TestClose(t *testing.T) {
	logger := zap.NewNop()
	clock := wrapper.NewClock()
	upgrader := wrapper.NewWebsocketUpgrader(&websocket.Upgrader{
		ReadBufferSize:  ws.BUFFER_SIZE,
		WriteBufferSize: ws.BUFFER_SIZE,
		CheckOrigin: func(r *http.Request) bool {
			return true
		},
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	handler := ws.NewWSHandler(ctx, upgrader, clock, logger)

	connCount := 0
	countChan := make(chan int, 1)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, err := handler.NewConnection(w, r)
		require.NoError(t, err)
		connCount++
		countChan <- connCount
	}))
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	require.NoError(t, err)
	defer func() {
		_ = conn.Close()
	}()

	// Wait for connection
	select {
	case <-countChan:
	case <-time.After(1 * time.Second):
		t.Fatal("timeout waiting for connection")
	}

	// Give a moment for connection to be fully established
	time.Sleep(50 * time.Millisecond)

	// Close all connections
	handler.Close()

	// Try to read from the connection - should fail because it's closed
	_ = conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	_, _, err = conn.ReadMessage()
	assert.Error(t, err)

	// Cleanup
	cancel()
	time.Sleep(50 * time.Millisecond) // Give time for background goroutine to exit
}

func TestSendAllWithFailedConnection(t *testing.T) {
	logger := zap.NewNop()
	clock := wrapper.NewClock()
	upgrader := wrapper.NewWebsocketUpgrader(&websocket.Upgrader{
		ReadBufferSize:  ws.BUFFER_SIZE,
		WriteBufferSize: ws.BUFFER_SIZE,
		CheckOrigin: func(r *http.Request) bool {
			return true
		},
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	handler := ws.NewWSHandler(ctx, upgrader, clock, logger)

	connCount := 0
	countChan := make(chan int, 2)

	// Create shared server
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, err := handler.NewConnection(w, r)
		require.NoError(t, err)
		connCount++
		countChan <- connCount
	}))
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")

	// Create two connections
	conn1, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	require.NoError(t, err)
	defer func() {
		_ = conn1.Close()
	}()

	// Wait for first connection
	select {
	case <-countChan:
	case <-time.After(1 * time.Second):
		t.Fatal("timeout waiting for first connection")
	}

	conn2, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	require.NoError(t, err)

	// Wait for second connection
	select {
	case <-countChan:
	case <-time.After(1 * time.Second):
		t.Fatal("timeout waiting for second connection")
	}

	// Give a moment for connections to be fully established
	time.Sleep(50 * time.Millisecond)

	// Close conn2 before sending
	_ = conn2.Close()
	time.Sleep(100 * time.Millisecond) // Give time for cleanup

	// Send message to all - only conn1 should succeed
	testMsg := map[string]string{"broadcast": "message"}
	err = handler.SendAll(testMsg)
	assert.NoError(t, err)

	// Read from conn1 - should still work
	var received1 map[string]string
	err = conn1.ReadJSON(&received1)
	assert.NoError(t, err)
	assert.Equal(t, testMsg, received1)

	// Cleanup
	_ = conn1.Close()
	cancel()
	time.Sleep(50 * time.Millisecond) // Give time for background goroutines to exit
}
