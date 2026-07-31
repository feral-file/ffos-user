package commandrouter_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest"

	"github.com/display-protocol/dp1-go/extension/playlists"
	dp1playlist "github.com/display-protocol/dp1-go/playlist"

	"github.com/feral-file/ffos-user/components/feral-controld/cdp"
	"github.com/feral-file/ffos-user/components/feral-controld/commandrouter"
	"github.com/feral-file/ffos-user/components/feral-controld/commands"
	"github.com/feral-file/ffos-user/components/feral-controld/devicectl"
	"github.com/feral-file/ffos-user/components/feral-controld/dp1"
	"github.com/feral-file/ffos-user/components/feral-controld/mintpairing"
	"github.com/feral-file/ffos-user/components/feral-controld/mocks"
	"github.com/feral-file/ffos-user/components/feral-controld/playersession"
	"github.com/feral-file/ffos-user/components/feral-controld/status"
)

// fakeRecoverySession is a directly-controllable commandrouter.RecoverySession
// double: err, when set, is what NavigateHomeInline returns.
type fakeRecoverySession struct {
	calls int
	opts  playersession.NavOptions
	err   error
}

func (f *fakeRecoverySession) NavigateHomeInline(opts playersession.NavOptions) error {
	f.calls++
	f.opts = opts
	return f.err
}

type testSetup struct {
	ctrl             *gomock.Controller
	ctx              context.Context
	mockExecutor     *mocks.MockExecutor
	mockCDP          *mocks.MockCDP
	mockDP1          *mocks.MockDP1
	mockJSON         *mocks.MockJSON
	mockStatusPoller *mocks.MockStatusPoller
	handler          commandrouter.Handler
	logger           *zap.Logger
}

func setup(t *testing.T) *testSetup {
	ctrl := gomock.NewController(t)
	logger := zaptest.NewLogger(t, zaptest.Level(zap.FatalLevel))
	ctx := context.Background()

	mockExecutor := mocks.NewMockExecutor(ctrl)
	mockCDP := mocks.NewMockCDP(ctrl)
	mockDP1 := mocks.NewMockDP1(ctrl)
	mockStatusPoller := mocks.NewMockStatusPoller(ctrl)
	mockJSON := mocks.NewMockJSON(ctrl)
	handler := commandrouter.New(mockExecutor, mockCDP, mockDP1, mockStatusPoller, nil, nil, nil, nil, mockJSON, logger)

	return &testSetup{
		ctrl:             ctrl,
		ctx:              ctx,
		mockExecutor:     mockExecutor,
		mockCDP:          mockCDP,
		mockDP1:          mockDP1,
		mockJSON:         mockJSON,
		mockStatusPoller: mockStatusPoller,
		handler:          handler,
		logger:           logger,
	}
}

func (ts *testSetup) teardown() {
	ts.ctrl.Finish()
}

// Helper functions
func float64Ptr(f float64) *float64 {
	return &f
}

func playerOkResponse() map[string]interface{} {
	return map[string]interface{}{
		"messageID": "1",
		"message": map[string]interface{}{
			"ok": true,
		},
	}
}

func playerNotOkResponse() map[string]interface{} {
	return map[string]interface{}{
		"messageID": "1",
		"message": map[string]interface{}{
			"ok": false,
		},
	}
}

// expectDisplayPlaylistSuccess sets up mock expectations for a successful
// displayPlaylist via URL: DP1 processing, CDP send returning ok, and ForceRefresh.
func expectDisplayPlaylistSuccess(ts *testSetup, playlistURL string, playlist *dp1.Playlist) {
	ts.mockDP1.EXPECT().
		ProcessPlaylistURLForCast(ts.ctx, playlistURL).
		Return(playlist, nil).
		Times(1)

	ts.mockCDP.EXPECT().
		Send(cdp.METHOD_EVALUATE, gomock.Any()).
		Return(playerOkResponse(), nil).
		Times(1)

	ts.mockStatusPoller.EXPECT().
		ForceRefresh().
		Times(1)
}

func TestCommandHandler_Process_NoCommand(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	command := commands.Command{
		Type:      "",
		Arguments: map[string]any{},
	}

	result, err := ts.handler.Process(ts.ctx, command)

	assert.NoError(t, err)
	assert.Nil(t, result)
}

func TestCommandHandler_Process_ControldCommand(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	cmd := commands.CMD_CONNECT
	args := map[string]interface{}{"clientDevice": map[string]interface{}{"device_id": "test-device"}}
	execResult := map[string]interface{}{"ok": true}

	payload := commands.Command{
		Type:      cmd,
		Arguments: args,
	}

	ts.mockExecutor.EXPECT().
		Execute(ts.ctx, commands.Command{
			Type:      cmd,
			Arguments: args,
		}).
		Return(execResult, nil).
		Times(1)

	result, err := ts.handler.Process(ts.ctx, payload)

	assert.NoError(t, err)
	assert.NotNil(t, result)
	assert.Equal(t, execResult, result)
}

func TestCommandHandler_Process_ControldCommand_Error(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	cmd := commands.CMD_SHUTDOWN
	args := map[string]interface{}{}
	execError := errors.New("failed to shutdown")

	command := commands.Command{
		Type:      cmd,
		Arguments: args,
	}

	ts.mockExecutor.EXPECT().
		Execute(ts.ctx, commands.Command{
			Type:      cmd,
			Arguments: args,
		}).
		Return(nil, execError).
		Times(1)

	result, err := ts.handler.Process(ts.ctx, command)

	assert.Error(t, err)
	assert.Equal(t, execError, err)
	assert.Nil(t, result)
}

func TestCommandHandler_Process_MintPairingApprovalDisabled(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	result, err := ts.handler.Process(ts.ctx, commands.Command{
		Type:      commands.CMD_MINT_PAIRING_APPROVAL,
		Arguments: map[string]interface{}{},
	})

	assert.NoError(t, err)
	resp, ok := result.(map[string]any)
	assert.True(t, ok)
	assert.Equal(t, false, resp["ok"])
	errObj, ok := resp["error"].(map[string]any)
	assert.True(t, ok)
	assert.Equal(t, "not_found", errObj["code"])
}

func TestCommandHandler_Process_StartMintPairingSessionDisabled(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	result, err := ts.handler.Process(ts.ctx, commands.Command{
		Type:      commands.CMD_START_MINT_PAIRING_SESSION,
		Arguments: map[string]interface{}{},
	})

	assert.NoError(t, err)
	resp, ok := result.(map[string]any)
	assert.True(t, ok)
	assert.Equal(t, false, resp["ok"])
	errObj, ok := resp["error"].(map[string]any)
	assert.True(t, ok)
	assert.Equal(t, "disabled", errObj["code"])
}

