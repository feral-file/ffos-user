//! BLE and D-Bus callback factory functions for feral-setupd.

use crate::app_state::AppState;
use crate::ble;
use crate::cdp::Cdp;
use crate::constant;
use crate::dbus_utils;
use crate::persistent_state;
use crate::phase_logic::{
    ble_terminal_reset_phase, get_setup_phase, needs_relayer_topic_fetch,
    pairing_confirmation_promotes_to_ready, qr_switch_blocked_by_update_failed,
};
use crate::setup_lifecycle::SetupPhase;
use crate::ui::{show_factory_reset, show_message, show_qrcode, show_webapp};
use crate::update_coordinator::{
    AvailableSystemUpdateFollowUp, RestorePageTarget, UpdateCheckResult, UpdateExecution,
    UpdateGuard, UpdateMode, available_system_update_follow_up, check_and_update_system,
    restore_page_target,
};
use crate::wifi_utils::{self, Error as WifiError};
use serde::Deserialize;
use std::sync::Arc;
use std::sync::atomic::Ordering;
use std::time::{Duration, Instant};
use tokio::task;

/// Re-latch the `UpdateFailed` state after a recovery retry that failed before confirming success.
///
/// Called on every early-exit path in a BLE/D-Bus recovery retry that started from `UpdateFailed`
/// but bailed before the update was confirmed (wrong Wi-Fi password, no internet after connect,
/// or a transient `VersionCheckFailed`). Without this, the latch cleared at retry entry is
/// silently lost mid-session: mobile polling sees `idle` or `ready` instead of `update_failed`,
/// the recovery screen is abandoned, and the unresolved mandatory update is invisible until the
/// next reboot.
///
/// `PRE_FAILURE_PHASE` was already re-saved to the correct durable phase by the latch-clear
/// block at retry entry, so this function only needs to restore `setup_phase = update_failed`
/// in-memory and on disk. Persist failure is best-effort: the in-memory latch is set, so mobile
/// still sees `update_failed` this session even if the disk write fails.
fn relatch_update_failed(app_state: &Arc<AppState>, caller: &str) {
    app_state.lifecycle.set(SetupPhase::UpdateFailed);
    if let Err(e) = app_state.lifecycle.persist(&app_state.state_store) {
        eprintln!("{caller}: Failed to re-latch UpdateFailed after retry failure: {e:#?}");
    }
}

