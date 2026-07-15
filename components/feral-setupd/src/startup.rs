//! Startup flows and initialization for feral-setupd.

use crate::app_state::{AppState, Page, unix_s};
use crate::ble::{Ble, BleCallbacks};
use crate::callbacks;
use crate::cdp::CdpHandle;
use crate::cfg;
use crate::connectivity::Connectivity;
use crate::constant;
use crate::dbus_utils;
use crate::persistent_state::{self, PersistentState};
use crate::phase_logic::{get_setup_phase, needs_relayer_topic_fetch};
use crate::setup_lifecycle::{SetupLifecycle, SetupPhase};
use crate::ui::{show_qrcode, show_system_upgrade, show_webapp};
use crate::update_coordinator::{
    StartupUpdateOutcome, UPDATE_FAILED_RECOVERED_MSG, UpdateExecution, UpdateGuard, UpdateMode,
    check_and_update_system, startup_update_check_outcome,
};
use crate::wifi_utils::SSIDsCacher;
use anyhow::{Context, Result};
use std::sync::Arc;
use std::sync::atomic::{AtomicBool, Ordering};
use std::time::Instant;
use tokio::sync::Mutex;
use tokio::time::Duration;

pub async fn init_app_state(ble_service: &Arc<Ble>) -> Result<Arc<AppState>> {
    let lifecycle = SetupLifecycle::new();
    let state_store = PersistentState::new(constant::CACHE_FILEPATH)?;

    // Restore durable phase (Pairing, Ready, UpdateFailed) from persistent storage after restart
    lifecycle.restore_from_store(&state_store);

    let app_state = Arc::new(AppState {
        device_id: ble_service.get_device_id().await,
        branch: cfg::branch().await?.to_string(),
        current_version: cfg::current_version().await?.to_string(),
        state_store,
        internet: Connectivity::spawn().await,
        page: Mutex::new(Page::None(unix_s())),
        auto_proceed: AtomicBool::new(false),
        lifecycle,
        update_in_progress: Arc::new(AtomicBool::new(false)),
    });
    sentry::configure_scope(|scope| {
        scope.set_tag("branch", app_state.branch.clone());
        scope.set_tag("version", app_state.current_version.clone());
        scope.set_user(Some(sentry::User {
            id: Some(app_state.device_id.clone()),
            ..Default::default()
        }));
    });
    println!("MAIN: App state initialized: {app_state:?}");
    Ok(app_state)
}

/// Build the reconnecting CDP handle, attempting one best-effort connect up front.
///
/// A headless device may have no Chromium at boot (no monitor → no kiosk → no CDP), so a failed
/// connect must NOT abort startup — this daemon now boots unconditionally and drives BLE/D-Bus/OTA
/// with zero CDP. When Chromium IS present the up-front connect makes the first `show_*` paint land
/// exactly as before; when it is absent, `spawn_cdp_reconnect_loop` keeps retrying and resyncs the
/// UI once a browser appears. This is why `run()` no longer `?`-propagates a CDP failure (that was
/// the fatal path that killed a headless boot).
///
/// Awaiting the connect here is safe only because every CDP HTTP fetch and the WS dial carry hard
/// timeouts (`CDP_HTTP_FETCH_TIMEOUT` / `CDP_WS_CONNECT_TIMEOUT`): a wedged DevTools endpoint that
/// accepts TCP but never responds fails this connect in bounded time instead of hanging startup
/// before `start_ble`.
pub async fn init_cdp() -> Arc<CdpHandle> {
    let chrome = Arc::new(CdpHandle::new(constant::CDP_URL));
    if let Err(e) = chrome.connect().await {
        eprintln!("MAIN: initial CDP connect failed, will retry in background: {e:#?}");
    }
    chrome
}

