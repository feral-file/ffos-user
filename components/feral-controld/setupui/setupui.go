// Package setupui pushes best-effort on-screen narration of device setup
// progress to ff-player (the kiosk Chromium app) over CDP.
//
// Narration is fire-and-forget by design. The phone-side captive portal is the
// real provisioning channel; the on-screen story is a courtesy for a bystander
// watching the panel. Chromium may be slow to load, crashed, or entirely absent
// (headless device) during provisioning, so every push here MUST tolerate the
// player not being reachable: pushes never block the caller, never return a
// fatal error, and never panic. A dead or absent screen must never gate or
// crash the provisioning state machine.
package setupui

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-controld/cdp"
)

// DefaultContractPath is the on-device location of the player capability
// manifest. It mirrors mintpairing's contract path; both narration surfaces are
// gated by the same file.
const DefaultContractPath = "/opt/feral/feral-player/ffos-player-contract.json"

const setupDisplayCommand = "setupDisplay"

// Setup narration states. These match the player-side contract exactly. The
// player accepts unknown state strings with {ok:true} and renders nothing, so
// adding a future state here is safe even against an older player.
const (
	stateSoftAPQR   = "softap_qr"
	stateJoining    = "joining"
	stateJoinFailed = "join_failed"
	stateUpdating   = "updating"
	stateClaimQR    = "claim_qr"
	stateReady      = "ready"
	stateHidden     = "hidden"

	// stateScanning is an extension state narrating the pre-AP Wi-Fi scan (the
	// device is looking for nearby networks before advertising its setup
	// hotspot). Like stateFactoryReset below it is deliberately NOT in the
	// required validation set: fielded players that predate it accept it as a
	// no-op ({ok:true}, renders nothing) and keep full narration support.
	stateScanning = "scanning"

	// stateFinalizing is an extension state covering the gap between a
	// successful Wi-Fi join and the claim QR: relayer topic wait plus the
	// pre-claim OTA version check (and its retries). Without it the screen is
	// black for those seconds with no hint that setup is still progressing.
	stateFinalizing = "finalizing"

	// stateFactoryReset is an extension state used by the in-process factory-reset
	// flow. It is deliberately NOT in the required set that
	// validateSetupDisplayContract checks: the currently-shipping player manifest
	// does not list it, and requiring it would fail the gate and disable ALL setup
	// narration on fielded players. Players that predate the corresponding
	// ff-player renderer accept it as a no-op ({ok:true}, renders nothing); once
	// ff-player adds the state it renders. This is the contract-level extensibility
	// path in action.
	stateFactoryReset = "factory_reset"

	// statePageReload is an INTERNAL queue sentinel, never sent to the player:
	// it marks a RequestPageReload entry riding the narration lane so the
	// worker executes it in order with narration pushes. It is not narration
	// intent — enqueueing it does not touch last, and it is invisible to
	// Narrating/Resync.
	statePageReload = "__page_reload"

	// reloadDoneKey holds a RequestPageReload entry's outcome callback.
	reloadDoneKey = "__done"
)

// defaultReadyPollInterval / defaultReadyPollTimeout bound the post-reload
// readiness poll (see awaitPageReady). The timeout is generous relative to a
// kiosk page boot (~seconds); after it, narration resumes best-effort — the
// pre-reload world, no worse.
const (
	defaultReadyPollInterval = 250 * time.Millisecond
	defaultReadyPollTimeout  = 15 * time.Second
)

// support tracks the one-shot manifest capability decision for the process
// lifetime. Once resolved it never flips: an older player that predates the
// setupDisplay contract yields a permanent no-narration fallback.
type support int

const (
	supportUnknown support = iota
	supportYes
	supportNo
)

// CDPSender is the narrow slice of the CDP client this package needs. Owning a
// single-method interface here keeps the seam injectable for tests without
// pulling in the full CDP surface. cdp.CDP satisfies it.
type CDPSender interface {
	NoLogSend(method string, params map[string]interface{}) (interface{}, error)
}

