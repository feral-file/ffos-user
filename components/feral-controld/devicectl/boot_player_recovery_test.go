package devicectl

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-controld/cdp"
	"github.com/feral-file/ffos-user/components/feral-controld/mocks"
	"github.com/feral-file/ffos-user/components/feral-controld/playersession"
	"github.com/feral-file/ffos-user/components/feral-controld/state"
	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

// This file exercises the boot player recovery state machine (design doc
// §3.2, implementation in boot_recovery.go): the total classification table,
// the executed-attempt budget, backoff-as-primary-re-entry, the capability
// fuse, and the NavResult rows.

// playerACK is the { message: { ok: true } } envelope a live player returns
// for an acknowledged command (see playerresponse.OK).
func playerACK() interface{} {
	return map[string]interface{}{"message": map[string]interface{}{"ok": true}}
}

// playerRefusal is the { message: { ok: false, error: reason } } envelope a
// live player returns for a refused command.
func playerRefusal(reason string) interface{} {
	return map[string]interface{}{"message": map[string]interface{}{"ok": false, "error": reason}}
}

// playerRefusalCoded is a refusal carrying the machine-readable `code` field.
func playerRefusalCoded(reason, code string) interface{} {
	return map[string]interface{}{"message": map[string]interface{}{"ok": false, "error": reason, "code": code}}
}

// settledExecutor builds an executor whose claim journey is over
// (pairingConfirmed leg of claimSettled) — the population boot recovery is
// allowed to touch — with a real clock (background backoff timers spawned in
// tests harmlessly outlive the test; none of these tests waits on one) and a
// no-op logger.
func settledExecutor(mockCDP *mocks.MockCDP) *executor {
	e := &executor{logger: zap.NewNop(), cdp: mockCDP, clock: wrapper.NewClock()}
	e.pairingConfirmed.Store(true)
	return e
}

// expectRefreshEvaluate registers the in-app refreshArtwork evaluate (Send,
// not NoLogSend — the actual command dispatch) and asserts the expression
// really carries the refreshArtwork command envelope.
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

// cdpDispatch is the boot-recovery round's two possible NoLogSend probes.
// gomock matches unconstrained EXPECT() registrations in REGISTRATION
// ORDER — two separate `.NoLogSend(cdp.METHOD_EVALUATE, gomock.Any())`
// registrations on the same mock do NOT dispatch by DoAndReturn content;
// the first one registered eats every call, silently starving the second.
// installCDPDispatch is therefore the ONE NoLogSend registration each test
// may install, dispatching internally by expression content.
type cdpDispatch struct {
	// status answers the __ffosPlayerStatus probe. nil means "unavailable"
	// (old player, or a transport hiccup) — the round falls through to the
	// handler-ready+evaluate path.
	status func() (interface{}, error)
	// handlerReady answers the awaitPlayerCommandHandlerReady poll. nil
	// means "installed" (ready:true) on every poll.
	handlerReady func() (interface{}, error)
}

func installCDPDispatch(mockCDP *mocks.MockCDP, d cdpDispatch) {
	mockCDP.EXPECT().
		NoLogSend(cdp.METHOD_EVALUATE, gomock.Any()).
		DoAndReturn(func(_ string, params map[string]interface{}) (interface{}, error) {
			expr, _ := params["expression"].(string)
			switch {
			case strings.Contains(expr, "__ffosPlayerStatus"):
				if d.status != nil {
					return d.status()
				}
				return nil, errors.New("old player: no __ffosPlayerStatus")
			case strings.Contains(expr, "typeof window.handleCDPRequest"):
				if d.handlerReady != nil {
					return d.handlerReady()
				}
				return map[string]interface{}{"ready": true}, nil
			default:
				return nil, fmt.Errorf("unexpected NoLogSend expression: %s", expr)
			}
		}).AnyTimes()
}

// fixedStatus is a cdpDispatch.status closure returning the same structured
// status payload on every call.
func fixedStatus(route string, handlerRegistered, hasArtwork bool, bootHydration string) func() (interface{}, error) {
	return func() (interface{}, error) {
		return map[string]interface{}{
			"protocol":          float64(1),
			"route":             route,
			"handlerRegistered": handlerRegistered,
			"hasArtwork":        hasArtwork,
			"bootHydration":     bootHydration,
		}, nil
	}
}

// expectHandlerReadyProbe installs a dispatcher with no structured status
// (old player) and an always-ready handler probe — the common case for
// tests exercising the refreshArtwork-evaluate path directly.
func expectHandlerReadyProbe(mockCDP *mocks.MockCDP) {
	installCDPDispatch(mockCDP, cdpDispatch{})
}

// expectStructuredStatus installs a dispatcher answering __ffosPlayerStatus
// with a fixed payload; the handler-ready probe is never reached by these
// tests (structured status alone resolves the round).
func expectStructuredStatus(mockCDP *mocks.MockCDP, route string, handlerRegistered, hasArtwork bool, bootHydration string) {
	installCDPDispatch(mockCDP, cdpDispatch{status: fixedStatus(route, handlerRegistered, hasArtwork, bootHydration)})
}

// fakeBootRecoverySession is a directly-controllable BootRecoverySession
// double. navFn, when set, is invoked synchronously in place of the real
// session's async worker, so NavResult-row tests stay deterministic with no
// goroutine synchronization needed. A nil navFn resolves NavExecuted+nil
// (verified success) by default.
type fakeBootRecoverySession struct {
	mu    sync.Mutex
	gen   uint64
	calls int
	navFn func(playersession.NavOptions, func(playersession.NavResult))
}

