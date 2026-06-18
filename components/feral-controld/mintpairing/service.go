package mintpairing

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	minter "github.com/feral-file/ff-art-computer-handoff/clients/ephemeral-token-minter/go"
	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-controld/cdp"
	"github.com/feral-file/ffos-user/components/feral-controld/config"
	"github.com/feral-file/ffos-user/components/feral-controld/qrdisplay"
	"github.com/feral-file/ffos-user/components/feral-controld/relayer"
	"github.com/feral-file/ffos-user/components/feral-controld/state"
	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

const (
	defaultIdleTTL            = 5 * time.Minute
	defaultPollInterval       = 500 * time.Millisecond
	defaultApprovalTimeout    = 5 * time.Minute
	minSessionTTLSeconds      = 90
	defaultSessionTTLSeconds  = 3600
	maxSessionTTLSeconds      = 86400
	terminalOperationTimeout  = 750 * time.Millisecond
	displayRecoveryTimeout    = 500 * time.Millisecond
	stopCleanupTimeout        = 1500 * time.Millisecond
	channelCloseTimeout       = 500 * time.Millisecond
	maxApprovalRequestIDBytes = 16

	approvalCancellationStatus = "cancelled" //nolint:misspell // Wire protocol status is documented with this spelling.
)

type Options struct {
	Enabled         bool
	BrokerBaseURL   string
	IdleTTL         time.Duration
	PollInterval    time.Duration
	ApprovalTimeout time.Duration
	RelayerBaseURL  string
}

func OptionsFromConfig(cfg *config.MintPairingConfig, relayerEndpoint string) Options {
	opts := Options{
		RelayerBaseURL:  relayerHTTPBaseString(relayerEndpoint),
		IdleTTL:         defaultIdleTTL,
		PollInterval:    defaultPollInterval,
		ApprovalTimeout: defaultApprovalTimeout,
	}
	if cfg == nil {
		return opts
	}
	opts.Enabled = cfg.Enabled
	opts.BrokerBaseURL = strings.TrimSpace(cfg.BrokerBaseURL)
	if cfg.IdleTTLSeconds > 0 {
		opts.IdleTTL = time.Duration(cfg.IdleTTLSeconds) * time.Second
	}
	if cfg.PollIntervalMillis > 0 {
		opts.PollInterval = time.Duration(cfg.PollIntervalMillis) * time.Millisecond
	}
	if cfg.ApprovalTimeoutSeconds > 0 {
		opts.ApprovalTimeout = time.Duration(cfg.ApprovalTimeoutSeconds) * time.Second
	}
	return opts
}

type Service interface {
	Start(ctx context.Context)
	Stop()
	HandleStartPairingSession(ctx context.Context, args map[string]any) (any, error)
	HandleApprovalDecision(ctx context.Context, args map[string]any) (any, error)
}

type service struct {
	opts           Options
	broker         brokerStarter
	sessionCreator sessionCreator
	relayer        relayer.Relayer
	cdp            cdp.CDP
	json           wrapper.JSON
	logger         *zap.Logger

	ctx    context.Context
	cancel context.CancelFunc

	startMu sync.Mutex
	// displayMu serializes player overlay mutations so a delayed terminal hide
	// cannot overtake a replacement pairing-code display.
	displayMu         sync.Mutex
	mu                sync.Mutex
	active            *activePairing
	displayOwner      *activePairing
	displayGeneration uint64
	pending           map[string]*pendingApproval
	doneMap           map[string]completedApproval
}

type brokerStarter interface {
	StartChannel(ctx context.Context, opts minter.StartChannelOptions) (brokerChannel, error)
}

type sessionCreator interface {
	CreateEphemeralSession(ctx context.Context, topicID string, request minter.MintRequest) (minter.MintResult, error)
}

type brokerChannel interface {
	PairingDisplay() minter.PairingDisplay
	MinterPublicKeyJWK() minter.PublicJWK
	PollMintRequest(ctx context.Context, afterSeq int64) (*minter.MintRequest, int64, error)
	SendMintSuccess(ctx context.Context, request minter.MintRequest, result minter.MintResult) (*minter.SendMessageResult, error)
	SendMintRejection(ctx context.Context, request minter.MintRequest, rejection minter.MintRejection) (*minter.SendMessageResult, error)
	Close(ctx context.Context) error
}

type brokerChannelAdapter struct {
	channel *minter.Channel
}

func (b brokerChannelAdapter) PairingDisplay() minter.PairingDisplay {
	return b.channel.PairingDisplay()
}

func (b brokerChannelAdapter) MinterPublicKeyJWK() minter.PublicJWK {
	return b.channel.MinterPublicKeyJWK()
}

func (b brokerChannelAdapter) PollMintRequest(ctx context.Context, afterSeq int64) (*minter.MintRequest, int64, error) {
	request, err := b.channel.PollMintRequest(ctx, afterSeq)
	if err != nil {
		return nil, afterSeq, err
	}
	if request == nil {
		return nil, afterSeq, nil
	}
	return request, maxInt64(afterSeq, request.Seq), nil
}