func TestCommandHandler_Process_StartMintPairingSessionRoutesToMintPairing(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	assert.Equal(t, commands.Type("startMintPairingSession"), commands.CMD_START_MINT_PAIRING_SESSION)

	args := map[string]any{"source": "controller"}
	want := map[string]any{"ok": true, "status": "started"}
	mintSvc := &fakeMintPairingService{startResult: want}
	ts.handler = commandrouter.New(ts.mockExecutor, ts.mockCDP, ts.mockDP1, ts.mockStatusPoller, mintSvc, nil, nil, nil, ts.mockJSON, ts.logger)

	result, err := ts.handler.Process(ts.ctx, commands.Command{
		Type:      commands.CMD_START_MINT_PAIRING_SESSION,
		Arguments: args,
	})

	assert.NoError(t, err)
	assert.Equal(t, want, result)
	assert.Equal(t, args, mintSvc.startArgs)
	assert.Equal(t, 1, mintSvc.startCalls)
}

func TestCommandHandler_Process_CloseMintPairingSessionDisabled(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	result, err := ts.handler.Process(ts.ctx, commands.Command{
		Type:      commands.CMD_CLOSE_MINT_PAIRING_SESSION,
		Arguments: map[string]interface{}{},
	})

	assert.NoError(t, err)
	resp, ok := result.(map[string]any)
	assert.True(t, ok)
	assert.Equal(t, false, resp["ok"])
	errObj, ok := resp["error"].(map[string]any)
	assert.True(t, ok)
	assert.Equal(t, "disabled", errObj["code"])
}

func TestCommandHandler_Process_CloseMintPairingSessionRoutesToMintPairing(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	assert.Equal(t, commands.Type("closeMintPairingSession"), commands.CMD_CLOSE_MINT_PAIRING_SESSION)

	args := map[string]any{"source": "controller"}
	want := map[string]any{"ok": true, "status": "closed"}
	mintSvc := &fakeMintPairingService{closeResult: want}
	ts.handler = commandrouter.New(ts.mockExecutor, ts.mockCDP, ts.mockDP1, ts.mockStatusPoller, mintSvc, nil, nil, nil, ts.mockJSON, ts.logger)

	result, err := ts.handler.Process(ts.ctx, commands.Command{
		Type:      commands.CMD_CLOSE_MINT_PAIRING_SESSION,
		Arguments: args,
	})

	assert.NoError(t, err)
	assert.Equal(t, want, result)
	assert.Equal(t, args, mintSvc.closeArgs)
	assert.Equal(t, 1, mintSvc.closeCalls)
}

func TestCommandHandler_Process_MintPairingApprovalRoutesToMintPairing(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	assert.Equal(t, commands.Type("mintPairingApprovalDecision"), commands.CMD_MINT_PAIRING_APPROVAL)

	args := map[string]any{"approvalRequestID": "mpa_1", "decision": "approve"}
	want := map[string]any{"ok": true, "status": "accepted"}
	mintSvc := &fakeMintPairingService{approvalResult: want}
	ts.handler = commandrouter.New(ts.mockExecutor, ts.mockCDP, ts.mockDP1, ts.mockStatusPoller, mintSvc, nil, nil, nil, ts.mockJSON, ts.logger)

	result, err := ts.handler.Process(ts.ctx, commands.Command{
		Type:      commands.CMD_MINT_PAIRING_APPROVAL,
		Arguments: args,
	})

	assert.NoError(t, err)
	assert.Equal(t, want, result)
	assert.Equal(t, args, mintSvc.approvalArgs)
	assert.Equal(t, 1, mintSvc.approvalCalls)
}

func TestCommandHandler_Process_NewGestureCommandsRouteToExecutor(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	cases := []struct {
		name string
		cmd  commands.Type
	}{
		{name: "doubleTapGesture", cmd: commands.CMD_MOUSE_DOUBLE_TAP_EVENT},
		{name: "longPressGesture", cmd: commands.CMD_MOUSE_LONG_PRESS_EVENT},
		{name: "clickAndDragGesture", cmd: commands.CMD_MOUSE_CLICK_AND_DRAG_EVENT},
		{name: "zoomGesture", cmd: commands.CMD_ZOOM_GESTURE},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ts.mockExecutor.EXPECT().
				Execute(ts.ctx, commands.Command{
					Type:      tc.cmd,
					Arguments: map[string]interface{}{},
				}).
				Return(devicectl.CmdOK, nil).
				Times(1)

			result, err := ts.handler.Process(ts.ctx, commands.Command{
				Type:      tc.cmd,
				Arguments: map[string]interface{}{},
			})

			assert.NoError(t, err)
			assert.Equal(t, devicectl.CmdOK, result)
		})
	}
}

type fakeMintPairingService struct {
	startCalls     int
	closeCalls     int
	approvalCalls  int
	startArgs      map[string]any
	closeArgs      map[string]any
	approvalArgs   map[string]any
	startResult    any
	closeResult    any
	approvalResult any
}

func (f *fakeMintPairingService) Start(context.Context) {}

func (f *fakeMintPairingService) Stop() {}

func (f *fakeMintPairingService) HandleStartPairingSession(_ context.Context, args map[string]any) (any, error) {
	f.startCalls++
	f.startArgs = args
	return f.startResult, nil
}

func (f *fakeMintPairingService) HandleClosePairingSession(_ context.Context, args map[string]any) (any, error) {
	f.closeCalls++
	f.closeArgs = args
	return f.closeResult, nil
}

func (f *fakeMintPairingService) HandleApprovalDecision(_ context.Context, args map[string]any) (any, error) {
	f.approvalCalls++
	f.approvalArgs = args
	return f.approvalResult, nil
}

func (f *fakeMintPairingService) DisplayActive() bool { return false }

func (f *fakeMintPairingService) SetSession(mintpairing.NavigationSession) {}

func TestCommandHandler_Process_DisplayPlaylist_WithURL(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	playlistURL := "https://example.com/playlist.json"
	mockPlaylist := &dp1.Playlist{
		Playlist: dp1playlist.Playlist{
			Items: []dp1playlist.PlaylistItem{
				{
					ID:       "item1",
					Title:    "Test Item",
					Source:   "https://example.com/video.mp4",
					Duration: float64Ptr(300),
					License:  "open",
				},
			},
		},
	}

	expectDisplayPlaylistSuccess(ts, playlistURL, mockPlaylist)

	command := commands.Command{
		Type:      commands.CMD_DISPLAY_PLAYLIST,
		Arguments: map[string]interface{}{"playlistUrl": playlistURL},
	}

	result, err := ts.handler.Process(ts.ctx, command)

	assert.NoError(t, err)
	assert.NotNil(t, result)
}

