package devicectl

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-controld/mocks"
	"github.com/feral-file/ffos-user/components/feral-controld/otagate"
	"github.com/feral-file/ffos-user/components/feral-controld/setupui"
	"github.com/feral-file/ffos-user/components/feral-controld/state"
	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
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
	cleared   atomic.Int32
	protected bool
	owner     bool
	pins      []string
}

func (f *fakeControl) ClaimQRPins() ([]string, bool) { return f.pins, f.pins != nil }

func (f *fakeControl) ConnectAuthority(context.Context) (bool, bool) { return f.protected, f.owner }

func (f *fakeControl) ClearInvitation() { f.cleared.Add(1) }

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

// Review F6: the invitation never outlives the QR that carried it.
func TestInvitationClearedWhenItsQRGoes(t *testing.T) {
	stateUnclaimed(t)
	// show:false (cloud ended pairing).
	fc := &fakeControl{}
	e := &executor{logger: zap.NewNop(), setupNarrator: &narratorSpy{}}
	e.SetControl(fc)
	_, err := e.showPairingQRCodeInProcess(context.Background(), false)
	require.NoError(t, err)
	assert.Equal(t, int32(1), fc.cleared.Load(), "show:false must clear the invitation")

	// Another narration took the panel: the watch clears it.
	fc = &fakeControl{fields: []string{"i", "s", "e", "g"}, ok: true}
	spy := &narratorSpy{calls: []string{"claim", "ready"}}
	e = &executor{logger: zap.NewNop(), setupNarrator: spy}
	e.SetControl(fc)
	e.scheduleInvitationRefresh(time.Now().Add(10 * time.Minute))
	e.refreshInvitationQR()
	assert.Equal(t, int32(1), fc.cleared.Load(), "a replaced QR must clear the invitation")
	assert.Nil(t, e.invTimer, "the watch stops once the invitation is cleared")

	// Still showing and not yet due: re-armed, nothing minted or cleared.
	fc = &fakeControl{fields: []string{"i", "s", "e", "g"}, ok: true}
	spy = &narratorSpy{calls: []string{"claim"}}
	e = &executor{logger: zap.NewNop(), setupNarrator: spy}
	e.SetControl(fc)
	e.scheduleInvitationRefresh(time.Now().Add(10 * time.Minute))
	e.refreshInvitationQR()
	assert.Equal(t, int32(0), fc.cleared.Load())
	assert.Equal(t, int32(0), fc.issued.Load())
	assert.NotNil(t, e.invTimer)
	e.stopInvitationRefresh()
}

func TestInvitationWatchIsBounded(t *testing.T) {
	e := &executor{logger: zap.NewNop()}
	e.scheduleInvitationRefresh(time.Now().Add(10 * time.Minute))
	defer e.stopInvitationRefresh()
	require.NotNil(t, e.invTimer)
	// The watch fires no later than invitationWatchInterval, so a QR taken
	// down by another narration voids its invitation within that bound.
	assert.LessOrEqual(t, invitationWatchInterval, 15*time.Second)
}

// Review F6: a legacy claim (connect) takes the claim QR down, and with it
// the invitation.
func TestConnectClaimClearsInvitation(t *testing.T) {
	ctrl := gomock.NewController(t)
	sm := mocks.NewMockStateManager(ctrl)
	state.InjectStateManagerForTesting(sm)
	t.Cleanup(state.ResetForTesting)
	sm.EXPECT().GetState().Return(&state.State{}).AnyTimes()
	sm.EXPECT().ClaimSnapshot().Return(state.ClaimInfo{}).AnyTimes()
	sm.EXPECT().SetConnectedDevice(gomock.Any()).Return(nil)
	mockCDP := mocks.NewMockCDP(ctrl)
	mockCDP.EXPECT().Send(gomock.Any(), gomock.Any()).Return(nil, errors.New("player not connected")).AnyTimes()

	fc := &fakeControl{}
	e := &executor{logger: zap.NewNop(), setupNarrator: &narratorSpy{}, json: wrapper.NewJSON(), cdp: mockCDP}
	e.SetControl(fc)
	_, err := e.connect(context.Background(), []byte(`{"clientDevice":{"device_id":"phone-1","device_name":"Phone","platform":1},"primaryAddress":"192.168.1.50"}`))
	require.NoError(t, err)
	assert.Equal(t, int32(1), fc.cleared.Load())
}

