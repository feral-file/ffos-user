package devicectl

// setSignatureVerificationMode persists the owner's DP-1 verification policy
// (feral-file/ffos-user#307) with the device-name record idiom.

import (
	"context"
	"os"
	"sync/atomic"
	"testing"
	"time"

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

func TestSetSignatureVerificationMode_RefusesOnceAFactoryResetIsStaged(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	e := &executor{logger: zap.NewNop(), os: mocks.NewMockOS(ctrl), json: wrapper.NewJSON()}
	e.resetStaged.Store(true)

	_, err := e.setSignatureVerificationMode(context.Background(), []byte(`{"mode":"strict"}`))

	require.Error(t, err)
	assert.Contains(t, err.Error(), "factory reset in progress")
}

// TestSetSignatureVerificationMode_LoserOfTheResetRaceSeesTheLatch: a setter
// admitted before the reset staged, but reaching the record after the reset
// took the lock, must not land behind the reset's clear.
func TestSetSignatureVerificationMode_LoserOfTheResetRaceSeesTheLatch(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	mockOS := mocks.NewMockOS(ctrl)
	e := &executor{logger: zap.NewNop(), os: mockOS, json: wrapper.NewJSON()}

	// The reset holds the record lock while it clears, and the setter is
	// admitted (resetStaged still false) before that.
	e.verificationModeMu.Lock()
	setterDone := make(chan error, 1)
	go func() {
		_, err := e.setSignatureVerificationMode(context.Background(), []byte(`{"mode":"strict"}`))
		setterDone <- err
	}()
	// Reset stages, then clears under the lock it already holds, then releases.
	e.resetStaged.Store(true)
	mockOS.EXPECT().Remove(constants.SIGNATURE_VERIFICATION_FILE).Return(nil)
	mockOS.EXPECT().Remove(constants.SIGNATURE_VERIFICATION_FILE + ".tmp").Return(nil)
	require.NoError(t, sigverify.ClearMode(mockOS))
	e.verificationModeMu.Unlock()

	select {
	case err := <-setterDone:
		require.Error(t, err)
		assert.Contains(t, err.Error(), "factory reset in progress")
	case <-time.After(2 * time.Second):
		t.Fatal("setter never returned")
	}
	// No WriteFile/Rename expectation: the setter must not have touched disk.
}

// TestSetSignatureVerificationMode_CompetingSettersSerialize: both stage
// through one .tmp path, so the second write must not begin until the first
// rename has landed — otherwise one caller can echo a mode the disk does
// not hold.
func TestSetSignatureVerificationMode_CompetingSettersSerialize(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	mockOS := mocks.NewMockOS(ctrl)
	e := &executor{logger: zap.NewNop(), os: mockOS, json: wrapper.NewJSON()}

	firstInWrite := make(chan struct{})
	releaseFirst := make(chan struct{})
	var writes atomic.Int32
	mockOS.EXPECT().MkdirAll(gomock.Any(), gomock.Any()).Return(nil).Times(2)
	gomock.InOrder(
		mockOS.EXPECT().WriteFile(constants.SIGNATURE_VERIFICATION_FILE+".tmp", []byte(`{"mode":"strict"}`), gomock.Any()).
			DoAndReturn(func(string, []byte, os.FileMode) error {
				writes.Add(1)
				close(firstInWrite)
				<-releaseFirst
				return nil
			}),
		mockOS.EXPECT().Rename(constants.SIGNATURE_VERIFICATION_FILE+".tmp", constants.SIGNATURE_VERIFICATION_FILE).Return(nil),
		mockOS.EXPECT().WriteFile(constants.SIGNATURE_VERIFICATION_FILE+".tmp", []byte(`{"mode":"silent"}`), gomock.Any()).
			DoAndReturn(func(string, []byte, os.FileMode) error { writes.Add(1); return nil }),
		mockOS.EXPECT().Rename(constants.SIGNATURE_VERIFICATION_FILE+".tmp", constants.SIGNATURE_VERIFICATION_FILE).Return(nil),
	)

	results := make(chan map[string]interface{}, 2)
	go func() {
		r, err := e.setSignatureVerificationMode(context.Background(), []byte(`{"mode":"strict"}`))
		require.NoError(t, err)
		results <- r.(map[string]interface{})
	}()
	<-firstInWrite
	go func() {
		r, err := e.setSignatureVerificationMode(context.Background(), []byte(`{"mode":"silent"}`))
		require.NoError(t, err)
		results <- r.(map[string]interface{})
	}()
	// The second setter is blocked on the lock: its write has not started.
	time.Sleep(100 * time.Millisecond)
	assert.Equal(t, int32(1), writes.Load(), "the second write must wait for the first rename")
	close(releaseFirst)

	for i := 0; i < 2; i++ {
		select {
		case r := <-results:
			assert.Equal(t, true, r["ok"])
		case <-time.After(2 * time.Second):
			t.Fatal("a setter never returned")
		}
	}
	assert.Equal(t, int32(2), writes.Load())
}

// TestSetSignatureVerificationMode_NotifiesObserverWithStoredMode: the
// observer (main wires the scheduler's re-drive to it) fires after a
// successful store with the stored mode, and never on a refused request.
func TestSetSignatureVerificationMode_NotifiesObserverWithStoredMode(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	mockOS := mocks.NewMockOS(ctrl)
	mockOS.EXPECT().MkdirAll(gomock.Any(), gomock.Any()).Return(nil)
	mockOS.EXPECT().WriteFile(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil)
	mockOS.EXPECT().Rename(gomock.Any(), gomock.Any()).Return(nil)
	// The observer is handed the EFFECTIVE mode, read back from disk, not the
	// value the caller passed — so the record read is expected.
	mockOS.EXPECT().ReadFile(constants.SIGNATURE_VERIFICATION_FILE).Return([]byte(`{"mode":"silent"}`), nil)
	e := &executor{logger: zap.NewNop(), os: mockOS, json: wrapper.NewJSON()}
	var seen []sigverify.Mode
	e.SetVerificationModeObserver(func(m sigverify.Mode) { seen = append(seen, m) })

	_, err := e.setSignatureVerificationMode(context.Background(), []byte(`{"mode":"silent"}`))
	require.NoError(t, err)
	assert.Equal(t, []sigverify.Mode{sigverify.ModeSilent}, seen)

	_, err = e.setSignatureVerificationMode(context.Background(), []byte(`{"mode":"loud"}`))
	require.Error(t, err)
	e.resetStaged.Store(true)
	_, err = e.setSignatureVerificationMode(context.Background(), []byte(`{"mode":"notify"}`))
	require.Error(t, err)
	assert.Equal(t, []sigverify.Mode{sigverify.ModeSilent}, seen, "a refused request notifies nobody")
}

// TestSetSignatureVerificationMode_RefusedWhileVerifierDisabled: with the
// config kill switch on, nothing enforces a mode, so nothing is stored,
// acknowledged, or announced.
func TestSetSignatureVerificationMode_RefusedWhileVerifierDisabled(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	e := &executor{logger: zap.NewNop(), os: mocks.NewMockOS(ctrl), json: wrapper.NewJSON()} // no disk access expected
	e.SetSignatureVerificationCapability(func() bool { return false })
	e.SetVerificationModeObserver(func(sigverify.Mode) { t.Fatal("no observer call for a refused request") })

	result, err := e.setSignatureVerificationMode(context.Background(), []byte(`{"mode":"strict"}`))

	require.Error(t, err)
	assert.Nil(t, result)
	assert.Contains(t, err.Error(), "disabled by device configuration")
}
