package sleepschedule

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEffectiveStatus_DefaultRecord_DisabledNoTransitions(t *testing.T) {
	for _, now := range []time.Time{
		time.Date(2026, 5, 5, 14, 30, 0, 0, time.UTC),
		time.Date(2026, 5, 5, 23, 0, 0, 0, time.UTC),
	} {
		t.Run(now.Format(time.RFC3339), func(t *testing.T) {
			status, changed := EffectiveStatus(now, DefaultRecord())
			require.NotNil(t, status)
			assert.False(t, changed)
			assert.False(t, status.Enabled)
			assert.Equal(t, DefaultSleepTime, status.SleepTime)
			assert.Equal(t, DefaultWakeTime, status.WakeTime)
			assert.Equal(t, StateAwake, status.CurrentState)
			assert.Nil(t, status.NextTransitionAt)
		})
	}
}

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

func TestNextOccurrence_SameLocalDayWhenStillAhead(t *testing.T) {
	loc := time.FixedZone("CST8", 8*3600)
	now := time.Date(2026, 5, 5, 14, 30, 0, 0, loc)
	ct := ClockTime{Hour: 22, Minute: 0}
	got := nextOccurrence(now, ct)
	want := time.Date(2026, 5, 5, 22, 0, 0, 0, loc)
	assert.Equal(t, want, got)
}

func TestNextOccurrence_DstSpringEuropeParis_WallClockNextDay(t *testing.T) {
	paris, err := time.LoadLocation("Europe/Paris")
	if err != nil {
		t.Skip("tzdata unavailable:", err)
	}
	// Spring forward night 2025-03-30: 02:00 CET -> 03:00 CEST. After transition,
	// +24h from "today's" 02:30 wall slot skews +1h vs the next calendar day's 02:30.
	now := time.Date(2025, 3, 30, 18, 0, 0, 0, paris)
	ct := ClockTime{Hour: 2, Minute: 30}
	got := nextOccurrence(now, ct)
	want := time.Date(2025, 3, 31, 2, 30, 0, 0, paris)
	assert.Equal(t, want, got)
	assert.Equal(t, 2, got.Hour())
	assert.Equal(t, 30, got.Minute())
}

func TestNextOccurrence_NextDayUsesCalendarAdvance_NotRaw86400s(t *testing.T) {
	paris, err := time.LoadLocation("Europe/Paris")
	if err != nil {
		t.Skip("tzdata unavailable:", err)
	}
	now := time.Date(2025, 3, 30, 18, 0, 0, 0, paris)
	ct := ClockTime{Hour: 2, Minute: 30}
	candidate := ct.OnDay(now)
	require.False(t, candidate.After(now))

	legacy := candidate.Add(24 * time.Hour)
	got := nextOccurrence(now, ct)
	assert.NotEqual(t, legacy, got, "legacy +24h must not match calendar next occurrence on this DST edge")
	assert.Equal(t, time.Hour, legacy.Sub(got), "calendar next is one hour earlier than +24h at this Paris spring edge")
}

