package status_test

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/feral-file/ffos-user/components/feral-controld/cdp"
	constants "github.com/feral-file/ffos-user/components/feral-controld/constant"
	"github.com/feral-file/ffos-user/components/feral-controld/mocks"
	"github.com/feral-file/ffos-user/components/feral-controld/sleepschedule"
	"github.com/feral-file/ffos-user/components/feral-controld/status"
	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

// testFF1ConfigJSON is minimal valid ff1-config for GetStatus (no network latest-version fetch).
const testFF1ConfigJSON = `{"version":"1.0.0","branch":"","endpoint":""}`

func expectDeviceStatusOSMocks(t *testing.T, mockOS *mocks.MockOS) {
	t.Helper()
	mockOS.EXPECT().ReadFile(constants.SCREEN_ORIENTATION_FILE).Return(nil, os.ErrNotExist).Times(1)
	mockOS.EXPECT().ReadFile(constants.FF1_CONFIG_FILE).Return([]byte(testFF1ConfigJSON), nil).Times(1)
	mockOS.EXPECT().ReadFile(constants.SLEEP_SCHEDULE_FILE).Return(nil, os.ErrNotExist).Times(1)
	mockOS.EXPECT().ReadFile("/home/feralfile/.state/analytics-toggle-off").Return(nil, os.ErrNotExist).Times(1)
	mockOS.EXPECT().ReadFile("/home/feralfile/.state/beta-features-toggle-on").Return(nil, os.ErrNotExist).Times(1)
	mockOS.EXPECT().IsNotExist(gomock.Any()).DoAndReturn(func(err error) bool { return os.IsNotExist(err) }).AnyTimes()
}

// expectDeviceStatusOSMocksSleepFile is like expectDeviceStatusOSMocks but returns a readable sleep-schedule.json body.
func expectDeviceStatusOSMocksSleepFile(t *testing.T, mockOS *mocks.MockOS, sleepScheduleJSON []byte) {
	t.Helper()
	mockOS.EXPECT().ReadFile(constants.SCREEN_ORIENTATION_FILE).Return(nil, os.ErrNotExist).Times(1)
	mockOS.EXPECT().ReadFile(constants.FF1_CONFIG_FILE).Return([]byte(testFF1ConfigJSON), nil).Times(1)
	mockOS.EXPECT().ReadFile(constants.SLEEP_SCHEDULE_FILE).Return(sleepScheduleJSON, nil).Times(1)
	mockOS.EXPECT().ReadFile("/home/feralfile/.state/analytics-toggle-off").Return(nil, os.ErrNotExist).Times(1)
	mockOS.EXPECT().ReadFile("/home/feralfile/.state/beta-features-toggle-on").Return(nil, os.ErrNotExist).Times(1)
	mockOS.EXPECT().IsNotExist(gomock.Any()).DoAndReturn(func(err error) bool { return os.IsNotExist(err) }).AnyTimes()
}

func expectDeviceStatusExecMocks(t *testing.T, mockExec *mocks.MockExec, nmcliCmd, pamCmd *mocks.MockExecCmd) {
	t.Helper()
	mockExec.EXPECT().
		CommandContext(gomock.Any(), "nmcli", "-t", "-f", "NAME,DEVICE,STATE", "connection", "show", "--active").
		Return(nmcliCmd).
		Times(1)
	nmcliCmd.EXPECT().Output().Return(nil, errors.New("nmcli unavailable")).Times(1)

	mockExec.EXPECT().CommandContext(gomock.Any(), "pamixer", gomock.Any()).Return(pamCmd).Times(2)
	pamCmd.EXPECT().Output().Return(nil, errors.New("pamixer unavailable")).Times(2)
}

func TestGetStatus_DisplayURL_FromCDP(t *testing.T) {
	t.Parallel()

	ctrl := gomock.NewController(t)
	mockOS := mocks.NewMockOS(ctrl)
	mockExec := mocks.NewMockExec(ctrl)
	mockHTTP := mocks.NewMockHTTPClient(ctrl)
	mockIO := mocks.NewMockIO(ctrl)
	mockCDP := mocks.NewMockCDP(ctrl)
	nmcliCmd := mocks.NewMockExecCmd(ctrl)
	pamCmd := mocks.NewMockExecCmd(ctrl)

	expectDeviceStatusOSMocks(t, mockOS)
	expectDeviceStatusExecMocks(t, mockExec, nmcliCmd, pamCmd)

	wantURL := "file:///opt/feral/ui/launcher/index.html?step=qr"
	mockCDP.EXPECT().PageNavigationURL(gomock.Any()).Return(wantURL, nil).Times(1)

	ds := status.NewDeviceStatus(wrapper.NewJSON(), mockOS, mockExec, mockHTTP, mockIO, mockCDP)
	resp, err := ds.GetStatus(context.Background())
	require.NoError(t, err)
	require.NotNil(t, resp)
	require.NotNil(t, resp.DisplayURL)
	assert.Equal(t, wantURL, *resp.DisplayURL)
}

