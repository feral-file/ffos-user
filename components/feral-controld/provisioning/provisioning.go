// Package provisioning is the FF1 setup access-point trigger state machine. It
// decides WHEN the device raises the captive-portal setup AP and drives the
// AP-bounce sequencing a Wi-Fi join requires on single-radio hardware.
//
// It composes three foundation primitives, each behind a narrow consumer-owned
// interface so the machine is testable without nmcli, D-Bus, or a real socket:
//   - a Wi-Fi controller (wifictl) for saved-profile checks, the pre-AP scan
//     cache, and the join itself;
//   - a softap.Backend for raising/tearing the NM hotspot;
//   - a Connectivity source (sys-monitord over D-Bus in production) for
//     reachability now and on change.
//
// Hard sequencing constraints from the wifictl/softap authors (single radio; NM
// serializes Wi-Fi ops), enforced here:
//  1. RefreshScanCache runs immediately BEFORE softap.Up (station mode still owns
//     the radio); while the AP is up the portal reads only CachedScan.
//  2. softap.Down runs BEFORE wifictl.Join; any join failure re-raises the AP so
//     the user can retry — auth failures included.
//
// This package imports neither relayer nor cdp.
package provisioning

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-controld/portal"
	"github.com/feral-file/ffos-user/components/feral-controld/softap"
	"github.com/feral-file/ffos-user/components/feral-controld/wifictl"
	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

// State is the provisioning lifecycle. It is observable via State and published
// to the Notifier on every change.
type State string

const (
	// StateOnline: the device reaches the network and has a saved Wi-Fi profile.
	// The setup AP is down.
	StateOnline State = "online"
	// StateOfflineRetrying: the device WAS provisioned but lost reachability. NM
	// keeps retrying; the AP is deferred until the sustained-offline window
	// elapses, so a router reboot never pops the setup AP.
	StateOfflineRetrying State = "offline_retrying"
	// StateUnprovisioned: no saved Wi-Fi profile but the device is reachable by
	// other means (e.g. Ethernet). The AP stays down — Ethernet devices never
	// raise the setup AP.
	StateUnprovisioned State = "unprovisioned"
	// StateAPActive: the setup AP and captive portal are up, awaiting credentials.
	StateAPActive State = "ap_active"
	// StateJoining: applying user-submitted credentials. The AP is down for the
	// duration of the attempt (single radio) and re-raised on failure.
	StateJoining State = "joining"
)

// Detail is the side-channel context published alongside a State change: enough
// for a narration UI to explain WHY the state changed without re-deriving it.
type Detail struct {
	// SSID is the AP SSID (APActive, once the AP is up) or the target join SSID
	// (Joining/failed).
	SSID string
	// PSK is populated ONLY on the StateAPActive notification the machine emits
	// once the setup AP is actually raised, carrying the live hotspot passphrase
	// so a narration surface can render the soft-AP QR. It is empty on every other
	// notification (the AP-not-yet-up entry, join failures, etc.), which is how a
	// Notifier tells "these are the AP credentials" from "this SSID is a join
	// target". It is never logged.
	PSK string
	// Reason is a short machine-readable cause (e.g. "auth-failure",
	// "sustained-offline", "unprovisioned").
	Reason string
	// Message is a human-facing description.
	Message string
}

// Notifier receives state changes for narration (a setupui package subscribes
// later). It is fire-and-forget: the machine calls OnStateChange inline on its
// single event-loop goroutine and guards against panics, but does NOT spawn a
// goroutine per callback, so ordering is preserved. Implementations MUST return
// promptly and push any slow work (e.g. driving CDP) onto their own goroutine —
// a blocking Notifier stalls the machine.
type Notifier interface {
	OnStateChange(State, Detail)
}

// WifiController is the subset of *wifictl.Controller the machine drives. Kept
// consumer-owned so tests fake it. Join returns error; the concrete type is
// *wifictl.JoinError, matched with errors.As to classify failures.
type WifiController interface {
	HasSavedProfile(ctx context.Context) (bool, error)
	RefreshScanCache(ctx context.Context) ([]string, error)
	CachedScan(ctx context.Context) ([]string, error)
	Join(ctx context.Context, ssid, psk string) error
}

