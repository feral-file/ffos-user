package devicectl

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

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

// playerRefusal is the { message: { ok: false, error: reason } } envelope a
// live player returns for a refused command.
func playerRefusal(reason string) interface{} {
	return map[string]interface{}{"message": map[string]interface{}{"ok": false, "error": reason}}
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

// expectHandlerReadyProbe answers the pre-refresh handler-readiness poll
// (awaitPlayerCommandHandlerReady) with "installed", so the recovery proceeds
// without waiting — the hydrated-page baseline every pre-poll test assumed.
func expectHandlerReadyProbe(t *testing.T, mockCDP *mocks.MockCDP) *gomock.Call {
	t.Helper()
	return mockCDP.EXPECT().
		NoLogSend(cdp.METHOD_EVALUATE, gomock.Any()).
		DoAndReturn(func(_ string, params map[string]interface{}) (interface{}, error) {
			expr, _ := params["expression"].(string)
			assert.True(t, strings.Contains(expr, "handleCDPRequest"),
				"readiness probe must check the command handler, got: %s", expr)
			return map[string]interface{}{"ready": true}, nil
		}).AnyTimes()
}

// TestBootPlayerRecovery_InAppRefreshSuffices: a live, ACKing player is
// recovered by the in-app artwork refresh alone — no page reload, so the
// player's own crossfade owns the transition.
func TestBootPlayerRecovery_InAppRefreshSuffices(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockCDP := mocks.NewMockCDP(ctrl)
	mockCDP.EXPECT().Initialized().Return(true)
	expectHandlerReadyProbe(t, mockCDP)
	expectRefreshEvaluate(t, mockCDP, playerACK(), nil).Times(1)
	// No Page.reload expectation: escalation must not happen on an ACK.

	e := settledExecutor(mockCDP)

	e.MaybeRecoverPlayerOnBootOnline(context.Background())
	e.MaybeRecoverPlayerOnBootOnline(context.Background()) // flap: latch holds
}

// TestBootPlayerRecovery_EscalatesToReload covers both dead-page shapes: the
// evaluate transport failing, and the evaluate succeeding without a player
// ACK (page up, app never booted). Each must escalate to one page reload,
// requested through the narration surface (which serializes it against
// narration pushes — see setupui.RequestPageReload).
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
			expectHandlerReadyProbe(t, mockCDP)
			expectRefreshEvaluate(t, mockCDP, tc.refreshResult, tc.refreshErr).Times(1)

			e := settledExecutor(mockCDP)
			spy := &narratorSpy{}
			e.setupNarrator = spy

			e.MaybeRecoverPlayerOnBootOnline(context.Background())
			e.MaybeRecoverPlayerOnBootOnline(context.Background()) // flap: latch holds
			assert.Equal(t, 1, spy.reloads, "exactly one reload must be requested")
			assert.Equal(t, 0, spy.reloadSkips)
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
	expectHandlerReadyProbe(t, mockCDP)
	expectRefreshEvaluate(t, mockCDP, playerACK(), nil).Times(1)
	e.CompletePendingBootPlayerRecovery()

	// Later reconnects find nothing parked; later flaps find the latch held.
	e.CompletePendingBootPlayerRecovery()
	e.MaybeRecoverPlayerOnBootOnline(context.Background())
	assert.False(t, e.bootPlayerRecoveryPending.Load())
}

