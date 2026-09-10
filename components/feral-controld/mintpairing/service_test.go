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
	"go.uber.org/zap/zaptest/observer"

	"github.com/feral-file/ffos-user/components/feral-controld/cdp"
	"github.com/feral-file/ffos-user/components/feral-controld/config"
	"github.com/feral-file/ffos-user/components/feral-controld/playersession"
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
		guard:             topicGuard{topicID: "topic-1"},
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
		guard:             topicGuard{topicID: "topic-1"},
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

func TestHandleApprovalDecision_KeepPairedTravelsWithTheApproval(t *testing.T) {
	defer state.ResetForTesting()
	state.GetState().Relayer.TopicID = "topic-1"

	s := newTestService()
	pending := &pendingApproval{
		approvalRequestID: "mpa_1",
		guard:             topicGuard{topicID: "topic-1"},
		channelID:         "ch_1",
		requestMessageID:  "msg_1",
		expiresAt:         time.Now().Add(time.Minute),
		decisionCh:        make(chan approvalDecisionRequest, 1),
	}
	s.registerPending(pending)

	args := validDecisionArgs("mpa_1", "topic-1", "ch_1", "msg_1")
	args["keepPaired"] = true

	result, err := s.HandleApprovalDecision(context.Background(), args)
	require.NoError(t, err)
	assert.Equal(t, "accepted", result.(approvalResponse).Status)

	select {
	case decision := <-pending.decisionCh:
		assert.True(t, decision.KeepPaired)
	default:
		t.Fatal("expected accepted decision to be delivered")
	}

	// The same decision replayed is idempotent; the same approval with the
	// owner's keep choice flipped is a different decision, not a duplicate.
	result, err = s.HandleApprovalDecision(context.Background(), args)
	require.NoError(t, err)
	assert.Equal(t, "already_accepted", result.(approvalResponse).Status)

	delete(args, "keepPaired")
	result, err = s.HandleApprovalDecision(context.Background(), args)
	require.NoError(t, err)
	resp := result.(approvalResponse)
	require.NotNil(t, resp.Error)
	assert.Equal(t, "already_decided", resp.Error.Code)
}

func TestParseDecision_KeepPairedDefaultsOffAndOnlyAppliesToApprovals(t *testing.T) {
	s := newTestService()

	args := validDecisionArgs("mpa_1", "topic-1", "ch_1", "msg_1")
	decision, err := s.parseDecision(args)
	require.NoError(t, err)
	assert.False(t, decision.KeepPaired, "keepPaired defaults to false")

	args["keepPaired"] = true
	decision, err = s.parseDecision(args)
	require.NoError(t, err)
	assert.True(t, decision.KeepPaired)

	args["decision"] = "reject"
	decision, err = s.parseDecision(args)
	require.NoError(t, err)
	assert.False(t, decision.KeepPaired, "keepPaired is meaningless on a rejection")

	args["decision"] = "approve"
	for _, bad := range []any{"yes", "true", nil, float64(1), map[string]any{}} {
		args["keepPaired"] = bad
		_, err = s.parseDecision(args)
		require.Errorf(t, err, "keepPaired %#v must be rejected, not read as false", bad)
	}

	// An explicitly null flag is malformed, not a silent "do not keep".
	args["keepPaired"] = nil
	result, err := s.HandleApprovalDecision(context.Background(), args)
	require.NoError(t, err)
	resp, ok := result.(approvalResponse)
	require.True(t, ok)
	require.NotNil(t, resp.Error)
	assert.Equal(t, "invalid_request", resp.Error.Code)
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
	}, lifetimeTimed)

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
			}, lifetimeTimed)

			require.NoError(t, err)
			assert.Equal(t, float64(tt.want), body["expiresInSeconds"])
		})
	}
}

func TestRelayerSessionCreator_KeepPairedAsksForAPersistentSessionWithoutTTL(t *testing.T) {
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"session":{"id":"session-1","persistent":true,"expiresAt":null},"token":"browser-token"}`))
	}))
	defer server.Close()

	creator := NewRelayerSessionCreator(server.URL, "", wrapper.NewHTTPClient(), wrapper.NewJSON())
	session, err := creator.CreateEphemeralSession(context.Background(), "topic-1", minter.MintRequest{
		BrowserInfo:               minter.BrowserInfo{Name: "Chrome", Label: "Gallery laptop"},
		RequestedExpiresInSeconds: 300,
	}, lifetimePersistent)

	require.NoError(t, err)
	assert.Equal(t, true, body["persistent"])
	assert.NotContains(t, body, "expiresInSeconds", "an owner-kept session carries no TTL")
	assert.Equal(t, "Gallery laptop", body["label"])
	assert.True(t, session.Persistent)
	assert.True(t, session.ExpiresAt.IsZero(), "a persistent session never expires")
	assert.Equal(t, "browser-token", session.Token)
}

func TestRelayerSessionCreator_RequesterFallbackAsksForTheLongestTimedSession(t *testing.T) {
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"session":{"id":"session-1","expiresAt":"2030-01-01T00:00:00Z"},"token":"browser-token"}`))
	}))
	defer server.Close()

	creator := NewRelayerSessionCreator(server.URL, "", wrapper.NewHTTPClient(), wrapper.NewJSON())
	session, err := creator.CreateEphemeralSession(context.Background(), "topic-1", minter.MintRequest{
		RequestedExpiresInSeconds: 300,
	}, lifetimeTimedFallbackRequester)

	require.NoError(t, err)
	assert.NotContains(t, body, "persistent", "an incapable requester is never offered a persistent session")
	assert.Equal(t, float64(maxSessionTTLSeconds), body["expiresInSeconds"],
		"the fallback outranks the browser's own request, the way keepPaired would have")
	assert.False(t, session.Persistent)
}

func TestRelayerSessionCreator_PersistenceFollowsTheRelayerAnswer(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, json.NewDecoder(r.Body).Decode(&map[string]any{}))
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"session":{"id":"session-1","expiresAt":"2030-01-01T00:00:00Z"},"token":"browser-token"}`))
	}))
	defer server.Close()

	creator := NewRelayerSessionCreator(server.URL, "", wrapper.NewHTTPClient(), wrapper.NewJSON())
	session, err := creator.CreateEphemeralSession(context.Background(), "topic-1", minter.MintRequest{}, lifetimePersistent)

	require.NoError(t, err)
	assert.False(t, session.Persistent, "a relayer that ignored persistent minted an expiring session")
	assert.False(t, session.ExpiresAt.IsZero())
}

func TestRelayerSessionCreator_RefusesAndRevokesAContradictoryReply(t *testing.T) {
	tests := []struct {
		name     string
		lifetime sessionLifetime
		reply    string
		reason   string
	}{
		{
			name:     "persistent with an expiry",
			lifetime: lifetimePersistent,
			reply:    `{"session":{"id":"session-1","persistent":true,"expiresAt":"2030-01-01T00:00:00Z"},"token":"browser-token"}`,
		},
		{
			// A present-but-zero expiry is a deadline the relayer failed to
			// write, not a null one, so it must not pass as persistent.
			name:     "persistent with a zero expiry",
			lifetime: lifetimePersistent,
			reply:    `{"session":{"id":"session-1","persistent":true,"expiresAt":"0001-01-01T00:00:00Z"},"token":"browser-token"}`,
		},
		{
			name:  "timed with no expiry",
			reply: `{"session":{"id":"session-1","expiresAt":null},"token":"browser-token"}`,
		},
		{
			// A relayer must not grant more than was asked: nobody chose to
			// keep this site paired.
			name:   "persistent reply to a request that did not ask to keep",
			reply:  `{"session":{"id":"session-1","persistent":true},"token":"browser-token"}`,
			reason: "did not ask to keep the site paired",
		},
		{
			name:     "persistent reply to the requester fallback",
			lifetime: lifetimeTimedFallbackRequester,
			reply:    `{"session":{"id":"session-1","persistent":true},"token":"browser-token"}`,
			reason:   "did not ask to keep the site paired",
		},
		{
			// An id with no token is a session the relayer allocated and the
			// device can never use: it must not be stranded.
			name:   "id with no token",
			reply:  `{"session":{"id":"session-1","expiresAt":"2030-01-01T00:00:00Z"},"token":""}`,
			reason: "token",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var revoked []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodDelete {
					revoked = append(revoked, r.URL.Path+"?topicID="+r.URL.Query().Get("topicID"))
					w.WriteHeader(http.StatusNoContent)
					return
				}
				w.WriteHeader(http.StatusCreated)
				_, _ = w.Write([]byte(tt.reply))
			}))
			defer server.Close()

			creator := NewRelayerSessionCreator(server.URL, "", wrapper.NewHTTPClient(), wrapper.NewJSON())
			_, err := creator.CreateEphemeralSession(context.Background(), "topic-1", minter.MintRequest{}, tt.lifetime)

			require.Error(t, err, "a contradictory relayer reply is refused")
			reason := tt.reason
			if reason == "" {
				reason = "expiresAt"
			}
			assert.Contains(t, err.Error(), reason)
			assert.Equal(t, []string{"/api/ephemeral-sessions/session-1?topicID=topic-1"}, revoked,
				"the committed session is revoked, not left holding a slot")
		})
	}
}

func TestRelayerSessionCreator_RefusesAReplyWithNoSessionID(t *testing.T) {
	var deletes int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			deletes++
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"session":{"expiresAt":"2030-01-01T00:00:00Z"},"token":"browser-token"}`))
	}))
	defer server.Close()

	creator := NewRelayerSessionCreator(server.URL, "", wrapper.NewHTTPClient(), wrapper.NewJSON())
	_, err := creator.CreateEphemeralSession(context.Background(), "topic-1", minter.MintRequest{}, lifetimeTimed)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "session id")
	assert.Zero(t, deletes, "there is no id to revoke")
}