func TestCommandHandler_Process_DisplayPlaylist_SyncsKioskReplayScope(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	playlistURL := "https://example.com/playlist.json"
	mockPlaylist := &dp1.Playlist{
		Playlist: dp1playlist.Playlist{
			Items: []dp1playlist.PlaylistItem{
				{ID: "item1", Source: "https://example.com/video.mp4"},
				{ID: "item2", Source: "https://example.com/app.js"},
			},
		},
	}
	expectDisplayPlaylistSuccess(ts, playlistURL, mockPlaylist)

	mockKioskReplay := mocks.NewMockOfflineCacheKioskReplay(ts.ctrl)
	mockKioskReplay.EXPECT().LockPlayback().AnyTimes()
	mockKioskReplay.EXPECT().UnlockPlayback().AnyTimes()
	mockKioskReplay.EXPECT().PlaybackGeneration().Return(uint64(0)).AnyTimes()
	mockKioskReplay.EXPECT().MarkPlaybackChanged().AnyTimes()
	mockKioskReplay.EXPECT().SyncPlaylist(ts.ctx, []string{"item1", "item2"}).Return(nil).Times(1)
	ts.handler = commandrouter.New(ts.mockExecutor, ts.mockCDP, ts.mockDP1, ts.mockStatusPoller, nil, nil, mockKioskReplay, nil, ts.mockJSON, ts.logger)

	command := commands.Command{
		Type:      commands.CMD_DISPLAY_PLAYLIST,
		Arguments: map[string]interface{}{"playlistUrl": playlistURL},
	}

	result, err := ts.handler.Process(ts.ctx, command)

	assert.NoError(t, err)
	assert.NotNil(t, result)
}

// TestCommandHandler_Process_DisplayPlaylist_HoldsPlaybackLockAcrossSyncAndSend
// is the regression test for the "replay scope and kiosk navigation are
// not serialized" hazard: the displayPlaylist path must hold the playback
// coordinator across BOTH the replay-scope sync AND the CDP navigation
// send, so a concurrent display command or playlist-refresher pass cannot
// interleave its own sync+send between them and leave the on-screen
// playlist and the Fetch interception scope disagreeing (see
// offlinecache.KioskReplay.LockPlayback's doc). gomock.InOrder pins the
// exact Lock -> Sync -> Send -> Unlock sequence: a future edit that moves
// the lock acquisition after the sync, releases it before the send, or
// drops it entirely fails here.
func TestCommandHandler_Process_DisplayPlaylist_HoldsPlaybackLockAcrossSyncAndSend(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	playlistURL := "https://example.com/playlist.json"
	mockPlaylist := &dp1.Playlist{Playlist: dp1playlist.Playlist{
		Items: []dp1playlist.PlaylistItem{{ID: "item1", Source: "https://example.com/video.mp4"}},
	}}
	ts.mockDP1.EXPECT().ProcessPlaylistURLForCast(ts.ctx, playlistURL).Return(mockPlaylist, nil).Times(1)
	ts.mockStatusPoller.EXPECT().ForceRefresh().Times(1)

	mockKioskReplay := mocks.NewMockOfflineCacheKioskReplay(ts.ctrl)
	lock := mockKioskReplay.EXPECT().LockPlayback().Times(1)
	sync := mockKioskReplay.EXPECT().SyncPlaylist(ts.ctx, []string{"item1"}).Return(nil).Times(1)
	// MarkPlaybackChanged must be announced UNDER the lock, after the sync
	// and before the unlock, so a concurrent resync defers to this
	// authoritative scope change (see KioskReplay.PlaybackGeneration).
	mark := mockKioskReplay.EXPECT().MarkPlaybackChanged().Times(1)
	send := ts.mockCDP.EXPECT().Send(cdp.METHOD_EVALUATE, gomock.Any()).Return(playerOkResponse(), nil).Times(1)
	unlock := mockKioskReplay.EXPECT().UnlockPlayback().Times(1)
	gomock.InOrder(lock, sync, mark, send, unlock)

	ts.handler = commandrouter.New(ts.mockExecutor, ts.mockCDP, ts.mockDP1, ts.mockStatusPoller, nil, nil, mockKioskReplay, nil, ts.mockJSON, ts.logger)

	result, err := ts.handler.Process(ts.ctx, commands.Command{
		Type:      commands.CMD_DISPLAY_PLAYLIST,
		Arguments: map[string]interface{}{"playlistUrl": playlistURL},
	})

	assert.NoError(t, err)
	assert.NotNil(t, result)
}

func TestCommandHandler_Process_DisplayPlaylist_KioskReplaySyncFailureDoesNotBlockDisplay(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	playlistURL := "https://example.com/playlist.json"
	mockPlaylist := &dp1.Playlist{
		Playlist: dp1playlist.Playlist{
			Items: []dp1playlist.PlaylistItem{{ID: "item1", Source: "https://example.com/video.mp4"}},
		},
	}
	expectDisplayPlaylistSuccess(ts, playlistURL, mockPlaylist)

	mockKioskReplay := mocks.NewMockOfflineCacheKioskReplay(ts.ctrl)
	mockKioskReplay.EXPECT().LockPlayback().AnyTimes()
	mockKioskReplay.EXPECT().UnlockPlayback().AnyTimes()
	mockKioskReplay.EXPECT().PlaybackGeneration().Return(uint64(0)).AnyTimes()
	mockKioskReplay.EXPECT().SyncPlaylist(ts.ctx, []string{"item1"}).Return(errors.New("dial failed")).Times(1)
	// The authoritative generation bump MUST still fire even though the
	// SyncPlaylist above errored: this display path is authoritative for
	// what SHOULD be on screen, so a concurrent corrective resync must
	// defer to it (see KioskReplay.PlaybackGeneration). Pinned to Times(1)
	// — not AnyTimes — so a future change that drops the bump on the sync-
	// error branch fails here instead of silently weakening the TOCTOU
	// guard. The display itself succeeds here, so the failure-path resync
	// (the only other MarkPlaybackChanged-adjacent caller) never runs.
	mockKioskReplay.EXPECT().MarkPlaybackChanged().Times(1)
	ts.handler = commandrouter.New(ts.mockExecutor, ts.mockCDP, ts.mockDP1, ts.mockStatusPoller, nil, nil, mockKioskReplay, nil, ts.mockJSON, ts.logger)

	command := commands.Command{
		Type:      commands.CMD_DISPLAY_PLAYLIST,
		Arguments: map[string]interface{}{"playlistUrl": playlistURL},
	}

	result, err := ts.handler.Process(ts.ctx, command)

	assert.NoError(t, err, "a replay-sync failure must never fail the display command itself")
	assert.NotNil(t, result)
}

