package devicectl

import (
	"testing"
	"time"

	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-controld/control"
	"github.com/feral-file/ffos-user/components/feral-controld/mocks"
	"github.com/feral-file/ffos-user/components/feral-controld/setupui"
	"github.com/feral-file/ffos-user/components/feral-controld/state"
)

func stateClaim(t *testing.T, claimed bool) {
	t.Helper()
	sm := mocks.NewMockStateManager(gomock.NewController(t))
	sm.EXPECT().ClaimSnapshot().Return(state.ClaimInfo{Claimed: claimed}).AnyTimes()
	state.InjectStateManagerForTesting(sm)
	t.Cleanup(state.ResetForTesting)
}

// The precheck order of the relay v2 show-code request (spec §7.2).
func TestRelayShowCode_Order(t *testing.T) {
	tests := []struct {
		name       string
		claimed    bool
		codeShown  bool // an invitation-bearing claim QR is on screen
		pinsOnlyQR bool // a claim QR is on screen but carries no invitation
		inFlight   bool
		runs       int
		wantCode   string
		wantNoop   bool
		wantRuns   int
	}{
		{name: "unclaimed is refused before anything else", codeShown: true, inFlight: true, runs: maxRelayShowCodeRuns,
			wantCode: control.CodeEnrollmentRequired, wantRuns: maxRelayShowCodeRuns},
		{name: "live code on screen: success, nothing runs or counts", claimed: true, codeShown: true, wantNoop: true},
		{name: "live code wins over a spent budget", claimed: true, codeShown: true, runs: maxRelayShowCodeRuns,
			wantNoop: true, wantRuns: maxRelayShowCodeRuns},
		{name: "in flight: success, nothing runs or counts", claimed: true, inFlight: true, runs: 1, wantNoop: true, wantRuns: 1},
		{name: "in flight wins over a spent budget", claimed: true, inFlight: true, runs: maxRelayShowCodeRuns,
			wantNoop: true, wantRuns: maxRelayShowCodeRuns},
		{name: "a pins-only QR is not a live code", claimed: true, pinsOnlyQR: true, wantRuns: 1},
		{name: "nothing showing: runs and counts", claimed: true, runs: maxRelayShowCodeRuns - 1, wantRuns: maxRelayShowCodeRuns},
		{name: "budget spent", claimed: true, runs: maxRelayShowCodeRuns, wantCode: control.CodeRateLimited, wantRuns: maxRelayShowCodeRuns},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			stateClaim(t, tc.claimed)
			spy := &narratorSpy{}
			e := &executor{logger: zap.NewNop(), setupNarrator: spy}
			e.SetControl(&fakeControl{})
			if tc.codeShown || tc.pinsOnlyQR {
				spy.calls = append(spy.calls, "claim")
			}
			if tc.codeShown {
				e.scheduleInvitationRefresh(time.Now().Add(time.Hour))
				defer e.stopInvitationRefresh()
			}
			e.relayShowCodeInFlight, e.relayShowCodeRuns = tc.inFlight, tc.runs

			reply, done, code := e.RelayShowCode()
			assert.Equal(t, tc.wantCode, code)
			assert.Equal(t, tc.wantRuns, e.relayShowCodeRuns)
			switch {
			case tc.wantCode != "":
				assert.Nil(t, reply)
				assert.Nil(t, done)
			case tc.wantNoop:
				assert.Equal(t, CmdOK, reply, "a no-op answers with the handler's success body")
				assert.Nil(t, done)
			default:
				require.NotNil(t, done)
				assert.True(t, e.relayShowCodeInFlight)
				done()
				assert.False(t, e.relayShowCodeInFlight)
			}
		})
	}
}

