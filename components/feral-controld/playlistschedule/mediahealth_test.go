package playlistschedule_test

import (
	"context"
	"testing"
	"time"

	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/assert"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest"

	dp1playlist "github.com/display-protocol/dp1-go/playlist"

	"github.com/feral-file/ffos-user/components/feral-controld/cdp"
	"github.com/feral-file/ffos-user/components/feral-controld/mocks"
	"github.com/feral-file/ffos-user/components/feral-controld/playlistschedule"
)

// A scheduler cutover push builds its own displayPlaylist command, so it
// carries the media-health hint argument for the cohort it pushes
// (feral-file/ffos-user#392), exactly like commandrouter's casts.
func TestPush_CarriesMediaHealthHintsForTheCohort(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	clock := mocks.NewMockClock(ctrl)
	cdpMock := mocks.NewMockCDP(ctrl)
	loc := time.UTC
	now := time.Date(2026, 7, 22, 12, 0, 0, 0, loc)
	clock.EXPECT().Now().Return(now).AnyTimes()
	clock.EXPECT().SleepContext(gomock.Any(), gomock.Any()).DoAndReturn(
		func(ctx context.Context, _ time.Duration) error {
			<-ctx.Done()
			return ctx.Err()
		},
	).AnyTimes()

	cdpMock.EXPECT().Initialized().Return(true).Times(1)
	cdpMock.EXPECT().Send(cdp.METHOD_EVALUATE, gomock.Any()).DoAndReturn(
		func(_ string, params map[string]interface{}) (interface{}, error) {
			expr, _ := params["expression"].(string)
			assert.Contains(t, expr, `"action":"now_display"`)
			assert.Contains(t, expr, `"mediaHealth":{"https://o/day22":{"state":"broken"}}`)
			return map[string]interface{}{"ok": true}, nil
		},
	).Times(1)

	sched := playlistschedule.New(context.Background(), cdpMock, clock, func() *time.Location {
		return loc
	}, zaptest.NewLogger(t, zaptest.Level(zap.FatalLevel)))
	var asked []dp1playlist.PlaylistItem
	sched.SetHintSource(func(items []dp1playlist.PlaylistItem) interface{} {
		asked = items
		return map[string]map[string]string{"https://o/day22": {"state": "broken"}}
	})

	_ = sched.Prepare(displayAtPlaylist(
		item("day22", "2026-07-22T00:00:00Z"),
		item("day23", "2026-07-23T00:00:00Z"),
	))
	sched.RecomputeNow(context.Background())

	if assert.Len(t, asked, 1, "hints are asked for the pushed cohort only") {
		assert.Equal(t, "day22", asked[0].ID)
	}
}
