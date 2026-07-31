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
	// StateStarting is the pre-assessment sentinel a new Machine holds until
	// the loop's initial connectivity assessment resolves. It exists so that
	// FIRST transition always counts as a change and notifies — initializing
	// to StateOnline made a boot-while-already-online resolve Online→Online
	// with no notification, so nothing downstream (the claim trigger most
	// critically) ever fired after a daemon restart on a healthy device.
	StateStarting State = "starting"
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

// ReasonUnprovisioned marks the ONLINE leg of StateUnprovisioned: sys-monitord
// confirmed WAN reachability and the device merely has no Wi-Fi profile
// (wired). Exported — unlike the narration-only Reason strings — because
// consumers key network-work decisions on it: the wiring notifier runs its
// WAN-dependent hooks only on StateOnline or on THIS reason, so every offline
// leg of StateUnprovisioned (link-present, link-unknown, link-lost, and any
// future one) fails closed by default. Renaming the literal must therefore
// break the build, not silently disarm that positive match.
const ReasonUnprovisioned = "unprovisioned"

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
// GetConnectivityStatus / connectivity_change actually expose. Offline readings
// alone do NOT raise the AP: Config.ActiveLink refines them with link state, so
// only link LOSS (no wire, no Wi-Fi association) reaches the AP path.
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

	// preAPScanAttempts / preAPScanRetryDelay bound the pre-raise scan. The
	// portal picker can only ever offer what this scan saw (the radio cannot
	// scan again once the AP holds it), so a transient scan failure is retried
	// rather than raising the AP over an empty or stale list.
	//
	// A scan that succeeds with ZERO networks is retried on the same footing as
	// an error. Post-AP-bounce that used to be the COMMON case rather than a
	// rare one — nmcli reports "the radio was not ready to scan" as an empty
	// list with exit 0 — but the cause is now handled where it belongs, by
	// wifictl.waitForScanReady gating the scan on the device becoming
	// scannable. This loop stays a backstop for the residue (NM's BSS list
	// still filling in after the mode flip), which is why the attempt count is
	// the modest original: the settling wait is no longer its job. The whole
	// loop runs under the "Looking for nearby Wi-Fi networks" narration with
	// the AP down, so its worst case is time the phone spends stranded.
	preAPScanAttempts   = 3
	preAPScanRetryDelay = 2 * time.Second
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

	// ActiveLink is an optional guard against raising the setup AP on a device
	// that has an active local link (ethernet OR an associated Wi-Fi station) but
	// is reported offline. The Connectivity source is internet-reachability only,
	// so a device whose upstream/router is down looks identical to a truly
	// disconnected one — yet the AP can only fix "cannot associate": re-submitting
	// credentials for a network the device is already on rejoins the same dead
	// network, and the cases the AP genuinely rescues (changed password, vanished
	// SSID) present as link DOWN, not up-but-offline. Raising the AP over a live
	// Wi-Fi association is actively harmful on single-radio hardware: it drops
	// the station link, killing LAN hub control and mDNS on an otherwise healthy
	// LAN. When ActiveLink reports true the machine keeps the AP down (exactly as
	// an online device does).
	//
	// The probe MUST exclude the device's own setup hotspot (production wires
	// status.LinkChecker.ExternalLink with the softap profile name): a leftover
	// hotspot from a failed teardown looks like a connected wifi device but is
	// not an uplink, and counting it would suppress the AP off its own residue.
	// As defense in depth the machine also ignores the guard entirely while it
	// knows its own AP is up (see probeLink).
	//
	// A non-nil error means the link state is UNKNOWN (nmcli failed or timed
	// out), and unknown never authorizes a raise: raising the AP is a one-way
	// door on Wi-Fi (the hotspot takes the radio, so the station can never
	// re-associate on its own), while deferring costs one 15s tick. The machine
	// defers the AP and re-probes on the next tick — the same bias hasProfile
	// applies to a failed nmcli profile listing. Nil disables the guard
	// (preserving the Connectivity-only behavior).
	ActiveLink func(ctx context.Context) (bool, error)

	// PortalAddr is the portal bind address (default ":80").
	PortalAddr string
	// OfflineWindow is how long an offline device must go WITHOUT a confirmed
	// local link before the AP raises (default 5m). It arms at the offline
	// assessment; from then on any tick that sees a link (or gets an
	// inconclusive probe) disarms it and the clock restarts at the first
	// confirmed absence, so past the first tick the window measures
	// continuous confirmed link absence — not time since the internet was
	// lost, and not time since the probe last ran.
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
	ap         softap.Backend
	wifi       WifiController
	conn       Connectivity
	clock      wrapper.Clock
	logger     *zap.Logger
	notifier   Notifier
	activeLink func(ctx context.Context) (bool, error)
	newPortal  func(portal.Config) PortalServer

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
	mu    sync.Mutex
	state State
	// lastReason is the Detail.Reason of the most recent transition() call,
	// notified or not. It exists so transition can detect a REASON change
	// within StateUnprovisioned — whose legs encode WAN reachability — and
	// notify on it; see transition for why a state-change-only notify loses
	// the wired-boot online edge.
	lastReason   string
	status       portal.Status
	apUp         bool
	apInfo       softap.Info
	portalSrv    PortalServer
	offlineSince time.Time
	// apDownPending records a failed softap.Down: the persisted hotspot profile
	// may still exist (possibly still broadcasting) even though apUp is false.
	// ensureAPDown retries the deletion on every reconcile until it succeeds,
	// and a successful softap.Up clears it (Up replaces the profile). Without
	// this flag a Down failure during the join bounce left the profile behind
	// until the next daemon boot's sweep.
	apDownPending bool

	// connUnknown records that the machine is running on an ASSUMED offline
	// state because a reachability query failed (controld starts before
	// sys-monitord, so the boot-time query commonly races the monitor's own
	// startup). Every tick re-queries until one succeeds; without this an
	// ONLINE first boot could raise the setup AP on the failed assumption and
	// keep it up forever — monitord may resolve its initial state before its
	// signal emitters are registered, so no connectivity_change event ever
	// corrects the guess. Touched only on the loop goroutine (loop /
	// onConnectivity / onTick), so it needs no lock.
	connUnknown bool

	// online is the last connectivity reading, kept so onTick can tell an
	// ONLINE unprovisioned device (ethernet steady state — never probe, never
	// raise) from an OFFLINE one (whose link must be re-checked every tick:
	// unplugging the cable of an already-offline device emits no
	// connectivity_change edge, so without the tick probe it would sit in
	// StateUnprovisioned forever instead of raising the AP). Touched only on
	// the loop goroutine, like connUnknown, so it needs no lock.
	online bool

	// linkProbeFailing throttles the failed-probe log: the probe runs every
	// 15s tick in the offline resting states, which are documented as
	// indefinite (air-gapped LAN), so a persistently broken nmcli would
	// otherwise emit 4 warnings/minute forever into logs that ship via
	// uploadLogs. Warn on the transition into failure, Debug on repeats.
	// Loop-goroutine-only, like connUnknown.
	linkProbeFailing bool
}