async fn internet_setup_successfully_cb(
    app_state: &Arc<AppState>,
    chromium: &Arc<Cdp>,
    guard: UpdateGuard,
    // True when this callback is entered from a recovery retry that started in `UpdateFailed`.
    // On VersionCheckFailed the latch must be restored so mobile still sees update_failed and
    // the recovery screen stays intact — see relatch_update_failed.
    relatch_on_failure: bool,
) -> Result<String, ble::BleStatus> {
    // Check and update system using Required mode (only mandatory updates)
    // Use NonBlocking execution since BLE flow needs to return quickly.
    // `guard` was acquired by the BLE callback before any Wi-Fi/phase/UI side effects and is handed
    // off here so the whole retry is serialized against OTA.
    match check_and_update_system(
        app_state,
        chromium,
        UpdateMode::Required,
        UpdateExecution::NonBlocking,
        guard,
    )
    .await
    {
        Ok(UpdateCheckResult::TooOldToUpgrade) => {
            app_state.lifecycle.set(SetupPhase::Idle);
            return Err(ble::BleStatus::VersionTooOld);
        }
        Ok(UpdateCheckResult::UpdateStarted) | Ok(UpdateCheckResult::UpdateInProgress) => {
            return Err(ble::BleStatus::DeviceUpdating);
        }
        Ok(UpdateCheckResult::VersionCheckFailed) => {
            if relatch_on_failure {
                // Transient version-check failure during a recovery retry: restore UpdateFailed
                // so mobile keeps seeing the failure state and the recovery screen stays intact.
                relatch_update_failed(app_state, "BLE");
            } else {
                // Preserve the durable phase check_and_update_system already restored
                // (Ready/Pairing); only collapse to Idle for non-durable phases. Forcing Idle
                // here would demote a device that just recovered from update_failed via this
                // BLE retry.
                app_state
                    .lifecycle
                    .set(ble_terminal_reset_phase(app_state.lifecycle.get()));
            }
            return Err(ble::BleStatus::VersionCheckFailed);
        }
        Ok(UpdateCheckResult::NoUpdateNeeded) => {} // Continue with normal flow
        Err(e) => {
            // This shouldn't happen with NonBlocking, but handle it anyway
            eprintln!("MAIN: Error during update check: {e:#?}");
            if relatch_on_failure {
                relatch_update_failed(app_state, "BLE");
            } else {
                app_state
                    .lifecycle
                    .set(ble_terminal_reset_phase(app_state.lifecycle.get()));
            }
            return Err(ble::BleStatus::VersionCheckFailed);
        }
    }

    // Reuse a persisted topic instead of re-fetching from controld on every BLE success.
    // get_relayer_info() blocks up to 31s and the BLE handler awaits this callback before replying,
    // so a Pairing/Ready device (which already has its topic) must return immediately rather than
    // risk a slow/failed GetRelayerTopicID. Only first-time setup (no usable persisted topic) hits
    // the network. See needs_relayer_topic_fetch.
    let state_store = &app_state.state_store;
    let persisted_topic = state_store.get(persistent_state::TOPIC_ID);
    let topic_id = if needs_relayer_topic_fetch(persisted_topic.as_deref()) {
        let fetched = match dbus_utils::get_relayer_info() {
            Ok(info) => info,
            Err(e) => {
                eprintln!("BLE: can't get relayer data from controld: {e:#?}");
                return Err(ble::BleStatus::ServerUnreachable);
            }
        };

        // Save topic_id to disk BEFORE setting Pairing phase to maintain invariant.
        // Fail the operation if save fails, so we don't create invalid state.
        state_store.set(persistent_state::TOPIC_ID, &fetched);
        if let Err(e) = state_store.save() {
            eprintln!("BLE: Failed to save topic_id: {e:#?}");
            return Err(ble::BleStatus::UnknownError);
        }
        fetched
    } else {
        // Unwrap is safe: needs_relayer_topic_fetch returned false only for Some(non-empty).
        persisted_topic.expect("persisted topic present when fetch is skipped")
    };

    // Only transition to Pairing if not already Ready.
    // Ready devices completing a BLE retry should stay Ready, not be demoted to Pairing.
    // This preserves the durable Ready phase across update check cycles.
    let current_phase = app_state.lifecycle.get();
    if current_phase != SetupPhase::Ready {
        // NOW safe to set Pairing phase (topic_id is guaranteed to exist on disk).
        // Pairing invariant: phase can only be Pairing when topic_id exists.
        app_state.lifecycle.set(SetupPhase::Pairing);
        if let Err(e) = app_state.lifecycle.persist(state_store) {
            eprintln!("BLE: Failed to persist Pairing phase: {e:#?}");
            // Phase is set in memory but not persisted - acceptable since topic_id is saved.
            // Next boot will restore from disk (Idle) and can re-fetch topic_id.
        }
    }

    let app_state = app_state.clone();
    let chromium = chromium.clone();
    task::spawn(async move {
        let _ = show_message(&chromium, &app_state, constant::SETUP_SUCCESSFULLY_MSG).await;
    });
    Ok(topic_id)
}

#[derive(Deserialize)]
struct BundledUploadLogsPayload {
    api_key: String,
    support_bundle_id: String,
}

fn parse_bundled_upload_logs_payload(
    payload: &[u8],
) -> serde_json::Result<BundledUploadLogsPayload> {
    serde_json::from_slice::<BundledUploadLogsPayload>(payload)
}

pub fn create_bt_connected_cb(
    app_state: Arc<AppState>,
    chromium: Arc<Cdp>,
) -> ble::BTConnectedCallback {
    Some(Box::new(move || {
        let app_state = app_state.clone();
        let chromium = chromium.clone();

        Box::pin(async move {
            let should_show_welcome = {
                let page = app_state.page.lock().await;
                use crate::app_state::Page;
                matches!(*page, Page::QRCode(_))
            };
            if should_show_welcome {
                let _ = show_message(&chromium, &app_state, constant::WELCOME_MSG).await;
            }
        })
    }))
}