// TestBootPlayerRecovery_ParkedRecoveryExpiresWithBootWindow is the
// delayed-CDP regression test: a recovery parked for a CDP connection that
// only arrives AFTER the kernel boot window closed (display plugged into a
// headless device hours later, mid-exhibition kiosk restart) must be DROPPED,
// not run — that late a first connection means Chromium just started and its
// page loaded with the network already up, a healthy load the boot scoping
// promises never to disturb. The INLINE path (WAN arriving late while CDP is
// already up) is deliberately not expired: the page it repairs loaded broken
// at boot and stays broken until repaired.
func TestBootPlayerRecovery_ParkedRecoveryExpiresWithBootWindow(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockCDP := mocks.NewMockCDP(ctrl)
	mockCDP.EXPECT().Initialized().Return(false)
	// No Send expectations: an expired recovery must not touch the page.

	e := settledExecutor(mockCDP)
	e.bootLifecycleProbe = func() bool { return false } // window already closed

	e.MaybeRecoverPlayerOnBootOnline(context.Background())
	assert.True(t, e.bootPlayerRecoveryPending.Load())

	// CDP's first connection arrives after the boot window: parked recovery
	// expires; nothing runs now or on any later reconnect.
	e.CompletePendingBootPlayerRecovery()
	assert.False(t, e.bootPlayerRecoveryPending.Load())
	e.CompletePendingBootPlayerRecovery()
	assert.True(t, e.bootPlayerRecoveryDone.Load(), "the boot's one-shot latch stays consumed")
}

// TestBootPlayerRecovery_ParkedRecoveryRunsWithinBootWindow: the probe
// answering "still within the window" lets the deferred completion run — the
// normal boot ordering (provisioning online at boot+10s, CDP at boot+30s).
func TestBootPlayerRecovery_ParkedRecoveryRunsWithinBootWindow(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockCDP := mocks.NewMockCDP(ctrl)
	mockCDP.EXPECT().Initialized().Return(false)

	e := settledExecutor(mockCDP)
	e.bootLifecycleProbe = func() bool { return true }

	e.MaybeRecoverPlayerOnBootOnline(context.Background())
	expectHandlerReadyProbe(t, mockCDP)
	expectRefreshEvaluate(t, mockCDP, playerACK(), nil).Times(1)
	e.CompletePendingBootPlayerRecovery()
	assert.False(t, e.bootPlayerRecoveryPending.Load())
}

// TestBootPlayerRecovery_InlinePathIgnoresBootWindowExpiry pins the deliberate
// asymmetry: with CDP already connected at the online transition, the recovery
// runs even when the probe reports the boot window closed — a late WAN arrival
// (Wi-Fi fixed hours after boot) repairs a page that loaded broken at boot and
// stayed broken. Only the DEFERRED (CDP-connect) completion expires. A future
// "cleanup" that routes the inline path through CompletePendingBootPlayerRecovery
// would break this test.
func TestBootPlayerRecovery_InlinePathIgnoresBootWindowExpiry(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockCDP := mocks.NewMockCDP(ctrl)
	mockCDP.EXPECT().Initialized().Return(true)
	expectHandlerReadyProbe(t, mockCDP)
	expectRefreshEvaluate(t, mockCDP, playerACK(), nil).Times(1)

	e := settledExecutor(mockCDP)
	e.bootLifecycleProbe = func() bool { return false } // window long closed

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

// TestBootPlayerRecovery_NarrationOnScreenSkipsReload: the reload fallback is
// the destructive step — it erases whatever setup narration is on screen, and
// a same-target reload does not drop the DevTools websocket, so the on-connect
// Resync that normally restores narration never fires. When the narration
// surface owns the screen the reload must be skipped (the skip decision lives
// on the narration lane itself — setupui.RequestPageReload — so it is atomic
// against concurrent narrators; this test pins the executor honoring the
// skipped outcome). Covers both concurrent owners: the startup OTA gate's
// required-update progress, and a factory reset that raced past the
// completion-entry claimSettled check (settled at entry, reset narration
// painted before the reload would land).
func TestBootPlayerRecovery_NarrationOnScreenSkipsReload(t *testing.T) {
	cases := []struct {
		name    string
		narrate func(*narratorSpy)
	}{
		{name: "required update narrating", narrate: func(s *narratorSpy) { s.ShowUpdating(40) }},
		{name: "factory reset raced past settled check", narrate: func(s *narratorSpy) { s.ShowFactoryReset() }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()

			mockCDP := mocks.NewMockCDP(ctrl)
			mockCDP.EXPECT().Initialized().Return(true)
			expectHandlerReadyProbe(t, mockCDP)
			// Refresh refused with NO reason (bare non-ACK) — the shape that
			// escalates (a reasoned "player alive" refusal no longer does).
			expectRefreshEvaluate(t, mockCDP, map[string]interface{}{}, nil).Times(1)

			e := settledExecutor(mockCDP)
			spy := &narratorSpy{}
			tc.narrate(spy)
			e.setupNarrator = spy

			e.MaybeRecoverPlayerOnBootOnline(context.Background())
			assert.Equal(t, 0, spy.reloads, "the overlay must survive: no reload may execute")
			assert.Equal(t, 1, spy.reloadSkips)
		})
	}
}

