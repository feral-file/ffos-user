package ephemeralsessions

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-controld/state"
	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

func TestService_HandleListSessions(t *testing.T) {
	defer state.ResetForTesting()
	state.GetState().Relayer.TopicID = "topic-1"

	var seen struct {
		Method string
		Path   string
		Topic  string
		APIKey string
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.Method = r.Method
		seen.Path = r.URL.Path
		seen.Topic = r.URL.Query().Get("topicID")
		seen.APIKey = r.Header.Get("API-KEY")
		_, _ = w.Write([]byte(`{
			"sessions": [{
				"id": "session-1",
				"status": "active",
				"createdAt": "2026-05-15T00:00:00.000Z",
				"expiresAt": "2026-06-14T00:00:00.000Z",
				"revokedAt": null,
				"createdIp": "203.0.113.10",
				"createdUserAgent": "Feral File Mobile",
				"browserUserAgent": "Mozilla/5.0",
				"browserName": "Chrome",
				"label": "objkt on Chrome",
				"lastUsedAt": null,
				"lastUsedIp": null,
				"lastUsedUserAgent": null,
				"token": "raw-token",
				"tokenHash": "hash"
			}]
		}`))
	}))
	defer server.Close()

	svc := New(server.URL, "api-key-1", wrapper.NewHTTPClient(), wrapper.NewJSON(), zap.NewNop())
	result, err := svc.HandleListSessions(context.Background(), nil)

	require.NoError(t, err)
	resp, ok := result.(listResponse)
	require.True(t, ok)
	require.Len(t, resp.Sessions, 1)
	assert.True(t, resp.OK)
	assert.Equal(t, "session-1", resp.Sessions[0].ID)
	assert.Equal(t, "Chrome", *resp.Sessions[0].BrowserName)
	assert.Equal(t, http.MethodGet, seen.Method)
	assert.Equal(t, "/api/ephemeral-sessions", seen.Path)
	assert.Equal(t, "topic-1", seen.Topic)
	assert.Equal(t, "api-key-1", seen.APIKey)

	raw, err := json.Marshal(result)
	require.NoError(t, err)
	assert.NotContains(t, string(raw), "raw-token")
	assert.NotContains(t, string(raw), "tokenHash")
}

func TestService_HandleRevokeSession(t *testing.T) {
	defer state.ResetForTesting()
	state.GetState().Relayer.TopicID = "topic-1"

	var seen struct {
		Method string
		Path   string
		Topic  string
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.Method = r.Method
		seen.Path = r.URL.EscapedPath()
		seen.Topic = r.URL.Query().Get("topicID")
		_, _ = w.Write([]byte(`{
			"session": {
				"id": "session/1",
				"status": "revoked",
				"createdAt": "2026-05-15T00:00:00.000Z",
				"expiresAt": "2026-06-14T00:00:00.000Z",
				"revokedAt": "2026-05-16T00:00:00.000Z"
			},
			"token": "raw-token"
		}`))
	}))
	defer server.Close()

	svc := New(server.URL, "", wrapper.NewHTTPClient(), wrapper.NewJSON(), zap.NewNop())
	result, err := svc.HandleRevokeSession(context.Background(), map[string]any{"sessionID": "session/1"})

	require.NoError(t, err)
	resp, ok := result.(revokeResponse)
	require.True(t, ok)
	assert.True(t, resp.OK)
	assert.Equal(t, "revoked", resp.Status)
	assert.Equal(t, "session/1", resp.Session.ID)
	assert.Equal(t, http.MethodDelete, seen.Method)
	assert.Equal(t, "/api/ephemeral-sessions/session%2F1", seen.Path)
	assert.Equal(t, "topic-1", seen.Topic)

	raw, err := json.Marshal(result)
	require.NoError(t, err)
	assert.NotContains(t, string(raw), "raw-token")
}