func (f *fakeBootRecoverySession) Generation() uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.gen
}

func (f *fakeBootRecoverySession) NavigateHome(opts playersession.NavOptions, done func(playersession.NavResult)) {
	f.mu.Lock()
	f.calls++
	fn := f.navFn
	f.mu.Unlock()
	if fn != nil {
		fn(opts, done)
		return
	}
	done(playersession.NavResult{Outcome: playersession.NavExecuted})
}

func (f *fakeBootRecoverySession) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func navResultDone(outcome playersession.NavOutcome, err error) func(playersession.NavOptions, func(playersession.NavResult)) {
	return func(_ playersession.NavOptions, done func(playersession.NavResult)) {
		done(playersession.NavResult{Outcome: outcome, Err: err})
	}
}

func writeBootRecoveryContract(t *testing.T, hasPlayerStatus bool) string {
	t.Helper()
	body := `{"contracts":{"setupDisplay":{"version":1,"requestKey":"request","states":["softap_qr","joining","join_failed","updating","claim_qr","ready","hidden"],"acceptedResponse":{"ok":true}}}}`
	if hasPlayerStatus {
		body = `{"contracts":{"playerStatus":{"version":1}}}`
	}
	path := filepath.Join(t.TempDir(), "ffos-player-contract.json")
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	return path
}

// awaitBootRecoveryState polls e.bootRecoveryState until it reaches want (or
// times out) — needed wherever a round's terminal transition happens inside
// an async NavigateHome/backoff-timer goroutine.
func awaitBootRecoveryState(t *testing.T, e *executor, want bootRecoveryState) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		e.bootRecoveryMu.Lock()
		got := e.bootRecoveryState
		e.bootRecoveryMu.Unlock()
		if got == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("boot recovery state = %s, want %s (timed out)", got, want)
		}
		time.Sleep(time.Millisecond)
	}
}

// --- one-shot arming / unclaimed devices ------------------------------------

// TestBootRecovery_UnclaimedExpiresLatchWithoutTouchingPlayer: on an
// unclaimed device the claim flow owns the screen; the machine must Expire
// immediately without any CDP traffic, and the one-shot latch (now the
// terminal state itself) must hold across later online flaps.
func TestBootRecovery_UnclaimedExpiresLatchWithoutTouchingPlayer(t *testing.T) {
	state.ResetForTesting()
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	mockCDP := mocks.NewMockCDP(ctrl) // no expectations: must not touch CDP

	e := &executor{logger: zap.NewNop(), cdp: mockCDP, clock: wrapper.NewClock()}

	e.MaybeRecoverPlayerOnBootOnline(context.Background())
	assert.Equal(t, bootRecExpired, e.bootRecoveryState)

	e.MaybeRecoverPlayerOnBootOnline(context.Background())
	assert.Equal(t, bootRecExpired, e.bootRecoveryState, "later flaps must not resurrect the machine")
	e.CompletePendingBootPlayerRecovery() // must also no-op
}

// TestBootRecovery_OnlineFlapIsNoOpOnceArmed: once armed (and settled to a
// terminal state), a later online flap must not re-run the round.
func TestBootRecovery_OnlineFlapIsNoOpOnceArmed(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	mockCDP := mocks.NewMockCDP(ctrl)
	mockCDP.EXPECT().Initialized().Return(true).AnyTimes()
	expectStructuredStatus(mockCDP, statusRoutePlaylist, true, false, bootHydrationOK)

	e := settledExecutor(mockCDP)
	e.MaybeRecoverPlayerOnBootOnline(context.Background())
	assert.Equal(t, bootRecSucceeded, e.bootRecoveryState)

	e.MaybeRecoverPlayerOnBootOnline(context.Background())
	assert.Equal(t, bootRecSucceeded, e.bootRecoveryState)
}

// --- row 1: fast-fail on no CDP connection ----------------------------------

// TestBootRecovery_NoCDPConnectionDefersWithoutCountingAttempt pins §3.2's
// first table row: cdp.Initialized()==false fast-fails to Deferred with NO
// attempt counted.
func TestBootRecovery_NoCDPConnectionDefersWithoutCountingAttempt(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	mockCDP := mocks.NewMockCDP(ctrl)
	mockCDP.EXPECT().Initialized().Return(false).AnyTimes()

	e := settledExecutor(mockCDP)
	e.MaybeRecoverPlayerOnBootOnline(context.Background())

	assert.Equal(t, bootRecDeferred, e.bootRecoveryState)
	assert.Equal(t, 0, e.bootRecoveryAttempts, "a no-connection deferral must not count as an attempt")
}

// --- row 2: StageHandler timeout on a live connection escalates ------------

// TestBootRecovery_HandlerNeverReadyOnLiveConnectionNavigates pins row 2: a
// page that never installs the command handler, on a still-live CDP
// connection, escalates straight to NavigateHome (skipping the fruitless
// evaluate) and counts as one executed attempt.
func TestBootRecovery_HandlerNeverReadyOnLiveConnectionNavigates(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	mockCDP := mocks.NewMockCDP(ctrl)
	mockCDP.EXPECT().Initialized().Return(true).AnyTimes()
	installCDPDispatch(mockCDP, cdpDispatch{
		handlerReady: func() (interface{}, error) { return map[string]interface{}{"ready": false}, nil },
	})

	e := settledExecutor(mockCDP)
	e.playerReadyPollInterval = time.Millisecond
	e.playerReadyPollTimeout = 5 * time.Millisecond
	sess := &fakeBootRecoverySession{}
	e.bootRecoverySession = sess

	e.MaybeRecoverPlayerOnBootOnline(context.Background())

	assert.Equal(t, 1, sess.callCount())
	assert.Equal(t, bootRecSucceeded, e.bootRecoveryState)
	assert.Equal(t, 1, e.bootRecoveryAttempts)
}

