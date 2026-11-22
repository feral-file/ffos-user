package devicectl

import (
	"context"
	"errors"
	"testing"

	"github.com/golang/mock/gomock"
	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-controld/mocks"
)

// TestRefreshArtworkAfterReplayRestore_SendsOneRefresh: once the player's
// command handler is up, exactly one refreshArtwork evaluate goes out.
func TestRefreshArtworkAfterReplayRestore_SendsOneRefresh(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	mockCDP := mocks.NewMockCDP(ctrl)
	expectHandlerReadyProbe(mockCDP)
	expectRefreshEvaluate(t, mockCDP, playerACK(), nil).Times(1)

	RefreshArtworkAfterReplayRestore(context.Background(), settledExecutor(mockCDP), zap.NewNop())
}

// TestRefreshArtworkAfterReplayRestore_RefusalAndErrorAreLeftAlone: a
// refusal or a transport error ends it. No second send, no navigate (the
// executor has no session wired; an escalation attempt would have nothing
// to call and the single Times(1) below would be exceeded by a retry).
func TestRefreshArtworkAfterReplayRestore_RefusalAndErrorAreLeftAlone(t *testing.T) {
	for name, reply := range map[string]struct {
		result interface{}
		err    error
	}{
		"refused":         {playerRefusal("no artwork"), nil},
		"transport error": {nil, errors.New("cdp down")},
	} {
		t.Run(name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()
			mockCDP := mocks.NewMockCDP(ctrl)
			expectHandlerReadyProbe(mockCDP)
			expectRefreshEvaluate(t, mockCDP, reply.result, reply.err).Times(1)

			RefreshArtworkAfterReplayRestore(context.Background(), settledExecutor(mockCDP), zap.NewNop())
		})
	}
}

// TestRefreshArtworkAfterReplayRestore_PastTheWindowSendsNothing: with the
// context done, no refresh is sent, whether the player never became ready
// or its readiness answer only arrived after the window closed.
func TestRefreshArtworkAfterReplayRestore_PastTheWindowSendsNothing(t *testing.T) {
	for name, ready := range map[string]bool{"never ready": false, "ready too late": true} {
		t.Run(name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()
			mockCDP := mocks.NewMockCDP(ctrl)
			installCDPDispatch(mockCDP, cdpDispatch{handlerReady: func() (interface{}, error) {
				return map[string]interface{}{"ready": ready}, nil
			}})

			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			RefreshArtworkAfterReplayRestore(ctx, settledExecutor(mockCDP), zap.NewNop())
		})
	}
}