// §7.1: connect is controller tier, but under the protocol only an owner
// claims or rebinds. A non-owner connect to a claimed device is a no-op ok;
// one to an unclaimed device is refused; an owner claims as before.
func TestConnectUnderControl_OnlyOwnerClaims(t *testing.T) {
	args := []byte(`{"clientDevice":{"device_id":"phone-2","device_name":"Phone 2","platform":1},"primaryAddress":"192.168.1.51"}`)
	setup := func(t *testing.T, claimed bool) (*executor, *mocks.MockStateManager) {
		ctrl := gomock.NewController(t)
		sm := mocks.NewMockStateManager(ctrl)
		state.InjectStateManagerForTesting(sm)
		t.Cleanup(state.ResetForTesting)
		sm.EXPECT().GetState().Return(&state.State{}).AnyTimes()
		sm.EXPECT().ClaimSnapshot().Return(state.ClaimInfo{Claimed: claimed}).AnyTimes()
		mockCDP := mocks.NewMockCDP(ctrl)
		mockCDP.EXPECT().Send(gomock.Any(), gomock.Any()).Return(nil, errors.New("player not connected")).AnyTimes()
		return &executor{logger: zap.NewNop(), setupNarrator: &narratorSpy{}, json: wrapper.NewJSON(), cdp: mockCDP}, sm
	}

	t.Run("claimed, non-owner: ok, nothing written", func(t *testing.T) {
		e, _ := setup(t, true) // no SetConnectedDevice expected
		e.SetControl(&fakeControl{protected: true})
		res, err := e.connect(context.Background(), args)
		require.NoError(t, err)
		assert.Equal(t, CmdOK, res)
	})
	t.Run("unclaimed, non-owner: refused", func(t *testing.T) {
		e, _ := setup(t, false)
		e.SetControl(&fakeControl{protected: true})
		_, err := e.connect(context.Background(), args)
		require.Error(t, err)
	})
	t.Run("unclaimed, owner: claims", func(t *testing.T) {
		e, sm := setup(t, false)
		sm.EXPECT().SetConnectedDevice(gomock.Any()).Return(nil)
		e.SetControl(&fakeControl{protected: true, owner: true})
		_, err := e.connect(context.Background(), args)
		require.NoError(t, err)
	})
	t.Run("legacy: unchanged", func(t *testing.T) {
		e, sm := setup(t, true)
		sm.EXPECT().SetConnectedDevice(gomock.Any()).Return(nil)
		e.SetControl(&fakeControl{})
		_, err := e.connect(context.Background(), args)
		require.NoError(t, err)
	})
}

// refusingSpy refuses automatic claim paints, like setupui does while
// another overlay owns the screen.
type refusingSpy struct{ *narratorSpy }

func (r *refusingSpy) ShowClaimQRAutomatic(string, string) {}

// Review N4d: an invitation is minted for a paint, and voided at once when
// the paint did not take.
func TestRefusedPaintVoidsInvitation(t *testing.T) {
	stateUnclaimed(t)
	ctrl := gomock.NewController(t)
	mockOS := mocks.NewMockOS(ctrl)
	mockOS.EXPECT().ReadFile(gomock.Any()).Return(nil, errors.New("absent")).AnyTimes()
	mockOS.EXPECT().IsNotExist(gomock.Any()).Return(true).AnyTimes()
	mockOS.EXPECT().Stat(gomock.Any()).Return(nil, errors.New("absent")).AnyTimes()

	fc := &fakeControl{fields: []string{"i", "s", "e", "g"}, ok: true}
	e := &executor{logger: zap.NewNop(), setupNarrator: &refusingSpy{&narratorSpy{}}, os: mockOS, json: wrapper.NewJSON()}
	e.SetControl(fc)
	e.paintClaimQRWithInvitation(context.Background(), true)
	assert.Equal(t, int32(1), fc.issued.Load())
	// One clear before the paint (nothing on screen: any old invitation goes,
	// review I3) and one after it (the refused paint voids what it minted).
	assert.Equal(t, int32(2), fc.cleared.Load(), "a refused paint must void the invitation it minted")
	assert.Nil(t, e.invTimer)

	// A paint that takes keeps it.
	fc = &fakeControl{fields: []string{"i", "s", "e", "g"}, ok: true}
	e = &executor{logger: zap.NewNop(), setupNarrator: &narratorSpy{}, os: mockOS, json: wrapper.NewJSON()}
	e.SetControl(fc)
	e.paintClaimQRWithInvitation(context.Background(), true)
	assert.Equal(t, int32(1), fc.cleared.Load(), "only the pre-paint clear; the painted invitation stays")
	e.stopInvitationRefresh()
}

// Review I3: repainting the claim QR after another narration replaced it
// voids the old invitation first, so the repaint mints a new one even
// inside the 15 s watch window.
func TestRepaintAfterReplacementMintsNewInvitation(t *testing.T) {
	stateUnclaimed(t)
	ctrl := gomock.NewController(t)
	mockOS := mocks.NewMockOS(ctrl)
	mockOS.EXPECT().ReadFile(gomock.Any()).Return(nil, errors.New("absent")).AnyTimes()
	mockOS.EXPECT().IsNotExist(gomock.Any()).Return(true).AnyTimes()
	mockOS.EXPECT().Stat(gomock.Any()).Return(nil, errors.New("absent")).AnyTimes()
	fc := &fakeControl{fields: []string{"i", "s", "e", "g"}, ok: true}
	spy := &narratorSpy{calls: []string{"claim", "ready"}} // replaced by another narration
	e := &executor{logger: zap.NewNop(), setupNarrator: spy, os: mockOS, json: wrapper.NewJSON()}
	e.SetControl(fc)
	e.paintClaimQRWithInvitation(context.Background(), false)
	assert.Equal(t, int32(1), fc.cleared.Load(), "the replaced QR's invitation must be voided before the repaint")
	assert.Equal(t, int32(1), fc.issued.Load())
	e.stopInvitationRefresh()
}

