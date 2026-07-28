package status

import (
	"context"
	//nolint:gosec
	"crypto/md5"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	dp1playlist "github.com/display-protocol/dp1-go/playlist"

	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-controld/cdp"
	constants "github.com/feral-file/ffos-user/components/feral-controld/constant"
	"github.com/feral-file/ffos-user/components/feral-controld/ddc"
	"github.com/feral-file/ffos-user/components/feral-controld/dp1"
	"github.com/feral-file/ffos-user/components/feral-controld/drm"
	"github.com/feral-file/ffos-user/components/feral-controld/relayer"
	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
	"github.com/feral-file/ffos-user/components/feral-controld/ws"
)

const POLL_INTERVAL = 5 * time.Second

type LoopMode string

const (
	LoopModeNone     LoopMode = "none"
	LoopModePlaylist LoopMode = "playlist"
	LoopModeOne      LoopMode = "one"
)

type PlayerStatus struct {
	Command     string        `json:"castCommand,omitempty"`
	PlaylistURL *string       `json:"playlistURL,omitempty"`
	Playlist    *dp1.Playlist `json:"playlist,omitempty"`
	Index       *int          `json:"index"`
	// Artwork render outcome reported by the player: 0 pending, 1 loading,
	// 2 ready, 3 failed. Relayed unchanged; controld does not interpret it.
	RenderStatus   *int                        `json:"renderStatus,omitempty"`
	IsPaused       *bool                       `json:"isPaused,omitempty"`
	Items          *[]dp1playlist.PlaylistItem `json:"items,omitempty"`
	Ok             bool                        `json:"ok,omitempty"`
	Error          *string                     `json:"error,omitempty"`
	DeviceSettings *struct {
		Scaling     *string `json:"scaling,omitempty"`
		Orientation *string `json:"orientation,omitempty"`
		// Device-level default playlist item duration in seconds, set via the
		// updateDefaultDuration cast command. Absent means "auto" (no
		// override). Must round-trip here or this typed re-marshal drops it
		// from player_status notifications before controllers can read it.
		DefaultDuration *float64 `json:"defaultDuration,omitempty"`
		// Device-level tombstone (museum label) state: "on", "off", or
		// "timed". Absent means the player never had one set and falls back
		// to "timed". Same round-trip requirement as DefaultDuration — ff-app
		// reads it to show the tombstone control's current selection.
		Tombstone *string `json:"tombstone,omitempty"`
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

	// displayConnected reports whether a physical display is attached (DRM
	// connector status). Consulted before each DDC poll: with no display,
	// ddcutil can never succeed, and every 5s round would spawn three ddcutil
	// subprocesses and log an info+warn+error triplet — a permanent log flood
	// on headless devices. Field (not a direct call) so tests can stub it.
	displayConnected func() bool

	// Whether CDP was initialized at the previous poll round. Only touched on the
	// Start goroutine. Used to catch the disconnected→connected transition: a
	// restarted Chromium reloads the web app from defaults, so status deduped
	// against pre-restart hashes must be pushed fresh (the old
	// PartOf=chromium-ready.target design reset these maps by restarting controld).
	cdpWasInitialized bool
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
		displayConnected:        func() bool { return drm.DisplayConnected(drm.DefaultSysfsRoot) },
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
	s.pollRound(ctx)

	for {
		select {
		case <-ctx.Done():
			s.logger.Info("Status polling stopped due to context cancellation")
			return
		case <-s.stopChan:
			s.logger.Info("Status polling stopped")
			return
		case <-statusTicker.C:
			s.pollRound(ctx)
		case <-s.refreshChan:
			s.logger.Info("Force refreshing status due to CDP command")
			s.pollRound(ctx)
		}
	}
}

// pollRound runs one full poll pass. Before polling it drops the dedup hashes
// whenever CDP has just (re)connected: the restarted Chromium's web app reloaded
// from defaults, so "unchanged since last push" no longer implies the client has
// the state — everything must go out fresh once.
func (s *poller) pollRound(ctx context.Context) {
	cdpInitialized := s.cdp.Initialized()
	if cdpInitialized && !s.cdpWasInitialized {
		s.logger.Info("CDP (re)connected, clearing status dedup caches for a fresh push")
		clear(s.lastRelayerStatusHashes)
		clear(s.lastWSStatusHashes)
	}
	s.cdpWasInitialized = cdpInitialized

	s.pollPlayerStatus(ctx)
	s.pollDeviceStatus(ctx)
	s.pollDDCStatus(ctx)
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
	// While CDP is intentionally absent (headless boot with no monitor, or mid-reconnect
	// after a kiosk/Chromium restart) every checkStatus send would fail at Error level and
	// emit a player-status error notification each interval, flooding logs and Sentry. Skip
	// the poll entirely in that state, but keep playback-duration accounting moving with a
	// "not playing" sample so metrics do not freeze while disconnected.
	if !s.cdp.Initialized() {
		s.updateArtPlaybackMetrics(false, time.Now())
		s.logger.Debug("Skipping player status poll: CDP not connected")
		return
	}

	pageURL, err := s.cdp.PageNavigationURL(ctx)
	if err != nil {
		s.logger.Debug("Failed to read page URL before player status poll", zap.Error(err))
	} else if !isPlayerPageURL(pageURL) {
		// Non-player pages do not support checkStatus at all, so polling CDP here
		// cannot produce a real player update. Skipping the command avoids waking
		// Chromium on QR/setup screens while keeping playback-duration accounting
		// moving forward with a "not playing" sample.
		s.updateArtPlaybackMetrics(false, time.Now())
		s.logger.Info("Skipping player status poll because Chromium is not on the player page",
			zap.String("page_url", pageURL),
		)
		return
	}

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
		s.logger.Debug("Player status is nil, skipping notification")
		return
	}

	s.updateArtPlaybackMetrics(isArtworkPlaying(playerStatus), now)

	s.RLock()
	suppressed := s.suppressPlayerNotifications
	s.RUnlock()
	if suppressed {
		s.logger.Debug("Player notifications suppressed (OOM recovery), skipping")
		return
	}

	lightweightPlayerStatus := s.lightweightPlayerStatus(playerStatus)
	s.logger.Debug("Sending lightweight player status", zap.Any("lightweightPlayerStatus_itemsLength", len(*lightweightPlayerStatus.Items)))

	s.sendNotification(ctx, relayer.NOTIFICATION_TYPE_PLAYER_STATUS, lightweightPlayerStatus)
}