func (b brokerChannelAdapter) SendMintSuccess(ctx context.Context, request minter.MintRequest, result minter.MintResult) (*minter.SendMessageResult, error) {
	return b.channel.SendMintSuccess(ctx, request, result)
}

func (b brokerChannelAdapter) SendMintRejection(ctx context.Context, request minter.MintRequest, rejection minter.MintRejection) (*minter.SendMessageResult, error) {
	return b.channel.SendMintRejection(ctx, request, rejection)
}

func (b brokerChannelAdapter) Close(ctx context.Context) error {
	return b.channel.Close(ctx)
}

type realBrokerStarter struct {
	client *minter.Client
}

func (b realBrokerStarter) StartChannel(ctx context.Context, opts minter.StartChannelOptions) (brokerChannel, error) {
	channel, err := b.client.StartChannel(ctx, opts)
	if err != nil {
		return nil, err
	}
	return brokerChannelAdapter{channel: channel}, nil
}

type pendingApproval struct {
	approvalRequestID string
	topicID           string
	channelID         string
	requestMessageID  string
	browserName       string
	expiresAt         time.Time
	decisionCh        chan approvalDecisionRequest
	accepted          *approvalDecisionRequest
}

type activePairing struct {
	channel     brokerChannel
	channelID   string
	pairingCode string
	expiresAt   time.Time
	displayGen  uint64
	cancel      context.CancelFunc
	done        chan struct{}
}

type completedApproval struct {
	accepted  approvalDecisionRequest
	expiresAt time.Time
}

type startPairingResponse struct {
	OK          bool   `json:"ok"`
	Status      string `json:"status"`
	ChannelID   string `json:"channelID"`
	PairingCode string `json:"pairingCode"`
	ExpiresAt   string `json:"expiresAt,omitempty"`
}

type approvalDecisionRequest struct {
	Version           int            `json:"v"`
	ApprovalRequestID string         `json:"approvalRequestID"`
	TopicID           string         `json:"topicID"`
	ChannelID         string         `json:"channelID"`
	RequestMessageID  string         `json:"requestMessageID"`
	Decision          string         `json:"decision"`
	Reason            string         `json:"reason,omitempty"`
	Retryable         bool           `json:"retryable,omitempty"`
	DecidedAt         string         `json:"decidedAt,omitempty"`
	Controller        map[string]any `json:"controller,omitempty"`
}

type approvalResponse struct {
	OK                bool              `json:"ok"`
	Status            string            `json:"status,omitempty"`
	ApprovalRequestID string            `json:"approvalRequestID,omitempty"`
	Error             *approvalRPCError `json:"error,omitempty"`
}

type approvalRPCError struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
}

func New(
	opts Options,
	relayerClient relayer.Relayer,
	cdpClient cdp.CDP,
	httpClient wrapper.HTTPClient,
	relayerAPIKey string,
	json wrapper.JSON,
	logger *zap.Logger,
) Service {
	brokerHTTPClient := &http.Client{Timeout: wrapper.HTTPClientTimeout}
	return newService(opts, realBrokerStarter{client: minter.NewClient(brokerHTTPClient)}, NewRelayerSessionCreator(opts.RelayerBaseURL, relayerAPIKey, httpClient, json), relayerClient, cdpClient, json, logger)
}

func newService(
	opts Options,
	broker brokerStarter,
	sessionCreator sessionCreator,
	relayerClient relayer.Relayer,
	cdpClient cdp.CDP,
	json wrapper.JSON,
	logger *zap.Logger,
) Service {
	if json == nil {
		json = wrapper.NewJSON()
	}
	return &service{
		opts:           opts,
		broker:         broker,
		sessionCreator: sessionCreator,
		relayer:        relayerClient,
		cdp:            cdpClient,
		json:           json,
		logger:         logger,
		pending:        make(map[string]*pendingApproval),
		doneMap:        make(map[string]completedApproval),
	}
}

func (s *service) Start(ctx context.Context) {
	if s == nil {
		return
	}
	runCtx, cancel := context.WithCancel(ctx)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cancel != nil {
		s.cancel()
	}
	s.ctx = runCtx
	s.cancel = cancel
}

func (s *service) Stop() {
	if s == nil {
		return
	}
	s.mu.Lock()
	cancel := s.cancel
	active := s.active
	var activeDone <-chan struct{}
	s.cancel = nil
	s.ctx = nil
	if active != nil {
		active.cancel()
		activeDone = active.done
		s.active = nil
	}
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if activeDone == nil {
		return
	}
	// main.go forces process exit after two seconds, so mint pairing shutdown
	// must use a smaller cleanup budget than the process-level guard.
	timer := time.NewTimer(stopCleanupTimeout)
	defer timer.Stop()
	select {
	case <-activeDone:
	case <-timer.C:
		s.logger.Warn("Timed out waiting for mint pairing cleanup", zap.String("channelID", active.channelID))
	}
}

