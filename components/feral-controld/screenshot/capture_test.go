package screenshot

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"hash/crc32"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"testing"

	"github.com/golang/mock/gomock"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/feral-file/ffos-user/components/feral-controld/mocks"
)

func TestFitDimensions(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		nativeWidth  int
		nativeHeight int
		bounds       Bounds
		wantWidth    int
		wantHeight   int
		wantScale    float64
	}{
		{
			name:         "native size without bounds",
			nativeWidth:  1920,
			nativeHeight: 1080,
			bounds:       Bounds{},
			wantWidth:    1920,
			wantHeight:   1080,
			wantScale:    1,
		},
		{
			name:         "width only",
			nativeWidth:  1920,
			nativeHeight: 1080,
			bounds:       Bounds{Width: 800},
			wantWidth:    800,
			wantHeight:   450,
			wantScale:    800.0 / 1920.0,
		},
		{
			name:         "height only",
			nativeWidth:  1920,
			nativeHeight: 1080,
			bounds:       Bounds{Height: 600},
			wantWidth:    1067,
			wantHeight:   600,
			wantScale:    600.0 / 1080.0,
		},
		{
			name:         "square box preserves landscape ratio",
			nativeWidth:  1920,
			nativeHeight: 1080,
			bounds:       Bounds{Width: 800, Height: 800},
			wantWidth:    800,
			wantHeight:   450,
			wantScale:    800.0 / 1920.0,
		},
		{
			name:         "portrait box is height limited",
			nativeWidth:  1080,
			nativeHeight: 1920,
			bounds:       Bounds{Width: 1000, Height: 600},
			wantWidth:    338,
			wantHeight:   600,
			wantScale:    600.0 / 1920.0,
		},
		{
			name:         "single bound cannot exceed aggregate pixel budget",
			nativeWidth:  1080,
			nativeHeight: 1920,
			bounds:       Bounds{Width: MaxDimension},
			wantWidth:    2160,
			wantHeight:   3840,
			wantScale:    2,
		},
		{
			name:         "full 4k UHD box is deliverable",
			nativeWidth:  3840,
			nativeHeight: 2160,
			bounds:       Bounds{Width: 3840, Height: 2160},
			wantWidth:    3840,
			wantHeight:   2160,
			wantScale:    1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			width, height, scale, err := fitDimensions(tt.nativeWidth, tt.nativeHeight, tt.bounds)

			require.NoError(t, err)
			assert.Equal(t, tt.wantWidth, width)
			assert.Equal(t, tt.wantHeight, height)
			assert.InDelta(t, tt.wantScale, scale, 0.000001)
		})
	}
}

func TestFitDimensionsRejectsBoxAbovePixelBudget(t *testing.T) {
	_, _, _, err := fitDimensions(4096, 4096, Bounds{Width: 4096, Height: 4096})

	require.Error(t, err)
	assert.ErrorContains(t, err, "cannot exceed 8294400 pixels")
}

func TestFitDimensionsRoundingStaysInsidePixelBudget(t *testing.T) {
	width, height, _, err := fitDimensions(4001, 2003, Bounds{Width: MaxDimension})

	require.NoError(t, err)
	assert.LessOrEqual(t, width*height, MaxPixels)
}

func TestCaptureMemoryBudgetCarriesMaximumSurface(t *testing.T) {
	// Four color bytes plus one PNG filter byte per maximum-length scanline.
	maxRawScanlines := MaxPixels*4 + MaxDimension
	assert.Less(t, maxRawScanlines, maxScreenshotBytes)

	// Leave 1 MiB beyond the largest admitted base64 body for the CDP JSON envelope.
	assert.Less(t, base64.StdEncoding.EncodedLen(maxScreenshotBytes)+(1<<20), maxCDPMessageBytes)
}

