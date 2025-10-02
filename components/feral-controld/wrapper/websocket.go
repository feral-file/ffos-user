//nolint:gosec
package wrapper

import (
	"context"
	go_http "net/http"
	"time"

	"github.com/gorilla/websocket"
)

//go:generate mockgen -source=websocket.go -destination=../mocks/websocket.go -package=mocks -mock_names=WebSocketDialer=MockWebSocketDialer
type WebSocketDialer interface {
	DialContext(ctx context.Context, url string, requestHeader go_http.Header) (WebSocketConn, *go_http.Response, error)
}

//go:generate mockgen -source=websocket.go -destination=../mocks/websocket.go -package=mocks -mock_names=WebSocketConn=MockWebSocketConn
type WebSocketConn interface {
	WriteJSON(v interface{}) error
	ReadMessage() (messageType int, p []byte, err error)
	WriteMessage(messageType int, data []byte) error
	WriteControl(messageType int, data []byte, deadline time.Time) error
	SetPongHandler(h func(appData string) error)
	SetReadDeadline(t time.Time) error
	Close() error
}

type webSocketDialer struct {
	dialer *websocket.Dialer
}

func NewWebSocketDialer(dialer *websocket.Dialer) WebSocketDialer {
	return &webSocketDialer{dialer: dialer}
}

func (d *webSocketDialer) DialContext(ctx context.Context, url string, requestHeader go_http.Header) (WebSocketConn, *go_http.Response, error) {
	conn, resp, err := d.dialer.DialContext(ctx, url, requestHeader)
	if err != nil {
		return nil, resp, err
	}
	return &webSocketConn{conn: conn}, resp, nil
}

type webSocketConn struct {
	conn *websocket.Conn
}

func (c *webSocketConn) WriteJSON(v interface{}) error {
	return c.conn.WriteJSON(v)
}

func (c *webSocketConn) ReadMessage() (int, []byte, error) {
	return c.conn.ReadMessage()
}

func (c *webSocketConn) WriteMessage(messageType int, data []byte) error {
	return c.conn.WriteMessage(messageType, data)
}

func (c *webSocketConn) WriteControl(messageType int, data []byte, deadline time.Time) error {
	return c.conn.WriteControl(messageType, data, deadline)
}

func (c *webSocketConn) SetPongHandler(h func(appData string) error) {
	c.conn.SetPongHandler(h)
}

func (c *webSocketConn) SetReadDeadline(t time.Time) error {
	return c.conn.SetReadDeadline(t)
}

func (c *webSocketConn) Close() error {
	return c.conn.Close()
}

//go:generate mockgen -source=websocket.go -destination=../mocks/websocket.go -package=mocks -mock_names=WebsocketUpgrader=MockWebsocketUpgrader
type WebsocketUpgrader interface {
	Upgrade(w go_http.ResponseWriter, r *go_http.Request, responseHeader go_http.Header) (WebSocketConn, error)
}

type websocketUpgrader struct {
	upgrader *websocket.Upgrader
}

func NewWebsocketUpgrader(upgrader *websocket.Upgrader) WebsocketUpgrader {
	return &websocketUpgrader{upgrader: upgrader}
}

func (u *websocketUpgrader) Upgrade(w go_http.ResponseWriter, r *go_http.Request, responseHeader go_http.Header) (WebSocketConn, error) {
	conn, err := u.upgrader.Upgrade(w, r, responseHeader)
	if err != nil {
		return nil, err
	}
	return &webSocketConn{conn: conn}, nil
}