// TestBootRecovery_HandlerReadyWaitObservesCtxCancellation pins that
// awaitPlayerCommandHandlerReady observes the ctx attemptBootRecovery
// threads through: with playerReadyPollTimeout set far longer than the
// caller's ctx, a canceled/expired ctx must interrupt the wait promptly
// instead of running out the full timeout.
func TestBootRecovery_HandlerReadyWaitObservesCtxCancellation(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	mockCDP := mocks.NewMockCDP(ctrl)
	mockCDP.EXPECT().Initialized().Return(true).AnyTimes()
	installCDPDispatch(mockCDP, cdpDispatch{
		handlerReady: func() (interface{}, error) { return map[string]interface{}{"ready": false}, nil },
	})

	e := settledExecutor(mockCDP)
	e.playerReadyPollInterval = time.Millisecond
	e.playerReadyPollTimeout = 2 * time.Second // much longer than the ctx below
	sess := &fakeBootRecoverySession{}
	e.bootRecoverySession = sess

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	start := time.Now()
	e.MaybeRecoverPlayerOnBootOnline(ctx)
	elapsed := time.Since(start)

	assert.Less(t, elapsed, 500*time.Millisecond,
		"ctx cancellation must interrupt the wait, not run out the full playerReadyPollTimeout")
	// Falls through to navigateForRecovery exactly as a plain timeout would
	// (handler never ready, connection still Initialized) — ctx-done and
	// timeout are the same outcome from the caller's side.
	assert.Equal(t, 1, sess.callCount())
}

// --- structured status classification (rows 3,4,6,7,8,9,10) ----------------

func TestBootRecovery_StructuredStatusClassification(t *testing.T) {
	cases := []struct {
		name              string
		route             string
		handlerRegistered bool
		hasArtwork        bool
		bootHydration     string
		wantState         bootRecoveryState
		wantNavigate      bool
	}{
		{
			name:          "row 3: hydration pending defers",
			route:         statusRoutePlaylist,
			bootHydration: bootHydrationPending,
			wantState:     bootRecDeferred,
		},
		{
			name:          "row 4: halted_cleared succeeds",
			route:         statusRoutePlaylist,
			bootHydration: bootHydrationHaltedCleared,
			wantState:     bootRecSucceeded,
		},
		{
			name:          "row 6: hydration failed navigates",
			route:         statusRoutePlaylist,
			bootHydration: bootHydrationFailed,
			wantState:     bootRecSucceeded, // fake session resolves NavExecuted+nil
			wantNavigate:  true,
		},
		{
			name:          "row 7: no artwork + hydration ok succeeds",
			route:         statusRoutePlaylist,
			hasArtwork:    false,
			bootHydration: bootHydrationOK,
			wantState:     bootRecSucceeded,
		},
		{
			name:          "row 8: route sleep defers (backoff + wake accelerator)",
			route:         statusRouteSleep,
			bootHydration: bootHydrationOK,
			wantState:     bootRecDeferred,
		},
		{
			name:          "row 9: route error expires, never navigates",
			route:         statusRouteError,
			bootHydration: bootHydrationOK,
			wantState:     bootRecExpired,
		},
		{
			name:              "row 10: artwork present, handler not registered, on /playlist defers",
			route:             statusRoutePlaylist,
			handlerRegistered: false,
			hasArtwork:        true,
			bootHydration:     bootHydrationOK,
			wantState:         bootRecDeferred,
		},
		{
			name:          "[NV9] halted_preserving classifies by route: sleep defers",
			route:         statusRouteSleep,
			bootHydration: bootHydrationHaltedPreserving,
			wantState:     bootRecDeferred,
		},
		{
			name:          "[NV9] halted_preserving classifies by route: error expires",
			route:         statusRouteError,
			bootHydration: bootHydrationHaltedPreserving,
			wantState:     bootRecExpired,
		},
		{
			// [minor #7] Route gates are checked BEFORE hydration
			// classification: bootHydration=failed alone would navigate
			// (see "row 6" above), but route=/error must take precedence —
			// [NV2] NEVER navigate on /error, regardless of what hydration
			// says. Before the fix this co-occurrence hit the
			// failed->navigate row first and violated NV2.
			name:          "[minor #7] route error takes precedence over hydration failed: expires, never navigates",
			route:         statusRouteError,
			bootHydration: bootHydrationFailed,
			wantState:     bootRecExpired,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()
			mockCDP := mocks.NewMockCDP(ctrl)
			mockCDP.EXPECT().Initialized().Return(true).AnyTimes()
			expectStructuredStatus(mockCDP, tc.route, tc.handlerRegistered, tc.hasArtwork, tc.bootHydration)

			e := settledExecutor(mockCDP)
			sess := &fakeBootRecoverySession{}
			e.bootRecoverySession = sess

			e.MaybeRecoverPlayerOnBootOnline(context.Background())

			assert.Equal(t, tc.wantState, e.bootRecoveryState)
			if tc.wantNavigate {
				assert.Equal(t, 1, sess.callCount(), "must have escalated to NavigateHome")
				assert.Equal(t, 1, e.bootRecoveryAttempts)
			} else {
				assert.Equal(t, 0, sess.callCount(), "must not have navigated")
			}
		})
	}
}

