package wrapper

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWebSocketConnReadLimitRejectsOversizedFrame(t *testing.T) {
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		_ = conn.WriteMessage(websocket.TextMessage, []byte(strings.Repeat("x", 64)))
	}))
	defer server.Close()

	dialer := NewWebSocketDialer(&websocket.Dialer{})
	conn, _, err := dialer.DialContext(
		context.Background(),
		"ws"+strings.TrimPrefix(server.URL, "http"),
		nil,
	)
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()

	conn.SetReadLimit(32)
	_, _, err = conn.ReadMessage()

	require.Error(t, err)
	assert.ErrorIs(t, err, websocket.ErrReadLimit)
}