type eventKind int

const (
	evConnectivity eventKind = iota
	evJoin
	evRescan
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
		activeLink:    cfg.ActiveLink,
		newPortal:     cfg.NewPortal,
		portalAddr:    cfg.PortalAddr,
		offlineWindow: cfg.OfflineWindow,
		checkInterval: cfg.CheckInterval,
		sup:           newSupervisor(cfg.Clock, logger),
		events:        make(chan event, eventBuffer),
		state:         StateStarting,
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
	// Reset AP/portal bookkeeping before anything else. On the first run this is
	// a no-op, but the supervisor reruns loop after a recovered panic with
	// whatever in-memory state the previous run left behind: with a stale
	// apUp=true the sweep below would delete the actual hotspot while every
	// future ensureAPUp early-returns on the flag — a dead AP the machine
	// believes is up, unrecoverable while the device stays offline. Stop any
	// portal listener the prior run still references and forget the raised AP so
	// this run rebuilds AP+portal from scratch.
	m.mu.Lock()
	leftoverSrv := m.portalSrv
	m.apUp = false
	m.portalSrv = nil
	m.apInfo = softap.Info{}
	m.mu.Unlock()
	if leftoverSrv != nil {
		stopCtx, cancel := context.WithTimeout(context.Background(), portalStopTimeout)
		if err := leftoverSrv.Stop(stopCtx); err != nil {
			m.logger.Warn("provisioning: stopping portal left over from a recovered run failed", zap.Error(err))
		}
		cancel()
	}

	// Sweep any leftover setup AP from a previous daemon life. An ungraceful
	// exit (SIGKILL, panic, power cut) leaves the persisted ff1-softap NM
	// profile behind — possibly still broadcasting — while this process boots
	// believing apUp=false, so ensureAPDown would never touch it. A fresh boot
	// never legitimately starts with our AP up, and the sweep must run BEFORE
	// the first pre-AP scan so the radio is back in station mode (constraint 1).
	if err := m.ap.Down(ctx); err != nil {
		m.logger.Warn("provisioning: boot-time sweep of leftover setup AP failed", zap.Error(err))
		m.mu.Lock()
		m.apDownPending = true
		m.mu.Unlock()
	} else {
		m.mu.Lock()
		m.apDownPending = false
		m.mu.Unlock()
	}

	unsub := m.conn.Subscribe(func(online bool) {
		select {
		case m.events <- event{kind: evConnectivity, online: online}:
		case <-ctx.Done():
		}
	})
	defer unsub()

	ticker := m.clock.NewTicker(m.checkInterval)
	defer ticker.Stop()

	// Initial assessment from a point-in-time reachability query. A failed
	// query is an ASSUMPTION, not a reading: mark it unknown so onTick keeps
	// re-querying until a real answer arrives (see connUnknown).
	online, err := m.conn.Online(ctx)
	if err != nil {
		m.logger.Warn("provisioning: initial connectivity query failed; assuming offline until a query succeeds", zap.Error(err))
		online = false
	}
	m.onConnectivity(ctx, online)
	if err != nil {
		// AFTER onConnectivity: it clears the flag (any assessment normally
		// resolves it), and this one did not.
		m.connUnknown = true
	}

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
			case evRescan:
				m.applyRescan(ctx)
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

// RequestRescan is the portal's RescanFunc: bounce the AP so a fresh
// station-mode scan can run (the radio cannot scan while the AP holds it).
// Like RequestJoin it returns immediately and the bounce runs on the loop
// goroutine, because taking the AP down drops the phone that pressed the
// button; the phone re-associates via the QR code and reloads the picker.
func (m *Machine) RequestRescan() error {
	select {
	case m.events <- event{kind: evRescan}:
		return nil
	default:
		m.logger.Warn("provisioning: rescan queue full, dropping request")
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
	// Any reading — a connectivity_change event or a successful re-query —
	// resolves an assumed-offline boot. (The loop re-arms the flag itself for
	// the one call it makes on a FAILED initial query.)
	m.connUnknown = false
	m.online = online
	if online {
		m.clearOffline()
		if m.hasProfile(ctx) {
			m.transition(ctx, StateOnline, Detail{Message: "Connected to the network"})
		} else {
			// Reachable without a saved Wi-Fi profile: Ethernet. Never raise the AP.
			m.transition(ctx, StateUnprovisioned, Detail{
				Reason:  ReasonUnprovisioned,
				Message: "Online via wired network; Wi-Fi not configured",
			})
		}
		return
	}

	// Offline.
	if !m.hasProfile(ctx) {
		// Link guard: a device with an active local link — in this no-profile
		// branch that is ethernet in practice, since NM cannot hold a station
		// association without a profile and the probe excludes our own hotspot
		// — is reachable on its LAN even when sys-monitord reports offline
		// (e.g. a WAN/router blip). It must never pop the setup AP, exactly as
		// an ONLINE device never does. Treat it as unprovisioned-but-linked
		// (AP stays down). Only a CONFIRMED absence of any link raises the AP;
		// a failed probe defers to the tick, which re-probes StateUnprovisioned
		// while offline (see onTick).
		m.mu.Lock()
		cur := m.state
		m.mu.Unlock()
		switch m.probeLink(ctx) {
		case linkPresent:
			m.clearOffline()
			m.transition(ctx, StateUnprovisioned, Detail{
				Reason:  "link-present",
				Message: "Network link present; Wi-Fi not configured",
			})
			return
		case linkUnknown:
			// A failed probe must never EVICT an active AP: after a failed
			// raise (state APActive with apUp false) the apUp short-circuit
			// does not apply, and bouncing APActive → Unprovisioned →
			// APActive at event/tick cadence would flap the on-screen setup
			// narration and spawn claim-flow goroutines each round trip.
			// Reconcile keeps retrying the raise instead; a CONFIRMED link
			// reading (either way) still resolves the state normally.
			if cur == StateAPActive {
				m.reconcile(ctx)
				return
			}
			m.clearOffline()
			m.transition(ctx, StateUnprovisioned, Detail{
				Reason:  "link-unknown",
				Message: "Checking the network link",
			})
			return
		case linkAbsent:
			// Confirmed absence. Raise immediately only where deferring has no
			// value: the boot assessment (StateStarting — a fresh device with
			// no profile and no link needs the AP right away) and StateAPActive
			// (the raise below is a state no-op that keeps a failed raise
			// retrying). Every other confirmed-absent reading routes through
			// the continuous-confirmed-absence window the tick measures — the
			// online→offline edge included, because a 20s LAN-switch reboot
			// must not flash setup over artwork on an ONLINE wired frame any
			// more than on a parked one (#233), and redundant offline
			// re-emissions (a sys-monitord restart re-emits its first probe
			// unconditionally; the connUnknown re-query feeds one in) must not
			// jump a window already running. This probe counts as ONE
			// confirmed absence: it arms the window if it is the first, and
			// only the tick — a full window later — raises. With no guard
			// wired there is no probe to confirm anything (probeLink
			// fabricates the absence), so the nil-guard baseline keeps the
			// original immediate raise.
			if m.activeLink != nil && cur != StateStarting && cur != StateAPActive {
				m.startOfflineWindow()
				// From StateUnprovisioned (the common case) this notifies only
				// on the LEG change into link-lost; the redundant confirmed-
				// absent re-emissions that merely re-feed the running window
				// (same state, same reason) stay suppressed — see transition.
				m.transition(ctx, StateUnprovisioned, Detail{
					Reason:  "link-lost",
					Message: "Network link lost; watching for it to return",
				})
				return
			}
		}

		// Unprovisioned AND confirmed link-less: the device cannot self-heal,
		// so raise the AP immediately.
		m.clearOffline()
		m.resetJoinStatus()
		m.transition(ctx, StateAPActive, Detail{
			Reason:  "unprovisioned",
			Message: "Set up Wi-Fi to continue",
		})
		return
	}

	// Provisioned but offline. A redundant offline reading arriving while the
	// setup AP is up carries NO new information — the hotspot holds the radio,
	// so the device is offline by definition — but falling through to
	// StateOfflineRetrying would reconcile the AP back DOWN, dropping the
	// portal out from under a phone mid-setup and costing another full
	// sustained-offline window before it returns. Such readings are routine
	// (a sys-monitord restart re-emits its first probe unconditionally; the
	// connUnknown re-query feeds one in after an assumed-offline boot). This
	// is the provisioned twin of the apUp short-circuit in probeLink, which
	// only ever protected the unprovisioned path.
	//
	// One APActive flavor may exit here: a FAILED/PENDING raise (apUp false)
	// whose link has recovered. No hotspot is up for the probe to mistake for
	// an uplink there, and the station can genuinely re-associate while NM
	// keeps refusing the raise — letting a later retry succeed would drop
	// that recovered link, the exact harm the guard exists to prevent.
	// probeLink short-circuits to linkAbsent while the AP is actually up, so
	// this exit can never evict a RAISED AP; a raised StateAPActive is left
	// only by going online or by a portal join. Anything short of a
	// confirmed sighting reconciles instead, so a failed raise keeps
	// retrying.
	m.mu.Lock()
	cur := m.state
	m.mu.Unlock()
	if cur == StateAPActive {
		if m.probeLink(ctx) == linkPresent {
			m.clearOffline()
			m.transition(ctx, StateOfflineRetrying, Detail{
				Reason:  "link-present",
				Message: "Reconnecting to Wi-Fi",
			})
			return
		}
		m.reconcile(ctx)
		return
	}

	// Keep NM retrying, and arm the sustained-offline window — but only when
	// no link guard is wired. That entry arming is the nil-guard baseline
	// ("5m from the offline event"); with a guard, the clock must start at the
	// first CONFIRMED link absence, which cannot be known here. Arming at this
	// event instead of at that first absent probe would let the window expire
	// after one tick less than its full length of confirmed absence (the first
	// probe runs a tick after this reading) — the contract is continuous
	// confirmed absence, so onTick arms it on the first linkAbsent result.
	if m.activeLink == nil {
		m.startOfflineWindow()
	}
	m.transition(ctx, StateOfflineRetrying, Detail{
		Reason:  "offline",
		Message: "Reconnecting to Wi-Fi",
	})
}

// onTick fires the sustained-offline window and retries any deferred AP op.
func (m *Machine) onTick(ctx context.Context) {
	// Recover from an assumed-offline boot: re-query reachability until one
	// query succeeds (a connectivity_change event also resolves it). Runs
	// BEFORE the window/reconcile logic so a corrected reading drives this
	// same tick's decisions.
	if m.connUnknown {
		online, err := m.conn.Online(ctx)
		if err != nil {
			m.logger.Warn("provisioning: connectivity re-query failed; still assuming offline", zap.Error(err))
		} else {
			m.logger.Info("provisioning: connectivity re-query resolved the assumed-offline boot", zap.Bool("online", online))
			m.onConnectivity(ctx, online)
		}
	}

	m.mu.Lock()
	st := m.state
	since := m.offlineSince
	m.mu.Unlock()

	switch st {
	case StateOfflineRetrying:
		// Link guard, provisioned flavor: the sustained-offline AP exists to
		// fix "cannot associate" (changed password, vanished SSID — both
		// present as link DOWN). A device still holding a link — ethernet, or
		// a Wi-Fi association whose WAN died — is on its intended path and
		// stays reachable on its LAN; raising the AP would drop that link on
		// single-radio hardware and cannot fix an upstream outage anyway.
		//
		// The probe runs EVERY tick, not just at expiry; a sighting of a link
		// (or an inconclusive probe) disarms the window and the clock
		// restarts at the next confirmed absence. That is what makes
		// OfflineWindow measure continuous LINK absence: sampling only at
		// expiry would let an association lost moments before the deadline
		// raise the AP after seconds of link loss — and on Wi-Fi that raise
		// is unrecoverable without a human (the hotspot takes the radio, so
		// reachability can never return on its own to tear it down).
		switch m.probeLink(ctx) {
		case linkPresent, linkUnknown:
			// A sighting DISARMS the window entirely; the next confirmed
			// absence arms a fresh one below. Disarm-then-rearm (rather than
			// re-arming to "now" here) matters twice over: a stale armed
			// window would raise the AP instantly the moment the link
			// disappears, and re-arming to the sighting time would let a tick
			// gap (a stalled loop, coarse test clocks) satisfy the expiry off
			// a SINGLE absent sample — the window must measure confirmed
			// absence, not time since the probe last ran. Fall through to
			// reconcile rather than returning: a pending AP-profile teardown
			// (apDownPending) must keep retrying on guarded ticks too.
			m.clearOffline()
		case linkAbsent:
			switch {
			case since.IsZero():
				// First confirmed absence since the last sighting (or since
				// entry): start the clock. The raise below needs the window
				// to elapse between this arming and a later absent tick, so
				// the AP only ever rises off repeated confirmations.
				m.startOfflineWindow()
			case m.clock.Now().Sub(since) >= m.offlineWindow:
				m.clearOffline()
				m.resetJoinStatus()
				m.transition(ctx, StateAPActive, Detail{
					Reason:  "sustained-offline",
					Message: "Wi-Fi unavailable; starting setup",
				})
				return
			}
		}

	case StateUnprovisioned:
		// An OFFLINE unprovisioned device parked here by the link guard emits
		// no further connectivity event when its cable is later unplugged (it
		// is already offline, so there is no edge) — without this tick probe
		// it would sit here forever instead of raising the AP. ONLINE
		// unprovisioned devices (ethernet steady state) are skipped entirely:
		// no 15s nmcli probe in the healthy case, and losing that link
		// surfaces as a connectivity_change event instead.
		//
		// Link loss detected here gets the SAME continuous-confirmed-absence
		// window as the provisioned flavor, not the immediate raise the
		// connectivity-event path applies: a 20s LAN-switch reboot on an
		// air-gapped wired frame must not pop the AP, because once the AP is
		// up nothing can lower it while the device stays offline (there is
		// deliberately no link-based exit from StateAPActive — the probe
		// cannot be trusted to distinguish a returned wire from the machine's
		// own hotspot under every wiring, so exits are online events and
		// portal joins only). The immediate-raise rule survives only at the
		// boot assessment (StateStarting), where a fresh link-less device
		// needs the AP right away; onConnectivity routes every later
		// confirmed-absent reading — online→offline edges and redundant
		// re-emissions alike — to this windowed path instead.
		if m.online {
			break
		}
		switch m.probeLink(ctx) {
		case linkPresent, linkUnknown:
			// Same disarm-on-sighting as above: the next confirmed absence
			// starts a fresh clock.
			m.clearOffline()
		case linkAbsent:
			switch {
			case since.IsZero():
				// First confirmed absence after being parked (entry paths
				// clear the window): start the clock; the raise needs a full
				// window of absence, at tick granularity.
				m.startOfflineWindow()
			case m.clock.Now().Sub(since) >= m.offlineWindow:
				// Re-check the profile at the raise boundary: a profile
				// acquired out-of-band while parked (e.g. a hub-driven join
				// attempt) makes this the PROVISIONED flavor — NM can retry
				// it, so hand the device the provisioned resting state and a
				// fresh window rather than an unprovisioned-style raise.
				if m.hasProfile(ctx) {
					m.clearOffline()
					m.startOfflineWindow()
					m.transition(ctx, StateOfflineRetrying, Detail{
						Reason:  "offline",
						Message: "Reconnecting to Wi-Fi",
					})
					return
				}
				m.clearOffline()
				m.resetJoinStatus()
				m.transition(ctx, StateAPActive, Detail{
					Reason:  "unprovisioned",
					Message: "Set up Wi-Fi to continue",
				})
				return
			}
		}

	case StateAPActive:
		// A FAILED/PENDING raise (apUp false) is the one APActive flavor a
		// link reading may exit: no hotspot is up for the probe to mistake
		// for an uplink, and the link can genuinely recover while NM keeps
		// refusing the raise — on an air-gapped LAN no connectivity event
		// will ever arrive, so without this probe the eventual retry would
		// SUCCEED and drop the recovered link, the exact harm the guard
		// exists to prevent. probeLink short-circuits to linkAbsent while
		// the AP is actually up (no nmcli spend, and the no-link-based-exit
		// rule for a RAISED AP is preserved); absent/unknown fall through to
		// reconcile, which keeps retrying the raise. The saved-profile check
		// routes the exit to the matching resting state, whose tick probes
		// re-arm the sustained window before any fresh raise.
		if m.probeLink(ctx) == linkPresent {
			m.clearOffline()
			if m.hasProfile(ctx) {
				m.transition(ctx, StateOfflineRetrying, Detail{
					Reason:  "link-present",
					Message: "Reconnecting to Wi-Fi",
				})
			} else {
				m.transition(ctx, StateUnprovisioned, Detail{
					Reason:  "link-present",
					Message: "Network link present; Wi-Fi not configured",
				})
			}
			return
		}
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

	// Constraint 2: AP (and its portal) down before the station-mode join. A
	// failed teardown aborts the attempt: joining with the hotspot possibly
	// still holding the radio violates the single-radio sequencing, and a
	// success would strand the leftover profile with nothing retrying its
	// deletion. Re-raising via StateAPActive self-heals instead — softap.Up
	// replaces the profile — and the phone polls /status for this outcome.
	if !m.ensureAPDown(ctx) {
		outcome := portal.Status{
			State:   portal.JoinFailed,
			SSID:    ssid,
			Reason:  "ap-teardown-failed",
			Message: "The device could not release its setup hotspot. Please try again.",
		}
		m.mu.Lock()
		m.status = outcome
		m.mu.Unlock()
		m.logger.Warn("provisioning: join aborted, setup AP teardown failed", zap.String("ssid", ssid))
		m.transition(ctx, StateAPActive, Detail{SSID: ssid, Reason: outcome.Reason, Message: outcome.Message})
		return
	}

	err := m.wifi.Join(ctx, ssid, psk)
	if err == nil {
		m.mu.Lock()
		m.status = portal.Status{State: portal.JoinSucceeded, SSID: ssid, Message: "Connected to " + ssid}
		m.mu.Unlock()
		m.logger.Info("provisioning: wifi join succeeded", zap.String("ssid", ssid))
		// Association is NOT reachability: joining a network with a dead
		// upstream must not park the machine in StateOnline — the device was
		// offline before the join and stays offline after it, so sys-monitord
		// emits no change event and nothing would ever correct the state (AP
		// down, offline window cleared, setup stranded). Route the outcome
		// through the same assessment the loop's startup uses: online confirms
		// StateOnline; offline files the device as provisioned-but-offline
		// with the AP down. From there the AP returns only after a full
		// window of confirmed LINK absence — an association that survives
		// (air-gapped LAN, dead WAN) keeps the AP down indefinitely by
		// design, because re-submitting credentials cannot fix a dead
		// upstream (see Config.ActiveLink). The /status outcome above stays
		// JoinSucceeded either way — the association DID succeed, which is
		// the portal's contract.
		online, oerr := m.conn.Online(ctx)
		if oerr != nil {
			m.logger.Warn("provisioning: post-join connectivity query failed; assuming offline until a query succeeds", zap.Error(oerr))
			online = false
		}
		m.onConnectivity(ctx, online)
		if oerr != nil {
			// Same assumed-vs-read discipline as the boot-time query (and the
			// same AFTER-onConnectivity ordering, since it clears the flag):
			// sys-monitord being briefly unavailable during the join must not
			// become a permanent offline verdict that raises the AP over a
			// working uplink after the offline window.
			m.connUnknown = true
		}
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

// applyRescan bounces the AP purely to refresh the scan cache: the radio can
// only scan in station mode, so the portal's "search for networks again"
// button costs one AP restart and the phone must re-associate. Only valid from
// StateAPActive; requests in any other state are ignored (a join in flight or
// an online device has no picker to refresh).
func (m *Machine) applyRescan(ctx context.Context) {
	m.mu.Lock()
	if m.state != StateAPActive {
		st := m.state
		m.mu.Unlock()
		m.logger.Warn("provisioning: rescan ignored, AP not active", zap.String("state", string(st)))
		return
	}
	// The bounce starts a fresh portal session; a join outcome from before it
	// would be stale on the page the re-associated phone loads. Reset directly:
	// resetJoinStatus is edge-gated on entering StateAPActive, which a rescan
	// never leaves.
	m.status = portal.Status{State: portal.JoinIdle}
	m.mu.Unlock()

	m.logger.Info("provisioning: rescan requested; bouncing setup AP")
	// Flip the screen to "scanning" IMMEDIATELY: the AP teardown below takes
	// seconds, and leaving the join QR up through it reads as a stalled button
	// press. ensureAPUp re-announces the same state when the fresh scan
	// actually starts; setupui coalesces the duplicate.
	m.notify(StateAPActive, Detail{
		Reason:  "scanning",
		Message: "Looking for nearby Wi-Fi networks",
	})
	m.ensureAPDown(ctx)
	// State is still StateAPActive, so reconcile re-raises via ensureAPUp: it
	// narrates the scan on-screen, completes a fresh scan pass, then brings the
	// AP (and its QR narration) back.
	m.reconcile(ctx)
}

// transition sets the desired state (notifying on change) and reconciles the
// AP/portal to match. Reconcile runs even when the state is unchanged so a
// previously failed AP operation converges.
//
// "Change" is the state — plus, within StateUnprovisioned only, the REASON:
// that state's legs encode WAN reachability (ReasonUnprovisioned is the online
// leg; link-* are offline parking legs), and the notifier's online-triggered
// hooks (claim QR, startup OTA gate, boot player recovery) hang off exactly
// this notification. A wired device that boots offline parks on a link-* leg;
// when the WAN probe later succeeds, onConnectivity re-targets the SAME state
// with ReasonUnprovisioned — a state-change-only notify swallows that edge and
// none of the hooks ever run for the boot. Same-state SAME-reason re-emissions
// (sys-monitord restarts re-emit their first probe; the tick re-probes while
// offline) stay suppressed, which is what the link-lost window path relies on.
func (m *Machine) transition(ctx context.Context, target State, d Detail) {
	m.mu.Lock()
	changed := m.state != target ||
		(target == StateUnprovisioned && m.lastReason != d.Reason)
	m.state = target
	m.lastReason = d.Reason
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

	// A pending teardown means the previous hotspot profile may still exist —
	// possibly still owning the radio. Resolve it BEFORE anything else: the
	// pre-AP scan needs station mode (constraint 1), and softap.Up refuses to
	// create over an undeletable leftover (the duplicate-profile hazard), so
	// scanning or raising before the deletion succeeds is wasted work at best
	// and a radio-mode violation at worst. apUp is false here, so this call
	// only retries the outstanding deletion; the tick retries this whole raise
	// until it converges.
	if !m.ensureAPDown(ctx) {
		return errors.New("previous setup AP teardown still pending; deferring raise")
	}

	// Announce the scan before it starts so the narration surface can show a
	// "looking for networks" state for however long the scan takes. Reason
	// "scanning" is the discriminator (see setupNotifier.OnStateChange); the
	// AP only comes up after the scan pass below completes.
	m.notify(StateAPActive, Detail{
		Reason:  "scanning",
		Message: "Looking for nearby Wi-Fi networks",
	})

	var scanErr error
	var ssids []string
	for attempt := 1; attempt <= preAPScanAttempts; attempt++ {
		ssids, scanErr = m.wifi.RefreshScanCache(ctx)
		if scanErr == nil && len(ssids) > 0 {
			break
		}
		m.logger.Warn("provisioning: pre-AP scan produced no usable result",
			zap.Int("attempt", attempt), zap.Int("max", preAPScanAttempts),
			zap.Int("ssids", len(ssids)), zap.Error(scanErr))
		if attempt < preAPScanAttempts {
			if err := m.clock.SleepContext(ctx, preAPScanRetryDelay); err != nil {
				break
			}
		}
	}
	if scanErr != nil || len(ssids) == 0 {
		// Non-fatal after retries: withholding the AP entirely would strand the
		// user. The portal serves whatever the cache still holds — wifictl keeps
		// the previous entries rather than letting an empty scan blank them —
		// and falls back to manual SSID entry only when there is nothing left.
		m.logger.Warn("provisioning: proceeding without a fresh scan result",
			zap.Error(scanErr))
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
		Rescan: m.RequestRescan,
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
			m.mu.Lock()
			m.apDownPending = true
			m.mu.Unlock()
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

// ensureAPDown tears the portal + AP down if up, and reports whether the AP
// side is known clean (profile deleted or never up). Idempotent. A failed
// softap.Down still clears apUp/portalSrv — the portal IS stopped and the pair
// must be re-raised together — but latches apDownPending so the next call
// (every reconcile hits one) retries the profile deletion until it succeeds.
func (m *Machine) ensureAPDown(ctx context.Context) bool {
	m.mu.Lock()
	up := m.apUp
	srv := m.portalSrv
	pending := m.apDownPending
	m.mu.Unlock()
	if !up {
		if !pending {
			return true
		}
		// A prior teardown failed with the portal already stopped: only the
		// profile deletion is outstanding. Retry just that.
		if err := m.ap.Down(ctx); err != nil {
			m.logger.Warn("provisioning: retry of setup AP profile deletion failed", zap.Error(err))
			return false
		}
		m.mu.Lock()
		m.apDownPending = false
		m.mu.Unlock()
		m.logger.Info("provisioning: leftover setup AP profile deleted on retry")
		return true
	}

	if srv != nil {
		stopCtx, cancel := context.WithTimeout(context.Background(), portalStopTimeout)
		if err := srv.Stop(stopCtx); err != nil {
			m.logger.Warn("provisioning: portal stop failed", zap.Error(err))
		}
		cancel()
	}
	downOK := true
	if err := m.ap.Down(ctx); err != nil {
		m.logger.Warn("provisioning: setup AP down failed; will retry deletion", zap.Error(err))
		downOK = false
	}

	m.mu.Lock()
	m.apUp = false
	m.portalSrv = nil
	m.apInfo = softap.Info{}
	m.apDownPending = !downOK
	m.mu.Unlock()
	if downOK {
		m.logger.Info("provisioning: setup AP torn down")
	}
	return downOK
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

// linkProbe is the tri-state outcome of one ActiveLink reading. The third
// state exists because the two errors have opposite costs: treating a failed
// probe as "absent" raises the AP — a one-way door on Wi-Fi that drops a
// possibly-healthy association — while treating it as unknown merely defers
// the decision one 15s tick.
type linkProbe int

const (
	// linkAbsent: the probe confirmed no external link. The only reading that
	// can authorize raising the AP.
	linkAbsent linkProbe = iota
	// linkPresent: an external link (ethernet or Wi-Fi station) is up.
	linkPresent
	// linkUnknown: the probe failed; defer any AP decision to the next tick.
	linkUnknown
)

// probeLink runs the optional ActiveLink guard. A nil guard reports absent,
// preserving the Connectivity-only behavior (no link suppression, AP raises as
// before).
//
// The machine's OWN setup AP never counts as a link. The production probe
// (status.LinkChecker.ExternalLink) already excludes the hotspot by its NM
// profile name — which also covers the apDownPending window where a leftover
// hotspot is still connected while apUp is false — but the apUp short-circuit
// stays as defense in depth for naive wirings: with a probe that matches the
// hotspot's wifi device, a redundant offline reading arriving with the AP
// raised (a monitord restart re-emits its first probe unconditionally; the
// connUnknown re-query path feeds one in after an assumed-offline boot) would
// take the link-present branch, transition away from StateAPActive and tear
// down the AP mid-setup — with no further connectivity event to ever re-raise
// it. The hotspot holds the radio; it is not an uplink.
func (m *Machine) probeLink(ctx context.Context) linkProbe {
	if m.activeLink == nil {
		return linkAbsent
	}
	m.mu.Lock()
	apUp := m.apUp
	m.mu.Unlock()
	if apUp {
		return linkAbsent
	}
	up, err := m.activeLink(ctx)
	if err != nil {
		if m.linkProbeFailing {
			m.logger.Debug("provisioning: link probe still failing; link state remains unknown", zap.Error(err))
		} else {
			m.linkProbeFailing = true
			m.logger.Warn("provisioning: link probe failed; treating link state as unknown", zap.Error(err))
		}
		return linkUnknown
	}
	m.linkProbeFailing = false
	if up {
		return linkPresent
	}
	return linkAbsent
}

// resetJoinStatus drops any prior join outcome before a FRESH AP raise
// (unprovisioned / sustained-offline). Without it the portal would greet a
// user mid-setup with the success banner of a join that happened weeks ago.
// The join-failure re-raise in applyJoin deliberately keeps its status: the
// phone re-associates and polls /status for exactly that outcome. That is why
// the reset is edge-gated on state: the unprovisioned-offline branch is
// level-triggered, and a redundant offline event while the AP is already up
// (e.g. wlan churn during the post-failure re-raise) must not wipe the
// outcome the phone is about to poll for.
func (m *Machine) resetJoinStatus() {
	m.mu.Lock()
	if m.state != StateAPActive {
		m.status = portal.Status{State: portal.JoinIdle}
	}
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