func (s *service) HandleStartPairingSession(ctx context.Context, _ map[string]any) (any, error) {
	if s == nil || !s.opts.Enabled {
		return commandError("disabled", "mint pairing is not enabled", false), nil
	}
	if strings.TrimSpace(s.opts.BrokerBaseURL) == "" {
		return commandError("invalid_config", "mint pairing broker base URL is not configured", false), nil
	}
	topicID := strings.TrimSpace(state.GetState().Relayer.TopicID)
	if topicID == "" {
		return commandError("topic_not_ready", "relayer topic is not ready", true), nil
	}

	s.startMu.Lock()
	defer s.startMu.Unlock()

	if active := s.currentActive(); active != nil {
		if err := s.showPairingCode(ctx, active); err != nil {
			s.logger.Warn("Failed to redisplay active mint pairing code", zap.Error(err), zap.String("channelID", active.channelID))
			return commandError("display_unavailable", "failed to display mint pairing QR code", true), nil
		}
		return startPairingResponse{
			OK:          true,
			Status:      "already_started",
			ChannelID:   active.channelID,
			PairingCode: active.pairingCode,
			ExpiresAt:   formatOptionalTime(active.expiresAt),
		}, nil
	}

	runCtx := ctx
	s.mu.Lock()
	if s.ctx != nil {
		runCtx = s.ctx
	}
	s.mu.Unlock()

	displayCtx, cancelDisplay := context.WithTimeout(ctx, wrapper.HTTPClientTimeout)
	defer cancelDisplay()
	channel, err := s.broker.StartChannel(displayCtx, minter.StartChannelOptions{
		BrokerBaseURL:      s.opts.BrokerBaseURL,
		IdleTTL:            s.opts.IdleTTL,
		ShortCodeRequested: true,
	})
	if err != nil {
		s.logger.Warn("Failed to start mint pairing broker channel", zap.Error(err))
		return commandError("broker_unavailable", "failed to start mint pairing broker channel", true), nil
	}

	display := channel.PairingDisplay()
	pairingCode := strings.TrimSpace(display.ShortCode)
	if pairingCode == "" {
		s.closeChannel(channel)
		return commandError("broker_response_invalid", "broker did not return a pairing code", true), nil
	}
	expiresAt := display.ExpiresAt
	if expiresAt.IsZero() {
		expiresAt = time.Now().Add(s.opts.IdleTTL)
	}
	sessionCtx, sessionCancel := context.WithDeadline(runCtx, expiresAt)
	active := &activePairing{
		channel:     channel,
		channelID:   display.ChannelID,
		pairingCode: pairingCode,
		expiresAt:   expiresAt,
		cancel:      sessionCancel,
		done:        make(chan struct{}),
	}

	if err := s.showPairingCode(ctx, active); err != nil {
		sessionCancel()
		s.closeChannel(channel)
		s.logger.Warn("Failed to display mint pairing QR code", zap.Error(err), zap.String("channelID", active.channelID))
		return commandError("display_unavailable", "failed to display mint pairing QR code", true), nil
	}

	s.mu.Lock()
	s.active = active
	s.mu.Unlock()

	go s.waitForBrowserAndApproval(sessionCtx, active, topicID)

	return startPairingResponse{
		OK:          true,
		Status:      "started",
		ChannelID:   active.channelID,
		PairingCode: active.pairingCode,
		ExpiresAt:   formatOptionalTime(active.expiresAt),
	}, nil
}

