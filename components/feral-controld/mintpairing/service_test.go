package mintpairing

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	minter "github.com/feral-file/ff-art-computer-handoff/clients/ephemeral-token-minter/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-controld/cdp"
	"github.com/feral-file/ffos-user/components/feral-controld/config"
	"github.com/feral-file/ffos-user/components/feral-controld/relayer"
	"github.com/feral-file/ffos-user/components/feral-controld/state"
	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

func TestHandleApprovalDecision_AcceptsAndDeduplicates(t *testing.T) {
	defer state.ResetForTesting()
	state.GetState().Relayer.TopicID = "topic-1"

	s := newTestService()
	pending := &pendingApproval{
		approvalRequestID: "mpa_1",
		topicID:           "topic-1",
		channelID:         "ch_1",
		requestMessageID:  "msg_1",
		expiresAt:         time.Now().Add(time.Minute),
		decisionCh:        make(chan approvalDecisionRequest, 1),
	}
	s.registerPending(pending)

	args := map[string]any{
		"v":                 float64(1),
		"approvalRequestID": "mpa_1",
		"topicID":           "topic-1",
		"channelID":         "ch_1",
		"requestMessageID":  "msg_1",
		"decision":          "approve",
	}

	result, err := s.HandleApprovalDecision(context.Background(), args)
	require.NoError(t, err)
	resp := result.(approvalResponse)
	assert.True(t, resp.OK)
	assert.Equal(t, "accepted", resp.Status)

	select {
	case decision := <-pending.decisionCh:
		assert.Equal(t, "approve", decision.Decision)
	default:
		t.Fatal("expected accepted decision to be delivered")
	}

	result, err = s.HandleApprovalDecision(context.Background(), args)
	require.NoError(t, err)
	resp = result.(approvalResponse)
	assert.True(t, resp.OK)
	assert.Equal(t, "already_accepted", resp.Status)

	args["decision"] = "reject"
	result, err = s.HandleApprovalDecision(context.Background(), args)
	require.NoError(t, err)
	resp = result.(approvalResponse)
	require.NotNil(t, resp.Error)
	assert.False(t, resp.OK)
	assert.Equal(t, "already_decided", resp.Error.Code)
}

func TestHandleApprovalDecision_RejectsMismatches(t *testing.T) {
	defer state.ResetForTesting()
	state.GetState().Relayer.TopicID = "topic-1"

	s := newTestService()
	s.registerPending(&pendingApproval{
		approvalRequestID: "mpa_1",
		topicID:           "topic-1",
		channelID:         "ch_1",
		requestMessageID:  "msg_1",
		expiresAt:         time.Now().Add(time.Minute),
		decisionCh:        make(chan approvalDecisionRequest, 1),
	})

	tests := []struct {
		name string
		args map[string]any
		code string
	}{
		{
			name: "invalid payload",
			args: map[string]any{"v": float64(1), "decision": "approve"},
			code: "invalid_request",
		},
		{
			name: "unknown approval",
			args: validDecisionArgs("mpa_missing", "topic-1", "ch_1", "msg_1"),
			code: "not_found",
		},
		{
			name: "topic mismatch",
			args: validDecisionArgs("mpa_1", "topic-2", "ch_1", "msg_1"),
			code: "topic_mismatch",
		},
		{
			name: "request mismatch",
			args: validDecisionArgs("mpa_1", "topic-1", "ch_2", "msg_1"),
			code: "request_mismatch",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := s.HandleApprovalDecision(context.Background(), tt.args)
			require.NoError(t, err)
			resp := result.(approvalResponse)
			require.NotNil(t, resp.Error)
			assert.False(t, resp.OK)
			assert.Equal(t, tt.code, resp.Error.Code)
		})
	}
}

func TestRelayerSessionCreator_CreateEphemeralSession(t *testing.T) {
	var seenRequest struct {
		Path        string
		TopicID     string
		APIKey      string
		ContentType string
		Body        map[string]any
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenRequest.Path = r.URL.Path
		seenRequest.TopicID = r.URL.Query().Get("topicID")
		seenRequest.APIKey = r.Header.Get("API-KEY")
		seenRequest.ContentType = r.Header.Get("Content-Type")
		require.NoError(t, json.NewDecoder(r.Body).Decode(&seenRequest.Body))
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"session":{"id":"session-1","expiresAt":"2030-01-01T00:00:00Z"},"token":"browser-token"}`))
	}))
	defer server.Close()

	creator := NewRelayerSessionCreator(server.URL, "api-key-1", wrapper.NewHTTPClient(), wrapper.NewJSON())
	result, err := creator.CreateEphemeralSession(context.Background(), "topic-1", minter.MintRequest{
		BrowserInfo: minter.BrowserInfo{
			Name:      "Chrome",
			UserAgent: "Browser UA",
			Label:     "Gallery laptop",
		},
		RequestedExpiresInSeconds: 3600,
	})

	require.NoError(t, err)
	assert.Equal(t, "session-1", result.SessionID)
	assert.Equal(t, "browser-token", result.Token)
	assert.Equal(t, server.URL, result.RelayerBaseURL)
	assert.Equal(t, "/api/ephemeral-sessions", seenRequest.Path)
	assert.Equal(t, "topic-1", seenRequest.TopicID)
	assert.Equal(t, "api-key-1", seenRequest.APIKey)
	assert.Equal(t, "application/json", seenRequest.ContentType)
	assert.Equal(t, "Chrome", seenRequest.Body["browserName"])
	assert.Equal(t, "Browser UA", seenRequest.Body["browserUserAgent"])
	assert.Equal(t, "Gallery laptop", seenRequest.Body["label"])
	assert.Equal(t, float64(3600), seenRequest.Body["expiresInSeconds"])
}

func TestRelayerSessionCreator_AppliesControldOwnedSessionTTLPolicy(t *testing.T) {
	tests := []struct {
		name      string
		requested int
		want      int
	}{
		{
			name: "default when browser does not request ttl",
			want: defaultSessionTTLSeconds,
		},
		{
			name:      "clamps below minimum",
			requested: minSessionTTLSeconds - 1,
			want:      minSessionTTLSeconds,
		},
		{
			name:      "clamps above maximum",
			requested: maxSessionTTLSeconds + 1,
			want:      maxSessionTTLSeconds,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var body map[string]any
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
				w.WriteHeader(http.StatusCreated)
				_, _ = w.Write([]byte(`{"session":{"id":"session-1","expiresAt":"2030-01-01T00:00:00Z"},"token":"browser-token"}`))
			}))
			defer server.Close()

			creator := NewRelayerSessionCreator(server.URL, "", wrapper.NewHTTPClient(), wrapper.NewJSON())
			_, err := creator.CreateEphemeralSession(context.Background(), "topic-1", minter.MintRequest{
				RequestedExpiresInSeconds: tt.requested,
			})

			require.NoError(t, err)
			assert.Equal(t, float64(tt.want), body["expiresInSeconds"])
		})
	}
}

func TestRelayerHTTPBaseString_NormalizesWebSocketEndpointToOrigin(t *testing.T) {
	tests := []struct {
		name     string
		endpoint string
		want     string
	}{
		{
			name:     "wss endpoint with path",
			endpoint: "wss://relayer.example/ws",
			want:     "https://relayer.example",
		},
		{
			name:     "ws endpoint with path query and fragment",
			endpoint: "ws://127.0.0.1:8080/ws?topic=abc#debug",
			want:     "http://127.0.0.1:8080",
		},
		{
			name:     "http path is preserved",
			endpoint: "https://relayer.example/base/",
			want:     "https://relayer.example/base",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, relayerHTTPBaseString(tt.endpoint))
		})
	}
}

func TestOptionsFromConfig_SetsDefaultPlayerContractPathWhenEnabled(t *testing.T) {
	opts := OptionsFromConfig(&config.MintPairingConfig{Enabled: true}, "")
	assert.Equal(t, defaultPlayerContractPath, opts.PlayerContractPath)

	opts = OptionsFromConfig(&config.MintPairingConfig{}, "")
	assert.Empty(t, opts.PlayerContractPath)
}

func TestHandleStartPairingSession_ValidatesPlayerContractBeforeBrokerStart(t *testing.T) {
	tests := []struct {
		name     string
		manifest string
		missing  bool
	}{
		{
			name:    "missing manifest",
			missing: true,
		},
		{
			name:     "malformed json",
			manifest: `{`,
		},
		{
			name:     "wrong contract path with loose token",
			manifest: `{"contracts":{"other":{"version":1,"requestKey":"request","states":["pairing_code","request_received","creating_token","hidden"],"acceptedResponse":{"ok":true}}},"loose":"mintPairingDisplay"}`,
		},
		{
			name:     "missing state",
			manifest: `{"contracts":{"mintPairingDisplay":{"version":1,"requestKey":"request","states":["pairing_code","request_received","creating_token"],"acceptedResponse":{"ok":true}}}}`,
		},
		{
			name:     "wrong accepted response",
			manifest: `{"contracts":{"mintPairingDisplay":{"version":1,"requestKey":"request","states":["pairing_code","request_received","creating_token","hidden"],"acceptedResponse":{"ok":false}}}}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			defer state.ResetForTesting()
			state.GetState().Relayer.TopicID = "topic-1"

			contractPath := filepath.Join(t.TempDir(), "ffos-player-contract.json")
			if !tt.missing {
				require.NoError(t, os.WriteFile(contractPath, []byte(tt.manifest), 0o600))
			}
			starter := &fakeBrokerStarter{channel: &fakeBrokerChannel{pairingCode: "PAIR-123"}}
			cdpClient := &fakeCDP{}
			s := newService(
				Options{
					Enabled:            true,
					BrokerBaseURL:      "https://broker.example",
					PlayerContractPath: contractPath,
				},
				starter,
				nil,
				nil,
				cdpClient,
				wrapper.NewJSON(),
				zap.NewNop(),
			).(*service)

			result, err := s.HandleStartPairingSession(context.Background(), nil)
			require.NoError(t, err)
			assertCommandError(t, result, "invalid_config", false)
			assert.Equal(t, 0, starter.startCount)
			assert.Empty(t, cdpClient.displayRequestsSnapshot())
		})
	}
}