// Connectivity reports the device's network reachability as sys-monitord sees
// it. Online is true when the device can reach the network via ANY interface
// (Wi-Fi or Ethernet); the machine treats Online as "do not raise the setup AP".
//
// This conflates "online via Ethernet" with "online via Wi-Fi", which is exactly
// what the AP decision needs (either way, no AP) and is what sys-monitord's
// GetConnectivityStatus / connectivity_change actually expose. The finer
// link-type-aware gating (mediator's LinkState seam) is a future refinement;
// this machine deliberately does not depend on it.
type Connectivity interface {
	Online(ctx context.Context) (bool, error)
	Subscribe(fn func(online bool)) (unsubscribe func())
}

// PortalServer is the lifecycle the machine drives on the captive portal. Satisfied
// by *portal.Server; injectable so tests avoid binding a socket.
type PortalServer interface {
	Start() error
	Stop(ctx context.Context) error
}

const (
	defaultPortalAddr    = ":80"
	defaultOfflineWindow = 5 * time.Minute
	defaultCheckInterval = 15 * time.Second
	portalStopTimeout    = 3 * time.Second
	eventBuffer          = 16
)

// Config wires the machine's dependencies and tunables.
type Config struct {
	AP           softap.Backend
	Wifi         WifiController
	Connectivity Connectivity
	Clock        wrapper.Clock
	Logger       *zap.Logger

	// Notifier is optional.
	Notifier Notifier

	// WiredLink is an optional guard against raising the setup AP on a device
	// that has an active WIRED (ethernet) link but is reported offline. The
	// Connectivity source is internet-reachability only, so a wired device whose
	// upstream/router is momentarily down looks identical to a truly disconnected
	// one — and an unprovisioned wired device would otherwise pop the setup AP even
	// though ethernet is its intended path. When WiredLink reports true the machine
	// treats the device as reachable-by-wire and keeps the AP down (exactly as an
	// ethernet-online device does). It is deliberately wired-ONLY: a wifi link that
	// is up but offline must still reach the AP path, because that is the
	// broken-credentials case the AP exists to fix. Nil disables the guard
	// (preserving the Connectivity-only behavior).
	WiredLink func(ctx context.Context) bool

	// PortalAddr is the portal bind address (default ":80").
	PortalAddr string
	// OfflineWindow is the sustained-offline duration before a provisioned device
	// raises the AP (default 5m).
	OfflineWindow time.Duration
	// CheckInterval is how often the machine re-evaluates the offline window and
	// retries a deferred AP operation (default 15s).
	CheckInterval time.Duration

	// NewPortal builds a PortalServer from a portal.Config. Defaults to
	// portal.NewServer; overridden in tests.
	NewPortal func(portal.Config) PortalServer
}

// Machine is the setup-AP trigger state machine.
type Machine struct {
	ap        softap.Backend
	wifi      WifiController
	conn      Connectivity
	clock     wrapper.Clock
	logger    *zap.Logger
	notifier  Notifier
	wiredLink func(ctx context.Context) bool
	newPortal func(portal.Config) PortalServer

	portalAddr    string
	offlineWindow time.Duration
	checkInterval time.Duration

	sup    *supervisor
	events chan event

	// lifecycle
	runMu  sync.Mutex
	cancel context.CancelFunc
	done   chan struct{}

	// shared state, guarded by mu; read by external goroutines (portal seams,
	// State) and written only by the transition goroutine.
	mu           sync.Mutex
	state        State
	status       portal.Status
	apUp         bool
	apInfo       softap.Info
	portalSrv    PortalServer
	offlineSince time.Time
}

type eventKind int

const (
	evConnectivity eventKind = iota
	evJoin
)

type event struct {
	kind   eventKind
	online bool
	ssid   string
	psk    string
}

// New builds a Machine, applying defaults.
func New(cfg Config) *Machine {
	logger := cfg.Logger
	if logger == nil {
		logger = zap.NewNop()
	}
	if cfg.PortalAddr == "" {
		cfg.PortalAddr = defaultPortalAddr
	}
	if cfg.OfflineWindow <= 0 {
		cfg.OfflineWindow = defaultOfflineWindow
	}
	if cfg.CheckInterval <= 0 {
		cfg.CheckInterval = defaultCheckInterval
	}
	if cfg.NewPortal == nil {
		cfg.NewPortal = func(pc portal.Config) PortalServer { return portal.NewServer(pc) }
	}
	return &Machine{
		ap:            cfg.AP,
		wifi:          cfg.Wifi,
		conn:          cfg.Connectivity,
		clock:         cfg.Clock,
		logger:        logger,
		notifier:      cfg.Notifier,
		wiredLink:     cfg.WiredLink,
		newPortal:     cfg.NewPortal,
		portalAddr:    cfg.PortalAddr,
		offlineWindow: cfg.OfflineWindow,
		checkInterval: cfg.CheckInterval,
		sup:           newSupervisor(cfg.Clock, logger),
		events:        make(chan event, eventBuffer),
		state:         StateOnline,
		status:        portal.Status{State: portal.JoinIdle},
	}
}

