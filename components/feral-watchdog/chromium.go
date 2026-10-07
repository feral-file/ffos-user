package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"go.uber.org/zap"
)

// errChromiumHeadless tags a failed health check that happened while Chromium
// is expected to be absent: no display connected (the kiosk waits for one), a
// developer VT other than tty1 active (start-kiosk.sh holds cage back), an
// update in progress (ffos#124: the update's IO storm may starve Chromium and
// the update ends in its own reboot), or the fallback hold in progress (the
// kiosk was stopped on purpose). The monitor
// loop logs these at debug instead of warning every check interval.
var errChromiumHeadless = errors.New("chromium down while headless (expected)")

const (
	// Chromium configuration
	CHROMIUM_CHECK_INTERVAL  = 5 * time.Second // Check CDP every 5 seconds
	CHROMIUM_REQUEST_TIMEOUT = 3 * time.Second
	// CHROMIUM_HANG_THRESHOLD is the steady-state budget: once we have seen
	// at least one 200 from /json/version, this much silence is treated as a
	// real renderer hang. Do not use this budget before the first success or
	// immediately after we issue a kiosk restart — Chromium's cold start
	// legitimately exceeds it on FF1 hardware, and conflating the two
	// produces restart loops on healthy devices.
	CHROMIUM_HANG_THRESHOLD = 20 * time.Second
	// CHROMIUM_STARTUP_GRACE is the cold-start budget: the longest we will
	// wait for Chromium to first expose /json/version before declaring it
	// stuck. Sized to cover feral-player.service (TimeoutStartSec=45s) +
	// chromium-kiosk.service RestartSec=5s + Chromium's own bring-up. Used
	// both at boot AND after every kiosk restart, since both situations
	// share the same "Chromium has not spoken to us yet" property.
	CHROMIUM_STARTUP_GRACE          = 90 * time.Second
	CHROMIUM_RESTART_HISTORY_SIZE   = 3 // Store the last 3 restarts
	CHROMIUM_MAX_RESTARTS_WINDOW    = 5 * time.Minute
	CHROMIUM_MAX_RESTARTS_THRESHOLD = 3 // 3 restarts within the window exhausts the budget
	// CHROMIUM_FALLBACK_HOLD is how long the fallback screen
	// (feral-kiosk-fallback.service: black, spinner, "Something went wrong...")
	// stays up after the restart budget is exhausted, before the reboot that
	// used to happen immediately. The reboot remains the self-heal rail (a
	// fresh boot clears transient faults and the nightly updaters need boots),
	// but a customer must see a stable error screen instead of a black screen
	// every ~5 minutes. restartHistory itself stays memory-only; what survives
	// the reboot is the fallback-reboot counter below.
	CHROMIUM_FALLBACK_HOLD = 15 * time.Minute
	// CHROMIUM_MAX_FALLBACK_REBOOTS caps how many consecutive boots may end in
	// a fallback-hold reboot before the monitor stops rebooting and parks on
	// the fallback screen indefinitely (ffos-user#254). Before this cap the
	// cycle "~5 min of restarts, 15 min hold, reboot" repeated forever, and
	// the device was never in one state long enough to diagnose. One reboot
	// is the self-heal attempt: if the same failure survives a fresh boot,
	// another reboot will not fix it. Parking keeps every other daemon up
	// (controld's LAN hub/AP, the nightly updater timer — whose own OTA reboot
	// is the way out), and the counter is cleared by the first healthy check,
	// so a fixed device returns to the normal ladder. A power cycle still
	// retries the full ladder, but ends parked rather than rebooting again.
	// The counter is persisted in chromiumFallbackStateFile.
	CHROMIUM_MAX_FALLBACK_REBOOTS = 1
)

