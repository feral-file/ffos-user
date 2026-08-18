package screenshot

import (
	"bytes"
	"context"
	"image"
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

func TestLiveCaptureNativeAndCustomBoundsCoexistWithPersistentClient(t *testing.T) {
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
	assertPersistentClientUsable(t, commandClient)

	native, err := capturer.Capture(context.Background(), Bounds{})
	require.NoError(t, err)
	nativeBounds := requireValidPNG(t, native)
	assert.Positive(t, nativeBounds.Dx())
	assert.Positive(t, nativeBounds.Dy())

	bounded, err := capturer.Capture(context.Background(), Bounds{Width: 800, Height: 800})
	require.NoError(t, err)
	boundedBounds := requireValidPNG(t, bounded)
	assert.LessOrEqual(t, boundedBounds.Dx(), 800)
	assert.LessOrEqual(t, boundedBounds.Dy(), 800)
	assert.True(
		t,
		boundedBounds.Dx() == 800 || boundedBounds.Dy() == 800,
		"expected one edge to reach the requested box, got %dx%d",
		boundedBounds.Dx(),
		boundedBounds.Dy(),
	)

	// The long-lived command connection must still answer after both captures
	// used their own short-lived CDP connections.
	assertPersistentClientUsable(t, commandClient)
	t.Logf(
		"live FF1 capture evidence: native PNG %dx%d, bounded PNG %dx%d; reported dimensions matched decoded PNG; persistent CDP remained usable",
		nativeBounds.Dx(),
		nativeBounds.Dy(),
		boundedBounds.Dx(),
		boundedBounds.Dy(),
	)
}

func requireValidPNG(t *testing.T, captured *Image) image.Rectangle {
	t.Helper()

	decoded, err := png.Decode(bytes.NewReader(captured.Data))
	require.NoError(t, err)
	bounds := decoded.Bounds()
	assert.Equal(t, bounds.Dx(), captured.Width)
	assert.Equal(t, bounds.Dy(), captured.Height)
	assert.NotEmpty(t, captured.SHA256)
	assert.False(t, captured.CapturedAt.IsZero())
	return bounds
}

func assertPersistentClientUsable(t *testing.T, commandClient cdp.CDP) {
	t.Helper()

	result, err := commandClient.Send(cdp.METHOD_EVALUATE, map[string]interface{}{
		"expression":    `JSON.stringify({"alive":true})`,
		"returnByValue": true,
	})
	require.NoError(t, err)
	assert.Equal(t, map[string]interface{}{"alive": true}, result)
}