func TestRelayerSessionCreator_ReportsAFailedRevokeOfARefusedSession(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"session":{"id":"session-1","persistent":true,"expiresAt":"2030-01-01T00:00:00Z"},"token":"browser-token"}`))
	}))
	defer server.Close()

	creator := NewRelayerSessionCreator(server.URL, "", wrapper.NewHTTPClient(), wrapper.NewJSON())
	_, err := creator.CreateEphemeralSession(context.Background(), "topic-1", minter.MintRequest{}, lifetimePersistent)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "expiresAt", "the refusal reason survives")
	assert.Contains(t, err.Error(), "revoking the refused session failed")
}

func TestRelayerSessionCreator_RevokeEphemeralSession(t *testing.T) {
	var seen struct {
		Method  string
		Path    string
		TopicID string
		APIKey  string
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.Method = r.Method
		seen.Path = r.URL.Path
		seen.TopicID = r.URL.Query().Get("topicID")
		seen.APIKey = r.Header.Get("API-KEY")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	creator := NewRelayerSessionCreator(server.URL, "api-key-1", wrapper.NewHTTPClient(), wrapper.NewJSON())
	require.NoError(t, creator.RevokeEphemeralSession(context.Background(), "topic-1", "session-1"))

	assert.Equal(t, http.MethodDelete, seen.Method)
	assert.Equal(t, "/api/ephemeral-sessions/session-1", seen.Path)
	assert.Equal(t, "topic-1", seen.TopicID)
	assert.Equal(t, "api-key-1", seen.APIKey)
}

func TestRelayerSessionCreator_ListEphemeralSessionIDs(t *testing.T) {
	tests := []struct {
		name  string
		reply string
	}{
		{
			name:  "wrapped in a sessions array",
			reply: `{"sessions":[{"id":"session-1","persistent":true},{"id":"session-2","expiresAt":"2030-01-01T00:00:00Z"}]}`,
		},
		{
			name:  "bare array",
			reply: `[{"id":"session-1"},{"id":"session-2"}]`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var seen struct {
				Method  string
				Path    string
				TopicID string
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				seen.Method = r.Method
				seen.Path = r.URL.Path
				seen.TopicID = r.URL.Query().Get("topicID")
				_, _ = w.Write([]byte(tt.reply))
			}))
			defer server.Close()

			creator := NewRelayerSessionCreator(server.URL, "", wrapper.NewHTTPClient(), wrapper.NewJSON())
			ids, err := creator.ListEphemeralSessionIDs(context.Background(), "topic-1")

			require.NoError(t, err)
			assert.Equal(t, []string{"session-1", "session-2"}, ids)
			assert.Equal(t, http.MethodGet, seen.Method)
			assert.Equal(t, "/api/ephemeral-sessions", seen.Path)
			assert.Equal(t, "topic-1", seen.TopicID)
		})
	}
}

func TestRelayerSessionCreator_ListEphemeralSessionIDsReportsAFailedList(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	creator := NewRelayerSessionCreator(server.URL, "", wrapper.NewHTTPClient(), wrapper.NewJSON())
	_, err := creator.ListEphemeralSessionIDs(context.Background(), "topic-1")

	require.Error(t, err, "an unreadable list must never look like an empty one")
}

func TestRelayerSessionCreator_RevokeTreatsAMissingSessionAsRevoked(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	creator := NewRelayerSessionCreator(server.URL, "", wrapper.NewHTTPClient(), wrapper.NewJSON())
	assert.NoError(t, creator.RevokeEphemeralSession(context.Background(), "topic-1", "session-1"))

	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer failing.Close()

	creator = NewRelayerSessionCreator(failing.URL, "", wrapper.NewHTTPClient(), wrapper.NewJSON())
	assert.Error(t, creator.RevokeEphemeralSession(context.Background(), "topic-1", "session-1"))
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
		name      string
		manifest  string
		missing   bool
		wantCode  string
		wantRetry bool
	}{
		{
			// A missing file IS the unreadable case (§: ErrPlayerContractUnreadable)
			// — the shape a pairing request during an OTA bundle swap or
			// boot-ordering race takes — so it must be retryable, not the
			// permanent invalid_config a genuinely malformed/incomplete
			// manifest gets.
			name:      "missing manifest is retryable (unreadable, not invalid)",
			missing:   true,
			wantCode:  "player_contract_unreadable",
			wantRetry: true,
		},
		{
			name:     "malformed json",
			manifest: `{`,
			wantCode: "invalid_config",
		},
		{
			name:     "wrong contract path with loose token",
			manifest: `{"contracts":{"other":{"version":1,"requestKey":"request","states":["pairing_code","request_received","creating_token","hidden"],"acceptedResponse":{"ok":true}}},"loose":"mintPairingDisplay"}`,
			wantCode: "invalid_config",
		},
		{
			name:     "missing state",
			manifest: `{"contracts":{"mintPairingDisplay":{"version":1,"requestKey":"request","states":["pairing_code","request_received","creating_token"],"acceptedResponse":{"ok":true}}}}`,
			wantCode: "invalid_config",
		},
		{
			name:     "wrong accepted response",
			manifest: `{"contracts":{"mintPairingDisplay":{"version":1,"requestKey":"request","states":["pairing_code","request_received","creating_token","hidden"],"acceptedResponse":{"ok":false}}}}`,
			wantCode: "invalid_config",
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
			assertCommandError(t, result, tt.wantCode, tt.wantRetry)
			assert.Equal(t, 0, starter.StartCount())
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
	assert.Equal(t, 1, starter.StartCount())
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

func TestHandleClosePairingSession_CancelsActivePairingAndClosesChannel(t *testing.T) {
	defer state.ResetForTesting()
	state.GetState().Relayer.TopicID = "topic-1"

	ch := &fakeBrokerChannel{
		pairingCode: "PAIR-123",
		expiresAt:   time.Now().Add(time.Minute),
		closed:      make(chan struct{}, 1),
	}
	cdpClient := &fakeCDP{}
	s := newService(
		Options{
			Enabled:       true,
			BrokerBaseURL: "https://broker.example",
			PollInterval:  time.Millisecond,
		},
		&fakeBrokerStarter{channel: ch},
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
	assert.True(t, result.(startPairingResponse).OK)
	assertEventuallyDisplayObserved(t, cdpClient, "pairing_code", "PAIR-123", "")

	result, err = s.HandleClosePairingSession(context.Background(), nil)
	require.NoError(t, err)
	resp := result.(closePairingResponse)
	assert.True(t, resp.OK)
	assert.Equal(t, "closed", resp.Status)
	assert.Equal(t, "ch_1", resp.ChannelID)

	select {
	case <-ch.closed:
	case <-time.After(time.Second):
		t.Fatal("expected close command to close the broker channel")
	}
	assertEventuallyDisplayObserved(t, cdpClient, "hidden", "", "")

	s.mu.Lock()
	assert.Nil(t, s.active)
	s.mu.Unlock()
}

// TestDisplayActive_TracksLiveOverlayOwnership pins the overlay-owner probe
// wired to playersession.Session.RegisterOverlayOwner: false before any
// pairing starts, true once the pairing code is showing, and false again
// once the session closes and releases the display.
func TestDisplayActive_TracksLiveOverlayOwnership(t *testing.T) {
	defer state.ResetForTesting()
	state.GetState().Relayer.TopicID = "topic-1"

	ch := &fakeBrokerChannel{
		pairingCode: "PAIR-123",
		expiresAt:   time.Now().Add(time.Minute),
		closed:      make(chan struct{}, 1),
	}
	cdpClient := &fakeCDP{}
	s := newService(
		Options{
			Enabled:       true,
			BrokerBaseURL: "https://broker.example",
			PollInterval:  time.Millisecond,
		},
		&fakeBrokerStarter{channel: ch},
		nil,
		nil,
		cdpClient,
		wrapper.NewJSON(),
		zap.NewNop(),
	).(*service)
	s.Start(context.Background())
	defer s.Stop()

	assert.False(t, s.DisplayActive(), "no pairing has started yet")

	result, err := s.HandleStartPairingSession(context.Background(), nil)
	require.NoError(t, err)
	assert.True(t, result.(startPairingResponse).OK)
	assertEventuallyDisplayObserved(t, cdpClient, "pairing_code", "PAIR-123", "")
	assert.True(t, s.DisplayActive(), "the pairing code overlay is live")

	_, err = s.HandleClosePairingSession(context.Background(), nil)
	require.NoError(t, err)
	assertEventuallyDisplayObserved(t, cdpClient, "hidden", "", "")
	assert.False(t, s.DisplayActive(), "the overlay was released on close")
}

func TestHandleClosePairingSession_ReturnsNotStartedWithoutActivePairing(t *testing.T) {
	s := newService(
		Options{Enabled: true},
		nil,
		nil,
		nil,
		&fakeCDP{},
		wrapper.NewJSON(),
		zap.NewNop(),
	).(*service)

	result, err := s.HandleClosePairingSession(context.Background(), nil)
	require.NoError(t, err)
	resp := result.(closePairingResponse)
	assert.True(t, resp.OK)
	assert.Equal(t, "not_started", resp.Status)
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
	assert.True(t, starter.ReceivedOptions().ShortCodeRequested)
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
	assert.Equal(t, 1, starter.StartCount())
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

func TestWaitForBrowserAndApproval_RefreshesCodeAfterPairingExpiryBeforeJoin(t *testing.T) {
	defer state.ResetForTesting()
	state.GetState().Relayer.TopicID = "topic-1"

	oldChannel := &fakeBrokerChannel{
		channelID:   "ch_old",
		pairingCode: "PAIR-OLD",
		expiresAt:   time.Now().Add(80 * time.Millisecond),
		closed:      make(chan struct{}, 1),
	}
	newChannel := &fakeBrokerChannel{
		channelID:   "ch_new",
		pairingCode: "PAIR-NEW",
		expiresAt:   time.Now().Add(time.Minute),
	}
	starter := &fakeBrokerStarter{channels: []brokerChannel{oldChannel, newChannel}}
	cdpClient := &fakeCDP{}
	s := newService(
		Options{
			Enabled:       true,
			BrokerBaseURL: "https://broker.example",
			PollInterval:  time.Millisecond,
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
	assert.True(t, result.(startPairingResponse).OK)
	assertEventuallyDisplayObserved(t, cdpClient, "pairing_code", "PAIR-OLD", "")

	select {
	case <-oldChannel.closed:
	case <-time.After(time.Second):
		t.Fatal("expected expired pairing code channel to close")
	}

	require.Eventually(t, func() bool {
		return starter.StartCount() == 2
	}, time.Second, 10*time.Millisecond)
	assertEventuallyDisplayObserved(t, cdpClient, "pairing_code", "PAIR-NEW", "")

	// HandleStartPairingSession displays the new code BEFORE publishing
	// s.active, so "PAIR-NEW observed" does not imply the refreshed session is
	// registered yet — a single read here raced that window on slow CI runners.
	var active *activePairing
	require.Eventually(t, func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		active = s.active
		return active != nil && active.channelID == "ch_new"
	}, time.Second, 10*time.Millisecond)
	assert.Equal(t, "PAIR-NEW", active.pairingCode)
	assert.Equal(t, 1, oldChannel.closeCount)
	assert.Equal(t, 0, newChannel.closeCount)
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

// fakeMintNavigationSession is a minimal, directly-controllable
// NavigationSession double, mirroring setupui's fakeNavigationSession: tests
// flip pending/ready/generation to drive parkForNavigation without a real
// playersession.Session.
type fakeMintNavigationSession struct {
	mu      sync.Mutex
	pending bool
	ready   bool
	gen     uint64
	target  uint64
}

func (f *fakeMintNavigationSession) NavigationPending() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pending
}

func (f *fakeMintNavigationSession) StageReady(playersession.Stage) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ready
}

func (f *fakeMintNavigationSession) Generation() uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.gen
}

func (f *fakeMintNavigationSession) NavigationTargetGeneration() uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.target
}

func (f *fakeMintNavigationSession) setPending(v bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pending = v
}

func (f *fakeMintNavigationSession) setReadyAndGeneration(ready bool, gen uint64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ready = ready
	f.gen = gen
}

func (f *fakeMintNavigationSession) setTarget(v uint64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.target = v
}

// TestShowPairingCode_ParksWhileNavigationPending pins M13: a display send
// parks while a recovery navigation is pending, delivering only once
// NavigationPending clears — mirroring setupui's park tests.
func TestShowPairingCode_ParksWhileNavigationPending(t *testing.T) {
	active := &activePairing{channelID: "ch", pairingCode: "PAIR-1"}
	cdpClient := &fakeCDP{}
	s := newTestService()
	s.cdp = cdpClient
	nav := &fakeMintNavigationSession{pending: true}
	s.SetSession(nav)
	s.navigationParkPollInterval = 5 * time.Millisecond
	s.navigationParkTimeout = 2 * time.Second

	done := make(chan error, 1)
	go func() { done <- s.showPairingCode(context.Background(), active) }()

	// Give the call a chance to observe the pending flag and start parking.
	time.Sleep(30 * time.Millisecond)
	cdpClient.mu.Lock()
	sentSoFar := len(cdpClient.displayRequests)
	cdpClient.mu.Unlock()
	assert.Zero(t, sentSoFar, "must park while NavigationPending is true")

	nav.setPending(false)
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the parked send to deliver")
	}
	assertLastDisplay(t, cdpClient, "pairing_code", "PAIR-1", "")
}

// TestShowPairingCode_ExitsParkWhenTargetGenerationReady pins the same
// target-generation park predicate setupui's park gets [Park predicate fix]:
// a bare StageReady with the navigation target still 0 (pre-bump) must NOT
// exit the park; once the target is set and matches Generation(), it does.
func TestShowPairingCode_ExitsParkWhenTargetGenerationReady(t *testing.T) {
	active := &activePairing{channelID: "ch", pairingCode: "PAIR-1"}
	cdpClient := &fakeCDP{}
	s := newTestService()
	s.cdp = cdpClient
	nav := &fakeMintNavigationSession{pending: true, ready: true} // gen 0, target 0 (pre-bump), already "ready"
	s.SetSession(nav)
	s.navigationParkPollInterval = 5 * time.Millisecond
	s.navigationParkTimeout = 2 * time.Second

	done := make(chan error, 1)
	go func() { done <- s.showPairingCode(context.Background(), active) }()

	time.Sleep(30 * time.Millisecond)
	cdpClient.mu.Lock()
	sentSoFar := len(cdpClient.displayRequests)
	cdpClient.mu.Unlock()
	assert.Zero(t, sentSoFar, "pre-bump (target==0) StageReady must not exit the park")

	nav.setReadyAndGeneration(true, 1)
	nav.setTarget(1)
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the parked send to deliver")
	}
	assertLastDisplay(t, cdpClient, "pairing_code", "PAIR-1", "")
}

// TestShowPairingCode_ExitsParkPromptlyWhenEnteredPostBump pins finding 1
// directly for mintpairing's park twin: a park entered AFTER the
// navigation's bump already happened (Generation() and the navigation's
// target are already the SAME value when this park starts) must exit
// PROMPTLY once that target generation is handler-ready, not stall for the
// full park timeout — see setupui's identical test for the full rationale.
func TestShowPairingCode_ExitsParkPromptlyWhenEnteredPostBump(t *testing.T) {
	active := &activePairing{channelID: "ch", pairingCode: "PAIR-1"}
	cdpClient := &fakeCDP{}
	s := newTestService()
	s.cdp = cdpClient
	nav := &fakeMintNavigationSession{pending: true, gen: 1, target: 1, ready: true}
	s.SetSession(nav)
	s.navigationParkPollInterval = 5 * time.Millisecond
	s.navigationParkTimeout = 2 * time.Second

	start := time.Now()
	err := s.showPairingCode(context.Background(), active)
	elapsed := time.Since(start)

	require.NoError(t, err)
	assert.Less(t, elapsed, 200*time.Millisecond,
		"a park entered post-bump must exit promptly, not stall for the full timeout")
	assertLastDisplay(t, cdpClient, "pairing_code", "PAIR-1", "")
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
		}, topicGuard{topicID: "topic-1"}, "mpa_1", approvalDecisionRequest{
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
	}, topicGuard{topicID: "topic-1"}, "mpa_1", approvalDecisionRequest{
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

func TestCompleteDecision_RevokesTheSessionWhenTheTopicChangesAfterCreation(t *testing.T) {
	defer state.ResetForTesting()
	state.GetState().Relayer.TopicID = "topic-1"

	ch := &fakeBrokerChannel{}
	creator := &recordingSessionCreator{persistent: true}
	// The topic moves while the session is being minted: the browser can never
	// be handed this session, and an owner-kept one has no TTL to clean it up.
	creator.onCreate = func() { state.GetState().Relayer.TopicID = "topic-2" }
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
		ChannelID:                  "ch_1",
		MessageID:                  "msg_1",
		SupportsPersistentSessions: true,
	}, topicGuard{topicID: "topic-1"}, "mpa_1", approvalDecisionRequest{
		ApprovalRequestID: "mpa_1",
		TopicID:           "topic-1",
		ChannelID:         "ch_1",
		RequestMessageID:  "msg_1",
		Decision:          "approve",
		KeepPaired:        true,
	})

	require.Error(t, err)
	assert.True(t, terminalSent)
	assert.Empty(t, ch.DeliveredSessions(), "a stale-topic session is never delivered")
	assert.Equal(t, []revokedSession{{topicID: "topic-1", sessionID: "session-1"}}, creator.revokes)

	ch.mu.Lock()
	assert.Equal(t, []string{"topic_changed"}, ch.rejectionReasons)
	ch.mu.Unlock()

	outcome := <-relayerClient.sent
	assertRelayerNotification(t, outcome, relayer.NOTIFICATION_TYPE_MINT_PAIRING_APPROVAL_OUTCOME)
	assert.Equal(t, "failed", outcome.Message.(map[string]any)["status"])
}

func TestWaitForInFlightCreates_BlocksUntilThePostCreateGuardHasRun(t *testing.T) {
	defer state.ResetForTesting()
	if _, err := state.SetRelayerTopicID("topic-1"); err != nil {
		t.Logf("SetRelayerTopicID save failed as expected in tests: %v", err)
	}
	startGuard := currentTopicGuard()

	inCreate := make(chan struct{})
	releaseCreate := make(chan struct{})
	ch := &fakeBrokerChannel{rejectionSent: make(chan struct{}, 1)}
	creator := &recordingSessionCreator{persistent: true}
	// The reset lands while this create is at the relayer.
	creator.onCreate = func() {
		close(inCreate)
		<-releaseCreate
		if _, _, err := state.InvalidateRelayerTopic(); err != nil {
			t.Logf("InvalidateRelayerTopic save failed as expected in tests: %v", err)
		}
	}
	s := newService(
		Options{RelayerBaseURL: "https://relayer.example"},
		nil,
		creator,
		&fakeRelayer{sent: make(chan relayer.Response, 2)},
		nil,
		wrapper.NewJSON(),
		zap.NewNop(),
	).(*service)

	mintDone := make(chan struct{})
	go func() {
		defer close(mintDone)
		_, _ = s.completeDecision(context.Background(), ch, minter.MintRequest{
			ChannelID:                  "ch_1",
			MessageID:                  "msg_1",
			SupportsPersistentSessions: true,
		}, startGuard, "mpa_1", approvalDecisionRequest{
			ApprovalRequestID: "mpa_1",
			TopicID:           "topic-1",
			ChannelID:         "ch_1",
			RequestMessageID:  "msg_1",
			Decision:          "approve",
			KeepPaired:        true,
		})
	}()

	<-inCreate

	// A reset asking now must not be told the coast is clear.
	tooSoon, cancelTooSoon := context.WithTimeout(context.Background(), 50*time.Millisecond)
	inFlight, err := s.WaitForInFlightCreates(tooSoon)
	cancelTooSoon()
	require.Error(t, err, "a create at the relayer is still in flight")
	assert.Equal(t, 1, inFlight, "the caller is told how many are outstanding")

	close(releaseCreate)

	settled, cancelSettled := context.WithTimeout(context.Background(), 2*time.Second)
	inFlight, err = s.WaitForInFlightCreates(settled)
	cancelSettled()
	require.NoError(t, err)
	assert.Zero(t, inFlight)

	<-mintDone
	// By the time the wait returned, the create had already run its guard and
	// revoked the session it made — which is what makes a sweep after this
	// wait complete.
	assert.Equal(t, []revokedSession{{topicID: "topic-1", sessionID: "session-1"}}, creator.revokes)
	assert.Empty(t, ch.DeliveredSessions())
}

// TestCompleteDecision_ResetBetweenAdmissionAndTheGuardCheckIsWaitedFor pins
// the window the create gate was moved to cover. The reset lands after this
// decision has been admitted but before it has re-read the claim: if admission
// came after that read, the reset would see an empty gate, sweep an empty
// topic, and only then would this mint POST — leaving a session behind at the
// relayer whose only remaining owner is a process about to reboot.
func TestCompleteDecision_ResetBetweenAdmissionAndTheGuardCheckIsWaitedFor(t *testing.T) {
	defer state.ResetForTesting()
	if _, err := state.SetRelayerTopicID("topic-1"); err != nil {
		t.Logf("SetRelayerTopicID save failed as expected in tests: %v", err)
	}
	startGuard := currentTopicGuard()

	admitted := make(chan struct{})
	resetDone := make(chan struct{})
	ch := &fakeBrokerChannel{rejectionSent: make(chan struct{}, 1)}
	creator := &recordingSessionCreator{persistent: true}
	s := newService(
		Options{RelayerBaseURL: "https://relayer.example"},
		nil,
		creator,
		&fakeRelayer{sent: make(chan relayer.Response, 2)},
		nil,
		wrapper.NewJSON(),
		zap.NewNop(),
	).(*service)
	// The reset lands in the window between admission and the guard check.
	s.afterCreateAdmission = func() {
		close(admitted)
		<-resetDone
	}

	mintDone := make(chan struct{})
	go func() {
		defer close(mintDone)
		_, _ = s.completeDecision(context.Background(), ch, minter.MintRequest{
			ChannelID:                  "ch_1",
			MessageID:                  "msg_1",
			SupportsPersistentSessions: true,
		}, startGuard, "mpa_1", approvalDecisionRequest{
			ApprovalRequestID: "mpa_1",
			TopicID:           "topic-1",
			ChannelID:         "ch_1",
			RequestMessageID:  "msg_1",
			Decision:          "approve",
			KeepPaired:        true,
		})
	}()

	<-admitted

	// This is the reset: invalidate, then ask whether anything is mid-create.
	if _, _, err := state.InvalidateRelayerTopic(); err != nil {
		t.Logf("InvalidateRelayerTopic save failed as expected in tests: %v", err)
	}
	tooSoon, cancelTooSoon := context.WithTimeout(context.Background(), 50*time.Millisecond)
	inFlight, err := s.WaitForInFlightCreates(tooSoon)
	cancelTooSoon()
	require.Error(t, err, "the reset must see this decision as in flight")
	assert.Equal(t, 1, inFlight)

	close(resetDone)

	settled, cancelSettled := context.WithTimeout(context.Background(), 2*time.Second)
	inFlight, err = s.WaitForInFlightCreates(settled)
	cancelSettled()
	require.NoError(t, err, "the wait clears once the decision has settled")
	assert.Zero(t, inFlight)

	<-mintDone

	// Admitted, then stopped by the guard: nothing was ever POSTed, so there
	// is nothing for the sweep to miss.
	assert.Zero(t, creator.calls, "the mint must not create a session for a wiped claim")
	assert.Empty(t, creator.revokes)
	assert.Empty(t, ch.DeliveredSessions())
	ch.mu.Lock()
	assert.Equal(t, []string{"topic_changed"}, ch.rejectionReasons)
	ch.mu.Unlock()
}

func TestWaitForInFlightCreates_ReturnsImmediatelyWhenNothingIsInFlight(t *testing.T) {
	s := newTestService()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	inFlight, err := s.WaitForInFlightCreates(ctx)

	require.NoError(t, err)
	assert.Zero(t, inFlight)
}

func TestRevokeTopicSessions_RevokesEveryListedSession(t *testing.T) {
	creator := &recordingSessionCreator{listIDs: []string{"session-1", "session-2"}}
	s := newService(
		Options{Enabled: true},
		nil,
		creator,
		nil,
		nil,
		wrapper.NewJSON(),
		zap.NewNop(),
	).(*service)

	revoked, err := s.RevokeTopicSessions(context.Background(), "topic-1")

	require.NoError(t, err)
	assert.Equal(t, 2, revoked)
	assert.Equal(t, []revokedSession{
		{topicID: "topic-1", sessionID: "session-1"},
		{topicID: "topic-1", sessionID: "session-2"},
	}, creator.revokes)
}

func TestRevokeTopicSessions_KeepsGoingAfterAFailure(t *testing.T) {
	creator := &recordingSessionCreator{
		listIDs:   []string{"session-1", "session-2"},
		revokeErr: errors.New("relayer unreachable"),
	}
	s := newService(
		Options{Enabled: true},
		nil,
		creator,
		nil,
		nil,
		wrapper.NewJSON(),
		zap.NewNop(),
	).(*service)

	revoked, err := s.RevokeTopicSessions(context.Background(), "topic-1")

	require.Error(t, err, "the caller is told what could not be revoked")
	assert.Zero(t, revoked)
	assert.Len(t, creator.revokes, 2, "one unreachable session must not strand the rest")
}

func TestRevokeTopicSessions_IsANoOpWhenMintPairingIsDisabled(t *testing.T) {
	creator := &recordingSessionCreator{listIDs: []string{"session-1"}}
	s := newService(
		Options{},
		nil,
		creator,
		nil,
		nil,
		wrapper.NewJSON(),
		zap.NewNop(),
	).(*service)

	revoked, err := s.RevokeTopicSessions(context.Background(), "topic-1")

	require.NoError(t, err)
	assert.Zero(t, revoked)
	assert.Empty(t, creator.revokes, "a device that never minted a session has nothing to sweep")
	assert.Empty(t, creator.listIDsCalls, "and must not spend reset budget on a relayer round trip")
}

func TestRevokeTopicSessions_IsANoOpWithoutATopic(t *testing.T) {
	creator := &recordingSessionCreator{listIDs: []string{"session-1"}}
	s := newService(
		Options{Enabled: true},
		nil,
		creator,
		nil,
		nil,
		wrapper.NewJSON(),
		zap.NewNop(),
	).(*service)

	revoked, err := s.RevokeTopicSessions(context.Background(), "  ")

	require.NoError(t, err)
	assert.Zero(t, revoked)
	assert.Empty(t, creator.revokes)
}

func TestHandleApprovalDecision_RejectsAnApprovalFromBeforeAReclaimOntoTheSameTopic(t *testing.T) {
	defer state.ResetForTesting()
	if _, err := state.SetRelayerTopicID("topic-1"); err != nil {
		t.Logf("SetRelayerTopicID save failed as expected in tests: %v", err)
	}

	s := newTestService()
	// The pairing began under the claim that existed a moment ago.
	pending := &pendingApproval{
		approvalRequestID: "mpa_1",
		guard:             currentTopicGuard(),
		channelID:         "ch_1",
		requestMessageID:  "msg_1",
		expiresAt:         time.Now().Add(time.Minute),
		decisionCh:        make(chan approvalDecisionRequest, 1),
	}
	s.registerPending(pending)

	// A factory reset and a re-claim that lands on the SAME topic id: every id
	// comparison still passes, and this approval belongs to the claim before it.
	if _, err := state.ClearClaim(); err != nil {
		t.Logf("ClearClaim save failed as expected in tests: %v", err)
	}
	if _, err := state.SetRelayerTopicID("topic-1"); err != nil {
		t.Logf("SetRelayerTopicID save failed as expected in tests: %v", err)
	}

	result, err := s.HandleApprovalDecision(context.Background(), validDecisionArgs("mpa_1", "topic-1", "ch_1", "msg_1"))
	require.NoError(t, err)
	resp := result.(approvalResponse)
	require.NotNil(t, resp.Error)
	assert.Equal(t, "topic_mismatch", resp.Error.Code,
		"a re-claim onto the same topic id is a different pairing")

	select {
	case <-pending.decisionCh:
		t.Fatal("a decision from the previous claim must never reach the mint flow")
	default:
	}
}

func TestCompleteDecision_RejectsAMintForAClaimThatWasReplacedByTheSameTopic(t *testing.T) {
	defer state.ResetForTesting()
	if _, err := state.SetRelayerTopicID("topic-1"); err != nil {
		t.Logf("SetRelayerTopicID save failed as expected in tests: %v", err)
	}

	// The claim this pairing began under.
	startGuard := currentTopicGuard()

	ch := &fakeBrokerChannel{rejectionSent: make(chan struct{}, 1)}
	creator := &recordingSessionCreator{persistent: true}
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

	// Reset and re-claim onto the same topic id before the decision is acted on.
	if _, err := state.ClearClaim(); err != nil {
		t.Logf("ClearClaim save failed as expected in tests: %v", err)
	}
	if _, err := state.SetRelayerTopicID("topic-1"); err != nil {
		t.Logf("SetRelayerTopicID save failed as expected in tests: %v", err)
	}

	terminalSent, err := s.completeDecision(context.Background(), ch, minter.MintRequest{
		ChannelID:                  "ch_1",
		MessageID:                  "msg_1",
		SupportsPersistentSessions: true,
	}, startGuard, "mpa_1", approvalDecisionRequest{
		ApprovalRequestID: "mpa_1",
		TopicID:           "topic-1",
		ChannelID:         "ch_1",
		RequestMessageID:  "msg_1",
		Decision:          "approve",
		KeepPaired:        true,
	})

	require.Error(t, err)
	assert.True(t, terminalSent)
	assert.Zero(t, creator.calls, "the replaced claim never mints a session")
	assert.Empty(t, ch.DeliveredSessions())

	ch.mu.Lock()
	assert.Equal(t, []string{"topic_changed"}, ch.rejectionReasons)
	ch.mu.Unlock()
}

// TestCompleteDecision_RevokesASessionTheResetSweepCouldNotSee drives the
// factory reset's exact interleaving against a mint that was already accepted:
//
//  1. the reset invalidates the claim — the topic generation moves
//  2. the reset sweeps the topic's sessions — THIS session does not exist yet,
//     so the sweep cannot possibly revoke it
//  3. the mint creates its session and tries to finish
//
// Step 3 is the only thing left that can end it, which is why the mint carries
// the guard it began under: it sees the moved generation and revokes what it
// just created instead of handing the browser a session against a wiped claim.
func TestCompleteDecision_RevokesASessionTheResetSweepCouldNotSee(t *testing.T) {
	defer state.ResetForTesting()
	if _, err := state.SetRelayerTopicID("topic-1"); err != nil {
		t.Logf("SetRelayerTopicID save failed as expected in tests: %v", err)
	}

	// The claim this pairing began under.
	startGuard := currentTopicGuard()

	ch := &fakeBrokerChannel{rejectionSent: make(chan struct{}, 1)}
	// listIDs stays empty: at sweep time the relayer holds nothing for this
	// topic, because the session below has not been created yet.
	creator := &recordingSessionCreator{persistent: true}
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

	sweptTopic := ""
	sweptCount := -1
	// The reset lands while this mint is creating its session.
	creator.onCreate = func() {
		topicID, _, err := state.InvalidateRelayerTopic()
		if err != nil {
			t.Logf("InvalidateRelayerTopic save failed as expected in tests: %v", err)
		}
		sweptTopic = topicID
		revoked, sweepErr := s.RevokeTopicSessions(context.Background(), topicID)
		require.NoError(t, sweepErr)
		sweptCount = revoked
	}

	terminalSent, err := s.completeDecision(context.Background(), ch, minter.MintRequest{
		ChannelID:                  "ch_1",
		MessageID:                  "msg_1",
		SupportsPersistentSessions: true,
	}, startGuard, "mpa_1", approvalDecisionRequest{
		ApprovalRequestID: "mpa_1",
		TopicID:           "topic-1",
		ChannelID:         "ch_1",
		RequestMessageID:  "msg_1",
		Decision:          "approve",
		KeepPaired:        true,
	})

	require.Error(t, err)
	assert.True(t, terminalSent)
	assert.Equal(t, "topic-1", sweptTopic)
	assert.Zero(t, sweptCount, "the sweep ran before this session existed")
	assert.Empty(t, ch.DeliveredSessions(), "a session for a wiped claim is never delivered")
	assert.Equal(t, []revokedSession{{topicID: "topic-1", sessionID: "session-1"}}, creator.revokes,
		"the mint revokes what the sweep could not see")
}

func TestCompleteDecision_RevokesASessionMintedAcrossAFactoryReset(t *testing.T) {
	defer state.ResetForTesting()
	if _, err := state.SetRelayerTopicID("topic-1"); err != nil {
		t.Logf("SetRelayerTopicID save failed as expected in tests: %v", err)
	}

	ch := &fakeBrokerChannel{rejectionSent: make(chan struct{}, 1)}
	// The claim this pairing begins under.
	startGuard := currentTopicGuard()
	creator := &recordingSessionCreator{persistent: true}
	// The factory reset lands while the session is being minted: it clears the
	// claim, which is what moves the topic out from under this mint.
	creator.onCreate = func() { _, _ = state.ClearClaim() }
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
		ChannelID:                  "ch_1",
		MessageID:                  "msg_1",
		SupportsPersistentSessions: true,
	}, startGuard, "mpa_1", approvalDecisionRequest{
		ApprovalRequestID: "mpa_1",
		TopicID:           "topic-1",
		ChannelID:         "ch_1",
		RequestMessageID:  "msg_1",
		Decision:          "approve",
		KeepPaired:        true,
	})

	require.Error(t, err, "a session minted for a wiped claim is never delivered")
	assert.True(t, terminalSent)
	assert.Empty(t, ch.DeliveredSessions())
	assert.Equal(t, []revokedSession{{topicID: "topic-1", sessionID: "session-1"}}, creator.revokes,
		"the reset must not leave a kept session alive on the old topic")
}

func TestCompleteDecision_RevokesWhenTheTopicMovesWhileTheSessionIsDelivered(t *testing.T) {
	defer state.ResetForTesting()
	if _, err := state.SetRelayerTopicID("topic-1"); err != nil {
		// The persisted write has nowhere to land in a test; the in-memory
		// topic and its generation are what this exercises.
		t.Logf("SetRelayerTopicID save failed as expected in tests: %v", err)
	}

	core, logs := observer.New(zap.WarnLevel)
	ch := &fakeBrokerChannel{}
	// The topic is reclaimed mid-delivery: the check passed, the session is
	// already on its way to the browser, and the topic it was minted for is
	// gone by the time the send returns.
	ch.onSend = func() {
		_, _ = state.ClearClaim()
		_, _ = state.SetRelayerTopicID("topic-1")
	}
	// The claim this pairing begins under.
	startGuard := currentTopicGuard()
	creator := &recordingSessionCreator{persistent: true}
	relayerClient := &fakeRelayer{sent: make(chan relayer.Response, 1)}
	s := newService(
		Options{RelayerBaseURL: "https://relayer.example"},
		nil,
		creator,
		relayerClient,
		nil,
		wrapper.NewJSON(),
		zap.New(core),
	).(*service)

	terminalSent, err := s.completeDecision(context.Background(), ch, minter.MintRequest{
		ChannelID:                  "ch_1",
		MessageID:                  "msg_1",
		SupportsPersistentSessions: true,
	}, startGuard, "mpa_1", approvalDecisionRequest{
		ApprovalRequestID: "mpa_1",
		TopicID:           "topic-1",
		ChannelID:         "ch_1",
		RequestMessageID:  "msg_1",
		Decision:          "approve",
		KeepPaired:        true,
	})

	require.NoError(t, err)
	assert.True(t, terminalSent)
	require.Len(t, ch.DeliveredSessions(), 1, "the browser's message is not taken back")
	assert.Equal(t, []revokedSession{{topicID: "topic-1", sessionID: "session-1"}}, creator.revokes,
		"the topic it was minted for is gone, so the session is revoked")
	assert.Equal(t, 1, logs.FilterMessage("Relayer topic changed while delivering a mint pairing session; revoking the session it was minted for").Len())

	outcome := <-relayerClient.sent
	assert.Equal(t, "completed", outcome.Message.(map[string]any)["status"])
}

func TestCompleteDecision_KeepsTheSessionWhenTheTopicIsUnchanged(t *testing.T) {
	defer state.ResetForTesting()
	if _, err := state.SetRelayerTopicID("topic-1"); err != nil {
		t.Logf("SetRelayerTopicID save failed as expected in tests: %v", err)
	}

	ch := &fakeBrokerChannel{}
	// An idempotent re-write of the same topic must not read as a change.
	ch.onSend = func() { _, _ = state.SetRelayerTopicID("topic-1") }
	// The claim this pairing begins under.
	startGuard := currentTopicGuard()
	creator := &recordingSessionCreator{persistent: true}
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

	_, err := s.completeDecision(context.Background(), ch, minter.MintRequest{
		ChannelID:                  "ch_1",
		MessageID:                  "msg_1",
		SupportsPersistentSessions: true,
	}, startGuard, "mpa_1", approvalDecisionRequest{
		ApprovalRequestID: "mpa_1",
		TopicID:           "topic-1",
		ChannelID:         "ch_1",
		RequestMessageID:  "msg_1",
		Decision:          "approve",
		KeepPaired:        true,
	})

	require.NoError(t, err)
	assert.Empty(t, creator.revokes, "a delivered session on an unchanged topic is left alone")

	outcome := <-relayerClient.sent
	assert.Equal(t, "completed", outcome.Message.(map[string]any)["status"])
}

func TestCompleteDecision_RevokesWhenTheBrokerRefusedTheMessage(t *testing.T) {
	defer state.ResetForTesting()
	state.GetState().Relayer.TopicID = "topic-1"

	// A broker client-error status is proof the message was never accepted.
	ch := &fakeBrokerChannel{successErr: errors.New("broker POST /v1/channels/ch_1/messages failed with status 410")}
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
	}, topicGuard{topicID: "topic-1"}, "mpa_1", approvalDecisionRequest{
		ApprovalRequestID: "mpa_1",
		TopicID:           "topic-1",
		ChannelID:         "ch_1",
		RequestMessageID:  "msg_1",
		Decision:          "approve",
	})

	require.Error(t, err)
	assert.False(t, terminalSent)
	assert.Equal(t, []revokedSession{{topicID: "topic-1", sessionID: "session-1"}}, creator.revokes,
		"a session the broker refused outright is revoked, timed or not")

	outcome := <-relayerClient.sent
	assertRelayerNotification(t, outcome, relayer.NOTIFICATION_TYPE_MINT_PAIRING_APPROVAL_OUTCOME)
	assert.Equal(t, "failed", outcome.Message.(map[string]any)["status"])
}

func TestCompleteDecision_LeavesTheSessionInPlaceWhenDeliveryIsIndeterminate(t *testing.T) {
	tests := []struct {
		name      string
		sendErr   error
		sessionID string
	}{
		{name: "timeout", sendErr: context.DeadlineExceeded},
		{name: "transport error", sendErr: errors.New("dial tcp: connection reset by peer")},
		{name: "broker server error", sendErr: errors.New("broker POST /v1/channels/ch_1/messages failed with status 503")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			defer state.ResetForTesting()
			state.GetState().Relayer.TopicID = "topic-1"

			core, logs := observer.New(zap.WarnLevel)
			ch := &fakeBrokerChannel{successErr: tt.sendErr}
			creator := &recordingSessionCreator{persistent: true}
			relayerClient := &fakeRelayer{sent: make(chan relayer.Response, 1)}
			s := newService(
				Options{RelayerBaseURL: "https://relayer.example"},
				nil,
				creator,
				relayerClient,
				nil,
				wrapper.NewJSON(),
				zap.New(core),
			).(*service)

			_, err := s.completeDecision(context.Background(), ch, minter.MintRequest{
				ChannelID:                  "ch_1",
				MessageID:                  "msg_1",
				SupportsPersistentSessions: true,
			}, topicGuard{topicID: "topic-1"}, "mpa_1", approvalDecisionRequest{
				ApprovalRequestID: "mpa_1",
				TopicID:           "topic-1",
				ChannelID:         "ch_1",
				RequestMessageID:  "msg_1",
				Decision:          "approve",
				KeepPaired:        true,
			})

			require.Error(t, err)
			assert.Empty(t, creator.revokes,
				"the browser may already hold this session, so it must not be revoked")

			left := logs.FilterMessage("Left a possibly delivered mint pairing session in place after a failed browser delivery")
			require.Equal(t, 1, left.Len())
			assert.Equal(t, "session-1", left.All()[0].ContextMap()["sessionID"],
				"the session id is logged so an abandoned session can be traced")

			outcome := <-relayerClient.sent
			assert.Equal(t, "failed", outcome.Message.(map[string]any)["status"])
		})
	}
}

func TestClassifyDeliveryFailure(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want deliveryVerdict
	}{
		{name: "nil", err: nil, want: deliveryUnknown},
		{name: "channel gone", err: errors.New("broker POST /v1/channels/ch_1/messages failed with status 404"), want: deliveryNeverSent},
		{name: "rejected outright", err: errors.New("broker POST /v1/channels/ch_1/messages failed with status 409"), want: deliveryNeverSent},
		{name: "rate limited", err: errors.New("broker POST /v1/channels/ch_1/messages failed with status 429"), want: deliveryNeverSent},
		{name: "server error", err: errors.New("broker POST /v1/channels/ch_1/messages failed with status 500"), want: deliveryUnknown},
		{name: "timeout", err: context.DeadlineExceeded, want: deliveryUnknown},
		{name: "unrecognized", err: errors.New("mint result token is required"), want: deliveryUnknown},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, classifyDeliveryFailure(tt.err))
		})
	}
}

func TestCompleteDecision_RevokeFailureIsLoggedAndChangesNothing(t *testing.T) {
	defer state.ResetForTesting()
	state.GetState().Relayer.TopicID = "topic-1"

	core, logs := observer.New(zap.WarnLevel)
	ch := &fakeBrokerChannel{successErr: errors.New("broker POST /v1/channels/ch_1/messages failed with status 410")}
	creator := &recordingSessionCreator{revokeErr: errors.New("relayer unreachable")}
	relayerClient := &fakeRelayer{sent: make(chan relayer.Response, 1)}
	s := newService(
		Options{RelayerBaseURL: "https://relayer.example"},
		nil,
		creator,
		relayerClient,
		nil,
		wrapper.NewJSON(),
		zap.New(core),
	).(*service)

	terminalSent, err := s.completeDecision(context.Background(), ch, minter.MintRequest{
		ChannelID: "ch_1",
		MessageID: "msg_1",
	}, topicGuard{topicID: "topic-1"}, "mpa_1", approvalDecisionRequest{
		ApprovalRequestID: "mpa_1",
		TopicID:           "topic-1",
		ChannelID:         "ch_1",
		RequestMessageID:  "msg_1",
		Decision:          "approve",
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "send mint success", "the browser-facing failure is what the caller sees")
	assert.False(t, terminalSent)
	assert.Len(t, creator.revokes, 1)
	assert.Equal(t, 1, logs.FilterMessage("Failed to revoke abandoned mint pairing session").Len())

	outcome := <-relayerClient.sent
	assertRelayerNotification(t, outcome, relayer.NOTIFICATION_TYPE_MINT_PAIRING_APPROVAL_OUTCOME)
	assert.Equal(t, "failed", outcome.Message.(map[string]any)["status"])
}

func TestCompleteDecision_KeepPairedDeliversAPersistentSessionWithNoExpiry(t *testing.T) {
	defer state.ResetForTesting()
	state.GetState().Relayer.TopicID = "topic-1"

	ch := &fakeBrokerChannel{}
	creator := &recordingSessionCreator{persistent: true}
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
		ChannelID:                  "ch_1",
		MessageID:                  "msg_1",
		SupportsPersistentSessions: true,
	}, topicGuard{topicID: "topic-1"}, "mpa_1", approvalDecisionRequest{
		ApprovalRequestID: "mpa_1",
		TopicID:           "topic-1",
		ChannelID:         "ch_1",
		RequestMessageID:  "msg_1",
		Decision:          "approve",
		KeepPaired:        true,
	})

	require.NoError(t, err)
	assert.True(t, terminalSent)
	assert.Equal(t, []sessionLifetime{lifetimePersistent}, creator.lifetimes)

	delivered := ch.DeliveredSessions()
	require.Len(t, delivered, 1)
	assert.True(t, delivered[0].Persistent)
	assert.True(t, delivered[0].ExpiresAt.IsZero(), "an owner-kept session is delivered without an expiry")
	assert.Equal(t, "https://relayer.example", delivered[0].RelayerBaseURL)

	// The payload the minter client encrypts for the browser.
	payload := decodeSessionPayload(t, delivered[0])
	assert.Equal(t, true, payload["persistent"])
	assert.Nil(t, payload["expiresAt"], "an owner-kept session carries a null expiry")

	outcome := <-relayerClient.sent
	assertRelayerNotification(t, outcome, relayer.NOTIFICATION_TYPE_MINT_PAIRING_APPROVAL_OUTCOME)
	assert.Equal(t, "completed", outcome.Message.(map[string]any)["status"])
	assert.Equal(t, "persistent", outcome.Message.(map[string]any)["lifetime"])
}

func TestCompleteDecision_KeepPairedFallsBackWhenTheRequesterCannotHoldAPersistentSession(t *testing.T) {
	defer state.ResetForTesting()
	state.GetState().Relayer.TopicID = "topic-1"

	core, logs := observer.New(zap.WarnLevel)
	ch := &fakeBrokerChannel{}
	creator := &recordingSessionCreator{}
	relayerClient := &fakeRelayer{sent: make(chan relayer.Response, 1)}
	s := newService(
		Options{RelayerBaseURL: "https://relayer.example"},
		nil,
		creator,
		relayerClient,
		nil,
		wrapper.NewJSON(),
		zap.New(core),
	).(*service)

	// The owner asked to keep the site paired, but this requester never
	// declared the capability: a client released before owner-kept sessions
	// cannot parse a session with no expiry.
	terminalSent, err := s.completeDecision(context.Background(), ch, minter.MintRequest{
		ChannelID: "ch_1",
		MessageID: "msg_1",
		Origin:    "https://gallery.example",
	}, topicGuard{topicID: "topic-1"}, "mpa_1", approvalDecisionRequest{
		ApprovalRequestID: "mpa_1",
		TopicID:           "topic-1",
		ChannelID:         "ch_1",
		RequestMessageID:  "msg_1",
		Decision:          "approve",
		KeepPaired:        true,
	})

	require.NoError(t, err)
	assert.True(t, terminalSent)
	assert.Equal(t, []sessionLifetime{lifetimeTimedFallbackRequester}, creator.lifetimes,
		"an incapable requester is never asked to hold a persistent session")

	delivered := ch.DeliveredSessions()
	require.Len(t, delivered, 1)
	assert.False(t, delivered[0].Persistent)
	assert.False(t, delivered[0].ExpiresAt.IsZero())

	fallback := logs.FilterMessage("Owner asked to keep this site paired, but the requester cannot hold a session without an expiry; minting the longest timed session instead")
	require.Equal(t, 1, fallback.Len())
	assert.Equal(t, "https://gallery.example", fallback.All()[0].ContextMap()["origin"])

	outcome := <-relayerClient.sent
	assertRelayerNotification(t, outcome, relayer.NOTIFICATION_TYPE_MINT_PAIRING_APPROVAL_OUTCOME)
	assert.Equal(t, "completed", outcome.Message.(map[string]any)["status"])
	assert.Equal(t, "timed_fallback_requester", outcome.Message.(map[string]any)["lifetime"],
		"the controller is told what the owner actually got")
}

func TestCompleteDecision_ReportsTheDeliveredLifetimeNotTheRequestedOne(t *testing.T) {
	defer state.ResetForTesting()
	state.GetState().Relayer.TopicID = "topic-1"

	ch := &fakeBrokerChannel{}
	// keepPaired was asked for and the requester could hold it, but the
	// relayer answered with an ordinary timed session.
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

	_, err := s.completeDecision(context.Background(), ch, minter.MintRequest{
		ChannelID:                  "ch_1",
		MessageID:                  "msg_1",
		SupportsPersistentSessions: true,
	}, topicGuard{topicID: "topic-1"}, "mpa_1", approvalDecisionRequest{
		ApprovalRequestID: "mpa_1",
		TopicID:           "topic-1",
		ChannelID:         "ch_1",
		RequestMessageID:  "msg_1",
		Decision:          "approve",
		KeepPaired:        true,
	})

	require.NoError(t, err)
	assert.Equal(t, []sessionLifetime{lifetimePersistent}, creator.lifetimes, "the ask was persistent")

	delivered := ch.DeliveredSessions()
	require.Len(t, delivered, 1)
	assert.False(t, delivered[0].Persistent)

	outcome := <-relayerClient.sent
	assert.Equal(t, "timed", outcome.Message.(map[string]any)["lifetime"],
		"the owner is told what the session is, not what was asked for")
}

func TestDeliveredOutcomeLifetime(t *testing.T) {
	tests := []struct {
		name      string
		requested sessionLifetime
		session   minter.MintResult
		want      string
	}{
		{name: "timed ask, timed session", requested: lifetimeTimed, session: minter.MintResult{ExpiresAt: time.Now()}, want: "timed"},
		{name: "persistent ask, persistent session", requested: lifetimePersistent, session: minter.MintResult{Persistent: true}, want: "persistent"},
		{name: "persistent ask, timed session", requested: lifetimePersistent, session: minter.MintResult{ExpiresAt: time.Now()}, want: "timed"},
		{name: "requester fallback, timed session", requested: lifetimeTimedFallbackRequester, session: minter.MintResult{ExpiresAt: time.Now()}, want: "timed_fallback_requester"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, deliveredOutcomeLifetime(tt.requested, tt.session))
		})
	}
}

func TestSessionLifetimeFor(t *testing.T) {
	s := newTestService()

	tests := []struct {
		name       string
		keepPaired bool
		capable    bool
		want       sessionLifetime
	}{
		{name: "no keep", want: lifetimeTimed},
		{name: "no keep, capable requester", capable: true, want: lifetimeTimed},
		{name: "keep, capable requester", keepPaired: true, capable: true, want: lifetimePersistent},
		{name: "keep, incapable requester", keepPaired: true, want: lifetimeTimedFallbackRequester},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lifetime := s.sessionLifetimeFor(
				approvalDecisionRequest{Decision: "approve", KeepPaired: tt.keepPaired},
				minter.MintRequest{SupportsPersistentSessions: tt.capable},
			)
			assert.Equal(t, tt.want, lifetime)
		})
	}
}

func TestCompleteDecision_WithoutKeepPairedDeliversAnExpiringSession(t *testing.T) {
	defer state.ResetForTesting()
	state.GetState().Relayer.TopicID = "topic-1"

	ch := &fakeBrokerChannel{}
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
	}, topicGuard{topicID: "topic-1"}, "mpa_1", approvalDecisionRequest{
		ApprovalRequestID: "mpa_1",
		TopicID:           "topic-1",
		ChannelID:         "ch_1",
		RequestMessageID:  "msg_1",
		Decision:          "approve",
	})

	require.NoError(t, err)
	assert.True(t, terminalSent)
	assert.Equal(t, []sessionLifetime{lifetimeTimed}, creator.lifetimes)

	delivered := ch.DeliveredSessions()
	require.Len(t, delivered, 1)
	assert.False(t, delivered[0].Persistent)
	assert.False(t, delivered[0].ExpiresAt.IsZero())

	payload := decodeSessionPayload(t, delivered[0])
	assert.NotContains(t, payload, "persistent")
	assert.NotNil(t, payload["expiresAt"], "a timed session always carries its expiry")

	outcome := <-relayerClient.sent
	assertRelayerNotification(t, outcome, relayer.NOTIFICATION_TYPE_MINT_PAIRING_APPROVAL_OUTCOME)
	assert.Equal(t, "completed", outcome.Message.(map[string]any)["status"])
}

// TestWaitForBrowserAndApproval_DropsARequestArrivingAfterTheClaimIsGone: the
// pairing worker sits in a broker poll on a socket this device still holds. If
// the claim goes while it waits — a factory reset — the request it then reads
// belongs to a pairing nobody on this device can honor any more. It must not
// reach the screen or the controller: a previous owner's approval prompt
// appearing on a device mid-wipe is exactly what the reset is undoing.
func TestWaitForBrowserAndApproval_DropsARequestArrivingAfterTheClaimIsGone(t *testing.T) {
	defer state.ResetForTesting()
	if _, err := state.SetRelayerTopicID("topic-1"); err != nil {
		t.Logf("SetRelayerTopicID save failed as expected in tests: %v", err)
	}

	polled := make(chan struct{})
	ch := &fakeBrokerChannel{
		pairingCode: "PAIR-123",
		beforePoll:  func() { closeOnce(polled) },
		request: &minter.MintRequest{
			ChannelID:   "ch_1",
			MessageID:   "msg_1",
			Origin:      "https://gallery.example",
			BrowserInfo: minter.BrowserInfo{Name: "Chrome"},
		},
	}
	cdpClient := &fakeCDP{}
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
		cdpClient,
		wrapper.NewJSON(),
		zap.NewNop(),
	).(*service)
	s.Start(context.Background())
	defer s.Stop()

	// The claim is wiped before the worker reads the request waiting for it.
	if _, _, err := state.InvalidateRelayerTopic(); err != nil {
		t.Logf("InvalidateRelayerTopic save failed as expected in tests: %v", err)
	}

	// A fresh start would be refused outright (no topic), so drive the worker
	// directly: this is a pairing that began under the claim that has since
	// been invalidated.
	startResult, err := s.HandleStartPairingSession(context.Background(), nil)
	require.NoError(t, err)
	assertCommandError(t, startResult, "topic_not_ready", true)
	staleGuard := topicGuard{topicID: "topic-1", generation: 0}
	active := &activePairing{
		channel:     ch,
		channelID:   "ch_1",
		pairingCode: "PAIR-123",
		expiresAt:   time.Now().Add(time.Minute),
		cancel:      func() {},
		done:        make(chan struct{}),
	}
	workerDone := make(chan struct{})
	go func() {
		defer close(workerDone)
		s.waitForBrowserAndApproval(context.Background(), active, staleGuard)
	}()

	select {
	case <-polled:
	case <-time.After(2 * time.Second):
		t.Fatal("the worker never polled for a request")
	}

	select {
	case <-workerDone:
	case <-time.After(2 * time.Second):
		t.Fatal("the worker never returned after dropping the request")
	}

	assert.Empty(t, cdpClient.displayRequestsSnapshot(), "a dropped request never reaches the screen")
	select {
	case sent := <-relayerClient.sent:
		t.Fatalf("a dropped request must not notify the controller: %v", sent)
	default:
	}
	ch.mu.Lock()
	assert.Equal(t, 0, ch.successCount)
	ch.mu.Unlock()
}

// TestCloseActivePairing_EndsTheWorkerAndStopsLaterRequests: the reset closes
// the pairing session it finds, and waits for the worker, so nothing is still
// polling for the claim being wiped.
// TestHandleStartPairingSession_ResetDuringTheBrokerCallLeavesNothingBehind:
// a start that is still inside StartChannel has published nothing, so without
// the in-progress slot a reset finds nothing to close — and the start it did
// not see goes on to paint a QR code and register a worker for a claim that
// is gone.
func TestHandleStartPairingSession_ResetDuringTheBrokerCallLeavesNothingBehind(t *testing.T) {
	defer state.ResetForTesting()
	state.GetState().Relayer.TopicID = "topic-1"

	entered := make(chan struct{})
	ch := &fakeBrokerChannel{pairingCode: "PAIR-123"}
	starter := &fakeBrokerStarter{channel: ch, entered: entered, blockUntilCanceled: true}
	cdpClient := &fakeCDP{}
	relayerClient := &fakeRelayer{sent: make(chan relayer.Response, 4)}
	s := newService(
		Options{
			Enabled:         true,
			BrokerBaseURL:   "https://broker.example",
			ApprovalTimeout: time.Minute,
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

	startResult := make(chan any, 1)
	go func() {
		result, err := s.HandleStartPairingSession(context.Background(), nil)
		assert.NoError(t, err)
		startResult <- result
	}()

	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("the broker call never started")
	}

	// The reset: it must SEE this start even though nothing is published.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	closed, err := s.CloseActivePairing(ctx)
	require.NoError(t, err, "the start must unwind inside the reset's budget")
	assert.True(t, closed, "a start in progress is something to close")

	select {
	case result := <-startResult:
		assertCommandError(t, result, "broker_unavailable", true)
	case <-time.After(2 * time.Second):
		t.Fatal("the canceled start never returned")
	}

	assert.Empty(t, cdpClient.displayRequestsSnapshot(), "a canceled start paints nothing")
	s.mu.Lock()
	active := s.active
	starting := s.starting
	s.mu.Unlock()
	assert.Nil(t, active, "no worker may be registered for a claim being wiped")
	assert.Nil(t, starting, "the in-progress slot is released")
}

// TestHandleStartPairingSession_DropsAChannelWhoseClaimMovedWhileStarting: the
// broker call can succeed just as the claim goes. The channel is closed and
// the start abandoned before anything is displayed or published.
func TestHandleStartPairingSession_DropsAChannelWhoseClaimMovedWhileStarting(t *testing.T) {
	defer state.ResetForTesting()
	if _, err := state.SetRelayerTopicID("topic-1"); err != nil {
		t.Logf("SetRelayerTopicID save failed as expected in tests: %v", err)
	}

	ch := &fakeBrokerChannel{pairingCode: "PAIR-123", closed: make(chan struct{}, 1)}
	starter := &fakeBrokerStarter{channel: ch}
	// The claim is wiped while the broker call is in flight.
	starter.beforeReturn = func() {
		if _, _, err := state.InvalidateRelayerTopic(); err != nil {
			t.Logf("InvalidateRelayerTopic save failed as expected in tests: %v", err)
		}
	}
	cdpClient := &fakeCDP{}
	relayerClient := &fakeRelayer{sent: make(chan relayer.Response, 4)}
	s := newService(
		Options{
			Enabled:         true,
			BrokerBaseURL:   "https://broker.example",
			ApprovalTimeout: time.Minute,
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
	assertCommandError(t, result, "topic_changed", true)

	select {
	case <-ch.closed:
	case <-time.After(2 * time.Second):
		t.Fatal("the channel that outlived its claim was never closed")
	}

	assert.Empty(t, cdpClient.displayRequestsSnapshot(), "nothing is painted for a wiped claim")
	s.mu.Lock()
	active := s.active
	s.mu.Unlock()
	assert.Nil(t, active, "no worker is registered")
	select {
	case sent := <-relayerClient.sent:
		t.Fatalf("nothing may be notified for a wiped claim: %v", sent)
	default:
	}
}

func TestCloseActivePairing_EndsTheWorkerAndStopsLaterRequests(t *testing.T) {
	defer state.ResetForTesting()
	state.GetState().Relayer.TopicID = "topic-1"

	ch := &fakeBrokerChannel{pairingCode: "PAIR-123"}
	cdpClient := &fakeCDP{}
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
		cdpClient,
		wrapper.NewJSON(),
		zap.NewNop(),
	).(*service)
	s.Start(context.Background())
	defer s.Stop()

	result, err := s.HandleStartPairingSession(context.Background(), nil)
	require.NoError(t, err)
	require.True(t, result.(startPairingResponse).OK)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	closed, err := s.CloseActivePairing(ctx)

	require.NoError(t, err, "the worker must exit inside the reset's budget")
	assert.True(t, closed, "there was a pairing session to close")

	// The browser's request lands after the close: with no worker left, it is
	// never read, never displayed, never notified.
	ch.mu.Lock()
	ch.request = &minter.MintRequest{ChannelID: "ch_1", MessageID: "msg_1", Origin: "https://gallery.example"}
	ch.mu.Unlock()

	time.Sleep(50 * time.Millisecond)
	for _, request := range cdpClient.displayRequestsSnapshot() {
		assert.NotEqual(t, "request_received", request["state"],
			"no worker is left to display a request arriving after the close")
	}
	select {
	case sent := <-relayerClient.sent:
		t.Fatalf("no worker is left to notify a controller after the close: %v", sent)
	default:
	}

	// Closing again reports there was nothing to close.
	closed, err = s.CloseActivePairing(ctx)
	require.NoError(t, err)
	assert.False(t, closed)
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

// decodeSessionPayload renders a delivered session the way the minter client
// serializes it into the encrypted mint_succeeded message the browser reads.
func decodeSessionPayload(t *testing.T, session minter.MintResult) map[string]any {
	t.Helper()
	raw, err := json.Marshal(session)
	require.NoError(t, err)
	var payload map[string]any
	require.NoError(t, json.Unmarshal(raw, &payload))
	return payload
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
	// mu guards every field: StartChannel runs on service-owned goroutines (e.g.
	// the pairing-code refresh timer) while tests poll the counters with
	// assert.Eventually, so unsynchronized access is a real data race.
	mu              sync.Mutex
	channel         brokerChannel
	channels        []brokerChannel
	err             error
	receivedOptions minter.StartChannelOptions
	startCount      int
	// entered is closed when StartChannel is entered; blockUntilCanceled
	// makes it hang there until its context is canceled; beforeReturn runs
	// just before a channel is handed back.
	entered            chan struct{}
	blockUntilCanceled bool
	beforeReturn       func()
}

func (f *fakeBrokerStarter) StartChannel(ctx context.Context, opts minter.StartChannelOptions) (brokerChannel, error) {
	// The hooks run OUTSIDE the lock: they model the broker call being slow,
	// and a test watching startCount must not be blocked by them.
	f.mu.Lock()
	entered := f.entered
	blockUntilCanceled := f.blockUntilCanceled
	beforeReturn := f.beforeReturn
	f.mu.Unlock()
	if entered != nil {
		closeOnce(entered)
	}
	if blockUntilCanceled {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if beforeReturn != nil {
		beforeReturn()
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	f.receivedOptions = opts
	f.startCount++
	if len(f.channels) > 0 {
		channel := f.channels[0]
		f.channels = f.channels[1:]
		return channel, f.err
	}
	return f.channel, f.err
}

func (f *fakeBrokerStarter) StartCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.startCount
}

func (f *fakeBrokerStarter) ReceivedOptions() minter.StartChannelOptions {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.receivedOptions
}

type fakeBrokerChannel struct {
	mu                sync.Mutex
	channelID         string
	pairingCode       string
	expiresAt         time.Time
	request           *minter.MintRequest
	rejectionSent     chan struct{}
	rejectionStarted  chan struct{}
	rejectionRelease  chan struct{}
	successSent       chan struct{}
	closed            chan struct{}
	rejectionDelay    time.Duration
	ignoredSeq        int64
	pollAfterSeqs     []int64
	successCount      int
	closeCount        int
	rejectionReasons  []string
	deliveredSessions []minter.MintResult
	successErr        error
	onSend            func()
	beforePoll        func()
}

func closeOnce(ch chan struct{}) {
	select {
	case <-ch:
	default:
		close(ch)
	}
}

func (f *fakeBrokerChannel) DeliveredSessions() []minter.MintResult {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]minter.MintResult(nil), f.deliveredSessions...)
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
	if f.beforePoll != nil {
		f.beforePoll()
	}
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

func (f *fakeBrokerChannel) SendMintSuccess(_ context.Context, _ minter.MintRequest, session minter.MintResult) (*minter.SendMessageResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.successCount++
	f.deliveredSessions = append(f.deliveredSessions, session)
	if f.onSend != nil {
		f.onSend()
	}
	if f.successErr != nil {
		return nil, f.successErr
	}
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
	calls        int
	lifetimes    []sessionLifetime
	persistent   bool
	onCreate     func()
	revokes      []revokedSession
	revokeErr    error
	listIDs      []string
	listErr      error
	listIDsCalls []string
}

func (r *recordingSessionCreator) ListEphemeralSessionIDs(_ context.Context, topicID string) ([]string, error) {
	r.listIDsCalls = append(r.listIDsCalls, topicID)
	return r.listIDs, r.listErr
}

type revokedSession struct {
	topicID   string
	sessionID string
}

func (r *recordingSessionCreator) RevokeEphemeralSession(_ context.Context, topicID string, sessionID string) error {
	r.revokes = append(r.revokes, revokedSession{topicID: topicID, sessionID: sessionID})
	return r.revokeErr
}

func (r *recordingSessionCreator) CreateEphemeralSession(_ context.Context, _ string, _ minter.MintRequest, lifetime sessionLifetime) (minter.MintResult, error) {
	r.calls++
	r.lifetimes = append(r.lifetimes, lifetime)
	if r.onCreate != nil {
		r.onCreate()
	}
	if r.persistent {
		return minter.MintResult{
			SessionID:  "session-1",
			Token:      "browser-token",
			Persistent: true,
		}, nil
	}
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
	revoked chan revokedSession
}

func (f fakeSessionCreator) ListEphemeralSessionIDs(_ context.Context, _ string) ([]string, error) {
	return nil, nil
}

func (f fakeSessionCreator) RevokeEphemeralSession(_ context.Context, topicID string, sessionID string) error {
	if f.revoked != nil {
		select {
		case f.revoked <- revokedSession{topicID: topicID, sessionID: sessionID}:
		default:
		}
	}
	return nil
}

func (f fakeSessionCreator) CreateEphemeralSession(ctx context.Context, _ string, _ minter.MintRequest, _ sessionLifetime) (minter.MintResult, error) {
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

func (f *fakeCDP) Start(context.Context, func()) {}
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