// ChromiumMonitor monitors Chromium browser health via Chrome DevTools Protocol.
//
// Hang detection has two distinct modes that must not be conflated:
//
//   - Pre-connect (hasEverConnected == false): we are still inside the
//     startup-grace window because Chromium has either never spoken to us
//     (cold boot) or has not spoken since we asked systemd to restart it.
//     The only escalation path here is "still no response after
//     CHROMIUM_STARTUP_GRACE", not the steady-state hang threshold.
//
//   - Post-connect (hasEverConnected == true): we know Chromium was alive
//     because we received at least one 200 since the last reset.
//     CHROMIUM_HANG_THRESHOLD silence now means the renderer is genuinely
//     stuck and a kiosk restart is the right answer.
//
// restartChromium intentionally transitions back to the pre-connect mode so
// the next check cycle does not pile a fresh restart onto an in-progress one.
//
// Display gating overrides both modes: while no DRM connector reads
// "connected" (headless — including amdgpu's persistent "unknown" readings on
// empty connectors), Chromium legitimately does not run and every failed check
// is expected, so escalation is suppressed entirely (no restart, no
// reboot-budget accumulation). Detection FAILS OPEN only when no connector
// status is readable at all, and a reconnect re-anchors the pre-connect grace
// window.
//
// Developer-console gating works the same way: while the active VT is not
// tty1 (a developer on getty@tty2 via Ctrl+Alt+F2), start-kiosk.sh refuses to
// launch cage, so Chromium is legitimately absent and escalation is
// suppressed; returning to tty1 re-anchors the grace window.
//
// Update gating (ffos#124) works the same way again: while ffos's updater
// holds its lock (CommandHandler.updateInProgress), escalation is suppressed
// and the end of the update re-anchors the grace window. Inside the fallback hold the hold
// is re-anchored instead, so its reboot cannot land mid-update.
//
// Fallback hold: once the restart budget is exhausted the monitor no longer
// reboots at once. It stops the kiosk, starts feral-kiosk-fallback.service
// (a stable "Something went wrong..." screen) and holds for
// CHROMIUM_FALLBACK_HOLD, then reboots — unless CHROMIUM_MAX_FALLBACK_REBOOTS
// consecutive boots have already ended that way, in which case it parks on
// the fallback screen with no further reboot. A successful check (manual
// restart, OTA) clears the hold, the restart history and the persisted
// fallback-reboot counter.
type ChromiumMonitor struct {
	mu                 sync.Mutex
	cdpEndpoint        string
	client             *http.Client
	logger             *zap.Logger
	restartHistory     []time.Time
	hasEverConnected   bool
	monitorStart       time.Time
	lastSuccessfulResp time.Time
	commandHandler     *CommandHandler

	// drmSysfsRoot is the sysfs root consulted for display-attach state. It is a
	// field (not the constant directly) so tests can inject a fixture directory.
	drmSysfsRoot string
	// headless latches "we last observed no connected display". It is
	// how a reconnect is detected: on a headless device Chromium legitimately
	// does not run, so escalation is suppressed while headless, and the first
	// check after a display reappears re-anchors the startup-grace window so a
	// just-plugged monitor gets the full grace instead of an instant restart.
	headless bool

	// ttyActiveFile is the sysfs file naming the active VT; a field so tests
	// can inject a fixture. devConsole latches "we last observed a VT other
	// than tty1 active" so the transition is logged once and the return to
	// tty1 re-anchors the startup grace, exactly like the headless latch.
	ttyActiveFile string
	devConsole    bool

	// updating latches "we last observed an update holding the updater
	// lock" (updateInProgress, ffos#124), so the transition is logged once
	// and the end of the update re-anchors the startup grace, exactly like
	// the headless and developer-console latches.
	updating bool

	// fallbackSince is non-zero while the fallback screen is showing after
	// the restart budget was exhausted; the hold ends in a reboot unless a
	// check succeeds first.
	fallbackSince time.Time

	// fallbackReboots mirrors the persisted count of consecutive boots that
	// ended in a fallback-hold reboot; it is loaded once at construction and
	// written through to fallbackStatePath. fallbackParked latches "the hold
	// elapsed with the reboot cap reached" so the parked state is logged once.
	fallbackStatePath string
	fallbackReboots   int
	fallbackParked    bool

	// cdpStuck (ffos-user#356) is set by SetCDPStuck from feral-controld's
	// EVENT_CDP_STUCK DBus signal: controld's OWN CDP client has failed to
	// (re)dial Chromium's page target for a sustained period, which
	// /json/version polling below CANNOT see on its own — Chromium can keep
	// answering /json/version while exposing zero or more than one devtools
	// page target. checkHangState folds this into the exact same restart
	// decision and suppression gates (headless/dev-console/update/fallback
	// hold) as an ordinary hang; it never bypasses them. Cleared by
	// restartChromium once acted on — see that function's doc for why a
	// signal that never resends "true" must not be left to re-trigger a
	// restart on every subsequent tick.
	cdpStuck bool
}

