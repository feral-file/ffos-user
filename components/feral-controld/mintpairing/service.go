package mintpairing

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	minter "github.com/feral-file/ff-art-computer-handoff/clients/ephemeral-token-minter/go"
	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-controld/cdp"
	"github.com/feral-file/ffos-user/components/feral-controld/config"
	"github.com/feral-file/ffos-user/components/feral-controld/playersession"
	"github.com/feral-file/ffos-user/components/feral-controld/qrdisplay"
	"github.com/feral-file/ffos-user/components/feral-controld/relayer"
	"github.com/feral-file/ffos-user/components/feral-controld/state"
	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

const (
	defaultIdleTTL           = 5 * time.Minute
	defaultPollInterval      = 500 * time.Millisecond
	defaultApprovalTimeout   = 5 * time.Minute
	minSessionTTLSeconds     = 90
	defaultSessionTTLSeconds = 3600
	maxSessionTTLSeconds     = 86400
	terminalOperationTimeout = 750 * time.Millisecond
	sessionRevokeTimeout     = 1500 * time.Millisecond
	// maxSessionListBytes bounds the relayer's session list. The cap is far
	// above a topic's session cap and exists so a wrong or hostile response
	// cannot be read into memory unbounded.
	maxSessionListBytes       = 1 << 20
	displayRecoveryTimeout    = 500 * time.Millisecond
	stopCleanupTimeout        = 1500 * time.Millisecond
	channelCloseTimeout       = 500 * time.Millisecond
	maxApprovalRequestIDBytes = 16
	defaultPlayerContractPath = "/opt/feral/feral-player/ffos-player-contract.json"

	approvalCancellationStatus = "cancelled" //nolint:misspell // Wire protocol status is documented with this spelling.
)

type Options struct {
	Enabled            bool
	BrokerBaseURL      string
	IdleTTL            time.Duration
	PollInterval       time.Duration
	ApprovalTimeout    time.Duration
	RelayerBaseURL     string
	PlayerContractPath string
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
	if opts.Enabled {
		opts.PlayerContractPath = defaultPlayerContractPath
	}
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
	HandleClosePairingSession(ctx context.Context, args map[string]any) (any, error)
	HandleApprovalDecision(ctx context.Context, args map[string]any) (any, error)
	// DisplayActive reports whether THIS process currently owns the player
	// overlay with a live mint-pairing display (pairing code or
	// request-received, painted by showPairingCode/showRequestReceived and
	// cleared by releaseDisplayOwnership) — the overlay-owner probe for
	// playersession.Session.RegisterOverlayOwner, so a recovery navigation
	// never erases a QR mid-pairing.
	DisplayActive() bool
	// SetSession wires the playersession.Session the display sends park
	// against while a recovery navigation is pending, mirroring
	// setupui.Service.SetSession's discipline. Call once at wiring time,
	// before Start; nil (never called) preserves pre-session behavior
	// exactly — the sends never park.
	SetSession(session NavigationSession)
	// RevokeTopicSessions revokes every browser session the relayer holds for
	// topicID and reports how many it revoked. Factory reset calls it before
	// the claim is cleared: a session minted under the old topic otherwise
	// outlives the claim it belongs to, and nothing on the re-claimed device
	// can reach it afterwards. Best effort by contract — the error says what
	// could not be revoked, and the caller decides whether that stops it.
	RevokeTopicSessions(ctx context.Context, topicID string) (int, error)
}

// NavigationSession is the narrow slice of playersession.Session the display
// sends consult to avoid racing a recovery navigation — consumer-owned,
// mirroring setupui.NavigationSession (and CDPSender's) idiom.
// *playersession.Session satisfies it.
type NavigationSession interface {
	NavigationPending() bool
	StageReady(st playersession.Stage) bool
	Generation() uint64
	// NavigationTargetGeneration reports the generation ID the in-flight
	// navigation bumped to, or 0 when no navigation is in flight past its
	// own bump. See playersession.Session.NavigationTargetGeneration's doc.
	NavigationTargetGeneration() uint64
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

	// session, when set (SetSession), is the playersession.Session the
	// display sends park against while a recovery navigation is pending
	// — same generation-snapshot park discipline setupui.Service gets
	// (see parkForNavigation). Nil in every existing test and any wiring
	// that predates the session — the sends then never park, preserving
	// pre-session behavior exactly. Immutable after construction.
	session NavigationSession
	// navigationParkPollInterval / navigationParkTimeout override the park
	// bounds (zero means the defaults); test-only, same pre-first-use
	// contract as session above.
	navigationParkPollInterval time.Duration
	navigationParkTimeout      time.Duration
}

type brokerStarter interface {
	StartChannel(ctx context.Context, opts minter.StartChannelOptions) (brokerChannel, error)
}

type sessionCreator interface {
	CreateEphemeralSession(ctx context.Context, topicID string, request minter.MintRequest, lifetime sessionLifetime) (minter.MintResult, error)
	RevokeEphemeralSession(ctx context.Context, topicID string, sessionID string) error
	ListEphemeralSessionIDs(ctx context.Context, topicID string) ([]string, error)
}

// sessionLifetime is what the device asks the relayer to mint for one approved
// request. It is decided from the owner's decision and the requester's declared
// capability together — never from the decision alone.
type sessionLifetime int

const (
	// lifetimeTimed: the ordinary session, under the controld-owned TTL policy
	// applied to the browser's requested lifetime.
	lifetimeTimed sessionLifetime = iota
	// lifetimePersistent: the owner asked to keep the site paired and the
	// requester declared it can hold a session with no expiry.
	lifetimePersistent
	// lifetimeTimedFallbackRequester: the owner asked to keep the site paired
	// but the requester never declared the capability — every client released
	// before owner-kept sessions existed requires a real expiresAt and cannot
	// parse a session without one. Handing it a persistent session would break
	// the page outright, so it gets the longest timed session instead and the
	// owner has to re-approve when it lapses.
	lifetimeTimedFallbackRequester
)

