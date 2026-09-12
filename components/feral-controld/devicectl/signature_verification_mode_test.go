package devicectl

// setSignatureVerificationMode persists the owner's DP-1 verification policy
// (feral-file/ffos-user#307) with the device-name record idiom.

import (
	"context"
	"os"
	"testing"

	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	constants "github.com/feral-file/ffos-user/components/feral-controld/constant"
	"github.com/feral-file/ffos-user/components/feral-controld/mocks"
	"github.com/feral-file/ffos-user/components/feral-controld/sigverify"
	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

func TestSetSignatureVerificationMode_PersistsAndEchoesStoredMode(t *testing.T) {
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
	e := &executor{logger: zap.NewNop(), os: mockOS, json: wrapper.NewJSON()}

	result, err := e.setSignatureVerificationMode(context.Background(), []byte(`{"mode":"strict"}`))

	require.NoError(t, err)
	assert.JSONEq(t, `{"mode":"strict"}`, string(written))
	// ok is part of the published reply contract; the echoed mode is the
	// STORED value a controller should adopt.
	assert.Equal(t, map[string]interface{}{"ok": true, "signatureVerificationMode": "strict"}, result)
}

func TestSetSignatureVerificationMode_RejectsBadInputWithoutTouchingDisk(t *testing.T) {
	for _, args := range []string{`{"mode":"loud"}`, `{"mode":"Strict"}`, `{}`, `{"mode":7}`, `not json`} {
		ctrl := gomock.NewController(t)
		mockOS := mocks.NewMockOS(ctrl) // any call is an unexpected-call failure
		e := &executor{logger: zap.NewNop(), os: mockOS, json: wrapper.NewJSON()}

		result, err := e.setSignatureVerificationMode(context.Background(), []byte(args))

		assert.Error(t, err, args)
		assert.Nil(t, result, args)
		assert.Contains(t, err.Error(), "invalid arguments", args)
		ctrl.Finish()
	}
}

// TestClearSignatureVerificationMode_HandOnClearsBothPaths: factory reset's
// clear removes the live record and a stranded .tmp, so a resold unit never
// inherits a previous owner's strict policy.
func TestClearSignatureVerificationMode_HandOnClearsBothPaths(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	mockOS := mocks.NewMockOS(ctrl)
	mockOS.EXPECT().Remove(constants.SIGNATURE_VERIFICATION_FILE).Return(nil)
	mockOS.EXPECT().Remove(constants.SIGNATURE_VERIFICATION_FILE + ".tmp").Return(nil)
	e := &executor{logger: zap.NewNop(), os: mockOS, json: wrapper.NewJSON()}

	assert.NoError(t, e.clearSignatureVerificationMode())
	_ = sigverify.DefaultMode
}