// Service renders setup narration to the player. Its typed Show* methods are
// safe to call from any goroutine and return immediately; the actual CDP push
// happens on a background worker.
type Service struct {
	cdp          CDPSender
	contractPath string
	// warnedUnreadable rate-limits the contract-unreadable Warn to once per
	// process; the re-check itself happens on every push (see
	// narrationSupported).
	warnedUnreadable bool
	logger           *zap.Logger

	// readyPollInterval / readyPollTimeout override the post-reload readiness
	// poll bounds (zero means the defaults). Immutable after construction:
	// read lock-free on the worker goroutine, so tests must set them before
	// the first push/reload spawns a worker.
	readyPollInterval time.Duration
	readyPollTimeout  time.Duration

	mu      sync.Mutex
	support support
	// last is the most recently intended narration state. It is retained (not
	// cleared after sending) so it can be re-pushed when CDP reconnects.
	last map[string]any
	// pending is the ordered queue of states the worker still needs to push.
	// Coalescing rule: a new push REPLACES a queued entry with the same "state"
	// value in place (so an OTA progress burst collapses to one trailing
	// "updating" send), but DISTINCT states are all delivered in order. The
	// distinction matters: the claim flow's ShowReady()+Hide() are two
	// back-to-back different states and both must reach the player — a
	// single-slot newest-intent-wins design silently dropped the Ready. Bounded
	// by maxPendingStates (drop-oldest) purely as a leak guard; in practice the
	// setup flow never queues more than a handful of distinct states.
	pending []map[string]any
	// running guards against spawning more than one worker goroutine at a time.
	running bool
}

// maxPendingStates bounds the pending queue. Setup narration has 10 distinct
// states total (including the scanning/finalizing/factory_reset extensions),
// so a deeper queue only ever means a stalled CDP send; dropping the oldest
// intent is the correct staleness policy for a courtesy overlay.
const maxPendingStates = 12

// New builds a narration Service. A blank contractPath falls back to
// DefaultContractPath. logger may be nil (narration then stays silent about its
// own failures, which is acceptable for a best-effort surface).
func New(sender CDPSender, contractPath string, logger *zap.Logger) *Service {
	if strings.TrimSpace(contractPath) == "" {
		contractPath = DefaultContractPath
	}
	if logger == nil {
		logger = zap.NewNop()
	}
	return &Service{
		cdp:          sender,
		contractPath: contractPath,
		logger:       logger,
	}
}

// ShowSoftAPQR narrates the soft-AP onboarding step: the phone should join the
// device's setup Wi-Fi. ssid is required; psk (the WPA2 passphrase) is optional
// and omitted from the payload when blank.
func (s *Service) ShowSoftAPQR(ssid string, psk string) {
	req := map[string]any{
		"state": stateSoftAPQR,
		"ssid":  ssid,
	}
	if strings.TrimSpace(psk) != "" {
		req["password"] = psk
	}
	s.push(req)
}

// ShowScanning narrates the pre-AP Wi-Fi scan: the device is searching for
// nearby networks and will advertise its setup hotspot once the scan
// completes. Extension state; older players render nothing (see
// stateScanning).
func (s *Service) ShowScanning() {
	s.push(map[string]any{"state": stateScanning})
}

// ShowJoining narrates that the device is attempting to join the chosen Wi-Fi.
func (s *Service) ShowJoining() {
	s.push(map[string]any{"state": stateJoining})
}

// ShowJoinFailed narrates a failed Wi-Fi join. reason is optional context and
// is omitted from the payload when blank.
func (s *Service) ShowJoinFailed(reason string) {
	req := map[string]any{"state": stateJoinFailed}
	if strings.TrimSpace(reason) != "" {
		req["reason"] = reason
	}
	s.push(req)
}

// ShowUpdating narrates an in-progress OTA update. progress is a percent
// (0-100).
func (s *Service) ShowUpdating(progress int) {
	s.push(map[string]any{
		"state":    stateUpdating,
		"progress": progress,
	})
}

