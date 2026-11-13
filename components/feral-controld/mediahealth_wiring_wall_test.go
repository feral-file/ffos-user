package main

import (
	"context"
	"errors"
	"image"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-controld/config"
	"github.com/feral-file/ffos-user/components/feral-controld/mediahealth"
	"github.com/feral-file/ffos-user/components/feral-controld/screenshot"
)

type stubArtwork struct {
	frame   *screenshot.ArtworkFrame
	err     error
	maxEdge int
}

func (s *stubArtwork) CaptureArtwork(_ context.Context, maxEdge int) (*screenshot.ArtworkFrame, error) {
	s.maxEdge = maxEdge
	return s.frame, s.err
}

func TestWireWallCapture_OffUnlessEnabled(t *testing.T) {
	store, err := mediahealth.Open(filepath.Join(t.TempDir(), "store.db"), mediahealth.Options{})
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	svc := mediahealth.New(mediahealth.Config{Store: store})
	kiosk := &stubArtwork{}

	assert.Nil(t, wireWallCapture(config.MediaHealthConfig{}, svc, kiosk, nil, zap.NewNop()), "absent block: off")
	assert.Nil(t, wireWallCapture(config.MediaHealthConfig{WallCapture: &config.MediaHealthWallCaptureConfig{}}, svc, kiosk, nil, zap.NewNop()), "present but not enabled: off")
	on := config.MediaHealthConfig{WallCapture: &config.MediaHealthWallCaptureConfig{Enabled: true}}
	assert.Nil(t, wireWallCapture(on, svc, nil, nil, zap.NewNop()), "no kiosk capturer: off")
	assert.NotNil(t, wireWallCapture(on, svc, kiosk, nil, zap.NewNop()))
}

// Every capturer failure — busy, covered, unreachable — is "not evidence";
// a frame passes through with its masks; the edge defaults to 1024.
func TestKioskWall_AdaptsTheArtworkCapture(t *testing.T) {
	for _, cause := range []error{screenshot.ErrBusy, screenshot.ErrCovered, screenshot.ErrUnavailable} {
		_, err := kioskWall{kiosk: &stubArtwork{err: cause}, maxEdge: 1024}.CaptureWall(context.Background())
		assert.True(t, errors.Is(err, mediahealth.ErrFrameUnavailable), "%v", cause)
		assert.True(t, errors.Is(err, cause), "the cause stays visible in the log")
	}
	stub := &stubArtwork{frame: &screenshot.ArtworkFrame{
		Image: image.NewRGBA(image.Rect(0, 0, 8, 8)), Masks: []image.Rectangle{image.Rect(0, 4, 2, 8)}, Clipped: true,
	}}
	f, err := kioskWall{kiosk: stub, maxEdge: defaultWallMaxEdge}.CaptureWall(context.Background())
	require.NoError(t, err)
	assert.Equal(t, []image.Rectangle{image.Rect(0, 4, 2, 8)}, f.Masks)
	assert.True(t, f.Clipped)
	assert.Equal(t, 1024, stub.maxEdge)
}