// TestCommandHandler_Process_DisplayPlaylist_CDPSendFailureRevertsKioskReplayScope
// is the regression test pinning that SyncPlaylist's pre-CDP-send scope
// switch to the NEW playlist is reverted when the CDP send itself fails:
// the kiosk never actually displayed the new playlist, so replay's scope
// must be re-synced back to whatever the player reports it is still
// showing, rather than being left pointed at a playlist load that never
// happened.
func TestCommandHandler_Process_DisplayPlaylist_CDPSendFailureRevertsKioskReplayScope(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	newURL := "https://example.com/new.json"
	newPlaylist := &dp1.Playlist{Playlist: dp1playlist.Playlist{
		Items: []dp1playlist.PlaylistItem{{ID: "item-new"}},
	}}
	ts.mockDP1.EXPECT().ProcessPlaylistURLForCast(ts.ctx, newURL).Return(newPlaylist, nil).Times(1)

	mockKioskReplay := mocks.NewMockOfflineCacheKioskReplay(ts.ctrl)
	mockKioskReplay.EXPECT().LockPlayback().AnyTimes()
	mockKioskReplay.EXPECT().UnlockPlayback().AnyTimes()
	mockKioskReplay.EXPECT().PlaybackGeneration().Return(uint64(0)).AnyTimes()
	mockKioskReplay.EXPECT().MarkPlaybackChanged().AnyTimes()
	mockKioskReplay.EXPECT().SyncPlaylist(ts.ctx, []string{"item-new"}).Return(nil).Times(1)
	ts.handler = commandrouter.New(ts.mockExecutor, ts.mockCDP, ts.mockDP1, ts.mockStatusPoller, nil, nil, mockKioskReplay, nil, ts.mockJSON, ts.logger)

	ts.mockCDP.EXPECT().
		Send(cdp.METHOD_EVALUATE, gomock.Any()).
		Return(nil, errors.New("cdp send failed")).
		Times(1)

	oldURL := "https://example.com/old.json"
	oldPlaylist := &dp1.Playlist{Playlist: dp1playlist.Playlist{
		Items: []dp1playlist.PlaylistItem{{ID: "item-old"}},
	}}
	ts.mockStatusPoller.EXPECT().FetchPlayerStatus(ts.ctx).Return(&status.PlayerStatus{
		Command:     string(commands.CMD_DISPLAY_PLAYLIST),
		PlaylistURL: &oldURL,
	}, nil).Times(1)
	ts.mockDP1.EXPECT().ProcessPlaylistURL(ts.ctx, oldURL, false).Return(oldPlaylist, nil).Times(1)
	mockKioskReplay.EXPECT().SyncPlaylist(ts.ctx, []string{"item-old"}).Return(nil).Times(1)

	result, err := ts.handler.Process(ts.ctx, commands.Command{
		Type:      commands.CMD_DISPLAY_PLAYLIST,
		Arguments: map[string]interface{}{"playlistUrl": newURL},
	})

	assert.Error(t, err)
	assert.Nil(t, result)
}

// TestCommandHandler_Process_DisplayPlaylist_PlayerRejectionRevertsKioskReplayScope
// mirrors the CDP-send-failure regression above for the other failure
// shape: the CDP send itself succeeds, but the player replies ok:false
// (rejecting the command), which must revert scope the same way.
func TestCommandHandler_Process_DisplayPlaylist_PlayerRejectionRevertsKioskReplayScope(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	newURL := "https://example.com/new.json"
	newPlaylist := &dp1.Playlist{Playlist: dp1playlist.Playlist{
		Items: []dp1playlist.PlaylistItem{{ID: "item-new"}},
	}}
	ts.mockDP1.EXPECT().ProcessPlaylistURLForCast(ts.ctx, newURL).Return(newPlaylist, nil).Times(1)

	mockKioskReplay := mocks.NewMockOfflineCacheKioskReplay(ts.ctrl)
	mockKioskReplay.EXPECT().LockPlayback().AnyTimes()
	mockKioskReplay.EXPECT().UnlockPlayback().AnyTimes()
	mockKioskReplay.EXPECT().PlaybackGeneration().Return(uint64(0)).AnyTimes()
	mockKioskReplay.EXPECT().MarkPlaybackChanged().AnyTimes()
	mockKioskReplay.EXPECT().SyncPlaylist(ts.ctx, []string{"item-new"}).Return(nil).Times(1)
	ts.handler = commandrouter.New(ts.mockExecutor, ts.mockCDP, ts.mockDP1, ts.mockStatusPoller, nil, nil, mockKioskReplay, nil, ts.mockJSON, ts.logger)

	ts.mockCDP.EXPECT().
		Send(cdp.METHOD_EVALUATE, gomock.Any()).
		Return(playerNotOkResponse(), nil).
		Times(1)
	ts.mockStatusPoller.EXPECT().ForceRefresh().Times(1)

	oldURL := "https://example.com/old.json"
	oldPlaylist := &dp1.Playlist{Playlist: dp1playlist.Playlist{
		Items: []dp1playlist.PlaylistItem{{ID: "item-old"}},
	}}
	ts.mockStatusPoller.EXPECT().FetchPlayerStatus(ts.ctx).Return(&status.PlayerStatus{
		Command:     string(commands.CMD_DISPLAY_PLAYLIST),
		PlaylistURL: &oldURL,
	}, nil).Times(1)
	ts.mockDP1.EXPECT().ProcessPlaylistURL(ts.ctx, oldURL, false).Return(oldPlaylist, nil).Times(1)
	mockKioskReplay.EXPECT().SyncPlaylist(ts.ctx, []string{"item-old"}).Return(nil).Times(1)

	result, err := ts.handler.Process(ts.ctx, commands.Command{
		Type:      commands.CMD_DISPLAY_PLAYLIST,
		Arguments: map[string]interface{}{"playlistUrl": newURL},
	})

	assert.NoError(t, err)
	assert.NotNil(t, result)
}

// TestCommandHandler_Process_DisplayPlaylist_FallsBackToCachedPlaylistWhenOffline
// is the regression test pinning that displayPlaylist with playlistUrl
// must be able to use the downloaded cache when offline: a playlist
// previously downloaded via downloadPlaylist for this exact URL must
// still be displayable when live DP-1 resolution fails.
func TestCommandHandler_Process_DisplayPlaylist_FallsBackToCachedPlaylistWhenOffline(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	playlistURL := "https://example.com/playlist.json"
	cachedRawBytes := []byte(`{"id":"playlist-1","items":[{"id":"item1","source":"https://example.com/video.mp4"}]}`)
	cachedPlaylist := &dp1.Playlist{Playlist: dp1playlist.Playlist{
		ID:    "playlist-1",
		Items: []dp1playlist.PlaylistItem{{ID: "item1", Source: "https://example.com/video.mp4"}},
	}}

	mockOfflineCache := mocks.NewMockOfflineCacheService(ts.ctrl)
	ts.handler = commandrouter.New(ts.mockExecutor, ts.mockCDP, ts.mockDP1, ts.mockStatusPoller, nil, mockOfflineCache, nil, nil, ts.mockJSON, ts.logger)

	ts.mockDP1.EXPECT().
		ProcessPlaylistURLForCast(ts.ctx, playlistURL).
		Return(nil, errors.New("network unreachable")).
		Times(1)

	mockOfflineCache.EXPECT().CachedPlaylistForURL(playlistURL).Return(json.RawMessage(cachedRawBytes), nil).Times(1)
	ts.mockJSON.EXPECT().
		Unmarshal(cachedRawBytes, gomock.Any()).
		DoAndReturn(func(_ []byte, v interface{}) error {
			p := v.(**dp1.Playlist)
			*p = cachedPlaylist
			return nil
		}).
		Times(1)

	ts.mockCDP.EXPECT().
		Send(cdp.METHOD_EVALUATE, gomock.Any()).
		Return(playerOkResponse(), nil).
		Times(1)

	ts.mockStatusPoller.EXPECT().ForceRefresh().Times(1)

	result, err := ts.handler.Process(ts.ctx, commands.Command{
		Type:      commands.CMD_DISPLAY_PLAYLIST,
		Arguments: map[string]interface{}{"playlistUrl": playlistURL},
	})

	assert.NoError(t, err)
	assert.NotNil(t, result)
}