/// Background loop that keeps the CDP handle connected and repaints the canonical UI on (re)connect.
///
/// Runs for the daemon's lifetime. Two states:
/// - Connected: periodically verify the connection still matches the live Chromium page
///   (`connection_is_current`, HTTP-only). A kiosk restart mints a new page target, so the cached
///   socket goes stale silently; after consecutive failed probes we drop it and fall through to
///   reconnect (single-probe blips are debounced to avoid repainting a healthy kiosk).
/// - Disconnected: retry `connect()` on a short interval. On success, resync the UI so the freshly
///   (re)started kiosk shows the surface the daemon believes it is on, instead of the launcher logo.
///
/// The up-front connect in `init_cdp` means a browser present at boot is already connected here, so
/// the initial paint is owned by the normal startup flow and this loop only monitors — it does not
/// double-paint. Resync is skipped while an OTA owns the device (see `resync_canonical_page`).
pub fn spawn_cdp_reconnect_loop(chrome: Arc<CdpHandle>, app_state: Arc<AppState>) {
    tokio::spawn(async move {
        // Consecutive failed liveness probes. The probe is an HTTP round-trip that can blip
        // transiently while the socket is perfectly healthy; declaring staleness on a single
        // failure would disconnect + resync, visibly reloading the kiosk page in steady state.
        let mut stale_probes: u32 = 0;
        loop {
            if chrome.is_connected().await {
                // A navigate that timed out on the cached socket is a corroborating staleness
                // signal (a kiosk restart can leave the socket writable but the target dead):
                // the suspect wake below cuts the sleep short so we probe right away, and a
                // suspect + failed probe skips the blip debounce. A suspect whose probe passes
                // was just Chromium rendering without replying — the flag is dropped.
                let nav_suspect = chrome.take_navigate_suspect();
                if chrome.connection_is_current().await {
                    stale_probes = 0;
                    liveness_sleep(&chrome).await;
                    continue;
                }
                stale_probes += 1;
                if !nav_suspect && stale_probes < constant::CDP_LIVENESS_STALE_PROBES {
                    liveness_sleep(&chrome).await;
                    continue;
                }
                // Stale or dead browser target: drop so the reconnect below rebinds to the new one.
                println!(
                    "MAIN: CDP connection no longer current after {stale_probes} probes, reconnecting"
                );
                stale_probes = 0;
                chrome.disconnect().await;
            }

            match chrome.connect().await {
                Ok(()) => {
                    // Fresh connection, fresh probe history: a count carried over from before a
                    // navigate-triggered drop must not let a single post-reconnect blip repaint.
                    stale_probes = 0;
                    println!("MAIN: CDP (re)connected, resyncing canonical UI");
                    crate::ui::resync_canonical_page(&app_state, &chrome).await;
                }
                Err(_) => {
                    tokio::time::sleep(Duration::from_millis(constant::CDP_RECONNECT_INTERVAL))
                        .await;
                }
            }
        }
    });
}

/// Sleep out one liveness interval, waking early if a navigate raises the zombie-socket suspect
/// flag so the loop probes immediately instead of leaving navigations silently dropped.
async fn liveness_sleep(chrome: &CdpHandle) {
    tokio::select! {
        _ = tokio::time::sleep(Duration::from_millis(constant::CDP_LIVENESS_CHECK_INTERVAL)) => {}
        _ = chrome.navigate_suspect_raised() => {}
    }
}

pub async fn start_ble(
    ble_service: &Arc<Ble>,
    app_state: &Arc<AppState>,
    chrome: &Arc<CdpHandle>,
    ssids_cacher: &Arc<SSIDsCacher>,
) -> Result<()> {
    let ble_callbacks = BleCallbacks {
        bt_connected: callbacks::create_bt_connected_cb(app_state.clone(), chrome.clone()),
        bt_disconnected: callbacks::create_bt_disconnected_cb(app_state.clone(), chrome.clone()),
        factory_reset: callbacks::create_factory_reset_cb(app_state.clone(), chrome.clone()),
        submit_logs: callbacks::create_submit_logs_cb(app_state.clone()),
        connect_wifi: callbacks::create_connect_wifi_cb(app_state.clone(), chrome.clone()),
        keep_wifi: callbacks::create_keep_wifi_cb(app_state.clone(), chrome.clone()),
        get_info: callbacks::create_get_info_cb(app_state.clone()),
    };

    ble_service
        .start(ble_callbacks, ssids_cacher.clone())
        .await
        .context("starting Bluetooth advertising")?;
    println!("MAIN: Bluetooth advertising started successfully");
    Ok(())
}

