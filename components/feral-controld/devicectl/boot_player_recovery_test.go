package devicectl

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/assert"
	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-controld/cdp"
	"github.com/feral-file/ffos-user/components/feral-controld/mocks"
	"github.com/feral-file/ffos-user/components/feral-controld/state"
)

// playerACK is the { message: { ok: true } } envelope a live player returns
// for an acknowledged command (see playerresponse.OK).
func playerACK() interface{} {
	return map[string]interface{}{"message": map[string]interface{}{"ok": true}}
}

// settledExecutor builds an executor whose claim journey is over
// (pairingConfirmed leg of claimSettled), the population the boot recovery is
// allowed to touch — unclaimed devices belong to the claim flow.
func settledExecutor(mockCDP *mocks.MockCDP) *executor {
	e := &executor{logger: zap.NewNop(), cdp: mockCDP}
	e.pairingConfirmed.Store(true)
	return e
}

// expectRefreshEvaluate registers the in-app refreshArtwork evaluate and
// asserts the expression really carries the refreshArtwork command envelope.
func expectRefreshEvaluate(t *testing.T, mockCDP *mocks.MockCDP, result interface{}, err error) *gomock.Call {
	t.Helper()
	return mockCDP.EXPECT().
		Send(cdp.METHOD_EVALUATE, gomock.Any()).
		DoAndReturn(func(_ string, params map[string]interface{}) (interface{}, error) {
			expr, _ := params["expression"].(string)
			assert.True(t, strings.Contains(expr, "refreshArtwork"),
				"evaluate must carry the refreshArtwork command, got: %s", expr)
			return result, err
		})
}

// TestBootPlayerRecovery_InAppRefreshSuffices: a live, ACKing player is
// recovered by the in-app artwork refresh alone — no page reload, so the
// player's own crossfade owns the transition.
func TestBootPlayerRecovery_InAppRefreshSuffices(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockCDP := mocks.NewMockCDP(ctrl)
	mockCDP.EXPECT().Initialized().Return(true)
	expectRefreshEvaluate(t, mockCDP, playerACK(), nil).Times(1)
	// No Page.reload expectation: escalation must not happen on an ACK.

	e := settledExecutor(mockCDP)

	e.MaybeRecoverPlayerOnBootOnline(context.Background())
	e.MaybeRecoverPlayerOnBootOnline(context.Background()) // flap: latch holds
}

// TestBootPlayerRecovery_EscalatesToReload covers both dead-page shapes: the
// evaluate transport failing, and the evaluate succeeding without a player
// ACK (page up, app never booted). Each must escalate to one Page.reload.
func TestBootPlayerRecovery_EscalatesToReload(t *testing.T) {
	cases := []struct {
		name          string
		refreshResult interface{}
		refreshErr    error
	}{
		{name: "evaluate transport error", refreshErr: errors.New("target closed")},
		{name: "no player ACK", refreshResult: map[string]interface{}{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()

			mockCDP := mocks.NewMockCDP(ctrl)
			mockCDP.EXPECT().Initialized().Return(true)
			expectRefreshEvaluate(t, mockCDP, tc.refreshResult, tc.refreshErr).Times(1)
			mockCDP.EXPECT().Send("Page.reload", gomock.Any()).Return(nil, nil).Times(1)

			e := settledExecutor(mockCDP)

			e.MaybeRecoverPlayerOnBootOnline(context.Background())
			e.MaybeRecoverPlayerOnBootOnline(context.Background()) // flap: latch holds
		})
	}
}

// TestBootPlayerRecovery_WANBeforeCDPConnectRunsOnceOnConnect is the
// lifecycle-ordering regression test: provisioning starts before the CDP
// supervisor, so the boot online transition can fire while Chromium has
// already painted its page (offline) but DevTools is not attached. The
// recovery must PARK — DevTools attach lag says nothing about page-load
// timing, so "no CDP yet" must not consume the boot's only attempt — and the
// CDP on-connect completion must then issue exactly one recovery, with later
// reconnects and online flaps staying no-ops.
func TestBootPlayerRecovery_WANBeforeCDPConnectRunsOnceOnConnect(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockCDP := mocks.NewMockCDP(ctrl)
	mockCDP.EXPECT().Initialized().Return(false)

	e := settledExecutor(mockCDP)

	// WAN confirmed before DevTools attached: nothing may be sent yet, the
	// recovery parks.
	e.MaybeRecoverPlayerOnBootOnline(context.Background())
	assert.True(t, e.bootPlayerRecoveryPending.Load(), "recovery must park, not burn the boot's only attempt")

	// CDP's first successful connection completes the parked recovery: exactly
	// one in-app refresh.
	expectRefreshEvaluate(t, mockCDP, playerACK(), nil).Times(1)
	e.CompletePendingBootPlayerRecovery()

	// Later reconnects find nothing parked; later flaps find the latch held.
	e.CompletePendingBootPlayerRecovery()
	e.MaybeRecoverPlayerOnBootOnline(context.Background())
	assert.False(t, e.bootPlayerRecoveryPending.Load())
}

// TestBootPlayerRecovery_UnclaimedDoesNotTouchPlayer: on an unclaimed device
// the claim flow owns the screen (finalizing narration, claim QR), and with
// no playlist the refresh would refuse and escalate to a Page.reload that can
// erase that overlay. Recovery must not touch CDP at all, and must not park
// anything for the CDP connect callback to fire later — the latch is consumed
// because post-claim content loads with the network already up.
func TestBootPlayerRecovery_UnclaimedDoesNotTouchPlayer(t *testing.T) {
	state.ResetForTesting() // fresh global state: no ConnectedDevice ID -> unclaimed

	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockCDP := mocks.NewMockCDP(ctrl)
	// No Initialized/Send expectations: the claim overlay must stay untouched.

	e := &executor{logger: zap.NewNop(), cdp: mockCDP}

	e.MaybeRecoverPlayerOnBootOnline(context.Background())
	assert.True(t, e.bootPlayerRecoveryDone.Load())
	assert.False(t, e.bootPlayerRecoveryPending.Load())

	// A CDP connect arriving later must not resurrect a recovery.
	e.CompletePendingBootPlayerRecovery()
}

// TestBootPlayerRecovery_TotalFailureDoesNotRetry: refresh AND reload both
// failing consumes the latch anyway — retrying on the next flap would
// reintroduce the mid-exhibition disturbance hazard, and the page is no worse
// off than before.
func TestBootPlayerRecovery_TotalFailureDoesNotRetry(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockCDP := mocks.NewMockCDP(ctrl)
	mockCDP.EXPECT().Initialized().Return(true)
	expectRefreshEvaluate(t, mockCDP, nil, errors.New("target closed")).Times(1)
	mockCDP.EXPECT().Send("Page.reload", gomock.Any()).Return(nil, errors.New("target closed")).Times(1)

	e := settledExecutor(mockCDP)

	e.MaybeRecoverPlayerOnBootOnline(context.Background())
	e.MaybeRecoverPlayerOnBootOnline(context.Background())
	assert.True(t, e.bootPlayerRecoveryDone.Load())
}
