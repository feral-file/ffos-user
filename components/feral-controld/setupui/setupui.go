// Package setupui owns setup narration intent. Provisioning never waits for
// the display: the shared overlay controller delivers on its bounded worker.
// Native host snapshots, separately, describe the observed overlay.
package setupui

import (
	"context"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-controld/overlay"
	"github.com/feral-file/ffos-user/components/feral-controld/playerlink"
)

const (
	stateSoftAPQR     = "softap_qr"
	stateJoining      = "joining"
	stateJoinFailed   = "join_failed"
	stateUpdating     = "updating"
	stateClaimQR      = "claim_qr"
	stateReady        = "ready"
	stateHidden       = "hidden"
	stateScanning     = "scanning"
	stateFinalizing   = "finalizing"
	stateFactoryReset = "factory_reset"
	stateConnecting   = "connecting"
	stateSetupError   = "setup_error"
	StateClaimQR      = stateClaimQR
	StateFinalizing   = stateFinalizing
	StateUpdating     = stateUpdating
	StateFactoryReset = stateFactoryReset
)

// Sender is the product boundary, independent of artwork/page lifecycle.
// SetOverlay must bound its I/O and must never log the credential fields.
type Sender interface {
	SetOverlay(context.Context, string, string, map[string]any) error
}
type Watcher interface {
	Watch(context.Context, func(playerlink.SnapshotDelivery)) error
}

type Service struct {
	sender Sender
	logger *zap.Logger
	ctrl   *overlay.Controller
	mu     sync.Mutex
	// Written in the controller's commit hook, in decision order.
	narrated     bool
	hiddenIntent bool
	// Last host observation; never inferred from an accepted send or intent.
	observed string
}

func New(sender Sender, logger *zap.Logger) *Service {
	if logger == nil {
		logger = zap.NewNop()
	}
	s := &Service{sender: sender, logger: logger}
	s.ctrl = overlay.New(setupPainter{s: s})
	return s
}