// deliveredOutcomeLifetime names the shape for the controller's approval
// outcome. It reads the session the browser actually received, never the one
// that was requested: a persistent ask answered by a relayer with a timed
// session is a timed session, and saying otherwise would have the app tell the
// owner a site is kept when it expires. requested only distinguishes WHY a
// timed session is timed — the capability fallback names itself so the app can
// correct copy it already showed.
func deliveredOutcomeLifetime(requested sessionLifetime, session minter.MintResult) string {
	if session.Persistent {
		return "persistent"
	}
	if requested == lifetimeTimedFallbackRequester {
		return "timed_fallback_requester"
	}
	return "timed"
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

// SendMintSuccess hands the created session to the minter client, which
// encrypts it for the browser: `persistent: true` with a null `expiresAt` for
// an owner-kept session, an ordinary expiry for a timed one. The client
// refuses a non-persistent result with no expiry, so the browser can never be
// handed a session whose deadline is missing or invented.
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
	// guard is the claim this pairing began under: the topic id AND the
	// generation. Keeping only the id would let a pairing survive its own
	// claim — clear the topic and re-claim onto the same id (a factory reset
	// and re-pair) and every id comparison still passes, so the approval the
	// previous owner started would mint a session under the new owner's claim.
	guard            topicGuard
	channelID        string
	requestMessageID string
	browserName      string
	expiresAt        time.Time
	decisionCh       chan approvalDecisionRequest
	accepted         *approvalDecisionRequest
}

type activePairing struct {
	channel     brokerChannel
	channelID   string
	pairingCode string
	expiresAt   time.Time
	phase       activePairingPhase
	browserName string
	displayGen  uint64
	cancel      context.CancelFunc
	done        chan struct{}
}

type completedApproval struct {
	accepted  approvalDecisionRequest
	expiresAt time.Time
}

type activePairingPhase string

const (
	activePairingPhasePairingCode     activePairingPhase = "pairing_code"
	activePairingPhasePendingApproval activePairingPhase = "pending_approval"
)

type startPairingResponse struct {
	OK          bool   `json:"ok"`
	Status      string `json:"status"`
	ChannelID   string `json:"channelID"`
	PairingCode string `json:"pairingCode,omitempty"`
	ExpiresAt   string `json:"expiresAt,omitempty"`
}

