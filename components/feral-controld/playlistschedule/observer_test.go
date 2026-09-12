package playlistschedule_test

import (
	"context"
	"testing"
	"time"

	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest"

	"github.com/feral-file/ffos-user/components/feral-controld/mocks"
	"github.com/feral-file/ffos-user/components/feral-controld/playlistschedule"
)

// TestSetPushObserver_RunsAfterAcceptedPush pins the seam signature
// verification's pending verdict relies on (feral-file/ffos-user#307): the
// observer sees PushStarting before every send and PushAccepted only after
// one the player accepted.
func TestSetPushObserver_RunsAfterAcceptedPush(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	clock := mocks.NewMockClock(ctrl)
	cdpMock := mocks.NewMockCDP(ctrl)
	now := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	clock.EXPECT().Now().Return(now).AnyTimes()
	clock.EXPECT().SleepContext(gomock.Any(), gomock.Any()).DoAndReturn(
		func(ctx context.Context, _ time.Duration) error { <-ctx.Done(); return ctx.Err() },
	).AnyTimes()
	cdpMock.EXPECT().Initialized().Return(true).AnyTimes()

	sched := playlistschedule.New(context.Background(), cdpMock, clock, func() *time.Location {
		return time.UTC
	}, zaptest.NewLogger(t, zaptest.Level(zap.FatalLevel)))
	defer sched.Stop()

	var observed []playlistschedule.PushPhase
	sched.SetPushObserver(func(p playlistschedule.PushPhase) { observed = append(observed, p) })

	_ = sched.Prepare(displayAtPlaylist(
		item("today", "2026-07-22T00:00:00Z"),
		item("tomorrow", "2026-07-23T00:00:00Z"),
	))
	sched.Commit()

	// Accepted push: observer fires once.
	cdpMock.EXPECT().Send(gomock.Any(), gomock.Any()).Return(map[string]any{
		"message": map[string]any{"ok": true},
	}, nil).Times(1)
	sched.RecomputeNow(context.Background())
	require.Equal(t, []playlistschedule.PushPhase{playlistschedule.PushStarting, playlistschedule.PushAccepted}, observed)

	// Rejected push: Starting fires (the send went out), Accepted does not.
	cdpMock.EXPECT().Send(gomock.Any(), gomock.Any()).Return(map[string]any{
		"message": map[string]any{"ok": false},
	}, nil).Times(1)
	sched.RecomputeNow(context.Background())
	assert.Equal(t, []playlistschedule.PushPhase{playlistschedule.PushStarting, playlistschedule.PushAccepted, playlistschedule.PushStarting}, observed)
}