pub fn create_bt_disconnected_cb(
    app_state: Arc<AppState>,
    chromium: Arc<Cdp>,
) -> ble::BTDisconnectedCallback {
    Some(Box::new(move || {
        let app_state = app_state.clone();
        let chromium = chromium.clone();

        Box::pin(async move {
            // Reads the canonical `page`. During a blocking version check that page is the
            // real prior surface (e.g. webapp), not the transient "Checking..." copy — the
            // progress UI navigates transiently and never records `page`. So a disconnect
            // mid-check now acts on the actual surface (keeping webapp/recovery instead of
            // dropping an already-settled device to QR), matching steady-state behavior.
            let should_go_qrcode = {
                let page = app_state.page.lock().await;
                !page.should_keep_on_bt_disconnect()
            };
            if should_go_qrcode {
                let _ = show_qrcode(&app_state, &chromium).await;
            }
        })
    }))
}

pub fn create_connect_wifi_cb(
    app_state: Arc<AppState>,
    chromium: Arc<Cdp>,
) -> ble::ConnectWifiCallback {
    Box::new(move |ssid, pwd| {
        let app_state = app_state.clone();
        let chromium = chromium.clone();
        let ssid = ssid.to_string();
        let pwd = pwd.to_string();
        Box::pin(async move {
            let start_time = Instant::now();

            // Acquire device ownership BEFORE any side effects, serializing this retry against
            // OTA for its entire duration. A load-only precheck left a TOCTOU window: it could
            // observe "no update", a concurrent task could then acquire ownership, and this path
            // would still switch Wi-Fi via nmcli (disrupting an in-flight download) and clobber
            // the setup_phase/UI mobile polls. Holding the guard from here through the update
            // check (handed to internet_setup_successfully_cb) closes that window. RAII releases
            // it on every early return below.
            let guard = match UpdateGuard::try_acquire(&app_state.update_in_progress) {
                Some(guard) => guard,
                None => {
                    println!("BLE: connect_wifi rejected, update in progress");
                    return Err(ble::BleStatus::DeviceUpdating);
                }
            };

            // Track whether we entered this retry from UpdateFailed so that any failure below
            // can re-latch the state. The latch must be cleared now (before transient phases),
            // but if the retry fails at any step the device must return to update_failed —
            // otherwise mobile polling loses visibility of the unresolved failure mid-session.
            let relatch_on_failure = app_state.lifecycle.get() == SetupPhase::UpdateFailed;

            // Clear and restore UpdateFailed phase on explicit WiFi retry.
            // Restore the pre-failure durable phase if tracked, otherwise use Idle.
            if relatch_on_failure {
                use crate::persistent_state::PRE_FAILURE_PHASE;

                let restored_phase = if let Some(phase_str) =
                    app_state.state_store.get(PRE_FAILURE_PHASE)
                {
                    let phase = SetupPhase::from_str(&phase_str);
                    println!(
                        "BLE: Restoring pre-failure phase '{}' on WiFi retry",
                        phase.as_str()
                    );
                    phase
                } else {
                    println!("BLE: Clearing UpdateFailed to Idle (no pre-failure phase tracked)");
                    SetupPhase::Idle
                };

                app_state.lifecycle.set(restored_phase);

                // Clear the pre-failure tracking
                app_state.state_store.set(PRE_FAILURE_PHASE, "");

                // Abort the retry if the cleared UpdateFailed phase cannot be persisted.
                // Proceeding would leave setup_phase=update_failed on disk while the
                // in-memory phase says otherwise, so a crash/reboot mid-retry would
                // resurrect UpdateFailed. Mirrors the D-Bus do_system_update guard.
                if let Err(e) = app_state.lifecycle.persist(&app_state.state_store) {
                    eprintln!("BLE: Failed to persist phase restoration, aborting retry: {e:#?}");
                    return Err(ble::BleStatus::UnknownError);
                }

                // Re-save durable phase to PRE_FAILURE_PHASE before entering transient state.
                // This ensures the phase survives the update check cycle and can be restored
                // if the retry fails again. Without this, a second failure would have no
                // durable phase to restore from, permanently losing Ready/Pairing state.
                if restored_phase.is_durable() && restored_phase != SetupPhase::UpdateFailed {
                    app_state
                        .state_store
                        .set(PRE_FAILURE_PHASE, restored_phase.as_str());
                    if let Err(e) = app_state.state_store.save() {
                        eprintln!("BLE: Failed to re-save PRE_FAILURE_PHASE before retry: {e:#?}");
                    }
                }
            }

            // Capture the durable phase (Ready/Pairing) this device had before the BLE setup.
            // The mandatory update check inside internet_setup_successfully_cb snapshots the
            // LIVE phase as its no-update restore target. WifiConnecting is transient, so if it
            // were still live at that point a no-update result would collapse to Idle and the
            // setup-success path would then demote a previously Ready device to Pairing. We
            // restore this captured phase just before the check (see below).
            let phase_before_wifi_connect = app_state.lifecycle.get();

            app_state.lifecycle.set(SetupPhase::WifiConnecting);
            // Show message
            let connecting_msg = format!("{}{}", constant::WIFI_CONNECTING_MSG_PREFIX, ssid);
            let _ = show_message(&chromium, &app_state, &connecting_msg).await;

            // Disable auto proceed since users want to setup another wifi
            // Instead of fixing the current internet (if there is any)
            app_state.auto_proceed.store(false, Ordering::Release);

            // Connect to wifi & return early if failed
            if let Err(e) = wifi_utils::connect(&ssid, &pwd).await {
                eprintln!(
                    "MAIN: Failed to connect to wifi \"{ssid}\" in {:?} ms: {e: }",
                    start_time.elapsed().as_millis()
                );
                if relatch_on_failure {
                    // Recovery retry: restore update_failed so mobile keeps seeing the failure.
                    relatch_update_failed(&app_state, "BLE");
                } else {
                    app_state.lifecycle.set(SetupPhase::Idle);
                }
                // Tell user that the wifi connection failed
                task::spawn(async move {
                    let _ =
                        show_message(&chromium, &app_state, constant::WIFI_FAILED_TO_CONNECT_MSG)
                            .await;
                });
                // This is a bit of a hack to detect wrong password
                // But the command doesn't provide a reliable way to detect this
                let err_code = match &e {
                    WifiError::NmcliFailure { stderr, .. } if stderr.contains("password") => {
                        ble::BleStatus::WrongWifiPassword
                    }
                    _ => ble::BleStatus::UnknownError,
                };
                return Err(err_code);
            }

            // Wait for internet connectivity (up to 6 seconds)
            // WiFi may be connected but internet routing can take a moment
            app_state
                .internet
                .wait_until_online(
                    Duration::from_millis(constant::WIFI_INTERNET_CHECK_INTERVAL),
                    Some(Duration::from_millis(constant::WIFI_INTERNET_WAIT_TIMEOUT)),
                )
                .await;

            // Return early if there is still no internet after waiting
            if !app_state.internet.is_online(true).await {
                if relatch_on_failure {
                    // Recovery retry: restore update_failed so mobile keeps seeing the failure.
                    relatch_update_failed(&app_state, "BLE");
                } else {
                    app_state.lifecycle.set(SetupPhase::Idle);
                }
                task::spawn(async move {
                    let _ = show_message(
                        &chromium,
                        &app_state,
                        constant::INTERNET_FAILED_TO_CONNECT_MSG,
                    )
                    .await;
                });
                return Err(ble::BleStatus::NoInternet);
            }

            // Restore the pre-connect durable phase so the mandatory update check's no-update
            // restoration keeps Ready/Pairing instead of resetting the transient WifiConnecting
            // to Idle (which would demote a previously Ready device to Pairing in the
            // setup-success path). Idle/Pairing devices are unchanged: a first-time Idle device
            // still flows to topic_id fetch -> Pairing below.
            app_state.lifecycle.set(phase_before_wifi_connect);
            internet_setup_successfully_cb(&app_state, &chromium, guard, relatch_on_failure).await
        })
    })
}

