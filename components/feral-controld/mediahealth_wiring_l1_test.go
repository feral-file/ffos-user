package main

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-controld/config"
	"github.com/feral-file/ffos-user/components/feral-controld/mediahealth"
)

type noopRenderer struct{}

func (noopRenderer) Render(context.Context, string, time.Duration) (*mediahealth.Frame, error) {
	return nil, mediahealth.ErrRendererUnavailable
}

// The L1 render probe is an experiment: nothing runs unless the config says
// so AND the offline cache's renderer exists; a bad fingerprint is dropped,
// never fatal.
func TestWireMediaHealthL1_OffByDefaultAndNeedsARenderer(t *testing.T) {
	store, err := mediahealth.Open(filepath.Join(t.TempDir(), "store.db"), mediahealth.Options{})
	require.NoError(t, err)
	defer func() { _ = store.Close() }()
	asleep := func() bool { return false }

	assert.Nil(t, wireMediaHealthL1(config.MediaHealthConfig{}, store, noopRenderer{}, asleep, nil, zap.NewNop()), "absent block: off")
	assert.Nil(t, wireMediaHealthL1(config.MediaHealthConfig{L1: &config.MediaHealthL1Config{}}, store, noopRenderer{}, asleep, nil, zap.NewNop()), "present but not enabled: off")
	assert.Nil(t, wireMediaHealthL1(config.MediaHealthConfig{L1: &config.MediaHealthL1Config{Enabled: true}}, store, nil, asleep, nil, zap.NewNop()), "enabled without a renderer: off")

	cfg := config.MediaHealthConfig{L1: &config.MediaHealthL1Config{
		Enabled: true,
		KnownBad: []config.MediaHealthFingerprint{
			{PHash: "0xdeadbeef", MaxDistance: 6, Label: "ok-entry"},
			{PHash: "not-hex", MaxDistance: 6, Label: "dropped"},
		},
	}}
	l1 := wireMediaHealthL1(cfg, store, noopRenderer{}, asleep, nil, zap.NewNop())
	require.NotNil(t, l1)
	assert.False(t, l1.Step(context.Background()), "awake: it renders nothing")
}