/// Whether a startup path must short-circuit to the `UpdateFailed` recovery screen instead of the
/// normal QR / update-check flow.
///
/// Both the online (`on_startup_with_internet`) and offline (`startup_without_internet`) paths
/// consult this so they stay in lockstep: an offline reboot must show the same failure UI as an
/// online reboot. Only the durable `UpdateFailed` phase qualifies — any other phase proceeds with
/// the normal flow. Keeping the decision in one predicate prevents the two branches from drifting.
pub fn startup_requires_update_failed_recovery(phase: SetupPhase) -> bool {
    phase == SetupPhase::UpdateFailed
}

/// Reboot-recovery UI for a persisted `UpdateFailed` phase.
///
/// Shows the recovered-failure message and leaves the phase untouched. Recovery is driven by an
/// explicit BLE/D-Bus retry (which also re-establishes connectivity), so callers must `return`
/// after this instead of falling through to QR / auto-proceed / update-check. Centralizing the
/// surface keeps the online and offline startup branches identical if the message changes.
pub async fn show_update_failed_recovery(
    app_state: &Arc<AppState>,
    chrome: &Arc<CdpHandle>,
) -> Result<()> {
    println!("MAIN: UpdateFailed phase restored; showing recovered failure message");
    println!("MAIN: Waiting for explicit BLE/D-Bus retry");
    show_system_upgrade(chrome, app_state, UPDATE_FAILED_RECOVERED_MSG).await
}

/// Startup path when the device does **not** have internet at boot time.
///
/// When this is called:
/// - `run` has waited (best-effort, non-fatal on timeout) for `controld` to be reachable.
/// - The initial internet check says the device is currently offline.
///
/// What it does:
/// - Warms the Wi-Fi SSID cache so the first BLE scan is fast.
/// - Shows the pairing QR code to let the user fix connectivity.
/// - Polls for internet with an aggressive or relaxed interval depending on
///   whether the device has ever connected before.
/// - Marks the device as "has connected before" in the cache once online.
/// - If the BLE flow has not opted out via `auto_proceed`, hands off to the
///   normal "startup with internet" flow.
///
/// Notes:
/// - If the user chooses a new Wi-Fi via BLE, the BLE flow clears
///   `auto_proceed`; in that case this function will not auto-advance and the
///   BLE setup path remains in control.
pub async fn startup_without_internet(
    app_state: &Arc<AppState>,
    chrome: &Arc<CdpHandle>,
    ssids_cacher: &Arc<SSIDsCacher>,
    used_to_connect: Option<&String>,
) -> Result<()> {
    // Show the QRCode so the user can do something with the internet
    let start_time = Instant::now();
    let _ = ssids_cacher.get().await;
    println!(
        "MAIN: Get SSIDs in {:?} ms",
        start_time.elapsed().as_millis()
    );

    // UpdateFailed reboot recovery must show the failure screen even when offline. Otherwise the
    // device would display QR (and a BLE connect could swap in the welcome message) while still
    // reporting setup_phase=update_failed to the mobile app. Recovery happens via an explicit
    // BLE/D-Bus retry that brings its own connectivity, so we skip QR/poll/auto-proceed here. The
    // SSID cache was warmed above so the recovery BLE scan stays fast.
    if startup_requires_update_failed_recovery(app_state.lifecycle.get()) {
        let _ = show_update_failed_recovery(app_state, chrome).await;
        return Ok(());
    }

    let _ = show_qrcode(app_state, chrome).await;
    app_state.auto_proceed.store(true, Ordering::Release);
    // If somehow, the device has internet
    // 1. Users fix the previous internet
    // 2. Users plug in the LAN cable (instead of setting up wifi via bluetooth)
    // We will take action immediately
    let urgency = if used_to_connect.is_some() {
        Duration::from_millis(constant::AGGRESSIVE_INTERNET_CHECK_INTERVAL)
    } else {
        Duration::from_millis(constant::RELAXED_INTERNET_CHECK_INTERVAL)
    };
    app_state.internet.wait_until_online(urgency, None).await;

    if used_to_connect.is_none() {
        app_state
            .state_store
            .set(persistent_state::CONNECTED, "true");
        app_state.state_store.save()?;
    }
    // We now have internet, but we need to check if
    // the internet comes from bluetooth (auto_proceed is set to false)
    // if it's from bluetooth, we shouldn't do anything else as the bluetooth
    // flow will handle it.
    //
    // Use compare_exchange so the auto-advance fires at most once and cannot race
    // a concurrent BLE connect_wifi that clears auto_proceed right after we observe
    // internet. Claiming the flag (true -> false) here also prevents a later BLE
    // flow from re-triggering startup once we've taken ownership.
    if app_state
        .auto_proceed
        .compare_exchange(true, false, Ordering::AcqRel, Ordering::Acquire)
        .is_ok()
    {
        on_startup_with_internet(app_state.clone(), chrome.clone()).await?;
    }
    Ok(())
}

