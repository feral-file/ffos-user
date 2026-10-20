package devicectl

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-controld/mocks"
	"github.com/feral-file/ffos-user/components/feral-controld/setupui"
	"github.com/feral-file/ffos-user/components/feral-controld/state"
)

func stateUnclaimed(t *testing.T) {
	t.Helper()
	sm := mocks.NewMockStateManager(gomock.NewController(t))
	sm.EXPECT().ClaimSnapshot().Return(state.ClaimInfo{}).AnyTimes()
	state.InjectStateManagerForTesting(sm)
	t.Cleanup(state.ResetForTesting)
}

type fakeControl struct {
	fields    []string
	ok        bool
	enrolled  atomic.Bool
	retireErr error
	retired   atomic.Int32
	issued    atomic.Int32
}

func (f *fakeControl) ClaimQRFields() ([]string, time.Time, bool) {
	f.issued.Add(1)
	return f.fields, time.Now().Add(10 * time.Minute), f.ok
}
func (f *fakeControl) Enrolled() bool { return f.enrolled.Load() }
func (f *fakeControl) Retire() error {
	f.retired.Add(1)
	return f.retireErr
}

func TestFormatDeviceConnectURL_AppendsInvitation(t *testing.T) {
	got := formatDeviceConnectURL("FF1-X", "topic-abc", true, "main/stable", "1.2.3", "pairing", "inv", "pinsig", "pinenc", "gen")
	want := "https://link.feralfile.com/device_connect/FF1-X|topic-abc|true|main%2Fstable|1.2.3|pairing|inv|pinsig|pinenc|gen"
	assert.Equal(t, want, got)
	// Old parsers read indices 0, 1 and 3; the topic stays at index 1.
	assert.Equal(t, formatDeviceConnectURL("FF1-X", "topic-abc", true, "main/stable", "1.2.3", "pairing"),
		"https://link.feralfile.com/device_connect/FF1-X|topic-abc|true|main%2Fstable|1.2.3|pairing")
}

func TestClaimSettled_OwnerEnrolled(t *testing.T) {
	stateUnclaimed(t)
	fc := &fakeControl{}
	e := &executor{logger: zap.NewNop()}
	assert.False(t, e.claimSettled())
	e.SetControl(fc)
	assert.False(t, e.claimSettled())
	fc.enrolled.Store(true)
	assert.True(t, e.claimSettled(), "an enrolled owner must stop the auto loop repainting the topic QR")
}

// hideSpy records the states HideIfShowing was asked about (narratorSpy
// labels the claim QR "claim", so its own HideIfShowing cannot see claim_qr).
type hideSpy struct {
	*narratorSpy
	hidden []string
}

func (h *hideSpy) HideIfShowing(states ...string) { h.hidden = append(h.hidden, states...) }

func TestControlOwnerEnrolled_TakesClaimQRDown(t *testing.T) {
	spy := &hideSpy{narratorSpy: &narratorSpy{}}
	e := &executor{logger: zap.NewNop(), setupNarrator: spy}
	e.SetControl(&fakeControl{})
	e.enrollQR.Store(true)
	e.scheduleInvitationRefresh(time.Now().Add(time.Hour))
	e.ControlOwnerEnrolled()
	assert.False(t, e.enrollQR.Load())
	assert.Nil(t, e.invTimer)
	assert.Equal(t, []string{setupui.StateClaimQR}, spy.hidden)
}

func TestRefreshInvitationQR_OnlyWhileShowing(t *testing.T) {
	stateUnclaimed(t)
	fc := &fakeControl{fields: []string{"i", "s", "e", "g"}, ok: true}
	spy := &narratorSpy{}
	e := &executor{logger: zap.NewNop(), setupNarrator: spy}
	e.SetControl(fc)
	// Nothing on screen: no invitation is minted.
	e.refreshInvitationQR()
	assert.Equal(t, int32(0), fc.issued.Load())
	// Enrolled meanwhile: nothing either.
	spy.calls = append(spy.calls, "claim")
	fc.enrolled.Store(true)
	e.refreshInvitationQR()
	assert.Equal(t, int32(0), fc.issued.Load())
	e.stopInvitationRefresh()
}

func TestFactoryReset_FailsWhenControlRetirementFails(t *testing.T) {
	fc := &fakeControl{retireErr: errors.New("ebusy")}
	e := &executor{logger: zap.NewNop()}
	e.SetControl(fc)
	_, err := e.factoryReset(context.Background())
	require.Error(t, err)
	assert.Equal(t, int32(1), fc.retired.Load())
	assert.False(t, e.ResetStaged(), "a refused reset must release the latch")
}