type closePairingResponse struct {
	OK        bool   `json:"ok"`
	Status    string `json:"status"`
	ChannelID string `json:"channelID,omitempty"`
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

	// KeepPairedRaw holds the wire value so an absent flag stays
	// distinguishable from an explicit null: decoding straight into a bool
	// turns `"keepPaired": null` into a silent false, which reads as "the
	// owner chose not to keep this site" when the controller in fact sent
	// something malformed. parseDecision resolves it into KeepPaired.
	KeepPairedRaw json.RawMessage `json:"keepPaired,omitempty"`
	KeepPaired    bool            `json:"-"`
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

// defaultNavigationParkPollInterval / defaultNavigationParkTimeout bound the
// display-send park while a playersession.Session recovery navigation is
// pending — same values and rationale as setupui's identical constants.
const (
	defaultNavigationParkPollInterval = 100 * time.Millisecond
	defaultNavigationParkTimeout      = 15 * time.Second
)

// SetSession wires the session the display sends park against. See the
// Service interface doc.
func (s *service) SetSession(session NavigationSession) {
	s.session = session
}

// parkForNavigation blocks the caller while a playersession.Session recovery
// navigation is pending, so a display send cannot race the page
// underneath it — the same discipline setupui.Service.parkForNavigation
// applies to narration sends. No-op when no session is wired (SetSession
// never called), which is every existing test and any pre-session build.
//
// Unlike setupui, this is NOT run on a dedicated queue-draining worker: the
// three call sites (showPairingCode, showRequestReceived,
// restoreDefaultDisplay) already run on their own goroutines (the broker
// session goroutine, or restoreDefaultDisplay's own `go`), so parking here
// blocks only that goroutine, not a shared queue. It is called BEFORE
// displayMu is taken (never while holding it): parking can take up to the
// full timeout, and holding displayMu across that would serialize every
// OTHER display mutation behind an unrelated navigation.
//
// The park exits on whichever comes FIRST: the navigation's TARGET
// generation reaching StageHandler (see NavigationTargetGeneration and
// setupui's parkForNavigation for the full rationale — a
// Generation()-snapshot-at-entry comparison stalls for the full timeout when
// the park is entered AFTER the bump already happened, which is common
// here); NavigationPending clearing; or the bounded park timeout — on the
// latter two the send still proceeds best-effort right after this returns.
func (s *service) parkForNavigation() {
	if s.session == nil || !s.session.NavigationPending() {
		return
	}
	interval := s.navigationParkPollInterval
	if interval <= 0 {
		interval = defaultNavigationParkPollInterval
	}
	timeout := s.navigationParkTimeout
	if timeout <= 0 {
		timeout = defaultNavigationParkTimeout
	}
	deadline := time.Now().Add(timeout)
	for s.session.NavigationPending() {
		if target := s.session.NavigationTargetGeneration(); target != 0 && target == s.session.Generation() && s.session.StageReady(playersession.StageHandler) {
			return
		}
		if time.Now().After(deadline) {
			s.logger.Info("Mint pairing display park timed out waiting on a pending navigation; delivering best-effort")
			return
		}
		time.Sleep(interval)
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
	if err := validatePlayerContractFile(s.opts.PlayerContractPath); err != nil {
		s.logger.Warn("Mint pairing player contract validation failed", zap.Error(err), zap.String("path", s.opts.PlayerContractPath))
		// A read FAILURE (unreadable — boot ordering, an OTA mid-replace of
		// the player bundle) is transient: report it retryable, not the same
		// permanent invalid_config a manifest that WAS read but genuinely
		// lacks mint pairing support gets. See ErrPlayerContractUnreadable.
		if errors.Is(err, ErrPlayerContractUnreadable) {
			return commandError("player_contract_unreadable", "mint pairing player contract is not readable yet", true), nil
		}
		return commandError("invalid_config", "mint pairing player contract is not valid", false), nil
	}
	startGuard := currentTopicGuard()
	if startGuard.topicID == "" {
		return commandError("topic_not_ready", "relayer topic is not ready", true), nil
	}

	s.startMu.Lock()
	defer s.startMu.Unlock()

	if active, phase, browserName := s.currentActive(); active != nil {
		if phase == activePairingPhasePendingApproval {
			if err := s.showRequestReceived(ctx, active, browserName); err != nil {
				s.logger.Warn("Failed to redisplay active mint pairing request status", zap.Error(err), zap.String("channelID", active.channelID))
				return commandError("display_unavailable", "failed to display mint pairing request status", true), nil
			}
			return startPairingResponse{
				OK:        true,
				Status:    "pending_approval",
				ChannelID: active.channelID,
				ExpiresAt: formatOptionalTime(active.expiresAt),
			}, nil
		}
		if err := s.showPairingCode(ctx, active); err != nil {
			s.logger.Warn("Failed to redisplay active mint pairing code", append(pairingDisplayLogFields(active.channelID, active.pairingCode, active.expiresAt), zap.Error(err))...)
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
	s.logger.Info(
		"Starting mint pairing broker channel",
		zap.String("brokerBaseURL", s.opts.BrokerBaseURL),
		zap.Duration("idleTTL", s.opts.IdleTTL),
		zap.Bool("shortCodeRequested", true),
	)
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
		s.logger.Warn("Mint pairing broker response missing pairing code", zap.String("channelID", display.ChannelID), zap.Time("expiresAt", display.ExpiresAt))
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
		phase:       activePairingPhasePairingCode,
		cancel:      sessionCancel,
		done:        make(chan struct{}),
	}
	s.logger.Info("Mint pairing broker channel started", pairingDisplayLogFields(active.channelID, active.pairingCode, active.expiresAt)...)

	if err := s.showPairingCode(ctx, active); err != nil {
		sessionCancel()
		s.closeChannel(channel)
		s.logger.Warn("Failed to display mint pairing QR code; closed broker channel", append(pairingDisplayLogFields(active.channelID, active.pairingCode, active.expiresAt), zap.Error(err))...)
		return commandError("display_unavailable", "failed to display mint pairing QR code", true), nil
	}
	s.logger.Info("Displayed mint pairing code", pairingDisplayLogFields(active.channelID, active.pairingCode, active.expiresAt)...)

	s.mu.Lock()
	s.active = active
	s.mu.Unlock()

	// The broker approval session must outlive the initiating RPC. sessionCtx is
	// still bounded by service shutdown and the broker pairing expiry.
	go s.waitForBrowserAndApproval(sessionCtx, active, startGuard) //nolint:gosec

	return startPairingResponse{
		OK:          true,
		Status:      "started",
		ChannelID:   active.channelID,
		PairingCode: active.pairingCode,
		ExpiresAt:   formatOptionalTime(active.expiresAt),
	}, nil
}

func (s *service) HandleClosePairingSession(context.Context, map[string]any) (any, error) {
	if s == nil || !s.opts.Enabled {
		return commandError("disabled", "mint pairing is not enabled", false), nil
	}
	active := s.cancelActivePairing()
	if active == nil {
		return closePairingResponse{OK: true, Status: "not_started"}, nil
	}
	s.logger.Info("Closing active mint pairing session by request", pairingDisplayLogFields(active.channelID, active.pairingCode, active.expiresAt)...)
	return closePairingResponse{OK: true, Status: "closed", ChannelID: active.channelID}, nil
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
	// The decision must name the pairing's topic, and that pairing's claim must
	// still be the live one. A claim cleared and re-taken onto the same topic
	// id is a different pairing: the id matches, the generation does not.
	if decision.TopicID != pending.guard.topicID || !pending.guard.sameAs(currentTopicGuard()) {
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
	if err := s.resolveKeepPaired(&decision); err != nil {
		return decision, err
	}
	switch decision.Decision {
	case "approve":
	case "reject":
		// keepPaired only means anything for an approval: there is no session
		// to keep behind a rejection. Normalizing it here also keeps decision
		// replay comparison honest — a rejection replayed with the flag set is
		// the same decision, not a conflicting one.
		decision.KeepPaired = false
		if strings.TrimSpace(decision.Reason) == "" {
			decision.Reason = "rejected_by_user"
		}
	default:
		return decision, errors.New("decision must be approve or reject")
	}
	return decision, nil
}

// resolveKeepPaired reads the wire value into KeepPaired. Absent is false;
// only a literal true or false is accepted, so a null or a "true" string is a
// malformed decision rather than a silent no.
func (s *service) resolveKeepPaired(decision *approvalDecisionRequest) error {
	raw := bytes.TrimSpace(decision.KeepPairedRaw)
	if len(raw) == 0 {
		decision.KeepPaired = false
		return nil
	}
	// A JSON null unmarshals into a bool without error, leaving it false — the
	// exact silent "the owner did not ask to keep this site" this check exists
	// to prevent — so it is rejected by value before the decode.
	if bytes.Equal(raw, []byte("null")) {
		return errors.New("keepPaired must be true or false")
	}
	var keepPaired bool
	if err := s.json.Unmarshal(raw, &keepPaired); err != nil {
		return errors.New("keepPaired must be true or false")
	}
	decision.KeepPaired = keepPaired
	return nil
}

func (s *service) waitForBrowserAndApproval(ctx context.Context, active *activePairing, guard topicGuard) {
	terminalSent := false
	refreshAfterClose := false
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
		if refreshAfterClose {
			go s.refreshExpiredPairingCode()
		}
	}()

	request, err := s.waitForMintRequest(ctx, active.channel)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) && s.shouldRefreshExpiredPairing(active) {
			refreshAfterClose = true
			s.logger.Info("Mint pairing code expired; refreshing broker channel", pairingDisplayLogFields(active.channelID, active.pairingCode, active.expiresAt)...)
			return
		}
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
		guard:             guard,
		channelID:         request.ChannelID,
		requestMessageID:  request.MessageID,
		browserName:       browserDisplayName(request.BrowserInfo),
		expiresAt:         expiresAt,
		decisionCh:        make(chan approvalDecisionRequest, 1),
	}
	s.registerPending(pending)
	defer s.unregisterPending(approvalRequestID)
	s.setActivePendingApproval(active, pending.browserName)

	if err := s.showRequestReceived(ctx, active, pending.browserName); err != nil {
		s.logger.Warn("Failed to display mint pairing request status", zap.Error(err), zap.String("channelID", active.channelID))
	}

	if err := s.sendApprovalRequest(ctx, approvalRequestID, guard.topicID, *request, active.channel.MinterPublicKeyJWK(), expiresAt); err != nil {
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
				terminalSent, err = s.completeDecisionWithBoundedContexts(context.Background(), active.channel, *request, guard, approvalRequestID, decision)
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
			terminalSent, err = s.completeDecisionWithBoundedContexts(context.Background(), active.channel, *request, guard, approvalRequestID, decision)
			if err != nil {
				s.logger.Warn("Failed to complete mint pairing decision", zap.Error(err), zap.String("channelID", active.channelID))
			}
			return
		}
		terminalSent = s.sendApprovalExpired(active, *request, approvalRequestID)
	case decision := <-pending.decisionCh:
		terminalSent, err = s.completeDecisionWithBoundedContexts(context.Background(), active.channel, *request, guard, approvalRequestID, decision)
		if err != nil {
			s.logger.Warn("Failed to complete mint pairing decision", zap.Error(err), zap.String("channelID", active.channelID))
		}
	}
}