func TestHandleStartPairingSession_AcceptsValidPlayerContract(t *testing.T) {
	defer state.ResetForTesting()
	state.GetState().Relayer.TopicID = "topic-1"

	contractPath := writeValidPlayerContract(t)
	starter := &fakeBrokerStarter{channel: &fakeBrokerChannel{pairingCode: "PAIR-123"}}
	cdpClient := &fakeCDP{}
	s := newService(
		Options{
			Enabled:            true,
			BrokerBaseURL:      "https://broker.example",
			IdleTTL:            time.Minute,
			PlayerContractPath: contractPath,
		},
		starter,
		nil,
		nil,
		cdpClient,
		wrapper.NewJSON(),
		zap.NewNop(),
	).(*service)
	s.Start(context.Background())
	defer s.Stop()

	result, err := s.HandleStartPairingSession(context.Background(), nil)
	require.NoError(t, err)
	resp := result.(startPairingResponse)
	assert.True(t, resp.OK)
	assert.Equal(t, "started", resp.Status)
	assert.Equal(t, 1, starter.startCount)
	assertEventuallyDisplayObserved(t, cdpClient, "pairing_code", "PAIR-123", "")
}

func TestHandleStartPairingSession_ReturnsCommandErrorForBrokerStartFailure(t *testing.T) {
	defer state.ResetForTesting()
	state.GetState().Relayer.TopicID = "topic-1"

	s := newService(
		Options{Enabled: true, BrokerBaseURL: "https://broker.example"},
		&fakeBrokerStarter{err: errors.New("broker down")},
		nil,
		nil,
		&fakeCDP{},
		wrapper.NewJSON(),
		zap.NewNop(),
	).(*service)

	result, err := s.HandleStartPairingSession(context.Background(), nil)
	require.NoError(t, err)
	assertCommandError(t, result, "broker_unavailable", true)
}

func TestHandleStartPairingSession_ReturnsCommandErrorForDisplayFailure(t *testing.T) {
	defer state.ResetForTesting()
	state.GetState().Relayer.TopicID = "topic-1"

	ch := &fakeBrokerChannel{pairingCode: "PAIR-123"}
	s := newService(
		Options{Enabled: true, BrokerBaseURL: "https://broker.example", IdleTTL: time.Minute},
		&fakeBrokerStarter{channel: ch},
		nil,
		nil,
		&fakeCDP{err: errors.New("cdp down")},
		wrapper.NewJSON(),
		zap.NewNop(),
	).(*service)

	result, err := s.HandleStartPairingSession(context.Background(), nil)
	require.NoError(t, err)
	assertCommandError(t, result, "display_unavailable", true)
	assert.Equal(t, 1, ch.closeCount)
}

func TestHandleStartPairingSession_ReturnsCommandErrorForApplicationDisplayFailure(t *testing.T) {
	defer state.ResetForTesting()
	state.GetState().Relayer.TopicID = "topic-1"

	ch := &fakeBrokerChannel{pairingCode: "PAIR-123"}
	s := newService(
		Options{Enabled: true, BrokerBaseURL: "https://broker.example", IdleTTL: time.Minute},
		&fakeBrokerStarter{channel: ch},
		nil,
		nil,
		&fakeCDP{appResponse: map[string]any{"ok": false, "error": "overlay unavailable"}},
		wrapper.NewJSON(),
		zap.NewNop(),
	).(*service)

	result, err := s.HandleStartPairingSession(context.Background(), nil)
	require.NoError(t, err)
	assertCommandError(t, result, "display_unavailable", true)
	assert.Equal(t, 1, ch.closeCount)
}

func TestHandleStartPairingSession_ReturnsCommandErrorForActiveRedisplayFailure(t *testing.T) {
	defer state.ResetForTesting()
	state.GetState().Relayer.TopicID = "topic-1"

	ch := &fakeBrokerChannel{pairingCode: "PAIR-123"}
	s := newService(
		Options{Enabled: true, BrokerBaseURL: "https://broker.example"},
		&fakeBrokerStarter{channel: ch},
		nil,
		nil,
		&fakeCDP{err: errors.New("cdp down")},
		wrapper.NewJSON(),
		zap.NewNop(),
	).(*service)
	active := &activePairing{
		channel:     ch,
		channelID:   "ch_1",
		pairingCode: "PAIR-123",
		expiresAt:   time.Now().Add(time.Minute),
		cancel:      func() {},
	}
	s.active = active

	result, err := s.HandleStartPairingSession(context.Background(), nil)
	require.NoError(t, err)
	assertCommandError(t, result, "display_unavailable", true)
	assert.Equal(t, 0, ch.closeCount, "redisplay failure must keep the active broker session alive")
	assert.Same(t, active, s.active)
}