/// Startup path when the device already has internet at boot time.
///
/// When this is called:
/// - `run` has waited (best-effort, non-fatal on timeout) for `controld` to be reachable.
/// - The initial internet check says the device is currently online.
///
/// What it does:
/// - Ensures the "has ever connected" flag is persisted in the cache.
/// - Delegates to `on_startup_with_internet` to either show the web app or a
///   reflashing QR code, depending on updater state and cached topic ID.
///
/// Notes:
/// - This path is used both on true first-boot with working internet and on
///   subsequent boots where connectivity is available immediately.
pub async fn startup_with_internet(
    app_state: &Arc<AppState>,
    chrome: &Arc<CdpHandle>,
    used_to_connect: Option<&String>,
) -> Result<()> {
    if used_to_connect.is_none() {
        app_state
            .state_store
            .set(persistent_state::CONNECTED, "true");
        app_state.state_store.save()?;
    }
    on_startup_with_internet(app_state.clone(), chrome.clone()).await
}

/// Handles the main startup flow once the device has a working internet connection.
///
/// When this is called:
/// - The app state and CDP connection have already been initialized.
/// - The caller has determined that the device currently has internet access.
///
/// What it does:
/// - Checks whether the running firmware/software is too old to auto-upgrade and, if so,
///   shows a reflashing QR code or a fallback message and stops further processing.
/// - If the device can be upgraded, checks whether an update is required and either
///   drives the updater flow or continues with normal startup.
/// - If no update is in progress, decides whether to show the web app or the pairing
///   QR code based on the presence of a cached topic ID and whether the device is in
///   qemu mode.
///
/// Notes:
/// - Any early return from this function (for example, when an update is required or
///   the device is too old) is intentional and means the usual "show art or QR" flow
///   should not continue.
pub async fn on_startup_with_internet(
    app_state: Arc<AppState>,
    chrome: Arc<CdpHandle>,
) -> Result<()> {
    // If UpdateFailed phase is set, skip automatic update check on startup.
    // Show the failure message (different from fresh failure) since this is reboot recovery.
    // Mobile app will see update_failed via device_info polling and can trigger explicit retry.
    if startup_requires_update_failed_recovery(app_state.lifecycle.get()) {
        println!("MAIN: Skipping startup update check - UpdateFailed phase is set");
        show_update_failed_recovery(&app_state, &chrome).await?;
        return Ok(());
    }

    // Check and update system using Required mode (only mandatory updates on startup).
    // This runs for ALL phases except UpdateFailed, maintaining consistency:
    // - First-time setup (Idle): checks before fetching topic_id → Pairing
    // - Reboot with Pairing: checks again to ensure no new mandatory updates
    // - Reboot with Ready: checks to keep device up-to-date
    // Use Blocking execution since we can wait for completion during startup.
    //
    // Acquire device ownership before the check. In the rare case an OTA already owns the device
    // (e.g. a BLE retry started one during the offline->online transition), skip the startup check
    // entirely and stay alive; the owner drives the UI. This matches the old behavior where the
    // self-acquire failed and returned UpdateInProgress (-> Halt -> Ok), just without the redundant
    // result hop.
    let guard = match UpdateGuard::try_acquire(&app_state.update_in_progress) {
        Some(guard) => guard,
        None => {
            println!("MAIN: startup update check skipped, update already in progress");
            return Ok(());
        }
    };
    let check_result = check_and_update_system(
        &app_state,
        &chrome,
        UpdateMode::Required,
        UpdateExecution::Blocking,
        guard,
    )
    .await?;
    match startup_update_check_outcome(check_result) {
        // Too old, updating, already running, or a failed version check. The core check already
        // drove the canonical UI and restored the phase; stay alive and stop here so we do not
        // overwrite that surface or exit the daemon. See startup_update_check_outcome for why a
        // failed check must NOT be fatal.
        StartupUpdateOutcome::Halt => return Ok(()),
        StartupUpdateOutcome::Continue => {} // No update needed: continue with normal flow.
    }

    // No update needed. Show UI based on current phase.
    // If we don't have a topic_id yet, try to get one and transition to Pairing.
    let current_phase = app_state.lifecycle.get();

    // If still in Idle and don't have a non-empty topic_id, try to get it
    if current_phase == SetupPhase::Idle && !try_allocate_pairing_topic(&app_state) {
        // PR #218 review: wait_for_controld is non-fatal now, so controld may simply be
        // absent here — and the old exit-on-timeout + Restart=always loop that used to
        // retry this whole startup path is gone. Without a retry, the QR painted below
        // would carry an EMPTY topic_id in its device_info and the device would sit in
        // Idle until a phone completed BLE setup. Self-heal in the background instead.
        spawn_pairing_topic_retry_loop(app_state.clone(), chrome.clone());
    }

    // Show UI based on phase
    let phase = app_state.lifecycle.get();
    println!("MAIN: startup_with_internet: phase={}", phase.as_str());

    match phase {
        SetupPhase::Ready => show_webapp(&app_state, &chrome).await,
        _ => show_qrcode(&app_state, &chrome).await,
    }
}

