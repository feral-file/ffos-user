package playlistschedule_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/display-protocol/dp1-go/extension/playlists"
	dp1playlist "github.com/display-protocol/dp1-go/playlist"
	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/feral-file/ffos-user/components/feral-controld/constant"
	"github.com/feral-file/ffos-user/components/feral-controld/dp1"
	"github.com/feral-file/ffos-user/components/feral-controld/mocks"
	"github.com/feral-file/ffos-user/components/feral-controld/playlistschedule"
	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

func TestFileStoreLoad(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockOS := mocks.NewMockOS(ctrl)
	json := wrapper.NewJSON()
	store := playlistschedule.NewFileStore(mockOS, json)
	playlist := storeTestPlaylist()
	data, err := json.Marshal(playlist)
	require.NoError(t, err)

	mockOS.EXPECT().
		ReadFile(constant.DISPLAY_AT_PLAYLIST_FILE).
		Return(data, nil)
	mockOS.EXPECT().
		IsNotExist(nil).
		Return(false)

	got, err := store.Load()
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, []string{"future"}, []string{got.Items[0].ID})
}

func TestFileStoreLoadMissingReturnsNil(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockOS := mocks.NewMockOS(ctrl)
	store := playlistschedule.NewFileStore(mockOS, wrapper.NewJSON())
	notExist := os.ErrNotExist

	mockOS.EXPECT().
		ReadFile(constant.DISPLAY_AT_PLAYLIST_FILE).
		Return(nil, notExist)
	mockOS.EXPECT().
		IsNotExist(notExist).
		Return(true)

	got, err := store.Load()
	require.NoError(t, err)
	assert.Nil(t, got)
}

func TestFileStoreLoadCorruptJSONReturnsError(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockOS := mocks.NewMockOS(ctrl)
	store := playlistschedule.NewFileStore(mockOS, wrapper.NewJSON())

	mockOS.EXPECT().
		ReadFile(constant.DISPLAY_AT_PLAYLIST_FILE).
		Return([]byte(`{`), nil)
	mockOS.EXPECT().
		IsNotExist(nil).
		Return(false)

	got, err := store.Load()
	require.Error(t, err)
	assert.Nil(t, got)
	assert.Contains(t, err.Error(), "decode displayAt playlist cache")
}

func TestFileStoreSaveWritesTempThenRenames(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockOS := mocks.NewMockOS(ctrl)
	store := playlistschedule.NewFileStore(mockOS, wrapper.NewJSON())
	stateDir := filepath.Dir(constant.DISPLAY_AT_PLAYLIST_FILE)
	tmp := constant.DISPLAY_AT_PLAYLIST_FILE + ".tmp"

	mockOS.EXPECT().
		MkdirAll(stateDir, os.FileMode(0o750)).
		Return(nil)
	mockOS.EXPECT().
		WriteFile(tmp, gomock.AssignableToTypeOf([]byte{}), os.FileMode(0o600)).
		DoAndReturn(func(_ string, data []byte, _ os.FileMode) error {
			assert.Contains(t, string(data), `"id":"future"`)
			return nil
		})
	mockOS.EXPECT().
		Rename(tmp, constant.DISPLAY_AT_PLAYLIST_FILE).
		Return(nil)

	require.NoError(t, store.Save(storeTestPlaylist()))
}

func TestFileStoreSavePropagatesWriteAndRenameFailures(t *testing.T) {
	for _, tt := range []struct {
		name string
		run  func(*mocks.MockOS, playlistschedule.Store) error
		want string
	}{
		{
			name: "write",
			run: func(mockOS *mocks.MockOS, store playlistschedule.Store) error {
				writeErr := errors.New("disk full")
				mockOS.EXPECT().MkdirAll(gomock.Any(), gomock.Any()).Return(nil)
				mockOS.EXPECT().WriteFile(gomock.Any(), gomock.Any(), gomock.Any()).Return(writeErr)
				return store.Save(storeTestPlaylist())
			},
			want: "write displayAt playlist cache",
		},
		{
			name: "rename",
			run: func(mockOS *mocks.MockOS, store playlistschedule.Store) error {
				renameErr := errors.New("rename failed")
				mockOS.EXPECT().MkdirAll(gomock.Any(), gomock.Any()).Return(nil)
				mockOS.EXPECT().WriteFile(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil)
				mockOS.EXPECT().Rename(gomock.Any(), gomock.Any()).Return(renameErr)
				return store.Save(storeTestPlaylist())
			},
			want: "finalize displayAt playlist cache",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()

			mockOS := mocks.NewMockOS(ctrl)
			store := playlistschedule.NewFileStore(mockOS, wrapper.NewJSON())

			err := tt.run(mockOS, store)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.want)
		})
	}
}

func TestFileStoreClearWritesEmptyTempThenRenames(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockOS := mocks.NewMockOS(ctrl)
	store := playlistschedule.NewFileStore(mockOS, wrapper.NewJSON())
	stateDir := filepath.Dir(constant.DISPLAY_AT_PLAYLIST_FILE)
	tmp := constant.DISPLAY_AT_PLAYLIST_FILE + ".tmp"

	mockOS.EXPECT().
		MkdirAll(stateDir, os.FileMode(0o750)).
		Return(nil)
	mockOS.EXPECT().
		WriteFile(tmp, []byte(nil), os.FileMode(0o600)).
		Return(nil)
	mockOS.EXPECT().
		Rename(tmp, constant.DISPLAY_AT_PLAYLIST_FILE).
		Return(nil)

	require.NoError(t, store.Clear())
}

func storeTestPlaylist() *dp1.Playlist {
	displayAt := "2026-07-22T00:00:00Z"
	return &dp1.Playlist{
		Playlist: dp1playlist.Playlist{
			Title:    "Daily",
			Schedule: &playlists.Schedule{ByDisplayAt: true},
			Items: []dp1playlist.PlaylistItem{
				{
					ID:        "future",
					Title:     "Future",
					Source:    "https://example.com/future.html",
					DisplayAt: &displayAt,
				},
			},
		},
	}
}
