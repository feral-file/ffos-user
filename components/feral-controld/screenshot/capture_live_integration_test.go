package screenshot

import (
	"bytes"
	"context"
	"image/png"
	"os"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-controld/cdp"
	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

func TestLiveCaptureCustomBounds(t *testing.T) {
	endpoint := os.Getenv("FF1_LIVE_CDP_ENDPOINT")
	if endpoint == "" {
		t.Skip("set FF1_LIVE_CDP_ENDPOINT to run against Chromium")
	}

	dialer := wrapper.NewWebSocketDialer(&websocket.Dialer{HandshakeTimeout: 5 * time.Second})
	commandClient := cdp.New(
		endpoint,
		dialer,
		wrapper.NewIO(),
		wrapper.NewJSON(),
		wrapper.NewHTTPClient(),
		zap.NewNop(),
	)
	require.NoError(t, commandClient.Init(context.Background()))
	defer commandClient.Close()

	capturer := New(endpoint, wrapper.NewHTTPClient(), dialer)

	image, err := capturer.Capture(context.Background(), Bounds{Width: 800, Height: 800})

	require.NoError(t, err)
	assert.LessOrEqual(t, image.Width, 800)
	assert.LessOrEqual(t, image.Height, 800)
	assert.True(t, image.Width == 800 || image.Height == 800, "expected one edge to reach the requested box, got %dx%d", image.Width, image.Height)
	_, err = png.DecodeConfig(bytes.NewReader(image.Data))
	require.NoError(t, err)
}