// ShowFinalizing narrates the post-join finalization window (topic wait +
// pre-claim version check) so the user sees progress instead of a black
// screen. Extension state; older players render nothing (see stateFinalizing).
func (s *Service) ShowFinalizing() {
	s.push(map[string]any{"state": stateFinalizing})
}

// ShowClaimQR narrates the final claim step, rendering url as a QR code. The
// caller constructs the device_connect URL; this package passes it through
// verbatim as the required url field. deviceName, when non-blank, is the
// mDNS-advertised name (e.g. "FF1-8EVTK3RE") the player weaves into the
// "open the app on the same Wi-Fi and it finds this frame automatically"
// guidance — the QR itself is the backup path.
func (s *Service) ShowClaimQR(url string, deviceName string) {
	req := map[string]any{
		"state": stateClaimQR,
		"url":   url,
	}
	if strings.TrimSpace(deviceName) != "" {
		req["device_name"] = deviceName
	}
	s.push(req)
}

// ShowReady narrates that setup completed successfully.
func (s *Service) ShowReady() {
	s.push(map[string]any{"state": stateReady})
}

// ShowFactoryReset narrates that an in-process factory reset is underway, before
// the device reboots into the factory snapshot. Like every push here it is
// best-effort: it must never gate or delay the reset itself. It uses the
// extension state stateFactoryReset, which is safe to send to players that do
// not yet render it (see the constant's note).
func (s *Service) ShowFactoryReset() {
	s.push(map[string]any{"state": stateFactoryReset})
}

// Hide clears any setup narration overlay, returning the player to its default
// display.
func (s *Service) Hide() {
	s.push(map[string]any{"state": stateHidden})
}

// SweepStaleOverlay hides the on-screen overlay ONLY if this process has not
// narrated anything yet. It exists for boot reconciliation: narration intent
// is in-memory, so after a daemon restart the player may still render the
// PREVIOUS life's overlay (e.g. a claim QR painted before a crash on a device
// that has since been claimed) — but an overlay THIS process painted is, by
// definition, not stale. last == nil is exactly the complement of what
// Resync can repair: any non-nil intent gets re-pushed on the next CDP
// (re)connect — even one whose original send failed, since a send failure
// triggers the reconnect that fires Resync — so the sweep covers precisely
// the one state Resync cannot, no more. The no-intent check and the hide are
// one critical section under the same mutex every push takes (pushIf), so a
// concurrent narrator (the startup OTA gate's ShowUpdating, a factory reset)
// can never have its live narration erased by the sweep: if its push wins
// the lock, the sweep no-ops; if the sweep wins, the push is enqueued after
// the hide and the screen still ends on the narration. A caller-side
// Narrating() probe followed by Hide() would reintroduce exactly that
// check-then-act race.
func (s *Service) SweepStaleOverlay() {
	s.pushIf(map[string]any{"state": stateHidden}, func(last map[string]any) bool { return last == nil })
}

// HideIfShowing hides only when the current narration intent is one of
// states, so a flow can clear the narration IT painted without erasing a
// concurrent narrator's. Same atomicity argument as SweepStaleOverlay: the
// state comparison and the hide share every push's critical section, so a
// racing ShowUpdating/ShowFactoryReset either lands first (the hide then
// no-ops) or is queued behind the hide (and the screen still ends on it).
// The canonical caller is the auto-claim flow clearing its own finalizing
// overlay after discovering mid-flow that the device settled — the exact
// moment another narrator may have taken the screen.
func (s *Service) HideIfShowing(states ...string) {
	s.pushIf(map[string]any{"state": stateHidden}, func(last map[string]any) bool {
		current := stringField(last, "state")
		for _, st := range states {
			if current == st {
				return true
			}
		}
		return false
	})
}

// StateFinalizing is exported for HideIfShowing callers that need to name the
// narration they own (the auto-claim flow's post-join gap overlay).
const StateFinalizing = stateFinalizing