func (s *service) HandleApprovalDecision(ctx context.Context, args map[string]any) (any, error) {
	if s == nil {
		return approvalError("", "not_found", "mint pairing is not enabled", false), nil
	}
	decision, err := s.parseDecision(args)
	if err != nil {
		return approvalError(decision.ApprovalRequestID, "invalid_request", err.Error(), false), nil
	}

	s.mu.Lock()
	s.pruneCompletedLocked()
	pending := s.pending[decision.ApprovalRequestID]
	if pending == nil {
		completed, ok := s.doneMap[decision.ApprovalRequestID]
		if ok {
			if sameDecision(completed.accepted, decision) {
				s.mu.Unlock()
				return approvalResponse{OK: true, Status: "already_accepted", ApprovalRequestID: decision.ApprovalRequestID}, nil
			}
			s.mu.Unlock()
			return approvalError(decision.ApprovalRequestID, "already_decided", "approval request already has a terminal decision", false), nil
		}
		s.mu.Unlock()
		return approvalError(decision.ApprovalRequestID, "not_found", "approval request is not pending", false), nil
	}
	if pending.accepted != nil {
		status := "already_accepted"
		if !sameDecision(*pending.accepted, decision) {
			s.mu.Unlock()
			return approvalError(decision.ApprovalRequestID, "already_decided", "approval request already has a terminal decision", false), nil
		}
		s.mu.Unlock()
		return approvalResponse{OK: true, Status: status, ApprovalRequestID: decision.ApprovalRequestID}, nil
	}
	if !pending.expiresAt.IsZero() && time.Now().After(pending.expiresAt) {
		s.mu.Unlock()
		return approvalError(decision.ApprovalRequestID, "expired", "approval request expired", false), nil
	}
	if decision.TopicID != pending.topicID || decision.TopicID != state.GetState().Relayer.TopicID {
		s.mu.Unlock()
		return approvalError(decision.ApprovalRequestID, "topic_mismatch", "approval decision does not match this device topic", false), nil
	}
	if decision.ChannelID != pending.channelID || decision.RequestMessageID != pending.requestMessageID {
		s.mu.Unlock()
		return approvalError(decision.ApprovalRequestID, "request_mismatch", "approval decision does not match the pending mint request", false), nil
	}
	pending.accepted = &decision
	s.mu.Unlock()

	select {
	case pending.decisionCh <- decision:
	default:
	}

	return approvalResponse{OK: true, Status: "accepted", ApprovalRequestID: decision.ApprovalRequestID}, nil
}

func (s *service) parseDecision(args map[string]any) (approvalDecisionRequest, error) {
	var decision approvalDecisionRequest
	raw, err := s.json.Marshal(args)
	if err != nil {
		return decision, fmt.Errorf("marshal approval decision: %w", err)
	}
	if err := s.json.Unmarshal(raw, &decision); err != nil {
		return decision, fmt.Errorf("decode approval decision: %w", err)
	}
	if decision.Version != 1 {
		return decision, errors.New("approval decision version must be 1")
	}
	if decision.ApprovalRequestID == "" || decision.TopicID == "" || decision.ChannelID == "" || decision.RequestMessageID == "" {
		return decision, errors.New("approvalRequestID, topicID, channelID, and requestMessageID are required")
	}
	switch decision.Decision {
	case "approve":
	case "reject":
		if strings.TrimSpace(decision.Reason) == "" {
			decision.Reason = "rejected_by_user"
		}
	default:
		return decision, errors.New("decision must be approve or reject")
	}
	return decision, nil
}

func (s *service) waitForBrowserAndApproval(ctx context.Context, active *activePairing, topicID string) {
	terminalSent := false
	defer func() {
		displayGeneration, restoreDisplay := s.releaseDisplayOwnership(active)
		if active.done != nil {
			close(active.done)
		}
		if restoreDisplay {
			go s.restoreDefaultDisplay(active.channelID, displayGeneration)
		}
		if terminalSent {
			// Terminal broker messages must remain pollable after controld sends
			// them. The broker's TTL handles cleanup; explicit Close is only for
			// channels that never reached browser-visible terminal delivery.
			return
		}
		s.closeChannel(active.channel)
	}()

	request, err := s.waitForMintRequest(ctx, active.channel)
	if err != nil {
		if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			s.logger.Warn("Mint pairing request wait failed", zap.Error(err), zap.String("channelID", active.channelID))
		}
		return
	}

	approvalRequestID, err := newApprovalRequestID()
	if err != nil {
		s.logger.Warn("Failed to create mint pairing approval request id", zap.Error(err), zap.String("channelID", active.channelID))
		return
	}
	expiresAt := time.Now().Add(s.opts.ApprovalTimeout)
	if !active.expiresAt.IsZero() && active.expiresAt.Before(expiresAt) {
		expiresAt = active.expiresAt
	}
	pending := &pendingApproval{
		approvalRequestID: approvalRequestID,
		topicID:           topicID,
		channelID:         request.ChannelID,
		requestMessageID:  request.MessageID,
		browserName:       browserDisplayName(request.BrowserInfo),
		expiresAt:         expiresAt,
		decisionCh:        make(chan approvalDecisionRequest, 1),
	}
	s.registerPending(pending)
	defer s.unregisterPending(approvalRequestID)

	if err := qrdisplay.ShowRequestReceived(ctx, s.cdp, pending.browserName); err != nil {
		s.logger.Warn("Failed to display mint pairing request status", zap.Error(err), zap.String("channelID", active.channelID))
	}

	if err := s.sendApprovalRequest(ctx, approvalRequestID, topicID, *request, active.channel.MinterPublicKeyJWK(), expiresAt); err != nil {
		_, sendErr := active.channel.SendMintRejection(ctx, *request, minter.MintRejection{Reason: "approval_unavailable", Retryable: true})
		if sendErr != nil {
			s.logger.Warn("Failed to send approval request and browser rejection", zap.Error(errors.Join(err, sendErr)), zap.String("channelID", active.channelID))
			return
		}
		terminalSent = true
		s.logger.Warn("Failed to send mint pairing approval request", zap.Error(err), zap.String("channelID", active.channelID))
		return
	}

	expireTimer := time.NewTimer(time.Until(expiresAt))
	defer expireTimer.Stop()

	select {
	case <-ctx.Done():
		if !time.Now().Before(expiresAt) {
			if decision, ok := s.acceptedDecision(pending); ok {
				terminalSent, err = s.completeDecisionWithBoundedContexts(context.Background(), active.channel, *request, topicID, approvalRequestID, decision)
				if err != nil {
					s.logger.Warn("Failed to complete mint pairing decision", zap.Error(err), zap.String("channelID", active.channelID))
				}
				return
			}
			terminalSent = s.sendApprovalExpired(active, *request, approvalRequestID)
			return
		}
		terminalSent = s.sendApprovalCancelled(active, *request, approvalRequestID)
		return
	case <-expireTimer.C:
		if decision, ok := s.acceptedDecision(pending); ok {
			terminalSent, err = s.completeDecisionWithBoundedContexts(context.Background(), active.channel, *request, topicID, approvalRequestID, decision)
			if err != nil {
				s.logger.Warn("Failed to complete mint pairing decision", zap.Error(err), zap.String("channelID", active.channelID))
			}
			return
		}
		terminalSent = s.sendApprovalExpired(active, *request, approvalRequestID)
	case decision := <-pending.decisionCh:
		terminalSent, err = s.completeDecisionWithBoundedContexts(context.Background(), active.channel, *request, topicID, approvalRequestID, decision)
		if err != nil {
			s.logger.Warn("Failed to complete mint pairing decision", zap.Error(err), zap.String("channelID", active.channelID))
		}
	}
}