func TestHandleStartPairingSession_DisplaysCodeAndCachesTerminalDecision(t *testing.T) {
	defer state.ResetForTesting()
	state.GetState().Relayer.TopicID = "topic-1"

	ch := &fakeBrokerChannel{
		pairingCode: "PAIR-123",
		request: &minter.MintRequest{
			ChannelID:   "ch_1",
			MessageID:   "msg_1",
			Origin:      "https://gallery.example",
			BrowserInfo: minter.BrowserInfo{Name: "Chrome"},
		},
	}
	relayerClient := &fakeRelayer{sent: make(chan relayer.Response, 4)}
	cdpClient := &fakeCDP{}
	starter := &fakeBrokerStarter{channel: ch}
	s := newService(
		Options{
			Enabled:         true,
			BrokerBaseURL:   "https://broker.example",
			ApprovalTimeout: 2 * time.Second,
			PollInterval:    time.Millisecond,
			RelayerBaseURL:  "https://relayer.example",
		},
		starter,
		fakeSessionCreator{},
		relayerClient,
		cdpClient,
		wrapper.NewJSON(),
		zap.NewNop(),
	).(*service)
	s.Start(context.Background())
	defer s.Stop()

	result, err := s.HandleStartPairingSession(context.Background(), nil)
	require.NoError(t, err)
	startResp := result.(startPairingResponse)
	assert.True(t, startResp.OK)
	assert.Equal(t, "started", startResp.Status)
	assert.Equal(t, "PAIR-123", startResp.PairingCode)
	assert.True(t, starter.receivedOptions.ShortCodeRequested)
	assertEventuallyDisplayObserved(t, cdpClient, "pairing_code", "PAIR-123", "")

	approval := <-relayerClient.sent
	assertRelayerNotification(t, approval, relayer.NOTIFICATION_TYPE_MINT_PAIRING_APPROVAL_REQUEST)
	approvalMessage := approval.Message.(map[string]any)
	approvalID := approvalMessage["approvalRequestID"].(string)
	assertEventuallyDisplayObserved(t, cdpClient, "request_received", "", "Chrome")

	result, err = s.HandleStartPairingSession(context.Background(), nil)
	require.NoError(t, err)
	pendingResp := result.(startPairingResponse)
	assert.True(t, pendingResp.OK)
	assert.Equal(t, "pending_approval", pendingResp.Status)
	assert.Empty(t, pendingResp.PairingCode)
	assert.Equal(t, "ch_1", pendingResp.ChannelID)
	assert.Equal(t, 1, starter.startCount)
	assert.Equal(t, 1, countDisplayRequests(cdpClient, "pairing_code", "PAIR-123", ""))
	assertLastDisplay(t, cdpClient, "request_received", "", "Chrome")

	decisionArgs := validDecisionArgs(approvalID, "topic-1", "ch_1", "msg_1")
	result, err = s.HandleApprovalDecision(context.Background(), decisionArgs)
	require.NoError(t, err)
	assert.Equal(t, "accepted", result.(approvalResponse).Status)

	outcome := <-relayerClient.sent
	assertRelayerNotification(t, outcome, relayer.NOTIFICATION_TYPE_MINT_PAIRING_APPROVAL_OUTCOME)
	assert.Equal(t, 1, ch.successCount)
	assert.Equal(t, 0, ch.closeCount, "terminal success must remain pollable for the browser")
	assertEventuallyDisplayObserved(t, cdpClient, "creating_token", "", "Chrome")
	assertEventuallyDisplayObserved(t, cdpClient, "hidden", "", "")

	result, err = s.HandleApprovalDecision(context.Background(), decisionArgs)
	require.NoError(t, err)
	assert.Equal(t, "already_accepted", result.(approvalResponse).Status)
}

func TestWaitForBrowserAndApproval_RestoresDisplayAfterControllerRejection(t *testing.T) {
	defer state.ResetForTesting()
	state.GetState().Relayer.TopicID = "topic-1"

	ch := &fakeBrokerChannel{
		pairingCode:   "PAIR-123",
		rejectionSent: make(chan struct{}, 1),
		request: &minter.MintRequest{
			ChannelID:   "ch_1",
			MessageID:   "msg_1",
			Origin:      "https://gallery.example",
			BrowserInfo: minter.BrowserInfo{Name: "Chrome"},
		},
	}
	relayerClient := &fakeRelayer{sent: make(chan relayer.Response, 4)}
	cdpClient := &fakeCDP{}
	s := newService(
		Options{
			Enabled:         true,
			BrokerBaseURL:   "https://broker.example",
			ApprovalTimeout: time.Minute,
			PollInterval:    time.Millisecond,
			RelayerBaseURL:  "https://relayer.example",
		},
		&fakeBrokerStarter{channel: ch},
		fakeSessionCreator{},
		relayerClient,
		cdpClient,
		wrapper.NewJSON(),
		zap.NewNop(),
	).(*service)
	s.Start(context.Background())
	defer s.Stop()

	result, err := s.HandleStartPairingSession(context.Background(), nil)
	require.NoError(t, err)
	assert.True(t, result.(startPairingResponse).OK)

	approval := <-relayerClient.sent
	assertRelayerNotification(t, approval, relayer.NOTIFICATION_TYPE_MINT_PAIRING_APPROVAL_REQUEST)
	approvalID := approval.Message.(map[string]any)["approvalRequestID"].(string)

	decisionArgs := validDecisionArgs(approvalID, "topic-1", "ch_1", "msg_1")
	decisionArgs["decision"] = "reject"
	decisionArgs["reason"] = "controller_declined"
	result, err = s.HandleApprovalDecision(context.Background(), decisionArgs)
	require.NoError(t, err)
	assert.Equal(t, "accepted", result.(approvalResponse).Status)

	select {
	case <-ch.rejectionSent:
	case <-time.After(time.Second):
		t.Fatal("expected controller rejection")
	}

	ch.mu.Lock()
	assert.Equal(t, []string{"controller_declined"}, ch.rejectionReasons)
	assert.Equal(t, 0, ch.closeCount, "terminal rejection must remain pollable for the browser")
	ch.mu.Unlock()

	outcome := <-relayerClient.sent
	assertRelayerNotification(t, outcome, relayer.NOTIFICATION_TYPE_MINT_PAIRING_APPROVAL_OUTCOME)
	assert.Equal(t, "rejected", outcome.Message.(map[string]any)["status"])
	assertEventuallyDisplayObserved(t, cdpClient, "hidden", "", "")
}

func TestHandleStartPairingSession_ApprovalRequestDisclosesEffectiveTTL(t *testing.T) {
	tests := []struct {
		name      string
		requested int
		effective int
	}{
		{
			name:      "defaulted ttl",
			effective: defaultSessionTTLSeconds,
		},
		{
			name:      "below minimum ttl",
			requested: minSessionTTLSeconds - 1,
			effective: minSessionTTLSeconds,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			defer state.ResetForTesting()
			state.GetState().Relayer.TopicID = "topic-1"

			ch := &fakeBrokerChannel{
				pairingCode:   "PAIR-123",
				rejectionSent: make(chan struct{}, 1),
				request: &minter.MintRequest{
					ChannelID:                 "ch_1",
					MessageID:                 "msg_1",
					Origin:                    "https://gallery.example",
					BrowserInfo:               minter.BrowserInfo{Name: "Chrome"},
					RequestedExpiresInSeconds: tt.requested,
				},
			}
			relayerClient := &fakeRelayer{sent: make(chan relayer.Response, 4)}
			s := newService(
				Options{
					Enabled:         true,
					BrokerBaseURL:   "https://broker.example",
					ApprovalTimeout: time.Minute,
					PollInterval:    time.Millisecond,
					RelayerBaseURL:  "https://relayer.example",
				},
				&fakeBrokerStarter{channel: ch},
				fakeSessionCreator{},
				relayerClient,
				&fakeCDP{},
				wrapper.NewJSON(),
				zap.NewNop(),
			).(*service)
			s.Start(context.Background())
			defer s.Stop()

			result, err := s.HandleStartPairingSession(context.Background(), nil)
			require.NoError(t, err)
			assert.True(t, result.(startPairingResponse).OK)

			approval := <-relayerClient.sent
			assertRelayerNotification(t, approval, relayer.NOTIFICATION_TYPE_MINT_PAIRING_APPROVAL_REQUEST)
			approvalMessage := approval.Message.(map[string]any)
			assert.Equal(t, tt.requested, approvalMessage["requestedExpiresInSeconds"])
			assert.Equal(t, tt.effective, approvalMessage["effectiveExpiresInSeconds"])
		})
	}
}

