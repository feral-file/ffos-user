package status

import (
	"context"
	//nolint:gosec
	"crypto/md5"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	dp1playlist "github.com/display-protocol/dp1-go/playlist"

	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-controld/cdp"
	"github.com/feral-file/ffos-user/components/feral-controld/ddc"
	"github.com/feral-file/ffos-user/components/feral-controld/dp1"
	"github.com/feral-file/ffos-user/components/feral-controld/relayer"
	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
	"github.com/feral-file/ffos-user/components/feral-controld/ws"
)

const (
	POLL_INTERVAL = 5 * time.Second
)

type LoopMode string

const (
	LoopModeNone     LoopMode = "none"
	LoopModePlaylist LoopMode = "playlist"
	LoopModeOne      LoopMode = "one"
)

type PlayerStatus struct {
	Command        string                      `json:"castCommand,omitempty"`
	PlaylistURL    *string                     `json:"playlistURL,omitempty"`
	Playlist       *dp1.Playlist               `json:"playlist,omitempty"`
	Index          *int                        `json:"index"`
	IsPaused       *bool                       `json:"isPaused,omitempty"`
	Items          *[]dp1playlist.PlaylistItem `json:"items,omitempty"`
	Ok             bool                        `json:"ok,omitempty"`
	Error          *string                     `json:"error,omitempty"`
	DeviceSettings *struct {
		Scaling     *string `json:"scaling,omitempty"`
		Orientation *string `json:"orientation,omitempty"`
	} `json:"deviceSettings,omitempty"`
	LoopMode *LoopMode `json:"loopMode,omitempty"`
	Shuffle  *bool     `json:"shuffle,omitempty"`
}

//go:generate mockgen -source=status.go -destination=../mocks/status.go -package=mocks -mock_names=Poller=MockStatusPoller

type Poller interface {
	Start(ctx context.Context)
	Stop()
	ForceRefresh()
	FetchPlayerStatus(ctx context.Context) (*PlayerStatus, error)
	SuppressPlayerNotifications(suppress bool)
}

// poller handles periodic polling of both player status via CDP and device status
type poller struct {
	sync.RWMutex
	cdp          cdp.CDP
	relayer      relayer.Relayer
	ws           ws.WS
	deviceStatus DeviceStatus
	panelDDC     ddc.PanelDDC
	logger       *zap.Logger
	stopChan     chan struct{}
	refreshChan  chan struct{}
	json         wrapper.JSON

	// Store last status hashes per channel to avoid duplicate notifications.
	// Relayer and websocket are tracked separately so relayer can catch up after reconnect.
	lastRelayerStatusHashes map[relayer.NotificationType]string
	lastWSStatusHashes      map[relayer.NotificationType]string

	// When true, pollPlayerStatus will still fetch but not send notifications.
	// Used during OOM recovery to prevent persisting stale player state.
	suppressPlayerNotifications bool

	// Track playback state transitions for duration accumulation.
	lastPlaybackSampleAt      time.Time
	lastIsPlaying             bool
	playbackSampleInitialized bool
}

func NewPoller(
	cdp cdp.CDP,
	r relayer.Relayer,
	ws ws.WS,
	ds DeviceStatus,
	panelDDC ddc.PanelDDC,
	json wrapper.JSON,
	logger *zap.Logger,
) Poller {
	return &poller{
		cdp:                     cdp,
		relayer:                 r,
		ws:                      ws,
		deviceStatus:            ds,
		panelDDC:                panelDDC,
		logger:                  logger,
		json:                    json,
		stopChan:                make(chan struct{}),
		refreshChan:             make(chan struct{}, 10), // Buffered channel to prevent blocking
		lastRelayerStatusHashes: make(map[relayer.NotificationType]string),
		lastWSStatusHashes:      make(map[relayer.NotificationType]string),
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

// shouldSendByHash checks whether a given channel should receive the notification.
func (s *poller) shouldSendByHash(lastHashes map[relayer.NotificationType]string, notificationType relayer.NotificationType, currentHash string) bool {
	s.RLock()
	lastHash, exists := lastHashes[notificationType]
	s.RUnlock()

	if !exists || lastHash != currentHash {
		return true
	}

	return false
}

func (s *poller) updateStatusHash(lastHashes map[relayer.NotificationType]string, notificationType relayer.NotificationType, currentHash string) {
	s.Lock()
	lastHashes[notificationType] = currentHash
	s.Unlock()
}

func (s *poller) Start(ctx context.Context) {
	s.logger.Info("Starting status polling (player and device)")

	// Ticker for player and device status (every 10 seconds)
	statusTicker := time.NewTicker(POLL_INTERVAL)
	defer statusTicker.Stop()

	// Poll immediately on start
	s.pollPlayerStatus(ctx)
	s.pollDeviceStatus(ctx)
	s.pollDDCStatus(ctx)

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
			s.pollDDCStatus(ctx)
		case <-s.refreshChan:
			s.logger.Info("Force refreshing status due to CDP command")
			s.pollPlayerStatus(ctx)
			s.pollDeviceStatus(ctx)
			s.pollDDCStatus(ctx)
		}
	}
}

