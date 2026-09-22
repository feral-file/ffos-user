package devicectl

import (
	"context"
	"errors"
	"testing"
	"time"

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
	//
	// `ok` is part of the published contract and the app treats a reply
	// without it as malformed, so it is pinned here rather than left to the
	// hub or the relayer to add — neither of them touches this map.
	assert.Equal(t, map[string]interface{}{"ok": true, "deviceName": "Living Room"}, result)
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
	assert.Equal(t, map[string]interface{}{"ok": true, "deviceName": ""}, result)
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

// A missing or null name is malformed, NOT a clear. The two are one character
// apart on the wire and opposite in effect, so an incomplete controller
// request must not silently erase an owner-set label. Note an omitted
// `request` object reaches this handler as literal `null`.
func TestSetDeviceName_RejectsAbsentAndNullName(t *testing.T) {
	for _, args := range []string{`{}`, `null`, `{"name":null}`} {
		t.Run(args, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()

			// No filesystem expectations: none of these may reach the disk.
			e := &executor{
				logger: zap.NewNop(),
				os:     mocks.NewMockOS(ctrl),
				json:   wrapper.NewJSON(),
			}

			_, err := e.setDeviceName(context.Background(), []byte(args))

			require.Error(t, err)
			assert.Contains(t, err.Error(), "name is required")
		})
	}
}

// The command router checks the reset latch before dispatch, which only proves
// no reset had staged when the request was ADMITTED. This pins the re-check
// inside the mutation lock — without it a rename can land after the reset
// cleared the record, and a rolled-back unit keeps the previous owner's label.
func TestSetDeviceName_RefusesOnceAFactoryResetIsStaged(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	e := &executor{
		logger: zap.NewNop(),
		os:     mocks.NewMockOS(ctrl),
		json:   wrapper.NewJSON(),
	}
	e.resetStaged.Store(true)

	_, err := e.setDeviceName(context.Background(), []byte(`{"name":"Living Room"}`))

	require.Error(t, err)
	assert.Contains(t, err.Error(), "factory reset in progress")
}

// The reset's clear must move the ADVERTISED name too. It previously notified
// only the claim observer, whose re-registration republishes the mediator's
// cached name — so a rolled-back reset kept announcing the old label.
func TestClearDeviceName_AnnouncesTheFallback(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockOS := mocks.NewMockOS(ctrl)
	// Clear removes the live record first (resold-frame leak guard), then the
	// staged temp.
	mockOS.EXPECT().Remove(constants.DEVICE_NAME_FILE).Return(nil)
	mockOS.EXPECT().Remove(constants.DEVICE_NAME_FILE + ".tmp").Return(nil)

	e := &executor{logger: zap.NewNop(), os: mockOS, json: wrapper.NewJSON()}

	var announced []string
	e.SetDeviceNameObserver(func(name string) { announced = append(announced, name) })

	require.NoError(t, e.clearDeviceName())
	assert.Equal(t, []string{""}, announced)
}

// A failed disk clear must still move the advertised name: factory reset logs
// the error and continues to the claim observer, whose mDNS re-register
// republishes the mediator's cached name — the broadcast leak outranks the
// local-disk one.
func TestClearDeviceName_AnnouncesTheFallbackEvenWhenClearFails(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	errEIO := errors.New("eio")
	mockOS := mocks.NewMockOS(ctrl)
	mockOS.EXPECT().Remove(constants.DEVICE_NAME_FILE).Return(errEIO)
	mockOS.EXPECT().IsNotExist(errEIO).Return(false)
	mockOS.EXPECT().Remove(constants.DEVICE_NAME_FILE + ".tmp").Return(nil)

	e := &executor{logger: zap.NewNop(), os: mockOS, json: wrapper.NewJSON()}

	var announced []string
	e.SetDeviceNameObserver(func(name string) { announced = append(announced, name) })

	err := e.clearDeviceName()
	require.ErrorIs(t, err, errEIO)
	assert.Equal(t, []string{""}, announced,
		"the fallback must be announced even when the disk clear failed")
}