func (s *service) sendApprovalCancelled(active *activePairing, request minter.MintRequest, approvalRequestID string) bool {
	terminalCtx, cancel := context.WithTimeout(context.Background(), terminalOperationTimeout)
	defer cancel()
	_, err := active.channel.SendMintRejection(terminalCtx, request, minter.MintRejection{Reason: approvalCancellationStatus, Retryable: true})
	s.sendApprovalOutcome(terminalCtx, approvalRequestID, request.ChannelID, request.MessageID, approvalCancellationStatus, "")
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
	s.sendApprovalOutcome(terminalCtx, approvalRequestID, request.ChannelID, request.MessageID, "expired", "")
	if err != nil {
		s.logger.Warn("Failed to send mint pairing expiration to browser", zap.Error(err), zap.String("channelID", active.channelID))
		return false
	}
	return true
}

func (s *service) completeDecisionWithBoundedContexts(parentCtx context.Context, channel brokerChannel, request minter.MintRequest, guard topicGuard, approvalRequestID string, decision approvalDecisionRequest) (bool, error) {
	return s.completeDecision(parentCtx, channel, request, guard, approvalRequestID, decision)
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

// completeDecision carries the guard the pairing began under, not just its
// topic id: every check below asks "is this still the same claim?", which a
// topic id alone cannot answer once a topic can be cleared and re-issued.
func (s *service) completeDecision(ctx context.Context, channel brokerChannel, request minter.MintRequest, guard topicGuard, approvalRequestID string, decision approvalDecisionRequest) (bool, error) {
	topicID := guard.topicID
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

	if !guard.sameAs(currentTopicGuard()) {
		return s.rejectTopicChanged(channel, request, topicID, approvalRequestID)
	}

	lifetime := s.sessionLifetimeFor(decision, request)

	sessionCtx, cancelSession := context.WithTimeout(ctx, wrapper.HTTPClientTimeout)
	session, err := s.sessionCreator.CreateEphemeralSession(sessionCtx, topicID, request, lifetime)
	cancelSession()
	if err != nil {
		sendErr := s.sendTerminalRejectionAndOutcome(channel, request, approvalRequestID, "session_create_failed", true, "failed")
		if sendErr != nil {
			return false, fmt.Errorf("create session and browser rejection failed: %w", errors.Join(err, sendErr))
		}
		return true, fmt.Errorf("create session: %w", err)
	}
	// Still the same claim the pairing began under? The session was minted for
	// it, and the answer is re-checked once more after delivery, because this
	// check and the send cannot be one atomic step.
	if !guard.sameAs(currentTopicGuard()) {
		// The session exists on the relayer but no browser will ever hold it.
		// Revoke after the terminal message so cleanup never delays what the
		// browser is told.
		terminalSent, topicErr := s.rejectTopicChanged(channel, request, topicID, approvalRequestID)
		s.revokeAbandonedSession(topicID, session.SessionID)
		return terminalSent, topicErr
	}
	if session.RelayerBaseURL == "" {
		session.RelayerBaseURL = s.opts.RelayerBaseURL
	}
	successCtx, cancelSuccess := context.WithTimeout(context.Background(), wrapper.HTTPClientTimeout)
	_, err = channel.SendMintSuccess(successCtx, request, session)
	cancelSuccess()
	if err != nil {
		outcomeCtx, cancelOutcome := context.WithTimeout(context.Background(), terminalOperationTimeout)
		s.sendApprovalOutcome(outcomeCtx, approvalRequestID, request.ChannelID, request.MessageID, "failed", "")
		cancelOutcome()
		// A failed send is not proof of non-delivery. Revoke only when the
		// broker refused the message outright; otherwise the browser may
		// already hold this session and revoking would cut off a site the
		// owner approved.
		if classifyDeliveryFailure(err) == deliveryNeverSent {
			s.revokeAbandonedSession(topicID, session.SessionID)
		} else {
			s.logger.Warn("Left a possibly delivered mint pairing session in place after a failed browser delivery",
				zap.Error(err),
				zap.String("topicID", topicID),
				zap.String("sessionID", session.SessionID),
				zap.Bool("persistent", session.Persistent))
		}
		return false, fmt.Errorf("send mint success: %w", err)
	}
	// The topic can move between the last check and the send. Re-read it
	// synchronously: if the topic this session was minted for is gone, the
	// session is dead on the relayer's side of the pairing no matter who holds
	// the token, so revoking it cannot cut off a valid pairing. The browser's
	// terminal message is already delivered and is not taken back.
	if after := currentTopicGuard(); !after.sameAs(guard) {
		s.logger.Warn("Relayer topic changed while delivering a mint pairing session; revoking the session it was minted for",
			zap.String("mintedForTopicID", topicID),
			zap.String("currentTopicID", after.topicID),
			zap.String("sessionID", session.SessionID),
			zap.Bool("persistent", session.Persistent))
		s.revokeAbandonedSession(topicID, session.SessionID)
	}

	outcomeCtx, cancelOutcome := context.WithTimeout(context.Background(), terminalOperationTimeout)
	s.sendApprovalOutcome(outcomeCtx, approvalRequestID, request.ChannelID, request.MessageID, "completed", deliveredOutcomeLifetime(lifetime, session))
	cancelOutcome()
	return true, nil
}

// sessionLifetimeFor reads the owner's decision against what the requester can
// actually hold. A keepPaired approval for a requester that never declared
// support is not an error and not a rejection: the owner still approved the
// site, so it gets the longest timed session the policy allows.
func (s *service) sessionLifetimeFor(decision approvalDecisionRequest, request minter.MintRequest) sessionLifetime {
	if !decision.KeepPaired {
		return lifetimeTimed
	}
	if request.SupportsPersistentSessions {
		return lifetimePersistent
	}
	s.logger.Warn("Owner asked to keep this site paired, but the requester cannot hold a session without an expiry; minting the longest timed session instead",
		zap.String("origin", request.Origin),
		zap.String("channelID", request.ChannelID),
		zap.Int("expiresInSeconds", maxSessionTTLSeconds))
	return lifetimeTimedFallbackRequester
}

// deliveryVerdict says what a failed browser delivery proves about whether the
// browser could be holding the session.
type deliveryVerdict int

const (
	// deliveryUnknown: the message may have reached the browser. A timeout, a
	// transport error, or a broker 5xx all leave the send in doubt, so the
	// session must be left alone.
	deliveryUnknown deliveryVerdict = iota
	// deliveryNeverSent: the broker refused the message before accepting it —
	// the channel is gone, closed, or the request was rejected outright — so
	// nothing was delivered.
	deliveryNeverSent
)

// brokerStatusPattern reads the broker status out of a minter-client error.
// The client returns unstructured errors, so its formatted status line is the
// only positive evidence available; a typed error upstream would make this
// structural.
var brokerStatusPattern = regexp.MustCompile(`failed with status (\d{3})`)

// classifyDeliveryFailure looks for proof that a failed SendMintSuccess never
// reached the browser. It is deliberately one-sided: only a broker client-error
// status counts as proof, and everything it does not positively recognize is
// deliveryUnknown. Revoking a session the browser already holds silently cuts
// off a site the owner approved, while leaving one behind costs at most a slot
// the owner can clear from the app's paired-sites screen — and a timed session
// expires on its own.
func classifyDeliveryFailure(err error) deliveryVerdict {
	if err == nil {
		return deliveryUnknown
	}
	match := brokerStatusPattern.FindStringSubmatch(err.Error())
	if match == nil {
		return deliveryUnknown
	}
	status, convErr := strconv.Atoi(match[1])
	if convErr != nil {
		return deliveryUnknown
	}
	if status >= 400 && status < 500 {
		return deliveryNeverSent
	}
	return deliveryUnknown
}

// RevokeTopicSessions revokes every session the relayer holds for topicID.
// See Service. It keeps going after a failure so one unreachable session
// cannot strand the rest, and reports the count it did revoke alongside the
// joined failures.
func (s *service) RevokeTopicSessions(ctx context.Context, topicID string) (int, error) {
	if s == nil || s.sessionCreator == nil {
		return 0, nil
	}
	if strings.TrimSpace(topicID) == "" {
		return 0, nil
	}
	sessionIDs, err := s.sessionCreator.ListEphemeralSessionIDs(ctx, topicID)
	if err != nil {
		return 0, fmt.Errorf("list relayer sessions: %w", err)
	}
	revoked := 0
	var failures []error
	for _, sessionID := range sessionIDs {
		if revokeErr := s.sessionCreator.RevokeEphemeralSession(ctx, topicID, sessionID); revokeErr != nil {
			failures = append(failures, fmt.Errorf("revoke %s: %w", sessionID, revokeErr))
			continue
		}
		revoked++
	}
	return revoked, errors.Join(failures...)
}

// revokeAbandonedSession returns a created session the browser never received.
// Best effort by design: the browser already has its terminal message, so a
// failed revoke is logged and changes nothing it was told. Leaving the session
// behind is not harmless — a persistent one has no TTL to clean it up and
// would hold one of the topic's owner-kept slots for good.
func (s *service) revokeAbandonedSession(topicID string, sessionID string) {
	if s.sessionCreator == nil || strings.TrimSpace(sessionID) == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), sessionRevokeTimeout)
	defer cancel()
	if err := s.sessionCreator.RevokeEphemeralSession(ctx, topicID, sessionID); err != nil {
		s.logger.Warn("Failed to revoke abandoned mint pairing session",
			zap.Error(err),
			zap.String("topicID", topicID),
			zap.String("sessionID", sessionID))
	}
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
	s.sendApprovalOutcome(outcomeCtx, approvalRequestID, request.ChannelID, request.MessageID, outcomeStatus, "")
	cancelOutcome()
	return err
}

