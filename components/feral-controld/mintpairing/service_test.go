package mintpairing

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	minter "github.com/feral-file/ff-art-computer-handoff/clients/ephemeral-token-minter/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-controld/cdp"
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
	require.Contains(t, cdpClient.lastURL, "pairing_code=")
	assert.True(t, strings.Contains(cdpClient.lastURL, "PAIR-123"))

	approval := <-relayerClient.sent
	require.Equal(t, "mint_pairing_approval_request", approval.Type)
	approvalMessage := approval.Message.(map[string]any)
	approvalID := approvalMessage["approvalRequestID"].(string)

	decisionArgs := validDecisionArgs(approvalID, "topic-1", "ch_1", "msg_1")
	result, err = s.HandleApprovalDecision(context.Background(), decisionArgs)
	require.NoError(t, err)
	assert.Equal(t, "accepted", result.(approvalResponse).Status)

	outcome := <-relayerClient.sent
	assert.Equal(t, "mint_pairing_approval_outcome", outcome.Type)
	assert.Equal(t, 1, ch.successCount)
	assert.Equal(t, 0, ch.closeCount, "terminal success must remain pollable for the browser")

	result, err = s.HandleApprovalDecision(context.Background(), decisionArgs)
	require.NoError(t, err)
	assert.Equal(t, "already_accepted", result.(approvalResponse).Status)
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

	require.Equal(t, "mint_pairing_approval_request", (<-relayerClient.sent).Type)

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
	assert.Equal(t, "mint_pairing_approval_outcome", outcome.Type)
	assert.Equal(t, "expired", outcome.Message.(map[string]any)["status"])
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
	require.Equal(t, "mint_pairing_approval_request", approval.Type)
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
	assert.Equal(t, "mint_pairing_approval_outcome", outcome.Type)
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
	err             error
	receivedOptions minter.StartChannelOptions
}

func (f *fakeBrokerStarter) StartChannel(_ context.Context, opts minter.StartChannelOptions) (brokerChannel, error) {
	f.receivedOptions = opts
	return f.channel, f.err
}

type fakeBrokerChannel struct {
	mu               sync.Mutex
	pairingCode      string
	expiresAt        time.Time
	request          *minter.MintRequest
	rejectionSent    chan struct{}
	successSent      chan struct{}
	closed           chan struct{}
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
	return minter.PairingDisplay{ChannelID: "ch_1", ShortCode: f.pairingCode, ExpiresAt: expiresAt}
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
	return &minter.SendMessageResult{ChannelID: "ch_1", Seq: 1, ExpiresAt: time.Now().Add(time.Minute)}, nil
}

func (f *fakeBrokerChannel) SendMintRejection(ctx context.Context, _ minter.MintRequest, rejection minter.MintRejection) (*minter.SendMessageResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
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
	return &minter.SendMessageResult{ChannelID: "ch_1", Seq: 1, ExpiresAt: time.Now().Add(time.Minute)}, nil
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

type fakeSessionCreator struct {
	delay time.Duration
}

func (f fakeSessionCreator) CreateEphemeralSession(ctx context.Context, _ string, _ minter.MintRequest) (minter.MintResult, error) {
	if f.delay > 0 {
		timer := time.NewTimer(f.delay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return minter.MintResult{}, ctx.Err()
		case <-timer.C:
		}
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
	lastURL string
	err     error
}

func (f *fakeCDP) Init(context.Context) error { return nil }
func (f *fakeCDP) Send(string, map[string]interface{}) (interface{}, error) {
	return nil, nil
}
func (f *fakeCDP) NoLogSend(method string, params map[string]interface{}) (interface{}, error) {
	if f.err != nil {
		return nil, f.err
	}
	if method == "Page.navigate" {
		f.lastURL, _ = params["url"].(string)
	}
	return map[string]interface{}{"ok": true}, nil
}
func (f *fakeCDP) PageNavigationURL(context.Context) (string, error) {
	return "", nil
}
func (f *fakeCDP) Close()            {}
func (f *fakeCDP) Initialized() bool { return true }

var _ cdp.CDP = (*fakeCDP)(nil)