// Run observes host state and replays current intent after each reconnect.
// A snapshot callback only updates memory/queues work; it never performs I/O.
// Renderer navigation/crashes do not reconnect this host-owned channel.
func (s *Service) Run(ctx context.Context, watcher Watcher) {
	delay := time.Second
	for ctx.Err() == nil {
		started := time.Now()
		err := watcher.Watch(ctx, func(delivery playerlink.SnapshotDelivery) {
			s.Observe(delivery.Snapshot)
			if delivery.Initial {
				s.Resync()
			}
		})
		s.mu.Lock()
		s.observed = ""
		s.mu.Unlock()
		if ctx.Err() != nil {
			return
		}
		s.logger.Info("Setup display observation disconnected", zap.Error(err))
		if time.Since(started) >= time.Minute {
			delay = time.Second
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		delay = min(delay*2, 30*time.Second)
	}
}

func (s *Service) Observe(snapshot playerlink.Snapshot) {
	state := ""
	if snapshot.Overlay != nil && snapshot.Overlay.Owner == "setup" && snapshot.Overlay.State != stateHidden {
		state = snapshot.Overlay.State
	}
	s.mu.Lock()
	s.observed = state
	s.mu.Unlock()
}

func (s *Service) CurrentNarrationState() (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.observed, s.observed != ""
}

// The native protocol requires connecting support. The old bundle-version
// hide fallback has no place in this coordinated runtime/controld release.
func (s *Service) ShowConnectingOrHide(message string) { s.ShowConnecting(message) }

func (s *Service) trySend(ctx context.Context, req map[string]any) error {
	state := stringField(req, "state")
	fields := make(map[string]any, len(req))
	for k, v := range req {
		if k != "state" {
			fields[k] = v
		}
	}
	err := s.sender.SetOverlay(ctx, "setup", state, fields)
	if err != nil {
		s.logger.Info("Setup narration delivery failed", zap.String("state", state), zap.Error(err))
	}
	return err
}

func stringField(req map[string]any, key string) string {
	value, _ := req[key].(string)
	return value
}

func (s *Service) ShowSoftAPQR(ssid string, psk string, portalURL string) {
	req := map[string]any{
		"state": stateSoftAPQR,
		"ssid":  ssid,
	}
	if strings.TrimSpace(psk) != "" {
		req["password"] = psk
	}
	if strings.TrimSpace(portalURL) != "" {
		req["portal_url"] = portalURL
	}
	s.push(req)
}

func (s *Service) ShowSoftAPPortalQR(ssid string, psk string, portalURL string) {
	req := map[string]any{
		"state":           stateSoftAPQR,
		"ssid":            ssid,
		"client_attached": true,
	}
	if strings.TrimSpace(psk) != "" {
		req["password"] = psk
	}
	if strings.TrimSpace(portalURL) != "" {
		req["portal_url"] = portalURL
	}
	s.push(req)
}

func (s *Service) ShowSoftAPQRRetry(ssid string, psk string, portalURL string, reason string) {
	req := map[string]any{
		"state":  stateSoftAPQR,
		"ssid":   ssid,
		"reason": reason,
	}
	if strings.TrimSpace(psk) != "" {
		req["password"] = psk
	}
	if strings.TrimSpace(portalURL) != "" {
		req["portal_url"] = portalURL
	}
	s.push(req)
}

func (s *Service) ShowScanning() {
	s.push(map[string]any{"state": stateScanning})
}

func (s *Service) ShowJoining() {
	s.push(map[string]any{"state": stateJoining})
}

func (s *Service) ShowJoinFailed(reason string) {
	req := map[string]any{"state": stateJoinFailed}
	if strings.TrimSpace(reason) != "" {
		req["reason"] = reason
	}
	s.push(req)
}

func (s *Service) ShowConnecting(message string) {
	req := map[string]any{"state": stateConnecting}
	if strings.TrimSpace(message) != "" {
		req["reason"] = message
	}
	s.push(req)
}

func (s *Service) ShowConnectingIfShowing(message string, states ...string) {
	req := map[string]any{"state": stateConnecting}
	if strings.TrimSpace(message) != "" {
		req["reason"] = message
	}
	s.pushIf(req, func(last map[string]any) bool {
		current := stringField(last, "state")
		for _, st := range states {
			if current == st {
				return true
			}
		}
		return false
	})
}

func (s *Service) ShowSetupError(reason string) {
	req := map[string]any{"state": stateSetupError}
	if strings.TrimSpace(reason) != "" {
		req["reason"] = reason
	}
	s.push(req)
}

func (s *Service) ShowUpdating(progress int) {
	s.push(map[string]any{
		"state":    stateUpdating,
		"progress": progress,
	})
}

func (s *Service) ShowFinalizing() {
	s.push(map[string]any{"state": stateFinalizing})
}

func (s *Service) ShowClaimQR(url string, deviceName string) {
	s.showClaimQR(url, deviceName, overlay.Owner)
}

func (s *Service) ShowClaimQRAutomatic(url string, deviceName string) {
	s.showClaimQR(url, deviceName, overlay.Automatic)
}

func (s *Service) showClaimQR(url string, deviceName string, pr overlay.Priority) {
	req := map[string]any{
		"state": stateClaimQR,
		"url":   url,
	}
	if strings.TrimSpace(deviceName) != "" {
		req["device_name"] = deviceName
	}
	s.showOwned(req, pr, nil)
}

func (s *Service) RefreshClaimQRName(resolve func() string) {
	cur, has := s.ctrl.Current()
	if !has || setupState(cur) != stateClaimQR {
		return
	}
	req := map[string]any{"state": stateClaimQR}
	if deviceName := resolve(); strings.TrimSpace(deviceName) != "" {
		req["device_name"] = deviceName
	}
	s.pushIf(req, func(last map[string]any) bool {
		if stringField(last, "state") != stateClaimQR {
			return false
		}
		req["url"] = stringField(last, "url")
		return true
	})
}

func (s *Service) RefreshClaimQRURL(build func() string) {
	cur, has := s.ctrl.Current()
	if !has || setupState(cur) != stateClaimQR {
		return
	}
	url := build()
	if url == "" {
		return
	}
	req := map[string]any{"state": stateClaimQR, "url": url}
	s.pushIf(req, func(last map[string]any) bool {
		if stringField(last, "state") != stateClaimQR {
			return false
		}
		if name := stringField(last, "device_name"); name != "" {
			req["device_name"] = name
		}
		return true
	})
}

func (s *Service) ClaimQRShowing() bool {
	cur, has := s.ctrl.Current()
	return has && setupState(cur) == stateClaimQR
}

func (s *Service) ShowReady() {
	s.push(map[string]any{"state": stateReady})
}

func (s *Service) ShowFactoryReset() {
	s.push(map[string]any{"state": stateFactoryReset})
}

func (s *Service) Hide() {
	s.push(map[string]any{"state": stateHidden})
}

func (s *Service) SweepStaleOverlay() {
	s.mu.Lock()
	narrated := s.narrated
	s.mu.Unlock()
	if narrated {
		return
	}
	// See the doc comment above for why this condition — not nil — is required.
	s.showOwned(map[string]any{"state": stateHidden}, overlay.Automatic, func(last map[string]any) bool {
		return len(last) == 0
	})
}

func (s *Service) HideIfShowing(states ...string) {
	s.hideIf(func(cur overlay.Overlay, has bool) bool {
		if !has || !isSetup(cur) {
			return false
		}
		current := setupState(cur)
		for _, st := range states {
			if current == st {
				return true
			}
		}
		return false
	})
}

func (s *Service) Narrating() bool {
	cur, has := s.ctrl.Current()
	return has && isSetup(cur) && setupState(cur) != stateHidden
}

func (s *Service) Resync() {
	if s.ctrl.Replay(s) {
		return
	}
	s.mu.Lock()
	hidden := s.hiddenIntent
	s.mu.Unlock()
	if !hidden {
		return
	}
	// ReplayHide, not HideIf: HideIf's own delivery is always the CURRENT
	// overlay, which is nil here by definition (nothing is current) — a
	// Kind-less delivery the shared Router cannot route to any painter, so it
	// would be silently dropped in production (caught in review on this very
	// fix). ReplayHide carries setupui's own Kind explicitly instead, and
	// still only queues it if nothing has taken the screen in the meantime,
	// re-checked under its own lock — same gap-closing reasoning as Replay
	// above.
	_ = s.ctrl.ReplayHide(overlay.Overlay{Kind: setupKind(stateHidden)})
}

func (s *Service) push(req map[string]any) {
	s.pushIf(req, nil)
}

func (s *Service) pushIf(req map[string]any, ok func(last map[string]any) bool) {
	if stringField(req, "state") == stateHidden {
		s.hideIf(func(cur overlay.Overlay, has bool) bool {
			return has && isSetup(cur)
		})
		return
	}
	s.showOwned(req, overlay.Owner, ok)
}
