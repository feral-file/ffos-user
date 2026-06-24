//go:build integration

package mintpairing

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	minter "github.com/feral-file/ff-art-computer-handoff/clients/ephemeral-token-minter/go"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-controld/cdp"
	"github.com/feral-file/ffos-user/components/feral-controld/relayer"
	"github.com/feral-file/ffos-user/components/feral-controld/state"
	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

const (
	liveHandoffBrokerBaseURL = "https://handoff.feralfile.com"
	liveRelayerHTTPBaseURL   = "https://relayer-dev.bitmark-development.workers.dev"
	liveRelayerWSBaseURL     = "wss://relayer-dev.bitmark-development.workers.dev"
	liveRelayerAPIKey        = "test"
)

func TestLiveHandoffRelayerAndBrokerFlow(t *testing.T) {
	if os.Getenv("FFOS_RUN_LIVE_HANDOFF_TEST") != "1" {
		t.Skip("set FFOS_RUN_LIVE_HANDOFF_TEST=1 to run the live handoff integration test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	defer state.ResetForTesting()

	topicID := verifyLiveRelayerRoundTrip(ctx, t, liveRelayerWSBaseURL, liveRelayerHTTPBaseURL, liveRelayerAPIKey)
	state.GetState().Relayer.TopicID = topicID

	requireLiveBrokerHealthy(ctx, t, liveHandoffBrokerBaseURL)

	jsDir := prepareSessionRecipientJS(ctx, t)
	capturedRelayer := newLiveCaptureRelayer()
	svc := newService(
		Options{
			Enabled:         true,
			BrokerBaseURL:   liveHandoffBrokerBaseURL,
			IdleTTL:         2 * time.Minute,
			PollInterval:    250 * time.Millisecond,
			ApprovalTimeout: 90 * time.Second,
			RelayerBaseURL:  liveRelayerHTTPBaseURL,
		},
		realBrokerStarter{client: minter.NewClient(&http.Client{Timeout: wrapper.HTTPClientTimeout})},
		NewRelayerSessionCreator(liveRelayerHTTPBaseURL, liveRelayerAPIKey, wrapper.NewHTTPClient(), wrapper.NewJSON()),
		capturedRelayer,
		&liveNoopCDP{},
		wrapper.NewJSON(),
		zap.NewNop(),
	).(*service)
	svc.Start(ctx)
	defer svc.Stop()

	result, err := svc.HandleStartPairingSession(ctx, nil)
	require.NoError(t, err)
	startResp, ok := result.(startPairingResponse)
	require.True(t, ok, "%#v", result)
	require.True(t, startResp.OK)
	require.Equal(t, "started", startResp.Status)
	require.NotEmpty(t, startResp.ChannelID)
	require.NotEmpty(t, startResp.PairingCode)

	jsCmd := startRecipientJS(ctx, t, jsDir, liveHandoffBrokerBaseURL, startResp.PairingCode)
	approval := waitForLiveRelayerResponse(ctx, t, capturedRelayer, "mint_pairing_approval_request")
	approvalMessage := requireResponseMessageMap(t, approval)
	require.Equal(t, topicID, requireStringField(t, approvalMessage, "topicID"))
	require.Equal(t, startResp.ChannelID, requireStringField(t, approvalMessage, "channelID"))
	require.Equal(t, "https://ffos-user.integration.test", requireStringField(t, approvalMessage, "origin"))
	require.NotEmpty(t, requireStringField(t, approvalMessage, "requestMessageID"))

	browserInfo, ok := approvalMessage["browserInfo"].(minter.BrowserInfo)
	require.True(t, ok)
	require.Equal(t, "Live Integration Browser", browserInfo.Name)
	require.Equal(t, "ffos-user live handoff test", browserInfo.Label)

	decisionResult, err := svc.HandleApprovalDecision(ctx, map[string]any{
		"v":                 float64(1),
		"approvalRequestID": requireStringField(t, approvalMessage, "approvalRequestID"),
		"topicID":           topicID,
		"channelID":         startResp.ChannelID,
		"requestMessageID":  requireStringField(t, approvalMessage, "requestMessageID"),
		"decision":          "approve",
		"decidedAt":         time.Now().UTC().Format(time.RFC3339),
		"controller": map[string]any{
			"clientID": "live-integration-test",
			"platform": "go-test",
		},
	})
	require.NoError(t, err)
	decisionResp, ok := decisionResult.(approvalResponse)
	require.True(t, ok)
	require.True(t, decisionResp.OK)
	require.Equal(t, "accepted", decisionResp.Status)

	waitForRecipientJS(t, jsCmd)
	outcome := waitForLiveRelayerResponse(ctx, t, capturedRelayer, "mint_pairing_approval_outcome")
	outcomeMessage := requireResponseMessageMap(t, outcome)
	require.Equal(t, "completed", requireStringField(t, outcomeMessage, "status"))
	require.Equal(t, requireStringField(t, approvalMessage, "approvalRequestID"), requireStringField(t, outcomeMessage, "approvalRequestID"))
}

func verifyLiveRelayerRoundTrip(ctx context.Context, t *testing.T, wsBaseURL string, httpBaseURL string, apiKey string) string {
	t.Helper()

	conn, _, err := websocket.DefaultDialer.DialContext(ctx, liveRelayerConnectionURL(t, wsBaseURL, apiKey, ""), nil)
	require.NoError(t, err)
	defer conn.Close()

	topicID := readLiveSystemTopicID(ctx, t, conn)
	done := make(chan liveCastResult, 1)
	go func() {
		done <- postLiveCast(ctx, httpBaseURL, apiKey, topicID, map[string]any{
			"command": "liveIntegrationPing",
			"request": map[string]any{
				"from":  "mobile-app-simulation",
				"nonce": time.Now().UTC().Format(time.RFC3339Nano),
			},
		})
	}()

	var payload relayer.Payload
	require.NoError(t, conn.ReadJSON(&payload))
	require.NotEmpty(t, payload.MessageID)
	require.NotNil(t, payload.Message.Command)
	require.Equal(t, "liveIntegrationPing", *payload.Message.Command)
	require.Equal(t, "mobile-app-simulation", payload.Message.Request["from"])

	require.NoError(t, conn.WriteJSON(relayer.Response{
		Type:      "RPC",
		MessageID: payload.MessageID,
		Message: map[string]any{
			"ok":       true,
			"handled":  "tv-simulation",
			"command":  *payload.Message.Command,
			"received": payload.Message.Request,
		},
	}))

	select {
	case result := <-done:
		require.NoError(t, result.err)
		require.Equal(t, http.StatusOK, result.status)
		require.Equal(t, true, result.body["ok"])
		require.Equal(t, "tv-simulation", result.body["handled"])
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}

	return topicID
}

func liveRelayerConnectionURL(t *testing.T, wsBaseURL string, apiKey string, topicID string) string {
	t.Helper()

	u, err := url.Parse(strings.TrimRight(wsBaseURL, "/") + "/api/connection")
	require.NoError(t, err)
	q := u.Query()
	q.Set("apiKey", apiKey)
	if topicID != "" {
		q.Set("topicID", topicID)
	}
	u.RawQuery = q.Encode()
	return u.String()
}

func readLiveSystemTopicID(ctx context.Context, t *testing.T, conn *websocket.Conn) string {
	t.Helper()

	for {
		if deadline, ok := ctx.Deadline(); ok {
			require.NoError(t, conn.SetReadDeadline(deadline))
		}
		var payload relayer.Payload
		require.NoError(t, conn.ReadJSON(&payload))
		if payload.MessageID != relayer.MESSAGE_ID_SYSTEM {
			continue
		}
		require.NotNil(t, payload.Message.TopicID)
		require.NotEmpty(t, *payload.Message.TopicID)
		return *payload.Message.TopicID
	}
}

type liveCastResult struct {
	status int
	body   map[string]any
	err    error
}

func postLiveCast(ctx context.Context, httpBaseURL string, apiKey string, topicID string, body map[string]any) liveCastResult {
	endpoint, err := url.Parse(strings.TrimRight(httpBaseURL, "/") + "/api/cast")
	if err != nil {
		return liveCastResult{err: err}
	}
	q := endpoint.Query()
	q.Set("topicID", topicID)
	endpoint.RawQuery = q.Encode()

	raw, err := json.Marshal(body)
	if err != nil {
		return liveCastResult{err: err}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(raw))
	if err != nil {
		return liveCastResult{err: err}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("API-KEY", apiKey)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return liveCastResult{err: err}
	}
	defer resp.Body.Close()

	var decoded struct {
		Message map[string]any `json:"message"`
		Error   map[string]any `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		return liveCastResult{status: resp.StatusCode, err: err}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return liveCastResult{status: resp.StatusCode, err: fmt.Errorf("cast failed with status %d: %v", resp.StatusCode, decoded.Error)}
	}
	return liveCastResult{status: resp.StatusCode, body: decoded.Message}
}

func requireLiveBrokerHealthy(ctx context.Context, t *testing.T, brokerBaseURL string) {
	t.Helper()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(brokerBaseURL, "/")+"/healthz", nil)
	require.NoError(t, err)

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("live broker %s is not healthy: GET /healthz returned %d; check broker deployment/upstream before debugging the client flow", brokerBaseURL, resp.StatusCode)
	}

	var body struct {
		OK bool `json:"ok"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
	require.True(t, body.OK, "live broker health response did not include ok=true")
}

func prepareSessionRecipientJS(ctx context.Context, t *testing.T) string {
	t.Helper()

	if jsDir := strings.TrimSpace(os.Getenv("FF_HANDOFF_JS_DIR")); jsDir != "" {
		runCommand(ctx, t, jsDir, "npm", "ci", "--silent")
		return jsDir
	}

	workDir := t.TempDir()
	repoDir := filepath.Join(workDir, "ff-art-computer-handoff")
	runCommand(ctx, t, workDir, "git", "clone", "--depth", "1", "https://github.com/feral-file/ff-art-computer-handoff.git", repoDir)
	jsDir := filepath.Join(repoDir, "clients", "session-recipient", "js")
	runCommand(ctx, t, jsDir, "npm", "ci", "--silent")
	return jsDir
}

type liveRecipientCommand struct {
	done chan error
}

func startRecipientJS(ctx context.Context, t *testing.T, jsDir string, brokerBaseURL string, pairingCode string) *liveRecipientCommand {
	t.Helper()

	testPath := filepath.Join(jsDir, "live-recipient.integration.test.ts")
	source := `import { describe, expect, it } from "vitest";
import { requestEphemeralSession } from "./src/client.js";

describe("live recipient handoff", () => {
  it("receives an encrypted minted relayer session through the broker", async () => {
    Object.defineProperty(globalThis, "location", {
      configurable: true,
      value: { origin: "https://ffos-user.integration.test" }
    });
    const session = await requestEphemeralSession({
      pairing: {
        brokerBaseUrl: process.env.FF_LIVE_BROKER_BASE_URL ?? "",
        shortCode: process.env.FF_LIVE_PAIRING_CODE ?? ""
      },
      browserInfo: {
        name: "Live Integration Browser",
        userAgent: "ffos-user-live-integration",
        label: "ffos-user live handoff test"
      },
      storage: false,
      pollIntervalMs: 250,
      maxWaitMs: 120000
    });
    expect(session.token.length).toBeGreaterThan(0);
    expect(session.sessionId.length).toBeGreaterThan(0);
    expect(Date.parse(session.expiresAt)).toBeGreaterThan(Date.now());
    expect(session.relayerBaseUrl).toBe("https://relayer-dev.bitmark-development.workers.dev");
  }, 120000);
});
`
	require.NoError(t, os.WriteFile(testPath, []byte(source), 0600))
	t.Cleanup(func() {
		_ = os.Remove(testPath)
	})

	cmd := exec.CommandContext(ctx, "npx", "vitest", "run", filepath.Base(testPath), "--pool=forks", "--testTimeout=120000")
	cmd.Dir = jsDir
	cmd.Env = append(os.Environ(),
		"FF_LIVE_BROKER_BASE_URL="+brokerBaseURL,
		"FF_LIVE_PAIRING_CODE="+pairingCode,
	)
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	require.NoError(t, cmd.Start())

	done := make(chan error, 1)
	go func() {
		err := cmd.Wait()
		if err != nil {
			err = fmt.Errorf("%w\n%s", err, output.String())
		}
		done <- err
	}()
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
	})
	return &liveRecipientCommand{done: done}
}

func waitForRecipientJS(t *testing.T, recipient *liveRecipientCommand) {
	t.Helper()

	select {
	case err := <-recipient.done:
		require.NoError(t, err)
	case <-time.After(130 * time.Second):
		t.Fatal("timed out waiting for JS recipient library")
	}
}

func runCommand(ctx context.Context, t *testing.T, dir string, name string, args ...string) {
	t.Helper()

	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, "%s %s failed in %s\n%s", name, strings.Join(args, " "), dir, string(output))
}

