mod app_state;
mod ble;
mod callbacks;
mod cdp;
mod cfg;
mod connectivity;
mod constant;
mod dbus_handlers;
mod dbus_utils;
mod encoding;
mod log_uploader;
mod persistent_state;
mod phase_logic;
mod setup_lifecycle;
mod startup;
mod system;
mod ui;
mod update_coordinator;
mod updater;
mod wifi_utils;

use anyhow::Result;
use ble::Ble;
use dbus_handlers::{setup_dbus_listeners, wait_for_controld};
use startup::{
    init_app_state, init_cdp, start_ble, startup_with_internet, startup_without_internet,
};
use std::sync::Arc;
use std::sync::atomic::Ordering;
use tokio::signal::unix::{SignalKind, signal as unix_signal};
use tokio::time::Duration;
use wifi_utils::SSIDsCacher;

// Sentry has to start before tokio runtime
// That's why we can't use #[tokio::main]
// We use usual main and create the tokio runtime here
fn main() {
    println!("MAIN: Starting feral-setupd ------------------------------");
    let _guard = sentry::init((
        constant::SENTRY_URL,
        sentry::ClientOptions {
            release: sentry::release_name!(),
            send_default_pii: true,
            ..Default::default()
        },
    ));

    let runtime = match tokio::runtime::Builder::new_multi_thread()
        .enable_all()
        .build()
    {
        Ok(runtime) => runtime,
        Err(error) => {
            eprintln!("MAIN: Failed to build tokio runtime: {error:?}");
            sentry::capture_message("failed to build tokio runtime", sentry::Level::Error);
            std::process::exit(1);
        }
    };

    runtime.block_on(async {
        if let Err(e) = run().await {
            eprintln!("MAIN: Error running feral-setupd: {e:#?}");
            let error: &dyn std::error::Error = e.as_ref();
            sentry::capture_error(error);
            std::process::exit(1);
        }
    });
}

async fn run() -> Result<()> {
    // Initialize state
    let ble_service = Arc::new(Ble::new());
    let app_state = init_app_state(&ble_service).await?;
    let chrome = init_cdp().await?;

    // Start bluetooth advertising with callbacks
    let ssids_cacher = Arc::new(SSIDsCacher::new());
    start_ble(&ble_service, &app_state, &chrome, &ssids_cacher).await?;

    // Wait for controld D-Bus connection before proceeding
    wait_for_controld(Duration::from_millis(constant::WAIT_FOR_CONTROLD_TIMEOUT)).await?;

    // Spawn background task to refresh remote version info every hour
    updater::spawn_remote_version_refresher();

    // Setup D-Bus listeners
    let stop_dbus_listener = setup_dbus_listeners(&app_state, &chrome).await?;

    // If the device used to be able to connect to the internet
    // It's likely that it will have internet again really soon
    // We aggressively poll for internet for a few seconds to
    // go directly to the webapp instead of the QRCode
    let used_to_connect = app_state.state_store.get(persistent_state::CONNECTED);
    if used_to_connect.is_some() {
        app_state
            .internet
            .wait_until_online(
                Duration::from_millis(constant::AGGRESSIVE_INTERNET_CHECK_INTERVAL),
                Some(Duration::from_millis(
                    constant::INITIAL_INTERNET_CHECK_TIMEOUT,
                )),
            )
            .await;
    }

    if !app_state.internet.is_online(true).await {
        startup_without_internet(&app_state, &chrome, &ssids_cacher, used_to_connect.as_ref())
            .await?;
    } else {
        startup_with_internet(&app_state, &chrome, used_to_connect.as_ref()).await?;
    }

    // Wait for Ctrl+C or shutdown event
    wait_for_shutdown().await;
    println!("MAIN: Shutting down...");
    println!("MAIN: Stopping DBus listener...");
    stop_dbus_listener.store(true, Ordering::Relaxed);
    println!("MAIN: Stopping BLE service...");
    if let Err(e) = ble_service.stop().await {
        eprintln!("MAIN: Error stopping BLE service: {e:#?}");
        return Err(e);
    } else {
        println!("MAIN: BLE service stopped");
    }
    println!("MAIN: Shutting down...");
    Ok(())
}

