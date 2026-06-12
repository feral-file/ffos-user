//! Startup flows and initialization for feral-setupd.

use crate::app_state::{AppState, Page, unix_s};
use crate::ble::{Ble, BleCallbacks};
use crate::callbacks;
use crate::cdp::Cdp;
use crate::cfg;
use crate::connectivity::Connectivity;
use crate::constant;
use crate::dbus_utils;
use crate::persistent_state::{self, PersistentState};
use crate::phase_logic::get_setup_phase;
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

pub async fn init_cdp() -> Result<Arc<Cdp>> {
    let chrome = Cdp::connect(constant::CDP_URL)
        .await
        .context("connecting to CDP")?;
    Ok(Arc::new(chrome))
}

pub async fn start_ble(
    ble_service: &Arc<Ble>,
    app_state: &Arc<AppState>,
    chrome: &Arc<Cdp>,
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
    chrome: &Arc<Cdp>,
) -> Result<()> {
    println!("MAIN: UpdateFailed phase restored; showing recovered failure message");
    println!("MAIN: Waiting for explicit BLE/D-Bus retry");
    show_system_upgrade(chrome, app_state, UPDATE_FAILED_RECOVERED_MSG).await
}

/// Startup path when the device does **not** have internet at boot time.
///
/// When this is called:
/// - `run` has already waited for `controld` to be reachable.
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
    chrome: &Arc<Cdp>,
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
/// - `run` has already waited for `controld` to be reachable.
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
    chrome: &Arc<Cdp>,
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
pub async fn on_startup_with_internet(app_state: Arc<AppState>, chrome: Arc<Cdp>) -> Result<()> {
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
    let state_store = &app_state.state_store;
    let current_phase = app_state.lifecycle.get();

    // If still in Idle and don't have topic_id, try to get it
    if current_phase == SetupPhase::Idle && state_store.get(persistent_state::TOPIC_ID).is_none() {
        match dbus_utils::get_relayer_info() {
            Ok(topic_id) => {
                // Save topic_id FIRST before setting Pairing phase
                state_store.set(persistent_state::TOPIC_ID, &topic_id);
                if let Err(e) = state_store.save() {
                    eprintln!("MAIN: Failed to save topic_id: {e:#?}");
                    // Don't transition to Pairing if save failed - keep Idle
                    // Device will retry on next boot or BLE flow
                } else {
                    // Topic_id saved successfully, now safe to transition to Pairing
                    app_state.lifecycle.set(SetupPhase::Pairing);
                    if let Err(e) = app_state.lifecycle.persist(state_store) {
                        eprintln!("MAIN: Error persisting Pairing phase: {e:#?}");
                        // Phase set in memory but not persisted - acceptable since topic_id is saved
                    }
                }
            }
            Err(e) => {
                eprintln!(
                    "MAIN: startup_with_internet: can't get relayer data from controld: {e:#?}"
                );
            }
        }
    }

    // Show UI based on phase
    let phase = app_state.lifecycle.get();
    println!("MAIN: startup_with_internet: phase={}", phase.as_str());

    match phase {
        SetupPhase::Ready => show_webapp(&app_state, &chrome).await,
        _ => show_qrcode(&app_state, &chrome).await,
    }
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
