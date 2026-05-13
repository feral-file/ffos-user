package devicectl

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"

	"github.com/feral-file/ffos-user/components/feral-controld/cdp"
	constants "github.com/feral-file/ffos-user/components/feral-controld/constant"
	"github.com/feral-file/ffos-user/components/feral-controld/mocks"
	"github.com/feral-file/ffos-user/components/feral-controld/sleepschedule"
	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

func TestSetSleepSchedule_ReenableClearsStaleManualOverride(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockOS := mocks.NewMockOS(ctrl)
	mockJSON := wrapper.NewJSON()
	mockCDP := mocks.NewMockCDP(ctrl)
	mockClock := mocks.NewMockClock(ctrl)

	now := time.Date(2026, 5, 13, 14, 0, 0, 0, time.Local)
	mockClock.EXPECT().Now().Return(now).Times(1)

	staleRecord := sleepschedule.Record{
		Enabled:       false,
		SleepTime:     "22:00",
		WakeTime:      "07:00",
		OverrideState: sleepschedule.StateSleeping.Ptr(),
	}
	staleBytes, err := json.Marshal(staleRecord)
	require.NoError(t, err)

	mockOS.EXPECT().
		ReadFile(constants.SLEEP_SCHEDULE_FILE).
		Return(staleBytes, nil).
		Times(1)

	mockOS.EXPECT().
		MkdirAll(gomock.Any(), os.FileMode(0o750)).
		Return(nil).
		Times(1)

	var savedRecord sleepschedule.Record
	mockOS.EXPECT().
		WriteFile(constants.SLEEP_SCHEDULE_FILE+".tmp", gomock.Any(), os.FileMode(0o600)).
		DoAndReturn(func(path string, data []byte, perm os.FileMode) error {
			require.NoError(t, json.Unmarshal(data, &savedRecord))
			return nil
		}).
		Times(1)

	mockOS.EXPECT().
		Rename(constants.SLEEP_SCHEDULE_FILE+".tmp", constants.SLEEP_SCHEDULE_FILE).
		Return(nil).
		Times(1)

	mockCDP.EXPECT().
		Send(cdp.METHOD_EVALUATE, gomock.Any()).
		Return(map[string]any{"result": map[string]any{}}, nil).
		Times(1)

	e := &executor{
		cdp:    mockCDP,
		os:     mockOS,
		json:   mockJSON,
		clock:  mockClock,
		logger: zaptest.NewLogger(t),
	}

	result, err := e.setSleepSchedule(context.Background(), []byte(`{"enabled":true}`))
	require.NoError(t, err)

	status, ok := result.(map[string]any)
	require.True(t, ok)

	schedule, ok := status["sleepSchedule"].(*sleepschedule.Status)
	require.True(t, ok)
	require.True(t, schedule.Enabled)
	require.Equal(t, sleepschedule.StateAwake, schedule.CurrentState)
	require.Nil(t, schedule.OverrideState)
	require.Nil(t, schedule.OverrideUntil)

	require.True(t, savedRecord.Enabled)
	require.Equal(t, "22:00", savedRecord.SleepTime)
	require.Equal(t, "07:00", savedRecord.WakeTime)
	require.Nil(t, savedRecord.OverrideState)
	require.Nil(t, savedRecord.OverrideUntil)
}