// The budget is gate runs started: taps during one run spend nothing, and
// the run after the last allowed one is refused.
func TestRelayShowCode_CountsRunsNotRequests(t *testing.T) {
	stateClaim(t, true)
	e := &executor{logger: zap.NewNop(), setupNarrator: &narratorSpy{}}
	e.SetControl(&fakeControl{})
	for run := 1; run <= maxRelayShowCodeRuns; run++ {
		_, done, code := e.RelayShowCode()
		require.Empty(t, code)
		require.NotNil(t, done)
		for range 5 {
			reply, again, code := e.RelayShowCode()
			require.Empty(t, code)
			require.Nil(t, again, "a request during a run must not start another")
			require.Equal(t, CmdOK, reply)
		}
		done()
		require.Equal(t, run, e.relayShowCodeRuns)
	}
	_, done, code := e.RelayShowCode()
	assert.Nil(t, done)
	assert.Equal(t, control.CodeRateLimited, code)
}

// A first-owner code requested on a claimed device comes down when nobody
// enrolled in time; the code of an unclaimed or released device does not.
func TestRequestedCodeDeadline(t *testing.T) {
	short := func(t *testing.T) {
		t.Helper()
		old := requestedCodeLife
		requestedCodeLife = -time.Second // already over when armed
		t.Cleanup(func() { requestedCodeLife = old })
	}
	newExec := func() (*executor, *hideSpy, *fakeControl) {
		spy := &hideSpy{narratorSpy: &narratorSpy{calls: []string{"claim"}}}
		fc := &fakeControl{fields: []string{"i", "s", "e", "g"}, ok: true}
		e := &executor{logger: zap.NewNop(), setupNarrator: spy}
		e.SetControl(fc)
		e.scheduleInvitationRefresh(time.Now().Add(time.Hour))
		return e, spy, fc
	}

	t.Run("claimed, purpose enroll: taken down by the watch", func(t *testing.T) {
		short(t)
		stateClaim(t, true)
		e, spy, fc := newExec()
		e.enrollQR.Store(true)
		e.armRequestedCodeDeadline()
		e.refreshInvitationQR()
		assert.Equal(t, []string{setupui.StateClaimQR}, spy.hidden)
		assert.Equal(t, int32(1), fc.cleared.Load(), "the code must stop being redeemable")
		assert.Equal(t, int32(0), fc.issued.Load(), "no fresh invitation after the deadline")
		assert.False(t, e.enrollQR.Load())
		assert.Nil(t, e.invTimer)
	})
	t.Run("claimed, before the deadline: stays", func(t *testing.T) {
		stateClaim(t, true)
		e, spy, fc := newExec()
		defer e.stopInvitationRefresh()
		e.enrollQR.Store(true)
		e.armRequestedCodeDeadline()
		e.refreshInvitationQR()
		assert.Empty(t, spy.hidden)
		assert.Equal(t, int32(0), fc.cleared.Load())
		assert.NotNil(t, e.invTimer)
	})
	t.Run("unclaimed device's own code has no deadline", func(t *testing.T) {
		short(t)
		stateClaim(t, false)
		e, spy, fc := newExec()
		defer e.stopInvitationRefresh()
		e.enrollQR.Store(true)
		e.armRequestedCodeDeadline()
		e.refreshInvitationQR()
		assert.Empty(t, spy.hidden)
		assert.Equal(t, int32(0), fc.cleared.Load())
	})
	t.Run("a later request without purpose enroll clears it", func(t *testing.T) {
		short(t)
		stateClaim(t, true)
		e, spy, _ := newExec()
		defer e.stopInvitationRefresh()
		e.enrollQR.Store(true)
		e.armRequestedCodeDeadline()
		e.enrollQR.Store(false)
		e.armRequestedCodeDeadline()
		e.refreshInvitationQR()
		assert.Empty(t, spy.hidden)
	})
	t.Run("a released device's code has no deadline", func(t *testing.T) {
		short(t)
		stateClaim(t, true)
		e, spy, _ := newExec()
		defer e.stopInvitationRefresh()
		e.enrollQR.Store(true)
		e.armRequestedCodeDeadline()
		e.setRequestedCodeDeadline(time.Time{}) // what ControlReleased does
		e.refreshInvitationQR()
		assert.Empty(t, spy.hidden)
	})
}
