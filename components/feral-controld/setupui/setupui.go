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
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-controld/cdp"
	"github.com/feral-file/ffos-user/components/feral-controld/playersession"
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
)

// defaultNavigationParkPollInterval / defaultNavigationParkTimeout bound the
// worker's park while a playersession.Session recovery navigation is pending
// (see parkForNavigation). The timeout mirrors defaultReadyPollTimeout's
// precedent: bounded so a page that never installs its handler cannot park
// narration for the process — see the NavigationPending doc for why an
// unconditional park would reintroduce the SoftAP-QR-goes-dark failure.
const (
	defaultNavigationParkPollInterval = 100 * time.Millisecond
	defaultNavigationParkTimeout      = 15 * time.Second
)

// NavigationSession is the narrow slice of playersession.Session the
// narration worker consults to avoid racing a recovery navigation —
// consumer-owned, mirroring the CDPSender idiom. *playersession.Session
// satisfies it. NavigationTargetGeneration is needed alongside
// StageReady/NavigationPending/Generation — see parkForNavigation's doc for
// why.
type NavigationSession interface {
	NavigationPending() bool
	StageReady(st playersession.Stage) bool
	Generation() uint64
	// NavigationTargetGeneration reports the generation ID the in-flight
	// navigation bumped to, or 0 when no navigation is in flight past its
	// own bump. See playersession.Session.NavigationTargetGeneration's doc.
	NavigationTargetGeneration() uint64
}

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

	// session, when set, is the playersession.Session the worker parks
	// narration sends against (see parkForNavigation). Nil in every existing
	// test and in any wiring that predates the session — the worker then
	// never parks, so existing behavior is exactly preserved. Immutable after
	// construction, same read-lock-free contract as the poll bounds above.
	session NavigationSession
	// navigationParkPollInterval / navigationParkTimeout override the park
	// bounds (zero means the defaults); same test-only, pre-worker-spawn
	// contract as the poll bounds above.
	navigationParkPollInterval time.Duration
	navigationParkTimeout      time.Duration

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

// SetSession wires the playersession.Session the worker consults to avoid
// racing a recovery navigation (see NavigationSession and parkForNavigation).
// Call once at wiring time, before the first push spawns a worker — the field
// is read lock-free on that goroutine. Passing nil (or never calling this) is
// safe and preserves pre-session behavior exactly: the worker never parks.
func (s *Service) SetSession(session NavigationSession) {
	s.session = session
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

// StateUpdating is exported for HideIfShowing callers that need to name the
// OTA update narration (the narrator-policy dispatch's settled-device
// permanent-failure path and the post-ladder reboot watchdog — design doc
// §3.3 — both clear "updating" without erasing a concurrent narrator's
// overlay).
const StateUpdating = stateUpdating

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
	// [minor #14] Resync now also runs as a generation-ready reconciler (on
	// EVERY document replacement, not just the original CDP on-connect
	// wiring), so it can fire while a genuine multi-state sequence is still
	// queued (e.g. the claim flow's ShowReady()+Hide(), two DISTINCT states
	// that must both reach the player — see the pending field's doc). The
	// old unconditional overwrite collapsed that queue down to s.last,
	// silently dropping the Ready. A non-empty queue is left alone here:
	// those states will still deliver in order once the worker's park
	// (parkForNavigation) releases post-generation-ready, so there is
	// nothing for Resync to add. Only an EMPTY queue means there is
	// genuinely nothing in flight for the new document to catch up on, and
	// re-enqueuing the current intent is what a reconnect/new-generation
	// resync is for.
	if len(s.pending) > 0 {
		s.mu.Unlock()
		return
	}
	s.pending = []map[string]any{s.last}
	starting := !s.running
	s.running = true
	s.mu.Unlock()
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
	s.enqueueLocked(req)
	starting := !s.running
	s.running = true
	s.mu.Unlock()
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
// Overflow (maxPendingStates) silently drops the oldest queued entry — the
// correct staleness policy for a courtesy overlay.
func (s *Service) enqueueLocked(req map[string]any) {
	state := stringField(req, "state")
	if n := len(s.pending); n > 0 && stringField(s.pending[n-1], "state") == state {
		s.pending[n-1] = req
		return
	}
	if len(s.pending) >= maxPendingStates {
		s.pending = s.pending[1:]
	}
	s.pending = append(s.pending, req)
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

		s.parkForNavigation()
		s.trySend(req)
	}
}