pub fn create_keep_wifi_cb(app_state: Arc<AppState>, chromium: Arc<Cdp>) -> ble::KeepWifiCallback {
    Box::new(move || {
        let app_state = app_state.clone();
        let chromium = chromium.clone();
        Box::pin(async move {
            // Acquire device ownership before any phase/UI side effects so the whole retry is
            // serialized against OTA (mirrors connect_wifi). A load-only precheck left a TOCTOU
            // window where a concurrent task could acquire ownership after the check while this
            // path still cleared/restored the persisted UpdateFailed latch. RAII releases the
            // guard on every early return below.
            let guard = match UpdateGuard::try_acquire(&app_state.update_in_progress) {
                Some(guard) => guard,
                None => {
                    println!("BLE: keep_wifi rejected, update in progress");
                    return Err(ble::BleStatus::DeviceUpdating);
                }
            };

            if !app_state.internet.is_online(true).await {
                return Err(ble::BleStatus::WifiRequired);
            }

            // Track whether we entered this retry from UpdateFailed so that a transient
            // VersionCheckFailed inside internet_setup_successfully_cb can re-latch.
            let relatch_on_failure = app_state.lifecycle.get() == SetupPhase::UpdateFailed;

            // Clear and restore UpdateFailed phase on explicit keep_wifi retry.
            // Restore the pre-failure durable phase if tracked, otherwise use Idle.
            if relatch_on_failure {
                use crate::persistent_state::PRE_FAILURE_PHASE;

                let restored_phase = if let Some(phase_str) =
                    app_state.state_store.get(PRE_FAILURE_PHASE)
                {
                    let phase = SetupPhase::from_str(&phase_str);
                    println!(
                        "BLE: Restoring pre-failure phase '{}' on keep_wifi retry",
                        phase.as_str()
                    );
                    phase
                } else {
                    println!("BLE: Clearing UpdateFailed to Idle (no pre-failure phase tracked)");
                    SetupPhase::Idle
                };

                app_state.lifecycle.set(restored_phase);

                // Clear the pre-failure tracking
                app_state.state_store.set(PRE_FAILURE_PHASE, "");

                // Abort the retry if the cleared UpdateFailed phase cannot be persisted.
                // Proceeding would leave setup_phase=update_failed on disk while the
                // in-memory phase says otherwise, so a crash/reboot mid-retry would
                // resurrect UpdateFailed. Mirrors the D-Bus do_system_update guard.
                if let Err(e) = app_state.lifecycle.persist(&app_state.state_store) {
                    eprintln!("BLE: Failed to persist phase restoration, aborting retry: {e:#?}");
                    return Err(ble::BleStatus::UnknownError);
                }

                // Re-save durable phase to PRE_FAILURE_PHASE before entering transient state.
                // This ensures the phase survives the update check cycle and can be restored
                // if the retry fails again. Without this, a second failure would have no
                // durable phase to restore from, permanently losing Ready/Pairing state.
                if restored_phase.is_durable() && restored_phase != SetupPhase::UpdateFailed {
                    app_state
                        .state_store
                        .set(PRE_FAILURE_PHASE, restored_phase.as_str());
                    if let Err(e) = app_state.state_store.save() {
                        eprintln!("BLE: Failed to re-save PRE_FAILURE_PHASE before retry: {e:#?}");
                    }
                }
            }

            internet_setup_successfully_cb(&app_state, &chromium, guard, relatch_on_failure).await
        })
    })
}