// TestCommandHandler_Process_DisplayPlaylist_ReturnsOriginalErrorWhenNoCachedFallback
// pins that the original live-resolution error is what gets reported when
// there is nothing to fall back to (offline caching disabled here), not a
// confusing "cache lookup failed" error about a fallback the caller never
// asked for.
func TestCommandHandler_Process_DisplayPlaylist_ReturnsOriginalErrorWhenNoCachedFallback(t *testing.T) {
	ts := setup(t) // setup() wires offlineCache as nil
	defer ts.teardown()

	playlistURL := "https://example.com/playlist.json"
	liveErr := errors.New("network unreachable")
	ts.mockDP1.EXPECT().
		ProcessPlaylistURLForCast(ts.ctx, playlistURL).
		Return(nil, liveErr).
		Times(1)

	result, err := ts.handler.Process(ts.ctx, commands.Command{
		Type:      commands.CMD_DISPLAY_PLAYLIST,
		Arguments: map[string]interface{}{"playlistUrl": playlistURL},
	})

	assert.ErrorIs(t, err, liveErr)
	assert.Nil(t, result)
}

func TestCommandHandler_Process_DisplayPlaylist_WithPlaylistObject(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	playlistMap := map[string]interface{}{
		"items": []interface{}{
			map[string]interface{}{
				"id":       "item1",
				"title":    "Test Item",
				"source":   "https://example.com/video.mp4",
				"duration": 300,
				"license":  "open",
			},
		},
	}
	playlistBytes := []byte(`{"items":[{"id":"item1","title":"Test Item"}]}`)
	mockPlaylist := &dp1.Playlist{
		Playlist: dp1playlist.Playlist{
			Items: []dp1playlist.PlaylistItem{
				{
					ID:       "item1",
					Title:    "Test Item",
					Source:   "https://example.com/video.mp4",
					Duration: float64Ptr(300),
					License:  "open",
				},
			},
		},
	}
	cdpResult := playerOkResponse()

	command := commands.Command{
		Type: commands.CMD_DISPLAY_PLAYLIST,
		Arguments: map[string]interface{}{
			"dp1_call": playlistMap,
		},
	}

	ts.mockJSON.EXPECT().
		Marshal(playlistMap).
		Return(playlistBytes, nil).
		Times(1)

	ts.mockJSON.EXPECT().
		Unmarshal(playlistBytes, gomock.Any()).
		DoAndReturn(func(data []byte, v interface{}) error {
			playlist := v.(**dp1.Playlist)
			*playlist = mockPlaylist
			return nil
		}).
		Times(1)

	ts.mockCDP.EXPECT().
		Send(cdp.METHOD_EVALUATE, gomock.Any()).
		Return(cdpResult, nil).
		Times(1)

	ts.mockStatusPoller.EXPECT().
		ForceRefresh().
		Times(1)

	result, err := ts.handler.Process(ts.ctx, command)

	assert.NoError(t, err)
	assert.Equal(t, cdpResult, result)
}

func TestCommandHandler_Process_RefreshArtwork(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	command := commands.Command{
		Type:      commands.CMD_REFRESH_ARTWORK,
		Arguments: map[string]interface{}{},
	}

	ts.mockCDP.EXPECT().
		Send("Network.clearBrowserCache", map[string]interface{}{}).
		Return(nil, nil).
		Times(1)

	ts.mockCDP.EXPECT().
		Send(cdp.METHOD_EVALUATE, gomock.Any()).
		Return(playerOkResponse(), nil).
		Times(1)

	ts.mockStatusPoller.EXPECT().
		ForceRefresh().
		Times(1)

	result, err := ts.handler.Process(ts.ctx, command)

	assert.NoError(t, err)
	assert.NotNil(t, result)
}

func TestCommandHandler_Process_DisplayPlaylist_WithDynamicQueries(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	playlistMap := map[string]interface{}{
		"items": []interface{}{},
		"dynamicQueries": []interface{}{
			map[string]interface{}{
				"endpoint": "https://api.example.com/graphql",
				"params": map[string]interface{}{
					"query": "test query",
				},
			},
		},
	}
	playlistBytes := []byte(`{"items":[],"dynamicQueries":[]}`)
	mockPlaylist := &dp1.Playlist{
		Playlist: dp1playlist.Playlist{
			Items: []dp1playlist.PlaylistItem{},
		},
		DynamicQueries: []dp1.LegacyDynamicQuery{
			{
				Endpoint: "https://api.example.com/graphql",
				Params: map[string]string{
					"query": "test query",
				},
			},
		},
	}
	processedPlaylist := &dp1.Playlist{
		Playlist: dp1playlist.Playlist{
			Items: []dp1playlist.PlaylistItem{
				{
					ID:       "item1",
					Source:   "https://example.com/video.mp4",
					Duration: float64Ptr(300),
				},
			},
		},
	}
	cdpResult := playerOkResponse()

	command := commands.Command{
		Type: commands.CMD_DISPLAY_PLAYLIST,
		Arguments: map[string]interface{}{
			"dp1_call": playlistMap,
		},
	}

	ts.mockJSON.EXPECT().
		Marshal(playlistMap).
		Return(playlistBytes, nil).
		Times(1)

	ts.mockJSON.EXPECT().
		Unmarshal(playlistBytes, gomock.Any()).
		DoAndReturn(func(data []byte, v interface{}) error {
			playlist := v.(**dp1.Playlist)
			*playlist = mockPlaylist
			return nil
		}).
		Times(1)

	ts.mockDP1.EXPECT().
		ProcessDynamicPlaylistForCast(ts.ctx, *mockPlaylist).
		Return(processedPlaylist, nil).
		Times(1)

	ts.mockCDP.EXPECT().
		Send(cdp.METHOD_EVALUATE, gomock.Any()).
		Return(cdpResult, nil).
		Times(1)

	ts.mockStatusPoller.EXPECT().
		ForceRefresh().
		Times(1)

	result, err := ts.handler.Process(ts.ctx, command)

	assert.NoError(t, err)
	assert.Equal(t, cdpResult, result)
}

