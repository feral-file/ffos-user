package ephemeralsessions

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-controld/state"
	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

const (
	errInvalidRequest = "invalid_request"
	errNotReady       = "not_ready"
	errNotFound       = "not_found"
	errUnauthorized   = "unauthorized"
	errRelayer        = "relayer_error"
)

type Service interface {
	HandleListSessions(ctx context.Context, args map[string]any) (any, error)
	HandleRevokeSession(ctx context.Context, args map[string]any) (any, error)
}

type service struct {
	baseURL    string
	apiKey     string
	httpClient wrapper.HTTPClient
	json       wrapper.JSON
	logger     *zap.Logger
}

type Session struct {
	ID                string  `json:"id"`
	Status            string  `json:"status"`
	CreatedAt         string  `json:"createdAt"`
	ExpiresAt         string  `json:"expiresAt"`
	RevokedAt         *string `json:"revokedAt"`
	CreatedIP         *string `json:"createdIp"`
	CreatedUserAgent  *string `json:"createdUserAgent"`
	BrowserUserAgent  *string `json:"browserUserAgent"`
	BrowserName       *string `json:"browserName"`
	Label             *string `json:"label"`
	LastUsedAt        *string `json:"lastUsedAt"`
	LastUsedIP        *string `json:"lastUsedIp"`
	LastUsedUserAgent *string `json:"lastUsedUserAgent"`
}

type errorResponse struct {
	OK    bool          `json:"ok"`
	Error responseError `json:"error"`
}

type responseError struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
}

type listResponse struct {
	OK       bool      `json:"ok"`
	Sessions []Session `json:"sessions"`
}

type revokeResponse struct {
	OK      bool    `json:"ok"`
	Status  string  `json:"status"`
	Session Session `json:"session"`
}

func New(baseURL string, apiKey string, httpClient wrapper.HTTPClient, json wrapper.JSON, logger *zap.Logger) Service {
	if json == nil {
		json = wrapper.NewJSON()
	}
	if logger == nil {
		logger = zap.NewNop()
	}
	return &service{
		baseURL:    strings.TrimRight(baseURL, "/"),
		apiKey:     apiKey,
		httpClient: httpClient,
		json:       json,
		logger:     logger,
	}
}

func (s *service) HandleListSessions(ctx context.Context, args map[string]any) (any, error) {
	if len(args) > 0 {
		return commandError(errInvalidRequest, "listEphemeralSessions request must be empty", false), nil
	}
	topicID, ok := currentTopicID()
	if !ok {
		return commandError(errNotReady, "relayer topic is not ready", true), nil
	}
	endpoint, err := s.endpoint("/api/ephemeral-sessions", topicID)
	if err != nil {
		s.logger.Warn("Failed to build relayer ephemeral sessions list URL", zap.Error(err))
		return commandError(errRelayer, "failed to build relayer request", true), nil
	}

	req, err := s.newRequest(ctx, http.MethodGet, endpoint)
	if err != nil {
		return nil, fmt.Errorf("build relayer ephemeral sessions list request: %w", err)
	}
	resp, err := s.do(req)
	if err != nil {
		s.logger.Warn("Failed to list relayer ephemeral sessions", zap.Error(err))
		return commandError(errRelayer, "failed to reach relayer", true), nil
	}
	defer func() {
		_ = resp.Body.Close()
	}()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return s.httpError(resp.StatusCode, false), nil
	}

	var decoded struct {
		Sessions []Session `json:"sessions"`
	}
	if err := s.json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		s.logger.Warn("Failed to decode relayer ephemeral sessions list response", zap.Error(err))
		return commandError(errRelayer, "failed to decode relayer response", true), nil
	}
	return listResponse{OK: true, Sessions: decoded.Sessions}, nil
}

func (s *service) HandleRevokeSession(ctx context.Context, args map[string]any) (any, error) {
	sessionID, ok := stringArg(args, "sessionID")
	if !ok {
		return commandError(errInvalidRequest, "revokeEphemeralSession requires sessionID", false), nil
	}
	topicID, ok := currentTopicID()
	if !ok {
		return commandError(errNotReady, "relayer topic is not ready", true), nil
	}
	endpoint, err := s.endpoint("/api/ephemeral-sessions/"+url.PathEscape(sessionID), topicID)
	if err != nil {
		s.logger.Warn("Failed to build relayer ephemeral session revoke URL", zap.Error(err))
		return commandError(errRelayer, "failed to build relayer request", true), nil
	}

	req, err := s.newRequest(ctx, http.MethodDelete, endpoint)
	if err != nil {
		return nil, fmt.Errorf("build relayer ephemeral session revoke request: %w", err)
	}
	resp, err := s.do(req)
	if err != nil {
		s.logger.Warn("Failed to revoke relayer ephemeral session", zap.Error(err))
		return commandError(errRelayer, "failed to reach relayer", true), nil
	}
	defer func() {
		_ = resp.Body.Close()
	}()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return s.httpError(resp.StatusCode, true), nil
	}

	var decoded struct {
		Session Session `json:"session"`
	}
	if err := s.json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		s.logger.Warn("Failed to decode relayer ephemeral session revoke response", zap.Error(err))
		return commandError(errRelayer, "failed to decode relayer response", true), nil
	}
	if decoded.Session.ID == "" || decoded.Session.ID != sessionID {
		s.logger.Warn("Relayer ephemeral session revoke response did not confirm requested session",
			zap.String("requestedSessionID", sessionID),
			zap.String("responseSessionID", decoded.Session.ID),
		)
		return commandError(errRelayer, "relayer response did not confirm revoked session", true), nil
	}
	return revokeResponse{OK: true, Status: "revoked", Session: decoded.Session}, nil
}

func (s *service) endpoint(path string, topicID string) (string, error) {
	if strings.TrimSpace(s.baseURL) == "" {
		return "", errors.New("relayer base URL is required")
	}
	endpoint, err := url.Parse(s.baseURL + path)
	if err != nil {
		return "", err
	}
	q := endpoint.Query()
	q.Set("topicID", topicID)
	endpoint.RawQuery = q.Encode()
	return endpoint.String(), nil
}

func (s *service) newRequest(ctx context.Context, method string, endpoint string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "feral-controld")
	if apiKey := strings.TrimSpace(s.apiKey); apiKey != "" {
		req.Header.Set("API-KEY", apiKey)
	}
	return req, nil
}

func (s *service) do(req *http.Request) (*http.Response, error) {
	if s.httpClient == nil {
		return nil, errors.New("http client is required")
	}
	return s.httpClient.Do(req)
}

func (s *service) httpError(statusCode int, mapNotFound bool) errorResponse {
	switch statusCode {
	case http.StatusUnauthorized:
		return commandError(errUnauthorized, "relayer rejected the configured API key", false)
	case http.StatusNotFound:
		if mapNotFound {
			return commandError(errNotFound, "ephemeral session not found", false)
		}
	}
	return commandError(errRelayer, fmt.Sprintf("relayer returned status %d", statusCode), true)
}

func currentTopicID() (string, bool) {
	topicID := strings.TrimSpace(state.GetState().Relayer.TopicID)
	return topicID, topicID != ""
}

func stringArg(args map[string]any, key string) (string, bool) {
	value, ok := args[key]
	if !ok {
		return "", false
	}
	s, ok := value.(string)
	if !ok {
		return "", false
	}
	s = strings.TrimSpace(s)
	return s, s != ""
}

func commandError(code string, message string, retryable bool) errorResponse {
	return errorResponse{
		OK: false,
		Error: responseError{
			Code:      code,
			Message:   message,
			Retryable: retryable,
		},
	}
}
