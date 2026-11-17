package screenshot

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"io"
	"net/http"
	"testing"

	"github.com/golang/mock/gomock"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/feral-file/ffos-user/components/feral-controld/mocks"
)

// artworkConn sets up target discovery and one page connection, and returns
// the connection mock for the CDP exchange.
func artworkConn(t *testing.T, ctrl *gomock.Controller) (*mocks.MockHTTPClient, *mocks.MockWebSocketDialer, *mocks.MockWebSocketConn) {
	t.Helper()
	httpClient := mocks.NewMockHTTPClient(ctrl)
	dialer := mocks.NewMockWebSocketDialer(ctrl)
	conn := mocks.NewMockWebSocketConn(ctrl)
	targets := `[{"type":"page","webSocketDebuggerUrl":"ws://127.0.0.1:9222/devtools/page/1"}]`
	httpClient.EXPECT().Do(gomock.Any()).Return(&http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewBufferString(targets))}, nil)
	dialer.EXPECT().DialContext(gomock.Any(), "ws://127.0.0.1:9222/devtools/page/1", http.Header(nil)).Return(conn, nil, nil)
	conn.EXPECT().SetReadLimit(int64(maxCDPMessageBytes))
	conn.EXPECT().SetReadDeadline(gomock.Any()).Return(nil)
	conn.EXPECT().SetWriteDeadline(gomock.Any()).Return(nil)
	conn.EXPECT().Close().Return(nil)
	return httpClient, dialer, conn
}

func expectEvaluate(t *testing.T, conn *mocks.MockWebSocketConn, layout string) {
	t.Helper()
	conn.EXPECT().WriteMessage(websocket.TextMessage, gomock.Any()).DoAndReturn(func(_ int, data []byte) error {
		var req cdpRequest
		require.NoError(t, json.Unmarshal(data, &req))
		assert.Equal(t, 1, req.ID)
		assert.Equal(t, methodEvaluate, req.Method)
		assert.Equal(t, true, req.Params["returnByValue"])
		assert.Contains(t, req.Params["expression"], `[data-testid="tombstone-overlay"]`)
		return nil
	})
	value, err := json.Marshal(layout)
	require.NoError(t, err)
	conn.EXPECT().ReadMessage().Return(websocket.TextMessage, []byte(`{"id":1,"result":{"result":{"type":"string","value":`+string(value)+`}}}`), nil)
}

func TestCaptureArtworkClipsAndMapsTheLabelMask(t *testing.T) {
	ctrl := gomock.NewController(t)
	httpClient, dialer, conn := artworkConn(t, ctrl)
	expectEvaluate(t, conn, `{"artwork":{"x":100,"y":50,"w":2000,"h":1000},"covered":false,
		"masks":[{"x":0,"y":900,"w":400,"h":300}],"viewport":{"x":0,"y":0,"w":3840,"h":2160}}`)
	conn.EXPECT().WriteMessage(websocket.TextMessage, gomock.Any()).DoAndReturn(func(_ int, data []byte) error {
		var req cdpRequest
		require.NoError(t, json.Unmarshal(data, &req))
		assert.Equal(t, methodCaptureScreenshot, req.Method)
		clip := req.Params["clip"].(map[string]interface{})
		assert.Equal(t, float64(100), clip["x"])
		assert.Equal(t, float64(50), clip["y"])
		assert.Equal(t, float64(2000), clip["width"])
		assert.Equal(t, float64(1000), clip["height"])
		assert.InDelta(t, 1024.0/2000.0, clip["scale"], 1e-9, "the longer edge lands on maxEdge")
		return nil
	})
	conn.EXPECT().ReadMessage().Return(websocket.TextMessage, captureResponse(t, 2, makePNG(t, 1024, 512)), nil)

	frame, err := NewKiosk("http://127.0.0.1:9222", httpClient, dialer).CaptureArtwork(context.Background(), 1024)
	require.NoError(t, err)
	assert.True(t, frame.Clipped)
	assert.Equal(t, image.Rect(0, 0, 1024, 512), frame.Image.Bounds())
	// Label (0,900)-(400,1200) CSS, clip origin (100,50), 0.512 px per CSS
	// px: (-51.2,435.2)-(153.6,588.8), rounded outward, cut to the frame.
	assert.Equal(t, []image.Rectangle{image.Rect(0, 435, 154, 512)}, frame.Masks)
}