// TestCommandHandler_Process_DisplayPlaylist_WithSpecDynamicQuery ensures dp1_call with only
// the DP-1 playlists extension dynamicQuery (no legacy dynamicQueries) still triggers
// ProcessDynamicPlaylist via HasDynamicContent().
func TestCommandHandler_Process_DisplayPlaylist_WithSpecDynamicQuery(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	playlistMap := map[string]interface{}{
		"items": []interface{}{},
		"dynamicQuery": map[string]interface{}{
			"profile":  "graphql-v1",
			"endpoint": "https://api.example.com/graphql",
			"query":    `query { items(limit: {{limit}}, offset: {{offset}}) { id title source } }`,
			"responseMapping": map[string]interface{}{
				"itemsPath":  "data.items",
				"itemSchema": "dp1/1.0",
			},
		},
	}
	playlistBytes := []byte(`{"items":[]}`)
	mockPlaylist := &dp1.Playlist{
		Playlist: dp1playlist.Playlist{
			Items: []dp1playlist.PlaylistItem{},
			DynamicQuery: &playlists.DynamicQuery{
				Profile:  dp1playlist.ProfileGraphQLV1,
				Endpoint: "https://api.example.com/graphql",
				Query:    `query { items(limit: {{limit}}, offset: {{offset}}) { id title source } }`,
				ResponseMapping: playlists.ResponseMapping{
					ItemsPath:  "data.items",
					ItemSchema: "dp1/1.0",
				},
			},
		},
	}
	assert.True(t, mockPlaylist.HasDynamicContent(), "fixture should be spec-only dynamic (no legacy dynamicQueries)")

	processedPlaylist := &dp1.Playlist{
		Playlist: dp1playlist.Playlist{
			Items: []dp1playlist.PlaylistItem{
				{
					ID:       "item1",
					Source:   "https://example.com/video.mp4",
					Duration: float64Ptr(300),
				},
			},
		},
	}
	cdpResult := playerOkResponse()

	command := commands.Command{
		Type: commands.CMD_DISPLAY_PLAYLIST,
		Arguments: map[string]interface{}{
			"dp1_call": playlistMap,
		},
	}

	ts.mockJSON.EXPECT().
		Marshal(playlistMap).
		Return(playlistBytes, nil).
		Times(1)

	ts.mockJSON.EXPECT().
		Unmarshal(playlistBytes, gomock.Any()).
		DoAndReturn(func(data []byte, v interface{}) error {
			playlist := v.(**dp1.Playlist)
			*playlist = mockPlaylist
			return nil
		}).
		Times(1)

	ts.mockDP1.EXPECT().
		ProcessDynamicPlaylistForCast(ts.ctx, *mockPlaylist).
		Return(processedPlaylist, nil).
		Times(1)

	ts.mockCDP.EXPECT().
		Send(cdp.METHOD_EVALUATE, gomock.Any()).
		Return(cdpResult, nil).
		Times(1)

	ts.mockStatusPoller.EXPECT().
		ForceRefresh().
		Times(1)

	result, err := ts.handler.Process(ts.ctx, command)

	assert.NoError(t, err)
	assert.Equal(t, cdpResult, result)
}

func TestCommandHandler_Process_DisplayPlaylist_Errors(t *testing.T) {
	tests := []struct {
		name          string
		setupFunc     func(*testSetup) commands.Command
		expectedError string
	}{
		{
			name: "invalid playlistUrl type",
			setupFunc: func(ts *testSetup) commands.Command {
				cmd := commands.CMD_DISPLAY_PLAYLIST
				return commands.Command{
					Type: cmd,
					Arguments: map[string]interface{}{
						"playlistUrl": 123, // Invalid type
					},
				}
			},
			expectedError: "playlistUrl is not a string or empty",
		},
		{
			name: "empty playlistUrl",
			setupFunc: func(ts *testSetup) commands.Command {
				cmd := commands.CMD_DISPLAY_PLAYLIST
				return commands.Command{
					Type: cmd,
					Arguments: map[string]interface{}{
						"playlistUrl": "",
					},
				}
			},
			expectedError: "playlistUrl is not a string or empty",
		},
		{
			name: "invalid playlist type",
			setupFunc: func(ts *testSetup) commands.Command {
				cmd := commands.CMD_DISPLAY_PLAYLIST
				return commands.Command{
					Type: cmd,
					Arguments: map[string]interface{}{
						"dp1_call": "not a map", // Invalid type
					},
				}
			},
			expectedError: "playlist is not a map",
		},
		{
			name: "unknown payload type",
			setupFunc: func(ts *testSetup) commands.Command {
				cmd := commands.CMD_DISPLAY_PLAYLIST
				return commands.Command{
					Type:      cmd,
					Arguments: map[string]interface{}{}, // Neither playlistUrl nor dp1_call
				}
			},
			expectedError: "unknown payload type",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts := setup(t)
			defer ts.teardown()

			payload := tt.setupFunc(ts)
			result, err := ts.handler.Process(ts.ctx, payload)

			assert.Error(t, err)
			assert.Contains(t, err.Error(), tt.expectedError)
			assert.Nil(t, result)
		})
	}
}

func TestCommandHandler_Process_NonControldCommand(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	cmd := commands.Type("someCustomCommand")
	args := map[string]interface{}{"key": "value"}
	cdpResult := map[string]interface{}{"result": "success"}

	payload := commands.Command{
		Type:      cmd,
		Arguments: args,
	}

	ts.mockCDP.EXPECT().
		Send(cdp.METHOD_EVALUATE, gomock.Any()).
		Return(cdpResult, nil).
		Times(1)

	ts.mockStatusPoller.EXPECT().
		ForceRefresh().
		Times(1)

	result, err := ts.handler.Process(ts.ctx, payload)

	assert.NoError(t, err)
	assert.Equal(t, cdpResult, result)
}

// TestCommandHandler_SendCDPRequest_GenerationStable_ReturnsReply pins the
// baseline: SetSessionGeneration wired but the generation does not move
// across the send, so the reply passes through exactly as before the
// re-check existed.
func TestCommandHandler_SendCDPRequest_GenerationStable_ReturnsReply(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()
	commandrouter.SetSessionGeneration(ts.handler, func() uint64 { return 3 }, ts.logger)

	cdpResult := map[string]interface{}{"result": "success"}
	ts.mockCDP.EXPECT().
		Send(cdp.METHOD_EVALUATE, gomock.Any()).
		Return(cdpResult, nil).
		Times(1)
	ts.mockStatusPoller.EXPECT().ForceRefresh().Times(1)

	result, err := ts.handler.Process(ts.ctx, commands.Command{
		Type:      commands.Type("someCustomCommand"),
		Arguments: map[string]interface{}{"key": "value"},
	})

	assert.NoError(t, err)
	assert.Equal(t, cdpResult, result)
}

// TestCommandHandler_SendCDPRequest_GenerationMovedDuringSend_LoudError pins
// design doc §2.4: unlike devicectl's sleep apply, a command reply answered
// while the page generation moved must surface loudly to the relayer/hub
// caller instead of reporting the reply as delivered — a cast or control
// command silently landing on a torn-down page must not read as success.
func TestCommandHandler_SendCDPRequest_GenerationMovedDuringSend_LoudError(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()
	gen := uint64(1)
	commandrouter.SetSessionGeneration(ts.handler, func() uint64 { return gen }, ts.logger)

	ts.mockCDP.EXPECT().
		Send(cdp.METHOD_EVALUATE, gomock.Any()).
		DoAndReturn(func(string, map[string]interface{}) (interface{}, error) {
			gen = 2 // the page navigated away while the send was in flight
			return map[string]interface{}{"result": "success"}, nil
		}).
		Times(1)
	// No ForceRefresh: Process returns the error before reaching it.

	result, err := ts.handler.Process(ts.ctx, commands.Command{
		Type:      commands.Type("someCustomCommand"),
		Arguments: map[string]interface{}{"key": "value"},
	})

	require.Error(t, err)
	assert.Nil(t, result)
	assert.Contains(t, err.Error(), "generation")
	assert.ErrorIs(t, err, commandrouter.ErrGenerationRace)
}

