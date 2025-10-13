package ws

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

const BUFFER_SIZE = 1024

//go:generate mockgen -source=ws.go -destination=../mocks/ws.go -package=mocks -mock_names=WS=MockWS
type WS interface {
	// NewConnection upgrades an HTTP connection to websocket and tracks it
	NewConnection(w http.ResponseWriter, r *http.Request) (string, error)

	// Send sends a message to a specific connection
	Send(connID string, message any) error

	// SendAll sends a message to all connections
	SendAll(message any) error

	// Close closes all connections
	Close()
}

type ws struct {
	mu          sync.RWMutex
	ctx         context.Context
	connections map[string]wrapper.WebSocketConn
	nextID      int
	upgrader    wrapper.WebsocketUpgrader

	clock  wrapper.Clock
	logger *zap.Logger
}

// NewWSHandler creates a new websocket handler
func NewWSHandler(ctx context.Context, upgrader wrapper.WebsocketUpgrader, clock wrapper.Clock, logger *zap.Logger) WS {
	return &ws{
		ctx:         ctx,
		connections: make(map[string]wrapper.WebSocketConn),
		nextID:      1,
		upgrader:    upgrader,
		clock:       clock,
		logger:      logger,
	}
}

func (ws *ws) NewConnection(w http.ResponseWriter, r *http.Request) (string, error) {
	conn, err := ws.upgrader.Upgrade(w, r, nil)
	if err != nil {
		ws.logger.Error("Failed to upgrade connection", zap.Error(err))
		return "", err
	}

	ws.mu.Lock()
	connID := fmt.Sprintf("conn-%d", ws.nextID)
	ws.nextID++
	ws.connections[connID] = conn
	ws.mu.Unlock()

	ws.logger.Info("New websocket connection established",
		zap.String("connID", connID),
		zap.Int("total_connections", len(ws.connections)))

	go ws.background(connID, conn)

	return connID, nil
}

// background handles the connection and closes it when it's done
func (ws *ws) background(connID string, conn wrapper.WebSocketConn) {
	// Create a channel to signal when ReadMessage completes
	done := make(chan error, 1)

	go func() {
		for {
			_, _, err := conn.ReadMessage()
			if err != nil {
				done <- err
				return
			}
			// If message is received successfully (shouldn't happen in one-directional mode)
			// just continue reading
		}
	}()

	select {
	case <-ws.ctx.Done():
		ws.logger.Info("Background goroutine exiting due to context cancellation", zap.String("connID", connID))
		// Clean up the connection when context is canceled
		ws.mu.Lock()
		ws.closeConn(connID)
		ws.mu.Unlock()
		return
	case err := <-done:
		// Connection closed or error occurred
		ws.logger.Info("Read failed, attempting to close connection",
			zap.String("connID", connID),
			zap.Error(err))
		ws.mu.Lock()
		ws.closeConn(connID)
		ws.mu.Unlock()
		return
	}
}

func (ws *ws) Send(connID string, message any) error {
	ws.mu.RLock()
	conn, exists := ws.connections[connID]
	ws.mu.RUnlock()

	if !exists {
		return fmt.Errorf("connection %s not found", connID)
	}

	// Write message
	err := conn.WriteJSON(message)
	if err != nil {
		return fmt.Errorf("failed to send message: %w", err)
	}

	ws.logger.Debug("Message sent to client", zap.String("connID", connID), zap.Any("message", message))

	return nil
}

func (ws *ws) SendAll(message any) error {
	ws.mu.RLock()
	if len(ws.connections) == 0 {
		ws.logger.Warn("No connections to send message to")
		ws.mu.RUnlock()
		return nil
	}

	connections := make(map[string]wrapper.WebSocketConn, len(ws.connections))
	maps.Copy(connections, ws.connections)
	ws.mu.RUnlock()

	var lastErr error
	successCount := 0

	for connID, conn := range connections {
		err := conn.WriteJSON(message)
		if err != nil {
			ws.logger.Error("Failed to send message to client",
				zap.String("connID", connID),
				zap.Error(err))
			// Close the connection if write fails
			ws.mu.Lock()
			ws.closeConn(connID)
			ws.mu.Unlock()
			lastErr = err
		} else {
			successCount++
		}
	}

	ws.logger.Info("Broadcast message sent", zap.Int("successful", successCount), zap.Int("failed", len(connections)-successCount), zap.Any("message", message))

	return lastErr
}

// closeConn close the connection
// This function is not thread safe, so it should be called with the lock held
func (ws *ws) closeConn(connID string) {
	ws.logger.Info("Closing connection", zap.String("connID", connID))
	if conn, exists := ws.connections[connID]; exists {
		// Write close message
		err := conn.WriteControl(
			websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""),
			ws.clock.Now().Add(2*time.Second),
		)
		if err != nil {
			ws.logger.Warn("Failed to write close message", zap.String("connID", connID), zap.Error(err))
		}

		// Close the connection
		err = conn.Close()
		if err != nil {
			ws.logger.Warn("Failed to close connection", zap.String("connID", connID), zap.Error(err))
		}

		// Remove the connection from the tracker
		delete(ws.connections, connID)
		ws.logger.Info("Connection closed", zap.String("connID", connID))
	}
}

func (ws *ws) Close() {
	ws.mu.Lock()
	defer ws.mu.Unlock()

	ws.logger.Info("Closing all websocket connections",
		zap.Int("count", len(ws.connections)))

	for connID := range ws.connections {
		ws.closeConn(connID)
	}

	ws.connections = make(map[string]wrapper.WebSocketConn)
	ws.logger.Info("All websocket connections closed")
}
