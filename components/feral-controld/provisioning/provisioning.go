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

// StateOfflineRetrying entry-edge reasons (F-01 / ux-must-fix M-0/M-1).
// Exported for the same build-breakage rationale as ReasonUnprovisioned: the
// wiring notifier narrates ONLY these reasons on StateOfflineRetrying — every
// other reason ("offline", "link-present", and any future one) deliberately
// leaves the screen as-is, because on the exhibition online→offline edge
// "keep showing artwork" is the correct behavior. These edges are different:
// each is an entry into a wait that is otherwise a silent black screen for
// minutes (or forever), on a device whose user is actively watching.
const (
	// ReasonBootOffline: boot assessment found a saved profile but a confirmed
	// absent link — the classic "frame moved to a new site" shape when the
	// scan still (or inconclusively) shows a saved SSID.
	ReasonBootOffline = "boot-offline"
	// ReasonBootNoInternet: boot assessment found a live link but no WAN —
	// the network exists and is joined, its upstream is dead. The AP will
	// deliberately never rise while the link holds, so the narration must not
	// promise setup mode.
	ReasonBootNoInternet = "boot-no-internet"
	// ReasonBootLinkUnknown: boot assessment could not read the link state;
	// conservative wording only (no assertions about the network).
	ReasonBootLinkUnknown = "boot-link-unknown"
	// ReasonJoinedNoInternet: a portal join ASSOCIATED but the post-join
	// reachability query confirmed no internet (F-01's captive/hotel Wi-Fi
	// shape). The association is real, so the AP stays down by design.
	ReasonJoinedNoInternet = "joined-no-internet"
	// ReasonJoinedConnUnknown: a portal join associated but the reachability
	// QUERY FAILED — offline is an assumption here, so the wording must hedge
	// ("checking…") rather than assert a dead network. Accepted gap: when a
	// later re-query confirms offline the machine is already in
	// StateOfflineRetrying, and the same-state transition does not re-notify
	// (see transition), so the hedge is the terminal narration for this leg.
	ReasonJoinedConnUnknown = "joined-conn-unknown"
	// ReasonRelocated marks the StateAPActive raise taken by the boot
	// relocation shortcut: saved profile + confirmed absent link + a live
	// station scan that positively shows NO saved SSID in range. Waiting out
	// the sustained-offline window cannot self-heal that, so the AP rises
	// immediately, mirroring the unprovisioned boot raise.
	ReasonRelocated = "relocated"
	// ReasonSetupError marks the escalation-latch notification (§4.6): a
	// persistent AP raise or teardown failure the machine cannot resolve on
	// its own. Detail.Message carries the user-facing prose; the notifier
	// renders it via the player's setup_error extension state. Emitted once at
	// the latch edge, never per retry tick.
	ReasonSetupError = "setup-error"
	// ReasonSetupIncomplete marks the §4.1 setup-incomplete raise: an
	// unclaimed device confirmed link-present + WAN-offline + not wired for a
	// full episode window, with no app contact. The AP returns so an app-less
	// user can re-enter setup.
	ReasonSetupIncomplete = "setup-incomplete"
	// ReasonUserRequested marks an app-triggered raise (startWifiSetup —
	// docs/app-triggered-wifi-setup.md). Named here because the §4.2 session
	// policy table keys on it; the command lands in a later stage.
	ReasonUserRequested = "user-requested"
	// ReasonAPSessionEnded is the narrated teardown landing for an unclaimed
	// frame's bounded AP session (episode AP phases, abandoned user-requested
	// sessions): constraint 4 — every teardown repaints, and this one carries
	// the station-phase prose.
	ReasonAPSessionEnded = "ap-session-ended"
	// ReasonAPSessionEndedSilent is the claimed-frame teardown landing and the
	// narrated→silent rewrite reason (constraint 4(b)): the notifier answers
	// it with a narrating-guarded hide so artwork returns instead of a stale
	// setup panel stranding over an exhibition.
	ReasonAPSessionEndedSilent = "ap-session-ended-silent"
	// ReasonAPRecheck narrates the recheck-cadence blink (both claim states):
	// the AP is briefly down while saved profiles are force-reactivated
	// against a fresh scan; the QR returns within minutes if the network is
	// still gone.
	ReasonAPRecheck = "ap-recheck"
	// ReasonSetupIncompleteSettled is the episode's terminal narration after
	// its raise cycles exhaust: the machine settles in STATION mode (the LAN
	// escape lives there) with a standing diagnosis.
	ReasonSetupIncompleteSettled = "setup-incomplete-settled"
	// ReasonSetupErrorCleared marks a latched setup_error ending WITHOUT its
	// own repaint: a resolved teardown wedge in a resting state, or a raise
	// episode exiting through link recovery before any raise succeeded (a
	// successful raise repaints softap_qr by itself and never emits this).
	// The notifier answers it with a narrating-guarded hide, so the error
	// panel cannot strand over artwork.
	ReasonSetupErrorCleared = "setup-error-cleared"
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
	// SavedWifiSSIDs returns the SSID each saved profile actually targets
	// (read from the profile, not its name) and whether any targets a hidden
	// network. The relocation check treats anyHidden as unverifiable: hidden
	// SSIDs never appear in scans, so their absence is not evidence.
	SavedWifiSSIDs(ctx context.Context) (ssids []string, anyHidden bool, err error)
	// ScanAllSSIDs is a forced live scan with NO display cap — the relocation
	// check reads scan absence as evidence for a one-way AP raise, and a
	// truncated list would fabricate absence for a weak in-range network.
	ScanAllSSIDs(ctx context.Context) ([]string, error)
	RefreshScanCache(ctx context.Context) ([]string, error)
	CachedScan(ctx context.Context) ([]string, error)
	// Join must honor ctx cancellation: applyJoin bounds it with a deadline
	// (Config.JoinTimeout) because it is the one long blocking call on the
	// loop goroutine — an implementation that ignores ctx wedges the machine
	// with the AP down and the portal stopped (D10). hidden requests directed
	// probing for a non-broadcasting SSID.
	Join(ctx context.Context, ssid, psk string, hidden bool) error
	// ActivationProfiles / ActivateProfile serve the recheck blink's forced
	// reactivation (§4.2): passive autoconnect after an AP→station flip is
	// unreliable, so the blink activates in-range saved profiles explicitly.
	// Both must honor ctx (the blink runs under a hard ceiling).
	ActivationProfiles(ctx context.Context) ([]wifictl.ActivationProfile, error)
	ActivateProfile(ctx context.Context, uuid string) error
	// SetDeviceAutoconnect flips the DEVICE-level runtime autoconnect flag,
	// gating only NM's OWN activations — explicit Join/ActivateProfile calls
	// stay effective while it is off. applyRescan is the sole consumer (see
	// its suppression block); the flag is runtime-only, so an NM restart or a
	// reboot restores it regardless of what this machine did. Must honor ctx.
	SetDeviceAutoconnect(ctx context.Context, enabled bool) error
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
	// defaultJoinTimeout bounds wifi.Join, the one long blocking call on the
	// loop goroutine: nmcli's own `--wait` default is 90s, and the margin
	// covers the pre-connect visibility wait plus profile cleanup. Without it
	// a wedged NM activation blocks the machine forever with the AP down and
	// the portal stopped (D10).
	defaultJoinTimeout = 120 * time.Second
	portalStopTimeout  = 3 * time.Second
	// shutdownRestoreTimeout bounds the shutdown autoconnect restore. It runs on
	// a FRESH context (the loop's is already canceled by then), so without a
	// bound of its own a wedged nmcli would hold shutdown open indefinitely.
	// The bound is also what makes it safe to order the restore AHEAD of the
	// shutdown AP teardown, which is unbounded by design (a leftover hotspot
	// profile is persistent and must be deleted): a wedged deletion can still
	// stall shutdown, but it can no longer starve the restore.
	shutdownRestoreTimeout = 5 * time.Second
	eventBuffer            = 16

	// offlineFreshSamplesAfterPause is how many fresh confirmed-absent link
	// samples the offline window demands after ANY inconclusive pause before
	// its expiry may fire. The window pauses (rather than resets) on
	// inconclusive probes, so its accumulated evidence can be arbitrarily old
	// when probing recovers — and a ONE-sample raise off that stale
	// accumulation is exactly the hazard the old reset-on-unknown behavior
	// existed to prevent. Two consecutive fresh samples keep the raise
	// grounded in current evidence at one tick of extra cost.
	offlineFreshSamplesAfterPause = 2

	// setupErrorFailStreak is how many CONSECUTIVE AP-raise (or AP-teardown)
	// failures latch the on-screen setup_error narration: ~2 minutes at tick
	// cadence — above transient NetworkManager restarts, an order of magnitude
	// below "the user walks away". Below the threshold the screen keeps
	// whatever the flow last painted (usually "scanning"), which is honest for
	// a transient; past it, "scanning" forever is the D5/D6 lying screen the
	// latch exists to replace.
	setupErrorFailStreak = 8

	// relocUnknownTolerance is how many inconclusive LINK PROBE samples the
	// boot relocation ladder absorbs before forfeiting for the boot. The old
	// behavior forfeited on the FIRST linkUnknown (one 3s nmcli timeout killed
	// the feature for the whole boot — D7); tolerating two keeps a flaky probe
	// from starving it while still forfeiting quickly when the probe is
	// genuinely broken, because the ladder's scan evidence is only as fresh as
	// the link probes interleaved with it.
	relocUnknownTolerance = 2

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

	// BootAssessment, when non-nil and true, marks this process start as part
	// of a DEVICE boot (kernel boot window) rather than a Restart=always
	// daemon restart mid-life. Only a boot gets the StateOfflineRetrying entry
	// narration and the relocation check: a daemon restart during an
	// exhibition-long outage re-enters StateStarting too, and narrating (or
	// worse, raising the AP) there would paint setup over artwork that is
	// playing fine from the offline cache — the same rationale as the
	// executor's wireBootLifecycleHooks gate. nil fails closed (never a
	// boot).
	//
	// Evaluated exactly ONCE, inside New: the classification is a property of
	// PROCESS START, and New runs at wiring time, moments after exec — the
	// same instant wireBootLifecycleHooks makes its decision. Deferring the
	// read to the initial offline assessment (which runs after the boot AP
	// sweep and the initial connectivity query) would let a slow boot — large
	// offline cache replay, a monitord D-Bus timeout — drift past the
	// window's edge and misclassify a genuine boot as a mid-life restart,
	// silently dropping both the narration and the relocation recovery.
	BootAssessment func() bool

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

	// ActiveLinkDetail, when non-nil, is preferred over ActiveLink: the same
	// probe with the ethernet-only verdict from the same nmcli read
	// (status.LinkChecker.ExternalLinkDetail). It exists for the §4.7 health
	// snapshot — the machine caches the link TYPE its tick probes saw, so
	// status polls never spend a probe of their own. Identical failure bias
	// to ActiveLink (an error is unknown and defers the AP).
	ActiveLinkDetail func(ctx context.Context) (link, wired bool, err error)

	// WiredLink, when non-nil, confirms or refutes a live ETHERNET link (the
	// status.LinkChecker.WiredLink seam; constraint 6 semantics: verdict from
	// ethernet rows only, errors surface). The escape policy consults it for
	// episode arming (a wired frame never runs the setup-incomplete episode)
	// and for the wired exit from a raised AP. nil means "never confirmable":
	// the episode then never arms and the wired exit never fires — the
	// fail-safe default for wirings that predate the seam.
	WiredLink func(ctx context.Context) (bool, error)

	// InitialClaimed supplies the boot-time claim reading for the machine's
	// loop-visible claim snapshot (constraint 8). known=false (state file
	// unreadable) is treated as CLAIMED — the fail-safe direction, since the
	// worst mistake is auto-raising the AP over a claimed exhibition frame.
	// Later flips arrive via SetClaimed (the executor's claim observer). nil
	// = unknown = claimed.
	InitialClaimed func() (claimed, known bool)

	// Tuning carries the §4.1/§4.2 cadence knobs; zero fields take the
	// package defaults (see the session defaults block in session.go).
	Tuning Tuning

	// PortalAddr is the portal bind address (default ":80").
	PortalAddr string
	// OfflineWindow is how much confirmed link absence an offline device must
	// accumulate before the AP raises (default 5m). The window is
	// SAMPLE-COUNTED, not wall-clock (docs/network-recovery-ux.md constraint
	// 7): the raise requires OfflineWindow/CheckInterval confirmed-absent
	// probe samples (one per tick) with no linkPresent sighting since arming —
	// a sighting fully resets it. Inconclusive probes PAUSE the count (bounded
	// — see pauseOfflineWindow) rather than resetting it, so a flaky probe can
	// no longer starve the raise forever (D7). Counting samples rather than
	// elapsed time also means a stalled loop (multi-second radio scans on the
	// tick goroutine) extends the window in real time instead of shrinking the
	// evidence behind a one-way raise.
	OfflineWindow time.Duration
	// CheckInterval is how often the machine re-evaluates the offline window and
	// retries a deferred AP operation (default 15s).
	CheckInterval time.Duration
	// JoinTimeout bounds a portal join attempt (default 120s); expiry surfaces
	// as the standard timeout failure and re-raises the AP. See
	// defaultJoinTimeout for why it exists.
	JoinTimeout time.Duration

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

	// startedAtBoot is Config.BootAssessment latched once by New (nil probe =
	// never a boot). A bool, not the probe itself, so the boot-vs-restart
	// classification cannot drift with /proc/uptime between wiring and the
	// initial offline assessment — see the Config field's doc.
	startedAtBoot bool
	newPortal     func(portal.Config) PortalServer

	portalAddr    string
	offlineWindow time.Duration
	checkInterval time.Duration
	joinTimeout   time.Duration
	// tuning is the resolved §4.1/§4.2 cadence set (defaults applied in New).
	tuning Tuning
	// offlineWindowSamples is OfflineWindow expressed in tick samples (derived
	// once in New) — the expiry threshold for offlineAbsent below.
	offlineWindowSamples int

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
	lastReason string
	status     portal.Status
	apUp       bool
	apInfo     softap.Info
	portalSrv  PortalServer
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

	// episodeNarration is the Reason of the narrated StateOfflineRetrying
	// panel currently on screen ("" when none) — the generalized §4.4 episode
	// marker: set by transition at every NARRATED OfflineRetrying entry (the
	// narratedOfflineReason set) and cleared by every transition that leaves
	// the state. A device that never leaves the state keeps its marker and
	// stays governed by the repaint machinery — DELIBERATE: that is the
	// repaint path, not a leak, and restoring a one-boot scoping here would
	// re-break the very repaints §4.4 exists for. The tick uses it to keep that prose truthful as
	// the link comes and goes: a sighted link falsifies "setup will start in
	// a few minutes" (the AP is correctly suppressed while a link holds, so
	// the promise would sit on an air-gapped LAN forever), and a lost link
	// falsifies "connected" — and a same-state re-transition is deduped, so
	// nothing else would ever repaint. A dedicated marker rather than
	// lastReason: a redundant offline re-emission (a monitord restart)
	// re-targets StateOfflineRetrying with the generic reason and overwrites
	// lastReason WITHOUT any screen change. Set only at the boot assessment's
	// narrated entry and cleared by every transition that leaves
	// StateOfflineRetrying, so the deliberately silent exhibition outage path
	// (which never sets it) can never inherit a stale marker from an earlier
	// boot episode. Loop-goroutine-only, like connUnknown.
	episodeNarration string

	// relocConfirms/relocArmTriesLeft drive the boot relocation confirmation
	// (M-0b). The check may only START at a boot assessment (BootAssessment
	// true) whose link is confirmed absent: that grants relocArmTriesLeft
	// scan attempts (the assessment itself plus the first link-less ticks) to
	// obtain the FIRST positive "no saved SSID" result — the arming budget
	// exists because the very first scan runs moments after the boot AP
	// sweep flips the radio to station mode, when an empty (inconclusive)
	// result is the documented failure shape, and a one-shot arming would
	// silently render the whole feature inert. Once armed (relocConfirms >
	// 0), each link-less 15s tick rescans and only relocConfirmScans
	// consecutive positives raise the AP (reason "relocated"): a single scan
	// at t≈0 cannot distinguish "the frame moved" from "the router is still
	// booting after the same power cut", and the raise is a one-way door. A
	// saved-SSID sighting (or a hidden-network profile) cancels the check
	// terminally for this boot; an inconclusive scan mid-confirmation
	// cancels too (the ladder is consecutive-positives by design); a link
	// sighting or any clearOffline cancels everything. Inconclusive LINK
	// probes are tolerated up to relocUnknownTolerance before forfeiting
	// (relocationUnknownSample) — the old first-unknown forfeit was D7. The
	// plain sustained-offline window keeps running throughout, untouched.
	// Loop-goroutine-only, like connUnknown.
	relocConfirms     int
	relocArmTriesLeft int
	// relocUnknowns counts inconclusive link-probe samples charged against the
	// ladder's tolerance (relocUnknownTolerance); disarmRelocation resets it.
	// Loop-goroutine-only, like relocConfirms.
	relocUnknowns int

	// Offline-window sample counters (docs/network-recovery-ux.md §4.3,
	// constraint 7). Loop-goroutine-only, like connUnknown: written by
	// onConnectivity/onTick and the clear/pause helpers, all of which run on
	// the loop goroutine (or directly in single-threaded tests).
	//
	// offlineAbsent is the confirmed-absent samples accumulated since arming
	// (0 = disarmed); the AP raise requires offlineAbsent >=
	// offlineWindowSamples AND offlineFreshNeeded == 0. offlinePause is the
	// length of the CURRENT run of inconclusive samples — a pause longer than
	// one full window discards the accumulation as stale. offlineFreshNeeded
	// is how many fresh confirmed samples expiry still demands after a pause
	// (set to offlineFreshSamplesAfterPause by every inconclusive sample), so
	// a single absent sample after a long unknown run can never fire the
	// one-way raise off stale evidence.
	offlineAbsent      int
	offlinePause       int
	offlineFreshNeeded int

	// wiredLink is Config.WiredLink (nil = never confirmable; see the Config
	// field for the fail-safe consequences).
	wiredLink func(ctx context.Context) (bool, error)

	// initialClaimed is Config.InitialClaimed, read once by Start (after the
	// persisted state loads — see Start's ordering note).
	initialClaimed func() (claimed, known bool)

	// claimed is the claim snapshot (constraint 8): seeded once at Start by
	// initialClaimed (unknown = claimed), written SYNCHRONOUSLY by SetClaimed
	// from executor goroutines — never queued, because a dropped claim would
	// be permanent and an armed episode would raise over a just-claimed frame
	// (see SetClaimed). mu-GUARDED for that reason; loop code reads it via
	// isClaimed. The machine never calls into the state package from the loop
	// goroutine — that would take the state lock mid-loop, the coupling class
	// commit 116a8ed removed elsewhere.
	claimed bool

	// activeLinkDetail is Config.ActiveLinkDetail (preferred over activeLink
	// when set — see the Config field).
	activeLinkDetail func(ctx context.Context) (link, wired bool, err error)

	// §4.7 health-snapshot caches, in the mu-guarded block because Snapshot
	// reads them from request goroutines. Written ONLY by real probes: the
	// probeLink apUp short-circuit and the nil-guard fabrication never touch
	// them, so a stale absent-with-no-type cannot persist past an AP phase
	// (the stale-type pin) and a poll never spends a probe of its own.
	// linkOutcomeKnown gates the outcome; wiredKnown gates the type (only the
	// detail probe and explicit WiredLink runs learn it).
	linkOutcomeKnown bool
	linkOutcome      linkProbe
	wiredKnown       bool
	wiredLast        bool
	// episodeDeferredShared mirrors "the episode is currently pausing on hub
	// contact" for the snapshot's deferred sub-state (the app learns its own
	// presence is holding the AP down). Written on the loop, read under mu.
	episodeDeferredShared bool

	// lastHubContact / lastPortalActivity / lastPortalTraffic live in the
	// mu-guarded block: they are WRITTEN by request goroutines (the hub
	// middleware's contact observer; the portal's handlers) and read by the
	// loop. Hub contact feeds the §4.1 episode's deferral; portal activity
	// (human actions only) feeds the §4.2 mid-portal teardown deferral;
	// portal traffic (ANY request, probes included) feeds the recheck-only
	// attached-phone deferral — see sessionExpiryDue for why the two signals
	// stay separate.
	lastHubContact     time.Time
	lastPortalActivity time.Time
	lastPortalTraffic  time.Time

	// Session-policy state (§4.2) — all loop-goroutine-only. sessionPolicy is
	// LATCHED from the raise reason at raise time and RETAINED across join
	// attempts (applyJoin cancels the phase timer, never the policy field, so
	// a join-failure re-raise re-arms a fresh timer under the retained policy
	// — one typo must not escalate a bounded session into an unbounded
	// cadence). sessionReason is the latched raise reason, re-used verbatim by
	// the recheck blink's re-raise. sessionPhaseStart is the current AP
	// phase's start (zero = no timer; armed on ensureAPUp success);
	// sessionFirstRaise anchors the absolute session cap across applyRescan
	// re-arms. skipNextPreAPScan lets a blink's fresh successful scan stand in
	// for ensureAPUp's own pre-raise pass (one radio pass instead of two).
	sessionPolicy     sessionPolicy
	sessionReason     string
	sessionPhaseStart time.Time
	sessionFirstRaise time.Time
	skipNextPreAPScan bool
	// recheckCycle indexes the recheck cadence's AP-phase ladder
	// (sessionPhaseBound): advanced by each completed still-gone blink,
	// reset only at clearOffline's episode boundary. Survives blink
	// re-raises and join-failure re-raises BY DESIGN — resetting on either
	// would let the cadence restart its short rungs forever and never reach
	// the 30-minute steady state.
	recheckCycle int

	// Setup-incomplete episode state (§4.1) — loop-goroutine-only. The
	// episode window is sample-counted like the offline window but with the
	// OPPOSITE polarity (it counts confirmed link-PRESENT samples whose full
	// predicate holds) and its own pause/deferral rules — see episodeSample.
	// episodeTarget is the samples required before the next raise (the first
	// window, then the station-ladder rungs). episodeDeferred* track the
	// hub-contact deferral budgets, charged per paused tick.
	episodeSamples       int
	episodePause         int
	episodeFreshNeeded   int
	episodeTarget        int
	episodeCycle         int
	episodeSettled       bool
	episodeDeferredCycle time.Duration
	episodeDeferredTotal time.Duration

	// Escalation latches (§4.6, fixes D5/D6). apRaiseFails / apReleaseFails
	// count CONSECUTIVE ensureAPUp / ensureAPDown failures; at
	// setupErrorFailStreak the machine notifies ReasonSetupError once and
	// records which flavor in setupErrorLatch. While latched, ensureAPUp
	// suppresses its "scanning" push so the error panel does not flap at tick
	// cadence against the retry loop (which continues underneath throughout).
	// The raise flavor clears on the first successful ap.Up (whose own
	// softap_qr narration repaints) AND at every episode boundary via
	// clearOffline (a failed-raise episode can exit through link recovery
	// without ever succeeding — see resetAPRaiseEscalation); the release
	// flavor clears on the first successful teardown, emitting
	// ReasonSetupErrorCleared where no repaint would otherwise follow.
	// Loop-goroutine-only, like connUnknown.
	apRaiseFails    int
	apReleaseFails  int
	setupErrorLatch string

	// autoconnectRestorePending is the restore latch behind applyRescan's
	// autoconnect suppression: set when a restore call FAILED (or, at
	// construction, unconditionally — see below), cleared by the first restore
	// that succeeds. onTick retries it every tick, which is what makes the
	// suppression's time-bound layered rather than dependent on one deferred
	// call succeeding.
	//
	// It is initialized TRUE deliberately: every daemon start then issues one
	// idempotent `autoconnect on` on its first tick, which self-heals a
	// suppression a crash (or a SIGKILL mid-bounce) left behind without needing
	// any crash-time bookkeeping.
	//
	// Unsynchronized, and loop-goroutine-only like apRaiseFails — with one
	// deliberate exception: Stop read-modify-writes it (via
	// payOutstandingAutoconnectRestore) after <-done, which orders the loop's
	// final write before it. Both call that helper, because a shutdown
	// mid-bounce cannot be serviced by any later tick. No other goroutine may
	// touch this field; see restoreAutoconnect's doc.
	autoconnectRestorePending bool
}

