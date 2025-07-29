//nolint:gosec
package wrapper

import (
	"context"
	"net/http"
	"time"

	"github.com/gorilla/websocket"
)

//go:generate mockgen -source=websocket.go -destination=../mocks/mock_websocket.go -package=mocks -mock_names=WebSocketDialerInterface=MockWebSocketDialer
type WebSocketDialerInterface interface {
	DialContext(ctx context.Context, url string, requestHeader http.Header) (WebSocketConnInterface, *http.Response, error)
}

//go:generate mockgen -source=websocket.go -destination=../mocks/mock_websocket.go -package=mocks -mock_names=WebSocketConnInterface=MockWebSocketConn
type WebSocketConnInterface interface {
	WriteJSON(v interface{}) error
	ReadMessage() (messageType int, p []byte, err error)
	WriteMessage(messageType int, data []byte) error
	WriteControl(messageType int, data []byte, deadline time.Time) error
	SetPongHandler(h func(appData string) error)
	SetReadDeadline(t time.Time) error
	Close() error
}

type WebSocketDialer struct {
	dialer *websocket.Dialer
}

func NewWebSocketDialer(dialer *websocket.Dialer) WebSocketDialerInterface {
	return &WebSocketDialer{dialer: dialer}
}

func (d *WebSocketDialer) DialContext(ctx context.Context, url string, requestHeader http.Header) (WebSocketConnInterface, *http.Response, error) {
	conn, resp, err := d.dialer.DialContext(ctx, url, requestHeader)
	if err != nil {
		return nil, resp, err
	}
	return &WebSocketConn{conn: conn}, resp, nil
}

type WebSocketConn struct {
	conn *websocket.Conn
}

func (c *WebSocketConn) WriteJSON(v interface{}) error {
	return c.conn.WriteJSON(v)
}

func (c *WebSocketConn) ReadMessage() (int, []byte, error) {
	return c.conn.ReadMessage()
}

func (c *WebSocketConn) WriteMessage(messageType int, data []byte) error {
	return c.conn.WriteMessage(messageType, data)
}

func (c *WebSocketConn) WriteControl(messageType int, data []byte, deadline time.Time) error {
	return c.conn.WriteControl(messageType, data, deadline)
}

func (c *WebSocketConn) SetPongHandler(h func(appData string) error) {
	c.conn.SetPongHandler(h)
}

func (c *WebSocketConn) SetReadDeadline(t time.Time) error {
	return c.conn.SetReadDeadline(t)
}

func (c *WebSocketConn) Close() error {
	return c.conn.Close()
}