func TestWaitForBrowserAndApproval_SendsApprovalExpiredAfterSessionDeadline(t *testing.T) {
	defer state.ResetForTesting()
	state.GetState().Relayer.TopicID = "topic-1"

	ch := &fakeBrokerChannel{
		pairingCode:   "PAIR-123",
		expiresAt:     time.Now().Add(80 * time.Millisecond),
		rejectionSent: make(chan struct{}, 1),
		closed:        make(chan struct{}, 1),
		request: &minter.MintRequest{
			ChannelID:   "ch_1",
			MessageID:   "msg_1",
			Origin:      "https://gallery.example",
			BrowserInfo: minter.BrowserInfo{Name: "Chrome"},
		},
	}
	relayerClient := &fakeRelayer{sent: make(chan relayer.Response, 4)}
	cdpClient := &fakeCDP{}
	s := newService(
		Options{
			Enabled:         true,
			BrokerBaseURL:   "https://broker.example",
			ApprovalTimeout: time.Minute,
			PollInterval:    time.Millisecond,
			RelayerBaseURL:  "https://relayer.example",
		},
		&fakeBrokerStarter{channel: ch},
		fakeSessionCreator{},
		relayerClient,
		cdpClient,
		wrapper.NewJSON(),
		zap.NewNop(),
	).(*service)
	s.Start(context.Background())
	defer s.Stop()

	result, err := s.HandleStartPairingSession(context.Background(), nil)
	require.NoError(t, err)
	assert.True(t, result.(startPairingResponse).OK)

	assertRelayerNotification(t, <-relayerClient.sent, relayer.NOTIFICATION_TYPE_MINT_PAIRING_APPROVAL_REQUEST)

	select {
	case <-ch.rejectionSent:
	case <-ch.closed:
		t.Fatal("channel closed before terminal expiration rejection was sent")
	case <-time.After(time.Second):
		t.Fatal("expected approval expiration rejection")
	}

	ch.mu.Lock()
	assert.Equal(t, []string{"approval_expired"}, ch.rejectionReasons)
	assert.Equal(t, 0, ch.closeCount, "terminal expiration must remain pollable for the browser")
	ch.mu.Unlock()

	outcome := <-relayerClient.sent
	assertRelayerNotification(t, outcome, relayer.NOTIFICATION_TYPE_MINT_PAIRING_APPROVAL_OUTCOME)
	assert.Equal(t, "expired", outcome.Message.(map[string]any)["status"])
	assertEventuallyDisplayObserved(t, cdpClient, "hidden", "", "")
}

func TestHandleStartPairingSession_StaleExpiredCleanupDoesNotOverwriteReplacementDisplay(t *testing.T) {
	defer state.ResetForTesting()
	state.GetState().Relayer.TopicID = "topic-1"

	rejectionStarted := make(chan struct{}, 1)
	releaseRejection := make(chan struct{})
	oldChannel := &fakeBrokerChannel{
		channelID:        "ch_old",
		pairingCode:      "PAIR-OLD",
		expiresAt:        time.Now().Add(80 * time.Millisecond),
		rejectionStarted: rejectionStarted,
		rejectionRelease: releaseRejection,
		request: &minter.MintRequest{
			ChannelID:   "ch_old",
			MessageID:   "msg_old",
			Origin:      "https://gallery.example",
			BrowserInfo: minter.BrowserInfo{Name: "Chrome"},
		},
	}
	newChannel := &fakeBrokerChannel{
		channelID:   "ch_new",
		pairingCode: "PAIR-NEW",
		expiresAt:   time.Now().Add(time.Minute),
	}
	cdpClient := &fakeCDP{}
	s := newService(
		Options{
			Enabled:         true,
			BrokerBaseURL:   "https://broker.example",
			ApprovalTimeout: time.Minute,
			PollInterval:    time.Millisecond,
			RelayerBaseURL:  "https://relayer.example",
		},
		&fakeBrokerStarter{channels: []brokerChannel{oldChannel, newChannel}},
		fakeSessionCreator{},
		&fakeRelayer{sent: make(chan relayer.Response, 4)},
		cdpClient,
		wrapper.NewJSON(),
		zap.NewNop(),
	).(*service)
	s.Start(context.Background())
	defer s.Stop()

	result, err := s.HandleStartPairingSession(context.Background(), nil)
	require.NoError(t, err)
	assert.True(t, result.(startPairingResponse).OK)
	assertEventuallyDisplayObserved(t, cdpClient, "pairing_code", "PAIR-OLD", "")

	s.mu.Lock()
	oldActive := s.active
	s.mu.Unlock()
	require.NotNil(t, oldActive)

	select {
	case <-rejectionStarted:
	case <-time.After(time.Second):
		t.Fatal("expected old session to begin expiration cleanup")
	}

	result, err = s.HandleStartPairingSession(context.Background(), nil)
	require.NoError(t, err)
	newStart := result.(startPairingResponse)
	assert.True(t, newStart.OK)
	assert.Equal(t, "started", newStart.Status)
	assert.Equal(t, "PAIR-NEW", newStart.PairingCode)
	assertEventuallyDisplayObserved(t, cdpClient, "pairing_code", "PAIR-NEW", "")

	close(releaseRejection)
	select {
	case <-oldActive.done:
	case <-time.After(time.Second):
		t.Fatal("expected old session cleanup to finish")
	}

	assertLastDisplay(t, cdpClient, "pairing_code", "PAIR-NEW", "")
}

func TestHandleStartPairingSession_RestartDuringDelayedTerminalHideLeavesNewDisplayLast(t *testing.T) {
	defer state.ResetForTesting()
	state.GetState().Relayer.TopicID = "topic-1"

	oldChannel := &fakeBrokerChannel{
		channelID:   "ch_old",
		pairingCode: "PAIR-OLD",
		expiresAt:   time.Now().Add(80 * time.Millisecond),
		request: &minter.MintRequest{
			ChannelID:   "ch_old",
			MessageID:   "msg_old",
			Origin:      "https://gallery.example",
			BrowserInfo: minter.BrowserInfo{Name: "Chrome"},
		},
	}
	newChannel := &fakeBrokerChannel{
		channelID:   "ch_new",
		pairingCode: "PAIR-NEW",
		expiresAt:   time.Now().Add(time.Minute),
	}
	cdpClient := &fakeCDP{
		defaultNavigateStarted: make(chan struct{}, 1),
		releaseDefaultNavigate: make(chan struct{}),
	}
	s := newService(
		Options{
			Enabled:         true,
			BrokerBaseURL:   "https://broker.example",
			ApprovalTimeout: time.Minute,
			PollInterval:    time.Millisecond,
			RelayerBaseURL:  "https://relayer.example",
		},
		&fakeBrokerStarter{channels: []brokerChannel{oldChannel, newChannel}},
		fakeSessionCreator{},
		&fakeRelayer{sent: make(chan relayer.Response, 4)},
		cdpClient,
		wrapper.NewJSON(),
		zap.NewNop(),
	).(*service)
	s.Start(context.Background())
	defer s.Stop()

	result, err := s.HandleStartPairingSession(context.Background(), nil)
	require.NoError(t, err)
	assert.True(t, result.(startPairingResponse).OK)
	assertEventuallyDisplayObserved(t, cdpClient, "pairing_code", "PAIR-OLD", "")

	select {
	case <-cdpClient.defaultNavigateStarted:
	case <-time.After(time.Second):
		t.Fatal("expected old terminal cleanup to begin display hide")
	}

	started := make(chan struct {
		response startPairingResponse
		err      error
	}, 1)
	go func() {
		result, err := s.HandleStartPairingSession(context.Background(), nil)
		if err != nil {
			started <- struct {
				response startPairingResponse
				err      error
			}{err: err}
			return
		}
		response, ok := result.(startPairingResponse)
		if !ok {
			started <- struct {
				response startPairingResponse
				err      error
			}{err: errors.New("restart returned unexpected response type")}
			return
		}
		started <- struct {
			response startPairingResponse
			err      error
		}{response: response}
	}()

	select {
	case <-started:
		t.Fatal("restart completed before delayed hide was released")
	case <-time.After(50 * time.Millisecond):
	}

	close(cdpClient.releaseDefaultNavigate)

	var restartResult struct {
		response startPairingResponse
		err      error
	}
	select {
	case restartResult = <-started:
	case <-time.After(time.Second):
		t.Fatal("expected restart to complete")
	}
	require.NoError(t, restartResult.err)
	newStart := restartResult.response
	assert.True(t, newStart.OK)
	assert.Equal(t, "started", newStart.Status)
	assert.Equal(t, "PAIR-NEW", newStart.PairingCode)
	assertLastDisplay(t, cdpClient, "pairing_code", "PAIR-NEW", "")
}