/// One attempt to allocate the pairing topic from controld and advance `Idle` → `Pairing`.
///
/// Mirrors the BLE success flow's invariant order exactly: the topic is persisted BEFORE the
/// phase transition, so `Pairing` can never be observed without a usable topic on disk. The
/// phase moves only while still `Idle` — a device mid-BLE-flow (Updating/WifiConnecting) or
/// already Pairing/Ready must never be dragged sideways by a late topic fetch. A persisted
/// topic with the phase still `Idle` (earlier phase-persist failure) intentionally returns
/// true without transitioning, matching the historical startup behavior: the BLE flow owns
/// that repair.
///
/// Returns true when a usable topic is in the store afterwards, whether this call fetched it
/// or another flow already had.
pub fn try_allocate_pairing_topic(app_state: &Arc<AppState>) -> bool {
    try_allocate_pairing_topic_with(app_state, dbus_utils::get_relayer_info)
}

/// Testable core of [`try_allocate_pairing_topic`]: `fetch_topic` is injected so tests can
/// simulate controld being away/back without a session D-Bus.
fn try_allocate_pairing_topic_with(
    app_state: &Arc<AppState>,
    fetch_topic: impl FnOnce() -> Result<String>,
) -> bool {
    let state_store = &app_state.state_store;
    if !needs_relayer_topic_fetch(state_store.get(persistent_state::TOPIC_ID).as_deref()) {
        return true;
    }
    let topic_id = match fetch_topic() {
        Ok(topic_id) => topic_id,
        Err(e) => {
            eprintln!("MAIN: can't get relayer data from controld: {e:#?}");
            return false;
        }
    };
    // Save topic_id FIRST before setting Pairing phase
    state_store.set(persistent_state::TOPIC_ID, &topic_id);
    if let Err(e) = state_store.save() {
        eprintln!("MAIN: Failed to save topic_id: {e:#?}");
        // Don't transition to Pairing if save failed - keep Idle. Report not-allocated so a
        // reboot (or the BLE flow) can redo this from scratch.
        return false;
    }
    if app_state.lifecycle.get() == SetupPhase::Idle {
        // Topic_id saved successfully, now safe to transition to Pairing
        app_state.lifecycle.set(SetupPhase::Pairing);
        if let Err(e) = app_state.lifecycle.persist(state_store) {
            eprintln!("MAIN: Error persisting Pairing phase: {e:#?}");
            // Phase set in memory but not persisted - acceptable since topic_id is saved
        }
    }
    true
}