func (s *service) sendApprovalCancelled(active *activePairing, request minter.MintRequest, approvalRequestID string) bool {
	terminalCtx, cancel := context.WithTimeout(context.Background(), terminalOperationTimeout)
	defer cancel()
	_, err := active.channel.SendMintRejection(terminalCtx, request, minter.MintRejection{Reason: approvalCancellationStatus, Retryable: true})
	s.sendApprovalOutcome(terminalCtx, approvalRequestID, request.ChannelID, request.MessageID, approvalCancellationStatus)
	if err != nil {
		s.logger.Warn("Failed to send mint pairing cancellation to browser", zap.Error(err), zap.String("channelID", active.channelID))
	}
	// Shutdown cancellation is best-effort: the broker channel has its own TTL,
	// and spending another close timeout after a cancellation timeout can exceed
	// controld's process-level forced-exit budget.
	return true
}

func (s *service) sendApprovalExpired(active *activePairing, request minter.MintRequest, approvalRequestID string) bool {
	terminalCtx, cancel := context.WithTimeout(context.Background(), terminalOperationTimeout)
	defer cancel()
	_, err := active.channel.SendMintRejection(terminalCtx, request, minter.MintRejection{Reason: "approval_expired", Retryable: true})
	s.sendApprovalOutcome(terminalCtx, approvalRequestID, request.ChannelID, request.MessageID, "expired")
	if err != nil {
		s.logger.Warn("Failed to send mint pairing expiration to browser", zap.Error(err), zap.String("channelID", active.channelID))
		return false
	}
	return true
}

func (s *service) completeDecisionWithBoundedContexts(parentCtx context.Context, channel brokerChannel, request minter.MintRequest, topicID string, approvalRequestID string, decision approvalDecisionRequest) (bool, error) {
	return s.completeDecision(parentCtx, channel, request, topicID, approvalRequestID, decision)
}

func (s *service) acceptedDecision(pending *pendingApproval) (approvalDecisionRequest, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if pending.accepted == nil {
		return approvalDecisionRequest{}, false
	}
	return *pending.accepted, true
}

func (s *service) waitForMintRequest(ctx context.Context, channel brokerChannel) (*minter.MintRequest, error) {
	var afterSeq int64
	for {
		request, nextAfterSeq, err := channel.PollMintRequest(ctx, afterSeq)
		if err != nil {
			return nil, fmt.Errorf("poll mint request: %w", err)
		}
		afterSeq = maxInt64(afterSeq, nextAfterSeq)
		if request != nil {
			return request, nil
		}
		if !sleepContext(ctx, s.opts.PollInterval) {
			return nil, ctx.Err()
		}
	}
}