func TestShowPairingCode_FailedReplacementDoesNotSuppressReleasedCleanup(t *testing.T) {
	oldActive := &activePairing{channelID: "ch_old", pairingCode: "PAIR-OLD", displayGen: 1}
	newActive := &activePairing{channelID: "ch_new", pairingCode: "PAIR-NEW"}
	cdpClient := &fakeCDP{
		appResponseForRequest: func(request map[string]any) any {
			if request["state"] == "pairing_code" && request["pairingCode"] == "PAIR-NEW" {
				return map[string]any{"ok": false, "error": "overlay unavailable"}
			}
			return map[string]any{"ok": true}
		},
	}
	s := newService(
		Options{},
		nil,
		nil,
		nil,
		cdpClient,
		wrapper.NewJSON(),
		zap.NewNop(),
	).(*service)
	s.active = oldActive
	s.displayOwner = oldActive
	s.displayGeneration = oldActive.displayGen

	displayGeneration, restoreDisplay := s.releaseDisplayOwnership(oldActive)
	require.True(t, restoreDisplay)
	require.Error(t, s.showPairingCode(context.Background(), newActive))

	s.restoreDefaultDisplay(oldActive.channelID, displayGeneration)

	assertLastDisplay(t, cdpClient, "hidden", "", "")
}

func TestStop_SendsApprovalCancelledForPendingRequest(t *testing.T) {
	defer state.ResetForTesting()
	state.GetState().Relayer.TopicID = "topic-1"

	ch := &fakeBrokerChannel{
		pairingCode:    "PAIR-123",
		expiresAt:      time.Now().Add(time.Minute),
		rejectionDelay: 50 * time.Millisecond,
		rejectionSent:  make(chan struct{}, 1),
		closed:         make(chan struct{}, 1),
		request: &minter.MintRequest{
			ChannelID:   "ch_1",
			MessageID:   "msg_1",
			Origin:      "https://gallery.example",
			BrowserInfo: minter.BrowserInfo{Name: "Chrome"},
		},
	}
	relayerClient := &fakeRelayer{sent: make(chan relayer.Response, 4)}
	cdpClient := &fakeCDP{}
	s := newService(
		Options{
			Enabled:         true,
			BrokerBaseURL:   "https://broker.example",
			ApprovalTimeout: time.Minute,
			PollInterval:    time.Millisecond,
			RelayerBaseURL:  "https://relayer.example",
		},
		&fakeBrokerStarter{channel: ch},
		fakeSessionCreator{},
		relayerClient,
		cdpClient,
		wrapper.NewJSON(),
		zap.NewNop(),
	).(*service)
	s.Start(context.Background())

	result, err := s.HandleStartPairingSession(context.Background(), nil)
	require.NoError(t, err)
	assert.True(t, result.(startPairingResponse).OK)

	assertRelayerNotification(t, <-relayerClient.sent, relayer.NOTIFICATION_TYPE_MINT_PAIRING_APPROVAL_REQUEST)

	s.Stop()

	ch.mu.Lock()
	assert.Equal(t, []string{approvalCancellationStatus}, ch.rejectionReasons)
	assert.Equal(t, 0, ch.closeCount, "terminal cancellation must remain pollable for the browser")
	ch.mu.Unlock()

	outcome := <-relayerClient.sent
	assertRelayerNotification(t, outcome, relayer.NOTIFICATION_TYPE_MINT_PAIRING_APPROVAL_OUTCOME)
	assert.Equal(t, approvalCancellationStatus, outcome.Message.(map[string]any)["status"])
	assertEventuallyDisplayObserved(t, cdpClient, "hidden", "", "")
}

func TestStop_BudgetFitsControldForcedShutdown(t *testing.T) {
	defer state.ResetForTesting()
	state.GetState().Relayer.TopicID = "topic-1"

	ch := &fakeBrokerChannel{
		pairingCode:    "PAIR-123",
		expiresAt:      time.Now().Add(time.Minute),
		rejectionDelay: terminalOperationTimeout + time.Second,
		request: &minter.MintRequest{
			ChannelID:   "ch_1",
			MessageID:   "msg_1",
			Origin:      "https://gallery.example",
			BrowserInfo: minter.BrowserInfo{Name: "Chrome"},
		},
	}
	relayerClient := &fakeRelayer{sent: make(chan relayer.Response, 4)}
	s := newService(
		Options{
			Enabled:         true,
			BrokerBaseURL:   "https://broker.example",
			ApprovalTimeout: time.Minute,
			PollInterval:    time.Millisecond,
			RelayerBaseURL:  "https://relayer.example",
		},
		&fakeBrokerStarter{channel: ch},
		fakeSessionCreator{},
		relayerClient,
		&fakeCDP{},
		wrapper.NewJSON(),
		zap.NewNop(),
	).(*service)
	s.Start(context.Background())

	result, err := s.HandleStartPairingSession(context.Background(), nil)
	require.NoError(t, err)
	assert.True(t, result.(startPairingResponse).OK)
	assertRelayerNotification(t, <-relayerClient.sent, relayer.NOTIFICATION_TYPE_MINT_PAIRING_APPROVAL_REQUEST)

	started := time.Now()
	s.Stop()
	elapsed := time.Since(started)

	assert.Less(t, elapsed, 2*time.Second, "mint pairing Stop must fit under controld's forced shutdown guard")
}

func TestStop_DoesNotWaitForDisplayRestore(t *testing.T) {
	defer state.ResetForTesting()
	state.GetState().Relayer.TopicID = "topic-1"

	ch := &fakeBrokerChannel{
		pairingCode: "PAIR-123",
		expiresAt:   time.Now().Add(time.Minute),
		request: &minter.MintRequest{
			ChannelID:   "ch_1",
			MessageID:   "msg_1",
			Origin:      "https://gallery.example",
			BrowserInfo: minter.BrowserInfo{Name: "Chrome"},
		},
	}
	relayerClient := &fakeRelayer{sent: make(chan relayer.Response, 4)}
	cdpClient := &fakeCDP{
		defaultNavigateStarted: make(chan struct{}, 1),
		releaseDefaultNavigate: make(chan struct{}),
	}
	s := newService(
		Options{
			Enabled:         true,
			BrokerBaseURL:   "https://broker.example",
			ApprovalTimeout: time.Minute,
			PollInterval:    time.Millisecond,
			RelayerBaseURL:  "https://relayer.example",
		},
		&fakeBrokerStarter{channel: ch},
		fakeSessionCreator{},
		relayerClient,
		cdpClient,
		wrapper.NewJSON(),
		zap.NewNop(),
	).(*service)
	s.Start(context.Background())

	result, err := s.HandleStartPairingSession(context.Background(), nil)
	require.NoError(t, err)
	assert.True(t, result.(startPairingResponse).OK)
	assertRelayerNotification(t, <-relayerClient.sent, relayer.NOTIFICATION_TYPE_MINT_PAIRING_APPROVAL_REQUEST)

	stopped := make(chan struct{})
	go func() {
		s.Stop()
		close(stopped)
	}()

	select {
	case <-stopped:
	case <-time.After(250 * time.Millisecond):
		t.Fatal("Stop waited for best-effort display restoration")
	}
	select {
	case <-cdpClient.defaultNavigateStarted:
	case <-time.After(time.Second):
		t.Fatal("expected default display restoration attempt")
	}
	close(cdpClient.releaseDefaultNavigate)
}