// -----------------------------------------------------------------------------
// Lifecycle (the Run/Start-Stop surface main.go composes)
// -----------------------------------------------------------------------------

// Start launches the supervised event loop in the background. A Machine whose
// Start is never called stays fully dormant and raises no AP; run() guards its
// Start call so the test app (which leaves Provisioning nil) never pops the setup
// AP.
func (m *Machine) Start(ctx context.Context) {
	m.runMu.Lock()
	defer m.runMu.Unlock()
	if m.cancel != nil {
		return // already started
	}
	runCtx, cancel := context.WithCancel(ctx)
	m.cancel = cancel
	m.done = make(chan struct{})
	go func() {
		defer close(m.done)
		m.sup.run(runCtx, "provisioning-loop", m.loop)
	}()
}

// Stop cancels the loop, waits for it to exit, and ensures the AP/portal are
// torn down.
func (m *Machine) Stop() {
	m.runMu.Lock()
	cancel := m.cancel
	done := m.done
	m.cancel = nil
	m.runMu.Unlock()

	if cancel == nil {
		return
	}
	cancel()
	<-done
	// Final teardown on a fresh context so a canceled runCtx cannot skip it.
	m.ensureAPDown(context.Background())
}

// RestartCount reports how many times the supervised loop was restarted after a
// panic. Observability hook (and supervision test seam).
func (m *Machine) RestartCount() int64 { return m.sup.restartCount() }

// loop is the single event-processing goroutine. All transitions run here (or,
// in tests, are invoked directly), so state mutation is serialized without a
// per-transition lock; mu guards only the fields external goroutines read.
func (m *Machine) loop(ctx context.Context) {
	unsub := m.conn.Subscribe(func(online bool) {
		select {
		case m.events <- event{kind: evConnectivity, online: online}:
		case <-ctx.Done():
		}
	})
	defer unsub()

	ticker := m.clock.NewTicker(m.checkInterval)
	defer ticker.Stop()

	// Initial assessment from a point-in-time reachability query.
	online, err := m.conn.Online(ctx)
	if err != nil {
		m.logger.Warn("provisioning: initial connectivity query failed; assuming offline", zap.Error(err))
		online = false
	}
	m.onConnectivity(ctx, online)

	for {
		select {
		case <-ctx.Done():
			m.ensureAPDown(context.Background())
			return
		case ev := <-m.events:
			switch ev.kind {
			case evConnectivity:
				m.onConnectivity(ctx, ev.online)
			case evJoin:
				m.applyJoin(ctx, ev.ssid, ev.psk)
			}
		case <-ticker.C():
			m.onTick(ctx)
		}
	}
}

// -----------------------------------------------------------------------------
// Portal-facing seams
// -----------------------------------------------------------------------------

// RequestJoin is the portal's JoinFunc: it validates the submission and hands it
// to the loop, returning immediately. The AP-bounce + join run asynchronously on
// the loop goroutine because taking the AP down drops the phone that submitted
// the form; the phone re-associates and polls /status (Status) for the outcome.
func (m *Machine) RequestJoin(ssid, password string) error {
	if strings.TrimSpace(ssid) == "" {
		return errors.New("please choose a Wi-Fi network")
	}
	select {
	case m.events <- event{kind: evJoin, ssid: ssid, psk: password}:
		return nil
	default:
		m.logger.Warn("provisioning: join queue full, dropping submission", zap.String("ssid", ssid))
		return errors.New("device is busy, please try again")
	}
}

// Status is the portal's StatusFunc: the current/last join outcome. It lives in
// the machine (not the portal) so it survives portal restarts / AP re-raises.
func (m *Machine) Status() portal.Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.status
}

// State returns the current lifecycle state.
func (m *Machine) State() State {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.state
}