func (s *service) completeDecision(ctx context.Context, channel brokerChannel, request minter.MintRequest, topicID string, approvalRequestID string, decision approvalDecisionRequest) (bool, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if decision.Decision == "reject" {
		reason := strings.TrimSpace(decision.Reason)
		if reason == "" {
			reason = "rejected_by_user"
		}
		err := s.sendTerminalRejectionAndOutcome(channel, request, approvalRequestID, reason, decision.Retryable, "rejected")
		return err == nil, err
	}

	if err := qrdisplay.ShowCreatingToken(ctx, s.cdp, browserDisplayName(request.BrowserInfo)); err != nil {
		s.logger.Warn("Failed to display mint pairing token creation status", zap.Error(err), zap.String("channelID", request.ChannelID))
	}

	if !currentRelayerTopicMatches(topicID) {
		return s.rejectTopicChanged(channel, request, topicID, approvalRequestID)
	}

	sessionCtx, cancelSession := context.WithTimeout(ctx, wrapper.HTTPClientTimeout)
	result, err := s.sessionCreator.CreateEphemeralSession(sessionCtx, topicID, request)
	cancelSession()
	if err != nil {
		sendErr := s.sendTerminalRejectionAndOutcome(channel, request, approvalRequestID, "session_create_failed", true, "failed")
		if sendErr != nil {
			return false, fmt.Errorf("create session and browser rejection failed: %w", errors.Join(err, sendErr))
		}
		return true, fmt.Errorf("create session: %w", err)
	}
	if !currentRelayerTopicMatches(topicID) {
		return s.rejectTopicChanged(channel, request, topicID, approvalRequestID)
	}
	if result.RelayerBaseURL == "" {
		result.RelayerBaseURL = s.opts.RelayerBaseURL
	}
	successCtx, cancelSuccess := context.WithTimeout(context.Background(), wrapper.HTTPClientTimeout)
	_, err = channel.SendMintSuccess(successCtx, request, result)
	cancelSuccess()
	if err != nil {
		outcomeCtx, cancelOutcome := context.WithTimeout(context.Background(), terminalOperationTimeout)
		s.sendApprovalOutcome(outcomeCtx, approvalRequestID, request.ChannelID, request.MessageID, "failed")
		cancelOutcome()
		return false, fmt.Errorf("send mint success: %w", err)
	}
	outcomeCtx, cancelOutcome := context.WithTimeout(context.Background(), terminalOperationTimeout)
	s.sendApprovalOutcome(outcomeCtx, approvalRequestID, request.ChannelID, request.MessageID, "completed")
	cancelOutcome()
	return true, nil
}

func (s *service) rejectTopicChanged(channel brokerChannel, request minter.MintRequest, expectedTopicID string, approvalRequestID string) (bool, error) {
	currentTopicID := currentRelayerTopicID()
	err := s.sendTerminalRejectionAndOutcome(channel, request, approvalRequestID, "topic_changed", true, "failed")
	if err != nil {
		return false, fmt.Errorf("send topic-changed rejection: %w", err)
	}
	return true, fmt.Errorf("relayer topic changed before mint pairing session creation: expected %q, got %q", expectedTopicID, currentTopicID)
}

func (s *service) sendTerminalRejectionAndOutcome(channel brokerChannel, request minter.MintRequest, approvalRequestID string, rejectionReason string, retryable bool, outcomeStatus string) error {
	rejectionCtx, cancelRejection := context.WithTimeout(context.Background(), terminalOperationTimeout)
	_, err := channel.SendMintRejection(rejectionCtx, request, minter.MintRejection{Reason: rejectionReason, Retryable: retryable})
	cancelRejection()

	// Terminal broker delivery and controller outcome each get their own small
	// budget so a canceled or exhausted session-creation context cannot hide
	// the terminal state from both sides of the handoff.
	outcomeCtx, cancelOutcome := context.WithTimeout(context.Background(), terminalOperationTimeout)
	s.sendApprovalOutcome(outcomeCtx, approvalRequestID, request.ChannelID, request.MessageID, outcomeStatus)
	cancelOutcome()
	return err
}

func currentRelayerTopicID() string {
	return strings.TrimSpace(state.GetState().Relayer.TopicID)
}

func currentRelayerTopicMatches(topicID string) bool {
	return currentRelayerTopicID() == strings.TrimSpace(topicID)
}

func browserDisplayName(info minter.BrowserInfo) string {
	for _, candidate := range []string{info.Name, info.Label} {
		if value := strings.TrimSpace(candidate); value != "" {
			return value
		}
	}
	return "the browser"
}

func (s *service) sendApprovalRequest(ctx context.Context, approvalRequestID string, topicID string, request minter.MintRequest, minterPublicKey minter.PublicJWK, expiresAt time.Time) error {
	msg := map[string]any{
		"v":                         1,
		"topicID":                   topicID,
		"approvalRequestID":         approvalRequestID,
		"channelID":                 request.ChannelID,
		"requestMessageID":          request.MessageID,
		"origin":                    request.Origin,
		"browserInfo":               request.BrowserInfo,
		"requestedExpiresInSeconds": request.RequestedExpiresInSeconds,
		"effectiveExpiresInSeconds": effectiveSessionTTLSeconds(request.RequestedExpiresInSeconds),
		"requestedAt":               time.Now().UTC().Format(time.RFC3339),
		"expiresAt":                 expiresAt.UTC().Format(time.RFC3339),
		"challenge": map[string]any{
			"algorithm":                   minter.Algorithm,
			"browserPublicKeyFingerprint": fingerprintPublicJWK(request.BrowserPublicKeyJWK),
			"minterPublicKeyFingerprint":  fingerprintPublicJWK(minterPublicKey),
		},
	}
	return s.sendMintPairingNotification(ctx, relayer.NOTIFICATION_TYPE_MINT_PAIRING_APPROVAL_REQUEST, approvalRequestID, msg, 10)
}