// Narrating reports whether the last intended narration state is a visible
// overlay — something has been shown and it was not subsequently hidden. It
// reflects INTENT (the last push), not delivery: a push whose CDP send failed
// still counts, which is the conservative reading for callers deciding whether
// a destructive page operation would erase someone's narration.
func (s *Service) Narrating() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.last != nil && stringField(s.last, "state") != stateHidden
}

// RequestPageReload enqueues a browser-level Page.reload into the SAME
// serialized lane as narration pushes, executing it only if no visible
// narration is intended by the time the worker reaches it. Riding the
// narration queue is what makes the check atomic: the worker is the sole
// executor, so any narration push racing the caller's decision either landed
// BEFORE the reload entry (the execution-time check then sees it and skips)
// or sits BEHIND it in the queue and repaints after the reload — reliably so,
// because an executed reload holds the lane until the replacement document is
// ready (awaitPageReady), so the repaint cannot evaluate into the dying
// document and be lost. Neither interleaving can erase an overlay — the
// caller-side probe-then-Send pattern this replaces could.
//
// The skip decision reads INTENT (the last push), not delivery, the same
// conservative bias Narrating documents. done, when non-nil, is invoked
// exactly once — executed=false/err=nil when the reload was skipped (visible
// narration, or the entry was superseded by a Resync/queue-overflow drop:
// both mean the page state was or is being repainted anyway), executed=true
// with the send's error otherwise. It runs on the narration lane and must not
// block.
func (s *Service) RequestPageReload(done func(executed bool, err error)) {
	req := map[string]any{"state": statePageReload, reloadDoneKey: done}
	s.mu.Lock()
	var dropped func(executed bool, err error)
	if len(s.pending) >= maxPendingStates {
		dropped = reloadDoneOf(s.pending[0])
		s.pending = s.pending[1:]
	}
	s.pending = append(s.pending, req)
	starting := !s.running
	s.running = true
	s.mu.Unlock()
	if dropped != nil {
		dropped(false, nil)
	}
	if starting {
		go s.worker()
	}
}

// reloadDoneOf extracts a queue entry's RequestPageReload callback, or nil for
// narration entries (and sentinel entries whose caller passed no callback).
func reloadDoneOf(req map[string]any) func(executed bool, err error) {
	if stringField(req, "state") != statePageReload {
		return nil
	}
	fn, _ := req[reloadDoneKey].(func(executed bool, err error))
	return fn
}

// execPageReload runs a dequeued RequestPageReload entry on the worker
// goroutine. The narration re-check happens HERE, ordered after every earlier
// push's effect on last — see RequestPageReload for the atomicity argument.
// Deliberately not gated on narrationSupported: this is a page operation, not
// narration, and must work against players that predate the setupDisplay
// contract.
func (s *Service) execPageReload(req map[string]any) {
	done := reloadDoneOf(req)
	if s.Narrating() {
		if done != nil {
			done(false, nil)
		}
		return
	}
	// Stamp a per-reload nonce into the CURRENT document before navigating.
	// The old and new documents run the SAME player app, so handler presence
	// alone cannot tell them apart — a readiness probe answered by the dying
	// document would resume narration into it (the exact loss this gate
	// prevents). The nonce cannot survive the navigation, so "handler present
	// AND nonce absent" provably identifies the replacement document.
	// Best-effort: the common reload reason is a dead page, where this
	// evaluate may fail — a failed stamp only weakens the probe back to
	// handler-presence (the dead page has no handler to answer with), it
	// never blocks the reload.
	nonce := strconv.FormatInt(time.Now().UnixNano(), 36)
	if _, stampErr := s.cdp.NoLogSend(cdp.METHOD_EVALUATE, map[string]interface{}{
		"expression":    fmt.Sprintf(`JSON.stringify({nonce: window.__ffosReloadNonce = %q})`, nonce),
		"returnByValue": true,
	}); stampErr != nil {
		// Keep the degraded mode diagnosable: a transient failure on a LIVE
		// page (socket blip) silently weakens the probe, and any narration
		// lost to a fooled probe would otherwise be untraceable in the field.
		s.logger.Debug("Reload nonce stamp failed; readiness probe degraded to handler presence",
			zap.Error(stampErr))
	}
	_, err := s.cdp.NoLogSend("Page.reload", map[string]interface{}{})
	if done != nil {
		done(true, err)
	}
	if err == nil {
		// Page.reload only ACCEPTS the navigation — the old document is still
		// being torn down when it returns, and an evaluate sent now can execute
		// in that dying context and be silently lost (a same-target reload
		// never drops the DevTools socket, so no reconnect Resync would repair
		// it). Hold the lane until the REPLACEMENT document is ready before any
		// queued narration is delivered. Blocking is safe here: this runs on
		// the worker goroutine, and pushes stay non-blocking enqueues.
		s.awaitPageReady(nonce)
	}
}