func TestNormalizeDays(t *testing.T) {
	cases := []struct {
		name    string
		in      []string
		want    []string
		wantErr bool
	}{
		{name: "nil means every day", in: nil, want: nil},
		{name: "subset canonical order", in: []string{"fri", "mon"}, want: []string{"mon", "fri"}},
		{name: "dedupes and normalizes case", in: []string{"Mon", " MON ", "tue"}, want: []string{"mon", "tue"}},
		{name: "all seven collapses to nil", in: []string{"sun", "mon", "tue", "wed", "thu", "fri", "sat"}, want: nil},
		{name: "empty list rejected", in: []string{}, wantErr: true},
		{name: "invalid token rejected", in: []string{"mon", "funday"}, wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NormalizeDays(tc.in)
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

// 2026-07-10 is a Friday; days below use a Mon-Fri work week so the panel
// must sleep from Friday's sleep time straight through to Monday's wake time.
func TestEffectiveStatus_WeekendOff_FridayNightSleepsThroughWeekend(t *testing.T) {
	now := time.Date(2026, 7, 10, 23, 15, 0, 0, time.Local)
	status, changed := EffectiveStatus(now, &Record{
		Enabled:   true,
		SleepTime: "22:00",
		WakeTime:  "07:00",
		Days:      []string{"mon", "tue", "wed", "thu", "fri"},
	})

	require.NotNil(t, status)
	assert.False(t, changed)
	assert.Equal(t, StateSleeping, status.CurrentState)
	assert.Equal(t, []string{"mon", "tue", "wed", "thu", "fri"}, status.Days)
	require.NotNil(t, status.NextTransitionAt)
	assert.Equal(t, time.Date(2026, 7, 13, 7, 0, 0, 0, time.Local), *status.NextTransitionAt)
}

func TestEffectiveStatus_WeekendOff_SaturdayDaytimeSleeping(t *testing.T) {
	now := time.Date(2026, 7, 11, 14, 0, 0, 0, time.Local)
	status, _ := EffectiveStatus(now, &Record{
		Enabled:   true,
		SleepTime: "22:00",
		WakeTime:  "07:00",
		Days:      []string{"mon", "tue", "wed", "thu", "fri"},
	})

	require.NotNil(t, status)
	assert.Equal(t, StateSleeping, status.CurrentState)
	require.NotNil(t, status.NextTransitionAt)
	assert.Equal(t, time.Date(2026, 7, 13, 7, 0, 0, 0, time.Local), *status.NextTransitionAt)
}

func TestEffectiveStatus_WeekendOff_FridayDaytimeAwakeUntilSleepTime(t *testing.T) {
	now := time.Date(2026, 7, 10, 14, 0, 0, 0, time.Local)
	status, _ := EffectiveStatus(now, &Record{
		Enabled:   true,
		SleepTime: "22:00",
		WakeTime:  "07:00",
		Days:      []string{"mon", "tue", "wed", "thu", "fri"},
	})

	require.NotNil(t, status)
	assert.Equal(t, StateAwake, status.CurrentState)
	require.NotNil(t, status.NextTransitionAt)
	assert.Equal(t, time.Date(2026, 7, 10, 22, 0, 0, 0, time.Local), *status.NextTransitionAt)
}

// With an after-midnight sleep time the awake window normally crosses the day
// boundary; an inactive day still sleeps from its own midnight, so Friday's
// open-ended evening ends at Saturday 00:00, not at Saturday's sleep time.
func TestEffectiveStatus_InactiveDayWithOvernightWindow_SleepsFromMidnight(t *testing.T) {
	now := time.Date(2026, 7, 10, 23, 0, 0, 0, time.Local)
	status, _ := EffectiveStatus(now, &Record{
		Enabled:   true,
		SleepTime: "01:00",
		WakeTime:  "09:00",
		Days:      []string{"sun", "mon", "tue", "wed", "thu", "fri"},
	})

	require.NotNil(t, status)
	assert.Equal(t, StateAwake, status.CurrentState)
	require.NotNil(t, status.NextTransitionAt)
	assert.Equal(t, time.Date(2026, 7, 11, 0, 0, 0, 0, time.Local), *status.NextTransitionAt)
}

// Manual overrides stay day-blind: waking the panel on an inactive Saturday
// lasts until that evening's clock sleep time, after which the day-aware
// natural state (sleeping) takes back over — not until Monday.
func TestManualWake_OnInactiveDay_ExpiresAtNextClockSleepTime(t *testing.T) {
	now := time.Date(2026, 7, 11, 14, 0, 0, 0, time.Local)
	record, err := ManualWake(&Record{
		Enabled:   true,
		SleepTime: "22:00",
		WakeTime:  "07:00",
		Days:      []string{"mon", "tue", "wed", "thu", "fri"},
	}, now)

	require.NoError(t, err)
	require.NotNil(t, record.OverrideState)
	assert.Equal(t, StateAwake, *record.OverrideState)
	require.NotNil(t, record.OverrideUntil)
	assert.Equal(t, time.Date(2026, 7, 11, 22, 0, 0, 0, time.Local), *record.OverrideUntil)
}

func TestValidate_RejectsInvalidDaysWhenEnabled(t *testing.T) {
	err := Validate(&Record{
		Enabled:   true,
		SleepTime: "22:00",
		WakeTime:  "07:00",
		Days:      []string{"someday"},
	})
	require.Error(t, err)
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
