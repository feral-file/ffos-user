package commandrouter_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-controld/cdp"
	"github.com/feral-file/ffos-user/components/feral-controld/commandrouter"
	"github.com/feral-file/ffos-user/components/feral-controld/hub"
	"github.com/feral-file/ffos-user/components/feral-controld/mediator"
	"github.com/feral-file/ffos-user/components/feral-controld/mintpairing"
	"github.com/feral-file/ffos-user/components/feral-controld/mocks"
	"github.com/feral-file/ffos-user/components/feral-controld/relayer"
	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

// Exercise both real transport adapters with the real router. A mocked router
// would hide the direct-reply bypass that notification-only coverage missed.
func TestCheckStatusShowingKeyEgress(t *testing.T) {
	const validKey = "71f1273d-4d1d-4a9a-b487-41a4a2275b03"
	for _, transport := range []string{"hub", "relayer"} {
		for _, wrapped := range []bool{false, true} {
			for _, tc := range []struct {
				name string
				key  interface{}
				keep bool
			}{
				{"canonical", validKey, true},
				{"legacy source", "0|work-a|https://example.test/art?signature=private-token", false},
				{"uppercase", strings.ToUpper(validKey), false},
				{"braced", "{" + validKey + "}", false},
				{"object", map[string]any{"source": "private-token"}, false},
				{"null", nil, false},
			} {
				t.Run(transport+"/"+tc.name+map[bool]string{true: "/wrapped", false: "/bare"}[wrapped], func(t *testing.T) {
					ctrl := gomock.NewController(t)
					ctx := context.Background()
					settings := map[string]any{"showingKey": tc.key, "margin": "10%", "compositionRevision": 7, "futureField": true}
					reply := map[string]any{"ok": true, "deviceSettings": settings, "futureStatus": "preserved"}
					if wrapped {
						reply = map[string]any{"messageID": "player-request", "message": reply}
					}
					player := mocks.NewMockCDP(ctrl)
					player.EXPECT().Send(cdp.METHOD_EVALUATE, gomock.Any()).Return(reply, nil)
					executor := newRoutableExecutor(ctrl)
					codec := wrapper.NewJSON()
					logger := zap.NewNop()
					router := commandrouter.New(executor, player, nil, nil, nil, nil, nil, nil, codec, logger)
					var encoded []byte
					if transport == "hub" {
						mux := http.NewServeMux()
						server := wrapper.NewHTTPServer(&http.Server{Handler: mux, ReadHeaderTimeout: time.Second})
						hub.New(ctx, mocks.NewMockWS(ctrl), router, nil, nil, server, codec, logger)
						response := httptest.NewRecorder()
						mux.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/cast", strings.NewReader(`{"command":"checkStatus","request":{}}`)))
						require.Equal(t, http.StatusOK, response.Code)
						encoded = response.Body.Bytes()
					} else {
						remote := mocks.NewMockRelayer(ctrl)
						bus := mocks.NewMockDBus(ctrl)
						bus.EXPECT().OnBusSignal(gomock.Any())
						var receive relayer.Handler
						remote.EXPECT().OnRelayerMessage(gomock.Any()).Do(func(h relayer.Handler) { receive = h })
						remote.EXPECT().Send(ctx, gomock.Any()).DoAndReturn(func(_ context.Context, response interface{}) error {
							payload := response.(relayer.Response)
							require.Equal(t, "controller-request", payload.MessageID)
							var err error
							encoded, err = json.Marshal(payload.Message)
							return err
						})
						med := mediator.New(ctx, remote, bus, player, router, executor, nil, codec, logger)
						med.Start()
						command := "checkStatus"
						require.NoError(t, receive(ctx, relayer.Payload{MessageID: "controller-request", Message: relayer.Message{Command: &command}}))
					}
					var decoded map[string]any
					require.NoError(t, json.Unmarshal(encoded, &decoded))
					if wrapped {
						require.Equal(t, "player-request", decoded["messageID"])
						decoded = decoded["message"].(map[string]any)
					}
					require.Equal(t, "preserved", decoded["futureStatus"])
					got := decoded["deviceSettings"].(map[string]any)
					require.Equal(t, "10%", got["margin"])
					require.Equal(t, float64(7), got["compositionRevision"])
					require.Equal(t, true, got["futureField"])
					if tc.keep {
						require.Equal(t, validKey, got["showingKey"])
					} else {
						require.NotContains(t, got, "showingKey")
						require.NotContains(t, string(encoded), "private-token")
					}
				})
			}
		}
	}
}