// -----------------------------------------------------------------------------
// Transition logic (runs on the loop goroutine, or directly in tests)
// -----------------------------------------------------------------------------

// onConnectivity applies a reachability reading and drives the resting-state
// decision. See the rule table in the package doc.
func (m *Machine) onConnectivity(ctx context.Context, online bool) {
	if online {
		m.clearOffline()
		if m.hasProfile(ctx) {
			m.transition(ctx, StateOnline, Detail{Message: "Connected to the network"})
		} else {
			// Reachable without a saved Wi-Fi profile: Ethernet. Never raise the AP.
			m.transition(ctx, StateUnprovisioned, Detail{
				Reason:  "unprovisioned",
				Message: "Online via wired network; Wi-Fi not configured",
			})
		}
		return
	}

	// Offline.
	if !m.hasProfile(ctx) {
		// Wired-link guard: a device with an active ethernet link is reachable by
		// wire even when sys-monitord reports offline (e.g. a WAN/router blip), and
		// ethernet is its intended path — it must never pop the setup AP, exactly as
		// an ethernet-ONLINE device never does. Treat it as unprovisioned-but-wired
		// (AP stays down). Wifi-link-up-but-offline is deliberately NOT caught here:
		// that is the broken-credentials case the AP exists to fix.
		if m.hasWiredLink(ctx) {
			m.clearOffline()
			m.transition(ctx, StateUnprovisioned, Detail{
				Reason:  "wired-link",
				Message: "Wired network present; Wi-Fi not configured",
			})
			return
		}

		// Unprovisioned AND no other connectivity: the device cannot self-heal,
		// so raise the AP immediately.
		m.clearOffline()
		m.resetJoinStatus()
		m.transition(ctx, StateAPActive, Detail{
			Reason:  "unprovisioned",
			Message: "Set up Wi-Fi to continue",
		})
		return
	}

	// Provisioned but offline: keep NM retrying, arm the sustained-offline window.
	// A transient outage below the window never reaches StateAPActive.
	m.startOfflineWindow()
	m.transition(ctx, StateOfflineRetrying, Detail{
		Reason:  "offline",
		Message: "Reconnecting to Wi-Fi",
	})
}

// onTick fires the sustained-offline window and retries any deferred AP op.
func (m *Machine) onTick(ctx context.Context) {
	m.mu.Lock()
	st := m.state
	since := m.offlineSince
	m.mu.Unlock()

	if st == StateOfflineRetrying && !since.IsZero() &&
		m.clock.Now().Sub(since) >= m.offlineWindow {
		// Wired-link guard, provisioned flavor: the sustained-offline AP exists to
		// fix broken Wi-Fi credentials, but a device on active ethernet is already
		// on its intended path — popping the setup AP would only add noise (and an
		// open WPA2 surface). Re-arm the window instead of raising: if the wire is
		// later unplugged while still offline, the NEXT expiry raises the AP, which
		// keeps the "sustained offline" semantics relative to losing the wire.
		if m.hasWiredLink(ctx) {
			// clearOffline+start = reset to a FRESH window: startOfflineWindow alone
			// is a no-op on an armed window, which would leave a stale expiry that
			// raises the AP instantly the moment the wire disappears.
			m.clearOffline()
			m.startOfflineWindow()
			return
		}
		m.clearOffline()
		m.resetJoinStatus()
		m.transition(ctx, StateAPActive, Detail{
			Reason:  "sustained-offline",
			Message: "Wi-Fi unavailable; starting setup",
		})
		return
	}

	// Idempotent retry: a prior ensureAPUp/Down that failed converges here.
	m.reconcile(ctx)
}