// awaitPageReady polls the reloaded page until the player app has installed
// its command handler in a document that is NOT the pre-reload one (see the
// nonce rationale in execPageReload), bounding the wait with readyPollTimeout.
// On timeout, narration resumes best-effort — exactly the delivery guarantee
// it had before the reload existed.
//
// The probe deliberately evaluates to a JSON STRING: the cdp client decodes
// only type:"string" (JSON-unmarshaled) and type:"object" evaluate results and
// rejects everything else, so a bare boolean expression would error on every
// poll and the gate would never pass on a real device.
func (s *Service) awaitPageReady(nonce string) {
	interval := s.readyPollInterval
	if interval <= 0 {
		interval = defaultReadyPollInterval
	}
	timeout := s.readyPollTimeout
	if timeout <= 0 {
		timeout = defaultReadyPollTimeout
	}
	probe := fmt.Sprintf(
		`JSON.stringify({ready: typeof window.handleCDPRequest === 'function' && window.__ffosReloadNonce !== %q})`,
		nonce)
	deadline := time.Now().Add(timeout)
	for {
		result, err := s.cdp.NoLogSend(cdp.METHOD_EVALUATE, map[string]interface{}{
			"expression":    probe,
			"returnByValue": true,
		})
		if err == nil && readyReported(result) {
			return
		}
		if time.Now().After(deadline) {
			s.logger.Info("Reloaded page did not report ready in time; narration resumes best-effort")
			return
		}
		time.Sleep(interval)
	}
}

// readyReported reads the readiness probe's decoded answer ({"ready": bool},
// arriving as the map the cdp client produces for JSON-string evaluates).
func readyReported(result any) bool {
	m, ok := result.(map[string]any)
	if !ok {
		return false
	}
	v, _ := m["ready"].(bool)
	return v
}

// Resync re-pushes the last intended narration state. It is the "CDP became
// available" trigger: wire it to the CDP client's on-connect callback so a
// reconnecting or freshly-loaded player catches up to the current setup state.
// It is a no-op if nothing has been shown yet.
func (s *Service) Resync() {
	s.mu.Lock()
	if s.last == nil {
		s.mu.Unlock()
		return
	}
	// A reconnect only needs the CURRENT intent; anything still queued is
	// superseded by re-painting the latest state. A superseded reload sentinel
	// resolves as skipped: a reconnect means the page just (re)loaded, so the
	// reload it was queued for is moot.
	var skipped []func(executed bool, err error)
	for _, q := range s.pending {
		if fn := reloadDoneOf(q); fn != nil {
			skipped = append(skipped, fn)
		}
	}
	s.pending = []map[string]any{s.last}
	starting := !s.running
	s.running = true
	s.mu.Unlock()
	for _, fn := range skipped {
		fn(false, nil)
	}
	if starting {
		go s.worker()
	}
}