/// Background self-heal for a failed startup topic allocation (PR #218 review).
///
/// setupd no longer exits when controld is unreachable — the old exit + `Restart=always`
/// loop was an accidental retry of this very fetch — so a first boot with controld away
/// would otherwise paint a pairing QR whose device_info carries an empty topic_id and stay
/// there. This loop retries until a topic is persisted, by us or by the BLE flow (whichever
/// wins; both persist through the same store, so the check below covers either). It repaints
/// the QR only while the stale-topic QR is still the surface on screen: any other page
/// belongs to another flow (BLE messages, updater, webapp) and must not be navigated away —
/// phones on those paths already get live device_info via BLE `get_info`.
pub fn spawn_pairing_topic_retry_loop(app_state: Arc<AppState>, chrome: Arc<CdpHandle>) {
    tokio::spawn(async move {
        loop {
            // Sleep first: the caller's inline attempt just failed.
            tokio::time::sleep(Duration::from_millis(
                constant::PAIRING_TOPIC_RETRY_INTERVAL,
            ))
            .await;
            if !needs_relayer_topic_fetch(
                app_state
                    .state_store
                    .get(persistent_state::TOPIC_ID)
                    .as_deref(),
            ) {
                // Another flow allocated it and owns the UI from here.
                return;
            }
            if !try_allocate_pairing_topic(&app_state) {
                continue;
            }
            let must_repaint = {
                let page = app_state.page.lock().await;
                should_repaint_qr_after_topic(&page)
            };
            if must_repaint {
                println!("MAIN: pairing topic allocated late, repainting QR with device_info");
                let _ = show_qrcode(&app_state, &chrome).await;
            }
            return;
        }
    });
}

/// Whether a late topic allocation must repaint the QR: only when the QR page — painted with
/// an empty topic_id in its device_info URL params — is still what is on screen.
pub fn should_repaint_qr_after_topic(page: &Page) -> bool {
    matches!(page, Page::QRCode(_))
}

