package oomrecovery_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"

	"github.com/feral-file/ffos-user/components/feral-controld/commands"
	constants "github.com/feral-file/ffos-user/components/feral-controld/constant"
	"github.com/feral-file/ffos-user/components/feral-controld/mocks"
	oomrecovery "github.com/feral-file/ffos-user/components/feral-controld/oom-recovery"
	"github.com/feral-file/ffos-user/components/feral-controld/status"
)

// setupCountFiles creates temporary counter files and patches the constants
// so the recoverer reads from them. Returns a cleanup function.
func setupCountFiles(t *testing.T, oomKillCount, handledCount int) func() {
	t.Helper()
	dir := t.TempDir()

	killFile := filepath.Join(dir, "chromium-oom-kill-count")
	handledFile := filepath.Join(dir, "chromium-oom-kill-handled-count")

	require.NoError(t, os.WriteFile(killFile, []byte(fmt.Sprintf("%d\n", oomKillCount)), 0600))
	require.NoError(t, os.WriteFile(handledFile, []byte(fmt.Sprintf("%d\n", handledCount)), 0600))

	origKill := constants.CHROMIUM_OOM_KILL_COUNT_FILE
	origHandled := constants.CHROMIUM_OOM_KILL_HANDLED_COUNT_FILE
	constants.CHROMIUM_OOM_KILL_COUNT_FILE = killFile
	constants.CHROMIUM_OOM_KILL_HANDLED_COUNT_FILE = handledFile

	return func() {
		constants.CHROMIUM_OOM_KILL_COUNT_FILE = origKill
		constants.CHROMIUM_OOM_KILL_HANDLED_COUNT_FILE = origHandled
	}
}

// waitDone blocks until the recovery goroutine has fully exited. Every test
// that starts a recovery must call this before returning: the recoverer logs
// through the test-scoped zaptest logger, and a log line emitted after the
// test function returns panics the whole package
// ("Log in goroutine after Test... has completed"). Once it returns, the
// handled-count file and mock calls are settled, so assertions after it
// can be plain reads rather than polls.
func waitDone(t *testing.T, r oomrecovery.Recoverer) {
	t.Helper()
	select {
	case <-r.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("recovery goroutine did not finish in time")
	}
}

func TestOOMRecovery_NoOOMEvent(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	cleanup := setupCountFiles(t, 3, 3)
	defer cleanup()

	mockStatusPoller := mocks.NewMockStatusPoller(ctrl)
	mockCmdHandler := mocks.NewMockCommandHandler(ctrl)

	r := oomrecovery.NewWithOptions(
		mockStatusPoller,
		mockCmdHandler,
		20*time.Millisecond,
		oomrecovery.MaxRetries,
		zaptest.NewLogger(t),
	)

	// No expectations on poller/handler — Start should return without doing anything.
	r.Start(context.Background())

	// Done must already be closed: no goroutine was launched, so nothing
	// is left to wait for.
	select {
	case <-r.Done():
	default:
		t.Fatal("Done should be closed synchronously when there is nothing to recover")
	}
}

func TestOOMRecovery_ImmediateSuccess(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	cleanup := setupCountFiles(t, 4, 1)
	defer cleanup()

	mockStatusPoller := mocks.NewMockStatusPoller(ctrl)
	mockCmdHandler := mocks.NewMockCommandHandler(ctrl)

	r := oomrecovery.NewWithOptions(
		mockStatusPoller,
		mockCmdHandler,
		20*time.Millisecond,
		oomrecovery.MaxRetries,
		zaptest.NewLogger(t),
	)

	mockStatusPoller.EXPECT().SuppressPlayerNotifications(true).Times(1)
	mockStatusPoller.EXPECT().SuppressPlayerNotifications(false).Times(1)

	// Player is already responsive on the immediate first attempt.
	mockStatusPoller.EXPECT().
		FetchPlayerStatus(gomock.Any()).
		Return(&status.PlayerStatus{Command: string(commands.CMD_DISPLAY_PLAYLIST)}, nil).
		Times(1)

	mockCmdHandler.EXPECT().
		Process(gomock.Any(), commands.Command{
			Type:      commands.CMD_DISPLAY_DEFAULT_PLAYLIST,
			Arguments: map[string]any{},
		}).
		Return(nil, nil).
		Times(1)

	r.Start(context.Background())

	waitDone(t, r)

	data, err := os.ReadFile(constants.CHROMIUM_OOM_KILL_HANDLED_COUNT_FILE)
	require.NoError(t, err)
	require.Equal(t, "4\n", string(data), "handled count file should be updated immediately")
}