// TestBootRecovery_ProtocolMismatchStillHonorsErrorRouteGate pins the fix for
// finding 3 (protocol fail-open on a safety gate): a payload advertising a
// protocol version this classifier does not understand must still honor
// route=="/error" and expire — [NV2] "never navigate over /error" must hold
// even against a future protocol bump, not silently disable the moment a
// player starts advertising protocol:2. Everything else in the payload
// (handlerRegistered, hasArtwork, bootHydration) would otherwise have fallen
// through to the live refresh path, which must never be reached here.
func TestBootRecovery_ProtocolMismatchStillHonorsErrorRouteGate(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	mockCDP := mocks.NewMockCDP(ctrl)
	mockCDP.EXPECT().Initialized().Return(true).AnyTimes()
	installCDPDispatch(mockCDP, cdpDispatch{
		status: func() (interface{}, error) {
			return map[string]interface{}{
				"protocol":          float64(2), // a future protocol version this classifier can't decode
				"route":             statusRouteError,
				"handlerRegistered": true,
				"hasArtwork":        true,
				"bootHydration":     bootHydrationOK,
			}, nil
		},
	})

	e := settledExecutor(mockCDP)
	sess := &fakeBootRecoverySession{}
	e.bootRecoverySession = sess

	e.MaybeRecoverPlayerOnBootOnline(context.Background())

	assert.Equal(t, bootRecExpired, e.bootRecoveryState, "must still expire on route=/error, not fall through to the live refresh path")
	assert.Equal(t, 0, sess.callCount(), "must never navigate over /error, regardless of protocol")
}

// --- row 11 + refusal-code rows 12/13/14/15/16 (evaluateRefreshArtwork) ----

// TestBootRecovery_RefreshAckSucceeds pins row 11.
func TestBootRecovery_RefreshAckSucceeds(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	mockCDP := mocks.NewMockCDP(ctrl)
	mockCDP.EXPECT().Initialized().Return(true).AnyTimes()
	expectHandlerReadyProbe(mockCDP)
	expectRefreshEvaluate(t, mockCDP, playerACK(), nil).Times(1)

	e := settledExecutor(mockCDP)
	e.MaybeRecoverPlayerOnBootOnline(context.Background())

	assert.Equal(t, bootRecSucceeded, e.bootRecoveryState)
	assert.Equal(t, 1, e.bootRecoveryAttempts, "the evaluate itself is the counted attempt")
}

func TestBootRecovery_RefusalCodeClassification(t *testing.T) {
	cases := []struct {
		name         string
		reply        interface{}
		wantState    bootRecoveryState
		wantNavigate bool
	}{
		{
			name:      "row 12: code=handler_pending defers (verify next re-entry)",
			reply:     playerRefusalCoded("No playlist handler registered yet", "handler_pending"),
			wantState: bootRecDeferred,
		},
		{
			name:      "row 13: code=no_artwork defers",
			reply:     playerRefusalCoded("No active artwork to refresh", "no_artwork"),
			wantState: bootRecDeferred,
		},
		{
			name:      "legacy string fallback: bare error text with no code classifies the same as row 12",
			reply:     playerRefusal("No playlist handler registered yet"),
			wantState: bootRecDeferred,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()
			mockCDP := mocks.NewMockCDP(ctrl)
			mockCDP.EXPECT().Initialized().Return(true).AnyTimes()
			expectHandlerReadyProbe(mockCDP)
			expectRefreshEvaluate(t, mockCDP, tc.reply, nil).Times(1)

			e := settledExecutor(mockCDP)
			e.MaybeRecoverPlayerOnBootOnline(context.Background())

			assert.Equal(t, tc.wantState, e.bootRecoveryState)
			assert.Equal(t, 1, e.bootRecoveryAttempts, "the evaluate is always the counted attempt")
		})
	}
}

// TestBootRecovery_PreviewUpdateFailedOnceThenNavigates pins row 14: the
// FIRST code=preview_update_failed defers (verify next re-entry); a SECOND
// occurrence escalates to NavigateHome.
func TestBootRecovery_PreviewUpdateFailedOnceThenNavigates(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	mockCDP := mocks.NewMockCDP(ctrl)
	mockCDP.EXPECT().Initialized().Return(true).AnyTimes()
	expectHandlerReadyProbe(mockCDP)
	refusal := playerRefusalCoded("Playlist handler could not update preview URL", "preview_update_failed")
	expectRefreshEvaluate(t, mockCDP, refusal, nil).Times(2)

	e := settledExecutor(mockCDP)
	sess := &fakeBootRecoverySession{}
	e.bootRecoverySession = sess

	// First occurrence: deferred, tolerance consumed.
	e.MaybeRecoverPlayerOnBootOnline(context.Background())
	assert.Equal(t, bootRecDeferred, e.bootRecoveryState)
	assert.Equal(t, 0, sess.callCount())
	assert.Equal(t, 1, e.bootRecoveryAttempts, "the first occurrence's evaluate counts one attempt")

	// Manually re-enter (bypassing the real backoff timer) to simulate the
	// second occurrence.
	e.attemptBootRecovery(context.Background(), "manual-retry")
	assert.Equal(t, bootRecSucceeded, e.bootRecoveryState)
	assert.Equal(t, 1, sess.callCount(), "the second occurrence must escalate to NavigateHome")
	// [minor #9] The second round reaches TWO distinct CDP-level actions (a
	// refused evaluate, then an escalated navigate), but must consume only
	// ONE budget slot for that round — total 2, not 3.
	assert.Equal(t, 2, e.bootRecoveryAttempts, "one round consuming both an evaluate and an escalated navigate must count as ONE attempt")
}

