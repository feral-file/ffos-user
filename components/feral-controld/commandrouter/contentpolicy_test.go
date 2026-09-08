package commandrouter_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"

	"github.com/feral-file/ffos-user/components/feral-controld/cdp"
	"github.com/feral-file/ffos-user/components/feral-controld/commandrouter"
	"github.com/feral-file/ffos-user/components/feral-controld/commands"
	"github.com/feral-file/ffos-user/components/feral-controld/contentpolicy"
	"github.com/feral-file/ffos-user/components/feral-controld/mocks"
	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

func newPolicyHandler(t *testing.T) (commandrouter.Handler, *mocks.MockCDP, *contentpolicy.Store) {
	t.Helper()
	ctrl := gomock.NewController(t)
	ex := newRoutableExecutor(ctrl)
	player := mocks.NewMockCDP(ctrl)
	store, err := contentpolicy.Open(filepath.Join(t.TempDir(), "policy.json"), false)
	require.NoError(t, err)
	h := commandrouter.New(ex, player, mocks.NewMockDP1(ctrl), mocks.NewMockStatusPoller(ctrl), nil, nil, nil, nil, wrapper.NewJSON(), zaptest.NewLogger(t))
	commandrouter.SetContentPolicy(h, store, zaptest.NewLogger(t))
	return h, player, store
}

func TestSetContentPolicyAwaitsMatchingPlayerAck(t *testing.T) {
	h, player, _ := newPolicyHandler(t)
	player.EXPECT().Send(cdp.METHOD_EVALUATE, gomock.Any()).DoAndReturn(func(_ string, params map[string]interface{}) (interface{}, error) {
		require.Equal(t, true, params["awaitPromise"])
		require.Equal(t, true, params["returnByValue"])
		return map[string]interface{}{"message": map[string]interface{}{
			"ok": true, "active": true,
			"contentPolicy": map[string]interface{}{"version": float64(1), "showMatureContent": true, "strictPersonal": false, "blockUnratedCurated": false},
		}}, nil
	})
	result, err := h.Process(context.Background(), commands.Command{Type: commands.CMD_SET_CONTENT_POLICY, Arguments: map[string]any{"showMatureContent": true, "strictPersonal": false}})
	require.NoError(t, err)
	require.Equal(t, true, result.(map[string]interface{})["active"])
}

func TestDisplayPlaylistAllBlockedDoesNotReachPlayer(t *testing.T) {
	h, _, _ := newPolicyHandler(t)
	_, err := h.Process(context.Background(), commands.Command{Type: commands.CMD_DISPLAY_PLAYLIST, Arguments: map[string]any{
		"dp1_call": map[string]interface{}{"dpVersion": "1.1.0", "title": "x", "items": []interface{}{map[string]interface{}{"source": "https://a", "contentRating": "mature"}}},
	}})
	require.Error(t, err)
	require.True(t, commandrouter.IsContentBlocked(err))
}

func TestSetContentPolicyCannotWriteAuditGate(t *testing.T) {
	h, _, _ := newPolicyHandler(t)
	result, err := h.Process(context.Background(), commands.Command{Type: commands.CMD_SET_CONTENT_POLICY, Arguments: map[string]any{
		"showMatureContent": false, "strictPersonal": false, "blockUnratedCurated": true,
	}})
	require.NoError(t, err)
	require.Equal(t, "invalidRequest", result.(map[string]interface{})["error"])
}

func TestDisplayPlaylistRejectsMalformedPresentRating(t *testing.T) {
	h, _, _ := newPolicyHandler(t)
	_, err := h.Process(context.Background(), commands.Command{Type: commands.CMD_DISPLAY_PLAYLIST, Arguments: map[string]any{
		"dp1_call": map[string]interface{}{"items": []interface{}{map[string]interface{}{"source": "https://a", "contentRating": nil}}},
	}})
	require.ErrorContains(t, err, "playlistInvalid")
}