// §11/§14.1: a claim QR with no invitation (claimed device, or the
// invitation refused) still carries the pins, with an empty <inv>, so a new
// controller pins from the screen rather than from the network.
func TestClaimURLCarriesPinsWithoutInvitation(t *testing.T) {
	ctrl := gomock.NewController(t)
	sm := mocks.NewMockStateManager(ctrl)
	sm.EXPECT().ClaimSnapshot().Return(state.ClaimInfo{Claimed: true, TopicID: "topic-abc"}).AnyTimes()
	state.InjectStateManagerForTesting(sm)
	t.Cleanup(state.ResetForTesting)
	mockOS := mocks.NewMockOS(ctrl)
	mockOS.EXPECT().ReadFile(gomock.Any()).Return(nil, errors.New("absent")).AnyTimes()
	mockOS.EXPECT().IsNotExist(gomock.Any()).Return(true).AnyTimes()
	mockOS.EXPECT().Stat(gomock.Any()).Return(nil, errors.New("absent")).AnyTimes()

	fc := &fakeControl{pins: []string{"", "pinsig", "pinenc", "gen"}}
	e := &executor{logger: zap.NewNop(), setupNarrator: &narratorSpy{}, os: mockOS, json: wrapper.NewJSON()}
	e.SetControl(fc)
	got := e.buildDeviceConnectURL(context.Background())
	assert.True(t, strings.HasSuffix(got, "|topic-abc|true|||pairing||pinsig|pinenc|gen"), got)
	assert.Equal(t, int32(0), fc.issued.Load(), "a claimed device without purpose enroll mints no invitation")
}

// Review round 4 L5: a show=true admitted while the device had no owner
// must not paint the topic-carrying claim QR when an owner enrolls while the
// pre-claim gate runs.
func TestShowPairingQRDoesNotPaintAfterEnrollmentDuringGate(t *testing.T) {
	run := func(t *testing.T, enrollDuringGate bool) []string {
		ctrl := gomock.NewController(t)
		sm := mocks.NewMockStateManager(ctrl)
		state.InjectStateManagerForTesting(sm)
		t.Cleanup(state.ResetForTesting)
		sm.EXPECT().GetState().Return(&state.State{Relayer: &state.RelayerState{TopicID: "topic-abc"}}).AnyTimes()
		sm.EXPECT().ClaimSnapshot().Return(state.ClaimInfo{TopicID: "topic-abc", TopicReady: true}).AnyTimes()
		mockOS := mocks.NewMockOS(ctrl)
		mockOS.EXPECT().ReadFile(gomock.Any()).Return(nil, errors.New("absent")).AnyTimes()
		mockOS.EXPECT().IsNotExist(gomock.Any()).Return(true).AnyTimes()
		mockOS.EXPECT().Stat(gomock.Any()).Return(nil, errors.New("absent")).AnyTimes()

		fc := &fakeControl{fields: []string{"i", "s", "e", "g"}, ok: true}
		mockHTTP := mocks.NewMockHTTPClient(ctrl)
		mockHTTP.EXPECT().NewRequest(gomock.Any(), gomock.Any(), gomock.Any()).
			DoAndReturn(func(method, url string, body io.Reader) (*http.Request, error) {
				return http.NewRequest(method, url, body)
			}).AnyTimes()
		mockHTTP.EXPECT().Do(gomock.Any()).DoAndReturn(func(*http.Request) (*http.Response, error) {
			if enrollDuringGate {
				fc.enrolled.Store(true)
			}
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(
				`{"min_runtime_version":"0.0.1","latest_version":"1.0.0"}`))}, nil
		}).AnyTimes()

		clk := &autoClaimClock{now: time.Unix(1_000_000, 0)}
		spy := &narratorSpy{}
		e := &executor{logger: zap.NewNop(), setupNarrator: spy, clock: clk, os: mockOS, json: wrapper.NewJSON(),
			otaGate: otagate.New(otagate.Deps{HTTP: mockHTTP, Clock: clk, Runner: fakeGateRunner{}, Config: &fakeGateConfig{}})}
		e.SetControl(fc)
		_, err := e.showPairingQRCodeInProcess(context.Background(), true)
		require.NoError(t, err)
		e.stopInvitationRefresh()
		return spy.calls
	}
	assert.Contains(t, run(t, false), "claim", "baseline: the gate passes and the QR paints")
	assert.NotContains(t, run(t, true), "claim", "an owner enrolled during the gate: no topic QR")
}