func TestGetStatus_DisplayURL_OmittedOnCDPError(t *testing.T) {
	t.Parallel()

	ctrl := gomock.NewController(t)
	mockOS := mocks.NewMockOS(ctrl)
	mockExec := mocks.NewMockExec(ctrl)
	mockHTTP := mocks.NewMockHTTPClient(ctrl)
	mockIO := mocks.NewMockIO(ctrl)
	mockCDP := mocks.NewMockCDP(ctrl)
	nmcliCmd := mocks.NewMockExecCmd(ctrl)
	pamCmd := mocks.NewMockExecCmd(ctrl)

	expectDeviceStatusOSMocks(t, mockOS)
	expectDeviceStatusExecMocks(t, mockExec, nmcliCmd, pamCmd)

	mockCDP.EXPECT().PageNavigationURL(gomock.Any()).Return("", errors.New("no debug targets")).Times(1)

	ds := status.NewDeviceStatus(wrapper.NewJSON(), mockOS, mockExec, mockHTTP, mockIO, mockCDP)
	resp, err := ds.GetStatus(context.Background())
	require.NoError(t, err)
	require.NotNil(t, resp)
	assert.Nil(t, resp.DisplayURL)
}

func TestGetStatus_SleepSchedule_WhenStateFilePresent(t *testing.T) {
	t.Parallel()

	ctrl := gomock.NewController(t)
	mockOS := mocks.NewMockOS(ctrl)
	mockExec := mocks.NewMockExec(ctrl)
	mockHTTP := mocks.NewMockHTTPClient(ctrl)
	mockIO := mocks.NewMockIO(ctrl)
	mockCDP := mocks.NewMockCDP(ctrl)
	nmcliCmd := mocks.NewMockExecCmd(ctrl)
	pamCmd := mocks.NewMockExecCmd(ctrl)

	expectDeviceStatusOSMocksSleepFile(t, mockOS, []byte(`{"enabled":false,"sleepTime":"22:00","wakeTime":"07:30"}`))
	expectDeviceStatusExecMocks(t, mockExec, nmcliCmd, pamCmd)
	mockCDP.EXPECT().PageNavigationURL(gomock.Any()).Return("", errors.New("no debug targets")).Times(1)

	ds := status.NewDeviceStatus(wrapper.NewJSON(), mockOS, mockExec, mockHTTP, mockIO, mockCDP)
	resp, err := ds.GetStatus(context.Background())
	require.NoError(t, err)
	require.NotNil(t, resp)
	require.NotNil(t, resp.SleepSchedule)
	assert.False(t, resp.SleepSchedule.Enabled)
	assert.Equal(t, "22:00", resp.SleepSchedule.SleepTime)
	assert.Equal(t, "07:30", resp.SleepSchedule.WakeTime)
	assert.Equal(t, sleepschedule.StateAwake, resp.SleepSchedule.CurrentState)
}

func TestGetStatus_DisplayURL_OmittedWhenCDPNil(t *testing.T) {
	t.Parallel()

	ctrl := gomock.NewController(t)
	mockOS := mocks.NewMockOS(ctrl)
	mockExec := mocks.NewMockExec(ctrl)
	mockHTTP := mocks.NewMockHTTPClient(ctrl)
	mockIO := mocks.NewMockIO(ctrl)
	nmcliCmd := mocks.NewMockExecCmd(ctrl)
	pamCmd := mocks.NewMockExecCmd(ctrl)

	expectDeviceStatusOSMocks(t, mockOS)
	expectDeviceStatusExecMocks(t, mockExec, nmcliCmd, pamCmd)

	var nilCDP cdp.CDP
	ds := status.NewDeviceStatus(wrapper.NewJSON(), mockOS, mockExec, mockHTTP, mockIO, nilCDP)
	resp, err := ds.GetStatus(context.Background())
	require.NoError(t, err)
	require.NotNil(t, resp)
	assert.Nil(t, resp.DisplayURL)
}