// applyJoin runs the AP-bounce join for a portal submission. Only valid from
// StateAPActive; duplicate/late submissions are ignored.
func (m *Machine) applyJoin(ctx context.Context, ssid, psk string) {
	m.mu.Lock()
	if m.state != StateAPActive {
		st := m.state
		m.mu.Unlock()
		m.logger.Warn("provisioning: join ignored, AP not active", zap.String("state", string(st)))
		return
	}
	m.state = StateJoining
	m.status = portal.Status{State: portal.JoinInProgress, SSID: ssid, Message: "Connecting to " + ssid}
	m.mu.Unlock()
	m.notify(StateJoining, Detail{SSID: ssid, Message: "Connecting to " + ssid})

	// Constraint 2: AP (and its portal) down before the station-mode join.
	m.ensureAPDown(ctx)

	err := m.wifi.Join(ctx, ssid, psk)
	if err == nil {
		m.mu.Lock()
		m.status = portal.Status{State: portal.JoinSucceeded, SSID: ssid, Message: "Connected to " + ssid}
		m.offlineSince = time.Time{}
		m.mu.Unlock()
		m.logger.Info("provisioning: wifi join succeeded", zap.String("ssid", ssid))
		// AP stays down; the device is online.
		m.transition(ctx, StateOnline, Detail{SSID: ssid, Message: "Connected to " + ssid})
		return
	}

	outcome := joinFailureStatus(ssid, err)
	m.mu.Lock()
	m.status = outcome
	m.mu.Unlock()
	m.logger.Warn("provisioning: wifi join failed",
		zap.String("ssid", ssid), zap.String("reason", outcome.Reason), zap.Error(err))
	// Constraint 2/3: re-raise the AP for retry on every failure, auth included.
	m.transition(ctx, StateAPActive, Detail{SSID: ssid, Reason: outcome.Reason, Message: outcome.Message})
}

// transition sets the desired state (notifying on change) and reconciles the
// AP/portal to match. Reconcile runs even when the state is unchanged so a
// previously failed AP operation converges.
func (m *Machine) transition(ctx context.Context, target State, d Detail) {
	m.mu.Lock()
	changed := m.state != target
	m.state = target
	m.mu.Unlock()

	if changed {
		m.notify(target, d)
	}
	m.reconcile(ctx)
}

// reconcile makes the AP/portal match the desired state. Idempotent.
func (m *Machine) reconcile(ctx context.Context) {
	m.mu.Lock()
	st := m.state
	m.mu.Unlock()

	switch st {
	case StateAPActive:
		if err := m.ensureAPUp(ctx); err != nil {
			m.logger.Error("provisioning: failed to raise setup AP; will retry", zap.Error(err))
		}
	case StateOnline, StateUnprovisioned, StateOfflineRetrying:
		m.ensureAPDown(ctx)
	case StateJoining:
		// AP lifecycle is driven inline by applyJoin during a join.
	}
}

// ensureAPUp raises the AP + portal if not already up. Constraint 1:
// RefreshScanCache runs immediately before softap.Up, while station mode still
// owns the radio.
func (m *Machine) ensureAPUp(ctx context.Context) error {
	m.mu.Lock()
	up := m.apUp
	m.mu.Unlock()
	if up {
		return nil
	}

	if _, err := m.wifi.RefreshScanCache(ctx); err != nil {
		// Non-fatal: the portal falls back to manual SSID entry on an empty cache.
		m.logger.Warn("provisioning: pre-AP scan refresh failed", zap.Error(err))
	}

	info, err := m.ap.Up(ctx)
	if err != nil {
		return err
	}

	srv := m.newPortal(portal.Config{
		Addr:   m.portalAddr,
		APSSID: info.SSID,
		Scan:   m.wifi.CachedScan,
		Join:   m.RequestJoin,
		Status: m.Status,
		Logger: m.logger,
	})
	if err := srv.Start(); err != nil {
		// The AP is up but the portal could not bind. Tear the radio hotspot back
		// down immediately so AP+portal stay atomic: apUp is still false here, so a
		// later ensureAPDown would no-op and an AP with no portal behind it would
		// otherwise keep broadcasting indefinitely (it survives daemon shutdown as
		// a persisted NM profile). Tearing down also returns the radio to station
		// mode, so the retry tick's RefreshScanCache honors constraint 1 again.
		// Backend.Down is idempotent, so this is safe even if Up half-failed.
		m.logger.Error("provisioning: portal failed to start; tearing setup AP back down", zap.Error(err))
		if derr := m.ap.Down(ctx); derr != nil {
			m.logger.Warn("provisioning: setup AP down after portal failure also failed", zap.Error(derr))
		}
		return err
	}

	m.mu.Lock()
	m.apUp = true
	m.apInfo = info
	m.portalSrv = srv
	m.mu.Unlock()
	m.logger.Info("provisioning: setup AP raised", zap.String("ssid", info.SSID))

	// Re-announce StateAPActive now that the AP is actually up, carrying the live
	// hotspot credentials. The transition-time notify fired before this point (AP
	// not yet raised, so no SSID/PSK to render); this second announcement is the
	// one a narration surface uses to paint the soft-AP QR. PSK-in-Detail is the
	// signal that "these are the AP credentials" (see Detail.PSK). Fires only on an
	// actual raise, not on every idempotent reconcile, because ensureAPUp returns
	// early above when the AP is already up.
	m.notify(StateAPActive, Detail{
		SSID:    info.SSID,
		PSK:     info.PSK,
		Reason:  "ap-active",
		Message: "Scan the QR code to set up Wi-Fi",
	})
	return nil
}

