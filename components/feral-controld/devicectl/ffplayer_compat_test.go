package devicectl

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/feral-file/ffos-user/components/feral-controld/ffplayerfixtures"
	"github.com/feral-file/ffos-user/components/feral-controld/mocks"
)

// decodeFixtureMap simulates the cdp client's decode of a Runtime.evaluate
// result that is itself a JSON string — see ffplayerfixtures' package doc
// and installCDPDispatch's own status closures, which already model
// "evaluate results arrive already unmarshaled" for every OTHER test in this
// file. Returns the `message` object specifically, matching what
// window.__ffosPlayerStatus() itself returns unwrapped (it has no
// {messageID, message} envelope — only the checkStatus/refreshArtwork
// command replies do).
func decodeFixtureMap(t *testing.T, raw string) map[string]interface{} {
	t.Helper()
	var decoded map[string]interface{}
	if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
		t.Fatalf("fixture is not valid JSON: %v\nraw: %s", err, raw)
	}
	return decoded
}

// TestFFPlayerCompat_DecodePlayerStatus drives the LITERAL
// window.__ffosPlayerStatus payloads the shipped ff-player produces
// (ffplayerfixtures.PlayerStatus*) through decodePlayerStatus — the exact
// boundary decode the boot-recovery classifier (§5.2 of
// docs/player-session-recovery.md) depends on for every row keyed on route/
// handlerRegistered/hasArtwork/bootHydration.
func TestFFPlayerCompat_DecodePlayerStatus(t *testing.T) {
	cases := []struct {
		name              string
		raw               string
		wantOK            bool
		wantRoute         string
		wantHandlerReg    bool
		wantHasArtwork    bool
		wantBootHydration string
	}{
		{
			name: "bootHydration=pending", raw: ffplayerfixtures.PlayerStatusBootHydrationPending,
			wantOK: true, wantRoute: statusRoutePlaylist, wantHandlerReg: true, wantHasArtwork: false,
			wantBootHydration: bootHydrationPending,
		},
		{
			name: "bootHydration=ok", raw: ffplayerfixtures.PlayerStatusBootHydrationOK,
			wantOK: true, wantRoute: statusRoutePlaylist, wantHandlerReg: true, wantHasArtwork: true,
			wantBootHydration: bootHydrationOK,
		},
		{
			name: "bootHydration=halted_cleared", raw: ffplayerfixtures.PlayerStatusBootHydrationHaltedCleared,
			wantOK: true, wantRoute: statusRoutePlaylist, wantHandlerReg: true, wantHasArtwork: false,
			wantBootHydration: bootHydrationHaltedCleared,
		},
		{
			name: "bootHydration=halted_preserving", raw: ffplayerfixtures.PlayerStatusBootHydrationHaltedPreserving,
			wantOK: true, wantRoute: statusRouteSleep, wantHandlerReg: true, wantHasArtwork: true,
			wantBootHydration: bootHydrationHaltedPreserving,
		},
		{
			name: "bootHydration=failed", raw: ffplayerfixtures.PlayerStatusBootHydrationFailed,
			wantOK: true, wantRoute: statusRoutePlaylist, wantHandlerReg: false, wantHasArtwork: false,
			wantBootHydration: bootHydrationFailed,
		},
		{
			name: "route=/error", raw: ffplayerfixtures.PlayerStatusRouteError,
			wantOK: true, wantRoute: statusRouteError, wantHandlerReg: true, wantHasArtwork: true,
			wantBootHydration: bootHydrationOK,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := decodeFixtureMap(t, tc.raw)
			st, ok := decodePlayerStatus(m, true)
			require.Equal(t, tc.wantOK, ok)
			assert.Equal(t, tc.wantRoute, st.Route)
			assert.Equal(t, tc.wantHandlerReg, st.HandlerRegistered)
			assert.Equal(t, tc.wantHasArtwork, st.HasArtwork)
			assert.Equal(t, tc.wantBootHydration, st.BootHydration)
		})
	}

	t.Run("protocol mismatch decodes as unavailable", func(t *testing.T) {
		m := decodeFixtureMap(t, ffplayerfixtures.PlayerStatusProtocolMismatchRouteError)
		_, ok := decodePlayerStatus(m, true)
		assert.False(t, ok, "a protocol this classifier doesn't understand must decode as unavailable, never misread")
	})
}