func TestWaitForBrowserAndApproval_RestoresDisplayAfterSessionCreateFailure(t *testing.T) {
	defer state.ResetForTesting()
	state.GetState().Relayer.TopicID = "topic-1"

	ch := &fakeBrokerChannel{
		pairingCode:   "PAIR-123",
		rejectionSent: make(chan struct{}, 1),
		request: &minter.MintRequest{
			ChannelID:   "ch_1",
			MessageID:   "msg_1",
			Origin:      "https://gallery.example",
			BrowserInfo: minter.BrowserInfo{Name: "Chrome"},
		},
	}
	relayerClient := &fakeRelayer{sent: make(chan relayer.Response, 4)}
	cdpClient := &fakeCDP{}
	s := newService(
		Options{
			Enabled:         true,
			BrokerBaseURL:   "https://broker.example",
			ApprovalTimeout: time.Minute,
			PollInterval:    time.Millisecond,
			RelayerBaseURL:  "https://relayer.example",
		},
		&fakeBrokerStarter{channel: ch},
		fakeSessionCreator{err: errors.New("relayer down")},
		relayerClient,
		cdpClient,
		wrapper.NewJSON(),
		zap.NewNop(),
	).(*service)
	s.Start(context.Background())
	defer s.Stop()

	result, err := s.HandleStartPairingSession(context.Background(), nil)
	require.NoError(t, err)
	assert.True(t, result.(startPairingResponse).OK)

	approval := <-relayerClient.sent
	assertRelayerNotification(t, approval, relayer.NOTIFICATION_TYPE_MINT_PAIRING_APPROVAL_REQUEST)
	approvalID := approval.Message.(map[string]any)["approvalRequestID"].(string)

	result, err = s.HandleApprovalDecision(context.Background(), validDecisionArgs(approvalID, "topic-1", "ch_1", "msg_1"))
	require.NoError(t, err)
	assert.Equal(t, "accepted", result.(approvalResponse).Status)

	select {
	case <-ch.rejectionSent:
	case <-time.After(time.Second):
		t.Fatal("expected session-create failure rejection")
	}

	ch.mu.Lock()
	assert.Equal(t, []string{"session_create_failed"}, ch.rejectionReasons)
	ch.mu.Unlock()

	outcome := <-relayerClient.sent
	assertRelayerNotification(t, outcome, relayer.NOTIFICATION_TYPE_MINT_PAIRING_APPROVAL_OUTCOME)
	assert.Equal(t, "failed", outcome.Message.(map[string]any)["status"])
	assertEventuallyDisplayObserved(t, cdpClient, "hidden", "", "")
}

func TestCompleteDecision_SendsSessionCreateFailureAfterContextCancellation(t *testing.T) {
	defer state.ResetForTesting()
	state.GetState().Relayer.TopicID = "topic-1"

	ch := &fakeBrokerChannel{rejectionSent: make(chan struct{}, 1)}
	creatorStarted := make(chan struct{}, 1)
	relayerClient := &fakeRelayer{sent: make(chan relayer.Response, 1)}
	s := newService(
		Options{RelayerBaseURL: "https://relayer.example"},
		nil,
		fakeSessionCreator{
			started: creatorStarted,
			release: make(chan struct{}),
		},
		relayerClient,
		&fakeCDP{},
		wrapper.NewJSON(),
		zap.NewNop(),
	).(*service)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct {
		terminalSent bool
		err          error
	}, 1)
	go func() {
		terminalSent, err := s.completeDecision(ctx, ch, minter.MintRequest{
			ChannelID:   "ch_1",
			MessageID:   "msg_1",
			BrowserInfo: minter.BrowserInfo{Name: "Chrome"},
		}, "topic-1", "mpa_1", approvalDecisionRequest{
			ApprovalRequestID: "mpa_1",
			TopicID:           "topic-1",
			ChannelID:         "ch_1",
			RequestMessageID:  "msg_1",
			Decision:          "approve",
		})
		done <- struct {
			terminalSent bool
			err          error
		}{terminalSent: terminalSent, err: err}
	}()

	select {
	case <-creatorStarted:
	case <-time.After(time.Second):
		t.Fatal("expected session creator to start")
	}
	cancel()

	select {
	case <-ch.rejectionSent:
	case <-time.After(time.Second):
		t.Fatal("expected session-create failure rejection after context cancellation")
	}

	result := <-done
	require.Error(t, result.err)
	assert.True(t, result.terminalSent)

	ch.mu.Lock()
	assert.Equal(t, []string{"session_create_failed"}, ch.rejectionReasons)
	ch.mu.Unlock()

	outcome := <-relayerClient.sent
	assertRelayerNotification(t, outcome, relayer.NOTIFICATION_TYPE_MINT_PAIRING_APPROVAL_OUTCOME)
	assert.Equal(t, "failed", outcome.Message.(map[string]any)["status"])
}

func TestCompleteDecision_RejectsStaleTopicBeforeCreatingSession(t *testing.T) {
	defer state.ResetForTesting()
	state.GetState().Relayer.TopicID = "topic-2"

	ch := &fakeBrokerChannel{rejectionSent: make(chan struct{}, 1)}
	creator := &recordingSessionCreator{}
	relayerClient := &fakeRelayer{sent: make(chan relayer.Response, 1)}
	s := newService(
		Options{RelayerBaseURL: "https://relayer.example"},
		nil,
		creator,
		relayerClient,
		nil,
		wrapper.NewJSON(),
		zap.NewNop(),
	).(*service)

	terminalSent, err := s.completeDecision(context.Background(), ch, minter.MintRequest{
		ChannelID: "ch_1",
		MessageID: "msg_1",
	}, "topic-1", "mpa_1", approvalDecisionRequest{
		ApprovalRequestID: "mpa_1",
		TopicID:           "topic-1",
		ChannelID:         "ch_1",
		RequestMessageID:  "msg_1",
		Decision:          "approve",
	})

	require.Error(t, err)
	assert.True(t, terminalSent)
	assert.Zero(t, creator.calls, "stale-topic approval must not mint a relayer session")

	ch.mu.Lock()
	assert.Equal(t, []string{"topic_changed"}, ch.rejectionReasons)
	assert.Equal(t, 0, ch.successCount)
	ch.mu.Unlock()

	outcome := <-relayerClient.sent
	assertRelayerNotification(t, outcome, relayer.NOTIFICATION_TYPE_MINT_PAIRING_APPROVAL_OUTCOME)
	assert.Equal(t, "failed", outcome.Message.(map[string]any)["status"])
}

func TestWaitForBrowserAndApproval_RejectsIfTopicChangesAfterApproval(t *testing.T) {
	defer state.ResetForTesting()
	state.GetState().Relayer.TopicID = "topic-1"

	ch := &fakeBrokerChannel{
		pairingCode:   "PAIR-123",
		rejectionSent: make(chan struct{}, 1),
		successSent:   make(chan struct{}, 1),
		request: &minter.MintRequest{
			ChannelID:   "ch_1",
			MessageID:   "msg_1",
			Origin:      "https://gallery.example",
			BrowserInfo: minter.BrowserInfo{Name: "Chrome"},
		},
	}
	creatorStarted := make(chan struct{}, 1)
	creatorRelease := make(chan struct{})
	relayerClient := &fakeRelayer{sent: make(chan relayer.Response, 4)}
	s := newService(
		Options{
			Enabled:         true,
			BrokerBaseURL:   "https://broker.example",
			ApprovalTimeout: time.Minute,
			PollInterval:    time.Millisecond,
			RelayerBaseURL:  "https://relayer.example",
		},
		&fakeBrokerStarter{channel: ch},
		fakeSessionCreator{started: creatorStarted, release: creatorRelease},
		relayerClient,
		&fakeCDP{},
		wrapper.NewJSON(),
		zap.NewNop(),
	).(*service)
	s.Start(context.Background())
	defer s.Stop()

	result, err := s.HandleStartPairingSession(context.Background(), nil)
	require.NoError(t, err)
	assert.True(t, result.(startPairingResponse).OK)

	approval := <-relayerClient.sent
	assertRelayerNotification(t, approval, relayer.NOTIFICATION_TYPE_MINT_PAIRING_APPROVAL_REQUEST)
	approvalID := approval.Message.(map[string]any)["approvalRequestID"].(string)

	result, err = s.HandleApprovalDecision(context.Background(), validDecisionArgs(approvalID, "topic-1", "ch_1", "msg_1"))
	require.NoError(t, err)
	assert.Equal(t, "accepted", result.(approvalResponse).Status)

	select {
	case <-creatorStarted:
	case <-time.After(time.Second):
		t.Fatal("expected session creator to start")
	}
	state.GetState().Relayer.TopicID = "topic-2"
	close(creatorRelease)

	select {
	case <-ch.rejectionSent:
	case <-ch.successSent:
		t.Fatal("stale-topic approval delivered browser success")
	case <-time.After(time.Second):
		t.Fatal("expected topic-changed rejection")
	}

	ch.mu.Lock()
	assert.Equal(t, []string{"topic_changed"}, ch.rejectionReasons)
	assert.Equal(t, 0, ch.successCount)
	ch.mu.Unlock()

	outcome := <-relayerClient.sent
	assertRelayerNotification(t, outcome, relayer.NOTIFICATION_TYPE_MINT_PAIRING_APPROVAL_OUTCOME)
	assert.Equal(t, "failed", outcome.Message.(map[string]any)["status"])
}