func TestCaptureArtworkRefusesACoveredArtwork(t *testing.T) {
	ctrl := gomock.NewController(t)
	httpClient, dialer, conn := artworkConn(t, ctrl)
	expectEvaluate(t, conn, `{"artwork":{"x":0,"y":0,"w":3840,"h":2160},"covered":true,"masks":[],"viewport":{"x":0,"y":0,"w":3840,"h":2160}}`)
	// No capture is sent: the loading overlay or a modal is on screen.
	_, err := NewKiosk("http://127.0.0.1:9222", httpClient, dialer).CaptureArtwork(context.Background(), 1024)
	require.ErrorIs(t, err, ErrCovered)
}

func TestCaptureArtworkFallsBackToTheViewport(t *testing.T) {
	ctrl := gomock.NewController(t)
	httpClient, dialer, conn := artworkConn(t, ctrl)
	expectEvaluate(t, conn, `{"artwork":null,"covered":false,"masks":[],"viewport":{"x":0,"y":0,"w":3840,"h":2160}}`)
	conn.EXPECT().WriteMessage(websocket.TextMessage, gomock.Any()).DoAndReturn(func(_ int, data []byte) error {
		var req cdpRequest
		require.NoError(t, json.Unmarshal(data, &req))
		clip := req.Params["clip"].(map[string]interface{})
		assert.Equal(t, float64(3840), clip["width"])
		assert.InDelta(t, 1024.0/3840.0, clip["scale"], 1e-9)
		return nil
	})
	conn.EXPECT().ReadMessage().Return(websocket.TextMessage, captureResponse(t, 2, makePNG(t, 1024, 576)), nil)
	frame, err := NewKiosk("http://127.0.0.1:9222", httpClient, dialer).CaptureArtwork(context.Background(), 1024)
	require.NoError(t, err)
	assert.False(t, frame.Clipped, "no artwork element found: the whole viewport, flagged")
	assert.Empty(t, frame.Masks)
}

func TestCaptureArtworkScriptExceptionIsAnError(t *testing.T) {
	ctrl := gomock.NewController(t)
	httpClient, dialer, conn := artworkConn(t, ctrl)
	conn.EXPECT().WriteMessage(websocket.TextMessage, gomock.Any()).Return(nil)
	conn.EXPECT().ReadMessage().Return(websocket.TextMessage, []byte(`{"id":1,"result":{"result":{"type":"object"},"exceptionDetails":{"text":"Uncaught"}}}`), nil)
	_, err := NewKiosk("http://127.0.0.1:9222", httpClient, dialer).CaptureArtwork(context.Background(), 1024)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "script threw")
}

func TestCaptureArtworkRejectsABadEdge(t *testing.T) {
	c := NewKiosk("http://127.0.0.1:9222", nil, nil)
	_, err := c.CaptureArtwork(context.Background(), 0)
	require.Error(t, err)
	_, err = c.CaptureArtwork(context.Background(), MaxDimension+1)
	require.Error(t, err)
}

func TestMasksInFrameScalesByTheDecodedSize(t *testing.T) {
	// A device pixel ratio of 2: the PNG is twice what the CSS scale says;
	// the mask follows the image, not the requested scale.
	got := masksInFrame([]cssRect{{X: 10, Y: 10, W: 20, H: 20}}, cssRect{X: 0, Y: 0, W: 100, H: 100}, image.Rect(0, 0, 200, 200))
	assert.Equal(t, []image.Rectangle{image.Rect(20, 20, 60, 60)}, got)
	assert.Empty(t, masksInFrame([]cssRect{{X: 500, Y: 500, W: 10, H: 10}}, cssRect{W: 100, H: 100}, image.Rect(0, 0, 100, 100)), "outside the clip: dropped")
}