func currentRelayerTopicID() string {
	return strings.TrimSpace(state.ClaimSnapshot().TopicID)
}

// topicGuard is a snapshot of which relayer topic this device answers to,
// taken atomically. Comparing two guards catches a topic that was cleared and
// reassigned between them — a factory reset and re-claim onto the same topic
// id reads as unchanged by id alone, but moves the generation.
type topicGuard struct {
	topicID    string
	generation uint64
}

func currentTopicGuard() topicGuard {
	snapshot := state.ClaimSnapshot()
	return topicGuard{
		topicID:    strings.TrimSpace(snapshot.TopicID),
		generation: snapshot.TopicGeneration,
	}
}

func (g topicGuard) sameAs(other topicGuard) bool {
	return g.topicID == other.topicID && g.generation == other.generation
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
		"v":                          1,
		"topicID":                    topicID,
		"approvalRequestID":          approvalRequestID,
		"channelID":                  request.ChannelID,
		"requestMessageID":           request.MessageID,
		"origin":                     request.Origin,
		"browserInfo":                request.BrowserInfo,
		"requestedExpiresInSeconds":  request.RequestedExpiresInSeconds,
		"effectiveExpiresInSeconds":  effectiveSessionTTLSeconds(request.RequestedExpiresInSeconds),
		"supportsPersistentSessions": request.SupportsPersistentSessions,
		"requestedAt":                time.Now().UTC().Format(time.RFC3339),
		"expiresAt":                  expiresAt.UTC().Format(time.RFC3339),
		"challenge": map[string]any{
			"algorithm":                   minter.Algorithm,
			"browserPublicKeyFingerprint": fingerprintPublicJWK(request.BrowserPublicKeyJWK),
			"minterPublicKeyFingerprint":  fingerprintPublicJWK(minterPublicKey),
		},
	}
	return s.sendMintPairingNotification(ctx, relayer.NOTIFICATION_TYPE_MINT_PAIRING_APPROVAL_REQUEST, approvalRequestID, msg, 10)
}