func TestWaitForMintRequest_AdvancesCursorPastIgnoredPollResult(t *testing.T) {
	s := newTestService()
	ch := &fakeBrokerChannel{
		ignoredSeq: 4,
		request: &minter.MintRequest{
			ChannelID: "ch_1",
			MessageID: "msg_1",
			Seq:       5,
		},
	}

	request, err := s.waitForMintRequest(context.Background(), ch)

	require.NoError(t, err)
	require.NotNil(t, request)
	assert.Equal(t, int64(5), request.Seq)
	assert.Equal(t, []int64{0, 4}, ch.pollAfterSeqs)
}

func TestWaitForBrowserAndApproval_AcceptedDecisionBeforeExpiryWinsAfterSessionDeadline(t *testing.T) {
	defer state.ResetForTesting()
	state.GetState().Relayer.TopicID = "topic-1"

	ch := &fakeBrokerChannel{
		pairingCode: "PAIR-123",
		expiresAt:   time.Now().Add(300 * time.Millisecond),
		successSent: make(chan struct{}, 1),
		request: &minter.MintRequest{
			ChannelID:   "ch_1",
			MessageID:   "msg_1",
			Origin:      "https://gallery.example",
			BrowserInfo: minter.BrowserInfo{Name: "Chrome"},
		},
	}
	relayerClient := &fakeRelayer{sent: make(chan relayer.Response, 4)}
	s := newService(
		Options{
			Enabled:         true,
			BrokerBaseURL:   "https://broker.example",
			ApprovalTimeout: time.Minute,
			PollInterval:    time.Millisecond,
			RelayerBaseURL:  "https://relayer.example",
		},
		&fakeBrokerStarter{channel: ch},
		fakeSessionCreator{delay: 220 * time.Millisecond},
		relayerClient,
		&fakeCDP{},
		wrapper.NewJSON(),
		zap.NewNop(),
	).(*service)
	s.Start(context.Background())
	defer s.Stop()

	result, err := s.HandleStartPairingSession(context.Background(), nil)
	require.NoError(t, err)
	assert.True(t, result.(startPairingResponse).OK)

	approval := <-relayerClient.sent
	assertRelayerNotification(t, approval, relayer.NOTIFICATION_TYPE_MINT_PAIRING_APPROVAL_REQUEST)
	approvalID := approval.Message.(map[string]any)["approvalRequestID"].(string)

	time.Sleep(220 * time.Millisecond)
	result, err = s.HandleApprovalDecision(context.Background(), validDecisionArgs(approvalID, "topic-1", "ch_1", "msg_1"))
	require.NoError(t, err)
	assert.Equal(t, "accepted", result.(approvalResponse).Status)

	select {
	case <-ch.successSent:
	case <-time.After(time.Second):
		t.Fatal("expected accepted decision to produce browser success")
	}

	ch.mu.Lock()
	assert.Equal(t, 1, ch.successCount)
	assert.NotContains(t, ch.rejectionReasons, "approval_expired")
	assert.Equal(t, 0, ch.closeCount, "terminal success must remain pollable for the browser")
	ch.mu.Unlock()

	outcome := <-relayerClient.sent
	assertRelayerNotification(t, outcome, relayer.NOTIFICATION_TYPE_MINT_PAIRING_APPROVAL_OUTCOME)
	assert.Equal(t, "completed", outcome.Message.(map[string]any)["status"])
}

func newTestService() *service {
	return newService(
		Options{ApprovalTimeout: time.Minute, PollInterval: time.Millisecond},
		nil,
		nil,
		nil,
		nil,
		wrapper.NewJSON(),
		zap.NewNop(),
	).(*service)
}

func validDecisionArgs(approvalID, topicID, channelID, messageID string) map[string]any {
	return map[string]any{
		"v":                 float64(1),
		"approvalRequestID": approvalID,
		"topicID":           topicID,
		"channelID":         channelID,
		"requestMessageID":  messageID,
		"decision":          "approve",
	}
}

func assertCommandError(t *testing.T, result any, code string, retryable bool) {
	t.Helper()
	resp, ok := result.(map[string]any)
	require.True(t, ok)
	assert.Equal(t, false, resp["ok"])
	errPayload, ok := resp["error"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, code, errPayload["code"])
	assert.Equal(t, retryable, errPayload["retryable"])
}

type fakeBrokerStarter struct {
	channel         brokerChannel
	channels        []brokerChannel
	err             error
	receivedOptions minter.StartChannelOptions
	startCount      int
}

func (f *fakeBrokerStarter) StartChannel(_ context.Context, opts minter.StartChannelOptions) (brokerChannel, error) {
	f.receivedOptions = opts
	f.startCount++
	if len(f.channels) > 0 {
		channel := f.channels[0]
		f.channels = f.channels[1:]
		return channel, f.err
	}
	return f.channel, f.err
}

type fakeBrokerChannel struct {
	mu               sync.Mutex
	channelID        string
	pairingCode      string
	expiresAt        time.Time
	request          *minter.MintRequest
	rejectionSent    chan struct{}
	rejectionStarted chan struct{}
	rejectionRelease chan struct{}
	successSent      chan struct{}
	closed           chan struct{}
	rejectionDelay   time.Duration
	ignoredSeq       int64
	pollAfterSeqs    []int64
	successCount     int
	closeCount       int
	rejectionReasons []string
}

func (f *fakeBrokerChannel) PairingDisplay() minter.PairingDisplay {
	expiresAt := f.expiresAt
	if expiresAt.IsZero() {
		expiresAt = time.Now().Add(time.Minute)
	}
	return minter.PairingDisplay{ChannelID: f.resolvedChannelID(), ShortCode: f.pairingCode, ExpiresAt: expiresAt}
}

func (f *fakeBrokerChannel) MinterPublicKeyJWK() minter.PublicJWK {
	return minter.PublicJWK{}
}

func (f *fakeBrokerChannel) PollMintRequest(_ context.Context, afterSeq int64) (*minter.MintRequest, int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pollAfterSeqs = append(f.pollAfterSeqs, afterSeq)
	if f.ignoredSeq > afterSeq {
		nextSeq := f.ignoredSeq
		f.ignoredSeq = 0
		return nil, nextSeq, nil
	}
	if f.request == nil {
		return nil, afterSeq, nil
	}
	request := f.request
	f.request = nil
	return request, request.Seq, nil
}

func (f *fakeBrokerChannel) SendMintSuccess(context.Context, minter.MintRequest, minter.MintResult) (*minter.SendMessageResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.successCount++
	if f.successSent != nil {
		select {
		case f.successSent <- struct{}{}:
		default:
		}
	}
	return &minter.SendMessageResult{ChannelID: f.resolvedChannelID(), Seq: 1, ExpiresAt: time.Now().Add(time.Minute)}, nil
}