// TestCommandHandler_Process_RefreshArtwork_GenerationRace_NoEscalation pins
// M5: a generation-race failure on refreshArtwork's own send must NOT
// escalate into NavigateHomeInline — the send itself succeeded and merely
// raced an UNRELATED page-generation change (a connectivity reconciler
// bump, a stamp-mismatch bump, ...), which is not evidence the page is
// broken. Escalating it would visibly restart a healthy page for no reason;
// the ErrGenerationRace error must instead reach the relayer so the caller
// retries.
func TestCommandHandler_Process_RefreshArtwork_GenerationRace_NoEscalation(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()
	sess := &fakeRecoverySession{}
	commandrouter.SetRecoverySession(ts.handler, sess, ts.logger)
	gen := uint64(1)
	commandrouter.SetSessionGeneration(ts.handler, func() uint64 { return gen }, ts.logger)

	command := commands.Command{
		Type:      commands.CMD_REFRESH_ARTWORK,
		Arguments: map[string]interface{}{},
	}

	ts.mockCDP.EXPECT().
		Send("Network.clearBrowserCache", map[string]interface{}{}).
		Return(nil, nil).
		Times(1)

	// The evaluate itself SUCCEEDS, but the generation moves while it is in
	// flight — a healthy page racing an unrelated bump, not a dead page.
	ts.mockCDP.EXPECT().
		Send(cdp.METHOD_EVALUATE, gomock.Any()).
		DoAndReturn(func(string, map[string]interface{}) (interface{}, error) {
			gen = 2
			return map[string]interface{}{"message": map[string]interface{}{"ok": true}}, nil
		}).
		Times(1)
	// No ForceRefresh: Process returns the error before reaching it.

	result, err := ts.handler.Process(ts.ctx, command)

	require.Error(t, err)
	assert.Nil(t, result)
	assert.ErrorIs(t, err, commandrouter.ErrGenerationRace)
	assert.Equal(t, 0, sess.calls, "a generation-race failure must never escalate to NavigateHomeInline")
}

// TestSetSessionGeneration_GatedHandlerIsNoOp guards the documented caveat:
// wiring it against the storm-protection gate (not the raw handler
// commandrouter.New returns) must degrade to a harmless no-op, never a panic,
// since the gate wrapper does not expose the private seam.
func TestSetSessionGeneration_GatedHandlerIsNoOp(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()
	gated := commandrouter.NewGate(ts.handler, commandrouter.GateConfig{Enabled: true, MaxConcurrent: 1}, ts.logger)
	commandrouter.SetSessionGeneration(gated, func() uint64 { return 9 }, ts.logger)
}

// --- Playback metrics tests ---

func TestCommandHandler_Metrics_DisplayPlaylist_Success(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	playlistURL := "https://example.com/playlist.json"
	mockPlaylist := &dp1.Playlist{
		Playlist: dp1playlist.Playlist{
			Items: []dp1playlist.PlaylistItem{{ID: "item1"}},
		},
	}

	beforeAttempts := status.PlaybackStartTotal()
	beforeFailures := status.PlaybackStartFailures()

	expectDisplayPlaylistSuccess(ts, playlistURL, mockPlaylist)

	_, err := ts.handler.Process(ts.ctx, commands.Command{
		Type:      commands.CMD_DISPLAY_PLAYLIST,
		Arguments: map[string]interface{}{"playlistUrl": playlistURL},
	})

	assert.NoError(t, err)
	assert.Equal(t, beforeAttempts+1, status.PlaybackStartTotal(), "attempt counter should increment")
	assert.Equal(t, beforeFailures, status.PlaybackStartFailures(), "failure counter should not increment on success")
}

func TestCommandHandler_Metrics_DisplayPlaylist_ControldError(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	beforeAttempts := status.PlaybackStartTotal()
	beforeFailures := status.PlaybackStartFailures()

	_, err := ts.handler.Process(ts.ctx, commands.Command{
		Type:      commands.CMD_DISPLAY_PLAYLIST,
		Arguments: map[string]interface{}{}, // neither playlistUrl nor dp1_call
	})

	assert.Error(t, err)
	assert.Equal(t, beforeAttempts+1, status.PlaybackStartTotal())
	assert.Equal(t, beforeFailures+1, status.PlaybackStartFailures(), "failure should be recorded for controld-side error")
}

func TestCommandHandler_Metrics_DisplayPlaylist_CDPFailure(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	playlistURL := "https://example.com/playlist.json"
	mockPlaylist := &dp1.Playlist{
		Playlist: dp1playlist.Playlist{
			Items: []dp1playlist.PlaylistItem{{ID: "item1"}},
		},
	}

	beforeAttempts := status.PlaybackStartTotal()
	beforeFailures := status.PlaybackStartFailures()

	ts.mockDP1.EXPECT().
		ProcessPlaylistURLForCast(ts.ctx, playlistURL).
		Return(mockPlaylist, nil).
		Times(1)

	ts.mockCDP.EXPECT().
		Send(cdp.METHOD_EVALUATE, gomock.Any()).
		Return(nil, errors.New("CDP write error")).
		Times(1)

	_, err := ts.handler.Process(ts.ctx, commands.Command{
		Type:      commands.CMD_DISPLAY_PLAYLIST,
		Arguments: map[string]interface{}{"playlistUrl": playlistURL},
	})

	assert.Error(t, err)
	assert.Equal(t, beforeAttempts+1, status.PlaybackStartTotal())
	assert.Equal(t, beforeFailures+1, status.PlaybackStartFailures())
}

func TestCommandHandler_Metrics_PlayerResponseNotOk(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	playlistURL := "https://example.com/playlist.json"
	mockPlaylist := &dp1.Playlist{
		Playlist: dp1playlist.Playlist{
			Items: []dp1playlist.PlaylistItem{{ID: "item1"}},
		},
	}

	beforeAttempts := status.PlaybackStartTotal()
	beforeFailures := status.PlaybackStartFailures()

	ts.mockDP1.EXPECT().
		ProcessPlaylistURLForCast(ts.ctx, playlistURL).
		Return(mockPlaylist, nil).
		Times(1)

	ts.mockCDP.EXPECT().
		Send(cdp.METHOD_EVALUATE, gomock.Any()).
		Return(playerNotOkResponse(), nil).
		Times(1)

	ts.mockStatusPoller.EXPECT().
		ForceRefresh().
		Times(1)

	_, err := ts.handler.Process(ts.ctx, commands.Command{
		Type:      commands.CMD_DISPLAY_PLAYLIST,
		Arguments: map[string]interface{}{"playlistUrl": playlistURL},
	})

	assert.NoError(t, err, "Process itself succeeds; failure is only in the metric")
	assert.Equal(t, beforeAttempts+1, status.PlaybackStartTotal())
	assert.Equal(t, beforeFailures+1, status.PlaybackStartFailures(), "failure should be recorded when player responds with ok: false")
}