// TestSetDeviceName_RepaintsAShowingClaimQR pins the second half of the
// rename bug: deviceDisplayName only resolves the name at paint time, and the
// claim QR is painted once per online/topic transition, so a rename that
// lands while it is showing must repaint the guidance text itself or the
// screen keeps naming the old label after mDNS already moved on.
func TestSetDeviceName_RepaintsAShowingClaimQR(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	e, _ := deviceNameExecutor(t, ctrl)
	spy := &narratorSpy{}
	e.setupNarrator = spy
	spy.ShowClaimQR("https://claim.example/x", "FF1-8EVTK3RE")

	_, err := e.setDeviceName(context.Background(), []byte(`{"name":"Living Room"}`))
	require.NoError(t, err)

	assert.Equal(t, []string{"claim", "refresh_claim_name"}, spy.calls)
	assert.Equal(t, "Living Room", spy.lastName)
}

// A clear through setDeviceName("") while the claim QR is showing repaints it
// with the serial — the same fallback deviceDisplayName applies at paint time,
// so the screen and the mDNS advertisement fall back together.
func TestSetDeviceName_ClearRepaintsAShowingClaimQRWithTheSerial(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	e, _ := deviceNameExecutor(t, ctrl)
	mockOS := e.os.(*mocks.MockOS)
	mockOS.EXPECT().ReadFile(constants.HOSTNAME_FILE).Return([]byte("FF1-8EVTK3RE\n"), nil)
	spy := &narratorSpy{}
	e.setupNarrator = spy
	spy.ShowClaimQR("https://claim.example/x", "Living Room")

	_, err := e.setDeviceName(context.Background(), []byte(`{"name":""}`))
	require.NoError(t, err)

	assert.Equal(t, []string{"claim", "refresh_claim_name"}, spy.calls)
	assert.Equal(t, "FF1-8EVTK3RE", spy.lastName)
}

// With no claim QR on screen a rename must not paint one: the refresh is a
// no-op and the serial is not even read (no HOSTNAME_FILE expectation here —
// gomock fails the test on an unexpected read).
func TestSetDeviceName_DoesNotPaintAClaimQRThatIsNotShowing(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	e, _ := deviceNameExecutor(t, ctrl)
	spy := &narratorSpy{}
	e.setupNarrator = spy
	spy.ShowReady()

	_, err := e.setDeviceName(context.Background(), []byte(`{"name":""}`))
	require.NoError(t, err)

	assert.Equal(t, []string{"ready"}, spy.calls)
}

// TestClearDeviceName_DoesNotRepaintTheClaimQR pins the factory-reset side:
// clearDeviceName runs after resetStaged latched and before factory_reset is
// painted, so a repaint here would flash the serial and a rotated connect URL
// over the reset narration. The spy is primed with a showing claim QR so a
// repaint, if one fired, would be recorded.
func TestClearDeviceName_DoesNotRepaintTheClaimQR(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockOS := mocks.NewMockOS(ctrl)
	mockOS.EXPECT().Remove(constants.DEVICE_NAME_FILE).Return(nil)
	mockOS.EXPECT().Remove(constants.DEVICE_NAME_FILE + ".tmp").Return(nil)
	spy := &narratorSpy{}
	e := &executor{logger: zap.NewNop(), os: mockOS, json: wrapper.NewJSON(), setupNarrator: spy}
	spy.ShowClaimQR("https://claim.example/x", "Living Room")

	require.NoError(t, e.clearDeviceName())

	assert.Equal(t, []string{"claim"}, spy.calls)
}

