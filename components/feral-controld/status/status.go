package status

import (
	"context"
	//nolint:gosec
	"crypto/md5"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	dp1playlist "github.com/display-protocol/dp1-validator/playlist"

	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-controld/cdp"
	"github.com/feral-file/ffos-user/components/feral-controld/dp1"
	"github.com/feral-file/ffos-user/components/feral-controld/relayer"
	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
	"github.com/feral-file/ffos-user/components/feral-controld/ws"
)

const (
	POLL_INTERVAL = 5 * time.Second
)

type PlayerStatus struct {
	Command        string                      `json:"castCommand,omitempty"`
	PlaylistURL    *string                     `json:"playlistURL,omitempty"`
	Playlist       *dp1.Playlist               `json:"playlist,omitempty"`
	Index          *int                        `json:"index,omitempty"`
	IsPaused       *bool                       `json:"isPaused,omitempty"`
	Items          *[]dp1playlist.PlaylistItem `json:"items,omitempty"`
	Ok             bool                        `json:"ok,omitempty"`
	Error          *string                     `json:"error,omitempty"`
	DeviceSettings struct {
		Scaling string `json:"scaling,omitempty"`
	} `json:"deviceSettings,omitempty"`
}

//go:generate mockgen -source=status.go -destination=../mocks/status.go -package=mocks -mock_names=Poller=MockStatusPoller

type Poller interface {
	Start(ctx context.Context)
	Stop()
	ForceRefresh()
	FetchPlayerStatus(ctx context.Context) (*PlayerStatus, error)
}

// poller handles periodic polling of both player status via CDP and device status
type poller struct {
	sync.RWMutex
	cdp          cdp.CDP
	relayer      relayer.Relayer
	ws           ws.WS
	deviceStatus DeviceStatus
	logger       *zap.Logger
	stopChan     chan struct{}
	refreshChan  chan struct{}
	json         wrapper.JSON

	// Store last status hashes for each notification type to avoid duplicate notifications
	lastStatusHashes map[relayer.NotificationType]string
}

func NewPoller(
	cdp cdp.CDP,
	r relayer.Relayer,
	ws ws.WS,
	ds DeviceStatus,
	json wrapper.JSON,
	logger *zap.Logger,
) Poller {
	return &poller{
		cdp:              cdp,
		relayer:          r,
		ws:               ws,
		deviceStatus:     ds,
		logger:           logger,
		json:             json,
		stopChan:         make(chan struct{}),
		refreshChan:      make(chan struct{}, 10), // Buffered channel to prevent blocking
		lastStatusHashes: make(map[relayer.NotificationType]string),
	}
}

// computeStatusHash computes a fast MD5 hash of the status data for comparison
func (s *poller) computeStatusHash(data interface{}) (string, error) {
	jsonData, err := json.Marshal(data)
	if err != nil {
		return "", err
	}

	//nolint:gosec
	hash := md5.Sum(jsonData)
	return fmt.Sprintf("%x", hash), nil
}

// shouldSendNotification checks if the status has changed since last notification
// Returns true if status changed or if this is the first time checking this status type
func (s *poller) shouldSendNotification(notificationType relayer.NotificationType, data interface{}) bool {
	if data == nil {
		return false
	}

	currentHash, err := s.computeStatusHash(data)
	if err != nil {
		// If we can't compute hash, send the notification anyway
		s.logger.Warn("Failed to compute status hash, sending notification anyway",
			zap.String("type", string(notificationType)),
			zap.Error(err))
		return true
	}

	s.RLock()
	lastHash, exists := s.lastStatusHashes[notificationType]
	s.RUnlock()

	if !exists || lastHash != currentHash {
		// Only acquire write lock when we need to update
		s.Lock()
		s.lastStatusHashes[notificationType] = currentHash
		s.Unlock()
		return true
	}

	return false
}

func (s *poller) Start(ctx context.Context) {
	s.logger.Info("Starting status polling (player and device)")

	// Ticker for player and device status (every 10 seconds)
	statusTicker := time.NewTicker(POLL_INTERVAL)
	defer statusTicker.Stop()

	// Poll immediately on start
	s.pollPlayerStatus(ctx)
	s.pollDeviceStatus(ctx)

	for {
		select {
		case <-ctx.Done():
			s.logger.Info("Status polling stopped due to context cancellation")
			return
		case <-s.stopChan:
			s.logger.Info("Status polling stopped")
			return
		case <-statusTicker.C:
			s.pollPlayerStatus(ctx)
			s.pollDeviceStatus(ctx)
		case <-s.refreshChan:
			s.logger.Debug("Force refreshing status due to CDP command")
			s.pollPlayerStatus(ctx)
			s.pollDeviceStatus(ctx)
		}
	}
}