func isPlayerPageURL(url string) bool {
	return strings.HasPrefix(url, constants.WEBAPP_URL)
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
	s.logger.Debug("Preparing notification delivery",
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
			s.logger.Debug("Relayer status unchanged, skipping relayer notification",
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
		s.logger.Debug("Websocket status unchanged, skipping websocket notification",
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
		s.logger.Debug("Relayer not connected, skipping device status poll",
			zap.Bool("relayer_connected", false),
		)
		return
	}

	s.logger.Debug("Polling device status",
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
		s.logger.Debug("Relayer not connected, skipping DDC status poll")
		return
	}

	// Refresh the tracker's display fingerprint every round, BEFORE the
	// no-display gate below can skip out. ShouldPoll's side effect is the
	// poller's only per-tick fingerprint observation; if it only ran while a
	// display is connected, an unplugged/powered-off period would be invisible
	// to the tracker (the poller stops looking exactly while the topology is
	// different), and verdicts latched around power-off (unsupported,
	// recovery-futile) would survive an off→on cycle whose end state
	// fingerprints identically to the start. Observing the "off" topology
	// resets the tracker, so the monitor's return starts a fresh display
	// generation — which also re-arms Generation()-keyed consumers like the
	// sleep panel leg's retry cap.
	shouldPoll := s.panelDDC != nil && s.panelDDC.ShouldPoll()

	// Headless: no DRM connector is connected, so ddcutil cannot find a display
	// and CollectStatus would fail (with recovery retries) every round. Skip the
	// poll; plugging a monitor in flips the connector to "connected" via HPD and
	// the next 5s round resumes polling. Cloud-initiated DDC commands still go
	// through the executor and report their own errors, so this only quiets the
	// background poll. A nil check keeps hand-constructed pollers (tests) safe.
	//
	// Both skip paths still push a one-shot "panel unreadable" DDC status (the
	// hash dedup in sendNotification collapses repeats): consumers that cached
	// the last real panel status (e.g. a brightness slider in the app) would
	// otherwise show stale values forever, because pre-gate code kept emitting
	// an Errors-carrying status when the panel became unreadable.
	if s.displayConnected != nil && !s.displayConnected() {
		s.logger.Debug("Skipping DDC status poll: no display connected")
		s.sendNotification(ctx, relayer.NOTIFICATION_TYPE_DDC_STATUS, ddcStatusNoDisplay())
		return
	}

	// The availability tracker judged the display DDC/CI-incapable; it
	// re-probes on display changes and on a slow interval. Not a fault — stay
	// quiet instead of logging every 5s round.
	if !shouldPoll {
		s.logger.Debug("Skipping DDC status poll: display does not support DDC/CI")
		s.sendNotification(ctx, relayer.NOTIFICATION_TYPE_DDC_STATUS, ddcStatusUnsupported())
		return
	}

	s.logger.Debug("Polling DDC panel status")

	ddcCtx, cancel := context.WithTimeout(ctx, ddcPollTimeout)
	defer cancel()

	ddcStatus, err := s.panelDDC.CollectStatus(ddcCtx)
	if err != nil {
		s.logger.Error("Failed to get DDC panel status", zap.Error(err))
		return
	}

	// A failed reprobe of a panel the tracker still judges unavailable must
	// push the SAME steady "unsupported" payload the skip path above pushes,
	// not the raw per-field ddcutil errors. Otherwise the two payloads
	// alternate in the per-type hash dedup (skip rounds send one, reprobe
	// rounds the other) and BOTH go to the relayer every reprobe cycle,
	// forever, while a monitor is merely powered off. ShouldPoll is false
	// right after a failed reprobe (the failure re-armed the next window) and
	// true after a successful one, so real status flows the moment the panel
	// answers again — and first-time failures of a not-yet-demoted panel still
	// surface their real error fields.
	if !s.panelDDC.ShouldPoll() {
		s.sendNotification(ctx, relayer.NOTIFICATION_TYPE_DDC_STATUS, ddcStatusUnsupported())
		return
	}

	s.sendNotification(ctx, relayer.NOTIFICATION_TYPE_DDC_STATUS, ddcStatus)
}

// ddcStatusNoDisplay / ddcStatusUnsupported are the poll-skip notifications:
// same DdcPanelStatus shape (Errors map) the pre-tracker code produced when
// ddcutil could not read the panel, so wire consumers need no new handling.
// Fresh values per call — sendNotification marshals asynchronously to two
// channels and must never share mutable state.
func ddcStatusNoDisplay() *ddc.DdcPanelStatus {
	return &ddc.DdcPanelStatus{Errors: map[string]string{"panel": "no display connected"}}
}

func ddcStatusUnsupported() *ddc.DdcPanelStatus {
	return &ddc.DdcPanelStatus{Errors: map[string]string{"panel": "display does not support DDC/CI"}}
}
