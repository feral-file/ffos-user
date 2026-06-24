package qrdisplay

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/feral-file/ffos-user/components/feral-controld/cdp"
)

func TestShowPairingCode_RejectsMalformedEvaluationResults(t *testing.T) {
	tests := []struct {
		name    string
		result  any
		wantErr string
	}{
		{
			name:    "missing ok",
			result:  map[string]any{"error": "missing handler status"},
			wantErr: "missing ok",
		},
		{
			name:    "nil result",
			result:  nil,
			wantErr: "unsupported result type <nil>",
		},
		{
			name:    "non map result",
			result:  "ok",
			wantErr: "unsupported result type string",
		},
		{
			name:    "malformed wrapped result",
			result:  map[string]any{"result": "not-an-object"},
			wantErr: "malformed Runtime.evaluate result",
		},
		{
			name:    "exception details",
			result:  map[string]any{"exceptionDetails": map[string]any{"text": "boom"}},
			wantErr: "evaluation raised exception",
		},
		{
			name:    "wrapped application response missing ok",
			result:  map[string]any{"result": map[string]any{"value": `{"error":"missing ok"}`}},
			wantErr: "missing ok",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ShowPairingCode(context.Background(), &fakeCDP{result: tt.result}, "PAIR-123")
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func TestShowPairingCode_RequiresApplicationOK(t *testing.T) {
	err := ShowPairingCode(context.Background(), &fakeCDP{result: map[string]any{"ok": false, "error": "overlay unavailable"}}, "PAIR-123")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "rejected request")
}

func TestShowPairingCode_AcceptsMessageWrappedApplicationOK(t *testing.T) {
	result := map[string]any{
		"message": map[string]any{"ok": true},
	}

	err := ShowPairingCode(context.Background(), &fakeCDP{result: result}, "PAIR-123")

	require.NoError(t, err)
}

func TestMintPairingDisplayCommands_EmitPlayerContractStates(t *testing.T) {
	tests := []struct {
		name      string
		send      func(*fakeCDP) error
		wantState string
	}{
		{
			name: "pairing code",
			send: func(c *fakeCDP) error {
				return ShowPairingCode(context.Background(), c, "PAIR-123")
			},
			wantState: "pairing_code",
		},
		{
			name: "request received",
			send: func(c *fakeCDP) error {
				return ShowRequestReceived(context.Background(), c, "Chrome")
			},
			wantState: "request_received",
		},
		{
			name: "creating token",
			send: func(c *fakeCDP) error {
				return ShowCreatingToken(context.Background(), c, "Chrome")
			},
			wantState: "creating_token",
		},
		{
			name: "hidden",
			send: func(c *fakeCDP) error {
				return ShowDefaultDisplay(context.Background(), c)
			},
			wantState: "hidden",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cdpClient := &fakeCDP{result: runtimeEvaluateStringResult(`{"ok":true}`)}

			require.NoError(t, tt.send(cdpClient))

			request := requireMintPairingDisplayRequest(t, cdpClient.params)
			assert.Equal(t, tt.wantState, request["state"])
		})
	}
}

type fakeCDP struct {
	result any
	err    error
	params map[string]interface{}
}

func (f *fakeCDP) Init(context.Context) error { return nil }

func (f *fakeCDP) Send(string, map[string]interface{}) (interface{}, error) {
	return nil, nil
}

func (f *fakeCDP) NoLogSend(method string, params map[string]interface{}) (interface{}, error) {
	if method != cdp.METHOD_EVALUATE {
		return nil, errors.New("unexpected method")
	}
	expression, _ := params["expression"].(string)
	if !strings.Contains(expression, `"command":"mintPairingDisplay"`) {
		return nil, errors.New("unexpected expression")
	}
	if params["returnByValue"] != true {
		return nil, errors.New("missing returnByValue")
	}
	f.params = params
	if f.err != nil {
		return nil, f.err
	}
	return f.result, nil
}

func (f *fakeCDP) PageNavigationURL(context.Context) (string, error) { return "", nil }
func (f *fakeCDP) Close()                                            {}
func (f *fakeCDP) Initialized() bool                                 { return true }

var _ cdp.CDP = (*fakeCDP)(nil)

func runtimeEvaluateStringResult(value string) map[string]any {
	return map[string]any{
		"result": map[string]any{
			"result": map[string]any{
				"type":  "string",
				"value": value,
			},
		},
	}
}

func requireMintPairingDisplayRequest(t *testing.T, params map[string]interface{}) map[string]any {
	t.Helper()

	expression, _ := params["expression"].(string)
	raw := strings.TrimPrefix(expression, "window.handleCDPRequest(")
	raw = strings.TrimSuffix(raw, ")")

	var payload struct {
		Command string         `json:"command"`
		Request map[string]any `json:"request"`
	}
	require.NoError(t, json.Unmarshal([]byte(raw), &payload))
	require.Equal(t, "mintPairingDisplay", payload.Command)
	require.NotNil(t, payload.Request)
	return payload.Request
}