func (s *poller) Stop() {
	s.logger.Info("Stopping status polling")
	close(s.stopChan)
}

// ForceRefresh triggers an immediate status poll
func (s *poller) ForceRefresh() {
	select {
	case s.refreshChan <- struct{}{}:
		// Successfully queued refresh
	default:
		// Channel is full, skip this refresh request
		s.logger.Debug("Refresh channel full, skipping force refresh")
	}
}

func (s *poller) pollPlayerStatus(ctx context.Context) {
	// Check if relayer is connected before polling
	if !s.relayer.IsConnected() {
		s.logger.Debug("Relayer not connected, skipping player status poll")
		return
	}

	s.logger.Debug("Polling player status from Chromium")

	playerStatus, err := s.FetchPlayerStatus(ctx)
	if err != nil {
		s.logger.Error("Failed to get player status from CDP", zap.Error(err))

		// Notify relayer about the CDP error in the expected format
		errorMessage := map[string]interface{}{
			"ok":    false,
			"error": err.Error(),
		}
		s.sendNotification(ctx, relayer.NOTIFICATION_TYPE_PLAYER_STATUS, errorMessage)
		return
	}

	s.sendNotification(ctx, relayer.NOTIFICATION_TYPE_PLAYER_STATUS, playerStatus)
}

func (s *poller) sendNotification(ctx context.Context, notificationType relayer.NotificationType, data interface{}) {
	if !s.shouldSendNotification(notificationType, data) {
		s.logger.Debug("Player status unchanged, skipping notification")
		return
	}

	// Send the notification via relayer
	if err := s.relayer.SendNotification(ctx, notificationType, data); err != nil {
		s.logger.Error("Failed to send notification via relayer", zap.Error(err))
	}

	// Send the noti via websocket
	noti := map[string]interface{}{
		"type":              "notification",
		"notification_type": string(notificationType),
		"message":           data,
	}
	if err := s.ws.SendAll(noti); err != nil {
		s.logger.Error("Failed to send notification via websocket", zap.Error(err))
	}
}

func (s *poller) FetchPlayerStatus(ctx context.Context) (*PlayerStatus, error) {
	payload := map[string]interface{}{
		"messageID": "",
		"message": map[string]interface{}{
			"command": "checkStatus",
			"request": map[string]interface{}{},
		},
	}
	// Marshal the payload to JSON string
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal checkStatus payload: %w", err)
	}

	// Send the payload to the CDP
	expr := fmt.Sprintf("window.handleCDPRequest(%s)", string(payloadBytes))
	result, err := s.cdp.NoLogSend(cdp.METHOD_EVALUATE, map[string]interface{}{
		"expression": expr,
	})
	if err != nil {
		return nil, fmt.Errorf("cdp evaluate failed: %w", err)
	}

	// Check if the result is a map
	resultMap, ok := result.(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("unexpected result type: %T", result)
	}

	// Get the message from the result
	message, ok := resultMap["message"]
	if !ok {
		return nil, fmt.Errorf("missing message in result")
	}

	// Marshal the message
	jsonMessage, err := s.json.Marshal(message)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal message: %w", err)
	}

	// Unmarshal the message
	playerStatus := &PlayerStatus{}
	if err := s.json.Unmarshal(jsonMessage, playerStatus); err != nil {
		return nil, fmt.Errorf("failed to unmarshal message: %w", err)
	}

	return playerStatus, nil
}

func (s *poller) pollDeviceStatus(ctx context.Context) {
	// Check if relayer is connected before polling
	if !s.relayer.IsConnected() {
		s.logger.Debug("Relayer not connected, skipping device status poll")
		return
	}

	s.logger.Debug("Polling device status")

	// Get device status using the shared function
	deviceStatus, err := s.deviceStatus.GetStatus(ctx)
	if err != nil {
		s.logger.Error("Failed to get device status", zap.Error(err))
		return
	}

	s.sendNotification(ctx, relayer.NOTIFICATION_TYPE_DEVICE_STATUS, deviceStatus)
}