// sendApprovalOutcome reports the terminal state to the controller. lifetime is
// the shape of the session the browser received and is empty for every outcome
// that delivered none.
func (s *service) sendApprovalOutcome(ctx context.Context, approvalRequestID string, channelID string, requestMessageID string, status string, lifetime string) {
	message := map[string]any{
		"v":                 1,
		"approvalRequestID": approvalRequestID,
		"channelID":         channelID,
		"requestMessageID":  requestMessageID,
		"status":            status,
		"completedAt":       time.Now().UTC().Format(time.RFC3339),
	}
	if lifetime != "" {
		message["lifetime"] = lifetime
	}
	err := s.sendMintPairingNotification(ctx, relayer.NOTIFICATION_TYPE_MINT_PAIRING_APPROVAL_OUTCOME, approvalRequestID, message, 10)
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

// DisplayActive reports whether this process currently owns the player
// overlay with a live mint-pairing display. See the Service interface doc.
func (s *service) DisplayActive() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.displayOwner != nil
}

func (s *service) registerPending(p *pendingApproval) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pending[p.approvalRequestID] = p
}

func (s *service) currentActive() (*activePairing, activePairingPhase, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active == nil {
		return nil, "", ""
	}
	if !s.active.expiresAt.IsZero() && time.Now().After(s.active.expiresAt) {
		s.active.cancel()
		s.active = nil
		return nil, "", ""
	}
	return s.active, s.active.phase, s.active.browserName
}

func (s *service) cancelActivePairing() *activePairing {
	s.mu.Lock()
	active := s.active
	if active != nil {
		s.active = nil
		active.cancel()
	}
	s.mu.Unlock()
	return active
}

func (s *service) shouldRefreshExpiredPairing(active *activePairing) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ctx != nil && s.active == active && active.phase == activePairingPhasePairingCode
}

func (s *service) refreshExpiredPairingCode() {
	s.mu.Lock()
	runCtx := s.ctx
	s.mu.Unlock()
	if runCtx == nil || runCtx.Err() != nil {
		return
	}
	result, err := s.HandleStartPairingSession(context.Background(), nil)
	if err != nil {
		s.logger.Warn("Failed to refresh expired mint pairing code", zap.Error(err))
		return
	}
	if response, ok := result.(startPairingResponse); ok && response.OK {
		s.logger.Info("Refreshed expired mint pairing code", pairingDisplayLogFields(response.ChannelID, response.PairingCode, parseOptionalTime(response.ExpiresAt))...)
		return
	}
	s.logger.Warn("Failed to refresh expired mint pairing code")
}

func (s *service) setActivePendingApproval(active *activePairing, browserName string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active != active {
		return
	}
	active.phase = activePairingPhasePendingApproval
	active.browserName = browserName
}