pub fn create_get_info_cb(app_state: Arc<AppState>) -> ble::GetInfoCallback {
    Some(Box::new(move || {
        let phase = get_setup_phase(&app_state);
        if matches!(phase.as_str(), "updating" | "checking_version") {
            // Trigger async refresh to avoid blocking BLE response on D-Bus timeout.
            // Fresh value will be available on next poll (mobile polls every few seconds).
            let _ = app_state.internet.trigger_refresh_async();
        }
        vec![crate::startup::build_device_info(&app_state)]
    }))
}

pub fn create_qrcode_switch_cb(
    app_state: Arc<AppState>,
    chromium: Arc<Cdp>,
) -> dbus_utils::ListenCallback {
    Box::new(move |msg| {
        println!("MAIN: QR switch callback received");
        let chromium = chromium.clone();
        let app_state = app_state.clone();
        let mut qrcode_requested = false;
        match msg.read1::<bool>() {
            Ok(true) => qrcode_requested = true,
            Err(e) => println!("MAIN: Error reading message: {e: }"),
            _ => {}
        }
        println!("MAIN: QR switch -> qrcode_requested={qrcode_requested}");
        tokio::runtime::Handle::current().block_on(async move {
            // A `show_pairing_qr_code(false)` pairing confirmation is a durable, one-shot signal:
            // controld ACKs it after this callback returns and does not re-emit. Record the
            // Pairing -> Ready transition BEFORE the navigation ownership gate so it is never
            // lost when an OTA owns the device. (Regression from afd9ee4, which gated the whole
            // switch and dropped this persist; the load-only check was kept by a898a72.) The
            // transition is also valid when an in-flight OTA op transiently masks Pairing with a
            // non-durable phase while a topic_id is already persisted; check_and_update_system's
            // restore then refuses to demote this Ready back to Pairing (phase_after_inflight_check).
            if !qrcode_requested {
                let has_topic = app_state
                    .state_store
                    .get(crate::persistent_state::TOPIC_ID)
                    .is_some_and(|t| !t.is_empty());
                if pairing_confirmation_promotes_to_ready(app_state.lifecycle.get(), has_topic) {
                    println!("MAIN: QR switch -> transitioning to Ready phase");
                    app_state.lifecycle.set(SetupPhase::Ready);
                    if let Err(e) = app_state.lifecycle.persist(&app_state.state_store) {
                        eprintln!("MAIN: Error persisting Ready phase: {e:#?}");
                    }
                }
            }

            // Navigation must not clobber a screen owned by an active OTA. Acquire ownership for
            // the navigation only; if an OTA owns the device, skip painting now — the durable
            // transition above is already recorded and the post-update reboot (or a later
            // canonical-page restore) repaints the correct surface. Held until this block
            // returns (RAII), serializing the navigation against a concurrent update check.
            let _guard = match UpdateGuard::try_acquire(&app_state.update_in_progress) {
                Some(guard) => guard,
                None => {
                    println!("MAIN: QR switch navigation skipped, update in progress");
                    return;
                }
            };

            // Stay on the permanent-failure surface until an explicit retry clears the latch.
            // handle_permanent_update_failure releases the update guard but keeps setup_phase ==
            // update_failed, so without this a QR/webapp switch from controld would navigate
            // Chromium off the failure message while the device still reports update_failed.
            // Only BLE connect_wifi/keep_wifi or D-Bus system_update clear the latch and move on.
            if qr_switch_blocked_by_update_failed(app_state.lifecycle.get()) {
                println!(
                    "MAIN: QR switch navigation skipped, update_failed latched (awaiting retry)"
                );
                return;
            }

            if qrcode_requested {
                let _ = show_qrcode(&app_state, &chromium).await;
            } else {
                let _ = show_webapp(&app_state, &chromium).await;
            }
        });
    })
}

