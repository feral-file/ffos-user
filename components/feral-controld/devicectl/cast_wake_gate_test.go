package devicectl

import (
	"context"
	"testing"

	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"

	"github.com/feral-file/ffos-user/components/feral-controld/cdp"
	"github.com/feral-file/ffos-user/components/feral-controld/mocks"
	"github.com/feral-file/ffos-user/components/feral-controld/sleepschedule"
)

// A failed wake leaves the tracker at playerUnknownFailed with the attempted
// state awake. The navigation gate reads that as "not blocked" (its question
// is whether navigating would route a sleeping wall out of /sleep), but the
// page IS still parked on /sleep, so the next controller cast must retry the
// wake rather than forward into it. PlayerCastWakeGate answers that question.
func TestPlayerCastWakeGate_FailedWakeKeepsCastEligible(t *testing.T) {
	ctrl := gomock.NewController(t)
	mockCDP := mocks.NewMockCDP(ctrl)
	mockCDP.EXPECT().Initialized().Return(true).AnyTimes()
	failNext := false
	mockCDP.EXPECT().Send(cdp.METHOD_EVALUATE, gomock.Any()).DoAndReturn(
		func(string, map[string]interface{}) (interface{}, error) {
			if failNext {
				failNext = false
				return nil, assert.AnError
			}
			return map[string]any{"result": map[string]any{}}, nil
		}).AnyTimes()

	e := &executor{cdp: mockCDP, logger: zaptest.NewLogger(t)}
	ctx := context.Background()
	needsWake := PlayerCastWakeGate(e)
	navBlocked := PlayerSleepGate(e)

	assert.False(t, needsWake(), "a bare executor is awake: nothing to wake")

	require.NoError(t, e.applySleepTransition(ctx, sleepschedule.StateSleeping, "sleep"))
	assert.True(t, needsWake(), "a sleeping player needs waking before a cast")

	failNext = true
	require.Error(t, e.applySleepTransition(ctx, sleepschedule.StateAwake, "wake-fails"))
	assert.False(t, navBlocked(), "the navigation gate reads a failed wake as not blocked")
	assert.True(t, needsWake(), "a cast after a failed wake must retry the wake, not forward into /sleep")

	require.NoError(t, e.applySleepTransition(ctx, sleepschedule.StateAwake, "wake-retry"))
	assert.False(t, needsWake(), "once the wake is confirmed, casts go straight through")
}

// The schedule's desired state alone is enough: a page reloaded inside the
// sleep window (tracker invalidated, not yet re-driven) is about to be put
// to sleep by the loop, so a controller's cast must set the awake override
// first or the loop would sleep the wall right after the cast lands.
func TestPlayerCastWakeGate_ScheduleSaysSleeping(t *testing.T) {
	e := &executor{logger: zaptest.NewLogger(t)}
	needsWake := PlayerCastWakeGate(e)

	e.sleepApplyMu.Lock()
	e.sleepScheduleDesiredCache = sleepschedule.StateSleeping
	e.sleepApplyMu.Unlock()
	assert.True(t, needsWake())

	e.sleepApplyMu.Lock()
	e.sleepScheduleDesiredCache = sleepschedule.StateAwake
	e.sleepApplyMu.Unlock()
	assert.False(t, needsWake())
}

// An executor without the tracker (test doubles) never asks for a wake.
func TestPlayerCastWakeGate_NoTrackerNeverWakes(t *testing.T) {
	ctrl := gomock.NewController(t)
	assert.False(t, PlayerCastWakeGate(mocks.NewMockExecutor(ctrl))())
}