func (s *service) showPairingCode(ctx context.Context, active *activePairing) error {
	// Park BEFORE taking displayMu — parking can take up to the full
	// timeout, and holding displayMu across it would serialize every OTHER
	// display mutation behind an unrelated navigation.
	s.parkForNavigation()
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

func (s *service) showRequestReceived(ctx context.Context, active *activePairing, browserName string) error {
	// See showPairingCode's comment: park before taking displayMu.
	s.parkForNavigation()
	s.displayMu.Lock()
	defer s.displayMu.Unlock()

	if err := qrdisplay.ShowRequestReceived(ctx, s.cdp, browserName); err != nil {
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
	// See showPairingCode's comment: park before taking displayMu.
	s.parkForNavigation()
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

type playerContractManifest struct {
	Contracts map[string]playerMintPairingDisplayContract `json:"contracts"`
}

type playerMintPairingDisplayContract struct {
	Version          int                            `json:"version"`
	RequestKey       string                         `json:"requestKey"`
	States           []string                       `json:"states"`
	AcceptedResponse playerContractAcceptedResponse `json:"acceptedResponse"`
}

type playerContractAcceptedResponse struct {
	OK bool `json:"ok"`
}

// ErrPlayerContractUnreadable marks a validation failure caused by failing to
// READ the player contract manifest, as opposed to a successfully-read
// manifest that lacks (or fails) the mintPairingDisplay contract. The two
// must not be conflated: unreadable is transient (boot ordering, an OTA
// mid-replace of the player bundle) and the caller should ask again; a
// manifest that WAS read but is invalid means the connected player's build
// genuinely does not support mint pairing. Mirrors
// setupui.ErrPlayerContractUnreadable's identical distinction for the same
// on-disk manifest.
var ErrPlayerContractUnreadable = errors.New("player contract unreadable")

func validatePlayerContractFile(path string) error {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil
	}
	raw, err := os.ReadFile(path) //nolint:gosec // Production uses the fixed player contract path; tests inject temp files.
	if err != nil {
		return fmt.Errorf("%w: %w", ErrPlayerContractUnreadable, err)
	}
	var manifest playerContractManifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return fmt.Errorf("decode player contract: %w", err)
	}
	contract, ok := manifest.Contracts["mintPairingDisplay"]
	if !ok {
		return errors.New("missing contracts.mintPairingDisplay")
	}
	if contract.Version != 1 {
		return errors.New("contracts.mintPairingDisplay.version must be 1")
	}
	if contract.RequestKey != "request" {
		return errors.New(`contracts.mintPairingDisplay.requestKey must be "request"`)
	}
	states := make(map[string]bool, len(contract.States))
	for _, state := range contract.States {
		states[state] = true
	}
	for _, required := range []string{"pairing_code", "request_received", "creating_token", "hidden"} {
		if !states[required] {
			return fmt.Errorf("contracts.mintPairingDisplay.states missing %q", required)
		}
	}
	if !contract.AcceptedResponse.OK {
		return errors.New("contracts.mintPairingDisplay.acceptedResponse.ok must be true")
	}
	return nil
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

func pairingDisplayLogFields(channelID string, pairingCode string, expiresAt time.Time) []zap.Field {
	fields := []zap.Field{
		zap.String("channelID", channelID),
		zap.String("pairingCodeEdge", logEdge(pairingCode)),
		zap.Int("pairingCodeLength", len(strings.TrimSpace(pairingCode))),
	}
	if !expiresAt.IsZero() {
		fields = append(fields, zap.Time("expiresAt", expiresAt))
	}
	return fields
}

func logEdge(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if len(value) <= 3 {
		return value
	}
	return value[:3] + "..." + value[len(value)-3:]
}

func sameDecision(a approvalDecisionRequest, b approvalDecisionRequest) bool {
	return a.ApprovalRequestID == b.ApprovalRequestID &&
		a.TopicID == b.TopicID &&
		a.ChannelID == b.ChannelID &&
		a.RequestMessageID == b.RequestMessageID &&
		a.Decision == b.Decision &&
		a.KeepPaired == b.KeepPaired
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

func parseOptionalTime(value string) time.Time {
	if strings.TrimSpace(value) == "" {
		return time.Time{}
	}
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}
	}
	return parsed
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

// CreateEphemeralSession mints one browser session on the relayer. A
// persistent lifetime sends `persistent: true`, omits `expiresInSeconds`
// entirely, and ignores the browser's requestedExpiresInSeconds — the owner's
// choice outranks the site's request. The requester fallback asks for the
// longest timed session instead of a persistent one. An ordinary timed
// lifetime applies the controld-owned TTL policy unchanged.
func (c *RelayerSessionCreator) CreateEphemeralSession(ctx context.Context, topicID string, request minter.MintRequest, lifetime sessionLifetime) (minter.MintResult, error) {
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
	}
	switch lifetime {
	case lifetimePersistent:
		body["persistent"] = true
	case lifetimeTimedFallbackRequester:
		body["expiresInSeconds"] = maxSessionTTLSeconds
	default:
		body["expiresInSeconds"] = effectiveSessionTTLSeconds(request.RequestedExpiresInSeconds)
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
			ID         string     `json:"id"`
			ExpiresAt  *time.Time `json:"expiresAt"`
			Persistent bool       `json:"persistent"`
		} `json:"session"`
		Token string `json:"token"`
	}
	if err := c.json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		return minter.MintResult{}, fmt.Errorf("decode relayer session response: %w", err)
	}
	// An id with no token is still a session the relayer allocated. Split the
	// two checks so the id can be revoked instead of stranded: from here on,
	// every validation failure goes through rejectAndRevoke.
	if decoded.Session.ID == "" {
		return minter.MintResult{}, errors.New("relayer session response missing session id")
	}
	if decoded.Token == "" {
		return minter.MintResult{}, c.rejectAndRevoke(decoded.Session.ID, topicID,
			errors.New("relayer session response missing token"))
	}
	// The relayer's answer decides, not the device's request: a relayer that
	// does not honor `persistent` returns an ordinary expiring session, and the
	// device must deliver it as one rather than promise the browser a session
	// that will quietly die.
	session := minter.MintResult{
		SessionID:      decoded.Session.ID,
		Token:          decoded.Token,
		Persistent:     decoded.Session.Persistent,
		RelayerBaseURL: c.baseURL,
	}
	// The two session shapes stay separable end to end, and a reply that mixes
	// them is malformed either way: an owner-kept session must carry no expiry
	// at all — a present-but-zero `expiresAt` is a deadline the relayer failed
	// to write, not a null one — and a timed session must carry a real expiry.
	// Failing here sends the browser a retryable session-create rejection
	// instead of a session whose deadline is invented, missing, or silently
	// dropped. The relayer already committed the session, so it is revoked
	// before the error returns: nothing the device refuses is left holding a
	// slot in the topic's cap.
	if session.Persistent {
		// A relayer must not grant more than the device asked for. Only a
		// persistent ASK may be answered with a kept session; anything else
		// getting one back is a relayer bug or a swapped reply, and silently
		// accepting it would pair a site forever that the owner never chose to
		// keep. (The reverse — asking persistent and being handed a timed
		// session — is the deliberate fallback above, not this case.)
		if lifetime != lifetimePersistent {
			return minter.MintResult{}, c.rejectAndRevoke(session.SessionID, topicID,
				errors.New("relayer minted a persistent session for a request that did not ask to keep the site paired"))
		}
		if decoded.Session.ExpiresAt != nil {
			return minter.MintResult{}, c.rejectAndRevoke(session.SessionID, topicID,
				errors.New("relayer session response has an expiresAt for a persistent session"))
		}
		return session, nil
	}
	if decoded.Session.ExpiresAt == nil || decoded.Session.ExpiresAt.IsZero() {
		return minter.MintResult{}, c.rejectAndRevoke(session.SessionID, topicID,
			errors.New("relayer session response missing expiresAt for a non-persistent session"))
	}
	session.ExpiresAt = *decoded.Session.ExpiresAt
	return session, nil
}

