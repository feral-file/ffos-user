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
)

// playerACK is the { message: { ok: true } } envelope a live player returns
// for an acknowledged command (see playerresponse.OK).
func playerACK() interface{} {
	return map[string]interface{}{"message": map[string]interface{}{"ok": true}}
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

	e := &executor{logger: zap.NewNop(), cdp: mockCDP}

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

			e := &executor{logger: zap.NewNop(), cdp: mockCDP}

			e.MaybeRecoverPlayerOnBootOnline(context.Background())
			e.MaybeRecoverPlayerOnBootOnline(context.Background()) // flap: latch holds
		})
	}
}

// TestBootPlayerRecovery_NoPageYetConsumesLatch: if CDP is not connected at
// the online transition, Chromium lost the race instead of the network — the
// page's first load will already see connectivity, so nothing needs
// recovering and a later flap must not touch the (healthy) load.
func TestBootPlayerRecovery_NoPageYetConsumesLatch(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockCDP := mocks.NewMockCDP(ctrl)
	mockCDP.EXPECT().Initialized().Return(false)
	// No Send expectations: neither refresh nor reload may be attempted.

	e := &executor{logger: zap.NewNop(), cdp: mockCDP}

	e.MaybeRecoverPlayerOnBootOnline(context.Background())
	e.MaybeRecoverPlayerOnBootOnline(context.Background())
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

	e := &executor{logger: zap.NewNop(), cdp: mockCDP}

	e.MaybeRecoverPlayerOnBootOnline(context.Background())
	e.MaybeRecoverPlayerOnBootOnline(context.Background())
	assert.True(t, e.bootPlayerRecoveryDone.Load())
}