// device_info is <device_id>|<topic_id>|<internet>|<branch>|<version>|<setup_phase>
pub fn build_device_info(app_state: &Arc<AppState>) -> String {
    let device_id = app_state.device_id.clone();
    let topic_id = app_state
        .state_store
        .get(persistent_state::TOPIC_ID)
        .unwrap_or_default();
    let has_internet = if app_state.internet.is_online_cached() {
        "true"
    } else {
        "false"
    };
    let branch = app_state.branch.clone().replace('/', "%2F");
    let version = app_state.current_version.clone();
    let setup_phase = get_setup_phase(app_state);

    format!("{device_id}|{topic_id}|{has_internet}|{branch}|{version}|{setup_phase}")
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::connectivity::Connectivity;
    use std::sync::Arc;

    #[test]
    fn build_device_info_includes_setup_phase() {
        let lifecycle = SetupLifecycle::new();
        lifecycle.set(SetupPhase::Updating);

        let temp_dir = tempfile::tempdir().unwrap();
        let state_file = temp_dir.path().join("state.txt");
        let state_store = PersistentState::new(state_file.to_str().unwrap()).unwrap();

        let app_state = Arc::new(AppState {
            device_id: "test-device".to_string(),
            branch: "main/stable".to_string(),
            current_version: "1.2.3".to_string(),
            state_store,
            internet: tokio::runtime::Runtime::new()
                .unwrap()
                .block_on(async { Connectivity::spawn().await }),
            page: Mutex::new(Page::None(0)),
            auto_proceed: AtomicBool::new(false),
            lifecycle,
            update_in_progress: Arc::new(AtomicBool::new(false)),
        });

        let device_info = build_device_info(&app_state);
        let parts: Vec<&str> = device_info.split('|').collect();

        assert_eq!(parts.len(), 6);
        assert_eq!(parts[0], "test-device");
        assert_eq!(parts[1], ""); // topic_id (empty in test)
        // parts[2] is internet status (true/false)
        assert_eq!(parts[3], "main%2Fstable"); // branch with URL encoding
        assert_eq!(parts[4], "1.2.3");
        assert_eq!(parts[5], "updating");
    }

    /// Regression for PR #206 review 4475889867: an offline reboot in UpdateFailed must short-circuit
    /// to the recovery screen just like the online path, instead of falling through to QR while still
    /// advertising setup_phase=update_failed. Only UpdateFailed triggers recovery; every other phase
    /// proceeds with the normal startup flow, keeping both startup branches in lockstep.
    #[test]
    fn startup_recovery_only_for_update_failed() {
        assert!(startup_requires_update_failed_recovery(
            SetupPhase::UpdateFailed
        ));
        for phase in [
            SetupPhase::Idle,
            SetupPhase::WifiConnecting,
            SetupPhase::CheckingVersion,
            SetupPhase::Updating,
            SetupPhase::Pairing,
            SetupPhase::Ready,
        ] {
            assert!(
                !startup_requires_update_failed_recovery(phase),
                "phase {phase:?} must not trigger UpdateFailed recovery",
            );
        }
    }

    /// Fixture for the topic-allocation tests: fresh store, default (Idle) lifecycle.
    fn topic_test_app_state(temp_dir: &tempfile::TempDir) -> Arc<AppState> {
        let state_file = temp_dir.path().join("state.txt");
        let state_store = PersistentState::new(state_file.to_str().unwrap()).unwrap();
        Arc::new(AppState {
            device_id: "test-device".to_string(),
            branch: "main/stable".to_string(),
            current_version: "1.2.3".to_string(),
            state_store,
            internet: tokio::runtime::Runtime::new()
                .unwrap()
                .block_on(async { Connectivity::spawn().await }),
            page: Mutex::new(Page::None(0)),
            auto_proceed: AtomicBool::new(false),
            lifecycle: SetupLifecycle::new(),
            update_in_progress: Arc::new(AtomicBool::new(false)),
        })
    }

    /// PR #218 review regression: with wait_for_controld now non-fatal, the startup topic
    /// fetch can run while controld is away. A failed fetch must leave the device exactly
    /// where it was — Idle, no topic — so the background retry (or a reboot) can redo the
    /// allocation from scratch.
    #[test]
    fn topic_allocation_failure_keeps_idle_and_reports_not_allocated() {
        let temp_dir = tempfile::tempdir().unwrap();
        let app_state = topic_test_app_state(&temp_dir);

        let allocated = try_allocate_pairing_topic_with(&app_state, || {
            Err(anyhow::anyhow!("controld not on the bus"))
        });

        assert!(!allocated);
        assert_eq!(app_state.lifecycle.get(), SetupPhase::Idle);
        let topic = app_state.state_store.get(persistent_state::TOPIC_ID);
        assert!(topic.as_deref().unwrap_or("").is_empty());
    }

    /// The recovery half of the same regression: once controld answers, the topic must be
    /// persisted BEFORE the Idle→Pairing transition (Pairing may never exist without a topic
    /// on disk) and device_info — what the QR repaint and BLE get_info publish — must carry
    /// the non-empty topic.
    #[test]
    fn topic_allocation_success_persists_topic_then_advances_to_pairing() {
        let temp_dir = tempfile::tempdir().unwrap();
        let app_state = topic_test_app_state(&temp_dir);

        let allocated = try_allocate_pairing_topic_with(&app_state, || Ok("topic-123".to_string()));

        assert!(allocated);
        assert_eq!(
            app_state
                .state_store
                .get(persistent_state::TOPIC_ID)
                .as_deref(),
            Some("topic-123")
        );
        assert_eq!(app_state.lifecycle.get(), SetupPhase::Pairing);

        let device_info = build_device_info(&app_state);
        let parts: Vec<&str> = device_info.split('|').collect();
        assert_eq!(parts[1], "topic-123");
        assert_eq!(parts[5], "pairing");
    }

    /// The BLE flow persists through the same store, so a topic that appeared between
    /// retries must be treated as done WITHOUT another controld round-trip — and without a
    /// phase transition, which the flow that allocated the topic owns.
    #[test]
    fn topic_allocation_skips_fetch_when_topic_already_persisted() {
        let temp_dir = tempfile::tempdir().unwrap();
        let app_state = topic_test_app_state(&temp_dir);
        app_state
            .state_store
            .set(persistent_state::TOPIC_ID, "topic-from-ble");

        let allocated = try_allocate_pairing_topic_with(&app_state, || {
            panic!("must not fetch when a topic is already persisted")
        });

        assert!(allocated);
        assert_eq!(app_state.lifecycle.get(), SetupPhase::Idle);
    }

    /// A late fetch completing while another flow moved the phase (e.g. a BLE-driven update
    /// check) must persist the topic but leave the phase alone — no sideways transitions.
    #[test]
    fn topic_allocation_success_outside_idle_leaves_phase_untouched() {
        let temp_dir = tempfile::tempdir().unwrap();
        let app_state = topic_test_app_state(&temp_dir);
        app_state.lifecycle.set(SetupPhase::Updating);

        let allocated = try_allocate_pairing_topic_with(&app_state, || Ok("topic-456".to_string()));

        assert!(allocated);
        assert_eq!(
            app_state
                .state_store
                .get(persistent_state::TOPIC_ID)
                .as_deref(),
            Some("topic-456")
        );
        assert_eq!(app_state.lifecycle.get(), SetupPhase::Updating);
    }

    /// The late-topic QR repaint may only replace the stale-topic QR itself; every other
    /// surface belongs to another flow (BLE messages, updater, webapp, factory reset) and
    /// phones there already receive live device_info over BLE get_info.
    #[test]
    fn qr_repaint_only_when_qr_is_on_screen() {
        assert!(should_repaint_qr_after_topic(&Page::QRCode(0)));
        for page in [
            Page::None(0),
            Page::Message(0, "Connecting to wifi".to_string()),
            Page::SystemUpgrade(0),
            Page::FactoryReset(0),
            Page::WebApp(0),
            Page::ReflashingRequired(0, "reflash".to_string()),
        ] {
            assert!(
                !should_repaint_qr_after_topic(&page),
                "page {page:?} must not be clobbered by a late topic repaint",
            );
        }
    }

    #[test]
    fn build_device_info_exposes_update_failed_for_mobile_polling() {
        let lifecycle = SetupLifecycle::new();
        let temp_dir = tempfile::tempdir().unwrap();
        let state_file = temp_dir.path().join("state.txt");
        let store = PersistentState::new(state_file.to_str().unwrap()).unwrap();

        lifecycle.set(SetupPhase::UpdateFailed);
        lifecycle.persist(&store).unwrap();

        let app_state = Arc::new(AppState {
            device_id: "test-device".to_string(),
            branch: "main/stable".to_string(),
            current_version: "1.2.3".to_string(),
            state_store: store,
            internet: tokio::runtime::Runtime::new()
                .unwrap()
                .block_on(async { Connectivity::spawn().await }),
            page: Mutex::new(Page::None(0)),
            auto_proceed: AtomicBool::new(false),
            lifecycle,
            update_in_progress: Arc::new(AtomicBool::new(false)),
        });

        let device_info = build_device_info(&app_state);
        let parts: Vec<&str> = device_info.split('|').collect();
        assert_eq!(parts[5], "update_failed");
    }
}