// Setup-error latch flavors, carried in setupErrorLatch and logged; the
// user-facing prose lives at the notify sites.
const (
	setupErrorAPStart   = "ap_start_failed"
	setupErrorAPRelease = "ap_release_failed"
)

// relocConfirmScans is how many consecutive positive "no saved SSID in a full
// live scan" results (one 15s tick apart once armed) the relocation shortcut
// requires before raising the AP: ~30-45s to setup on a genuinely moved frame
// that arms at boot — within M-0's ≤1 minute acceptance — while a router
// rebooting from the same power cut gets that long to reappear.
// relocArmTries bounds how many scans may be SPENT trying to arm (boot
// assessment + the next link-less ticks): late arming trades a slightly later
// raise (~60-75s worst case) for not forfeiting the feature to one
// radio-settling empty scan.
const (
	relocConfirmScans = 3
	relocArmTries     = 3
)

type eventKind int

const (
	evConnectivity eventKind = iota
	evJoin
	evRescan
	evClaim
	evUserSetup
)

type event struct {
	kind    eventKind
	online  bool
	ssid    string
	psk     string
	hidden  bool
	claimed bool
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
	if cfg.JoinTimeout <= 0 {
		cfg.JoinTimeout = defaultJoinTimeout
	}
	if cfg.NewPortal == nil {
		cfg.NewPortal = func(pc portal.Config) PortalServer { return portal.NewServer(pc) }
	}
	// The sample threshold rounds up: a window that is not a whole multiple of
	// the tick must err toward MORE confirmed evidence before the one-way
	// raise, never less. Minimum 1 so a degenerate config still requires one
	// confirmed sample.
	windowSamples := int((cfg.OfflineWindow + cfg.CheckInterval - 1) / cfg.CheckInterval)
	if windowSamples < 1 {
		windowSamples = 1
	}
	// The claim snapshot defaults to CLAIMED (constraint 8's fail-safe
	// direction — never auto-raise over a possibly claimed exhibition frame);
	// Start seeds it from InitialClaimed once the persisted state is loaded.
	claimed := true
	return &Machine{
		ap:               cfg.AP,
		wifi:             cfg.Wifi,
		conn:             cfg.Connectivity,
		clock:            cfg.Clock,
		logger:           logger,
		notifier:         cfg.Notifier,
		activeLink:       cfg.ActiveLink,
		activeLinkDetail: cfg.ActiveLinkDetail,
		wiredLink:        cfg.WiredLink,
		claimed:          claimed,
		initialClaimed:   cfg.InitialClaimed,
		tuning:           cfg.Tuning.withDefaults(cfg.CheckInterval, logger),
		// Latched HERE, not read at the offline assessment: New runs at
		// wiring time (moments after process start), so this is the honest
		// "did this process start at boot" answer regardless of how long the
		// boot AP sweep or the initial connectivity query later takes.
		startedAtBoot:        cfg.BootAssessment != nil && cfg.BootAssessment(),
		newPortal:            cfg.NewPortal,
		portalAddr:           cfg.PortalAddr,
		offlineWindow:        cfg.OfflineWindow,
		checkInterval:        cfg.CheckInterval,
		joinTimeout:          cfg.JoinTimeout,
		offlineWindowSamples: windowSamples,
		sup:                  newSupervisor(cfg.Clock, logger),
		events:               make(chan event, eventBuffer),
		state:                StateStarting,
		status:               portal.Status{State: portal.JoinIdle},
		// Startup self-heal: the first tick unconditionally re-enables device
		// autoconnect (idempotent), covering a suppression stranded by a crash
		// mid-rescan-bounce. See the field's doc.
		autoconnectRestorePending: true,
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
	// The boot claim snapshot is read at START, not construction: New runs in
	// initializeApp, BEFORE run() has loaded the persisted state, so a
	// construction-time read would always see the pre-load empty state as
	// "unclaimed" — exactly the unsafe direction constraint 8 forbids. Start
	// runs after the state load; the loop goroutine has not spawned yet, so
	// the write takes m.mu only for uniformity with SetClaimed's writer.
	// Unknown (nil probe or known=false) keeps the
	// fail-safe claimed default from New.
	if m.initialClaimed != nil {
		if c, known := m.initialClaimed(); known {
			m.mu.Lock()
			m.claimed = c
			m.mu.Unlock()
		}
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
// runMu is held for the WHOLE call, not just the field swap: the post-<-done
// work below touches loop-owned state (autoconnectRestorePending), and its
// safety rests on the loop having exited. Releasing the lock early would let a
// concurrent Start — which sees cancel already nil — spawn a second loop
// goroutine that races that state. The loop never takes runMu, so holding it
// across <-done cannot deadlock.
func (m *Machine) Stop() {
	m.runMu.Lock()
	defer m.runMu.Unlock()
	cancel := m.cancel
	done := m.done
	m.cancel = nil

	if cancel == nil {
		return
	}
	cancel()
	<-done
	// Payment BEFORE teardown, mirroring the loop's ctx.Done branch: the
	// payment is bounded, the profile deletion below is not, and the reverse
	// order would let a wedged deletion starve the restore. The loop's own
	// branch pays too, but it is not guaranteed to run — a panic recovered
	// while the ctx is already canceled (or a shutdown landing in the
	// supervisor's post-panic backoff) makes supervisor.run return WITHOUT
	// re-entering loop. This call is what makes the payment unconditional for
	// the Stop route. Safe to touch the loop-owned latch here: <-done orders
	// this after the loop's last write.
	m.payOutstandingAutoconnectRestore()
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
	m.onConnectivity(ctx, online, err != nil)

	for {
		select {
		case <-ctx.Done():
			// Pay any outstanding autoconnect restore on a context that can
			// still complete — applyRescan's deferred restore runs on THIS
			// ctx, so a shutdown landing inside the bounce leaves the flag off
			// with a latch no further tick will ever service. The startup
			// self-heal only covers shutdowns that are followed by a start: an
			// intentional `systemctl --user stop` is not, and would leave NM
			// unable to auto-reconnect the device until an NM restart or a
			// reboot.
			//
			// Ordered BEFORE the teardown below deliberately: the payment is
			// bounded (shutdownRestoreTimeout) but the profile deletion is
			// not, so the reverse order would let a wedged nmcli starve the
			// restore until systemd kills the unit. Restoring while the AP may
			// still be up loses nothing — there is no re-raise at shutdown, so
			// NM reconnecting once the AP drops is the desired end state, not
			// the race the suppression exists to remove.
			//
			// This branch covers a shutdown driven by canceling Start's ctx
			// with no Stop call. It is NOT the whole story — see Stop, which
			// repeats the payment because supervisor.run can return without
			// re-entering loop at all.
			m.payOutstandingAutoconnectRestore()
			// Final teardown on a fresh context so the canceled runCtx cannot
			// skip it.
			m.ensureAPDown(context.Background())
			return
		case ev := <-m.events:
			switch ev.kind {
			case evConnectivity:
				m.onConnectivity(ctx, ev.online, false)
			case evJoin:
				m.applyJoin(ctx, ev.ssid, ev.psk, ev.hidden)
			case evRescan:
				m.applyRescan(ctx)
			case evClaim:
				m.applyClaim(ctx, ev.claimed)
			case evUserSetup:
				m.applyUserSetup(ctx)
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
func (m *Machine) RequestJoin(req portal.JoinRequest) error {
	ssid := req.SSID
	if req.Manual {
		// Trim ONLY the manual-entry branch: phone keyboards autocomplete a
		// trailing space onto typed names, and the resulting join fails "not
		// found" against advice ("move closer") that cannot help. Picker
		// values pass through VERBATIM — leading/trailing bytes are valid SSID
		// content, and the scan side deliberately preserves them
		// (wifictl.SavedWifiSSIDs documents the equality hazard).
		ssid = strings.TrimSpace(ssid)
	}
	// Emptiness is judged on the trimmed value for BOTH branches (a
	// whitespace-only submission is no submission), but only the manual branch
	// above actually mutates what gets joined.
	if strings.TrimSpace(ssid) == "" {
		return errors.New("please choose a Wi-Fi network")
	}
	select {
	case m.events <- event{kind: evJoin, ssid: ssid, psk: req.Password, hidden: req.Hidden}:
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

// onConnectivity applies one reachability assessment. assumed marks a reading
// that is an ASSUMPTION, not a measurement: the caller's Online query failed
// and offline was substituted (boot assessment, post-join assessment). The
// flag owns connUnknown directly — callers no longer re-arm it after the call
// — and it also picks the hedged narration on the Joining entry edge, where
// asserting "this network has no internet" off a failed query would be a lie.
func (m *Machine) onConnectivity(ctx context.Context, online bool, assumed bool) {
	// Any real reading — a connectivity_change event or a successful re-query
	// — resolves an assumed-offline state; an assumed one (re-)arms it so
	// onTick keeps re-querying until a real answer arrives.
	m.connUnknown = assumed
	m.online = online
	if online {
		m.clearOffline()
		// WAN confirmation cancels the setup-incomplete episode outright
		// (§4.1's cancel edge) — the world the episode escapes from is gone.
		m.cancelEpisode(ctx, "online confirmed")
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
			// The out-of-box exemption, mirrored from the raised-AP wired exit
			// (see wiredExitDue). This branch is only reachable with apUp
			// FALSE — probeLink short-circuits to absent while our own hotspot
			// holds the radio — so it is the FAILED-raise flavor: NM keeps
			// refusing the AP while a link is present. For a provisioned frame
			// that exit is right (the saved network can carry the device). For
			// the out-of-box session there is nothing to fall back to: landing
			// StateUnprovisioned hides the overlay and no window ever re-arms
			// while the link holds, so an air-gapped cable would strand an
			// unclaimed frame on a blank screen with the raise abandoned.
			// Keep wanting the AP and let reconcile keep retrying instead.
			// A confirmed ONLINE reading still exits above — WAN reachability
			// is the correct end of an out-of-box session, and this guard is
			// deliberately below it.
			if cur == StateAPActive && m.sessionPolicy == sessionUnbounded {
				m.reconcile(ctx)
				return
			}
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
			// Inconclusive: pause the window rather than resetting it (§4.3 —
			// an unknown is not counter-evidence, and resetting here let one
			// probe hiccup restart the whole wait).
			m.pauseOfflineWindow()
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
				m.armOfflineWindow()
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

	// DEVICE-BOOT assessment of a provisioned-but-offline device (M-0): the
	// screen would otherwise stay black for the whole sustained-offline
	// window, and a frame moved to a new site cannot self-heal at all — its
	// saved SSID is simply not there. Gated on startedAtBoot, NOT on
	// StateStarting alone: controld is Restart=always, so a daemon restart
	// during an exhibition-long outage re-enters StateStarting too, and
	// narrating (or raising the AP) there would paint setup over artwork
	// playing fine from the offline cache. Ordering: narrate FIRST from one
	// link probe (the relocation check below runs multi-second radio scans,
	// and this edge exists precisely so the screen stops being silently
	// black), then arm the tick-confirmed relocation check. The probe/scan
	// feed narration and the shortcut ONLY: window arming stays exactly as
	// before (nil-guard arms here, a wired guard arms on onTick's first
	// confirmed absence), so #233's continuous-confirmed-absence contract is
	// untouched.
	if cur == StateStarting {
		// startedAtBoot was latched by New at wiring time, so a slow path to
		// this assessment (boot AP sweep, a monitord D-Bus timeout, offline
		// cache replay) cannot drift the classification past the boot
		// window's edge — a process that started at boot narrates no matter
		// how late this line runs.
		if !m.startedAtBoot {
			// Telemetry for the feature's silent-decline mode: without this
			// line a field report of "the moved frame stayed black / waited
			// the full window" cannot be told apart from a mid-life daemon
			// restart versus a boot so slow the daemon itself was started
			// outside the window.
			m.logger.Info("provisioning: this process did not start within the device-boot window; keeping the un-narrated path")
		} else {
			probe := m.probeLink(ctx)
			if m.activeLink == nil {
				m.armOfflineWindow()
			}
			d := bootOfflineDetail(probe)
			m.transition(ctx, StateOfflineRetrying, d)
			// Set AFTER the transition (which clears the marker only for
			// non-OfflineRetrying targets): the marker must describe what the
			// paint above actually put on screen.
			m.episodeNarration = d.Reason
			// Relocation check, first scan attempt (the radio is back in station
			// mode — the boot AP sweep precedes this assessment — so a live scan
			// is possible). ORDERING IS LOAD-BEARING: the fields are set AFTER
			// the transition above because clearOffline() disarms the check, and
			// while today's reconcile path for OfflineRetrying happens not to
			// call clearOffline, arming first would couple the feature's
			// survival to that incidental fact. onTick takes every later sample
			// and fires the raise.
			if probe == linkAbsent {
				m.relocArmTriesLeft = relocArmTries
				// The first sample runs only on a MEASURED offline reading.
				// On an assumed-offline boot (the Online query failed — the
				// documented monitord startup race) the ladder keeps its
				// budget but does not scan: three early scans could raise
				// the one-way AP on a device whose connectivity was never
				// actually read — the exact false assumption connUnknown
				// exists to contain. onTick applies the same gate, so the
				// ladder stays frozen until a successful query confirms
				// offline (it then advances normally) or reads online
				// (clearOffline disarms everything). Freezing rather than
				// declining to arm matters: arming happens ONLY here, so a
				// decline would leave the feature inert for the whole boot
				// over a few seconds of monitord lag.
				if !m.connUnknown {
					m.relocationScanStep(ctx)
				}
			}
			return
		}
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
		m.armOfflineWindow()
	}
	// The Joining entry edge is F-01's silent-forever screen: the join
	// ASSOCIATED (the portal already reported JoinSucceeded) but the upstream
	// is dead, and with the association holding the link, the AP deliberately
	// never returns. Narrate it. Every other edge (online→offline on an
	// exhibition device, redundant re-emissions) keeps the generic reason,
	// which the notifier deliberately does not paint.
	detail := Detail{Reason: "offline", Message: "Reconnecting to Wi-Fi"}
	// The D4 verdict repaint: while the joined-conn-unknown hedge is the
	// panel on screen ("Checking internet access…"), a CONFIRMED offline
	// reading is a verdict change the screen must follow — the generic tail
	// reason below is deliberately un-narrated, and before this the hedge was
	// the terminal narration forever. The dedupe extension in transition (a
	// reason change INTO the narrated set notifies) is what makes this
	// repaint deliverable at all.
	if cur == StateOfflineRetrying && m.episodeNarration == ReasonJoinedConnUnknown && !assumed {
		m.mu.Lock()
		ssid := m.status.SSID
		m.mu.Unlock()
		detail = Detail{SSID: ssid, Reason: ReasonJoinedNoInternet,
			Message: "Connected to " + ssid + ", but this network has no internet access"}
	}
	if cur == StateJoining {
		m.mu.Lock()
		ssid := m.status.SSID
		m.mu.Unlock()
		if assumed {
			// The reachability QUERY failed; offline is an assumption, so
			// hedge — asserting a dead network here would smear a possibly
			// healthy one. If a later re-query confirms offline the machine
			// is already in this state and will not re-notify (transition
			// dedupe), so this hedge is the leg's terminal narration by
			// design.
			detail = Detail{SSID: ssid, Reason: ReasonJoinedConnUnknown,
				Message: "Connected to " + ssid + ". Checking internet access…"}
		} else {
			detail = Detail{SSID: ssid, Reason: ReasonJoinedNoInternet,
				Message: "Connected to " + ssid + ", but this network has no internet access"}
		}
	}
	m.transition(ctx, StateOfflineRetrying, detail)
}

// bootOfflineDetail picks the boot-entry narration for StateOfflineRetrying by
// what the link probe could actually confirm — each message must only promise
// what the machine will really do (the AP never rises while a link holds, and
// the wait before setup is a floor, not a bound: a wired guard arms the window
// at the first confirmed-absent tick and any inconclusive probe restarts it,
// while the relocation check can legitimately start setup sooner — hence
// "a few minutes … if the connection does not return" rather than a number).
func bootOfflineDetail(probe linkProbe) Detail {
	switch probe {
	case linkPresent:
		return Detail{Reason: ReasonBootNoInternet,
			Message: "Connected to the network, but there is no internet access. Retrying…"}
	case linkUnknown:
		return Detail{Reason: ReasonBootLinkUnknown,
			Message: "Checking the network connection…"}
	default:
		// Deliberately does not assert "network not found" (this branch also
		// covers "SSID in range but cannot associate" — changed password —
		// and the narration is painted BEFORE any scan runs) and does not
		// assert "unable to connect" either: at boot this paints seconds
		// after start, while NM's own autoconnect attempt is typically still
		// in flight, so a confirmed-absent probe here only proves "no
		// activated link right now", not that connecting failed.
		return Detail{Reason: ReasonBootOffline,
			Message: "Looking for your Wi-Fi network… Setup mode will start in a few minutes if the connection does not return."}
	}
}

// repaintBootNarration keeps the boot-entry prose in lockstep with the most
// recent link probe: every site that acts on a probe result while the boot
// narration is on screen routes it here, so the screen can never disagree
// with the evidence the machine just used. Routes through bootOfflineDetail —
// the single probe→prose table — and repaints only when the wording actually
// changes, so repeated same-outcome probes cost nothing. No-op outside a
// boot-narrated episode (the marker is set only at the narrated boot entry),
// which is what keeps the deliberately silent exhibition path silent.
func (m *Machine) repaintBootNarration(probe linkProbe) {
	// Only the PROBE-DRIVEN boot reasons repaint from a link probe; the other
	// narrated reasons (joined-*, ap-session-ended, ap-recheck, settled) are
	// verdict- or session-driven and repaint via their own events — letting a
	// probe overwrite a station-phase or settled panel with boot prose would
	// clobber the §4.1/§4.2 narration on every tick.
	switch m.episodeNarration {
	case ReasonBootOffline, ReasonBootNoInternet, ReasonBootLinkUnknown:
	default:
		return
	}
	if d := bootOfflineDetail(probe); m.episodeNarration != d.Reason {
		m.episodeNarration = d.Reason
		m.notify(StateOfflineRetrying, d)
	}
}

// narratedOfflineReason names the StateOfflineRetrying entry reasons that
// paint a panel (the §4.4 whitelist for the dedupe extension and the episode
// marker). Everything else in that state is deliberately silent — the
// exhibition path.
func narratedOfflineReason(r string) bool {
	switch r {
	case ReasonBootOffline, ReasonBootNoInternet, ReasonBootLinkUnknown,
		ReasonJoinedNoInternet, ReasonJoinedConnUnknown,
		ReasonAPSessionEnded, ReasonAPRecheck, ReasonSetupIncompleteSettled:
		return true
	default:
		return false
	}
}

// silentOfflineReason names the deliberately un-narrated OfflineRetrying
// legs. Transitions from a narrated reason into one of these emit the
// explicit silent hide (constraint 4(b)) — the notifier's leave-the-screen
// default there would strand the last narrated panel as a permanent lie.
func silentOfflineReason(r string) bool {
	return r == "offline" || r == "link-present"
}

// relocScanOutcome is the tri-state result of one relocation scan attempt.
// Three states because the two negatives have opposite consequences: a
// DISPROVED result ends the check for this boot, an INCONCLUSIVE one may be
// retried while the arming budget lasts.
type relocScanOutcome int

const (
	// relocScanInconclusive: profile list or scan failed, or the scan came
	// back EMPTY (the documented radio-settling shape right after the boot
	// AP sweep). Retryable within the arming budget; forfeits an in-progress
	// confirmation (the ladder is consecutive clean positives by design).
	relocScanInconclusive relocScanOutcome = iota
	// relocScanConfirmedAbsent: a successful, non-empty, uncapped live scan
	// positively showed none of the profile-declared saved SSIDs.
	relocScanConfirmedAbsent
	// relocScanDisproved: terminal for this boot — a saved SSID is actually
	// in range (the device may associate on its own), a saved profile
	// targets a hidden network (its SSID can never appear in a scan, so
	// absence is unobtainable as evidence for the whole set), or there are
	// no saved Wi-Fi profiles at all.
	relocScanDisproved
)

// relocationScan runs one saved-SSIDs-vs-full-scan comparison. Every outcome
// is logged: this check's failure mode is silently never firing, and a field
// report of "the moved frame still waited five minutes" must be diagnosable
// from the journal alone.
func (m *Machine) relocationScan(ctx context.Context) relocScanOutcome {
	saved, anyHidden, err := m.wifi.SavedWifiSSIDs(ctx)
	if err != nil {
		m.logger.Warn("provisioning: relocation check: saved-SSID list failed", zap.Error(err))
		return relocScanInconclusive
	}
	if len(saved) == 0 {
		m.logger.Info("provisioning: relocation check: no saved Wi-Fi profiles")
		return relocScanDisproved
	}
	if anyHidden {
		m.logger.Info("provisioning: relocation check: a saved profile targets a hidden network; not scan-verifiable")
		return relocScanDisproved
	}
	ssids, err := m.wifi.ScanAllSSIDs(ctx)
	if err != nil {
		m.logger.Warn("provisioning: relocation check: scan failed", zap.Error(err))
		return relocScanInconclusive
	}
	if len(ssids) == 0 {
		m.logger.Info("provisioning: relocation check: empty scan (radio still settling?)")
		return relocScanInconclusive
	}
	inRange := make(map[string]struct{}, len(ssids))
	for _, s := range ssids {
		inRange[s] = struct{}{}
	}
	for _, s := range saved {
		if _, ok := inRange[s]; ok {
			m.logger.Info("provisioning: relocation check: a saved network is in range", zap.Int("scanned", len(ssids)))
			return relocScanDisproved
		}
	}
	m.logger.Info("provisioning: relocation check: no saved network in a full scan",
		zap.Int("saved", len(saved)), zap.Int("scanned", len(ssids)))
	return relocScanConfirmedAbsent
}

// relocationScanStep runs ONE relocation scan attempt (at the boot assessment
// or a link-less tick) and advances the arming/confirmation state. It returns
// true once relocConfirmScans consecutive positives have been observed — the
// caller then raises the AP. No-op when the check is neither armed nor has
// arming budget left, so post-budget ticks cost no scans.
func (m *Machine) relocationScanStep(ctx context.Context) bool {
	if m.relocConfirms == 0 && m.relocArmTriesLeft == 0 {
		return false
	}
	switch m.relocationScan(ctx) {
	case relocScanConfirmedAbsent:
		m.relocConfirms++
		m.relocArmTriesLeft = 0 // armed: consecutive positives only from here
		return m.relocConfirms >= relocConfirmScans
	case relocScanDisproved:
		m.disarmRelocation("a saved network is in range, unverifiable, or absent entirely")
	case relocScanInconclusive:
		if m.relocConfirms > 0 {
			m.disarmRelocation("inconclusive scan mid-confirmation")
			return false
		}
		m.relocArmTriesLeft--
		if m.relocArmTriesLeft <= 0 {
			m.relocArmTriesLeft = 0
			m.logger.Info("provisioning: boot relocation check disarmed",
				zap.String("reason", "arming budget exhausted on inconclusive scans"))
		}
	}
	return false
}

// disarmRelocation cancels a pending boot relocation confirmation, logging
// why when it actually cancels something (reason "" fires silently — used
// where the caller logs its own outcome, e.g. the confirmed raise).
func (m *Machine) disarmRelocation(reason string) {
	if m.relocConfirms == 0 && m.relocArmTriesLeft == 0 {
		return
	}
	if reason != "" {
		m.logger.Info("provisioning: boot relocation check disarmed", zap.String("reason", reason))
	}
	m.relocConfirms = 0
	m.relocArmTriesLeft = 0
	m.relocUnknowns = 0
}

// onTick fires the sustained-offline window and retries any deferred AP op.
func (m *Machine) onTick(ctx context.Context) {
	// Autoconnect restore, retried until it succeeds. Placed at the TOP and
	// outside the state switch on purpose: every branch below can return early,
	// and this must run on every tick in every state — it is both the retry for
	// a failed restore and the startup self-heal for a suppression a crash left
	// behind (the latch starts true; see the field's doc).
	if m.autoconnectRestorePending {
		m.restoreAutoconnect(ctx)
	}

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
			m.onConnectivity(ctx, online, false)
		}
	}

	m.mu.Lock()
	st := m.state
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
		// The probe runs EVERY tick, not just at expiry; a link sighting fully
		// resets the window, and an inconclusive probe pauses it (bounded —
		// see pauseOfflineWindow). That is what makes OfflineWindow measure
		// confirmed LINK absence: sampling only at expiry would let an
		// association lost moments before the deadline raise the AP after
		// seconds of link loss — and on Wi-Fi that raise is unrecoverable
		// without a human (the hotspot takes the radio, so reachability can
		// never return on its own to tear it down).
		probe := m.probeLink(ctx)
		switch probe {
		case linkPresent:
			// A sighting fully resets the window AND the relocation ladder
			// (the §4.3 single-truth rule): the link's existence is direct
			// counter-evidence against both, and a stale armed window would
			// raise the AP instantly the moment the link later disappears —
			// the window must measure confirmed absence since the LAST
			// sighting. Fall through to reconcile rather than returning: a
			// pending AP-profile teardown (apDownPending) must keep retrying
			// on guarded ticks too.
			m.clearOffline()
			// The link-PRESENT shape's escape (§4.1): each confirmed-present
			// tick is one sample for the setup-incomplete episode, which may
			// raise the AP for an unclaimed no-WAN device. Runs before the
			// repaint so a raise's own narration wins the tick.
			m.episodeSample(ctx)
			m.mu.Lock()
			raised := m.state == StateAPActive
			m.mu.Unlock()
			if raised {
				return
			}
			// The reset above just changed what is TRUE (a present link
			// suppresses the AP outright), so the boot prose must follow —
			// the same-state transition dedupe repaints nothing on its own,
			// and a stale "setup will start in a few minutes" would sit on
			// screen forever on an air-gapped LAN.
			m.repaintBootNarration(probe)
		case linkUnknown:
			// Inconclusive: PAUSE, never reset (§4.3). The old reset-on-unknown
			// let one 3s nmcli timeout restart the whole window forever (D7);
			// the pause keeps the accumulated evidence, bounded by the discard
			// and freshness rules in pauseOfflineWindow. The relocation ladder
			// charges its own bounded tolerance instead of forfeiting on the
			// first unknown. Same fall-through-to-reconcile as the sighting
			// branch.
			m.pauseOfflineWindow()
			m.relocationUnknownSample()
			m.repaintBootNarration(probe)
		case linkAbsent:
			// Confirmed link loss hands the device to the untouched
			// link-absent machinery (constraint 12): the two evidence shapes
			// never share an episode, and this cancel is what keeps the
			// settled state from suppressing a sustained-offline raise for a
			// device whose link is genuinely gone.
			m.cancelEpisode(ctx, "link loss confirmed")
			// Boot relocation confirmation (M-0b): may only START at a boot
			// assessment with a confirmed-absent link; each still-link-less
			// tick runs one scan step (arming within the bounded budget, or
			// confirming once armed — see relocationScanStep). Only
			// relocConfirmScans consecutive positives raise the AP (~30-45s
			// after boot when armed at the assessment — a router recovering
			// from the same power cut gets that long to reappear). The plain
			// window below is the fallback either way, and keeps running
			// throughout.
			// The connUnknown gate mirrors the boot entry's: while offline is
			// only ASSUMED (every query so far failed), the ladder neither
			// scans nor spends budget — scan-confirmed absence must never
			// compound an unmeasured connectivity assumption into the one-way
			// raise. The re-query at the top of this tick clears the flag on
			// the first successful offline reading, so the ladder resumes on
			// the same tick that confirms.
			if !m.connUnknown && m.relocationScanStep(ctx) {
				// FINAL link re-check before the one-way door: the probe at
				// the top of this tick predates the scan above, which runs
				// multi-second radio work on this same goroutine — a link
				// that appeared meanwhile (the ethernet escape hatch plugged
				// in; an AP whose beacons the scan pass missed but the
				// supplicant then caught) has its event QUEUED behind this
				// tick and cannot be seen before the raise. Anything short of
				// a confirmed-absent link right now takes the standard
				// sighting semantics — clearOffline disarms the ladder and
				// the window, and the next confirmed absence re-arms the
				// plain window — because on Wi-Fi the raise drops the
				// recovered link and nothing can lower the AP again without a
				// human.
				if g := m.probeLink(ctx); g != linkAbsent {
					m.logger.Info("provisioning: link sighted during the final relocation scan; deferring to the offline window")
					// Sighting semantics per outcome: a confirmed sighting
					// resets both mechanisms (single-truth), while an
					// inconclusive re-probe only pauses the window and charges
					// the ladder's tolerance — the raise stays deferred either
					// way, which is all the veto requires.
					if g == linkPresent {
						m.clearOffline()
					} else {
						m.pauseOfflineWindow()
						m.relocationUnknownSample()
					}
					// The veto acts on this fresh probe, so the narration
					// must too — waiting for the next tick's repaint would
					// leave the setup promise up for 15s after setup was
					// just deferred.
					m.repaintBootNarration(g)
					break
				}
				m.logger.Info("provisioning: relocation confirmed by repeated scans; starting setup",
					zap.Int("scans", relocConfirmScans))
				m.disarmRelocation("")
				m.clearOffline()
				m.resetJoinStatus()
				m.transition(ctx, StateAPActive, Detail{
					Reason:  ReasonRelocated,
					Message: "None of the saved Wi-Fi networks are nearby; starting setup",
				})
				return
			}
			// Boot-narration downgrade, the upgrade's mirror: an entry that
			// asserted "connected, no internet" (or hedged "checking") stops
			// being true once the link is confirmed gone, and the setup
			// promise becomes accurate again — the window below is arming.
			// (No double paint on the expiry-veto path below: with a wired
			// guard, expiry requires earlier absent ticks whose downgrade
			// already settled the marker on the promise; with a nil guard
			// the entry probe was necessarily linkAbsent — marker already
			// the promise — and the re-probe can never veto.)
			m.repaintBootNarration(linkAbsent)
			// One confirmed-absent sample per tick; the raise fires only once
			// a full window of samples has accumulated with no sighting and no
			// outstanding post-pause freshness debt (constraint 7 / §4.3).
			m.recordOfflineAbsent()
			if m.offlineWindowExpired() {
				// Re-probe before THIS one-way door too: the relocation scan
				// above runs multi-second radio work after the top-of-tick
				// probe. A link that came back during a non-confirming scan
				// would otherwise be dropped by a raise justified only by the
				// pre-scan probe. A confirmed sighting resets everything; an
				// inconclusive re-probe pauses, so the raise waits for the
				// freshness debt to clear. On scan-free ticks the two probes
				// are milliseconds apart, so this is effectively free.
				if g := m.probeLink(ctx); g != linkAbsent {
					m.logger.Info("provisioning: link sighted at the expiry re-check; deferring the sustained-offline raise")
					if g == linkPresent {
						m.clearOffline()
					} else {
						m.pauseOfflineWindow()
						m.relocationUnknownSample()
					}
					m.repaintBootNarration(g)
					break
				}
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
		case linkPresent:
			// Same single-truth reset as the provisioned flavor: the next
			// confirmed absence starts a fresh count.
			m.clearOffline()
		case linkUnknown:
			// Inconclusive: pause, never reset (§4.3). No relocation charge —
			// the ladder never runs in this state.
			m.pauseOfflineWindow()
		case linkAbsent:
			m.recordOfflineAbsent()
			if m.offlineWindowExpired() {
				// Re-check the profile at the raise boundary: a profile
				// acquired out-of-band while parked (e.g. a hub-driven join
				// attempt) makes this the PROVISIONED flavor — NM can retry
				// it, so hand the device the provisioned resting state and a
				// fresh window rather than an unprovisioned-style raise.
				if m.hasProfile(ctx) {
					m.clearOffline()
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
		m.mu.Lock()
		apUp := m.apUp
		m.mu.Unlock()
		if apUp {
			// A RAISED AP has two — and only two — automatic exits beyond
			// online/portal-join. (1) The wired sighting (§4.2): an ethernet
			// row in the device survey has none of the own-hotspot ambiguity
			// the probeLink short-circuit protects Wi-Fi against, the AP
			// cannot fix a wired network, and a later online reading would
			// tear it down anyway — so plugging a cable in ends the session,
			// user-requested included. The out-of-box unbounded session is the
			// one exemption (see wiredExitDue): there is no saved network for
			// the AP to compete with, and lowering it would leave an
			// unclaimed air-gapped frame on a blank screen. (2) The session
			// policy's own phase expiry (recheck blink, episode station phase,
			// user-session abandonment net).
			if m.wiredExitDue(ctx) {
				m.exitRaisedAPForWire(ctx)
				return
			}
			if m.sessionExpiryDue() {
				m.expireSession(ctx)
				return
			}
			break
		}
		// A FAILED/PENDING raise (apUp false) is the one APActive flavor a
		// LINK reading may exit: no hotspot is up for the probe to mistake
		// for an uplink, and the link can genuinely recover while NM keeps
		// refusing the raise — on an air-gapped LAN no connectivity event
		// will ever arrive, so without this probe the eventual retry would
		// SUCCEED and drop the recovered link, the exact harm the guard
		// exists to prevent. Absent/unknown fall through to reconcile, which
		// keeps retrying the raise. The saved-profile check routes the exit
		// to the matching resting state, whose tick probes re-arm the
		// sustained window before any fresh raise.
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
func (m *Machine) applyJoin(ctx context.Context, ssid, psk string, hidden bool) {
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
	// The join cancels the session TIMER, never the policy latch (§4.2): a
	// failure re-raise re-arms a fresh timer under the retained policy, so a
	// typo inside a bounded session keeps that session's own bound.
	m.cancelSessionTimer()
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

	// Deadline on the one long blocking call the loop goroutine makes (D10):
	// a wedged NM activation would otherwise block the machine forever with
	// the AP down and the portal stopped — no tick, no event, no recovery. A
	// deadline expiry is reported as the existing timeout failure, so the
	// standard failure path below re-raises the AP. The wifictl cleanup path
	// already detaches from this ctx, so the half-created profile still gets
	// deleted after a timeout kill.
	joinCtx, cancel := context.WithTimeout(ctx, m.joinTimeout)
	err := m.wifi.Join(joinCtx, ssid, psk, hidden)
	cancel()
	if err != nil && errors.Is(joinCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
		// The deadline killed the join mid-flight; nmcli's own error for a
		// killed process classifies as unknown, so name the real cause. Parent
		// cancellation (daemon shutdown) is deliberately excluded — that is
		// not a network timeout and the machine is exiting anyway.
		err = &wifictl.JoinError{Kind: wifictl.JoinErrTimeout,
			Output: "join did not complete within " + m.joinTimeout.String()}
	}
	if err == nil {
		m.mu.Lock()
		m.status = portal.Status{State: portal.JoinSucceeded, SSID: ssid, Message: "Connected to " + ssid}
		m.mu.Unlock()
		m.logger.Info("provisioning: wifi join succeeded", zap.String("ssid", ssid))
		// A completed portal join is the strongest world-changed signal there
		// is (§4.1): the episode resets fully, so a user who picked a
		// wrong-but-live SSID first gets a full-length episode on the network
		// they actually meant, not a shortened runway.
		m.cancelEpisode(ctx, "portal join completed")
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
		// assumed carries the failed-query fact into the assessment: it keeps
		// connUnknown armed (sys-monitord being briefly unavailable during
		// the join must not become a permanent offline verdict) and it picks
		// the hedged Joining-edge narration over the "no internet" assertion.
		m.onConnectivity(ctx, online, oerr != nil)
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
	// An actively rescanning user is present: re-arm the session's current
	// phase (§4.2 — intended, bounded by the absolute cap in
	// sessionExpiryDue; on a recheck session this resets only the current AP
	// phase, whose cadence is unbounded by design).
	m.armSessionTimer()
	// Flip the screen to "scanning" IMMEDIATELY: the AP teardown below takes
	// seconds, and leaving the join QR up through it reads as a stalled button
	// press. ensureAPUp re-announces the same state when the fresh scan
	// actually starts; setupui coalesces the duplicate.
	m.notify(StateAPActive, Detail{
		Reason:  "scanning",
		Message: "Looking for nearby Wi-Fi networks",
	})
	// Suppress NM's OWN activations for the length of the bounce. Between the
	// teardown below and the re-raise the radio sits in station mode with every
	// saved profile's autoconnect live, so NM races this function for it: on
	// FF1-8EVTK3RE the policy engine started auto-activating a saved profile
	// 5.9s after teardown and lost to the re-raise by 0.52s, which killed the
	// association mid-`config`. Either winner is survivable (the state is still
	// StateAPActive, so the next reconcile re-raises and cuts NM off again) —
	// what the suppression buys is that the outcome no longer depends on which
	// side wins a sub-second race, which is what made field behavior
	// irreproducible.
	//
	// Scope is deliberately THIS call frame only, and the guarantee that it
	// cannot bleed into the other teardown paths is structural, not a check:
	// applyRescan runs synchronously on the loop goroutine, so the blink, a
	// portal join, and the session landings (which legitimately DEPEND on
	// autoconnect to restore the previous network) can never overlap the
	// window — a join submitted mid-bounce queues behind the restore.
	//
	// Do NOT widen it to cover a failed re-raise retried by the tick: NM
	// grabbing a network in that gap is the desired recovery, not the race
	// this exists to remove.
	//
	// The restore is scheduled BEFORE the suppress attempt and regardless of
	// its outcome, because the outcome is not knowable from the return value:
	// no error can distinguish "the flip never applied" from "it applied and
	// the call then failed" (a ctx kill, a timeout, exec teardown after the
	// write reached NM). Conditioning the restore on success is therefore the
	// one shape that can leave the flag off with no latch and no retry — and a
	// long-running daemon never re-runs the startup self-heal, so such a leak
	// would survive until an NM restart, silently removing the autoconnect the
	// session landings depend on. Restoring is idempotent, so being wrong in
	// the other direction costs one nmcli call on a rare error path.
	//
	// Registering the defer first also covers a panic inside the suppress call
	// itself, a failed ensureAPUp, and a panic anywhere in the bounce; the
	// latch inside restoreAutoconnect covers the restore failing.
	//
	// A shutdown landing inside the bounce is the one case this defer cannot
	// finish on its own: ctx is already canceled, so the restore fails and
	// latches on a loop that will never tick again. BOTH the loop's ctx.Done
	// branch and Stop then pay that latch on a fresh bounded context (see
	// payOutstandingAutoconnectRestore for why neither alone is enough) — do
	// not delete either believing Restart=always covers this, because an
	// intentional `systemctl --user stop` is never followed by the start whose
	// self-heal would fix it.
	defer m.restoreAutoconnect(ctx)
	if err := m.wifi.SetDeviceAutoconnect(ctx, false); err != nil {
		// Fail OPEN: the race is a quality problem, the rescan is the feature
		// the user pressed. Proceed with today's unsuppressed bounce.
		m.logger.Warn("provisioning: autoconnect suppression failed; rescan proceeds unsuppressed", zap.Error(err))
	}
	m.ensureAPDown(ctx)
	// State is still StateAPActive, so reconcile re-raises via ensureAPUp: it
	// narrates the scan on-screen, completes a fresh scan pass, then brings the
	// AP (and its QR narration) back.
	m.reconcile(ctx)
}

// restoreAutoconnect re-enables device autoconnect, latching a tick retry when
// the call fails.
//
// It read-modify-writes autoconnectRestorePending, which is UNSYNCHRONIZED, so
// the caller set is closed on purpose: the loop goroutine, or Stop after <-done
// (which orders the loop's last write before it). Do not call it from a request
// goroutine — the portal seams, the hub observer, SetClaimed — because nothing
// orders those against the loop.
//
// A leak is bounded rather than fatal even if every layer above fails: the
// blink and portal joins activate profiles EXPLICITLY and are unaffected by the
// flag, so the only casualty is the session landings' reliance on autoconnect
// to restore the previous network — worst case one sustained-offline window
// plus the first recheck blink, after which the normal machinery recovers. The
// flag itself dies with the next NM restart or reboot.
func (m *Machine) restoreAutoconnect(ctx context.Context) {
	if err := m.wifi.SetDeviceAutoconnect(ctx, true); err != nil {
		// Throttled like the link probe's failure log (linkProbeFailing): the
		// retry runs at tick cadence and NM being down for minutes is the
		// EXPECTED case the startup self-heal exists for — an unthrottled Warn
		// would put 4 lines/minute in the journal for a condition that is
		// already latched and visible.
		if m.autoconnectRestorePending {
			m.logger.Debug("provisioning: autoconnect restore still failing", zap.Error(err))
		} else {
			m.logger.Warn("provisioning: autoconnect restore failed; retrying every tick until it succeeds", zap.Error(err))
		}
		m.autoconnectRestorePending = true
		return
	}
	if m.autoconnectRestorePending {
		m.logger.Info("provisioning: device autoconnect restored")
	}
	m.autoconnectRestorePending = false
}

// payOutstandingAutoconnectRestore issues the `autoconnect on` the latch says is
// owed, on a FRESH bounded context. It exists for shutdown: applyRescan's
// deferred restore runs on the loop context, which a shutdown mid-bounce has
// already canceled, so the restore fails and latches with no tick left to
// service it.
//
// Called from BOTH the loop's ctx.Done branch and Stop, deliberately: neither
// alone covers every shutdown. The loop branch is skipped entirely when
// supervisor.run returns without re-entering loop (a panic recovered under an
// already-canceled ctx; a shutdown landing in the post-panic backoff), and Stop
// is not reached when the caller shuts down by canceling Start's ctx. The call
// is idempotent and latch-gated, so running it twice costs nothing.
//
// Safe on the loop goroutine and from Stop after <-done (the channel close
// orders the loop's last write before Stop's read — including a write made
// while unwinding a panic, which happens on the loop goroutine before the
// supervisor recovers); never call it from anywhere else.
//
// Ordering note: BOTH sites run this BEFORE their shutdown ensureAPDown, on
// purpose. This call is bounded; the profile deletion is not, so the reverse
// order would let a wedged deletion starve the restore until systemd kills the
// unit — re-creating exactly the leak the payment exists to close. Restoring
// while the AP may still be up loses nothing: there is no re-raise at
// shutdown, so NM reconnecting once the AP drops is the desired end state,
// not the race the suppression exists to remove.
func (m *Machine) payOutstandingAutoconnectRestore() {
	if !m.autoconnectRestorePending {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), shutdownRestoreTimeout)
	defer cancel()
	m.restoreAutoconnect(ctx)
	// Escalate a failure HERE, at production level. restoreAutoconnect's own
	// failure log is throttled to Debug once the latch is already set (it
	// assumes a tick will retry), which on this path is both invisible in the
	// journal and untrue — this is the last attempt anything will make.
	//
	// Worded as "could not confirm" deliberately: the latch is also true from
	// construction, so a shutdown before the first tick reaches this having
	// suppressed nothing. The daemon cannot tell that case from a real leak
	// without reading the flag back, so it must not assert one.
	if m.autoconnectRestorePending {
		m.logger.Error("provisioning: could not confirm device autoconnect is enabled at shutdown; " +
			"if it was left off, NM will not auto-reconnect saved profiles until it restarts or the device reboots")
	}
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
	// Session policy is latched from the raise reason at raise time (§4.2's
	// table); reasons outside the table inherit (see latchSessionPolicy).
	if target == StateAPActive {
		m.latchSessionPolicy(d.Reason)
	}
	// Leaving StateOfflineRetrying ends the narrated panel's on-screen life —
	// whatever paints next owns the screen. Without this clear, a narrated
	// boot outage followed by online and a LATER mid-exhibition outage would
	// let the tick repaint boot prose over the deliberately silent exhibition
	// path. Loop-goroutine-only field, deliberately outside the lock.
	if target != StateOfflineRetrying {
		m.episodeNarration = ""
	}
	m.mu.Lock()
	prevState, prevReason := m.state, m.lastReason
	changed := m.state != target ||
		(target == StateUnprovisioned && m.lastReason != d.Reason) ||
		// Constraint 10's extension: reason changes INTO the narrated
		// OfflineRetrying set notify (the §4.4 repaints depend on it);
		// transitions into the silent legs keep today's dedupe — that is
		// what keeps the exhibition path silent.
		(target == StateOfflineRetrying && narratedOfflineReason(d.Reason) && m.lastReason != d.Reason)
	m.state = target
	m.lastReason = d.Reason
	m.mu.Unlock()

	// Constraint 4(b): a narrated OfflineRetrying panel followed by a SILENT
	// reason in the same state would strand as a permanent lie (the
	// notifier's default deliberately leaves the screen as-is there) — e.g.
	// the ap-recheck blink's "returns in a moment" surviving forever over a
	// claimed frame whose link came back without WAN. Emit the explicit
	// silent-hide reason instead; the notifier's handling is
	// narrating-guarded like every other hide.
	if target == StateOfflineRetrying && prevState == StateOfflineRetrying &&
		narratedOfflineReason(prevReason) && silentOfflineReason(d.Reason) {
		m.episodeNarration = ""
		m.notify(target, Detail{Reason: ReasonAPSessionEndedSilent})
	}

	if changed {
		if target == StateOfflineRetrying && narratedOfflineReason(d.Reason) {
			m.episodeNarration = d.Reason
		}
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
	// AP only comes up after the scan pass below completes. Suppressed while a
	// setup_error is latched: this push runs on every retry tick, and
	// alternating scanning/setup_error at 15s cadence would flap the very
	// panel the latch exists to hold steady (§4.6); the latch clears on the
	// first successful raise, whose softap_qr announcement repaints.
	if m.setupErrorLatch == "" {
		m.notify(StateAPActive, Detail{
			Reason:  "scanning",
			Message: "Looking for nearby Wi-Fi networks",
		})
	}

	// A recheck blink's own successful scan moments ago stands in for this
	// pass (one radio pass instead of two, and a fresher portal picker); the
	// flag is one-shot and only ever set off a non-empty, error-free blink
	// scan — an empty post-bounce scan is the documented failure shape the
	// retry loop below exists for.
	skipScan := m.skipNextPreAPScan
	m.skipNextPreAPScan = false

	var scanErr error
	var ssids []string
	for attempt := 1; attempt <= preAPScanAttempts && !skipScan; attempt++ {
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
	if !skipScan && (scanErr != nil || len(ssids) == 0) {
		// Non-fatal after retries: withholding the AP entirely would strand the
		// user. The portal serves whatever the cache still holds — wifictl keeps
		// the previous entries rather than letting an empty scan blank them —
		// and falls back to manual SSID entry only when there is nothing left.
		m.logger.Warn("provisioning: proceeding without a fresh scan result",
			zap.Error(scanErr))
	}

	info, err := m.ap.Up(ctx)
	if err != nil {
		m.noteAPRaiseFailure()
		return err
	}

	srv := m.newPortal(portal.Config{
		Addr:   m.portalAddr,
		APSSID: info.SSID,
		Scan:   m.wifi.CachedScan,
		Join:   m.RequestJoin,
		Status: m.Status,
		Rescan: m.RequestRescan,
		// Human-caused portal requests defer session teardowns (§4.2); the
		// portal classifies (action handlers only), the machine timestamps.
		ActivityObserved: m.observePortalActivity,
		// ANY portal request — probes included — feeds the recheck-only
		// attached-phone deferral (see sessionExpiryDue for the scope).
		TrafficObserved: m.observePortalTraffic,
		Logger:          m.logger,
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
		m.noteAPRaiseFailure()
		return err
	}

	m.mu.Lock()
	m.apUp = true
	m.apInfo = info
	m.portalSrv = srv
	m.mu.Unlock()
	m.clearAPRaiseFailures()
	// Every successful raise (re-)arms the session phase timer under the
	// latched policy — including the join-failure re-raise, whose clock reset
	// is the §4.2 table's "resets its clock".
	m.armSessionTimer()
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
			m.noteAPReleaseFailure()
			return false
		}
		m.mu.Lock()
		m.apDownPending = false
		m.mu.Unlock()
		m.clearAPReleaseFailures()
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
		m.clearAPReleaseFailures()
		m.logger.Info("provisioning: setup AP torn down")
	} else {
		m.noteAPReleaseFailure()
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
	if m.activeLink == nil && m.activeLinkDetail == nil {
		// Nil-guard fabrication: NOT a real probe, so the §4.7 caches stay
		// untouched (they must only ever carry measured evidence).
		return linkAbsent
	}
	m.mu.Lock()
	apUp := m.apUp
	m.mu.Unlock()
	if apUp {
		// Short-circuit while our own AP holds the radio: also not a real
		// probe — writing it would persist a stale absent-with-no-type past
		// the AP phase (the §4.7 stale-type pin).
		return linkAbsent
	}
	var up, wired bool
	var err error
	wiredKnown := false
	if m.activeLinkDetail != nil {
		// Preferred: the same nmcli read also yields the ethernet-only
		// verdict, so the health snapshot learns the link TYPE for free.
		up, wired, err = m.activeLinkDetail(ctx)
		wiredKnown = err == nil
	} else {
		up, err = m.activeLink(ctx)
	}
	outcome := linkUnknown
	if err == nil {
		if up {
			outcome = linkPresent
		} else {
			outcome = linkAbsent
		}
	}
	m.mu.Lock()
	m.linkOutcomeKnown = err == nil
	m.linkOutcome = outcome
	if wiredKnown {
		m.wiredKnown, m.wiredLast = true, wired
	}
	m.mu.Unlock()
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
	return outcome
}

// probeWired wraps the WiredLink seam so every real wire verdict also feeds
// the §4.7 type cache. All wiredLink consumers route through this.
func (m *Machine) probeWired(ctx context.Context) (bool, error) {
	wired, err := m.wiredLink(ctx)
	if err == nil {
		m.mu.Lock()
		m.wiredKnown, m.wiredLast = true, wired
		m.mu.Unlock()
	}
	return wired, err
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

// recordOfflineAbsent counts one confirmed-absent TICK sample toward the
// offline window, arming it on the first. It also works off any post-pause
// freshness debt, so expiry (offlineWindowExpired) is always gated on current
// evidence.
func (m *Machine) recordOfflineAbsent() {
	m.offlinePause = 0
	m.offlineAbsent++
	if m.offlineFreshNeeded > 0 {
		m.offlineFreshNeeded--
	}
}

// armOfflineWindow counts the confirmed absence behind an EVENT-path reading,
// but only ever as the window's FIRST sample. Events can arrive in bursts (a
// sys-monitord restart re-emits its first probe unconditionally; the
// connUnknown re-query feeds one in), and letting each one append a sample
// would let event cadence outrun tick cadence and silently shorten the
// continuous-absence guarantee behind the one-way raise. Tick samples
// (recordOfflineAbsent) are the only ones that accumulate.
func (m *Machine) armOfflineWindow() {
	if m.offlineAbsent == 0 {
		m.recordOfflineAbsent()
	}
}

// offlineWindowExpired reports whether the accumulated confirmed absence
// authorizes the sustained raise: a full window of samples AND no outstanding
// post-pause freshness debt.
func (m *Machine) offlineWindowExpired() bool {
	return m.offlineAbsent >= m.offlineWindowSamples && m.offlineFreshNeeded == 0
}

// pauseOfflineWindow absorbs one INCONCLUSIVE sample (failed link probe): the
// count neither advances nor resets (§4.3's pause-not-reset — the old
// reset-on-unknown let one 3s nmcli timeout restart the whole 5-minute wait
// forever, D7). Two bounds keep the retained accumulation honest, because its
// evidence ages while probing is dark and the raise it feeds is one-way:
//   - a pause longer than one full window discards the accumulation outright
//     (evidence older than a window-length of silence is stale), and
//   - any pause leaves a freshness debt (offlineFreshSamplesAfterPause) that
//     expiry must work off with consecutive fresh confirmed samples.
//
// No-op while disarmed: with nothing accumulated there is nothing to protect.
func (m *Machine) pauseOfflineWindow() {
	if m.offlineAbsent == 0 {
		return
	}
	m.offlinePause++
	if m.offlinePause > m.offlineWindowSamples {
		m.resetOfflineWindow()
		return
	}
	m.offlineFreshNeeded = offlineFreshSamplesAfterPause
}

func (m *Machine) resetOfflineWindow() {
	m.offlineAbsent, m.offlinePause, m.offlineFreshNeeded = 0, 0, 0
}

// clearOffline fully resets the offline window AND disarms the boot relocation
// ladder. It is the single-truth helper for every reading that falsifies the
// premise of both mechanisms at once — going online, a linkPresent sighting,
// an AP raise (docs/network-recovery-ux.md §4.3: "any linkPresent sighting
// resets both mechanisms at once"). Disarming here guarantees a stale armed
// relocation can never survive into a LATER offline episode and bypass the
// sustained-offline window mid-exhibition. Inconclusive readings must NOT come
// here: they pause (pauseOfflineWindow) and charge the ladder's bounded
// tolerance (relocationUnknownSample) instead.
func (m *Machine) clearOffline() {
	m.resetOfflineWindow()
	m.disarmRelocation("offline state cleared (online, link sighted, or AP raised)")
	// Every clearOffline site is an EPISODE boundary (going online, a link
	// sighting — including the failed-raise exits — or a fresh AP raise), so
	// the raise-escalation streak resets here too: a raise episode can exit
	// through link recovery WITHOUT ever succeeding, and a streak surviving
	// that exit would (a) stop measuring CONSECUTIVE failures and (b) leave a
	// stale latch that suppresses both the next wedge's notification and its
	// scanning narration — the second wedge would show nothing at all. The
	// RELEASE streak is deliberately NOT reset: teardown retries continue
	// across resting states until they succeed, so its wedge genuinely spans
	// these boundaries.
	m.resetAPRaiseEscalation()
	// The recheck blink's one-shot scan-skip belongs to the re-raise that
	// immediately follows it and nothing else. Today every set IS consumed by
	// that raise, but the flag is a bare bool with no expiry, so any future
	// path that returns from ensureAPUp before consuming it (the
	// pending-teardown deferral is the near miss) would hand a much later
	// raise a stale portal picker. Clearing it here binds its lifetime to the
	// AP-world boundary every clearOffline site already marks.
	m.skipNextPreAPScan = false
	// The recheck backoff ladder re-bases at the same boundary: a landing —
	// online, a link sighting, a fresh raise after an interlude — means the
	// NEXT recheck session is a new outage and deserves the short early
	// rungs again. The blink's still-gone re-raise deliberately never calls
	// clearOffline (see runRecheckBlink), which is exactly what lets the
	// counter climb toward the 30-minute steady state within one session.
	m.recheckCycle = 0
}

// resetAPRaiseEscalation resets the raise-failure streak and dismisses a
// latched ap_start_failed panel when its episode ends WITHOUT a successful
// raise (see clearOffline). The dismissal is an explicit cleared notify —
// unlike the success path (clearAPRaiseFailures), no softap_qr repaint is
// coming, and the failed-raise exits land in states whose entry reasons the
// notifier deliberately leaves un-repainted (the link-present legs), which
// would strand the error panel as a permanent lie over a recovered device.
func (m *Machine) resetAPRaiseEscalation() {
	m.apRaiseFails = 0
	if m.setupErrorLatch != setupErrorAPStart {
		return
	}
	m.setupErrorLatch = ""
	m.notify(m.State(), Detail{Reason: ReasonSetupErrorCleared})
}

// noteAPRaiseFailure counts one failed AP raise toward the setup_error latch
// (§4.6, D6): a raise that fails persistently (rfkill, an empty
// /etc/hostname, :80 already bound) otherwise shows "scanning" forever while
// reconcile retries at tick cadence with no counter and no escalation. Fires
// the notification exactly once at the streak threshold; retries continue
// underneath throughout.
func (m *Machine) noteAPRaiseFailure() {
	m.apRaiseFails++
	if m.apRaiseFails != setupErrorFailStreak || m.setupErrorLatch != "" {
		return
	}
	m.setupErrorLatch = setupErrorAPStart
	m.logger.Error("provisioning: setup AP raise failing persistently; escalating on screen",
		zap.Int("failures", m.apRaiseFails))
	m.notify(m.State(), Detail{
		Reason: ReasonSetupError,
		Message: "The Art Computer could not start setup mode. It will keep trying automatically. " +
			"If this persists, disconnect power for ten seconds and restart.",
	})
}

// clearAPRaiseFailures resets the raise streak on a successful ap.Up. No
// explicit repaint: the successful raise's own softap_qr announcement (which
// follows in ensureAPUp) is the repaint, so the latch just releases the
// "scanning" suppression.
func (m *Machine) clearAPRaiseFailures() {
	m.apRaiseFails = 0
	if m.setupErrorLatch == setupErrorAPStart {
		m.setupErrorLatch = ""
	}
}

// noteAPReleaseFailure is the teardown twin (§4.6, D5): NM refusing to delete
// the hotspot profile wedges apDownPending forever — the hotspot may still
// broadcast with nothing listening — and the screen shows whatever was last
// painted, indefinitely.
func (m *Machine) noteAPReleaseFailure() {
	m.apReleaseFails++
	if m.apReleaseFails != setupErrorFailStreak || m.setupErrorLatch != "" {
		return
	}
	m.setupErrorLatch = setupErrorAPRelease
	m.logger.Error("provisioning: setup AP teardown failing persistently; escalating on screen",
		zap.Int("failures", m.apReleaseFails))
	m.notify(m.State(), Detail{
		Reason: ReasonSetupError,
		Message: "The Art Computer could not release its setup hotspot. It will keep trying automatically. " +
			"If this persists, disconnect power for ten seconds and restart.",
	})
}

// clearAPReleaseFailures resets the teardown streak on a successful teardown.
// Unlike the raise flavor, no later narration necessarily follows (the machine
// may be sitting in Online/OfflineRetrying, whose own transitions are deduped),
// so a latched panel is dismissed explicitly via ReasonSetupErrorCleared —
// except from StateAPActive, where the imminent raise repaints on its own.
func (m *Machine) clearAPReleaseFailures() {
	m.apReleaseFails = 0
	if m.setupErrorLatch != setupErrorAPRelease {
		return
	}
	m.setupErrorLatch = ""
	if st := m.State(); st != StateAPActive {
		m.notify(st, Detail{Reason: ReasonSetupErrorCleared})
	}
}

// relocationUnknownSample charges one inconclusive link-probe sample against
// the boot relocation ladder's tolerance. The ladder's scan evidence is only
// as trustworthy as the link probes interleaved with it, so persistent
// unknowns forfeit — but only past relocUnknownTolerance, where the old
// behavior forfeited on the very first one (D7's second half: a single 3s
// nmcli timeout killed the boot-scoped feature for the whole boot).
func (m *Machine) relocationUnknownSample() {
	if m.relocConfirms == 0 && m.relocArmTriesLeft == 0 {
		return
	}
	m.relocUnknowns++
	if m.relocUnknowns > relocUnknownTolerance {
		m.disarmRelocation("forfeited after repeated inconclusive link probes")
	}
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