func (s *poller) Stop() {
	s.logger.Info("Stopping status polling")
	close(s.stopChan)
}

func (s *poller) SuppressPlayerNotifications(suppress bool) {
	s.Lock()
	s.suppressPlayerNotifications = suppress
	s.Unlock()
	s.logger.Info("Player notification suppression changed", zap.Bool("suppress", suppress))
}

// ForceRefresh triggers an immediate status poll
func (s *poller) ForceRefresh() {
	select {
	case s.refreshChan <- struct{}{}:
		// Successfully queued refresh
	default:
		// Channel is full, skip this refresh request
		s.logger.Info("Refresh channel full, skipping force refresh")
	}
}

func (s *poller) pollPlayerStatus(ctx context.Context) {
	now := time.Now()

	playerStatus, err := s.FetchPlayerStatus(ctx)
	if err != nil {
		s.updateArtPlaybackMetrics(false, now)
		s.logger.Error("Failed to get player status from CDP", zap.Error(err))

		// Notify relayer about the CDP error in the expected format
		errorMessage := map[string]interface{}{
			"ok":    false,
			"error": err.Error(),
		}
		s.sendNotification(ctx, relayer.NOTIFICATION_TYPE_PLAYER_STATUS, errorMessage)
		return
	}

	// Handle nil playerStatus (CDP returned nil result when player is not playing in case showing QR code)
	if playerStatus == nil {
		s.updateArtPlaybackMetrics(false, now)
		s.logger.Info("Player status is nil, skipping notification")
		return
	}

	s.updateArtPlaybackMetrics(isArtworkPlaying(playerStatus), now)

	s.RLock()
	suppressed := s.suppressPlayerNotifications
	s.RUnlock()
	if suppressed {
		s.logger.Info("Player notifications suppressed (OOM recovery), skipping")
		return
	}

	lightweightPlayerStatus := s.lightweightPlayerStatus(playerStatus)
	s.logger.Info("Sending lightweight player status", zap.Any("lightweightPlayerStatus_itemsLength", len(*lightweightPlayerStatus.Items)))

	s.sendNotification(ctx, relayer.NOTIFICATION_TYPE_PLAYER_STATUS, lightweightPlayerStatus)
}

