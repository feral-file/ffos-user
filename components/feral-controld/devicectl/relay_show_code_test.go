package devicectl

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-controld/control"
	"github.com/feral-file/ffos-user/components/feral-controld/mocks"
	"github.com/feral-file/ffos-user/components/feral-controld/otagate"
	"github.com/feral-file/ffos-user/components/feral-controld/setupui"
	"github.com/feral-file/ffos-user/components/feral-controld/state"
	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
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

// The command itself owns the deadline: an earlier request's deadline is
// cleared before the gate (its takedown must not fire under this request),
// and the paint of a requested code on a claimed device arms a new one.
func TestShowPairingQRCode_ArmsRequestedCodeDeadline(t *testing.T) {
	tests := []struct {
		name      string
		args      string
		wantArmed bool
	}{
		{"purpose enroll on a claimed device", `{"show":true,"purpose":"enroll"}`, true},
		{"no purpose", `{"show":true}`, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			sm := mocks.NewMockStateManager(ctrl)
			state.InjectStateManagerForTesting(sm)
			t.Cleanup(state.ResetForTesting)
			sm.EXPECT().GetState().Return(&state.State{}).AnyTimes()
			sm.EXPECT().ClaimSnapshot().Return(state.ClaimInfo{Claimed: true, TopicID: "topic-abc", TopicReady: true}).AnyTimes()
			mockOS := mocks.NewMockOS(ctrl)
			mockOS.EXPECT().ReadFile(gomock.Any()).Return(nil, errors.New("absent")).AnyTimes()
			mockOS.EXPECT().IsNotExist(gomock.Any()).Return(true).AnyTimes()
			mockOS.EXPECT().Stat(gomock.Any()).Return(nil, errors.New("absent")).AnyTimes()

			var e *executor
			var duringGate time.Time
			mockHTTP := mocks.NewMockHTTPClient(ctrl)
			mockHTTP.EXPECT().NewRequest(gomock.Any(), gomock.Any(), gomock.Any()).
				DoAndReturn(func(method, url string, body io.Reader) (*http.Request, error) {
					return http.NewRequest(method, url, body)
				}).AnyTimes()
			mockHTTP.EXPECT().Do(gomock.Any()).DoAndReturn(func(*http.Request) (*http.Response, error) {
				e.invMu.Lock()
				duringGate = e.requestedCodeDeadline
				e.invMu.Unlock()
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(
					`{"min_runtime_version":"0.0.1","latest_version":"1.0.0"}`))}, nil
			}).AnyTimes()

			clk := &autoClaimClock{now: time.Unix(1_000_000, 0)}
			fc := &fakeControl{fields: []string{"i", "s", "e", "g"}, ok: true, pins: []string{"", "s", "e", "g"}}
			e = &executor{logger: zap.NewNop(), setupNarrator: &narratorSpy{}, clock: clk, os: mockOS, json: wrapper.NewJSON(),
				otaGate: otagate.New(otagate.Deps{HTTP: mockHTTP, Clock: clk, Runner: fakeGateRunner{}, Config: &fakeGateConfig{}})}
			e.SetControl(fc)
			e.setRequestedCodeDeadline(time.Now().Add(-time.Minute)) // an earlier request's, already over

			before := time.Now()
			_, err := e.showPairingQRCode(context.Background(), []byte(tc.args))
			require.NoError(t, err)
			defer e.stopInvitationRefresh()

			assert.True(t, duringGate.IsZero(), "the earlier deadline must be cleared before the gate runs")
			e.invMu.Lock()
			got := e.requestedCodeDeadline
			e.invMu.Unlock()
			if !tc.wantArmed {
				assert.True(t, got.IsZero())
				return
			}
			assert.False(t, got.Before(before.Add(requestedCodeLife)), "armed for requestedCodeLife from the paint")
		})
	}
}