func (f *fakeBrokerChannel) SendMintRejection(ctx context.Context, _ minter.MintRequest, rejection minter.MintRejection) (*minter.SendMessageResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if f.rejectionStarted != nil {
		select {
		case f.rejectionStarted <- struct{}{}:
		default:
		}
	}
	if f.rejectionRelease != nil {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-f.rejectionRelease:
		}
	}
	if f.rejectionDelay > 0 {
		timer := time.NewTimer(f.rejectionDelay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rejectionReasons = append(f.rejectionReasons, rejection.Reason)
	if f.rejectionSent != nil {
		select {
		case f.rejectionSent <- struct{}{}:
		default:
		}
	}
	return &minter.SendMessageResult{ChannelID: f.resolvedChannelID(), Seq: 1, ExpiresAt: time.Now().Add(time.Minute)}, nil
}

func (f *fakeBrokerChannel) Close(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closeCount++
	if f.closed != nil {
		select {
		case f.closed <- struct{}{}:
		default:
		}
	}
	return nil
}

func (f *fakeBrokerChannel) resolvedChannelID() string {
	if f.channelID != "" {
		return f.channelID
	}
	return "ch_1"
}

type recordingSessionCreator struct {
	calls int
}

func (r *recordingSessionCreator) CreateEphemeralSession(context.Context, string, minter.MintRequest) (minter.MintResult, error) {
	r.calls++
	return minter.MintResult{
		SessionID: "session-1",
		Token:     "browser-token",
		ExpiresAt: time.Now().Add(time.Hour),
	}, nil
}

type fakeSessionCreator struct {
	delay   time.Duration
	started chan struct{}
	release chan struct{}
	err     error
}

func (f fakeSessionCreator) CreateEphemeralSession(ctx context.Context, _ string, _ minter.MintRequest) (minter.MintResult, error) {
	if f.started != nil {
		select {
		case f.started <- struct{}{}:
		default:
		}
	}
	if f.release != nil {
		select {
		case <-ctx.Done():
			return minter.MintResult{}, ctx.Err()
		case <-f.release:
		}
	}
	if f.delay > 0 {
		timer := time.NewTimer(f.delay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return minter.MintResult{}, ctx.Err()
		case <-timer.C:
		}
	}
	if f.err != nil {
		return minter.MintResult{}, f.err
	}
	return minter.MintResult{
		SessionID: "session-1",
		Token:     "browser-token",
		ExpiresAt: time.Now().Add(time.Hour),
	}, nil
}

type fakeRelayer struct {
	sent chan relayer.Response
}

func (f *fakeRelayer) IsConnected() bool { return true }
func (f *fakeRelayer) Connect(context.Context) error {
	return nil
}
func (f *fakeRelayer) RetryableConnect(context.Context) error {
	return nil
}
func (f *fakeRelayer) Send(_ context.Context, data interface{}) error {
	response, ok := data.(relayer.Response)
	if ok {
		f.sent <- response
	}
	return nil
}
func (f *fakeRelayer) OnRelayerMessage(relayer.Handler)     {}
func (f *fakeRelayer) RemoveRelayerMessage(relayer.Handler) {}
func (f *fakeRelayer) Close()                               {}

type fakeCDP struct {
	mu                     sync.Mutex
	displayRequests        []map[string]any
	err                    error
	appResponse            any
	appResponseForRequest  func(map[string]any) any
	defaultNavigateStarted chan struct{}
	releaseDefaultNavigate chan struct{}
}

func (f *fakeCDP) Init(context.Context) error { return nil }
func (f *fakeCDP) Send(string, map[string]interface{}) (interface{}, error) {
	return nil, nil
}
func (f *fakeCDP) NoLogSend(method string, params map[string]interface{}) (interface{}, error) {
	if f.err != nil {
		return nil, f.err
	}
	if method == cdp.METHOD_EVALUATE {
		request, ok := mintPairingDisplayRequest(params)
		if !ok {
			return f.mintPairingDisplayResponse(nil), nil
		}

		if request["state"] == "hidden" && f.releaseDefaultNavigate != nil {
			if f.defaultNavigateStarted != nil {
				select {
				case f.defaultNavigateStarted <- struct{}{}:
				default:
				}
			}
			<-f.releaseDefaultNavigate
		}

		f.mu.Lock()
		f.displayRequests = append(f.displayRequests, request)
		f.mu.Unlock()
		return f.mintPairingDisplayResponse(request), nil
	}
	return f.mintPairingDisplayResponse(nil), nil
}
func (f *fakeCDP) PageNavigationURL(context.Context) (string, error) {
	return "", nil
}
func (f *fakeCDP) Close()            {}
func (f *fakeCDP) Initialized() bool { return true }

var _ cdp.CDP = (*fakeCDP)(nil)

func (f *fakeCDP) mintPairingDisplayResponse(request map[string]any) any {
	if f.appResponseForRequest != nil {
		return f.appResponseForRequest(request)
	}
	if f.appResponse != nil {
		return f.appResponse
	}
	return map[string]any{"ok": true}
}

func mintPairingDisplayRequest(params map[string]interface{}) (map[string]any, bool) {
	expression, _ := params["expression"].(string)
	const prefix = "window.handleCDPRequest("
	if !strings.HasPrefix(expression, prefix) || !strings.HasSuffix(expression, ")") {
		return nil, false
	}

	raw := strings.TrimSuffix(strings.TrimPrefix(expression, prefix), ")")
	var payload struct {
		Command string         `json:"command"`
		Request map[string]any `json:"request"`
	}
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		return nil, false
	}
	if payload.Command != "mintPairingDisplay" || payload.Request == nil {
		return nil, false
	}
	return payload.Request, true
}

func (f *fakeCDP) displayRequestsSnapshot() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	copied := make([]map[string]any, len(f.displayRequests))
	copy(copied, f.displayRequests)
	return copied
}

func assertRelayerNotification(t *testing.T, response relayer.Response, notificationType relayer.NotificationType) {
	t.Helper()
	assert.Equal(t, "notification", response.Type)
	assert.Equal(t, string(notificationType), response.NotificationType)
	assert.Equal(t, 10, response.PersistRecordCount)
}

func assertEventuallyDisplayObserved(t *testing.T, cdpClient *fakeCDP, state string, pairingCode string, browserName string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if displayObserved(cdpClient.displayRequestsSnapshot(), state, pairingCode, browserName) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	assert.Failf(t, "mint pairing display state not observed", "state=%q pairingCode=%q browserName=%q requests=%v", state, pairingCode, browserName, cdpClient.displayRequestsSnapshot())
}

func assertLastDisplay(t *testing.T, cdpClient *fakeCDP, state string, pairingCode string, browserName string) {
	t.Helper()
	requests := cdpClient.displayRequestsSnapshot()
	require.NotEmpty(t, requests)
	last := requests[len(requests)-1]
	assertDisplayRequest(t, last, state, pairingCode, browserName)
}

func countDisplayRequests(cdpClient *fakeCDP, state string, pairingCode string, browserName string) int {
	count := 0
	for _, request := range cdpClient.displayRequestsSnapshot() {
		if requestMatchesDisplay(request, state, pairingCode, browserName) {
			count++
		}
	}
	return count
}

func displayObserved(requests []map[string]any, state string, pairingCode string, browserName string) bool {
	for _, request := range requests {
		if requestMatchesDisplay(request, state, pairingCode, browserName) {
			return true
		}
	}
	return false
}

func assertDisplayRequest(t *testing.T, request map[string]any, state string, pairingCode string, browserName string) {
	t.Helper()
	assert.True(t, requestMatchesDisplay(request, state, pairingCode, browserName), "request=%v", request)
}

func requestMatchesDisplay(request map[string]any, state string, pairingCode string, browserName string) bool {
	if request["state"] != state {
		return false
	}
	if pairingCode != "" && request["pairingCode"] != pairingCode {
		return false
	}
	if browserName != "" && request["browserName"] != browserName {
		return false
	}
	return true
}

func writeValidPlayerContract(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ffos-player-contract.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"contracts":{"mintPairingDisplay":{"version":1,"requestKey":"request","states":["pairing_code","request_received","creating_token","hidden"],"acceptedResponse":{"ok":true}}}}`), 0o600))
	return path
}
