package sigverify_test

import (
	"errors"
	"os"
	"strings"
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

// TestParseMode_ErrorNeverEchoesTheValue: the value is caller-sized (a LAN
// request can carry megabytes in `mode`) and the error reaches logs and the
// hub/relayer reply, so it must stay bounded regardless of input.
func TestParseMode_ErrorNeverEchoesTheValue(t *testing.T) {
	huge := strings.Repeat("x", 4<<20)

	_, err := sigverify.ParseMode(huge)

	require.ErrorIs(t, err, sigverify.ErrInvalidMode)
	assert.Less(t, len(err.Error()), 100)
	assert.NotContains(t, err.Error(), "xxxx")
	_, err = sigverify.ParseMode("https://evil.example/leak")
	assert.NotContains(t, err.Error(), "evil")
}

// TestLoadMode_EmptyRecordIsCorruption: an EXISTING empty record can only
// come from a write lost in SaveMode's rename window, so it is a lost
// setting to diagnose, not an unconfigured unit.
func TestLoadMode_EmptyRecordIsCorruption(t *testing.T) {
	for _, raw := range [][]byte{{}, []byte("  \n")} {
		ctrl := gomock.NewController(t)
		mockOS := mocks.NewMockOS(ctrl)
		mockOS.EXPECT().ReadFile(constants.SIGNATURE_VERIFICATION_FILE).Return(raw, nil)

		mode, err := sigverify.LoadMode(mockOS, wrapper.NewJSON())

		assert.Equal(t, sigverify.DefaultMode, mode)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "empty record")
		ctrl.Finish()
	}
}

// TestMode_Allows is the whole policy table: only strict refuses, and it
// refuses everything not proven valid, a missing verdict included.
func TestMode_Allows(t *testing.T) {
	valid := &sigverify.Verdict{Status: sigverify.StatusValid}
	invalid := &sigverify.Verdict{Status: sigverify.StatusInvalid}
	unsigned := &sigverify.Verdict{Status: sigverify.StatusUnsigned}
	for _, tc := range []struct {
		mode    sigverify.Mode
		verdict *sigverify.Verdict
		want    bool
	}{
		{sigverify.ModeSilent, valid, true}, {sigverify.ModeSilent, invalid, true}, {sigverify.ModeSilent, unsigned, true}, {sigverify.ModeSilent, nil, true},
		{sigverify.ModeNotify, valid, true}, {sigverify.ModeNotify, invalid, true}, {sigverify.ModeNotify, unsigned, true}, {sigverify.ModeNotify, nil, true},
		{sigverify.ModeStrict, valid, true}, {sigverify.ModeStrict, invalid, false}, {sigverify.ModeStrict, unsigned, false}, {sigverify.ModeStrict, nil, false},
	} {
		assert.Equal(t, tc.want, tc.mode.Allows(tc.verdict), "%s / %v", tc.mode, tc.verdict)
	}
}
