package devicectl

import (
	"context"
	"testing"

	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	constants "github.com/feral-file/ffos-user/components/feral-controld/constant"
	"github.com/feral-file/ffos-user/components/feral-controld/mocks"
	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

// deviceNameExecutor builds a bare executor with a real JSON wrapper and a
// mocked filesystem, and returns the bytes the rename actually wrote.
func deviceNameExecutor(t *testing.T, ctrl *gomock.Controller) (*executor, *[]byte) {
	t.Helper()

	var written []byte
	mockOS := mocks.NewMockOS(ctrl)
	mockOS.EXPECT().MkdirAll(gomock.Any(), gomock.Any()).Return(nil)
	mockOS.EXPECT().
		WriteFile(constants.DEVICE_NAME_FILE+".tmp", gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ string, data []byte, _ interface{}) error {
			written = data
			return nil
		})
	// Renamed into place rather than written directly: the mDNS advertiser
	// reads this file on re-registration, so a torn write would advertise a
	// half-written name.
	mockOS.EXPECT().
		Rename(constants.DEVICE_NAME_FILE+".tmp", constants.DEVICE_NAME_FILE).
		Return(nil)

	e := &executor{
		logger: zap.NewNop(),
		os:     mockOS,
		json:   wrapper.NewJSON(),
	}
	return e, &written
}

func TestSetDeviceName_StoresSanitizedAndAnnouncesIt(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	e, written := deviceNameExecutor(t, ctrl)

	var announced []string
	e.SetDeviceNameObserver(func(name string) { announced = append(announced, name) })

	result, err := e.setDeviceName(context.Background(), []byte(`{"name":"  Living\nRoom  "}`))
	require.NoError(t, err)

	assert.JSONEq(t, `{"name":"Living Room"}`, string(*written))
	// The reply and the announcement both carry the STORED form. A controller
	// that echoed its own input would drift from the device on the first name
	// that needed cleaning.
	assert.Equal(t, map[string]interface{}{"deviceName": "Living Room"}, result)
	assert.Equal(t, []string{"Living Room"}, announced,
		"a rename must re-register mDNS, or a second controller keeps the old label")
}

func TestSetDeviceName_EmptyNameIsAValidRequest(t *testing.T) {
	// Clearing the field is how an owner undoes a rename; the unit falls back
	// to advertising its serial. This must not read as a malformed command.
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	e, written := deviceNameExecutor(t, ctrl)

	var announced []string
	e.SetDeviceNameObserver(func(name string) { announced = append(announced, name) })

	result, err := e.setDeviceName(context.Background(), []byte(`{"name":"   "}`))
	require.NoError(t, err)

	assert.JSONEq(t, `{"name":""}`, string(*written))
	assert.Equal(t, map[string]interface{}{"deviceName": ""}, result)
	assert.Equal(t, []string{""}, announced)
}

func TestSetDeviceName_RejectsMalformedArguments(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	// No filesystem expectations: a malformed command must not reach the disk.
	e := &executor{
		logger: zap.NewNop(),
		os:     mocks.NewMockOS(ctrl),
		json:   wrapper.NewJSON(),
	}

	_, err := e.setDeviceName(context.Background(), []byte(`not json`))

	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid arguments")
}