// push enqueues req (see the pending field for the coalescing rule) and ensures
// a worker is draining the queue. It returns immediately; the CDP send never
// happens on the caller's goroutine. Retry policy is deliberately "retry on
// next change", not a hot loop: a failed send is not re-attempted on its own.
// The last state is retained for Resync so a later CDP reconnect can recover it.
func (s *Service) push(req map[string]any) {
	s.pushIf(req, nil)
}

// pushIf is the single enqueue-and-start path every narration intent takes:
// when ok is non-nil it is evaluated UNDER the queue mutex and a false answer
// drops the push entirely. Keeping the conditional ops (SweepStaleOverlay,
// HideIfShowing) on this exact path — rather than duplicating the tail — is
// what makes their "atomic with every push" claim structural: a future change
// to queue policy or worker-spawn discipline cannot apply to plain pushes
// alone and leave the conditional ops silently divergent.
//
// ok receives the current intent (s.last) as its argument BECAUSE it runs
// while s.mu is held: handing the guarded state in leaves the predicate no
// reason to reach back into the Service, whose exported methods take the same
// non-reentrant mutex — a predicate calling one (e.g. Narrating) would
// deadlock holding s.mu and wedge every push from every goroutine, breaking
// the package's pushes-never-block contract.
func (s *Service) pushIf(req map[string]any, ok func(last map[string]any) bool) {
	s.mu.Lock()
	if ok != nil && !ok(s.last) {
		s.mu.Unlock()
		return
	}
	s.last = req
	dropped := s.enqueueLocked(req)
	starting := !s.running
	s.running = true
	s.mu.Unlock()
	if dropped != nil {
		// An overflow-evicted reload sentinel resolves as skipped (see
		// RequestPageReload); narration entries drop silently as before.
		dropped(false, nil)
	}
	if starting {
		go s.worker()
	}
}

// enqueueLocked applies the coalescing rule: a push matching the TRAILING
// queued state replaces it in place (newest payload); everything else appends
// in arrival order. Trailing-only is load-bearing: replacing a same-state
// entry buried under LATER states would reorder narration — queued
// softap_qr→joining→join_failed plus a fresh softap_qr would deliver the new
// QR FIRST and leave the screen on the obsolete failure. The screen must
// always END on the newest state, so a repeat after intervening states
// re-appends. Bursts that matter for coalescing (OTA progress) are contiguous,
// so they still collapse to one trailing entry. Caller holds mu.
//
// The returned callback is non-nil only when the overflow guard evicted a
// reload sentinel; the caller must invoke it after releasing mu.
func (s *Service) enqueueLocked(req map[string]any) func(executed bool, err error) {
	state := stringField(req, "state")
	if n := len(s.pending); n > 0 && stringField(s.pending[n-1], "state") == state {
		s.pending[n-1] = req
		return nil
	}
	var dropped func(executed bool, err error)
	if len(s.pending) >= maxPendingStates {
		dropped = reloadDoneOf(s.pending[0])
		s.pending = s.pending[1:]
	}
	s.pending = append(s.pending, req)
	return dropped
}

// worker drains pending narration states one at a time, in order, until the
// queue is empty. Same-state bursts (OTA progress) collapse via enqueueLocked;
// distinct states each get their own send so ordered sequences like
// Ready→Hidden are delivered, not coalesced away.
func (s *Service) worker() {
	for {
		s.mu.Lock()
		if len(s.pending) == 0 {
			s.running = false
			s.mu.Unlock()
			return
		}
		req := s.pending[0]
		s.pending = s.pending[1:]
		s.mu.Unlock()

		if stringField(req, "state") == statePageReload {
			s.execPageReload(req)
			continue
		}
		s.trySend(req)
	}
}

