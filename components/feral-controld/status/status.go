package status

import (
	"context"
	//nolint:gosec
	"crypto/md5"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/feral-file/ffos-user/components/feral-controld/cdp"
	"github.com/feral-file/ffos-user/components/feral-controld/relayer"

	"go.uber.org/zap"
)

const (
	POLL_INTERVAL = 5 * time.Second
)

//go:generate mockgen -source=status.go -destination=../mocks/status.go -package=mocks -mock_names=Poller=MockStatusPoller

type Poller interface {
	Start(ctx context.Context)
	Stop()
	ForceRefresh()
}

// poller handles periodic polling of both player status via CDP and device status
type poller struct {
	sync.RWMutex
	cdp          cdp.CDP
	relayer      relayer.Relayer
	deviceStatus DeviceStatus
	logger       *zap.Logger
	stopChan     chan struct{}
	refreshChan  chan struct{}

	// Store last status hashes for each notification type to avoid duplicate notifications
	lastStatusHashes map[relayer.NotificationType]string
}

func NewPoller(
	cdp cdp.CDP,
	r relayer.Relayer,
	ds DeviceStatus,
	logger *zap.Logger,
) Poller {
	return &poller{
		cdp:              cdp,
		relayer:          r,
		deviceStatus:     ds,
		logger:           logger,
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

	message, err := FetchPlayerStatus(ctx, s.cdp, s.logger)
	if err != nil {
		s.logger.Error("Failed to fetch player status", zap.Error(err))
		return
	}

	// Check if we should send this notification
	if !s.shouldSendNotification(relayer.NOTIFICATION_TYPE_PLAYER_STATUS, message) {
		s.logger.Debug("Player status unchanged, skipping notification")
		return
	}

	err = s.relayer.SendNotification(ctx, relayer.NOTIFICATION_TYPE_PLAYER_STATUS, message)
	if err != nil {
		s.logger.Error("Failed to send player status notification", zap.Error(err))
	}
}

// FetchPlayerStatus fetches the current player status via CDP once and returns the "message" payload
func FetchPlayerStatus(ctx context.Context, c cdp.CDP, logger *zap.Logger) (map[string]interface{}, error) {
	payloadStr, err := buildCheckStatusPayload()
	if err != nil {
		return nil, err
	}

	expr := fmt.Sprintf("window.handleCDPRequest(%s)", payloadStr)
	resultMap, err := sendCDPRequest(c, expr)
	if err != nil {
		return nil, err
	}

	message, err := extractMessage(resultMap)
	if err != nil {
		return nil, err
	}

	logger.Debug("Fetched player status", zap.Any("message", message))
	return message, nil
}

// buildCheckStatusPayload constructs the CDP payload for checkStatus command.
func buildCheckStatusPayload() (string, error) {
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
		return "", fmt.Errorf("failed to marshal checkStatus payload: %w", err)
	}
	return string(payloadBytes), nil
}

// sendCDPRequest evaluates a JavaScript expression via CDP and returns the result as map.
func sendCDPRequest(c cdp.CDP, expression string) (map[string]interface{}, error) {
	result, err := c.NoLogSend(cdp.METHOD_EVALUATE, map[string]interface{}{
		"expression": expression,
	})
	if err != nil {
		return nil, fmt.Errorf("cdp evaluate failed: %w", err)
	}

	resultMap, ok := result.(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("unexpected result type: %T", result)
	}
	return resultMap, nil
}

// extractMessage safely extracts the "message" field as map[string]interface{}.
func extractMessage(result map[string]interface{}) (map[string]interface{}, error) {
	message, ok := result["message"]
	if !ok {
		return nil, fmt.Errorf("missing 'message' in result")
	}
	msg, ok := message.(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("'message' has unexpected type: %T", message)
	}
	return msg, nil
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

	// Check if we should send this notification
	if !s.shouldSendNotification(relayer.NOTIFICATION_TYPE_DEVICE_STATUS, deviceStatus) {
		s.logger.Debug("Device status unchanged, skipping notification")
		return
	}

	// Send the device status as a notification
	err = s.relayer.SendNotification(ctx, relayer.NOTIFICATION_TYPE_DEVICE_STATUS, deviceStatus)
	if err != nil {
		s.logger.Error("Failed to send device status notification", zap.Error(err))
	}
}