func (s *service) sendApprovalOutcome(ctx context.Context, approvalRequestID string, channelID string, requestMessageID string, status string) {
	err := s.sendMintPairingNotification(ctx, relayer.NOTIFICATION_TYPE_MINT_PAIRING_APPROVAL_OUTCOME, approvalRequestID, map[string]any{
		"v":                 1,
		"approvalRequestID": approvalRequestID,
		"channelID":         channelID,
		"requestMessageID":  requestMessageID,
		"status":            status,
		"completedAt":       time.Now().UTC().Format(time.RFC3339),
	}, 10)
	if err != nil {
		s.logger.Warn("Failed to send mint pairing approval outcome", zap.Error(err), zap.String("approvalRequestID", approvalRequestID))
	}
}

func (s *service) sendMintPairingNotification(ctx context.Context, notificationType relayer.NotificationType, messageID string, message any, persistRecordCount int) error {
	if s.relayer == nil {
		return nil
	}
	return s.relayer.Send(ctx, relayer.Response{
		Type:               "notification",
		MessageID:          messageID,
		NotificationType:   string(notificationType),
		PersistRecordCount: persistRecordCount,
		Message:            message,
	})
}

func (s *service) registerPending(p *pendingApproval) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pending[p.approvalRequestID] = p
}

func (s *service) currentActive() *activePairing {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active == nil {
		return nil
	}
	if !s.active.expiresAt.IsZero() && time.Now().After(s.active.expiresAt) {
		s.active.cancel()
		s.active = nil
		return nil
	}
	return s.active
}

func (s *service) showPairingCode(ctx context.Context, active *activePairing) error {
	s.displayMu.Lock()
	defer s.displayMu.Unlock()

	if err := qrdisplay.ShowPairingCode(ctx, s.cdp, active.pairingCode); err != nil {
		return err
	}

	s.mu.Lock()
	s.displayGeneration++
	active.displayGen = s.displayGeneration
	s.displayOwner = active
	s.mu.Unlock()
	return nil
}

func (s *service) releaseDisplayOwnership(active *activePairing) (uint64, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active == active {
		s.active = nil
	}
	if s.displayOwner != active {
		return active.displayGen, false
	}
	s.displayOwner = nil
	return active.displayGen, true
}

func (s *service) closeChannel(channel brokerChannel) {
	closeCtx, cancel := context.WithTimeout(context.Background(), channelCloseTimeout)
	defer cancel()
	if err := channel.Close(closeCtx); err != nil {
		s.logger.Warn("Failed to close mint pairing channel", zap.Error(err))
	}
}

func (s *service) restoreDefaultDisplay(channelID string, displayGeneration uint64) {
	s.displayMu.Lock()
	defer s.displayMu.Unlock()

	// The ownership check must happen at send time. Cleanup runs in a detached
	// goroutine, so a newer session can claim the overlay after the old session
	// releases it but before the hidden command reaches Chromium.
	s.mu.Lock()
	stale := s.displayGeneration != displayGeneration || s.displayOwner != nil
	s.mu.Unlock()
	if stale {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), displayRecoveryTimeout)
	defer cancel()
	if err := qrdisplay.ShowDefaultDisplay(ctx, s.cdp); err != nil {
		s.logger.Warn("Failed to restore default display after mint pairing", zap.Error(err), zap.String("channelID", channelID))
	}
}

func (s *service) unregisterPending(approvalRequestID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	pending := s.pending[approvalRequestID]
	delete(s.pending, approvalRequestID)
	if pending != nil && pending.accepted != nil {
		retention := s.opts.ApprovalTimeout
		if retention <= 0 {
			retention = defaultApprovalTimeout
		}
		s.doneMap[approvalRequestID] = completedApproval{
			accepted:  *pending.accepted,
			expiresAt: time.Now().Add(retention),
		}
	}
	s.pruneCompletedLocked()
}

func (s *service) pruneCompletedLocked() {
	now := time.Now()
	for id, completed := range s.doneMap {
		if now.After(completed.expiresAt) {
			delete(s.doneMap, id)
		}
	}
}

func approvalError(approvalRequestID string, code string, message string, retryable bool) approvalResponse {
	return approvalResponse{
		OK:                false,
		ApprovalRequestID: approvalRequestID,
		Error: &approvalRPCError{
			Code:      code,
			Message:   message,
			Retryable: retryable,
		},
	}
}