func (s *poller) sendNotification(ctx context.Context, notificationType relayer.NotificationType, message interface{}) {
	if message == nil {
		s.logger.Debug("Notification message is nil, skipping notification",
			zap.String("notification_type", string(notificationType)))
		return
	}

	currentHash, err := s.computeStatusHash(message)
	forceSend := err != nil
	if forceSend {
		// If hash cannot be computed, attempt to send anyway.
		s.logger.Warn("Failed to compute status hash, sending notification without dedupe",
			zap.String("notification_type", string(notificationType)),
			zap.Error(err))
	}

	relayerConnected := s.relayer.IsConnected()
	s.logger.Info("Preparing notification delivery",
		zap.String("notification_type", string(notificationType)),
		zap.Bool("relayer_connected", relayerConnected),
		zap.Bool("force_send", forceSend),
		zap.Bool("hash_available", err == nil),
	)

	data := map[string]interface{}{
		"type":                 "notification",
		"notification_type":    string(notificationType),
		"message":              message,
		"persist_record_count": 1,
	}

	// Send the notification via relayer only when connected.
	if relayerConnected {
		if forceSend || s.shouldSendByHash(s.lastRelayerStatusHashes, notificationType, currentHash) {
			if err := s.relayer.Send(ctx, data); err != nil {
				s.logger.Error("Failed to send notification via relayer",
					zap.String("notification_type", string(notificationType)),
					zap.Error(err),
				)
			} else {
				s.logger.Info("Notification sent via relayer",
					zap.String("notification_type", string(notificationType)),
				)
				s.updateStatusHash(s.lastRelayerStatusHashes, notificationType, currentHash)
			}
		} else {
			s.logger.Info("Relayer status unchanged, skipping relayer notification",
				zap.String("notification_type", string(notificationType)))
		}
	} else {
		s.logger.Debug("Relayer not connected, skipping relayer notification send",
			zap.String("notification_type", string(notificationType)))
	}

	// Send the data via websocket
	if forceSend || s.shouldSendByHash(s.lastWSStatusHashes, notificationType, currentHash) {
		if err := s.ws.SendAll(data); err != nil {
			s.logger.Error("Failed to send notification via websocket",
				zap.String("notification_type", string(notificationType)),
				zap.Error(err),
			)
		} else {
			s.logger.Info("Notification sent via websocket",
				zap.String("notification_type", string(notificationType)),
			)
			s.updateStatusHash(s.lastWSStatusHashes, notificationType, currentHash)
		}
	} else {
		s.logger.Info("Websocket status unchanged, skipping websocket notification",
			zap.String("notification_type", string(notificationType)))
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

	if result == nil {
		// FIXME: This should not happen, resolve the root cause
		// We accept it for now to avoid flooding sentry with errors
		s.logger.Warn("CDP returned nil result for player status")
		return nil, nil
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

// lightweightPlayerStatus creates a lightweight player status by removing the large fields (Playlist) from the player status
// For all items, remove the source field from the item
func (s *poller) lightweightPlayerStatus(playerStatus *PlayerStatus) *PlayerStatus {
	items := make([]dp1playlist.PlaylistItem, 0)
	if playerStatus.Items != nil {
		items = make([]dp1playlist.PlaylistItem, 0, len(*playerStatus.Items))
		for _, item := range *playerStatus.Items {
			itemCopy := item
			itemCopy.Source = ""
			items = append(items, itemCopy)
		}
	}

	playerStatus.Items = &items
	playerStatus.Playlist = &dp1.Playlist{}
	return playerStatus
}

func (s *poller) pollDeviceStatus(ctx context.Context) {
	// Check if relayer is connected before polling
	if !s.relayer.IsConnected() {
		s.logger.Info("Relayer not connected, skipping device status poll",
			zap.Bool("relayer_connected", false),
		)
		return
	}

	s.logger.Info("Polling device status",
		zap.Bool("relayer_connected", true),
	)

	// Get device status using the shared function
	deviceStatus, err := s.deviceStatus.GetStatus(ctx)
	if err != nil {
		s.logger.Error("Failed to get device status", zap.Error(err))
		return
	}

	s.sendNotification(ctx, relayer.NOTIFICATION_TYPE_DEVICE_STATUS, deviceStatus)
}

// ddcPollTimeout bounds how long a single DDC status collection may run.
// CollectStatus shells out to ddcutil (detect + getvcp, each with a retry
// path), so worst-case is ~4 subprocess invocations over I2C. 15 seconds is
// generous for healthy hardware but short enough to prevent a bad I2C/DDC path
// from blocking the main poll loop indefinitely.
const ddcPollTimeout = 15 * time.Second

func (s *poller) pollDDCStatus(ctx context.Context) {
	if !s.relayer.IsConnected() {
		s.logger.Info("Relayer not connected, skipping DDC status poll")
		return
	}

	s.logger.Info("Polling DDC panel status")

	ddcCtx, cancel := context.WithTimeout(ctx, ddcPollTimeout)
	defer cancel()

	ddcStatus, err := s.panelDDC.CollectStatus(ddcCtx)
	if err != nil {
		s.logger.Error("Failed to get DDC panel status", zap.Error(err))
		return
	}

	s.sendNotification(ctx, relayer.NOTIFICATION_TYPE_DDC_STATUS, ddcStatus)
}
