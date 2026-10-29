package commandrouter_test

import (
	"errors"
	"testing"

	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	dp1playlist "github.com/display-protocol/dp1-go/playlist"

	"github.com/feral-file/ffos-user/components/feral-controld/cdp"
	"github.com/feral-file/ffos-user/components/feral-controld/commandrouter"
	"github.com/feral-file/ffos-user/components/feral-controld/commands"
	"github.com/feral-file/ffos-user/components/feral-controld/dp1"
)

func sleepTestPlaylist() *dp1.Playlist {
	return &dp1.Playlist{
		Playlist: dp1playlist.Playlist{
			Items: []dp1playlist.PlaylistItem{
				{ID: "item1", Source: "https://example.com/video.mp4", Duration: float64Ptr(300), License: "open"},
			},
		},
	}
}

// A person's cast (hub or relayer) while the player sleeps wakes it first,
// then casts. The wake must land before the player send.
func TestCommandHandler_Process_DisplayPlaylist_ControllerCastWakesSleepingPlayer(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()
	commandrouter.SetPlayerAsleepForTest(ts.handler, func() bool { return true })

	playlistURL := "https://example.com/playlist.json"
	ts.mockDP1.EXPECT().
		ProcessPlaylistURLForCast(gomock.Any(), playlistURL).
		Return(sleepTestPlaylist(), nil).
		Times(1)
	wake := ts.mockExecutor.EXPECT().
		Execute(gomock.Any(), commands.Command{Type: commands.CMD_WAKE_NOW}).
		Return(map[string]any{"ok": true}, nil).
		Times(1)
	send := ts.mockCDP.EXPECT().
		Send(cdp.METHOD_EVALUATE, gomock.Any()).
		Return(playerOkResponse(), nil).
		Times(1)
	gomock.InOrder(wake, send)
	ts.mockStatusPoller.EXPECT().ForceRefresh().Times(1)

	result, err := ts.handler.Process(commands.FromController(ts.ctx), commands.Command{
		Type:      commands.CMD_DISPLAY_PLAYLIST,
		Arguments: map[string]any{"playlistUrl": playlistURL},
	})

	require.NoError(t, err)
	assert.NotNil(t, result)
}

// The device's own casts (scheduler cutovers, OOM recovery) carry no
// controller mark and leave a sleeping player asleep: no wakeNow is issued.
func TestCommandHandler_Process_DisplayPlaylist_DeviceCastLeavesSleepingPlayerAsleep(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()
	commandrouter.SetPlayerAsleepForTest(ts.handler, func() bool { return true })

	playlistURL := "https://example.com/playlist.json"
	expectDisplayPlaylistSuccess(ts, playlistURL, sleepTestPlaylist())

	result, err := ts.handler.Process(ts.ctx, commands.Command{
		Type:      commands.CMD_DISPLAY_PLAYLIST,
		Arguments: map[string]any{"playlistUrl": playlistURL},
	})

	require.NoError(t, err)
	assert.NotNil(t, result)
}

// An awake player needs no wake, whoever casts.
func TestCommandHandler_Process_DisplayPlaylist_ControllerCastWhileAwakeDoesNotWake(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()
	commandrouter.SetPlayerAsleepForTest(ts.handler, func() bool { return false })

	playlistURL := "https://example.com/playlist.json"
	ts.mockDP1.EXPECT().
		ProcessPlaylistURLForCast(gomock.Any(), playlistURL).
		Return(sleepTestPlaylist(), nil).
		Times(1)
	ts.mockCDP.EXPECT().
		Send(cdp.METHOD_EVALUATE, gomock.Any()).
		Return(playerOkResponse(), nil).
		Times(1)
	ts.mockStatusPoller.EXPECT().ForceRefresh().Times(1)

	result, err := ts.handler.Process(commands.FromController(ts.ctx), commands.Command{
		Type:      commands.CMD_DISPLAY_PLAYLIST,
		Arguments: map[string]any{"playlistUrl": playlistURL},
	})

	require.NoError(t, err)
	assert.NotNil(t, result)
}

// A wake that fails fails the cast: nothing reaches the player, and the reply
// does not claim a display that cannot happen.
func TestCommandHandler_Process_DisplayPlaylist_WakeFailureFailsCast(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()
	commandrouter.SetPlayerAsleepForTest(ts.handler, func() bool { return true })

	ts.mockExecutor.EXPECT().
		Execute(gomock.Any(), commands.Command{Type: commands.CMD_WAKE_NOW}).
		Return(nil, errors.New("player not connected")).
		Times(1)

	result, err := ts.handler.Process(commands.FromController(ts.ctx), commands.Command{
		Type:      commands.CMD_DISPLAY_PLAYLIST,
		Arguments: map[string]any{"playlistUrl": "https://example.com/playlist.json"},
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "player not connected")
	assert.Nil(t, result)
}

// Commands that do not put artwork on screen never wake the player.
func TestCommandHandler_Process_RefreshArtwork_DoesNotWakeSleepingPlayer(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()
	commandrouter.SetPlayerAsleepForTest(ts.handler, func() bool { return true })

	ts.mockCDP.EXPECT().Send(gomock.Any(), gomock.Any()).Return(playerOkResponse(), nil).AnyTimes()
	ts.mockStatusPoller.EXPECT().ForceRefresh().AnyTimes()

	_, err := ts.handler.Process(commands.FromController(ts.ctx), commands.Command{
		Type:      commands.CMD_REFRESH_ARTWORK,
		Arguments: map[string]any{},
	})

	require.NoError(t, err)
}