// TestBootRecovery_UnclassifiedRefusalCapabilityFuse pins rows 15/16: a bare
// non-ACK / unknown code escalates to NavigateHome ONLY when the player
// contract confirms contracts.playerStatus support; otherwise (unreadable OR
// genuinely absent) it defers conservatively and NEVER navigates.
func TestBootRecovery_UnclassifiedRefusalCapabilityFuse(t *testing.T) {
	t.Run("capability present: escalates", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		defer ctrl.Finish()
		mockCDP := mocks.NewMockCDP(ctrl)
		mockCDP.EXPECT().Initialized().Return(true).AnyTimes()
		expectHandlerReadyProbe(mockCDP)
		expectRefreshEvaluate(t, mockCDP, map[string]interface{}{}, nil).Times(1)

		e := settledExecutor(mockCDP)
		e.bootRecoveryContractPath = writeBootRecoveryContract(t, true)
		sess := &fakeBootRecoverySession{}
		e.bootRecoverySession = sess

		e.MaybeRecoverPlayerOnBootOnline(context.Background())
		assert.Equal(t, 1, sess.callCount())
		assert.Equal(t, bootRecSucceeded, e.bootRecoveryState)
	})

	t.Run("capability absent from a readable manifest: conservative, never navigates, latches", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		defer ctrl.Finish()
		mockCDP := mocks.NewMockCDP(ctrl)
		mockCDP.EXPECT().Initialized().Return(true).AnyTimes()
		expectHandlerReadyProbe(mockCDP)
		expectRefreshEvaluate(t, mockCDP, map[string]interface{}{}, nil).Times(1)

		e := settledExecutor(mockCDP)
		e.bootRecoveryContractPath = writeBootRecoveryContract(t, false) // no playerStatus contract
		sess := &fakeBootRecoverySession{}
		e.bootRecoverySession = sess

		e.MaybeRecoverPlayerOnBootOnline(context.Background())
		assert.Equal(t, 0, sess.callCount(), "must never navigate on an absent capability")
		assert.Equal(t, bootRecDeferred, e.bootRecoveryState)
		assert.True(t, e.bootRecoveryConservative, "absence from a READ manifest must latch conservative mode")
	})

	t.Run("capability unreadable: re-checks next re-entry, never latches", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		defer ctrl.Finish()
		mockCDP := mocks.NewMockCDP(ctrl)
		mockCDP.EXPECT().Initialized().Return(true).AnyTimes()
		expectHandlerReadyProbe(mockCDP)
		expectRefreshEvaluate(t, mockCDP, map[string]interface{}{}, nil).Times(1)

		e := settledExecutor(mockCDP)
		e.bootRecoveryContractPath = filepath.Join(t.TempDir(), "does-not-exist.json") // unreadable
		sess := &fakeBootRecoverySession{}
		e.bootRecoverySession = sess

		e.MaybeRecoverPlayerOnBootOnline(context.Background())
		assert.Equal(t, 0, sess.callCount())
		assert.Equal(t, bootRecDeferred, e.bootRecoveryState)
		assert.False(t, e.bootRecoveryConservative, "an unreadable manifest must never latch")
	})

	// [minor #10] A round that already read the structured status
	// successfully has LIVE PROOF of the capability the manifest fuse
	// exists to approximate — it must skip the fuse entirely, even against
	// a manifest that (falsely, e.g. stale/OTA-mid-replace) reports the
	// capability absent.
	t.Run("live status probe this round skips the manifest fuse entirely", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		defer ctrl.Finish()
		mockCDP := mocks.NewMockCDP(ctrl)
		mockCDP.EXPECT().Initialized().Return(true).AnyTimes()
		// Structured status answers this round (falls through: handler
		// registered, has artwork, on /playlist — nothing else to classify),
		// so the live refresh attempt below still runs and gets refused
		// unclassifiably.
		installCDPDispatch(mockCDP, cdpDispatch{
			status: fixedStatus(statusRoutePlaylist, true, true, bootHydrationOK),
		})
		expectRefreshEvaluate(t, mockCDP, map[string]interface{}{}, nil).Times(1)

		e := settledExecutor(mockCDP)
		e.bootRecoveryContractPath = writeBootRecoveryContract(t, false) // no playerStatus contract
		sess := &fakeBootRecoverySession{}
		e.bootRecoverySession = sess

		e.MaybeRecoverPlayerOnBootOnline(context.Background())
		assert.Equal(t, 1, sess.callCount(), "live proof this round must escalate despite the manifest reporting absent")
		assert.Equal(t, bootRecSucceeded, e.bootRecoveryState)
		assert.False(t, e.bootRecoveryConservative, "the manifest fuse must never even run when live proof already exists")
	})
}

// --- NavResult rows (17-21) -------------------------------------------------