func TestService_HandleRevokeSession_RejectsInvalidSuccessBody(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{
			name: "missing session",
			body: `{"ok":true}`,
		},
		{
			name: "empty session id",
			body: `{"session":{"id":"","status":"revoked"}}`,
		},
		{
			name: "mismatched session id",
			body: `{"session":{"id":"session-2","status":"revoked"}}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			defer state.ResetForTesting()
			state.GetState().Relayer.TopicID = "topic-1"

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(tt.body))
			}))
			defer server.Close()

			svc := New(server.URL, "", wrapper.NewHTTPClient(), wrapper.NewJSON(), zap.NewNop())
			result, err := svc.HandleRevokeSession(context.Background(), map[string]any{"sessionID": "session-1"})

			require.NoError(t, err)
			resp := assertErrorCode(t, result, errRelayer)
			assert.True(t, resp.Error.Retryable)
		})
	}
}

func TestService_ValidationErrors(t *testing.T) {
	tests := []struct {
		name string
		call func(Service) (any, error)
		code string
	}{
		{
			name: "list rejects non empty request",
			call: func(s Service) (any, error) {
				return s.HandleListSessions(context.Background(), map[string]any{"unexpected": true})
			},
			code: errInvalidRequest,
		},
		{
			name: "revoke requires session id",
			call: func(s Service) (any, error) {
				return s.HandleRevokeSession(context.Background(), map[string]any{})
			},
			code: errInvalidRequest,
		},
		{
			name: "revoke rejects blank session id",
			call: func(s Service) (any, error) {
				return s.HandleRevokeSession(context.Background(), map[string]any{"sessionID": " "})
			},
			code: errInvalidRequest,
		},
		{
			name: "revoke rejects non string session id",
			call: func(s Service) (any, error) {
				return s.HandleRevokeSession(context.Background(), map[string]any{"sessionID": 42})
			},
			code: errInvalidRequest,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			defer state.ResetForTesting()
			state.GetState().Relayer.TopicID = "topic-1"

			svc := New("http://127.0.0.1", "", wrapper.NewHTTPClient(), wrapper.NewJSON(), zap.NewNop())
			result, err := tt.call(svc)

			require.NoError(t, err)
			assertErrorCode(t, result, tt.code)
		})
	}
}

func TestService_MissingTopic(t *testing.T) {
	defer state.ResetForTesting()

	svc := New("http://127.0.0.1", "", wrapper.NewHTTPClient(), wrapper.NewJSON(), zap.NewNop())
	result, err := svc.HandleListSessions(context.Background(), nil)

	require.NoError(t, err)
	resp := assertErrorCode(t, result, errNotReady)
	assert.True(t, resp.Error.Retryable)
}

func TestService_HTTPErrorMapping(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		revoke     bool
		wantCode   string
		retryable  bool
	}{
		{
			name:       "list maps 401 to unauthorized",
			statusCode: http.StatusUnauthorized,
			wantCode:   errUnauthorized,
			retryable:  false,
		},
		{
			name:       "revoke maps 404 to not found",
			statusCode: http.StatusNotFound,
			revoke:     true,
			wantCode:   errNotFound,
			retryable:  false,
		},
		{
			name:       "list maps 404 to relayer error",
			statusCode: http.StatusNotFound,
			wantCode:   errRelayer,
			retryable:  true,
		},
		{
			name:       "revoke maps 500 to relayer error",
			statusCode: http.StatusInternalServerError,
			revoke:     true,
			wantCode:   errRelayer,
			retryable:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			defer state.ResetForTesting()
			state.GetState().Relayer.TopicID = "topic-1"

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.statusCode)
			}))
			defer server.Close()

			svc := New(server.URL, "", wrapper.NewHTTPClient(), wrapper.NewJSON(), zap.NewNop())
			var result any
			var err error
			if tt.revoke {
				result, err = svc.HandleRevokeSession(context.Background(), map[string]any{"sessionID": "session-1"})
			} else {
				result, err = svc.HandleListSessions(context.Background(), nil)
			}

			require.NoError(t, err)
			resp := assertErrorCode(t, result, tt.wantCode)
			assert.Equal(t, tt.retryable, resp.Error.Retryable)
		})
	}
}

func assertErrorCode(t *testing.T, result any, want string) errorResponse {
	t.Helper()
	resp, ok := result.(errorResponse)
	require.True(t, ok)
	assert.False(t, resp.OK)
	assert.Equal(t, want, resp.Error.Code)
	return resp
}