// SetCDPStuck records whether feral-controld has reported its CDP
// page-target dial as stuck (ffos-user#356). Safe to call from the mediator's
// DBus-signal goroutine concurrently with check()'s own goroutine.
func (m *ChromiumMonitor) SetCDPStuck(stuck bool) {
	m.mu.Lock()
	m.cdpStuck = stuck
	m.mu.Unlock()
}

// NewChromiumMonitor creates a new Chromium monitor instance.
//
// The monitor starts in the pre-connect mode: hasEverConnected is false and
// monitorStart anchors the CHROMIUM_STARTUP_GRACE budget. The first 200 from
// /json/version flips us into post-connect mode; from that point on the
// shorter CHROMIUM_HANG_THRESHOLD applies. This separation is what prevents
// cold-boot devices and post-restart cycles from logging
// "Chromium browser hang detected" while Chromium is legitimately starting up.
func NewChromiumMonitor(cdpEndpoint string, logger *zap.Logger, commandHandler *CommandHandler) *ChromiumMonitor {
	fallbackReboots, err := loadChromiumFallbackReboots(chromiumFallbackStateFile)
	if err != nil {
		logger.Error("Chromium: could not read persisted fallback-reboot count; assuming none",
			zap.Error(err))
	} else if fallbackReboots > 0 {
		logger.Warn("Chromium: previous boot(s) ended in a fallback-hold reboot",
			zap.Int("consecutive_fallback_reboots", fallbackReboots),
			zap.Int("max_fallback_reboots", CHROMIUM_MAX_FALLBACK_REBOOTS))
	}
	return &ChromiumMonitor{
		fallbackStatePath: chromiumFallbackStateFile,
		fallbackReboots:   fallbackReboots,
		cdpEndpoint:       cdpEndpoint,
		client: &http.Client{
			Timeout: CHROMIUM_REQUEST_TIMEOUT,
		},
		logger:           logger,
		restartHistory:   make([]time.Time, 0, CHROMIUM_RESTART_HISTORY_SIZE),
		hasEverConnected: false,
		monitorStart:     time.Now(),
		commandHandler:   commandHandler,
		drmSysfsRoot:     defaultDRMSysfsRoot,
		ttyActiveFile:    defaultTTYActiveFile,
	}
}

// Start begins the CDP monitoring process
func (m *ChromiumMonitor) Start(ctx context.Context) {
	m.logger.Info("Chromium: Starting Chromium monitor",
		zap.String("endpoint", m.cdpEndpoint),
		zap.Duration("check_interval", CHROMIUM_CHECK_INTERVAL),
		zap.Duration("hang_threshold", CHROMIUM_HANG_THRESHOLD))

	ticker := time.NewTicker(CHROMIUM_CHECK_INTERVAL)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			m.logger.Info("Chromium: Monitor shutting down")
			return
		case <-ticker.C:
			if err := m.check(ctx); err != nil {
				if errors.Is(err, errChromiumHeadless) {
					// Expected while no display is attached; the headless
					// transition itself is logged once in checkHangState.
					m.logger.Debug("Chromium: Health check failed while headless", zap.Error(err))
				} else {
					m.logger.Warn("Chromium: Health check failed", zap.Error(err))
				}
			}
		}
	}
}

func (m *ChromiumMonitor) Stop() {
	if m.client != nil {
		m.client.CloseIdleConnections()
	}
}