// TestBootPlayerRecovery_HiddenNarrationAllowsReload: a hidden overlay must
// NOT block the escalation — after the claim flow's Hide, the reload is the
// legitimate recovery for a dead page.
func TestBootPlayerRecovery_HiddenNarrationAllowsReload(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockCDP := mocks.NewMockCDP(ctrl)
	mockCDP.EXPECT().Initialized().Return(true)
	expectHandlerReadyProbe(t, mockCDP)
	expectRefreshEvaluate(t, mockCDP, map[string]interface{}{}, nil).Times(1)

	e := settledExecutor(mockCDP)
	spy := &narratorSpy{}
	spy.ShowFinalizing()
	spy.Hide() // narration shown, then cleared
	e.setupNarrator = spy

	e.MaybeRecoverPlayerOnBootOnline(context.Background())
	assert.Equal(t, 1, spy.reloads)
	assert.Equal(t, 0, spy.reloadSkips)
}

// TestBootPlayerRecovery_ReloadTransportFailureRearmsForNextConnect: a
// transport-level reload failure means the recovery never ran, so the done
// callback re-arms the park and the NEXT CDP connect retries the whole
// refresh-then-reload sequence — the only path that can legitimately run the
// recovery machinery twice in one boot. Online flaps must still find the
// done latch held (no third entry point), and once a retry's reload goes
// through the park is consumed for good.
func TestBootPlayerRecovery_ReloadTransportFailureRearmsForNextConnect(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockCDP := mocks.NewMockCDP(ctrl)
	mockCDP.EXPECT().Initialized().Return(true)
	expectHandlerReadyProbe(t, mockCDP)
	expectRefreshEvaluate(t, mockCDP, nil, errors.New("target closed")).Times(2)

	e := settledExecutor(mockCDP)
	spy := &narratorSpy{reloadErr: errors.New("target closed")}
	e.setupNarrator = spy

	// First attempt: refresh dies at transport, reload dies at transport —
	// the park re-arms for the next connect instead of stranding the broken
	// pre-network page for the whole exhibition.
	e.MaybeRecoverPlayerOnBootOnline(context.Background())
	assert.Equal(t, 1, spy.reloads)
	assert.True(t, e.bootPlayerRecoveryPending.Load(),
		"a transport-failed reload must re-arm the park for the next CDP connect")
	assert.True(t, e.bootPlayerRecoveryDone.Load())

	// An online flap in the gap must not run anything: the done latch holds.
	e.MaybeRecoverPlayerOnBootOnline(context.Background())
	assert.Equal(t, 1, spy.reloads)

	// Next CDP connect: transport healthy now — exactly one more
	// refresh(escalation)+reload, and the park is consumed.
	spy.reloadErr = nil
	e.CompletePendingBootPlayerRecovery()
	assert.Equal(t, 2, spy.reloads)
	assert.False(t, e.bootPlayerRecoveryPending.Load())

	// Later reconnects find nothing parked.
	e.CompletePendingBootPlayerRecovery()
	assert.Equal(t, 2, spy.reloads)
}