func TestCommandHandler_Metrics_PlayerResponseMissingMessage(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	playlistURL := "https://example.com/playlist.json"
	mockPlaylist := &dp1.Playlist{
		Playlist: dp1playlist.Playlist{
			Items: []dp1playlist.PlaylistItem{{ID: "item1"}},
		},
	}

	beforeAttempts := status.PlaybackStartTotal()
	beforeFailures := status.PlaybackStartFailures()

	ts.mockDP1.EXPECT().
		ProcessPlaylistURLForCast(ts.ctx, playlistURL).
		Return(mockPlaylist, nil).
		Times(1)

	ts.mockCDP.EXPECT().
		Send(cdp.METHOD_EVALUATE, gomock.Any()).
		Return(map[string]interface{}{"unexpected": "shape"}, nil).
		Times(1)

	ts.mockStatusPoller.EXPECT().
		ForceRefresh().
		Times(1)

	_, err := ts.handler.Process(ts.ctx, commands.Command{
		Type:      commands.CMD_DISPLAY_PLAYLIST,
		Arguments: map[string]interface{}{"playlistUrl": playlistURL},
	})

	assert.NoError(t, err)
	assert.Equal(t, beforeAttempts+1, status.PlaybackStartTotal())
	assert.Equal(t, beforeFailures+1, status.PlaybackStartFailures(), "failure should be recorded when response has no message.ok")
}

func TestCommandHandler_Metrics_DisplayDefaultPlaylist_NoMetrics(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	cdpResult := map[string]interface{}{"result": "success"}

	beforeAttempts := status.PlaybackStartTotal()
	beforeFailures := status.PlaybackStartFailures()

	ts.mockCDP.EXPECT().
		Send(cdp.METHOD_EVALUATE, gomock.Any()).
		Return(cdpResult, nil).
		Times(1)

	ts.mockStatusPoller.EXPECT().
		ForceRefresh().
		Times(1)

	_, err := ts.handler.Process(ts.ctx, commands.Command{
		Type:      commands.CMD_DISPLAY_DEFAULT_PLAYLIST,
		Arguments: map[string]interface{}{},
	})

	assert.NoError(t, err)
	assert.Equal(t, beforeAttempts, status.PlaybackStartTotal(), "displayDefaultPlaylist should not record metrics")
	assert.Equal(t, beforeFailures, status.PlaybackStartFailures(), "displayDefaultPlaylist should not record metrics")
}

func TestCommandHandler_Metrics_NonPlaybackCommand_NoMetrics(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	beforeAttempts := status.PlaybackStartTotal()

	ts.mockExecutor.EXPECT().
		Execute(ts.ctx, gomock.Any()).
		Return(nil, errors.New("some error")).
		Times(1)

	_, err := ts.handler.Process(ts.ctx, commands.Command{
		Type:      commands.CMD_SHUTDOWN,
		Arguments: map[string]interface{}{},
	})

	assert.Error(t, err)
	assert.Equal(t, beforeAttempts, status.PlaybackStartTotal(), "non-playback command should not increment attempt counter")
}

// refreshArtwork must survive a dead player page: the evaluate path needs
// window.handleCDPRequest, which is exactly what's missing when Chromium is
// serving a stale/broken bundle (#234) — the situation a refresh exists to
// fix. Cache clear + a session NavigateHomeInline (navigate-to-entry, never
// reload-in-place — design doc §5) is the recovery.
func TestCommandHandler_Process_RefreshArtwork_DeadPageRecoversViaNavigate(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()
	sess := &fakeRecoverySession{}
	commandrouter.SetRecoverySession(ts.handler, sess, ts.logger)

	command := commands.Command{
		Type:      commands.CMD_REFRESH_ARTWORK,
		Arguments: map[string]interface{}{},
	}

	ts.mockCDP.EXPECT().
		Send("Network.clearBrowserCache", map[string]interface{}{}).
		Return(nil, nil).
		Times(1)

	// Page evaluate fails — no live player app.
	ts.mockCDP.EXPECT().
		Send(cdp.METHOD_EVALUATE, gomock.Any()).
		Return(nil, errors.New("evaluate failed: handleCDPRequest is not defined")).
		Times(1)

	ts.mockStatusPoller.EXPECT().
		ForceRefresh().
		Times(1)

	result, err := ts.handler.Process(ts.ctx, command)

	assert.NoError(t, err)
	assert.True(t, isPlayerResponseOkForTest(result))
	assert.Equal(t, 1, sess.calls, "the dead evaluate must escalate to exactly one NavigateHomeInline")
	assert.True(t, sess.opts.PurgeCache, "the recovery navigation must purge the cache")
}

// When both the evaluate and the navigate escalation fail, the command must
// report the original failure — a dead CDP connection is not recoverable
// here.
func TestCommandHandler_Process_RefreshArtwork_NavigateAlsoFails(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()
	sess := &fakeRecoverySession{err: errors.New("no CDP connection")}
	commandrouter.SetRecoverySession(ts.handler, sess, ts.logger)

	command := commands.Command{
		Type:      commands.CMD_REFRESH_ARTWORK,
		Arguments: map[string]interface{}{},
	}

	ts.mockCDP.EXPECT().
		Send("Network.clearBrowserCache", map[string]interface{}{}).
		Return(nil, nil).
		Times(1)

	evalErr := errors.New("evaluate failed")
	ts.mockCDP.EXPECT().
		Send(cdp.METHOD_EVALUATE, gomock.Any()).
		Return(nil, evalErr).
		Times(1)

	result, err := ts.handler.Process(ts.ctx, command)

	assert.Error(t, err)
	assert.Equal(t, evalErr, err)
	assert.Nil(t, result)
	assert.Equal(t, 1, sess.calls)
}

// With no session wired (a build wired before Phase 2b, or a test double),
// the escalation must degrade to the original failure rather than panicking.
func TestCommandHandler_Process_RefreshArtwork_NoSessionWiredReportsOriginalFailure(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	command := commands.Command{
		Type:      commands.CMD_REFRESH_ARTWORK,
		Arguments: map[string]interface{}{},
	}

	ts.mockCDP.EXPECT().
		Send("Network.clearBrowserCache", map[string]interface{}{}).
		Return(nil, nil).
		Times(1)

	evalErr := errors.New("evaluate failed")
	ts.mockCDP.EXPECT().
		Send(cdp.METHOD_EVALUATE, gomock.Any()).
		Return(nil, evalErr).
		Times(1)

	result, err := ts.handler.Process(ts.ctx, command)

	assert.Error(t, err)
	assert.Equal(t, evalErr, err)
	assert.Nil(t, result)
}

// Non-refresh commands must NOT get the reload fallback — a failed
// displayPlaylist evaluate is a real failure the caller needs to see.
func isPlayerResponseOkForTest(result interface{}) bool {
	m, ok := result.(map[string]interface{})
	if !ok {
		return false
	}
	msg, ok := m["message"].(map[string]interface{})
	if !ok {
		return false
	}
	okVal, _ := msg["ok"].(bool)
	return okVal
}