// trySend performs one best-effort CDP push. All failures are logged and
// swallowed; nothing here is fatal to provisioning.
func (s *Service) trySend(req map[string]any) {
	if !s.narrationSupported() {
		return
	}

	payload, err := json.Marshal(map[string]any{
		"command": setupDisplayCommand,
		"request": req,
	})
	if err != nil {
		// A non-serializable request is a programming error, but narration must
		// still not be fatal; log and move on.
		s.logger.Debug("Failed to marshal setup narration payload", zap.Error(err), zap.Any("request", req))
		return
	}

	result, err := s.cdp.NoLogSend(cdp.METHOD_EVALUATE, map[string]interface{}{
		"expression":    "window.handleCDPRequest(" + string(payload) + ")",
		"returnByValue": true,
	})
	if err != nil {
		// Expected while Chromium is still loading or has crashed during setup —
		// but Info, not Debug: narration sends use NoLogSend (the payload embeds
		// the AP PSK), so this line is the ONLY production trace of a state that
		// never reached the screen.
		s.logger.Info("Setup narration push failed", zap.Error(err), zap.String("state", stringField(req, "state")))
		return
	}
	if err := validateSetupDisplayResult(result); err != nil {
		// The player actively rejected the state: a contract violation, not a
		// timing hiccup.
		s.logger.Warn("Setup narration push rejected", zap.Error(err), zap.String("state", stringField(req, "state")))
		return
	}
	// Positive confirmation the state reached the screen (NoLogSend hides the
	// payload, so this is the only production trace). updating stays quiet: its
	// per-percent pushes would flood the log across an OTA.
	if state := stringField(req, "state"); state != stateUpdating {
		s.logger.Info("Setup narration pushed", zap.String("state", state))
	}
}

// narrationSupported resolves, once, whether the player advertises setupDisplay
// support. The decision is cached for the process lifetime: an older player
// yields a permanent no-narration fallback so narration-disabled is
// indistinguishable from narration-working from the state machine's side.
func (s *Service) narrationSupported() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch s.support {
	case supportYes:
		return true
	case supportNo:
		return false
	}
	if err := validateSetupDisplayContract(s.contractPath); err != nil {
		if errors.Is(err, errContractUnreadable) {
			// Do NOT latch supportNo on a read failure: the very first push
			// fires within seconds of boot (provisioning starts before CDP),
			// and the player bundle/rootfs may not be readable at that exact
			// instant — or an OTA may be mid-replace of the bundle. Latching
			// would permanently kill narration for the process. Stay
			// undecided: this push is skipped and the next one re-checks. A
			// genuinely absent manifest keeps narration off through this same
			// path, at the cost of one file read per push attempt.
			if !s.warnedUnreadable {
				s.warnedUnreadable = true
				s.logger.Warn("Setup narration deferred: player contract unreadable; re-checking on the next push",
					zap.Error(err), zap.String("path", s.contractPath))
			}
			return false
		}
		s.support = supportNo
		// Logged exactly once, at Warn: expected on players that predate the
		// setupDisplay contract, but on a SoftAP-era device it means NO setup
		// narration (no QR, no join feedback) — that must be findable in
		// production logs, which run at Info.
		s.logger.Warn("Setup narration disabled: player contract lacks setupDisplay support",
			zap.Error(err), zap.String("path", s.contractPath))
		return false
	}
	s.support = supportYes
	return true
}

func validateSetupDisplayResult(result any) error {
	response, err := normalizeEvaluationResult(result)
	if err != nil {
		return err
	}
	ok, hasOK := response["ok"].(bool)
	if !hasOK {
		return fmt.Errorf("setup display response missing ok: %v", response)
	}
	if !ok {
		return fmt.Errorf("setup display rejected request: %v", response)
	}
	return nil
}

