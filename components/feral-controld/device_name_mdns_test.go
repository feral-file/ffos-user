package main

import (
	stdos "os"
	"testing"

	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/assert"
	"go.uber.org/zap"

	constants "github.com/feral-file/ffos-user/components/feral-controld/constant"
	"github.com/feral-file/ffos-user/components/feral-controld/mocks"
	"github.com/feral-file/ffos-user/components/feral-controld/state"
	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

// The invariant this whole feature rests on: an owner's name changes the
// advertised LABEL and nothing else. The advertised ID stays the hostname —
// the serial — because that is the key the pairing app, the registry,
// telemetry, and the owner-contact record all resolve on. If a rename could
// move it, renaming a frame would look to every one of those systems like a
// different machine appearing and the old one vanishing.
func TestResolveMDNSDeviceInfo_NameLabelsWithoutMovingIdentity(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockOS := mocks.NewMockOS(ctrl)
	mockOS.EXPECT().ReadFile(constants.HOSTNAME_FILE).Return([]byte("FF1-3EMX1VQF\n"), nil)
	mockOS.EXPECT().
		ReadFile(constants.DEVICE_NAME_FILE).
		Return([]byte(`{"name":"Living Room"}`), nil)

	info := resolveMDNSDeviceInfo(mockOS, wrapper.NewJSON(), state.ClaimInfo{}, zap.NewNop())

	assert.Equal(t, "Living Room", info.Name)
	assert.Equal(t, "FF1-3EMX1VQF", info.ID)
}

func TestResolveMDNSDeviceInfo_UnnamedUnitAdvertisesItsSerial(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockOS := mocks.NewMockOS(ctrl)
	mockOS.EXPECT().ReadFile(constants.HOSTNAME_FILE).Return([]byte("FF1-3EMX1VQF"), nil)
	mockOS.EXPECT().ReadFile(constants.DEVICE_NAME_FILE).Return(nil, stdos.ErrNotExist)
	mockOS.EXPECT().IsNotExist(stdos.ErrNotExist).Return(true)

	info := resolveMDNSDeviceInfo(mockOS, wrapper.NewJSON(), state.ClaimInfo{}, zap.NewNop())

	assert.Equal(t, "FF1-3EMX1VQF", info.Name)
	assert.Equal(t, "FF1-3EMX1VQF", info.ID)
}

// A corrupt record must not be able to take the device off the network. The
// name is cosmetic; discovery is not.
func TestResolveMDNSDeviceInfo_CorruptNameRecordFallsBackToSerial(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockOS := mocks.NewMockOS(ctrl)
	mockOS.EXPECT().ReadFile(constants.HOSTNAME_FILE).Return([]byte("FF1-3EMX1VQF"), nil)
	mockOS.EXPECT().ReadFile(constants.DEVICE_NAME_FILE).Return([]byte("{not json"), nil)

	info := resolveMDNSDeviceInfo(mockOS, wrapper.NewJSON(), state.ClaimInfo{}, zap.NewNop())

	assert.Equal(t, "FF1-3EMX1VQF", info.Name)
	assert.Equal(t, "FF1-3EMX1VQF", info.ID)
}