func commandError(code string, message string, retryable bool) map[string]any {
	return map[string]any{
		"ok": false,
		"error": map[string]any{
			"code":      code,
			"message":   message,
			"retryable": retryable,
		},
	}
}

func sameDecision(a approvalDecisionRequest, b approvalDecisionRequest) bool {
	return a.ApprovalRequestID == b.ApprovalRequestID &&
		a.TopicID == b.TopicID &&
		a.ChannelID == b.ChannelID &&
		a.RequestMessageID == b.RequestMessageID &&
		a.Decision == b.Decision
}

func sleepContext(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func maxInt64(a int64, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func newApprovalRequestID() (string, error) {
	b := make([]byte, maxApprovalRequestIDBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "mpa_" + base64.RawURLEncoding.EncodeToString(b), nil
}

func fingerprintPublicJWK(key minter.PublicJWK) string {
	raw := key.KeyType + "|" + key.Curve + "|" + key.X + "|" + key.Y
	sum := sha256.Sum256([]byte(raw))
	return "sha256-" + base64.RawURLEncoding.EncodeToString(sum[:])
}

func formatOptionalTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

type RelayerSessionCreator struct {
	baseURL    string
	apiKey     string
	httpClient wrapper.HTTPClient
	json       wrapper.JSON
}

func NewRelayerSessionCreator(baseURL string, apiKey string, httpClient wrapper.HTTPClient, json wrapper.JSON) *RelayerSessionCreator {
	if json == nil {
		json = wrapper.NewJSON()
	}
	return &RelayerSessionCreator{
		baseURL:    strings.TrimRight(baseURL, "/"),
		apiKey:     apiKey,
		httpClient: httpClient,
		json:       json,
	}
}

func (c *RelayerSessionCreator) CreateEphemeralSession(ctx context.Context, topicID string, request minter.MintRequest) (minter.MintResult, error) {
	if c.httpClient == nil {
		return minter.MintResult{}, errors.New("http client is required")
	}
	if strings.TrimSpace(c.baseURL) == "" {
		return minter.MintResult{}, errors.New("relayer base URL is required")
	}
	endpoint, err := url.Parse(c.baseURL + "/api/ephemeral-sessions")
	if err != nil {
		return minter.MintResult{}, fmt.Errorf("parse relayer session URL: %w", err)
	}
	q := endpoint.Query()
	q.Set("topicID", topicID)
	endpoint.RawQuery = q.Encode()

	body := map[string]any{
		"browserName":      request.BrowserInfo.Name,
		"browserUserAgent": request.BrowserInfo.UserAgent,
		"label":            request.BrowserInfo.Label,
		"expiresInSeconds": effectiveSessionTTLSeconds(request.RequestedExpiresInSeconds),
	}
	raw, err := c.json.Marshal(body)
	if err != nil {
		return minter.MintResult{}, fmt.Errorf("marshal relayer session request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(raw))
	if err != nil {
		return minter.MintResult{}, fmt.Errorf("build relayer session request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "feral-controld")
	if apiKey := strings.TrimSpace(c.apiKey); apiKey != "" {
		req.Header.Set("API-KEY", apiKey)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return minter.MintResult{}, fmt.Errorf("post relayer session request: %w", err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return minter.MintResult{}, fmt.Errorf("relayer session request failed with status %d", resp.StatusCode)
	}

	var decoded struct {
		Session struct {
			ID        string    `json:"id"`
			ExpiresAt time.Time `json:"expiresAt"`
		} `json:"session"`
		Token string `json:"token"`
	}
	if err := c.json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		return minter.MintResult{}, fmt.Errorf("decode relayer session response: %w", err)
	}
	if decoded.Session.ID == "" || decoded.Token == "" {
		return minter.MintResult{}, errors.New("relayer session response missing session id or token")
	}
	return minter.MintResult{
		SessionID:      decoded.Session.ID,
		Token:          decoded.Token,
		ExpiresAt:      decoded.Session.ExpiresAt,
		RelayerBaseURL: c.baseURL,
	}, nil
}

func effectiveSessionTTLSeconds(requested int) int {
	if requested <= 0 {
		return defaultSessionTTLSeconds
	}
	if requested < minSessionTTLSeconds {
		return minSessionTTLSeconds
	}
	if requested > maxSessionTTLSeconds {
		return maxSessionTTLSeconds
	}
	return requested
}

func relayerHTTPBaseString(endpoint string) string {
	u, err := url.Parse(strings.TrimSpace(endpoint))
	if err != nil {
		return ""
	}
	switch u.Scheme {
	case "wss":
		u.Scheme = "https"
		u.Path = ""
		u.RawPath = ""
	case "ws":
		u.Scheme = "http"
		u.Path = ""
		u.RawPath = ""
	}
	u.RawQuery = ""
	u.Fragment = ""
	return strings.TrimRight(u.String(), "/")
}