func TestOOMRecovery_WaitsForPlayerThenSendsCommand(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	cleanup := setupCountFiles(t, 5, 2)
	defer cleanup()

	mockStatusPoller := mocks.NewMockStatusPoller(ctrl)
	mockCmdHandler := mocks.NewMockCommandHandler(ctrl)

	r := oomrecovery.NewWithOptions(
		mockStatusPoller,
		mockCmdHandler,
		20*time.Millisecond,
		oomrecovery.MaxRetries,
		zaptest.NewLogger(t),
	)

	mockStatusPoller.EXPECT().SuppressPlayerNotifications(true).Times(1)
	mockStatusPoller.EXPECT().SuppressPlayerNotifications(false).Times(1)

	// Immediate attempt: player not ready.
	mockStatusPoller.EXPECT().
		FetchPlayerStatus(gomock.Any()).
		Return(nil, nil).
		Times(1)
	// First ticker attempt: player ready.
	mockStatusPoller.EXPECT().
		FetchPlayerStatus(gomock.Any()).
		Return(&status.PlayerStatus{Command: string(commands.CMD_DISPLAY_PLAYLIST)}, nil).
		Times(1)

	mockCmdHandler.EXPECT().
		Process(gomock.Any(), commands.Command{
			Type:      commands.CMD_DISPLAY_DEFAULT_PLAYLIST,
			Arguments: map[string]any{},
		}).
		Return(nil, nil).
		Times(1)

	r.Start(context.Background())

	waitDone(t, r)

	data, err := os.ReadFile(constants.CHROMIUM_OOM_KILL_HANDLED_COUNT_FILE)
	require.NoError(t, err)
	require.Equal(t, "5\n", string(data), "handled count file should be updated to oom kill count")
}

func TestOOMRecovery_MaxRetries(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	const maxRetries = 3

	cleanup := setupCountFiles(t, 7, 0)
	defer cleanup()

	mockStatusPoller := mocks.NewMockStatusPoller(ctrl)
	mockCmdHandler := mocks.NewMockCommandHandler(ctrl)

	r := oomrecovery.NewWithOptions(
		mockStatusPoller,
		mockCmdHandler,
		20*time.Millisecond,
		maxRetries,
		zaptest.NewLogger(t),
	)

	mockStatusPoller.EXPECT().SuppressPlayerNotifications(true).Times(1)
	mockStatusPoller.EXPECT().SuppressPlayerNotifications(false).Times(1)

	mockStatusPoller.EXPECT().
		FetchPlayerStatus(gomock.Any()).
		Return(nil, nil).
		Times(maxRetries)

	r.Start(context.Background())

	waitDone(t, r)

	data, err := os.ReadFile(constants.CHROMIUM_OOM_KILL_HANDLED_COUNT_FILE)
	require.NoError(t, err)
	require.Equal(t, "7\n", string(data), "handled count file should be updated even on max retries")
}

func TestOOMRecovery_ContextCancellation(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	cleanup := setupCountFiles(t, 2, 0)
	defer cleanup()

	mockStatusPoller := mocks.NewMockStatusPoller(ctrl)
	mockCmdHandler := mocks.NewMockCommandHandler(ctrl)

	r := oomrecovery.NewWithOptions(
		mockStatusPoller,
		mockCmdHandler,
		20*time.Millisecond,
		oomrecovery.MaxRetries,
		zaptest.NewLogger(t),
	)

	ctx, cancel := context.WithCancel(context.Background())

	mockStatusPoller.EXPECT().SuppressPlayerNotifications(true).Times(1)
	mockStatusPoller.EXPECT().SuppressPlayerNotifications(false).Times(1)
	mockStatusPoller.EXPECT().
		FetchPlayerStatus(gomock.Any()).
		Return(nil, nil).
		AnyTimes()

	r.Start(ctx)

	time.AfterFunc(50*time.Millisecond, cancel)

	waitDone(t, r)

	data, err := os.ReadFile(constants.CHROMIUM_OOM_KILL_HANDLED_COUNT_FILE)
	require.NoError(t, err)
	require.Equal(t, "2\n", string(data), "handled count file should be updated on context cancel")
}