type liveCaptureRelayer struct {
	sent chan relayer.Response
}

func newLiveCaptureRelayer() *liveCaptureRelayer {
	return &liveCaptureRelayer{sent: make(chan relayer.Response, 8)}
}

func (r *liveCaptureRelayer) IsConnected() bool                      { return true }
func (r *liveCaptureRelayer) Connect(context.Context) error          { return nil }
func (r *liveCaptureRelayer) RetryableConnect(context.Context) error { return nil }
func (r *liveCaptureRelayer) OnRelayerMessage(relayer.Handler)       {}
func (r *liveCaptureRelayer) RemoveRelayerMessage(relayer.Handler)   {}
func (r *liveCaptureRelayer) Close()                                 {}

func (r *liveCaptureRelayer) Send(ctx context.Context, data interface{}) error {
	resp, ok := data.(relayer.Response)
	if !ok {
		return fmt.Errorf("unexpected relayer send payload %T", data)
	}
	select {
	case r.sent <- resp:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func waitForLiveRelayerResponse(ctx context.Context, t *testing.T, r *liveCaptureRelayer, responseType string) relayer.Response {
	t.Helper()

	for {
		select {
		case resp := <-r.sent:
			if resp.Type == "notification" && resp.NotificationType == responseType {
				return resp
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
}

func requireResponseMessageMap(t *testing.T, response relayer.Response) map[string]any {
	t.Helper()

	message, ok := response.Message.(map[string]any)
	require.True(t, ok)
	return message
}

func requireStringField(t *testing.T, fields map[string]any, key string) string {
	t.Helper()

	value, ok := fields[key].(string)
	require.True(t, ok, "field %q should be a string", key)
	require.NotEmpty(t, value)
	return value
}

type liveNoopCDP struct {
	expressions []string
}

func (c *liveNoopCDP) Init(context.Context) error                               { return nil }
func (c *liveNoopCDP) Send(string, map[string]interface{}) (interface{}, error) { return nil, nil }
func (c *liveNoopCDP) PageNavigationURL(context.Context) (string, error) {
	return "", errors.New("not implemented")
}
func (c *liveNoopCDP) Close()            {}
func (c *liveNoopCDP) Initialized() bool { return true }

func (c *liveNoopCDP) NoLogSend(method string, params map[string]interface{}) (interface{}, error) {
	if method != cdp.METHOD_EVALUATE {
		return nil, fmt.Errorf("unexpected CDP method %q", method)
	}
	expression, ok := params["expression"].(string)
	if !ok || expression == "" {
		return nil, errors.New("missing evaluation expression")
	}
	if !strings.Contains(expression, `"command":"mintPairingDisplay"`) {
		return nil, fmt.Errorf("unexpected evaluation expression %q", expression)
	}
	c.expressions = append(c.expressions, expression)
	return map[string]any{
		"result": map[string]any{
			"result": map[string]any{
				"type":  "string",
				"value": `{"ok":true}`,
			},
		},
	}, nil
}

var _ cdp.CDP = (*liveNoopCDP)(nil)
