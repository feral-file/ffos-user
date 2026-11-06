package commandrouter_test

// A direct checkStatus reply (LAN /api/cast) must carry the device's
// current series-playback override at deviceSettings.seriesPlayback, not
// only in reply to an explicit getSeriesPlayback query (ffos-user#425
// follow-up) — pollPlayerStatus never runs for this path (see
// sendCDPRequest), same reason TestCheckStatusOverlayEgress exists for
// overlay. Also proves the controld-owned drop-then-set contract: a
// player-supplied (or spoofed) value under deviceSettings.seriesPlayback is
// dropped and replaced, and a reply whose deviceSettings sub-map is
// entirely absent still gets the override attached.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-controld/cdp"
	"github.com/feral-file/ffos-user/components/feral-controld/commandrouter"
	"github.com/feral-file/ffos-user/components/feral-controld/dp1"
	"github.com/feral-file/ffos-user/components/feral-controld/hub"
	"github.com/feral-file/ffos-user/components/feral-controld/mocks"
	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

func checkStatusReply(t *testing.T, reply map[string]any) map[string]any {
	t.Helper()
	ctrl := gomock.NewController(t)
	ctx := context.Background()
	player := mocks.NewMockCDP(ctrl)
	player.EXPECT().Send(cdp.METHOD_EVALUATE, gomock.Any()).Return(reply, nil)
	executor := newRoutableExecutor(ctrl)
	codec := wrapper.NewJSON()
	logger := zap.NewNop()
	router := commandrouter.New(executor, player, nil, nil, nil, nil, nil, nil, codec, logger)

	store := newSeriesPlaybackStore(t)
	commandrouter.SetSeriesPlayback(router, store, logger)
	store.Lock()
	mode := dp1.SeriesPlaybackFirst
	_, err := store.UpdateLocked(&mode)
	store.Unlock()
	require.NoError(t, err)

	mux := http.NewServeMux()
	server := wrapper.NewHTTPServer(&http.Server{Handler: mux, ReadHeaderTimeout: 0})
	hub.New(ctx, mocks.NewMockWS(ctrl), router, nil, nil, server, codec, logger)
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, lanCheckStatusRequest())
	require.Equal(t, http.StatusOK, response.Code)

	var decoded map[string]any
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &decoded))
	return decoded
}

func TestCheckStatusSeriesPlaybackEgress_AttachesTheCurrentOverride(t *testing.T) {
	decoded := checkStatusReply(t, map[string]any{"ok": true})

	settings, ok := decoded["deviceSettings"].(map[string]any)
	require.True(t, ok, "expected deviceSettings to be present: %v", decoded)
	require.Equal(t, "first", settings["seriesPlayback"])
}

// deviceSettings may be entirely absent from the player's reply (every one
// of its OTHER sub-fields also unset) — the override must still land.
func TestCheckStatusSeriesPlaybackEgress_AllocatesDeviceSettingsWhenAbsent(t *testing.T) {
	decoded := checkStatusReply(t, map[string]any{"ok": true})

	require.Contains(t, decoded, "deviceSettings")
}

// A player (or spoofed) reply claiming a seriesPlayback value must be
// dropped and replaced by the wired store's own answer, never trusted.
func TestCheckStatusSeriesPlaybackEgress_DropsPlayerSuppliedValue(t *testing.T) {
	decoded := checkStatusReply(t, map[string]any{
		"ok":             true,
		"deviceSettings": map[string]any{"seriesPlayback": "full", "scaling": "fit"},
	})

	settings := decoded["deviceSettings"].(map[string]any)
	require.Equal(t, "first", settings["seriesPlayback"], "must report the wired store's mode, not the player-supplied value")
	require.Equal(t, "fit", settings["scaling"], "other deviceSettings fields must survive untouched")
}

// Same two-location strip as TestCheckStatusOverlayEgress: a reply carrying
// the {messageID, message:{...}} envelope must have a spoofed value at
// BOTH the top-level deviceSettings and the message sub-map's dropped, with
// only the resolved target (message's) getting the real value set.
func TestCheckStatusSeriesPlaybackEgress_DropsSpoofedValueAtBothEnvelopeLocations(t *testing.T) {
	decoded := checkStatusReply(t, map[string]any{
		"messageID":      "player-request",
		"deviceSettings": map[string]any{"seriesPlayback": "random"},
		"message": map[string]any{
			"ok":             true,
			"deviceSettings": map[string]any{"seriesPlayback": "full"},
		},
	})

	require.Equal(t, "player-request", decoded["messageID"])

	topSettings, ok := decoded["deviceSettings"].(map[string]any)
	require.True(t, ok)
	require.NotContains(t, topSettings, "seriesPlayback", "the spoofed top-level value must be dropped")

	message, ok := decoded["message"].(map[string]any)
	require.True(t, ok)
	settings, ok := message["deviceSettings"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "first", settings["seriesPlayback"], "the message sub-map is the resolved target and must carry the wired store's mode")
}
