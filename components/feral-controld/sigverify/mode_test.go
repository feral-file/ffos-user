package sigverify_test

import (
	"errors"
	"os"
	"testing"

	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	constants "github.com/feral-file/ffos-user/components/feral-controld/constant"
	"github.com/feral-file/ffos-user/components/feral-controld/mocks"
	"github.com/feral-file/ffos-user/components/feral-controld/sigverify"
	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

func TestParseMode(t *testing.T) {
	for _, ok := range []string{"silent", "notify", "strict"} {
		m, err := sigverify.ParseMode(ok)
		require.NoError(t, err)
		assert.Equal(t, sigverify.Mode(ok), m)
	}
	for _, bad := range []string{"", "Strict", "off", "notify "} {
		_, err := sigverify.ParseMode(bad)
		assert.ErrorIs(t, err, sigverify.ErrInvalidMode, bad)
	}
}

func TestLoadMode_MissingFileIsDefault(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	mockOS := mocks.NewMockOS(ctrl)
	mockOS.EXPECT().ReadFile(constants.SIGNATURE_VERIFICATION_FILE).Return(nil, os.ErrNotExist)
	mockOS.EXPECT().IsNotExist(os.ErrNotExist).Return(true)

	mode, err := sigverify.LoadMode(mockOS, wrapper.NewJSON())

	require.NoError(t, err)
	assert.Equal(t, sigverify.DefaultMode, mode)
	assert.Equal(t, sigverify.ModeNotify, mode, "the default is the non-blocking, visible mode")
}

func TestLoadMode_CorruptOrUnknownFallsBackToDefaultWithError(t *testing.T) {
	for _, body := range []string{"{not json", `{"mode":"paranoid"}`} {
		ctrl := gomock.NewController(t)
		mockOS := mocks.NewMockOS(ctrl)
		mockOS.EXPECT().ReadFile(constants.SIGNATURE_VERIFICATION_FILE).Return([]byte(body), nil)

		mode, err := sigverify.LoadMode(mockOS, wrapper.NewJSON())

		assert.Error(t, err, body)
		assert.Equal(t, sigverify.DefaultMode, mode, "a bad record must never block casts")
		ctrl.Finish()
	}
}

func TestLoadMode_ReadErrorFallsBackToDefaultWithError(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	mockOS := mocks.NewMockOS(ctrl)
	ioErr := errors.New("EIO")
	mockOS.EXPECT().ReadFile(constants.SIGNATURE_VERIFICATION_FILE).Return(nil, ioErr)
	mockOS.EXPECT().IsNotExist(ioErr).Return(false)

	mode, err := sigverify.LoadMode(mockOS, wrapper.NewJSON())

	assert.ErrorIs(t, err, ioErr)
	assert.Equal(t, sigverify.DefaultMode, mode)
}

func TestSaveMode_TmpThenRename_ThenLoadsBack(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	mockOS := mocks.NewMockOS(ctrl)
	var written []byte
	gomock.InOrder(
		mockOS.EXPECT().MkdirAll("/home/feralfile/.state", os.FileMode(0o750)).Return(nil),
		mockOS.EXPECT().WriteFile(constants.SIGNATURE_VERIFICATION_FILE+".tmp", gomock.Any(), os.FileMode(0o600)).
			DoAndReturn(func(_ string, data []byte, _ os.FileMode) error { written = data; return nil }),
		mockOS.EXPECT().Rename(constants.SIGNATURE_VERIFICATION_FILE+".tmp", constants.SIGNATURE_VERIFICATION_FILE).Return(nil),
	)

	require.NoError(t, sigverify.SaveMode(mockOS, wrapper.NewJSON(), sigverify.ModeStrict))
	assert.JSONEq(t, `{"mode":"strict"}`, string(written))

	mockOS.EXPECT().ReadFile(constants.SIGNATURE_VERIFICATION_FILE).Return(written, nil)
	mode, err := sigverify.LoadMode(mockOS, wrapper.NewJSON())
	require.NoError(t, err)
	assert.Equal(t, sigverify.ModeStrict, mode)
}

func TestSaveMode_RejectsUnknownBeforeTouchingDisk(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	mockOS := mocks.NewMockOS(ctrl) // no expectations: any call fails the test

	err := sigverify.SaveMode(mockOS, wrapper.NewJSON(), sigverify.Mode("loud"))

	assert.ErrorIs(t, err, sigverify.ErrInvalidMode)
}

func TestClearMode_RemovesLiveAndTmp_ToleratesAbsence(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	mockOS := mocks.NewMockOS(ctrl)
	mockOS.EXPECT().Remove(constants.SIGNATURE_VERIFICATION_FILE).Return(nil)
	mockOS.EXPECT().Remove(constants.SIGNATURE_VERIFICATION_FILE + ".tmp").Return(os.ErrNotExist)
	mockOS.EXPECT().IsNotExist(os.ErrNotExist).Return(true)

	assert.NoError(t, sigverify.ClearMode(mockOS))
}

func TestClearMode_LiveFailureDoesNotSkipTmp(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	mockOS := mocks.NewMockOS(ctrl)
	eio := errors.New("EIO")
	mockOS.EXPECT().Remove(constants.SIGNATURE_VERIFICATION_FILE).Return(eio)
	mockOS.EXPECT().IsNotExist(eio).Return(false)
	mockOS.EXPECT().Remove(constants.SIGNATURE_VERIFICATION_FILE + ".tmp").Return(nil)

	assert.ErrorIs(t, sigverify.ClearMode(mockOS), eio)
}