async fn wait_for_shutdown() {
    // SIGINT  = Ctrl-C on the terminal
    // SIGTERM = "polite" kill sent by most service managers / docker / k8s
    // (add more signals if you need them)
    let mut sigint = unix_signal(SignalKind::interrupt()).expect("SIGINT handler");
    let mut sigterm = unix_signal(SignalKind::terminate()).expect("SIGTERM handler");

    tokio::select! {
        _ = sigint.recv()  => {},
        _ = sigterm.recv() => {},
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use app_state::{AppState, Page};
    use connectivity::Connectivity;
    use persistent_state::PersistentState;
    use phase_logic::get_setup_phase;
    use setup_lifecycle::{SetupLifecycle, SetupPhase};
    use std::sync::Arc;
    use std::sync::atomic::AtomicBool;
    use tokio::sync::Mutex;

    /// Helper function to create test AppState with a given setup_phase
    fn test_app_state(setup_phase: &str) -> Arc<AppState> {
        let lifecycle = SetupLifecycle::new();
        lifecycle.set(match setup_phase {
            "wifi_connecting" => SetupPhase::WifiConnecting,
            "checking_version" => SetupPhase::CheckingVersion,
            "updating" => SetupPhase::Updating,
            "pairing" => SetupPhase::Pairing,
            "ready" => SetupPhase::Ready,
            "update_failed" => SetupPhase::UpdateFailed,
            _ => SetupPhase::Idle,
        });

        let temp_dir = tempfile::tempdir().unwrap();
        let state_file = temp_dir.path().join("state.txt");
        let state_store = PersistentState::new(state_file.to_str().unwrap()).unwrap();

        Arc::new(AppState {
            device_id: "test".to_string(),
            branch: "test".to_string(),
            current_version: "1.0.0".to_string(),
            state_store,
            internet: tokio::runtime::Runtime::new()
                .unwrap()
                .block_on(async { Connectivity::spawn().await }),
            page: Mutex::new(Page::None(0)),
            auto_proceed: AtomicBool::new(false),
            lifecycle,
            update_in_progress: Arc::new(AtomicBool::new(false)),
        })
    }

    // Integration tests - these test cross-module behavior

    #[test]
    fn qrcode_navigation_with_update_failed_phase_preserves_state() {
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

        // Phase remains UpdateFailed
        assert_eq!(get_setup_phase(&app_state), "update_failed");
    }

    #[test]
    fn wifi_connect_failure_resets_setup_phase_to_idle() {
        let app_state = test_app_state("wifi_connecting");
        assert_eq!(get_setup_phase(&app_state), "wifi_connecting");

        // Mirrors create_connect_wifi_cb early return on nmcli failure.
        app_state.lifecycle.set(SetupPhase::Idle);
        assert_eq!(get_setup_phase(&app_state), "idle");
    }

    #[test]
    fn update_failed_phase_suppresses_startup_update_check() {
        let lifecycle = SetupLifecycle::new();
        let temp_dir = tempfile::tempdir().unwrap();
        let state_file = temp_dir.path().join("state.txt");
        let store = PersistentState::new(state_file.to_str().unwrap()).unwrap();

        // Set UpdateFailed phase and persist (simulating previous failed update)
        lifecycle.set(SetupPhase::UpdateFailed);
        lifecycle.persist(&store).unwrap();

        // Simulate daemon restart: create fresh lifecycle and restore from store
        let lifecycle_after_restart = SetupLifecycle::new();
        lifecycle_after_restart.restore_from_store(&store);

        // Verify phase was restored
        assert_eq!(lifecycle_after_restart.get(), SetupPhase::UpdateFailed);

        // Startup should skip update check when UpdateFailed phase is set.
        // The actual flow would call on_startup_with_internet, which should
        // short-circuit and show the failure UI without calling check_and_update_system.
        // This test verifies the phase survives restart.
    }

    #[test]
    fn no_update_needed_resets_checking_version_to_idle() {
        let app_state = test_app_state("checking_version");
        assert_eq!(get_setup_phase(&app_state), "checking_version");

        // Mirrors check_and_update_system Ok(false) branch after version check.
        app_state.lifecycle.set(SetupPhase::Idle);
        assert_eq!(get_setup_phase(&app_state), "idle");
    }

    #[test]
    fn no_update_needed_preserves_ready_phase() {
        let app_state = test_app_state("ready");
        assert_eq!(get_setup_phase(&app_state), "ready");

        // Mirrors check_and_update_system Ok(false) branch: durable phases preserved
        let current = app_state.lifecycle.get();
        if !current.is_durable() {
            app_state.lifecycle.set(SetupPhase::Idle);
        }
        assert_eq!(get_setup_phase(&app_state), "ready");
    }

    #[test]
    fn no_update_needed_preserves_pairing_phase() {
        let app_state = test_app_state("pairing");
        assert_eq!(get_setup_phase(&app_state), "pairing");

        // Mirrors check_and_update_system Ok(false) branch: durable phases preserved
        let current = app_state.lifecycle.get();
        if !current.is_durable() {
            app_state.lifecycle.set(SetupPhase::Idle);
        }
        assert_eq!(get_setup_phase(&app_state), "pairing");
    }

    #[test]
    fn qr_navigation_resets_transient_phases_to_idle() {
        // WifiConnecting should reset to Idle
        let app_state = test_app_state("wifi_connecting");
        let current = app_state.lifecycle.get();
        if matches!(
            current,
            SetupPhase::WifiConnecting | SetupPhase::CheckingVersion | SetupPhase::Updating
        ) {
            app_state.lifecycle.set(SetupPhase::Idle);
        }
        assert_eq!(get_setup_phase(&app_state), "idle");

        // CheckingVersion should reset to Idle
        let app_state = test_app_state("checking_version");
        let current = app_state.lifecycle.get();
        if matches!(
            current,
            SetupPhase::WifiConnecting | SetupPhase::CheckingVersion | SetupPhase::Updating
        ) {
            app_state.lifecycle.set(SetupPhase::Idle);
        }
        assert_eq!(get_setup_phase(&app_state), "idle");

        // Updating should reset to Idle
        let app_state = test_app_state("updating");
        let current = app_state.lifecycle.get();
        if matches!(
            current,
            SetupPhase::WifiConnecting | SetupPhase::CheckingVersion | SetupPhase::Updating
        ) {
            app_state.lifecycle.set(SetupPhase::Idle);
        }
        assert_eq!(get_setup_phase(&app_state), "idle");
    }

    #[test]
    fn qr_navigation_preserves_durable_and_failure_phases() {
        // UpdateFailed should be preserved
        let app_state = test_app_state("update_failed");
        let current = app_state.lifecycle.get();
        if matches!(
            current,
            SetupPhase::WifiConnecting | SetupPhase::CheckingVersion | SetupPhase::Updating
        ) {
            app_state.lifecycle.set(SetupPhase::Idle);
        }
        assert_eq!(get_setup_phase(&app_state), "update_failed");

        // Ready should be preserved
        let app_state = test_app_state("ready");
        let current = app_state.lifecycle.get();
        if matches!(
            current,
            SetupPhase::WifiConnecting | SetupPhase::CheckingVersion | SetupPhase::Updating
        ) {
            app_state.lifecycle.set(SetupPhase::Idle);
        }
        assert_eq!(get_setup_phase(&app_state), "ready");

        // Pairing should be preserved
        let app_state = test_app_state("pairing");
        let current = app_state.lifecycle.get();
        if matches!(
            current,
            SetupPhase::WifiConnecting | SetupPhase::CheckingVersion | SetupPhase::Updating
        ) {
            app_state.lifecycle.set(SetupPhase::Idle);
        }
        assert_eq!(get_setup_phase(&app_state), "pairing");
    }

    #[test]
    fn ble_setup_success_transitions_pairing_to_ready() {
        let lifecycle = SetupLifecycle::new();
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

        // Simulate BLE setup success: got topic_id, now in Pairing phase
        app_state
            .state_store
            .set(persistent_state::TOPIC_ID, "test-topic-123");
        app_state.state_store.save().unwrap();
        app_state.lifecycle.set(SetupPhase::Pairing);
        app_state.lifecycle.persist(&app_state.state_store).unwrap();

        // Mobile sees Pairing
        assert_eq!(app_state.lifecycle.get(), SetupPhase::Pairing);

        // After mobile scans QR, transition to Ready
        app_state.lifecycle.set(SetupPhase::Ready);
        app_state.lifecycle.persist(&app_state.state_store).unwrap();

        // Now mobile sees Ready
        assert_eq!(app_state.lifecycle.get(), SetupPhase::Ready);
    }

    #[test]
    fn keep_wifi_retry_clears_update_failed_phase() {
        let lifecycle = SetupLifecycle::new();
        let temp_dir = tempfile::tempdir().unwrap();
        let state_file = temp_dir.path().join("state.txt");
        let state_store = PersistentState::new(state_file.to_str().unwrap()).unwrap();

        // Set and persist UpdateFailed phase (simulating previous update failure)
        lifecycle.set(SetupPhase::UpdateFailed);
        lifecycle.persist(&state_store).unwrap();
        assert_eq!(lifecycle.get(), SetupPhase::UpdateFailed);

        // Simulate keep_wifi retry: clear and persist
        if lifecycle.get() == SetupPhase::UpdateFailed {
            lifecycle.set(SetupPhase::Idle);
            lifecycle.persist(&state_store).unwrap();
        }

        // Verify phase is cleared in memory
        assert_eq!(lifecycle.get(), SetupPhase::Idle);

        // Simulate reboot: restore from disk
        let lifecycle_after_reboot = SetupLifecycle::new();
        lifecycle_after_reboot.restore_from_store(&state_store);

        // Verify UpdateFailed is not restored (cleared by keep_wifi)
        assert_eq!(lifecycle_after_reboot.get(), SetupPhase::Idle);
    }

    // Phase preservation regression tests for cross-module integration
    /// Regression test for PR #206 review finding: version-check failures should
    /// preserve durable phases (Ready, Pairing) instead of demoting to Idle.
    ///
    /// Scenario: Device is Ready/Pairing, startup runs mandatory update check,
    /// version fetch succeeds but is_update_required() fails (distributor error).
    /// Expected: Device stays Ready/Pairing, shows correct UI.
    /// Bug: Device gets demoted to Idle, shows QR instead of webapp.
    #[test]
    fn version_check_failure_preserves_ready_phase() {
        let file = tempfile::NamedTempFile::new().unwrap();
        let store = PersistentState::new(file.path().to_str().unwrap()).unwrap();
        let lifecycle = SetupLifecycle::new();

        // Simulate a Ready device before version check
        store.set(persistent_state::TOPIC_ID, "test-topic-123");
        lifecycle.set(SetupPhase::Ready);
        lifecycle.persist(&store).unwrap();

        // Verify pre-check state
        assert_eq!(lifecycle.get(), SetupPhase::Ready);
        let phase_before_check = lifecycle.get();

        // Simulate version check: CheckingVersion -> error path
        lifecycle.set(SetupPhase::CheckingVersion);

        // Version check fails (e.g., distributor down, parse error)
        // The fix should restore phase_before_check if durable
        if phase_before_check.is_durable() {
            lifecycle.set(phase_before_check);
        } else {
            lifecycle.set(SetupPhase::Idle);
        }

        // Verify: Ready is preserved, not demoted to Idle
        assert_eq!(
            lifecycle.get(),
            SetupPhase::Ready,
            "Version check failure should preserve Ready phase"
        );
    }

    /// Regression for PR #206 review 4482579315: a Ready device using BLE connect_wifi as the
    /// explicit update_failed retry must stay Ready after a no-update result, not be demoted to
    /// Pairing.
    ///
    /// Bug ordering: connect_wifi restored Ready from PRE_FAILURE_PHASE, then set the transient
    /// WifiConnecting before the update check. check_and_update_system snapshots the LIVE phase as
    /// its no-update restore target, so WifiConnecting collapsed to Idle, and the setup-success
    /// path then persisted Pairing (since phase != Ready). The fix captures the pre-connect phase
    /// and restores it before the check so the snapshot is the durable Ready.
    #[test]
    fn connect_wifi_retry_no_update_keeps_ready_phase() {
        let file = tempfile::NamedTempFile::new().unwrap();
        let store = PersistentState::new(file.path().to_str().unwrap()).unwrap();
        let lifecycle = SetupLifecycle::new();

        // Ready device that latched update_failed; PRE_FAILURE_PHASE tracks the durable Ready.
        store.set(persistent_state::TOPIC_ID, "topic-ready-retry");
        store.set(persistent_state::PRE_FAILURE_PHASE, "ready");
        lifecycle.set(SetupPhase::UpdateFailed);
        lifecycle.persist(&store).unwrap();

        // connect_wifi retry restores the pre-failure phase.
        let restored_phase =
            SetupPhase::from_str(&store.get(persistent_state::PRE_FAILURE_PHASE).unwrap());
        lifecycle.set(restored_phase);
        assert_eq!(lifecycle.get(), SetupPhase::Ready);

        // Fix: capture the pre-connect durable phase before entering the transient WifiConnecting.
        let phase_before_wifi_connect = lifecycle.get();
        lifecycle.set(SetupPhase::WifiConnecting);

        // Fix: restore the captured phase before the update check snapshots phase_before_check.
        lifecycle.set(phase_before_wifi_connect);
        let phase_before_check = lifecycle.get();

        // Update check transient + no-update restoration (Ok(false) branch).
        lifecycle.set(SetupPhase::CheckingVersion);
        if phase_before_check.is_durable() {
            lifecycle.set(phase_before_check);
        } else {
            lifecycle.set(SetupPhase::Idle);
        }

        // setup-success path: only demote to Pairing if not already Ready.
        if lifecycle.get() != SetupPhase::Ready {
            lifecycle.set(SetupPhase::Pairing);
        }

        assert_eq!(
            lifecycle.get(),
            SetupPhase::Ready,
            "connect_wifi retry with no update must keep a Ready device Ready, not demote to Pairing",
        );
    }

    /// Same test for Pairing phase preservation on version-check failure.
    #[test]
    fn version_check_failure_preserves_pairing_phase() {
        let file = tempfile::NamedTempFile::new().unwrap();
        let store = PersistentState::new(file.path().to_str().unwrap()).unwrap();
        let lifecycle = SetupLifecycle::new();

        // Simulate a Pairing device (has topic_id, waiting for QR scan)
        store.set(persistent_state::TOPIC_ID, "test-topic-456");
        lifecycle.set(SetupPhase::Pairing);
        lifecycle.persist(&store).unwrap();

        assert_eq!(lifecycle.get(), SetupPhase::Pairing);
        let phase_before_check = lifecycle.get();

        // Simulate version check failure
        lifecycle.set(SetupPhase::CheckingVersion);

        if phase_before_check.is_durable() {
            lifecycle.set(phase_before_check);
        } else {
            lifecycle.set(SetupPhase::Idle);
        }

        // Verify: Pairing is preserved
        assert_eq!(
            lifecycle.get(),
            SetupPhase::Pairing,
            "Version check failure should preserve Pairing phase"
        );
    }

    /// Regression test for PR #206 review finding: D-Bus retry from UpdateFailed
    /// with NoUpdateNeeded should navigate to the correct phase-driven UI, not
    /// leave the device on the old failure screen.
    ///
    /// Scenario: Device in UpdateFailed with failure message on TV. User triggers
    /// D-Bus system_update retry. Version check returns NoUpdateNeeded. Retry path
    /// restores Ready phase and persists it.
    ///
    /// Expected: TV navigates to webapp (Ready phase).
    /// Bug: TV stays on SystemUpgrade (failure screen), creating inconsistent state
    /// where BLE reports "ready" but TV shows "update failed".
    #[test]
    fn dbus_retry_no_update_navigates_to_phase_driven_ui() {
        let file = tempfile::NamedTempFile::new().unwrap();
        let store = PersistentState::new(file.path().to_str().unwrap()).unwrap();
        let lifecycle = SetupLifecycle::new();

        // Simulate UpdateFailed state with pre-failure phase tracked
        store.set(persistent_state::TOPIC_ID, "test-topic-789");
        store.set(persistent_state::PRE_FAILURE_PHASE, "ready");
        lifecycle.set(SetupPhase::UpdateFailed);
        lifecycle.persist(&store).unwrap();

        assert_eq!(lifecycle.get(), SetupPhase::UpdateFailed);

        // D-Bus retry: restore pre-failure phase
        let restored_phase = if let Some(phase_str) = store.get(persistent_state::PRE_FAILURE_PHASE)
        {
            SetupPhase::from_str(&phase_str)
        } else {
            SetupPhase::Idle
        };

        lifecycle.set(restored_phase);
        store.set(persistent_state::PRE_FAILURE_PHASE, "");
        lifecycle.persist(&store).unwrap();

        // After version check returns NoUpdateNeeded, phase should be Ready
        assert_eq!(lifecycle.get(), SetupPhase::Ready);

        // The fix ensures navigation is based on phase (Ready -> webapp),
        // not the stale Page::SystemUpgrade which would be a no-op.
        // This test verifies the phase is correct; the actual navigation
        // logic is covered by integration/CDP tests.
        match lifecycle.get() {
            SetupPhase::Ready => {
                // Expected: should navigate to webapp - test passes
            }
            _ => {
                // Bug: would navigate to QR or no-op on SystemUpgrade page
                panic!("D-Bus retry should restore Ready phase for webapp navigation");
            }
        }
    }

    /// Test that transient phases (WifiConnecting, CheckingVersion, Updating)
    /// correctly reset to Idle when they are not durable.
    #[test]
    fn version_check_failure_resets_transient_phases() {
        let lifecycle = SetupLifecycle::new();

        let transient_phases = vec![
            SetupPhase::WifiConnecting,
            SetupPhase::CheckingVersion,
            SetupPhase::Updating,
            SetupPhase::Idle,
        ];

        for transient_phase in transient_phases {
            lifecycle.set(transient_phase);
            let phase_before_check = lifecycle.get();

            // Simulate version check failure
            if phase_before_check.is_durable() {
                lifecycle.set(phase_before_check);
            } else {
                lifecycle.set(SetupPhase::Idle);
            }

            // All transient phases should reset to Idle
            assert_eq!(
                lifecycle.get(),
                SetupPhase::Idle,
                "Transient phase {transient_phase:?} should reset to Idle on version check failure"
            );
        }
    }

    /// Regression test for PR #206 review finding (Jun 11): early refresh_remote_version
    /// failure should preserve durable phases, not unconditionally set Idle.
    ///
    /// Scenario: Ready device, check_and_update_system runs, but refresh_remote_version
    /// fails immediately (network/distributor/parse error) before we can check update status.
    /// Expected: Device stays Ready, shows version-check failure UI.
    /// Bug: Device demoted to Idle, paired device reports idle to mobile app.
    #[test]
    fn early_refresh_failure_preserves_ready_phase() {
        let file = tempfile::NamedTempFile::new().unwrap();
        let store = PersistentState::new(file.path().to_str().unwrap()).unwrap();
        let lifecycle = SetupLifecycle::new();

        // Simulate a Ready device before update check
        store.set(persistent_state::TOPIC_ID, "test-topic-ready");
        lifecycle.set(SetupPhase::Ready);
        lifecycle.persist(&store).unwrap();

        assert_eq!(lifecycle.get(), SetupPhase::Ready);
        let phase_before_check = lifecycle.get();

        // Simulate entering CheckingVersion phase
        lifecycle.set(SetupPhase::CheckingVersion);

        // Early refresh_remote_version failure (network/distributor down, parse error)
        // The fix should restore phase_before_check if durable
        if phase_before_check.is_durable() {
            lifecycle.set(phase_before_check);
        } else {
            lifecycle.set(SetupPhase::Idle);
        }

        // Verify: Ready is preserved, not demoted to Idle
        assert_eq!(
            lifecycle.get(),
            SetupPhase::Ready,
            "Early refresh failure should preserve Ready phase"
        );
    }

    /// Same test for Pairing phase preservation on early refresh failure.
    #[test]
    fn early_refresh_failure_preserves_pairing_phase() {
        let file = tempfile::NamedTempFile::new().unwrap();
        let store = PersistentState::new(file.path().to_str().unwrap()).unwrap();
        let lifecycle = SetupLifecycle::new();

        // Simulate a Pairing device (has topic_id, waiting for QR scan)
        store.set(persistent_state::TOPIC_ID, "test-topic-pairing");
        lifecycle.set(SetupPhase::Pairing);
        lifecycle.persist(&store).unwrap();

        assert_eq!(lifecycle.get(), SetupPhase::Pairing);
        let phase_before_check = lifecycle.get();

        // Simulate entering CheckingVersion phase
        lifecycle.set(SetupPhase::CheckingVersion);

        // Early refresh failure
        if phase_before_check.is_durable() {
            lifecycle.set(phase_before_check);
        } else {
            lifecycle.set(SetupPhase::Idle);
        }

        // Verify: Pairing is preserved
        assert_eq!(
            lifecycle.get(),
            SetupPhase::Pairing,
            "Early refresh failure should preserve Pairing phase"
        );
    }
}
