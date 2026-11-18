package commandrouter_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-controld/commandrouter"
	"github.com/feral-file/ffos-user/components/feral-controld/dp1"
	"github.com/feral-file/ffos-user/components/feral-controld/hub"
	"github.com/feral-file/ffos-user/components/feral-controld/mediator"
	"github.com/feral-file/ffos-user/components/feral-controld/mocks"
	"github.com/feral-file/ffos-user/components/feral-controld/playerlink"
	"github.com/feral-file/ffos-user/components/feral-controld/relayer"
	"github.com/feral-file/ffos-user/components/feral-controld/status"
	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

// Exercise both actual transport adapters. In particular the legacy relayer
// must send failure replies rather than silently dropping raw transport errors.
func nativeReplyThroughTransport(t *testing.T, ts *testSetup, transport, command string) map[string]any {
	t.Helper()
	var encoded []byte
	codec := wrapper.NewJSON()
	ctx := context.Background()
	logger := zap.NewNop()
	if transport == "hub" {
		mux := http.NewServeMux()
		server := wrapper.NewHTTPServer(&http.Server{Handler: mux, ReadHeaderTimeout: time.Second})
		hub.New(ctx, mocks.NewMockWS(ts.ctrl), ts.handler, nil, nil, server, codec, logger)
		request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:1111/api/cast", strings.NewReader(`{"command":"`+command+`","request":{"mode":"one"}}`))
		request.RemoteAddr = "192.168.1.20:40000"
		response := httptest.NewRecorder()
		mux.ServeHTTP(response, request)
		require.Equal(t, http.StatusOK, response.Code)
		encoded = response.Body.Bytes()
	} else {
		remote := mocks.NewMockRelayer(ts.ctrl)
		bus := mocks.NewMockDBus(ts.ctrl)
		bus.EXPECT().OnBusSignal(gomock.Any())
		var receive relayer.Handler
		remote.EXPECT().OnRelayerMessage(gomock.Any()).Do(func(h relayer.Handler) { receive = h })
		remote.EXPECT().Send(ctx, gomock.Any()).DoAndReturn(func(_ context.Context, response interface{}) error {
			payload := response.(relayer.Response)
			require.Equal(t, "controller-request", payload.MessageID)
			var err error
			encoded, err = json.Marshal(payload.Message)
			return err
		}).Times(1)
		med := mediator.New(ctx, remote, bus, ts.handler, ts.mockExecutor, nil, codec, logger)
		med.Start()
		require.NoError(t, receive(ctx, relayer.Payload{MessageID: "controller-request", Message: relayer.Message{Command: &command, Request: map[string]any{"mode": "one"}}}))
	}
	var decoded map[string]any
	require.NoError(t, json.Unmarshal(encoded, &decoded))
	return decoded["message"].(map[string]any)
}

func TestNativeStatusEgress(t *testing.T) {
	for _, transport := range []string{"hub", "relayer"} {
		for _, loading := range []bool{false, true} {
			t.Run(transport+map[bool]string{false: "/shown", true: "/loading"}[loading], func(t *testing.T) {
				ts := setup(t)
				snapshot := controlSnapshot()
				key := "71f1273d-4d1d-4a9a-b487-41a4a2275b03"
				snapshot.Selected.ID = key
				snapshot.Presented.ID = key
				snapshot.Presented.Display = playerlink.Display{Scaling: "fit", Margin: json.RawMessage(`"10%"`), Background: "#112233"}
				snapshot.Presented.DisplayRevision = 1
				snapshot.Composition = &playerlink.Composition{Occurrence: key, Revision: 7, DisplayRevision: 1, Display: snapshot.Presented.Display}
				snapshot.Settings = playerlink.Settings{Framing: "artwork", Tombstone: "on"}
				if loading {
					snapshot.Generation = 3
					snapshot.Selected.ID = "incoming"
					snapshot.Selected.Generation = 3
					snapshot.Selected.Stage = "loading"
					snapshot.Selected.Index = 1
				}
				p := &controlPlayer{state: snapshot}
				commandrouter.SetNativeControls(ts.handler, p)
				commandrouter.SetOverlaySource(ts.handler, func() *status.Overlay { return &status.Overlay{Owner: "setup", State: "claim_qr"} }, ts.logger)
				store := newSeriesPlaybackStore(t)
				commandrouter.SetSeriesPlayback(ts.handler, store, ts.logger)
				store.Lock()
				mode := dp1.SeriesPlaybackFirst
				_, err := store.UpdateLocked(&mode)
				store.Unlock()
				require.NoError(t, err)
				reply := nativeReplyThroughTransport(t, ts, transport, "checkStatus")
				require.Equal(t, true, reply["ok"])
				settings := reply["deviceSettings"].(map[string]any)
				require.Equal(t, key, settings["showingKey"])
				require.Equal(t, "10%", settings["margin"])
				require.Equal(t, float64(7), settings["compositionRevision"])
				require.Equal(t, "first", settings["seriesPlayback"])
				require.Equal(t, "on", settings["tombstone"])
				require.Equal(t, "claim_qr", reply["overlay"].(map[string]any)["state"])
				require.NotContains(t, reply, "outcomes")
				require.NotContains(t, reply, "skippedIndices")
				require.NotContains(t, reply, "signatureStatus")
				require.Zero(t, p.calls)
				runtime := reply["runtime"].(map[string]any)
				require.Equal(t, key, runtime["presented"].(map[string]any)["id"])
				if loading {
					require.Equal(t, "incoming", runtime["selected"].(map[string]any)["id"])
				}
			})
		}
	}
}
func TestNativeStatusAndControlFailuresReachSenders(t *testing.T) {
	for _, transport := range []string{"hub", "relayer"} {
		for _, command := range []string{"checkStatus", "setLoop"} {
			t.Run(transport+"/"+command, func(t *testing.T) {
				ts := setup(t)
				p := &controlPlayer{state: controlSnapshot()}
				code := "unavailable"
				if command == "checkStatus" {
					p.snapshotErr = errors.New("host stopped")
				} else {
					p.err = errors.New("lost mutation reply")
					code = "outcome_unknown"
				}
				commandrouter.SetNativeControls(ts.handler, p)
				reply := nativeReplyThroughTransport(t, ts, transport, command)
				require.Equal(t, false, reply["ok"])
				require.Equal(t, code, reply["code"])
			})
		}
	}
}