async fn do_factory_reset(chromium: &Arc<Cdp>, app_state: &Arc<AppState>) {
    // Show factory reset page
    let _ = show_factory_reset(chromium, app_state).await;
    // Execute factory reset
    if let Err(e) = crate::system::factory_reset().await {
        eprintln!("MAIN: Failed to execute factory reset: {e:#?}");
    }
}

async fn do_system_update(chromium: &Arc<Cdp>, app_state: &Arc<AppState>) {
    // Acquire device ownership BEFORE any connectivity probe, no-internet navigation, or
    // UpdateFailed latch clear/restore. A load-only precheck left a TOCTOU window where a
    // concurrent task could acquire ownership after the check while this path still mutated the
    // persisted latch and UI. Holding the guard from here through check_and_update_system (which
    // receives it) serializes the whole manual retry against OTA. RAII releases it on return.
    let guard = match UpdateGuard::try_acquire(&app_state.update_in_progress) {
        Some(guard) => guard,
        None => {
            println!("MAIN: system_update ignored, update already in progress");
            return;
        }
    };

    // Check internet connectivity first before clearing latch
    if !app_state.internet.is_online(true).await {
        eprintln!("MAIN: System update requested but no internet connection");
        let _ = show_message(
            chromium,
            app_state,
            constant::INTERNET_FAILED_TO_CONNECT_MSG,
        )
        .await;
        return;
    }

    // Track whether we entered from UpdateFailed so a transient VersionCheckFailed below
    // can re-latch the state rather than silently leaving the device in a non-failure phase.
    let relatch_on_failure = app_state.lifecycle.get() == SetupPhase::UpdateFailed;

    // Clear and restore UpdateFailed phase on explicit D-Bus manual retry.
    // Restore the pre-failure durable phase if tracked, otherwise use Idle.
    if relatch_on_failure {
        use crate::persistent_state::PRE_FAILURE_PHASE;

        let restored_phase = if let Some(phase_str) = app_state.state_store.get(PRE_FAILURE_PHASE) {
            let phase = SetupPhase::from_str(&phase_str);
            println!(
                "DBUS: Restoring pre-failure phase '{}' on manual system update",
                phase.as_str()
            );
            phase
        } else {
            println!("DBUS: Clearing UpdateFailed to Idle (no pre-failure phase tracked)");
            SetupPhase::Idle
        };

        app_state.lifecycle.set(restored_phase);

        // Clear the pre-failure tracking
        app_state.state_store.set(PRE_FAILURE_PHASE, "");

        if let Err(e) = app_state.lifecycle.persist(&app_state.state_store) {
            eprintln!("DBUS: Failed to persist phase restoration: {e:#?}");
            return;
        }
    }

    // Use Available mode for user-triggered updates (check for any newer version)
    // Use Blocking execution since D-Bus callback runs in a spawned task anyway
    let result = check_and_update_system(
        app_state,
        chromium,
        UpdateMode::Available,
        UpdateExecution::Blocking,
        guard,
    )
    .await;

    // Re-latch UpdateFailed if this was a recovery retry and the version check failed
    // transiently. The latch-clear block above already ran, so without this the device
    // would leave update_failed mid-session while the mandatory update is still unresolved.
    if relatch_on_failure {
        if let Ok(UpdateCheckResult::VersionCheckFailed) = &result {
            relatch_update_failed(app_state, "DBUS");
        }
    }

    match available_system_update_follow_up(&result) {
        AvailableSystemUpdateFollowUp::RestoreCurrentPage => {
            // The blocking version-check progress UI repaints Chromium with the transient
            // "Checking for updates..." screen and intentionally does NOT record
            // app_state.page (see navigate_transient_message). So after a no-update result
            // the TV is left on that transient screen while app_state.page still holds the
            // real canonical surface. We must actively re-show that canonical page or the
            // device stays stuck on "Checking for updates...". The decision is factored into
            // restore_page_target so it can be unit-tested without driving CDP.
            let current_page = { app_state.page.lock().await.clone() };
            let target = restore_page_target(&current_page, app_state.lifecycle.get());
            let nav_result = match target {
                RestorePageTarget::Webapp => show_webapp(app_state, chromium).await,
                RestorePageTarget::Qrcode => show_qrcode(app_state, chromium).await,
                RestorePageTarget::Message(msg) => show_message(chromium, app_state, &msg).await,
                RestorePageTarget::FactoryReset => show_factory_reset(chromium, app_state).await,
                RestorePageTarget::NoChange => Ok(()),
            };
            if let Err(e) = nav_result {
                eprintln!("MAIN: Failed to restore canonical page after D-Bus no-update: {e:#?}");
            }
        }
        AvailableSystemUpdateFollowUp::LogFailure => {
            eprintln!("MAIN: System update failed: {result:#?}");
        }
        AvailableSystemUpdateFollowUp::NoOp => {}
    }
}