// normalizeEvaluationResult unwraps the nested Runtime.evaluate envelope down to
// the player's {ok:...} application response. It mirrors the tolerant unwrapping
// used by the mint-pairing display path so both narration surfaces accept the
// same result shapes.
func normalizeEvaluationResult(result any) (map[string]any, error) {
	resultMap, ok := result.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("setup display returned unsupported result type %T", result)
	}
	if _, hasException := resultMap["exceptionDetails"]; hasException {
		return nil, fmt.Errorf("setup display evaluation raised exception: %v", resultMap["exceptionDetails"])
	}
	if _, hasOK := resultMap["ok"]; hasOK {
		return resultMap, nil
	}
	if message, ok := resultMap["message"]; ok {
		return normalizeEvaluationResult(message)
	}
	if value, ok := resultMap["value"]; ok {
		return normalizeEvaluationValue(value)
	}

	rawResult, hasResult := resultMap["result"]
	if !hasResult {
		return resultMap, nil
	}
	rawResultMap, ok := rawResult.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("setup display returned malformed Runtime.evaluate result: %v", resultMap)
	}
	if _, hasException := rawResultMap["exceptionDetails"]; hasException {
		return nil, fmt.Errorf("setup display evaluation raised exception: %v", rawResultMap["exceptionDetails"])
	}
	if nested, ok := rawResultMap["result"]; ok {
		return normalizeEvaluationResult(nested)
	}
	if value, ok := rawResultMap["value"]; ok {
		return normalizeEvaluationValue(value)
	}
	return nil, fmt.Errorf("setup display returned unsupported Runtime.evaluate result: %v", rawResultMap)
}

func normalizeEvaluationValue(value any) (map[string]any, error) {
	if raw, ok := value.(string); ok {
		var decoded map[string]any
		if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
			return nil, fmt.Errorf("decode setup display response: %w", err)
		}
		return decoded, nil
	}
	return normalizeEvaluationResult(value)
}

type playerContractManifest struct {
	Contracts map[string]playerDisplayContract `json:"contracts"`
}

type playerDisplayContract struct {
	Version          int                            `json:"version"`
	RequestKey       string                         `json:"requestKey"`
	States           []string                       `json:"states"`
	AcceptedResponse playerContractAcceptedResponse `json:"acceptedResponse"`
}

type playerContractAcceptedResponse struct {
	OK bool `json:"ok"`
}

// errContractUnreadable marks a validation failure caused by failing to READ
// the manifest, as opposed to a successfully-read manifest that lacks
// setupDisplay support. The two must not be conflated: unreadable may be
// transient (boot ordering, OTA mid-replace) and is retried, while a read
// manifest without the contract latches narration off for the process.
var errContractUnreadable = errors.New("player contract unreadable")

// validateSetupDisplayContract reports whether the player manifest at path
// advertises the setupDisplay contract this Service speaks. It mirrors
// mintpairing's contract validation mechanism (read the manifest from the
// filesystem, not over HTTP).
func validateSetupDisplayContract(path string) error {
	path = strings.TrimSpace(path)
	if path == "" {
		return fmt.Errorf("player contract path is empty")
	}
	raw, err := os.ReadFile(path) //nolint:gosec // Production uses the fixed player contract path; tests inject temp files.
	if err != nil {
		return fmt.Errorf("%w: %w", errContractUnreadable, err)
	}
	var manifest playerContractManifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return fmt.Errorf("decode player contract: %w", err)
	}
	contract, ok := manifest.Contracts["setupDisplay"]
	if !ok {
		return fmt.Errorf("missing contracts.setupDisplay")
	}
	if contract.Version != 1 {
		return fmt.Errorf("contracts.setupDisplay.version must be 1")
	}
	if contract.RequestKey != "request" {
		return fmt.Errorf(`contracts.setupDisplay.requestKey must be "request"`)
	}
	states := make(map[string]bool, len(contract.States))
	for _, state := range contract.States {
		states[state] = true
	}
	for _, required := range []string{stateSoftAPQR, stateJoining, stateJoinFailed, stateUpdating, stateClaimQR, stateReady, stateHidden} {
		if !states[required] {
			return fmt.Errorf("contracts.setupDisplay.states missing %q", required)
		}
	}
	if !contract.AcceptedResponse.OK {
		return fmt.Errorf("contracts.setupDisplay.acceptedResponse.ok must be true")
	}
	return nil
}

func stringField(req map[string]any, key string) string {
	if v, ok := req[key].(string); ok {
		return v
	}
	return ""
}
