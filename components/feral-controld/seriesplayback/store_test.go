package seriesplayback_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/feral-file/ffos-user/components/feral-controld/dp1"
	"github.com/feral-file/ffos-user/components/feral-controld/seriesplayback"
)

func TestOpen_WithNoFileYetDefaultsToFollowTheDocument(t *testing.T) {
	store, err := seriesplayback.Open(filepath.Join(t.TempDir(), "series-playback.json"))
	require.NoError(t, err)

	store.Lock()
	defer store.Unlock()
	assert.Nil(t, store.CurrentLocked().Mode)
	assert.True(t, store.DurableLocked())
}

// AC2 of feral-file/ffos-user#425: nullable, absent meaning "follow the
// document", distinguishable from an explicit first.
func TestUpdateLocked_PersistsAnExplicitModeDistinctFromAbsent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "series-playback.json")
	store, err := seriesplayback.Open(path)
	require.NoError(t, err)

	first := dp1.SeriesPlaybackFirst
	store.Lock()
	setting, err := store.UpdateLocked(&first)
	store.Unlock()
	require.NoError(t, err)
	require.NotNil(t, setting.Mode)
	assert.Equal(t, dp1.SeriesPlaybackFirst, *setting.Mode)

	reopened, err := seriesplayback.Open(path)
	require.NoError(t, err)
	reopened.Lock()
	defer reopened.Unlock()
	require.NotNil(t, reopened.CurrentLocked().Mode)
	assert.Equal(t, dp1.SeriesPlaybackFirst, *reopened.CurrentLocked().Mode)
}

func TestUpdateLocked_NilModeClearsBackToFollowTheDocument(t *testing.T) {
	path := filepath.Join(t.TempDir(), "series-playback.json")
	store, err := seriesplayback.Open(path)
	require.NoError(t, err)

	full := dp1.SeriesPlaybackFull
	store.Lock()
	_, err = store.UpdateLocked(&full)
	require.NoError(t, err)
	setting, err := store.UpdateLocked(nil)
	store.Unlock()
	require.NoError(t, err)
	assert.Nil(t, setting.Mode)

	reopened, err := seriesplayback.Open(path)
	require.NoError(t, err)
	reopened.Lock()
	defer reopened.Unlock()
	assert.Nil(t, reopened.CurrentLocked().Mode)
}

// AC4 of feral-file/ffos-user#425: random is accepted and stored as its own
// value — only NarrowEditions collapses it to first, not the store.
func TestUpdateLocked_StoresRandomAsItsOwnValue(t *testing.T) {
	store, err := seriesplayback.Open(filepath.Join(t.TempDir(), "series-playback.json"))
	require.NoError(t, err)

	random := dp1.SeriesPlaybackRandom
	store.Lock()
	setting, err := store.UpdateLocked(&random)
	store.Unlock()
	require.NoError(t, err)
	require.NotNil(t, setting.Mode)
	assert.Equal(t, dp1.SeriesPlaybackRandom, *setting.Mode)
}

func TestFallback_ReportsNonDurable(t *testing.T) {
	store := seriesplayback.Fallback(filepath.Join(t.TempDir(), "unreadable", "series-playback.json"))
	store.Lock()
	defer store.Unlock()
	assert.False(t, store.DurableLocked())
	assert.Nil(t, store.CurrentLocked().Mode)
}

func TestOpen_RejectsATruncatedFileRatherThanSilentlyResettingIt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "series-playback.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"versio`), 0o600))

	_, err := seriesplayback.Open(path)
	assert.Error(t, err)
}

// The owner's series-playback override must not outlive their claim, same
// as content policy and the device name.
func TestResetLocked_ReturnsToFollowTheDocumentAndRemovesTheFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "series-playback.json")
	store, err := seriesplayback.Open(path)
	require.NoError(t, err)

	first := dp1.SeriesPlaybackFirst
	store.Lock()
	_, err = store.UpdateLocked(&first)
	require.NoError(t, err)
	setting, err := store.ResetLocked()
	store.Unlock()
	require.NoError(t, err)
	assert.Nil(t, setting.Mode)

	_, statErr := os.Stat(path)
	assert.True(t, os.IsNotExist(statErr))
}