func TestBootRecovery_NavResultClassification(t *testing.T) {
	cases := []struct {
		name         string
		navFn        func(playersession.NavOptions, func(playersession.NavResult))
		wantState    bootRecoveryState
		wantAttempts int
	}{
		{
			name:         "row 17: NavExecuted verified succeeds",
			navFn:        navResultDone(playersession.NavExecuted, nil),
			wantState:    bootRecSucceeded,
			wantAttempts: 1,
		},
		{
			name:         "row 18: NavExecuted + err defers, counts as attempt",
			navFn:        navResultDone(playersession.NavExecuted, errors.New("verify timeout")),
			wantState:    bootRecDeferred,
			wantAttempts: 1,
		},
		{
			name:         "row 19: NavSkippedOverlay expires",
			navFn:        navResultDone(playersession.NavSkippedOverlay, nil),
			wantState:    bootRecExpired,
			wantAttempts: 0,
		},
		{
			name:         "row 20: NavSkippedAsleep defers (backoff + wake accelerator), no count",
			navFn:        navResultDone(playersession.NavSkippedAsleep, nil),
			wantState:    bootRecDeferred,
			wantAttempts: 0,
		},
		{
			name:         "row 21: NavSuperseded defers, no count",
			navFn:        navResultDone(playersession.NavSuperseded, nil),
			wantState:    bootRecDeferred,
			wantAttempts: 0,
		},
		{
			name:         "row 21: NavEvicted defers, no count",
			navFn:        navResultDone(playersession.NavEvicted, nil),
			wantState:    bootRecDeferred,
			wantAttempts: 0,
		},
		{
			// A NavOutcome this switch does not recognize (a future addition
			// to the playersession package) must not silently wedge the
			// machine in Attempting with no timer scheduled — see
			// navigateForRecovery's default case.
			name:         "unrecognized NavOutcome defers conservatively, no count",
			navFn:        navResultDone(playersession.NavOutcome(99), nil),
			wantState:    bootRecDeferred,
			wantAttempts: 0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()
			mockCDP := mocks.NewMockCDP(ctrl)
			mockCDP.EXPECT().Initialized().Return(true).AnyTimes()
			// bootHydration=failed drives every case straight into navigateForRecovery.
			expectStructuredStatus(mockCDP, statusRoutePlaylist, true, true, bootHydrationFailed)

			e := settledExecutor(mockCDP)
			sess := &fakeBootRecoverySession{navFn: tc.navFn}
			e.bootRecoverySession = sess

			e.MaybeRecoverPlayerOnBootOnline(context.Background())

			assert.Equal(t, tc.wantState, e.bootRecoveryState)
			assert.Equal(t, tc.wantAttempts, e.bootRecoveryAttempts)
		})
	}
}

// TestBootRecovery_NoSessionWiredDegradesToDeferredAttempt: a build that
// hasn't wired a session (or a test double) must never panic on an
// escalation — it counts as a failed executed attempt and defers.
func TestBootRecovery_NoSessionWiredDegradesToDeferredAttempt(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	mockCDP := mocks.NewMockCDP(ctrl)
	mockCDP.EXPECT().Initialized().Return(true).AnyTimes()
	expectStructuredStatus(mockCDP, statusRoutePlaylist, true, true, bootHydrationFailed)

	e := settledExecutor(mockCDP) // bootRecoverySession left nil
	e.MaybeRecoverPlayerOnBootOnline(context.Background())

	assert.Equal(t, bootRecDeferred, e.bootRecoveryState)
	assert.Equal(t, 1, e.bootRecoveryAttempts)
}

// TestBootRecovery_LateTransitionAgainstTerminalStateIsNoOp pins minor #12:
// a late call to succeed/expire/defer (the shape a delayed NavigateHome
// NavResult callback would take, arriving after the machine already settled
// via some other path) must never move an already-terminal state — no
// bogus re-transition, no corrupted terminal invariant.
func TestBootRecovery_LateTransitionAgainstTerminalStateIsNoOp(t *testing.T) {
	terminalStates := []bootRecoveryState{bootRecSucceeded, bootRecExpired, bootRecExhausted}
	for _, terminal := range terminalStates {
		t.Run(terminal.String(), func(t *testing.T) {
			e := &executor{logger: zap.NewNop()}
			e.bootRecoveryState = terminal
			e.bootRecoveryAttempts = bootRecoveryMaxExecuted // so a late defer would otherwise exhaust, not just defer

			e.succeedBootRecovery("late-succeed")
			assert.Equal(t, terminal, e.bootRecoveryState, "late succeed must not move an already-terminal state")

			e.expireBootRecovery("late-expire")
			assert.Equal(t, terminal, e.bootRecoveryState, "late expire must not move an already-terminal state")

			e.deferBootRecovery("late-defer")
			assert.Equal(t, terminal, e.bootRecoveryState, "late defer must not move an already-terminal state")
			assert.Nil(t, e.bootRecoveryBackoffCancel, "a late defer must not schedule a backoff timer")
		})
	}
}

// --- budget rule -------------------------------------------------------------

// TestBootRecovery_ExhaustedAfterMaxExecutedAttempts pins the budget rule:
// once bootRecoveryMaxExecuted (3) attempts have EXECUTED, THAT round's own
// deferral lands on Exhausted instead of scheduling another backoff round —
// so exactly 3 evaluates run in total, not 4.
func TestBootRecovery_ExhaustedAfterMaxExecutedAttempts(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	mockCDP := mocks.NewMockCDP(ctrl)
	mockCDP.EXPECT().Initialized().Return(true).AnyTimes()
	expectHandlerReadyProbe(mockCDP)
	// Every evaluate refuses with an unclassifiable reason (bare non-ACK),
	// and the capability fuse is unreadable (never latches, never
	// navigates) — so every round defers (or exhausts) and counts one
	// attempt.
	expectRefreshEvaluate(t, mockCDP, map[string]interface{}{}, nil).Times(3)

	e := settledExecutor(mockCDP)
	e.bootRecoveryContractPath = filepath.Join(t.TempDir(), "missing.json")

	e.MaybeRecoverPlayerOnBootOnline(context.Background()) // attempt 1
	assert.Equal(t, bootRecDeferred, e.bootRecoveryState)
	assert.Equal(t, 1, e.bootRecoveryAttempts)

	e.attemptBootRecovery(context.Background(), "manual-retry") // attempt 2
	assert.Equal(t, bootRecDeferred, e.bootRecoveryState)
	assert.Equal(t, 2, e.bootRecoveryAttempts)

	e.attemptBootRecovery(context.Background(), "manual-retry") // attempt 3: budget spent
	assert.Equal(t, bootRecExhausted, e.bootRecoveryState)
	assert.Equal(t, 3, e.bootRecoveryAttempts)

	// Terminal: a further re-entry must not run another round.
	e.attemptBootRecovery(context.Background(), "manual-retry")
	assert.Equal(t, bootRecExhausted, e.bootRecoveryState)
	assert.Equal(t, 3, e.bootRecoveryAttempts)
}

