package sleepschedule

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEffectiveStatus_DaytimeWindow(t *testing.T) {
	now := time.Date(2026, 5, 5, 14, 30, 0, 0, time.Local)
	status, changed := EffectiveStatus(now, &Record{
		Enabled:   true,
		SleepTime: "22:00",
		WakeTime:  "07:00",
	})

	require.NotNil(t, status)
	assert.False(t, changed)
	assert.Equal(t, StateAwake, status.CurrentState)
	require.NotNil(t, status.NextTransitionAt)
	assert.Equal(t, 22, status.NextTransitionAt.Hour())
	assert.Equal(t, 0, status.NextTransitionAt.Minute())
}

func TestEffectiveStatus_OvernightSleepingWindow(t *testing.T) {
	now := time.Date(2026, 5, 5, 23, 15, 0, 0, time.Local)
	status, changed := EffectiveStatus(now, &Record{
		Enabled:   true,
		SleepTime: "22:00",
		WakeTime:  "07:00",
	})

	require.NotNil(t, status)
	assert.False(t, changed)
	assert.Equal(t, StateSleeping, status.CurrentState)
	require.NotNil(t, status.NextTransitionAt)
	assert.Equal(t, 7, status.NextTransitionAt.Hour())
	assert.Equal(t, 0, status.NextTransitionAt.Minute())
}

func TestNormalize_ExpiredOverride(t *testing.T) {
	now := time.Date(2026, 5, 5, 8, 0, 0, 0, time.Local)
	expired := now.Add(-time.Minute)
	record, changed := Normalize(&Record{
		Enabled:       true,
		SleepTime:     "22:00",
		WakeTime:      "07:00",
		OverrideState: StateSleeping.Ptr(),
		OverrideUntil: &expired,
	}, now)

	require.NotNil(t, record)
	assert.True(t, changed)
	assert.Nil(t, record.OverrideState)
	assert.Nil(t, record.OverrideUntil)
}

func TestManualSleep_UsesNextWakeBoundary(t *testing.T) {
	now := time.Date(2026, 5, 5, 14, 30, 0, 0, time.Local)
	record, err := ManualSleep(&Record{
		Enabled:   true,
		SleepTime: "22:00",
		WakeTime:  "07:00",
	}, now)

	require.NoError(t, err)
	require.NotNil(t, record.OverrideState)
	assert.Equal(t, StateSleeping, *record.OverrideState)
	require.NotNil(t, record.OverrideUntil)
	assert.Equal(t, 7, record.OverrideUntil.Hour())
	assert.Equal(t, 0, record.OverrideUntil.Minute())
	assert.True(t, record.OverrideUntil.After(now))
}

func TestManualWake_UsesNextSleepBoundary(t *testing.T) {
	now := time.Date(2026, 5, 5, 23, 30, 0, 0, time.Local)
	record, err := ManualWake(&Record{
		Enabled:   true,
		SleepTime: "22:00",
		WakeTime:  "07:00",
	}, now)

	require.NoError(t, err)
	require.NotNil(t, record.OverrideState)
	assert.Equal(t, StateAwake, *record.OverrideState)
	require.NotNil(t, record.OverrideUntil)
	assert.Equal(t, 22, record.OverrideUntil.Hour())
	assert.Equal(t, 0, record.OverrideUntil.Minute())
	assert.True(t, record.OverrideUntil.After(now))
}