// TestFFPlayerCompat_BootRecoveryClassification drives the SAME literal
// fixtures end to end through the real boot recovery state machine
// (runBootRecoveryRound via MaybeRecoverPlayerOnBootOnline), asserting the
// exact classification docs/player-session-recovery.md §5.2 documents —
// this is "the classifications the state machine depends on", not just the
// decode step in isolation.
func TestFFPlayerCompat_BootRecoveryClassification(t *testing.T) {
	t.Run("bootHydration=pending defers, no attempt", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		defer ctrl.Finish()
		mockCDP := mocks.NewMockCDP(ctrl)
		mockCDP.EXPECT().Initialized().Return(true).AnyTimes()
		installFFPlayerFixtureStatus(mockCDP, ffplayerfixtures.PlayerStatusBootHydrationPending)

		e := settledExecutor(mockCDP)
		e.MaybeRecoverPlayerOnBootOnline(context.Background())

		assert.Equal(t, bootRecDeferred, e.bootRecoveryState)
		assert.Equal(t, 0, e.bootRecoveryAttempts)
	})

	t.Run("bootHydration=ok with artwork falls through to a live refresh ACK: succeeds", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		defer ctrl.Finish()
		mockCDP := mocks.NewMockCDP(ctrl)
		mockCDP.EXPECT().Initialized().Return(true).AnyTimes()
		installCDPDispatch(mockCDP, cdpDispatch{
			status: func() (interface{}, error) {
				return decodeFixtureMap(t, ffplayerfixtures.PlayerStatusBootHydrationOK), nil
			},
		})
		expectRefreshEvaluate(t, mockCDP, map[string]interface{}{"message": map[string]interface{}{"ok": true}}, nil).Times(1)

		e := settledExecutor(mockCDP)
		e.MaybeRecoverPlayerOnBootOnline(context.Background())

		assert.Equal(t, bootRecSucceeded, e.bootRecoveryState)
		assert.Equal(t, 1, e.bootRecoveryAttempts)
	})

	t.Run("bootHydration=halted_cleared succeeds immediately, no attempt", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		defer ctrl.Finish()
		mockCDP := mocks.NewMockCDP(ctrl)
		mockCDP.EXPECT().Initialized().Return(true).AnyTimes()
		installFFPlayerFixtureStatus(mockCDP, ffplayerfixtures.PlayerStatusBootHydrationHaltedCleared)

		e := settledExecutor(mockCDP)
		e.MaybeRecoverPlayerOnBootOnline(context.Background())

		assert.Equal(t, bootRecSucceeded, e.bootRecoveryState)
		assert.Equal(t, 0, e.bootRecoveryAttempts)
	})

	t.Run("bootHydration=halted_preserving on /sleep defers by route, no attempt", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		defer ctrl.Finish()
		mockCDP := mocks.NewMockCDP(ctrl)
		mockCDP.EXPECT().Initialized().Return(true).AnyTimes()
		installFFPlayerFixtureStatus(mockCDP, ffplayerfixtures.PlayerStatusBootHydrationHaltedPreserving)

		e := settledExecutor(mockCDP)
		e.MaybeRecoverPlayerOnBootOnline(context.Background())

		assert.Equal(t, bootRecDeferred, e.bootRecoveryState)
		assert.Equal(t, 0, e.bootRecoveryAttempts)
	})

	t.Run("bootHydration=failed escalates to NavigateHome", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		defer ctrl.Finish()
		mockCDP := mocks.NewMockCDP(ctrl)
		mockCDP.EXPECT().Initialized().Return(true).AnyTimes()
		installFFPlayerFixtureStatus(mockCDP, ffplayerfixtures.PlayerStatusBootHydrationFailed)

		e := settledExecutor(mockCDP)
		sess := &fakeBootRecoverySession{}
		e.bootRecoverySession = sess
		e.MaybeRecoverPlayerOnBootOnline(context.Background())

		assert.Equal(t, 1, sess.callCount(), "bootHydration=failed must escalate to NavigateHome")
		assert.Equal(t, bootRecSucceeded, e.bootRecoveryState) // fakeBootRecoverySession resolves NavExecuted+nil by default
	})

	t.Run("route=/error expires and never navigates", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		defer ctrl.Finish()
		mockCDP := mocks.NewMockCDP(ctrl)
		mockCDP.EXPECT().Initialized().Return(true).AnyTimes()
		installFFPlayerFixtureStatus(mockCDP, ffplayerfixtures.PlayerStatusRouteError)

		e := settledExecutor(mockCDP)
		sess := &fakeBootRecoverySession{}
		e.bootRecoverySession = sess
		e.MaybeRecoverPlayerOnBootOnline(context.Background())

		assert.Equal(t, bootRecExpired, e.bootRecoveryState)
		assert.Equal(t, 0, sess.callCount(), "must never navigate over /error")
	})

	t.Run("protocol mismatch with route=/error still expires and never navigates", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		defer ctrl.Finish()
		mockCDP := mocks.NewMockCDP(ctrl)
		mockCDP.EXPECT().Initialized().Return(true).AnyTimes()
		installFFPlayerFixtureStatus(mockCDP, ffplayerfixtures.PlayerStatusProtocolMismatchRouteError)

		e := settledExecutor(mockCDP)
		sess := &fakeBootRecoverySession{}
		e.bootRecoverySession = sess
		e.MaybeRecoverPlayerOnBootOnline(context.Background())

		assert.Equal(t, bootRecExpired, e.bootRecoveryState, "the /error safety check must hold even against an undecodable protocol")
		assert.Equal(t, 0, sess.callCount())
	})
}

// installFFPlayerFixtureStatus wires a cdpDispatch whose __ffosPlayerStatus
// probe answers with the literal fixture JSON (decoded once, the same way
// the real cdp client would).
func installFFPlayerFixtureStatus(mockCDP *mocks.MockCDP, raw string) {
	var decoded map[string]interface{}
	_ = json.Unmarshal([]byte(raw), &decoded)
	installCDPDispatch(mockCDP, cdpDispatch{
		status: func() (interface{}, error) { return decoded, nil },
	})
}