func TestCapturerCaptureCustomSize(t *testing.T) {
	ctrl := gomock.NewController(t)
	httpClient := mocks.NewMockHTTPClient(ctrl)
	dialer := mocks.NewMockWebSocketDialer(ctrl)
	conn := mocks.NewMockWebSocketConn(ctrl)

	targets := `[{"type":"page","webSocketDebuggerUrl":"ws://127.0.0.1:9222/devtools/page/1"}]`
	httpClient.EXPECT().Do(gomock.Any()).DoAndReturn(func(req *http.Request) (*http.Response, error) {
		assert.Equal(t, "http://127.0.0.1:9222/json", req.URL.String())
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(bytes.NewBufferString(targets)),
		}, nil
	})
	dialer.EXPECT().DialContext(gomock.Any(), "ws://127.0.0.1:9222/devtools/page/1", http.Header(nil)).Return(conn, nil, nil)
	conn.EXPECT().SetReadLimit(int64(maxCDPMessageBytes))
	conn.EXPECT().SetReadDeadline(gomock.Any()).Return(nil)
	conn.EXPECT().SetWriteDeadline(gomock.Any()).Return(nil)
	conn.EXPECT().Close().Return(nil)

	conn.EXPECT().WriteMessage(websocket.TextMessage, gomock.Any()).DoAndReturn(func(_ int, data []byte) error {
		var request cdpRequest
		require.NoError(t, json.Unmarshal(data, &request))
		assert.Equal(t, 1, request.ID)
		assert.Equal(t, methodGetLayoutMetrics, request.Method)
		return nil
	})
	conn.EXPECT().ReadMessage().Return(websocket.TextMessage, []byte(`{
		"id":1,
		"result":{"cssVisualViewport":{"pageX":100,"pageY":50,"clientWidth":1920,"clientHeight":1080}}
	}`), nil)

	pngBytes := makePNG(t, 800, 450)
	conn.EXPECT().WriteMessage(websocket.TextMessage, gomock.Any()).DoAndReturn(func(_ int, data []byte) error {
		var request struct {
			ID     int                    `json:"id"`
			Method string                 `json:"method"`
			Params map[string]interface{} `json:"params"`
		}
		require.NoError(t, json.Unmarshal(data, &request))
		assert.Equal(t, 2, request.ID)
		assert.Equal(t, methodCaptureScreenshot, request.Method)
		clip, ok := request.Params["clip"].(map[string]interface{})
		require.True(t, ok)
		assert.InDelta(t, 800.0/1920.0, clip["scale"], 0.000001)
		assert.Equal(t, float64(100), clip["x"])
		assert.Equal(t, float64(50), clip["y"])
		assert.Equal(t, float64(1920), clip["width"])
		assert.Equal(t, float64(1080), clip["height"])
		return nil
	})
	conn.EXPECT().ReadMessage().Return(websocket.TextMessage, captureResponse(t, 2, pngBytes), nil)

	capturer := New("http://127.0.0.1:9222", httpClient, dialer)
	got, err := capturer.Capture(context.Background(), Bounds{Width: 800, Height: 800})

	require.NoError(t, err)
	assert.Equal(t, pngBytes, got.Data)
	assert.Equal(t, 800, got.Width)
	assert.Equal(t, 450, got.Height)
	assert.NotEmpty(t, got.SHA256)
	assert.False(t, got.CapturedAt.IsZero())
}