// --- backoff is the PRIMARY re-entry trigger --------------------------------

// TestBootRecovery_BackoffTimerIsPrimaryReentry proves a Deferred round
// re-enters on its OWN, from the scheduled backoff timer, with no external
// accelerator firing — using a near-instant fake clock so the test does not
// wait out the real 15s ladder.
func TestBootRecovery_BackoffTimerIsPrimaryReentry(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	mockCDP := mocks.NewMockCDP(ctrl)
	mockCDP.EXPECT().Initialized().Return(true).AnyTimes()

	var rounds int32
	mockCDP.EXPECT().
		NoLogSend(cdp.METHOD_EVALUATE, gomock.Any()).
		DoAndReturn(func(_ string, params map[string]interface{}) (interface{}, error) {
			expr, _ := params["expression"].(string)
			require.True(t, strings.Contains(expr, "__ffosPlayerStatus"))
			n := atomic.AddInt32(&rounds, 1)
			if n == 1 {
				// First round: still hydrating -> Deferred, backoff scheduled.
				return map[string]interface{}{
					"protocol": float64(1),
					"route":    statusRoutePlaylist, "handlerRegistered": true,
					"hasArtwork": false, "bootHydration": bootHydrationPending,
				}, nil
			}
			// Second round (fired by the backoff timer): hydration settled with
			// nothing to cast -> Succeeded, ending the loop.
			return map[string]interface{}{
				"protocol": float64(1),
				"route":    statusRoutePlaylist, "handlerRegistered": true,
				"hasArtwork": false, "bootHydration": bootHydrationOK,
			}, nil
		}).AnyTimes()

	e := settledExecutor(mockCDP)
	e.clock = &instantClock{}

	e.MaybeRecoverPlayerOnBootOnline(context.Background())
	awaitBootRecoveryState(t, e, bootRecSucceeded)
	assert.GreaterOrEqual(t, int(rounds), 2, "the backoff timer must have driven a second round on its own")
}

// TestBootRecovery_BackoffTimerExpiresWhenBootWindowClosed pins M3: the
// backoff timer's re-entry is EARLY re-entry same as every other trigger, so
// it must consult the boot-window probe too — a device that boots into its
// sleep window must not keep a 240s probe loop alive all night and
// NavigateHome a healthy page at the wake edge, hours past boot.
func TestBootRecovery_BackoffTimerExpiresWhenBootWindowClosed(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	mockCDP := mocks.NewMockCDP(ctrl)
	// No connection at the online transition: the first round defers via
	// "no-cdp-connection" without gating on the window (MaybeRecoverPlayerOnBootOnline's
	// inline attempt deliberately never gates), scheduling a backoff timer.
	mockCDP.EXPECT().Initialized().Return(false).AnyTimes()

	e := settledExecutor(mockCDP)
	e.clock = &instantClock{}
	e.bootLifecycleProbe = func() bool { return false } // window closed

	e.MaybeRecoverPlayerOnBootOnline(context.Background())
	assert.Equal(t, bootRecDeferred, e.bootRecoveryState, "the no-connection fast-fail must not gate on the window")

	// The instant-clock backoff timer fires immediately; its re-entry must
	// see the closed window and expire instead of attempting another round.
	awaitBootRecoveryState(t, e, bootRecExpired)
	assert.Equal(t, 0, e.bootRecoveryAttempts, "no attempt must have executed")
}

// instantClock is a wrapper.Clock whose SleepContext returns immediately
// (still respecting ctx cancellation), letting backoff-timer re-entry tests
// run without a real wall-clock wait.
type instantClock struct{}

func (instantClock) Now() time.Time      { return time.Now() }
func (instantClock) Sleep(time.Duration) {}
func (instantClock) SleepContext(ctx context.Context, _ time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		return nil
	}
}
func (instantClock) NewTicker(time.Duration) wrapper.Ticker { panic("unused") }

// --- park / CDP-connect completion -----------------------------------------

// TestBootRecovery_ParksThenCompletesOnCDPConnect: WAN confirmed before
// DevTools attached must PARK (row 1's fast-fail: Deferred, no attempt
// counted — nothing sent), and the first CDP connect completes it. Both
// Armed and Deferred are "parked" as far as CompletePendingBootPlayerRecovery
// is concerned; row 1's fast-fail landing on Deferred (not a bare Armed) is
// itself covered by TestBootRecovery_NoCDPConnectionDefersWithoutCountingAttempt.
func TestBootRecovery_ParksThenCompletesOnCDPConnect(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	mockCDP := mocks.NewMockCDP(ctrl)
	mockCDP.EXPECT().Initialized().Return(false) // at the online transition: not connected yet

	e := settledExecutor(mockCDP)
	e.MaybeRecoverPlayerOnBootOnline(context.Background())
	assert.Equal(t, bootRecDeferred, e.bootRecoveryState, "must park, not burn the boot's only attempt")
	assert.Equal(t, 0, e.bootRecoveryAttempts, "the no-connection fast-fail must not count as an attempt")

	mockCDP.EXPECT().Initialized().Return(true).AnyTimes()
	expectStructuredStatus(mockCDP, statusRoutePlaylist, true, false, bootHydrationOK)
	e.CompletePendingBootPlayerRecovery()
	assert.Equal(t, bootRecSucceeded, e.bootRecoveryState)
}

