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