// TestCheckStatusMintPairingEgress is the regression coverage #388's round-3
// review named: a direct checkStatus reply (LAN /api/cast or relayer) must
// carry mintPairing from the wired mint pairing service, not only the
// poller's pushed player_status notifications (pollPlayerStatus never runs
// for this path — see sendCDPRequest). Also proves the controld-owned
// drop-then-set contract on BOTH reply shapes a player can send (round-4
// review: the bare and {messageID, message:{...}}-enveloped shapes each
// strip any player-supplied mintPairing, including — the enveloped shape's
// own failure mode — a spoofed key at the top level alongside a real
// "message" sub-map, same as TestCheckStatusShowingKeyEgress covers for
// showingKey).
func TestCheckStatusMintPairingEgress(t *testing.T) {
	expiresAt := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, transport := range []string{"hub", "relayer"} {
		for _, wrapped := range []bool{false, true} {
			for _, tc := range []struct {
				name     string
				overlay  mintpairing.OverlayStatus
				wantKey  bool
				wantJSON map[string]any
			}{
				{
					name:    "pairing code showing",
					overlay: mintpairing.OverlayStatus{Showing: true, State: "pairing_code", ChannelID: "chan-1", PairingCode: "123456", ExpiresAt: expiresAt},
					wantKey: true,
					wantJSON: map[string]any{
						"state":       "pairing_code",
						"channelId":   "chan-1",
						"pairingCode": "123456",
						"expiresAt":   expiresAt.Format(time.RFC3339),
					},
				},
				{
					name:    "request received, no code left to report",
					overlay: mintpairing.OverlayStatus{Showing: true, State: "request_received", ChannelID: "chan-1"},
					wantKey: true,
					wantJSON: map[string]any{
						"state":     "request_received",
						"channelId": "chan-1",
					},
				},
				{
					name:    "creating token, no code left to report",
					overlay: mintpairing.OverlayStatus{Showing: true, State: "creating_token", ChannelID: "chan-1"},
					wantKey: true,
					wantJSON: map[string]any{
						"state":     "creating_token",
						"channelId": "chan-1",
					},
				},
				{
					name:    "hidden",
					overlay: mintpairing.OverlayStatus{},
					wantKey: false,
				},
			} {
				t.Run(transport+"/"+tc.name+map[bool]string{true: "/wrapped", false: "/bare"}[wrapped], func(t *testing.T) {
					ctrl := gomock.NewController(t)
					ctx := context.Background()
					// A player (or spoofed) reply claiming mintPairing must be
					// dropped, never trusted — this field is controld-owned —
					// at BOTH locations an enveloped reply can carry it, which
					// is what distinguishes this from a single-location strip.
					spoofed := map[string]any{"state": "pairing_code"}
					reply := map[string]any{"ok": true, "mintPairing": spoofed}
					if wrapped {
						reply = map[string]any{"messageID": "player-request", "message": reply, "mintPairing": spoofed}
					}
					player := mocks.NewMockCDP(ctrl)
					player.EXPECT().Send(cdp.METHOD_EVALUATE, gomock.Any()).Return(reply, nil)
					executor := newRoutableExecutor(ctrl)
					mintSvc := &fakeMintPairingService{overlayStatus: tc.overlay}
					codec := wrapper.NewJSON()
					logger := zap.NewNop()
					router := commandrouter.New(executor, player, nil, nil, mintSvc, nil, nil, nil, codec, logger)
					var encoded []byte
					if transport == "hub" {
						mux := http.NewServeMux()
						server := wrapper.NewHTTPServer(&http.Server{Handler: mux, ReadHeaderTimeout: time.Second})
						hub.New(ctx, mocks.NewMockWS(ctrl), router, nil, nil, server, codec, logger)
						response := httptest.NewRecorder()
						mux.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/cast", strings.NewReader(`{"command":"checkStatus","request":{}}`)))
						require.Equal(t, http.StatusOK, response.Code)
						encoded = response.Body.Bytes()
					} else {
						remote := mocks.NewMockRelayer(ctrl)
						bus := mocks.NewMockDBus(ctrl)
						bus.EXPECT().OnBusSignal(gomock.Any())
						var receive relayer.Handler
						remote.EXPECT().OnRelayerMessage(gomock.Any()).Do(func(h relayer.Handler) { receive = h })
						remote.EXPECT().Send(ctx, gomock.Any()).DoAndReturn(func(_ context.Context, response interface{}) error {
							payload := response.(relayer.Response)
							require.Equal(t, "controller-request", payload.MessageID)
							var err error
							encoded, err = json.Marshal(payload.Message)
							return err
						})
						med := mediator.New(ctx, remote, bus, player, router, executor, nil, codec, logger)
						med.Start()
						command := "checkStatus"
						require.NoError(t, receive(ctx, relayer.Payload{MessageID: "controller-request", Message: relayer.Message{Command: &command}}))
					}
					var decoded map[string]any
					require.NoError(t, json.Unmarshal(encoded, &decoded))
					if wrapped {
						require.NotContains(t, decoded, "mintPairing", "spoofed top-level key must be dropped on an enveloped reply")
						decoded = decoded["message"].(map[string]any)
					}
					if !tc.wantKey {
						require.NotContains(t, decoded, "mintPairing")
						return
					}
					require.Equal(t, tc.wantJSON, decoded["mintPairing"])
				})
			}
		}
	}
}