pub fn create_factory_reset_cb(
    app_state: Arc<AppState>,
    chromium: Arc<Cdp>,
) -> ble::FactoryResetCallback {
    Some(Box::new(move || {
        let chromium = chromium.clone();
        let app_state = app_state.clone();
        Box::pin(async move {
            do_factory_reset(&chromium, &app_state).await;
        })
    }))
}

pub fn create_factory_reset_dbus_cb(
    app_state: Arc<AppState>,
    chromium: Arc<Cdp>,
) -> dbus_utils::ListenCallback {
    Box::new(move |_msg| {
        println!("MAIN: Factory reset DBus callback received");
        let chromium = chromium.clone();
        let app_state = app_state.clone();
        task::spawn(async move {
            do_factory_reset(&chromium, &app_state).await;
        });
    })
}

pub fn create_system_update_dbus_cb(
    app_state: Arc<AppState>,
    chromium: Arc<Cdp>,
) -> dbus_utils::ListenCallback {
    Box::new(move |_msg| {
        println!("MAIN: System update DBus callback received");
        let chromium = chromium.clone();
        let app_state = app_state.clone();
        task::spawn(async move {
            do_system_update(&chromium, &app_state).await;
        });
    })
}

/// Core log upload logic - zips logs folder and uploads via v2 API
async fn do_upload_logs(
    device_id: &str,
    api_key: &str,
    source: &str,
    branch: &str,
    version: &str,
    support_bundle_id: Option<&str>,
) {
    println!("MAIN: Uploading logs (source: {source})");

    if let Err(e) = crate::log_uploader::submit_logs(
        device_id,
        api_key,
        source,
        branch,
        version,
        support_bundle_id,
    )
    .await
    {
        eprintln!("MAIN: Failed to submit logs: error code {e}");
    } else {
        println!("MAIN: Logs submitted successfully");
    }
}