// parkForNavigation blocks the worker while a playersession.Session recovery
// navigation is pending [NV4], so a narration send cannot race the page
// underneath it. It is a no-op when no session is wired (SetSession never
// called), which is every existing test and any pre-session build. The park
// exits on whichever comes FIRST: the navigation's TARGET generation reaching
// StageHandler; NavigationPending clearing; or the bounded park timeout —
// on either of the latter two the item is still delivered best-effort right
// after this returns, exactly as today. Without a bounded exit a page that
// never installs its handler would park narration indefinitely, reintroducing
// the SoftAP-QR-goes-dark failure the positive-only barrier cache (NV1)
// removed.
//
// [Park predicate fix] Uses NavigationTargetGeneration, NOT a
// Generation()-snapshot-at-entry comparison [former M2 fix]: that older
// approach broke when the park was entered AFTER the bump already happened
// (a real, common timing — parkForNavigation runs once per queued item, and
// NavigationPending stays true for the navigation's entire ~20s verifyCap
// window while awaitRouteSettled keeps polling an idle "/" wall) — the
// snapshot IS already the new generation in that case, so
// "Generation() != entrySnapshot" can never become true and the park stalled
// for its full timeout on every send entered post-bump. NavigationTargetGeneration
// instead identifies the SPECIFIC generation the in-flight navigation
// produced, independent of when this call happened to start: pre-bump it
// reads 0 (never exits via this path, preserving the original M2 intent of
// never trusting a stale pre-bump StageReady positive), and once the bump
// happens it holds that generation's ID until the navigation finishes,
// letting a post-bump-entered park exit the moment that SPECIFIC generation
// is handler-ready rather than waiting out the whole navigation.
func (s *Service) parkForNavigation() {
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
			s.logger.Info("Narration park timed out waiting on a pending navigation; delivering best-effort")
			return
		}
		time.Sleep(interval)
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

// ErrPlayerContractUnreadable marks a validation failure caused by failing to
// READ the player contract manifest, as opposed to a successfully-read
// manifest that lacks the contract being checked. The two must not be
// conflated: unreadable may be transient (boot ordering, an OTA mid-replace
// of the player bundle) and must be re-checked on the next attempt, never
// latched; a manifest that WAS read but lacks the contract means the
// connected player's build genuinely does not support it [W8]. Exported so
// other packages' capability fuses can apply the identical distinction —
// devicectl's boot-recovery classification (design doc §3.2) checks
// errors.Is(err, setupui.ErrPlayerContractUnreadable) against
// ValidatePlayerStatusContract exactly as this package's own
// narrationSupported does against validateSetupDisplayContract.
var ErrPlayerContractUnreadable = errors.New("player contract unreadable")

// errContractUnreadable is retained as a private alias so every existing
// reference below (and every existing test) keeps working unchanged; new
// code, in this package or elsewhere, should prefer the exported name.
var errContractUnreadable = ErrPlayerContractUnreadable

// readPlayerContractManifest reads and decodes the player contract manifest
// at path, wrapping a read failure in ErrPlayerContractUnreadable. Shared by
// every contract-specific validator in this file (validateSetupDisplayContract,
// ValidatePlayerStatusContract) so the unreadable-vs-absent distinction is
// defined exactly once.
func readPlayerContractManifest(path string) (playerContractManifest, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return playerContractManifest{}, fmt.Errorf("player contract path is empty")
	}
	raw, err := os.ReadFile(path) //nolint:gosec // Production uses the fixed player contract path; tests inject temp files.
	if err != nil {
		return playerContractManifest{}, fmt.Errorf("%w: %w", ErrPlayerContractUnreadable, err)
	}
	var manifest playerContractManifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return playerContractManifest{}, fmt.Errorf("decode player contract: %w", err)
	}
	return manifest, nil
}

// PlayerStatusContractVersion is the version ValidatePlayerStatusContract
// requires for contracts.playerStatus (design doc §4.3).
const PlayerStatusContractVersion = 1

// ValidatePlayerStatusContract reports whether the player manifest at path
// advertises contracts.playerStatus {version:1} — the structured status probe
// (window.__ffosPlayerStatus) devicectl's boot-recovery classification and
// the NavigateHome error-page/route gates depend on. Sibling of
// validateSetupDisplayContract, sharing its exact unreadable-vs-absent
// distinction via ErrPlayerContractUnreadable. Exported here — rather than in
// a new shared package — because devicectl already imports this package for
// narration, so it can call this directly with no new import-cycle risk:
// setupui imports nothing back from devicectl.
func ValidatePlayerStatusContract(path string) error {
	manifest, err := readPlayerContractManifest(path)
	if err != nil {
		return err
	}
	contract, ok := manifest.Contracts["playerStatus"]
	if !ok {
		return fmt.Errorf("missing contracts.playerStatus")
	}
	if contract.Version != PlayerStatusContractVersion {
		return fmt.Errorf("contracts.playerStatus.version must be %d", PlayerStatusContractVersion)
	}
	return nil
}

// validateSetupDisplayContract reports whether the player manifest at path
// advertises the setupDisplay contract this Service speaks. It mirrors
// mintpairing's contract validation mechanism (read the manifest from the
// filesystem, not over HTTP).
func validateSetupDisplayContract(path string) error {
	manifest, err := readPlayerContractManifest(path)
	if err != nil {
		return err
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