func TestCapturerCaptureRejectsImageOutsideRequestedBounds(t *testing.T) {
	ctrl := gomock.NewController(t)
	httpClient := mocks.NewMockHTTPClient(ctrl)
	dialer := mocks.NewMockWebSocketDialer(ctrl)
	conn := mocks.NewMockWebSocketConn(ctrl)

	targets := `[{"type":"page","webSocketDebuggerUrl":"ws://127.0.0.1:9222/devtools/page/1"}]`
	httpClient.EXPECT().Do(gomock.Any()).Return(&http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(bytes.NewBufferString(targets)),
	}, nil)
	dialer.EXPECT().DialContext(gomock.Any(), gomock.Any(), http.Header(nil)).Return(conn, nil, nil)
	conn.EXPECT().SetReadLimit(int64(maxCDPMessageBytes))
	conn.EXPECT().SetReadDeadline(gomock.Any()).Return(nil)
	conn.EXPECT().SetWriteDeadline(gomock.Any()).Return(nil)
	conn.EXPECT().Close().Return(nil)
	conn.EXPECT().WriteMessage(websocket.TextMessage, gomock.Any()).Return(nil)
	conn.EXPECT().ReadMessage().Return(websocket.TextMessage, []byte(`{
		"id":1,
		"result":{"cssLayoutViewport":{"clientWidth":1920,"clientHeight":1080}}
	}`), nil)
	conn.EXPECT().WriteMessage(websocket.TextMessage, gomock.Any()).Return(nil)
	conn.EXPECT().ReadMessage().Return(websocket.TextMessage, captureResponse(t, 2, makePNG(t, 801, 450)), nil)

	capturer := New("http://127.0.0.1:9222", httpClient, dialer)
	_, err := capturer.Capture(context.Background(), Bounds{Width: 800, Height: 800})

	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidImage)
}

func TestCapturerCaptureReturnsUnavailableWithoutSinglePageTarget(t *testing.T) {
	ctrl := gomock.NewController(t)
	httpClient := mocks.NewMockHTTPClient(ctrl)
	dialer := mocks.NewMockWebSocketDialer(ctrl)

	httpClient.EXPECT().Do(gomock.Any()).Return(&http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(bytes.NewBufferString(`[{"type":"worker"}]`)),
	}, nil)

	capturer := New("http://127.0.0.1:9222", httpClient, dialer)
	_, err := capturer.Capture(context.Background(), Bounds{})

	require.Error(t, err)
	assert.ErrorIs(t, err, ErrUnavailable)
}

func TestCapturerCaptureReturnsBusyWithoutStartingSecondCapture(t *testing.T) {
	capturer := &capturer{captureSlot: make(chan struct{}, 1)}
	capturer.captureSlot <- struct{}{}

	_, err := capturer.Capture(context.Background(), Bounds{})

	assert.ErrorIs(t, err, ErrBusy)
}

func TestDecodeScreenshotRejectsTruncatedPNG(t *testing.T) {
	fullPNG := makePNG(t, 32, 18)
	truncatedPNG := fullPNG[:len(fullPNG)-12] // Remove the required IEND chunk.

	_, configErr := png.DecodeConfig(bytes.NewReader(truncatedPNG))
	require.NoError(t, configErr, "regression fixture must pass metadata-only validation")

	_, err := decodeScreenshot(base64.StdEncoding.EncodeToString(truncatedPNG), Bounds{})

	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidImage)
}

func TestDecodeScreenshotRejectsOversizedDeclaredSurface(t *testing.T) {
	data := pngWithDeclaredDimensions(t, MaxDimension+1, MaxDimension+1)

	_, err := decodeScreenshot(base64.StdEncoding.EncodeToString(data), Bounds{})

	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidImage)
	assert.ErrorContains(t, err, "pixel decode limit")
}

func captureResponse(t *testing.T, id int, data []byte) []byte {
	t.Helper()

	response, err := json.Marshal(map[string]interface{}{
		"id": id,
		"result": map[string]string{
			"data": base64.StdEncoding.EncodeToString(data),
		},
	})
	require.NoError(t, err)
	return response
}

func makePNG(t *testing.T, width, height int) []byte {
	t.Helper()

	img := image.NewRGBA(image.Rect(0, 0, width, height))
	img.Set(0, 0, color.RGBA{R: 0xff, A: 0xff})
	var buf bytes.Buffer
	require.NoError(t, png.Encode(&buf, img))
	return buf.Bytes()
}

func pngWithDeclaredDimensions(t *testing.T, width, height uint32) []byte {
	t.Helper()

	data := makePNG(t, 1, 1)
	binary.BigEndian.PutUint32(data[16:20], width)
	binary.BigEndian.PutUint32(data[20:24], height)
	binary.BigEndian.PutUint32(data[29:33], crc32.ChecksumIEEE(data[12:29]))
	return data
}