// TestBootPlayerRecovery_PlayerAliveRefusalSkipsReload pins the cross-repo
// contract fix: the player's reasoned refusals mean the app is ALIVE — it
// either parked the refresh for its own replay ("No playlist handler
// registered yet", replayed when the playlist route mounts) or has no artwork
// a reload could repair ("No active artwork to refresh", castInfo still
// hydrating or genuinely empty). Escalating either to Page.reload killed a
// healthy in-flight page load and discarded the queued replay. The one-shot
// latch stays consumed: the player owns the recovery from here.
func TestBootPlayerRecovery_PlayerAliveRefusalSkipsReload(t *testing.T) {
	for _, reason := range []string{
		"No playlist handler registered yet",
		"No active artwork to refresh",
	} {
		t.Run(reason, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()

			mockCDP := mocks.NewMockCDP(ctrl)
			mockCDP.EXPECT().Initialized().Return(true)
			expectHandlerReadyProbe(t, mockCDP)
			expectRefreshEvaluate(t, mockCDP, playerRefusal(reason), nil).Times(1)

			e := settledExecutor(mockCDP)
			spy := &narratorSpy{}
			e.setupNarrator = spy

			e.MaybeRecoverPlayerOnBootOnline(context.Background())
			assert.Equal(t, 0, spy.reloads, "an alive player's refusal must not be answered with a reload")
			assert.Equal(t, 0, spy.reloadSkips, "no reload may even be requested")
			assert.True(t, e.bootPlayerRecoveryDone.Load())
			assert.False(t, e.bootPlayerRecoveryPending.Load())
		})
	}
}

// TestBootPlayerRecovery_WaitsForHydratingHandler: the first CDP connect
// routinely precedes the player app installing window.handleCDPRequest (the
// connect loop needs only a page target). The recovery must poll the handler
// into existence and then succeed in-app — not misread the young page as
// dead and reload it (which roughly doubled boot-to-artwork on healthy
// boots).
func TestBootPlayerRecovery_WaitsForHydratingHandler(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockCDP := mocks.NewMockCDP(ctrl)
	mockCDP.EXPECT().Initialized().Return(true)
	probes := 0
	mockCDP.EXPECT().
		NoLogSend(cdp.METHOD_EVALUATE, gomock.Any()).
		DoAndReturn(func(_ string, _ map[string]interface{}) (interface{}, error) {
			probes++
			if probes < 3 {
				return map[string]interface{}{"ready": false}, nil
			}
			return map[string]interface{}{"ready": true}, nil
		}).Times(3)
	expectRefreshEvaluate(t, mockCDP, playerACK(), nil).Times(1)
	// No reload expectation: a hydrating page is not a dead page.

	e := settledExecutor(mockCDP)
	e.playerReadyPollInterval = time.Millisecond

	e.MaybeRecoverPlayerOnBootOnline(context.Background())
}

// TestBootPlayerRecovery_HandlerNeverReadyEscalatesToReload: a page that never
// installs the command handler is the dead page the reload exists for — the
// poll timing out must fall through to the escalation, not park forever.
func TestBootPlayerRecovery_HandlerNeverReadyEscalatesToReload(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockCDP := mocks.NewMockCDP(ctrl)
	mockCDP.EXPECT().Initialized().Return(true)
	mockCDP.EXPECT().
		NoLogSend(cdp.METHOD_EVALUATE, gomock.Any()).
		Return(map[string]interface{}{"ready": false}, nil).
		AnyTimes()
	expectRefreshEvaluate(t, mockCDP, nil, errors.New("target closed")).Times(1)

	e := settledExecutor(mockCDP)
	e.playerReadyPollInterval = time.Millisecond
	e.playerReadyPollTimeout = 5 * time.Millisecond
	spy := &narratorSpy{}
	e.setupNarrator = spy

	e.MaybeRecoverPlayerOnBootOnline(context.Background())
	assert.Equal(t, 1, spy.reloads, "a handler that never appears must escalate to the reload")
}