// rejectAndRevoke revokes a session the device is refusing and returns the
// reason it refused it. A failed revoke is folded into the error rather than
// hidden: the caller's session-create rejection is retryable either way, and
// the leftover session is then visible in the app's paired-sites list.
func (c *RelayerSessionCreator) rejectAndRevoke(sessionID string, topicID string, reason error) error {
	ctx, cancel := context.WithTimeout(context.Background(), sessionRevokeTimeout)
	defer cancel()
	if err := c.RevokeEphemeralSession(ctx, topicID, sessionID); err != nil {
		return fmt.Errorf("%w (revoking the refused session failed: %w)", reason, err)
	}
	return reason
}

// ListEphemeralSessionIDs reads the ids of every session the relayer holds for
// this topic. The list response is the app's paired-sites source, and it is
// read here for the one job the device needs: knowing what to revoke. Both
// wrapper shapes the relayer may return — an object with a sessions array, or
// a bare array — decode, so a shape change on that side cannot silently leave
// sessions behind.
func (c *RelayerSessionCreator) ListEphemeralSessionIDs(ctx context.Context, topicID string) ([]string, error) {
	if c.httpClient == nil {
		return nil, errors.New("http client is required")
	}
	if strings.TrimSpace(c.baseURL) == "" {
		return nil, errors.New("relayer base URL is required")
	}
	endpoint, err := url.Parse(c.baseURL + "/api/ephemeral-sessions")
	if err != nil {
		return nil, fmt.Errorf("parse relayer session list URL: %w", err)
	}
	q := endpoint.Query()
	q.Set("topicID", topicID)
	endpoint.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("build relayer session list request: %w", err)
	}
	req.Header.Set("User-Agent", "feral-controld")
	if apiKey := strings.TrimSpace(c.apiKey); apiKey != "" {
		req.Header.Set("API-KEY", apiKey)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("list relayer sessions: %w", err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("relayer session list failed with status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxSessionListBytes))
	if err != nil {
		return nil, fmt.Errorf("read relayer session list: %w", err)
	}

	type sessionRecord struct {
		ID string `json:"id"`
	}
	var wrapped struct {
		Sessions []sessionRecord `json:"sessions"`
	}
	records := []sessionRecord(nil)
	if err := c.json.Unmarshal(body, &wrapped); err == nil && wrapped.Sessions != nil {
		records = wrapped.Sessions
	} else if err := c.json.Unmarshal(body, &records); err != nil {
		return nil, fmt.Errorf("decode relayer session list: %w", err)
	}

	ids := make([]string, 0, len(records))
	for _, record := range records {
		if id := strings.TrimSpace(record.ID); id != "" {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

// RevokeEphemeralSession deletes one session on the relayer. A session the
// relayer no longer has (404) is already in the state the caller wanted, so it
// is not an error.
func (c *RelayerSessionCreator) RevokeEphemeralSession(ctx context.Context, topicID string, sessionID string) error {
	if c.httpClient == nil {
		return errors.New("http client is required")
	}
	if strings.TrimSpace(c.baseURL) == "" {
		return errors.New("relayer base URL is required")
	}
	if strings.TrimSpace(sessionID) == "" {
		return errors.New("session id is required")
	}
	endpoint, err := url.Parse(c.baseURL + "/api/ephemeral-sessions/" + url.PathEscape(sessionID))
	if err != nil {
		return fmt.Errorf("parse relayer session revoke URL: %w", err)
	}
	q := endpoint.Query()
	q.Set("topicID", topicID)
	endpoint.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, endpoint.String(), nil)
	if err != nil {
		return fmt.Errorf("build relayer session revoke request: %w", err)
	}
	req.Header.Set("User-Agent", "feral-controld")
	if apiKey := strings.TrimSpace(c.apiKey); apiKey != "" {
		req.Header.Set("API-KEY", apiKey)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("delete relayer session: %w", err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()
	if resp.StatusCode == http.StatusNotFound {
		return nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("relayer session revoke failed with status %d", resp.StatusCode)
	}
	return nil
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