// check performs a single CDP health check
func (m *ChromiumMonitor) check(ctx context.Context) error {
	versionURL := fmt.Sprintf("%s/json/version", m.cdpEndpoint)

	// Create context with timeout
	timeoutCtx, cancel := context.WithTimeout(ctx, CHROMIUM_REQUEST_TIMEOUT)
	defer cancel()

	req, err := http.NewRequestWithContext(timeoutCtx, http.MethodGet, versionURL, nil)
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	resp, err := m.client.Do(req)

	// Check for response and connection errors
	if err != nil {
		if m.checkHangState(ctx) {
			return fmt.Errorf("%w: chromium request failed: %w", errChromiumHeadless, err)
		}
		return fmt.Errorf("chromium request failed: %w", err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	// Check status code
	if resp.StatusCode != http.StatusOK {
		if m.checkHangState(ctx) {
			return fmt.Errorf("%w: chromium returned non-200 status: %d", errChromiumHeadless, resp.StatusCode)
		}
		return fmt.Errorf("chromium returned non-200 status: %d", resp.StatusCode)
	}

	// Read and discard response body to free up connections
	// Go uses connection pooling, this helps reuse the connection
	_, err = io.Copy(io.Discard, resp.Body)
	if err != nil {
		return fmt.Errorf("failed to read response body: %w", err)
	}

	// First success since (re)start arms post-connect hang detection. Until
	// this flips, sustained failures must use the larger startup-grace budget
	// instead of the steady-state hang threshold.
	m.mu.Lock()
	// ffos-user#356: capture whether we were in ANY suppressed state —
	// headless, dev console, update, or a fallback hold — before clearing
	// the first three below. cdphealth.Monitor has no visibility into any
	// of them: it would have latched cdpStuck=true throughout one that ran
	// past StuckThreshold (a headless device never runs Chromium at all),
	// so that latch is stale the moment we exit, exactly like headless/
	// devConsole/updating themselves. check() — not checkHangState — owns
	// this particular transition (checkHangState's own `reconnected` reset
	// only fires when ITS failure-path call observes the stale fields
	// still true; by the time this success path calls checkHangState below,
	// headless/devConsole/updating are already cleared here, so that reset
	// never sees them and never runs).
	wasSuppressed := m.headless || m.devConsole || m.updating || !m.fallbackSince.IsZero()
	m.lastSuccessfulResp = time.Now()
	m.hasEverConnected = true
	// A 200 proves a display is attached and Chromium is up. Clear the headless
	// and developer-console latches so a later genuine hang is escalated
	// normally rather than being mistaken for a reconnect and granted a fresh
	// grace window.
	m.headless = false
	m.devConsole = false
	m.updating = false
	if wasSuppressed {
		// Clearing here, not unconditionally on every success, matters:
		// cdpStuck must still be able to trigger checkHangState's cdpStuck
		// case below for the ordinary case this feature exists for — a
		// healthy /json/version with controld independently reporting its
		// CDP dial stuck — which clearing it on every success would defeat
		// outright. It is only stale, and therefore safe (indeed necessary)
		// to clear, right when exiting a state cdphealth could not see.
		m.cdpStuck = false
	}
	// Chromium came back while the fallback screen was up (someone restarted
	// the kiosk by hand, an OTA fixed the bundle): drop the hold and forget the
	// exhausted budget so a later fault gets the full restart ladder again.
	// The kiosk's ExecStartPre already stopped feral-kiosk-fallback.service.
	recovered := !m.fallbackSince.IsZero()
	if recovered {
		m.fallbackSince = time.Time{}
		m.fallbackParked = false
		m.restartHistory = m.restartHistory[:0]
	}
	// A healthy Chromium ends the run of fallback reboots (#254). Checked on
	// every success but only writes when there is something to clear, so the
	// steady state touches no disk. In-memory is zeroed even if the removal
	// fails: retrying would log every 5 s, and the cost of a stale file is one
	// extra parked hold after a later failure, not a lost self-heal.
	clearPersisted := m.fallbackReboots > 0
	m.fallbackReboots = 0
	statePath := m.fallbackStatePath
	m.mu.Unlock()

	if recovered {
		m.commandHandler.clearKioskFallback()
		m.logger.Info("Chromium: recovered while fallback screen was showing; resuming normal monitoring")
	}
	if clearPersisted {
		if err := clearChromiumFallbackReboots(statePath); err != nil {
			m.logger.Error("Chromium: failed to clear persisted fallback-reboot count", zap.Error(err))
		} else {
			m.logger.Info("Chromium: healthy; cleared persisted fallback-reboot count")
		}
	}

	// ffos-user#356: a healthy /json/version does not prove controld's own
	// CDP page-target dial is healthy too (see cdpStuck's doc), so the
	// escalation check runs here as well, not only on failure below. By
	// this point hasEverConnected/lastSuccessfulResp were just set above, so
	// checkHangState's ordinary hang/startup-grace cases read as healthy and
	// only the cdpStuck case can fire — it never races or duplicates the
	// failure-path's own call to checkHangState.
	m.checkHangState(ctx)

	return nil
}

// checkHangState decides whether sustained failure to reach /json/version, OR
// feral-controld's own cdpStuck report (ffos-user#356), warrants escalating
// to a kiosk restart. It is called on every failed check AND, since
// ffos-user#356, at the end of every successful one too (from check()) so a
// controld-reported stuck page-target dial can still escalate even while
// /json/version itself answers fine. The cost of false positives is high:
// any spurious restart will be repeated on the next 5-second tick.
//
// It reports whether the failure happened while headless (no connected
// display, a developer VT active, an update in progress, or the fallback hold
// in progress), so the
// caller can tag it as expected rather than warn-worthy.
//
// The decision splits on hasEverConnected. Pre-connect, we wait through
// CHROMIUM_STARTUP_GRACE; post-connect, the shorter CHROMIUM_HANG_THRESHOLD
// applies. Both branches additionally consult the chromium-kiosk.service
// activating state so we don't pile a fresh restart onto a restart that
// systemd or someone else (OTA, user) is already running. cdpStuck can
// escalate in either branch, subject to the same gates.
func (m *ChromiumMonitor) checkHangState(ctx context.Context) (headless bool) {
	// Display gating comes first. On a headless device the kiosk deliberately
	// waits for a display before launching Chromium, so /json/version is
	// legitimately absent and is NOT a Chromium failure. While no connector
	// reads "connected" we skip the entire escalation path: no kiosk restart
	// and — critically — no restartHistory accumulation, so a device that sits
	// headless for hours cannot later trip the 3-restarts-in-5-minutes reboot
	// budget. The read is done outside m.mu because it touches the filesystem.
	displayConnected := isDisplayConnected(m.drmSysfsRoot)
	// One read of the VT file per check so the decision and the log line
	// below name the same VT.
	activeVT, vtReadable := readActiveVT(m.ttyActiveFile)
	onKioskVT := kioskVTActive(activeVT, vtReadable)
	// Update gate (ffos#124): while an OTA or package update holds the
	// updater lock, its IO storm can starve /json/version long enough to walk
	// the restart ladder, and the fallback hold would end in a reboot that
	// kills the update. Escalation is suppressed like the developer console:
	// no kiosk restart, no budget accumulation, and a fresh grace when the
	// update ends (usually it ends in its own reboot instead). Read outside
	// m.mu for the same reason as the two reads above.
	updating := m.commandHandler.updateInProgress()

	m.mu.Lock()
	// Fallback hold: the kiosk was stopped on purpose and the error screen is
	// up, so a failed check is expected. The hold ends in the reboot that used
	// to be immediate, but the two suppression gates still apply inside it:
	//   - no display: a headless device must never trip a reboot (the
	//     invariant the display gate exists for), so the hold is abandoned and
	//     the monitor drops into ordinary headless suppression; the kiosk
	//     restarts normally once a display returns (fallbackShown is cleared
	//     so restartKiosk is allowed again, and its ExecStartPre clears the
	//     screen);
	//   - developer console on another VT: the console is most needed exactly
	//     when the kiosk has no picture, so the hold is re-anchored rather
	//     than rebooting the developer out of their shell; a full hold
	//     starts over once tty1 is active again;
	//   - an update in progress: re-anchored the same way, so the hold's
	//     reboot cannot land mid-update; a full hold starts over afterwards.
	// fallbackSince is reset before rebooting so a failed reboot command
	// (sudo/systemctl outage) cannot re-fire every 5 s; it then drops into the
	// normal restart ladder, which is acceptable.
	if !m.fallbackSince.IsZero() {
		if !displayConnected {
			m.fallbackSince = time.Time{}
			m.fallbackParked = false
			// Forget the exhausted budget too: the kiosk is stopped, so once a
			// display returns the reconnect grace must end in a kiosk RESTART,
			// not in an immediate second fallback because three stale stamps
			// are still inside the five-minute window.
			m.restartHistory = m.restartHistory[:0]
			m.headless = true
			m.mu.Unlock()
			m.commandHandler.clearKioskFallback()
			m.logger.Info("Chromium: display disconnected during fallback hold; abandoning the hold, suppressing escalation until a display reconnects")
			return true
		}
		if !onKioskVT {
			enteredDevConsole := !m.devConsole
			m.devConsole = true
			m.fallbackSince = time.Now()
			parked := m.fallbackParked
			m.mu.Unlock()
			if enteredDevConsole {
				// Parked means no reboot is pending, so say so rather than
				// claim one is being deferred; the hold is re-anchored either
				// way and re-parks 15 min after tty1 returns.
				msg := "Chromium: developer console active during fallback hold; deferring the reboot until tty1 is active"
				if parked {
					msg = "Chromium: developer console active while parked on the fallback screen; no reboot pending"
				}
				m.logger.Info(msg, zap.String("active_vt", activeVT))
			}
			return true
		}
		m.devConsole = false
		if updating {
			enteredUpdating := !m.updating
			m.updating = true
			m.fallbackSince = time.Now()
			m.mu.Unlock()
			if enteredUpdating {
				m.logger.Info("Chromium: update in progress during fallback hold; deferring the hold until the update ends")
			}
			return true
		}
		m.updating = false
		held := time.Since(m.fallbackSince)
		if held < CHROMIUM_FALLBACK_HOLD {
			m.mu.Unlock()
			return true
		}
		// Reboot cap reached (#254): a fresh boot already failed the same way,
		// so park on the fallback screen instead of rebooting again. The hold
		// stays armed (fallbackSince untouched, fallbackShown still set) so
		// the display/dev-console gates above and the recovery path in check()
		// keep working exactly as during an ordinary hold.
		if m.fallbackReboots >= CHROMIUM_MAX_FALLBACK_REBOOTS {
			enteredPark := !m.fallbackParked
			m.fallbackParked = true
			reboots := m.fallbackReboots
			m.mu.Unlock()
			if enteredPark {
				m.logger.Error("Chromium: fallback hold elapsed but the reboot cap is reached; staying on the fallback screen without rebooting",
					zap.Int("consecutive_fallback_reboots", reboots),
					zap.Int("max_fallback_reboots", CHROMIUM_MAX_FALLBACK_REBOOTS))
				m.commandHandler.reportFallbackParked(ctx, reboots)
			}
			return true
		}
		m.fallbackSince = time.Time{}
		m.fallbackReboots++
		reboots := m.fallbackReboots
		statePath := m.fallbackStatePath
		m.mu.Unlock()
		// Persist before rebooting. If the write fails, reboot anyway: that is
		// exactly the pre-#254 behavior, and a device that cannot write its
		// home directory is better served by the self-heal than by parking.
		if err := storeChromiumFallbackReboots(statePath, reboots); err != nil {
			m.logger.Error("Chromium: failed to persist fallback-reboot count; rebooting anyway",
				zap.Error(err))
		}
		m.commandHandler.clearKioskFallback()
		m.logger.Error("Chromium: fallback hold elapsed, triggering system reboot",
			zap.Duration("held", held),
			zap.Duration("hold", CHROMIUM_FALLBACK_HOLD),
			zap.Int("consecutive_fallback_reboots", reboots),
			zap.Int("max_fallback_reboots", CHROMIUM_MAX_FALLBACK_REBOOTS))
		m.commandHandler.rebootSystem(ctx, CrashReasonChromiumCrash)
		return true
	}
	if !displayConnected {
		// Latch headless so the first check after a reconnect re-anchors grace.
		// Log the transition once instead of warning every check interval.
		enteredHeadless := !m.headless
		m.headless = true
		m.mu.Unlock()
		if enteredHeadless {
			m.logger.Info("Chromium: No display connected; suppressing health-check escalation until a display reconnects")
		}
		return true
	}
	if !onKioskVT {
		// Developer console: a VT other than tty1 is active, so
		// start-kiosk.sh's wait_for_vt1 holds cage back and Chromium is
		// legitimately down. Same treatment as headless: no restart, no
		// budget accumulation, transition logged once.
		enteredDevConsole := !m.devConsole
		m.devConsole = true
		m.mu.Unlock()
		if enteredDevConsole {
			m.logger.Info("Chromium: developer console active; suppressing health-check escalation until tty1 is active",
				zap.String("active_vt", activeVT))
		}
		return true
	}
	if updating {
		enteredUpdating := !m.updating
		m.updating = true
		m.mu.Unlock()
		if enteredUpdating {
			m.logger.Info("Chromium: update in progress; suppressing health-check escalation until it ends")
		}
		return true
	}
	reconnected := m.headless || m.devConsole || m.updating
	if reconnected {
		// Display (re)appeared after a headless period, or the developer
		// returned to tty1. Escalation resumes, but with a FRESH pre-connect
		// grace window: a just-plugged monitor (or a kiosk that wait_for_vt1
		// only now lets start) must get the full CHROMIUM_STARTUP_GRACE for
		// Chromium to cold-start, not an instant restart driven by the stale
		// monitorStart from before.
		m.headless = false
		m.devConsole = false
		m.updating = false
		m.hasEverConnected = false
		m.monitorStart = time.Now()
		m.lastSuccessfulResp = time.Time{}
		// ffos-user#356: cdpStuck must reset here too, alongside its three
		// siblings above. cdphealth.Monitor has no idea a display/VT/update
		// gate was ever closed — on a headless device it is GUARANTEED to
		// latch cdpStuck=true after StuckThreshold, since Chromium never
		// runs there at all. Without this reset, the fresh grace window just
		// armed above is immediately defeated by the switch's own
		// `!cdpStuck` check on the very next tick, firing a restart before
		// Chromium has had any chance to cold-start — exactly the restart
		// storm this reset block exists to prevent for the other three
		// latches. If controld's dial is genuinely still stuck once the
		// gate reopens, it reports again after another StuckThreshold.
		m.cdpStuck = false
	}
	hasEverConnected := m.hasEverConnected
	timeSinceLast := time.Since(m.lastSuccessfulResp)
	timeSinceStart := time.Since(m.monitorStart)
	cdpStuck := m.cdpStuck
	m.mu.Unlock()

	if reconnected {
		m.logger.Info("Chromium: Display reconnected, developer console left or update ended; resuming health-check escalation with a fresh startup grace",
			zap.Duration("startup_grace", CHROMIUM_STARTUP_GRACE))
	}

	var (
		shouldRestart bool
		reason        string
	)
	switch {
	case !hasEverConnected:
		if timeSinceStart <= CHROMIUM_STARTUP_GRACE && !cdpStuck {
			// Cold boot or post-restart bring-up still in progress, and
			// controld has not (yet) reported its own dial stuck either.
			// Stay quiet — the noisy "Chromium browser hang detected" line
			// is reserved for genuine post-connect renderer hangs.
			return
		}
		shouldRestart = true
		if timeSinceStart > CHROMIUM_STARTUP_GRACE {
			reason = "startup_grace_exceeded"
		} else {
			reason = "controld_cdp_stuck"
		}
	case timeSinceLast > CHROMIUM_HANG_THRESHOLD:
		shouldRestart = true
		reason = "hang_threshold_exceeded"
	case cdpStuck:
		// ffos-user#356: /json/version is healthy (this check may be
		// running from check()'s SUCCESS path), but feral-controld's own
		// CDP page-target dial has been stuck for cdphealth.StuckThreshold
		// — a failure mode /json/version alone can never see.
		shouldRestart = true
		reason = "controld_cdp_stuck"
	}

	if !shouldRestart {
		return
	}

	// systemctl call deliberately outside the monitor mutex. The call shells
	// out and can block tens of milliseconds; holding m.mu would block the
	// next check() unnecessarily. The state we read above is sufficient to
	// reach this point — re-acquiring is only needed for the restart write.
	if m.commandHandler != nil && m.commandHandler.isKioskActivating(ctx) {
		m.logger.Warn("Chromium: Restart trigger met but chromium-kiosk.service is activating; deferring",
			zap.String("reason", reason),
			zap.Duration("time_since_last_response", timeSinceLast),
			zap.Duration("time_since_monitor_start", timeSinceStart))
		return
	}

	switch reason {
	case "startup_grace_exceeded":
		m.logger.Error("Chromium: Chromium failed to come up within startup grace",
			zap.Duration("budget", CHROMIUM_STARTUP_GRACE),
			zap.Duration("elapsed", timeSinceStart))
	case "controld_cdp_stuck":
		m.logger.Error("Chromium: feral-controld reported its CDP page-target dial stuck; restarting kiosk (ffos-user#356)",
			zap.Duration("time_since_last_response", timeSinceLast),
			zap.Duration("time_since_monitor_start", timeSinceStart))
	default:
		m.logger.Error("Chromium: Chromium browser hang detected",
			zap.Duration("time_since_last_response", timeSinceLast),
			zap.Duration("threshold", CHROMIUM_HANG_THRESHOLD))
	}

	m.mu.Lock()
	m.restartChromium(ctx)
	m.mu.Unlock()
	return false
}

// restartChromium issues a kiosk restart (or, if we've burned through the
// restart budget, stops the kiosk and shows the fallback screen; the reboot
// follows after CHROMIUM_FALLBACK_HOLD in checkHangState) and then drops the
// monitor back into pre-connect mode.
//
// The pre-connect reset is load-bearing: Chromium will be unavailable for
// longer than CHROMIUM_HANG_THRESHOLD after a kiosk restart, and without this
// reset the next check 20 seconds later will see "no response" and issue
// another restart, exhausting the 3-restart budget in under a minute and
// rebooting healthy devices.
//
// Callers must hold m.mu.
func (m *ChromiumMonitor) restartChromium(ctx context.Context) {
	now := time.Now()
	m.restartHistory = append(m.restartHistory, now)

	// Keep only 3 recent restarts
	if len(m.restartHistory) > CHROMIUM_RESTART_HISTORY_SIZE {
		m.restartHistory = m.restartHistory[1:]
	}

	// ffos-user#356: clear the externally-reported latch as soon as this
	// escalation is acted on — by EITHER branch below (an ordinary kiosk
	// restart, or the restart-budget-exhausted fallback hold), not only the
	// ordinary-restart branch's own state reset further down. cdphealth's
	// Monitor only re-sends "true" on a FRESH disconnect episode (an actual
	// reconnect in between); if this action does not fix the underlying
	// stall, controld's view of "still stuck" never changes, so it never
	// re-sends either. Leaving m.cdpStuck set would re-trigger
	// checkHangState's cdpStuck case on literally the next 5s tick,
	// bypassing whatever gate just engaged (the fresh pre-connect grace, or
	// the fallback hold) and reproducing the exact restart-storm bug the
	// ordinary-restart branch's own ESTABLISHED state reset exists to
	// prevent. Any continued failure now escalates through the ordinary
	// startup-grace/hang-threshold/fallback ladder instead, exactly like any
	// other hang.
	m.cdpStuck = false

	// Budget exhausted: show the stable error screen instead of rebooting
	// right away. No kiosk restart is issued — the fallback owns the display
	// until the hold elapses (reboot) or a check succeeds (recovery). If the
	// screen cannot be shown (unit absent on an older image, sudo refused),
	// the kiosk has already been stopped, so reboot immediately exactly as
	// before this policy rather than hold on a black screen.
	if m.shouldTriggerReboot() {
		m.logger.Error("Chromium: restart budget exhausted; showing fallback screen and holding before reboot",
			zap.Int("restarts", len(m.restartHistory)),
			zap.Duration("window", CHROMIUM_MAX_RESTARTS_WINDOW),
			zap.Duration("hold", CHROMIUM_FALLBACK_HOLD))
		switch m.commandHandler.showKioskFallback(ctx) {
		case kioskFallbackShown:
			m.fallbackSince = now
		case kioskFallbackBusy:
			// A RAM/GPU-triggered kiosk restart is in flight; nothing was
			// stopped. Leave the budget exhausted and let the next tick
			// retry once the other operation has cleared.
			m.logger.Warn("Chromium: fallback screen deferred; another kiosk operation is in flight")
		case kioskFallbackUnavailable:
			m.logger.Error("Chromium: fallback screen unavailable; triggering system reboot now")
			m.commandHandler.rebootSystem(ctx, CrashReasonChromiumCrash)
		}
		return
	}

	// Execute the restart command
	m.logger.Warn("Chromium: Restarting chromium-kiosk.service")
	m.commandHandler.restartKiosk(ctx)

	// Drop back to pre-connect mode. The next check cycle will then use the
	// startup-grace budget (90s) rather than the steady-state hang threshold
	// (20s), so we don't fire a second restart while Chromium is still
	// coming up from the first one.
	m.hasEverConnected = false
	m.monitorStart = now
	m.lastSuccessfulResp = time.Time{}
}

// shouldTriggerReboot reports whether the restart budget is exhausted
// (CHROMIUM_MAX_RESTARTS_THRESHOLD restarts within CHROMIUM_MAX_RESTARTS_WINDOW),
// which now enters the fallback hold rather than rebooting immediately.
func (m *ChromiumMonitor) shouldTriggerReboot() bool {
	if len(m.restartHistory) < CHROMIUM_MAX_RESTARTS_THRESHOLD {
		return false
	}

	// If the oldest of the recent restarts is within the window, the budget is spent
	return time.Since(m.restartHistory[0]) <= CHROMIUM_MAX_RESTARTS_WINDOW
}