pub fn create_submit_logs_cb(app_state: Arc<AppState>) -> ble::SubmitLogsCallback {
    Box::new(move |_user_id, api_key, _title, support_bundle_id| {
        let api_key = api_key.to_string();
        let support_bundle_id = support_bundle_id.map(str::to_string);
        let device_id = app_state.device_id.clone();
        let branch = app_state.branch.clone();
        let version = app_state.current_version.clone();
        Box::pin(async move {
            do_upload_logs(
                &device_id,
                &api_key,
                "ble",
                &branch,
                &version,
                support_bundle_id.as_deref(),
            )
            .await;
        })
    })
}

pub fn create_upload_logs_dbus_cb(app_state: Arc<AppState>) -> dbus_utils::ListenCallback {
    Box::new(move |msg| {
        println!("MAIN: Upload logs DBus callback received");
        // Read parameters: user_id, api_key, title (title unused in v2 API)
        let (_user_id, api_key, _title): (String, String, String) =
            match msg.read3::<String, String, String>() {
                Ok(params) => params,
                Err(e) => {
                    eprintln!("MAIN: Failed to read upload logs parameters: {e:#?}");
                    return;
                }
            };

        let device_id = app_state.device_id.clone();
        let branch = app_state.branch.clone();
        let version = app_state.current_version.clone();
        task::spawn(async move {
            do_upload_logs(&device_id, &api_key, "dbus", &branch, &version, None).await;
        });
    })
}

pub fn create_upload_logs_with_bundle_dbus_cb(
    app_state: Arc<AppState>,
) -> dbus_utils::ListenCallback {
    Box::new(move |msg| {
        println!("MAIN: Upload logs with bundle DBus callback received");
        let payload = match msg.read1::<Vec<u8>>() {
            Ok(payload) => payload,
            Err(e) => {
                eprintln!("MAIN: Failed to read bundled upload logs payload: {e:#?}");
                return;
            }
        };
        let payload = match parse_bundled_upload_logs_payload(&payload) {
            Ok(payload) => payload,
            Err(e) => {
                eprintln!("MAIN: Failed to parse bundled upload logs payload: {e:#?}");
                return;
            }
        };

        let device_id = app_state.device_id.clone();
        let branch = app_state.branch.clone();
        let version = app_state.current_version.clone();
        task::spawn(async move {
            do_upload_logs(
                &device_id,
                &payload.api_key,
                "dbus",
                &branch,
                &version,
                Some(&payload.support_bundle_id),
            )
            .await;
        });
    })
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn parse_bundled_upload_logs_payload_accepts_controld_schema() {
        let payload = br#"{
            "user_id": "ignored-user",
            "api_key": "test-api-key",
            "title": "ignored-title",
            "support_bundle_id": "bundle-123"
        }"#;

        let parsed =
            parse_bundled_upload_logs_payload(payload).expect("parse bundled upload payload");

        assert_eq!(parsed.api_key, "test-api-key");
        assert_eq!(parsed.support_bundle_id, "bundle-123");
    }
}