// TestBootRecovery_ParkedExpiresAtBootWindow: the CDP-connect completion
// gates on the boot-lifecycle probe — a connection arriving after the
// window closed means Chromium just started with the network already up.
func TestBootRecovery_ParkedExpiresAtBootWindow(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	mockCDP := mocks.NewMockCDP(ctrl)
	mockCDP.EXPECT().Initialized().Return(false)

	e := settledExecutor(mockCDP)
	e.bootLifecycleProbe = func() bool { return false } // window closed

	e.MaybeRecoverPlayerOnBootOnline(context.Background())
	assert.Equal(t, bootRecDeferred, e.bootRecoveryState)

	e.CompletePendingBootPlayerRecovery()
	assert.Equal(t, bootRecExpired, e.bootRecoveryState)
}

// TestBootRecovery_InlinePathIgnoresBootWindow pins the deliberate
// asymmetry: the INLINE attempt (CDP already connected at the online
// transition) never gates on the boot window — only the CDP-connect
// completion trigger does (see CompletePendingBootPlayerRecovery's doc).
func TestBootRecovery_InlinePathIgnoresBootWindow(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	mockCDP := mocks.NewMockCDP(ctrl)
	mockCDP.EXPECT().Initialized().Return(true).AnyTimes()
	expectStructuredStatus(mockCDP, statusRoutePlaylist, true, false, bootHydrationOK)

	e := settledExecutor(mockCDP)
	e.bootLifecycleProbe = func() bool { return false } // window long closed

	e.MaybeRecoverPlayerOnBootOnline(context.Background())
	assert.Equal(t, bootRecSucceeded, e.bootRecoveryState)
}

// --- accelerators ------------------------------------------------------------

// TestBootRecovery_RetryBootRecoveryEarlyAccelerator: a generation-bump or
// onAwake accelerator must re-enter a Deferred round early, and must be a
// no-op against Idle/Attempting/terminal states.
func TestBootRecovery_RetryBootRecoveryEarlyAccelerator(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	mockCDP := mocks.NewMockCDP(ctrl)
	mockCDP.EXPECT().Initialized().Return(true).AnyTimes()

	var second bool
	var mu sync.Mutex
	mockCDP.EXPECT().
		NoLogSend(cdp.METHOD_EVALUATE, gomock.Any()).
		DoAndReturn(func(_ string, params map[string]interface{}) (interface{}, error) {
			mu.Lock()
			isSecond := second
			second = true
			mu.Unlock()
			if !isSecond {
				return map[string]interface{}{
					"protocol": float64(1),
					"route":    statusRoutePlaylist, "handlerRegistered": true,
					"hasArtwork": false, "bootHydration": bootHydrationPending,
				}, nil
			}
			return map[string]interface{}{
				"protocol": float64(1),
				"route":    statusRoutePlaylist, "handlerRegistered": true,
				"hasArtwork": false, "bootHydration": bootHydrationOK,
			}, nil
		}).AnyTimes()

	e := settledExecutor(mockCDP)
	// No accelerator against Idle: RetryBootRecovery must not arm anything.
	e.retryBootRecoveryEarly()
	assert.Equal(t, bootRecIdle, e.bootRecoveryState)

	e.MaybeRecoverPlayerOnBootOnline(context.Background())
	require.Equal(t, bootRecDeferred, e.bootRecoveryState)

	e.retryBootRecoveryEarly() // the accelerator: must re-enter now, not wait 15s
	awaitBootRecoveryState(t, e, bootRecSucceeded)

	// No-op against a terminal state.
	e.retryBootRecoveryEarly()
	assert.Equal(t, bootRecSucceeded, e.bootRecoveryState)
}

// TestBootRecovery_RetryBootRecoveryEarlyAccelerator_ExpiresWhenBootWindowClosed
// pins the other M3 case: the accelerator (a generation bump or a confirmed
// wake edge, both wired via RetryBootRecovery) must also consult the
// boot-window probe on early re-entry, not just the CDP-connect completion
// path — the same "device slept through boot, don't NavigateHome a healthy
// page at the wake edge" hazard applies here too.
func TestBootRecovery_RetryBootRecoveryEarlyAccelerator_ExpiresWhenBootWindowClosed(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	mockCDP := mocks.NewMockCDP(ctrl)
	mockCDP.EXPECT().Initialized().Return(false) // parks Deferred at the online transition

	e := settledExecutor(mockCDP)
	e.MaybeRecoverPlayerOnBootOnline(context.Background())
	require.Equal(t, bootRecDeferred, e.bootRecoveryState)

	e.bootLifecycleProbe = func() bool { return false } // window closed before the accelerator fires
	e.retryBootRecoveryEarly()

	assert.Equal(t, bootRecExpired, e.bootRecoveryState)
	assert.Equal(t, 0, e.bootRecoveryAttempts, "no attempt must have executed")
}