// ensureAPDown tears the portal + AP down if up. Best-effort and idempotent.
func (m *Machine) ensureAPDown(ctx context.Context) {
	m.mu.Lock()
	up := m.apUp
	srv := m.portalSrv
	m.mu.Unlock()
	if !up {
		return
	}

	if srv != nil {
		stopCtx, cancel := context.WithTimeout(context.Background(), portalStopTimeout)
		if err := srv.Stop(stopCtx); err != nil {
			m.logger.Warn("provisioning: portal stop failed", zap.Error(err))
		}
		cancel()
	}
	if err := m.ap.Down(ctx); err != nil {
		m.logger.Warn("provisioning: setup AP down failed", zap.Error(err))
	}

	m.mu.Lock()
	m.apUp = false
	m.portalSrv = nil
	m.apInfo = softap.Info{}
	m.mu.Unlock()
	m.logger.Info("provisioning: setup AP torn down")
}

// -----------------------------------------------------------------------------
// helpers
// -----------------------------------------------------------------------------

// hasProfile reports whether a saved Wi-Fi profile exists. On an nmcli error it
// biases toward "provisioned" (true) so a transient listing failure defers the
// AP rather than spuriously popping setup on a genuinely provisioned device.
func (m *Machine) hasProfile(ctx context.Context) bool {
	ok, err := m.wifi.HasSavedProfile(ctx)
	if err != nil {
		m.logger.Warn("provisioning: saved-profile check failed; assuming provisioned", zap.Error(err))
		return true
	}
	return ok
}

// hasWiredLink reports whether the optional WiredLink guard sees an active
// ethernet link. A nil guard reports false, preserving the Connectivity-only
// behavior (no wired suppression).
func (m *Machine) hasWiredLink(ctx context.Context) bool {
	if m.wiredLink == nil {
		return false
	}
	return m.wiredLink(ctx)
}

// resetJoinStatus drops any prior join outcome before a FRESH AP raise
// (unprovisioned / sustained-offline). Without it the portal would greet a
// user mid-setup with the success banner of a join that happened weeks ago.
// The join-failure re-raise in applyJoin deliberately keeps its status: the
// phone re-associates and polls /status for exactly that outcome.
func (m *Machine) resetJoinStatus() {
	m.mu.Lock()
	m.status = portal.Status{State: portal.JoinIdle}
	m.mu.Unlock()
}

func (m *Machine) startOfflineWindow() {
	m.mu.Lock()
	if m.offlineSince.IsZero() {
		m.offlineSince = m.clock.Now()
	}
	m.mu.Unlock()
}

func (m *Machine) clearOffline() {
	m.mu.Lock()
	m.offlineSince = time.Time{}
	m.mu.Unlock()
}

// notify publishes a state change to the Notifier, guarded against panics. See
// the Notifier contract: it must not block.
func (m *Machine) notify(s State, d Detail) {
	if m.notifier == nil {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			m.logger.Error("provisioning: notifier panicked", zap.Any("panic", r))
		}
	}()
	m.notifier.OnStateChange(s, d)
}

// joinFailureStatus maps a wifictl join error to a user-facing portal Status.
func joinFailureStatus(ssid string, err error) portal.Status {
	reason := "unknown"
	msg := "Could not connect to that network. Please try again."

	var je *wifictl.JoinError
	if errors.As(err, &je) {
		reason = je.Kind.String()
		switch je.Kind {
		case wifictl.JoinErrAuth:
			msg = "Wrong Wi-Fi password. Please check it and try again."
		case wifictl.JoinErrSSIDNotFound:
			msg = "That network was not found. Move closer or pick another network."
		case wifictl.JoinErrTimeout:
			msg = "The network did not respond in time. Please try again."
		}
	}
	return portal.Status{State: portal.JoinFailed, SSID: ssid, Reason: reason, Message: msg}
}