// TestPaintClaimQR_WaitsForAnInFlightRename pins the paint-side lock: a paint
// whose name read could otherwise land before a concurrent rename's Save, and
// whose unconditional push then lands after that rename's refresh, must
// instead wait for the rename to finish and paint the name it stored.
func TestPaintClaimQR_WaitsForAnInFlightRename(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockOS := mocks.NewMockOS(ctrl)
	mockOS.EXPECT().ReadFile(constants.DEVICE_NAME_FILE).
		Return([]byte(`{"name":"Living Room"}`), nil)
	spy := &narratorSpy{}
	e := &executor{logger: zap.NewNop(), os: mockOS, json: wrapper.NewJSON(), setupNarrator: spy}

	// A rename is mid-flight: it holds the record lock.
	e.deviceNameMu.Lock()
	painted := make(chan struct{})
	go func() {
		e.paintClaimQR("https://claim.example/x")
		close(painted)
	}()

	select {
	case <-painted:
		t.Fatal("the paint must not read the name while a rename holds the lock")
	case <-time.After(50 * time.Millisecond):
	}
	assert.Empty(t, spy.calls)

	e.deviceNameMu.Unlock()
	select {
	case <-painted:
	case <-time.After(2 * time.Second):
		t.Fatal("the paint never ran after the rename released the lock")
	}
	assert.Equal(t, []string{"claim"}, spy.calls)
	assert.Equal(t, "Living Room", spy.lastName)
}

// TestDeviceDisplayName_PrefersOwnerNameOverSerial pins the bug where the
// claim QR's guidance text was built from deviceID() (the serial, read once
// from /etc/hostname and never anything else) instead of the current owner
// name: after a rename, mDNS and device status picked up the new name — both
// go through devicename.Load — but the claim-QR text stayed on the serial
// forever. deviceDisplayName must resolve the same record the rename wrote.
func TestDeviceDisplayName_PrefersOwnerNameOverSerial(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockOS := mocks.NewMockOS(ctrl)
	mockOS.EXPECT().ReadFile(constants.DEVICE_NAME_FILE).
		Return([]byte(`{"name":"Living Room"}`), nil)

	e := &executor{logger: zap.NewNop(), os: mockOS, json: wrapper.NewJSON()}

	assert.Equal(t, "Living Room", e.deviceDisplayName())
}

// TestDeviceDisplayName_FallsBackToSerialWhenUnnamed covers the unnamed and
// cleared-name cases: both must fall back to the serial, matching
// resolveMDNSDeviceInfo's own fallback so the claim-QR guidance and the mDNS
// advertisement agree on what an unnamed unit is called. Only those two
// surfaces fall back: status.device_status's deviceName deliberately stays ""
// for an unnamed unit (its presence is the rename-capability signal, see
// docs/api-design.md) and is not a third surface to match.
func TestDeviceDisplayName_FallsBackToSerialWhenUnnamed(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockOS := mocks.NewMockOS(ctrl)
	mockOS.EXPECT().ReadFile(constants.DEVICE_NAME_FILE).
		Return(nil, errors.New("no such file"))
	mockOS.EXPECT().IsNotExist(gomock.Any()).Return(true)
	mockOS.EXPECT().ReadFile(constants.HOSTNAME_FILE).
		Return([]byte("FF1-8EVTK3RE\n"), nil)

	e := &executor{logger: zap.NewNop(), os: mockOS, json: wrapper.NewJSON()}

	assert.Equal(t, "FF1-8EVTK3RE", e.deviceDisplayName())
}

// TestDeviceDisplayName_FallsBackToSerialOnCorruptRecord: a corrupt name
// record must not error the claim QR over a cosmetic field.
func TestDeviceDisplayName_FallsBackToSerialOnCorruptRecord(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockOS := mocks.NewMockOS(ctrl)
	mockOS.EXPECT().ReadFile(constants.DEVICE_NAME_FILE).
		Return([]byte("not json"), nil)
	mockOS.EXPECT().ReadFile(constants.HOSTNAME_FILE).
		Return([]byte("FF1-8EVTK3RE\n"), nil)

	e := &executor{logger: zap.NewNop(), os: mockOS, json: wrapper.NewJSON()}

	assert.Equal(t, "FF1-8EVTK3RE", e.deviceDisplayName())
}
