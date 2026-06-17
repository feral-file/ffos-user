//! Update orchestration, retry logic, and phase management for system updates.

use crate::app_state::{AppState, Page};
use crate::cdp::Cdp;
use crate::constant;
use crate::phase_logic::phase_after_inflight_check;
use crate::setup_lifecycle::SetupPhase;
use crate::ui::{
    VersionCheckProgress, show_message, show_reflashing_qrcode, show_system_upgrade,
    show_version_check_failure,
};
use crate::updater;
use anyhow::Result;
use std::sync::Arc;
use std::sync::atomic::{AtomicBool, Ordering};
use tokio::{
    task,
    time::{self, Duration},
};

pub const MAX_UPDATE_RETRIES: u8 = 3;

// Message shown when update fails (fresh failure during current session)
pub const UPDATE_FAILED_FRESH_MSG: &str = "Update didn't complete. Use your phone to try again, or restart the device. If it still fails, contact support@feralfile.com.";

// Message shown on reboot when UpdateFailed phase is restored from disk.
// User already restarted, so guide them to retry via phone or contact support.
pub const UPDATE_FAILED_RECOVERED_MSG: &str = "Previous update failed. The device attempted to update but couldn't complete. Use your phone to retry the update. If the problem persists, contact support@feralfile.com for assistance.";

/// Controls when an update should be triggered.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum UpdateMode {
    /// Update only when below the minimum supported version (mandatory update).
    Required,
    /// Update when any newer version is available (optional/user-triggered update).
    Available,
}

/// Controls how the update check executes side effects (UI updates, update process).
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum UpdateExecution {
    /// Run operations in foreground (blocking). Use for startup/D-Bus flows where
    /// we can wait for completion.
    Blocking,
    /// Spawn operations in background. Use for BLE flows where we need to return
    /// quickly to send a response to the mobile app.
    NonBlocking,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum UpdateErrorType {
    Transient,
    Permanent,
}

/// Update failure with classification decided from the updater message before
/// setupd adds context wrappers for logging.
#[derive(Debug)]
pub struct UpdateAttemptError {
    source: anyhow::Error,
    kind: UpdateErrorType,
}

impl std::fmt::Display for UpdateAttemptError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        self.source.fmt(f)
    }
}

impl std::error::Error for UpdateAttemptError {
    fn source(&self) -> Option<&(dyn std::error::Error + 'static)> {
        Some(self.source.as_ref())
    }
}

/// Classify error messages from bash update scripts and updater infrastructure.
/// Transient errors are worth retrying at the setupd level; permanent errors are not.
///
/// This classification matches actual error messages produced by:
/// - feral-updater.sh and feral-system-update.sh (ffos repo)
/// - updater.rs internal errors (this repo)
///
/// # ⚠️ FRAGILE CONTRACT WARNING
///
/// This function relies on **exact string matching** with error messages from shell scripts
/// in the `ffos` repository (`feral-system-update.sh`, `feral-recovery-update.sh`).
///
/// **Any changes to script error messages MUST be accompanied by:**
/// - Updates to this classification function
/// - Verification via integration testing
///
/// **Future improvement:** Consider structured error codes or stable prefixes to reduce
/// brittleness and improve cross-repository maintainability.
pub fn classify_updater_message(message: &str) -> UpdateErrorType {
    let msg = message.trim();

    // ════════════════════════════════════════════════════════════════════════
    // PERMANENT ERRORS: Do not retry at setupd level
    // ════════════════════════════════════════════════════════════════════════

    // Cryptographic failures (server published bad/corrupt image)
    if msg.starts_with("Error: Signature verification failed for")
        || msg.starts_with("Error: Signature file ")
    {
        return UpdateErrorType::Permanent;
    }

    // Image integrity failures (corrupt or invalid image structure)
    if msg == "airootfs.sfs not found in image." || msg == "No post-extraction script found in ISO"
    {
        return UpdateErrorType::Permanent;
    }

    // Filesystem operation failures (disk full, permissions, etc.)
    if msg == "Failed to create snapshot '/.snapshots/@ota_prev'. Aborting."
        || msg.starts_with("Failed to create snapshot")
    {
        return UpdateErrorType::Permanent;
    }

    // Unknown/unclassified errors from updater.rs
    if msg == "Unknown error occurred" {
        return UpdateErrorType::Permanent;
    }

    // ════════════════════════════════════════════════════════════════════════
    // TRANSIENT ERRORS: Worth retrying at setupd level
    // ════════════════════════════════════════════════════════════════════════

    // Network connectivity and download failures (no internet or server unreachable)
    if msg == "No network connection. Aborting update."
        || msg == "Failed to retrieve content length for image."
        || msg == "Failed to download OTA image."
        || msg == "Failed to download signature file."
        || msg == "Failed to download recovery ISO."
    {
        return UpdateErrorType::Transient;
    }

    // setupd infrastructure failures (service start, log file access)
    if msg.contains("Failed to start updater service")
        || msg.contains("Failed to open /var/log/updaterd.log")
        || msg == "updater closed channel without sending progress"
    {
        return UpdateErrorType::Transient;
    }

    // Lock failures (another update process running or stale lock)
    // Transient because lock may be released by the time we retry
    if msg.contains("Lock already held by another instance")
        || msg.contains("either Lock already held")
    {
        return UpdateErrorType::Transient;
    }

    // ════════════════════════════════════════════════════════════════════════
    // BASH ERR TRAP: Parse the trapped command to determine if network-related
    // ════════════════════════════════════════════════════════════════════════
    //
    // Format: EXCEPTION ERR: LINE=118 CMD="curl -u ... -o /var/tmp/ota/image.zip"
    //
    // Network commands (curl, wget) are transient; other failures (mount, rsync,
    // btrfs, mkinitcpio, etc.) are permanent.
    //
    if msg.starts_with("EXCEPTION ERR:") {
        if msg.contains("curl") || msg.contains("wget") {
            return UpdateErrorType::Transient;
        }
        return UpdateErrorType::Permanent;
    }

    // ════════════════════════════════════════════════════════════════════════
    // DEFAULT: Unknown errors are permanent (avoid infinite retry loops)
    // ════════════════════════════════════════════════════════════════════════
    UpdateErrorType::Permanent
}

/// RAII guard for the update_in_progress flag.
/// Automatically releases the flag when dropped, ensuring cleanup on all return paths
/// (success, early return, panic, or error propagation via `?`).
///
/// Owns an Arc<AtomicBool> to be 'static-safe for tokio::spawn usage.
pub struct UpdateGuard {
    flag: Arc<AtomicBool>,
}

impl UpdateGuard {
    pub fn try_acquire(flag: &Arc<AtomicBool>) -> Option<Self> {
        if flag
            .compare_exchange(false, true, Ordering::Acquire, Ordering::Relaxed)
            .is_ok()
        {
            Some(UpdateGuard { flag: flag.clone() })
        } else {
            None
        }
    }
}

impl Drop for UpdateGuard {
    fn drop(&mut self) {
        self.flag.store(false, Ordering::Release);
    }
}

pub async fn update(app_state: Arc<AppState>, chrome: Arc<Cdp>) -> Result<()> {
    app_state.lifecycle.set(SetupPhase::Updating);

    let latest_version = updater::latest_version().await.unwrap_or_else(|e| {
        eprintln!("MAIN: latest_version for update banner: {e:#?}");
        "Unknown".to_string()
    });
    let base_msg = format!("{} {}", &constant::UPDATING_MSG_PREFIX, latest_version);
    let default_subtext = constant::UPDATING_MSG_SUBTEXT;

    let mut attempt = 0u8;
    loop {
        attempt += 1;

        if attempt == 1 {
            let _ = show_system_upgrade(
                &chrome,
                &app_state,
                &format!("{base_msg}&subtext={default_subtext}"),
            )
            .await;
        }

        match attempt_update(&app_state, &chrome, &base_msg).await {
            Ok(()) => {
                // Update succeeded. Reset to Idle; if device was already paired,
                // phase will be restored to Ready on next boot from persistent state.
                //
                // Clear the stale recovery marker. PRE_FAILURE_PHASE was written before
                // entering the transient update states, but is only overwritten on a
                // subsequent check when the prior phase is durable. Leaving it set would
                // let a later failure that starts from a non-durable phase restore a
                // phantom Ready/Pairing during update_failed recovery.
                use crate::persistent_state::PRE_FAILURE_PHASE;
                app_state.state_store.set(PRE_FAILURE_PHASE, "");
                app_state.lifecycle.set(SetupPhase::Idle);
                if let Err(e) = app_state.state_store.save() {
                    eprintln!("MAIN: Failed to clear PRE_FAILURE_PHASE on update success: {e:#?}");
                }
                return Ok(());
            }
            Err(e) => {
                match e.kind {
                    UpdateErrorType::Transient if attempt < MAX_UPDATE_RETRIES => {
                        eprintln!(
                            "MAIN: Transient update error (attempt {attempt}/{MAX_UPDATE_RETRIES}): {e:#?}"
                        );
                        let retry_msg = format!(
                            "Retrying update (attempt {}/{MAX_UPDATE_RETRIES})",
                            attempt + 1
                        );
                        let _ = show_system_upgrade(
                            &chrome,
                            &app_state,
                            &format!("{base_msg}&subtext={retry_msg}"),
                        )
                        .await;
                        // Exponential backoff: 2^attempt seconds (2s, 4s, 8s, ...)
                        let backoff_seconds = 1u64 << attempt; // 2^attempt
                        time::sleep(Duration::from_secs(backoff_seconds)).await;
                        continue;
                    }
                    UpdateErrorType::Permanent | UpdateErrorType::Transient => {
                        handle_permanent_update_failure(&app_state, &chrome, &e.source).await;
                        return Err(e.source.context("update failed permanently"));
                    }
                }
            }
        }
    }
}

async fn attempt_update(
    app_state: &Arc<AppState>,
    chrome: &Arc<Cdp>,
    base_msg: &str,
) -> Result<(), UpdateAttemptError> {
    let mut rx = updater::spawn_updater().map_err(|e| UpdateAttemptError {
        kind: classify_updater_message(&e.to_string()),
        source: e,
    })?;
    let mut received_progress = false;

    while let Some(res) = rx.recv().await {
        match res {
            Ok(msg) => {
                received_progress = true;
                let _ =
                    show_system_upgrade(chrome, app_state, &format!("{base_msg}&subtext={msg}"))
                        .await;
            }
            Err(e) => {
                let _ = show_system_upgrade(chrome, app_state, &format!("{base_msg}&subtext={e}"))
                    .await;
                let kind = classify_updater_message(&e.to_string());
                return Err(UpdateAttemptError {
                    source: e.context("update process failed"),
                    kind,
                });
            }
        }
    }

    // Channel closed without receiving any progress - updater failed to start.
    // This happens when watchdog stop fails, systemd start fails, or log file not found.
    if !received_progress {
        let message = "updater closed channel without sending progress";
        return Err(UpdateAttemptError {
            kind: classify_updater_message(message),
            source: anyhow::anyhow!(message),
        });
    }

    Ok(())
}

/// Show a clear failure message and keep it visible while `setup_phase=update_failed`.
/// Mobile app can trigger retry via `keep_wifi` or `connect_wifi` BLE commands.
/// Uses fresh failure message since this is called immediately after update failure.
pub async fn handle_permanent_update_failure(
    app_state: &Arc<AppState>,
    chrome: &Arc<Cdp>,
    error: &anyhow::Error,
) {
    eprintln!("MAIN: Permanent update failure: {error:#?}");

    // PRE_FAILURE_PHASE was already stored in check_and_update_system before entering
    // CheckingVersion, capturing the original Ready/Pairing phase before transient states.

    app_state.lifecycle.set(SetupPhase::UpdateFailed);
    if let Err(e) = app_state.lifecycle.persist(&app_state.state_store) {
        eprintln!("MAIN: Failed to persist UpdateFailed phase: {e:#?}");
        eprintln!("MAIN: Phase is set in memory; mobile sees update_failed until restart");
    }
    // Show fresh failure message - user can try restarting or retry via phone
    let _ = show_system_upgrade(chrome, app_state, UPDATE_FAILED_FRESH_MSG).await;
}

/// Result of checking and potentially executing a system update.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum UpdateCheckResult {
    /// Device is too old to auto-upgrade; reflashing is required.
    TooOldToUpgrade,
    /// An update was started (device will reboot after completion).
    UpdateStarted,
    /// No update is needed/available for the given mode.
    NoUpdateNeeded,
    /// Failed to check version information from the server.
    VersionCheckFailed,
    /// Update already in progress; concurrent attempt was skipped.
    #[allow(dead_code)]
    UpdateInProgress,
}

/// How `on_startup_with_internet` proceeds after the mandatory startup update check.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum StartupUpdateOutcome {
    /// Stop the startup flow early and leave the daemon running in steady state. The core check
    /// already drove the canonical UI (reflashing screen, updater-owned progress, or failure copy)
    /// and restored the correct phase, so startup must NOT continue to topic_id/QR/webapp logic.
    Halt,
    /// Continue to topic_id fetch and surface selection (the normal no-update path).
    Continue,
}

/// Map a mandatory startup update-check result to the startup control-flow decision.
///
/// Critically, `VersionCheckFailed` maps to `Halt`, NOT a fatal error: a transient
/// distributor/network/parse failure during the startup check must keep `feral-setupd` alive as a
/// BLE/polling retry surface. Propagating an error instead would exit the process and, under
/// `Restart=always` + `StartLimitBurst=5`, crash-loop the daemon until systemd stops restarting it.
/// The failure UI and durable-phase restoration already happened inside `check_and_update_system`.
pub fn startup_update_check_outcome(result: UpdateCheckResult) -> StartupUpdateOutcome {
    match result {
        UpdateCheckResult::TooOldToUpgrade
        | UpdateCheckResult::UpdateStarted
        | UpdateCheckResult::UpdateInProgress
        | UpdateCheckResult::VersionCheckFailed => StartupUpdateOutcome::Halt,
        UpdateCheckResult::NoUpdateNeeded => StartupUpdateOutcome::Continue,
    }
}

/// Follow-up after `check_and_update_system(UpdateMode::Available, …)` in the D-Bus path.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum AvailableSystemUpdateFollowUp {
    /// No newer build: re-show the CURRENT canonical page so the TV leaves the transient
    /// "Checking for updates..." URL the progress UI may have painted.
    RestoreCurrentPage,
    /// Blocking orchestration failed (CDP navigate, updater start, etc.).
    LogFailure,
    /// Core already drove the final TV screen (update started, reflash, classified fetch error).
    NoOp,
}

/// Decide the follow-up for the D-Bus `system_update` path.
///
/// On `NoUpdateNeeded` the core did NOT navigate a final screen, so the TV is whatever was painted
/// last — and because the version-check progress UI is transient (`navigate_transient_message`
/// never records `page`), that may be the "Checking for updates..." URL even though the canonical
/// page moved on. The caller therefore re-shows the CURRENT canonical page (read after the check):
/// this both corrects a stuck transient screen and is inherently clobber-safe — it re-asserts the
/// page another operation may have set during the check, never a stale pre-check snapshot. The
/// progress task is already drained by `finish()` before this runs, so no later transient
/// navigation can race the re-show. Other results already drove their own final screen → `NoOp`.
pub fn available_system_update_follow_up(
    result: &Result<UpdateCheckResult>,
) -> AvailableSystemUpdateFollowUp {
    match result {
        Ok(UpdateCheckResult::NoUpdateNeeded) => AvailableSystemUpdateFollowUp::RestoreCurrentPage,
        Err(_) => AvailableSystemUpdateFollowUp::LogFailure,
        Ok(_) => AvailableSystemUpdateFollowUp::NoOp,
    }
}

/// Canonical surface to re-show after a D-Bus `NoUpdateNeeded` check.
///
/// The version-check progress UI repaints Chromium with a transient "Checking for updates..."
/// screen without recording `app_state.page`, so the no-update follow-up must actively re-show the
/// real canonical surface or the TV stays stuck on that transient screen.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum RestorePageTarget {
    Webapp,
    Qrcode,
    Message(String),
    FactoryReset,
    /// Nothing to restore. `ReflashingRequired` cannot be rebuilt from the stored page state (the
    /// flashing-guide URL / QR content is not retained) and is unreachable on `NoUpdateNeeded`
    /// anyway, since a too-old device returns `TooOldToUpgrade`. `None` has no canonical surface.
    NoChange,
}

/// Decide which canonical page to re-show after a D-Bus no-update check.
///
/// `SystemUpgrade` is treated as a stale failure screen: once the manual retry cleared
/// `update_failed`, re-painting the failure copy would be wrong, so it maps to the phase-appropriate
/// surface (webapp when `Ready`, otherwise QR). Every other re-showable page maps back to itself so
/// Chromium leaves the transient progress screen.
pub fn restore_page_target(page: &Page, phase: SetupPhase) -> RestorePageTarget {
    match page {
        Page::SystemUpgrade(_) => match phase {
            SetupPhase::Ready => RestorePageTarget::Webapp,
            _ => RestorePageTarget::Qrcode,
        },
        Page::WebApp(_) => RestorePageTarget::Webapp,
        Page::QRCode(_) => RestorePageTarget::Qrcode,
        Page::Message(_, msg) => RestorePageTarget::Message(msg.clone()),
        Page::FactoryReset(_) => RestorePageTarget::FactoryReset,
        Page::ReflashingRequired(..) | Page::None(_) => RestorePageTarget::NoChange,
    }
}

/// Shared system update logic that checks version status and optionally triggers an update.
///
/// This function handles:
/// 1. Checking if the device is too old to auto-upgrade (shows reflashing QR code if so)
/// 2. Checking if an update is needed based on the provided `UpdateMode`
/// 3. Triggering the update process if needed
///
/// # Arguments
/// * `app_state` - Application state
/// * `chrome` - CDP connection for UI updates
/// * `mode` - Controls whether to check for required updates only or any available update
/// * `execution` - Controls whether operations block or run in background
/// * `guard` - The already-acquired device-ownership guard. Callers MUST acquire this at their
///   entry point (before any phase/UI/network side effects) so setup actions and OTA are serialized
///   end-to-end, not just at this function's boundary. This closes the TOCTOU window where a load-only
///   precheck could pass and a concurrent task then acquired ownership while the first path kept
///   mutating Wi-Fi/phase/UI. The guard is released by RAII when this function returns, except on a
///   non-blocking `UpdateStarted`, where ownership transfers into the background `update()` task and
///   is held until the device reboots.
///
/// # Returns
/// * `Ok(UpdateCheckResult)` indicating what action was taken
/// * `Err` if a critical error occurred during the check (only possible with `Blocking` execution)
pub async fn check_and_update_system(
    app_state: &Arc<AppState>,
    chrome: &Arc<Cdp>,
    mode: UpdateMode,
    execution: UpdateExecution,
    guard: UpdateGuard,
) -> Result<UpdateCheckResult> {
    // Note: Failure latch clearing is handled by explicit retry call sites (BLE/D-Bus),
    // not here. Startup flow needs to preserve persisted failure state so mobile app
    // can observe update_failed across restarts (issue #447).

    // Ownership was acquired by the caller before its side effects. Bind it here so RAII releases it
    // on every return path (and so the non-blocking branch can move it into the update task).
    let _guard = guard;

    // Preserve the current phase to restore if no update is needed.
    // Without this, durable phases (Ready, Pairing) would be lost during the
    // mandatory no-update check, demoting paired devices to QR setup.
    let phase_before_check = app_state.lifecycle.get();

    // Store durable phase as PRE_FAILURE_PHASE before entering transient states.
    // This ensures update_failed recovery can restore Ready/Pairing even though
    // the failure occurs while in Updating (transient).
    if phase_before_check.is_durable() && phase_before_check != SetupPhase::UpdateFailed {
        use crate::persistent_state::PRE_FAILURE_PHASE;
        app_state
            .state_store
            .set(PRE_FAILURE_PHASE, phase_before_check.as_str());
        if let Err(e) = app_state.state_store.save() {
            eprintln!("MAIN: Failed to persist pre-failure phase: {e:#?}");
        }
    }

    app_state.lifecycle.set(SetupPhase::CheckingVersion);

    let progress = VersionCheckProgress::start(execution, chrome.clone());

    // BLE/NonBlocking flows hold the mobile notification open until this fetch resolves
    // (`handle_connect_wifi` awaits the callback before replying), so cap them to a single
    // attempt (worst case ≈ one request timeout) instead of the full ~34s retry budget that
    // would violate the BLE response contract (docs/api-design.md "BLE response"). Blocking
    // startup/D-Bus flows can wait, so they keep the full budget for resilience.
    let retries = match execution {
        UpdateExecution::Blocking => updater::RefreshRetries::Full,
        UpdateExecution::NonBlocking => updater::RefreshRetries::Single,
    };

    // Force a fresh fetch first. This is a user-triggered/blocking check, so a failed live
    // refresh must surface as classified copy rather than silently falling back to stale
    // cached metadata (which could hide an outage or a newly raised minimum version).
    if let Err(ve) = updater::refresh_remote_version(retries, progress.as_updater_progress()).await
    {
        progress.finish().await;
        // Restore durable phase or reset to Idle on refresh failure (preserving paired state when
        // distributor/network/parse errors occur during forced metadata refresh), without demoting
        // a Ready that a pairing confirmation recorded while we were refreshing.
        app_state.lifecycle.set(phase_after_inflight_check(
            phase_before_check,
            app_state.lifecycle.get(),
        ));
        return show_version_check_failure(app_state, chrome, execution, ve).await;
    }
    // First check if device is too old to auto-upgrade (reads the freshly refreshed cache)
    match updater::is_too_old_to_upgrade(progress.as_updater_progress()).await {
        Ok(true) => {
            // Finish progress UI before reflashing screens; otherwise queued progress
            // navigations can overwrite the QR/message we are about to show.
            progress.finish().await;

            // Device is too old, show reflashing QR code
            if let Ok(Some(flashing_guide)) = updater::flashing_guide_url().await {
                let current_version = app_state.current_version.clone();
                let latest_version = updater::latest_version().await.unwrap_or_else(|e| {
                    eprintln!("MAIN: latest_version for reflashing banner: {e:#?}");
                    "Unknown".to_string()
                });
                let min_upgradeable = updater::min_upgradeable_version()
                    .await
                    .unwrap_or_else(|e| {
                        eprintln!("MAIN: min_upgradeable_version for reflashing banner: {e:#?}");
                        None
                    })
                    .unwrap_or_else(|| "Unknown".to_string());

                match execution {
                    UpdateExecution::Blocking => {
                        show_reflashing_qrcode(
                            app_state,
                            chrome,
                            &flashing_guide,
                            &current_version,
                            &latest_version,
                            &min_upgradeable,
                        )
                        .await?;
                    }
                    UpdateExecution::NonBlocking => {
                        task::spawn({
                            let app_state = app_state.clone();
                            let chrome = chrome.clone();
                            async move {
                                let _ = show_reflashing_qrcode(
                                    &app_state,
                                    &chrome,
                                    &flashing_guide,
                                    &current_version,
                                    &latest_version,
                                    &min_upgradeable,
                                )
                                .await;
                            }
                        });
                    }
                }
            } else {
                // Fallback to showing message without QR code if no flashing guide URL
                match execution {
                    UpdateExecution::Blocking => {
                        show_message(chrome, app_state, constant::REFLASHING_REQUIRED_MSG).await?;
                    }
                    UpdateExecution::NonBlocking => {
                        task::spawn({
                            let app_state = app_state.clone();
                            let chrome = chrome.clone();
                            async move {
                                let _ = show_message(
                                    &chrome,
                                    &app_state,
                                    constant::REFLASHING_REQUIRED_MSG,
                                )
                                .await;
                            }
                        });
                    }
                }
            }
            app_state.lifecycle.set(SetupPhase::Idle);
            return Ok(UpdateCheckResult::TooOldToUpgrade);
        }
        Ok(false) => {} // Device can be upgraded, continue
        Err(e) => {
            // Continue with the update check if this fails. This is safe because the forced
            // `refresh_remote_version` above already succeeded (otherwise we'd have early-returned),
            // so the cache is populated and this read is unlikely to fail; the subsequent
            // `is_update_required`/`is_update_available` reads the same cache and surfaces any
            // classified error there. The reflash gate is only skipped on a genuinely transient
            // post-refresh failure, recovered on the next check.
            eprintln!("MAIN: Error checking if device is too old: {e:#?}");
        }
    }

    // Check if update is needed based on mode
    let needs_update = match mode {
        UpdateMode::Required => updater::is_update_required(progress.as_updater_progress()).await,
        UpdateMode::Available => updater::is_update_available(progress.as_updater_progress()).await,
    };

    progress.finish().await;

    match needs_update {
        Ok(true) => {
            match execution {
                UpdateExecution::Blocking => {
                    // For blocking execution during startup, keep setupd alive on permanent
                    // failure so mobile polling can observe setup_phase=update_failed.
                    // The guard remains held during update and auto-releases when this
                    // branch completes.
                    if let Err(e) = update(app_state.clone(), chrome.clone()).await {
                        eprintln!("MAIN: Blocking update failed: {e:#?}");
                        // update() already set setup_phase=update_failed and showed
                        // error UI, then transitioned to QR. Return success so daemon
                        // stays alive and mobile can poll the failure state.
                    }
                }
                UpdateExecution::NonBlocking => {
                    // For non-blocking execution (e.g., BLE-triggered updates), spawn
                    // background task and transfer guard ownership into the task.
                    // The guard remains held from acquisition to task completion,
                    // preventing concurrent updates.
                    let app_state_clone = app_state.clone();
                    let chrome_clone = chrome.clone();
                    task::spawn(async move {
                        let _guard = _guard; // Transfer guard ownership into task
                        let _ = update(app_state_clone, chrome_clone).await;
                    });
                }
            }
            Ok(UpdateCheckResult::UpdateStarted)
        }
        Ok(false) => {
            if mode == UpdateMode::Available {
                println!("MAIN: System update requested but no update available");
            }
            // Restore the durable phase captured before the transient CheckingVersion (Ready/Pairing
            // preserved across the no-op check; non-durable collapses to Idle). phase_after_inflight_check
            // additionally refuses to demote a Ready that a pairing confirmation recorded while this
            // check was running, so the one-shot confirmation is not lost.
            app_state.lifecycle.set(phase_after_inflight_check(
                phase_before_check,
                app_state.lifecycle.get(),
            ));
            Ok(UpdateCheckResult::NoUpdateNeeded)
        }
        Err(ve) => {
            // Restore durable phase or reset to Idle on version-check failure (preserving paired
            // state when distributor/network errors occur), without demoting a Ready that a pairing
            // confirmation recorded while this check was running.
            app_state.lifecycle.set(phase_after_inflight_check(
                phase_before_check,
                app_state.lifecycle.get(),
            ));
            show_version_check_failure(app_state, chrome, execution, ve).await
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::atomic::Ordering;

    #[test]
    fn classify_updater_message_marks_network_failures_transient() {
        // Actual error from feral-updater.sh line 47
        assert_eq!(
            classify_updater_message("No network connection. Aborting update."),
            UpdateErrorType::Transient
        );
        // Actual error from feral-system-update.sh line 147
        assert_eq!(
            classify_updater_message("Failed to retrieve content length for image."),
            UpdateErrorType::Transient
        );
        // Download failures (fail-fast without retry in scripts)
        // Used by both system and recovery update scripts
        assert_eq!(
            classify_updater_message("Failed to download OTA image."),
            UpdateErrorType::Transient
        );
        assert_eq!(
            classify_updater_message("Failed to download signature file."),
            UpdateErrorType::Transient
        );
        assert_eq!(
            classify_updater_message("Failed to download recovery ISO."),
            UpdateErrorType::Transient
        );
    }

    #[test]
    fn classify_updater_message_marks_setupd_infrastructure_failures_transient() {
        // Errors from updater.rs
        assert_eq!(
            classify_updater_message("Failed to start updater service: systemd error"),
            UpdateErrorType::Transient
        );
        assert_eq!(
            classify_updater_message("Failed to open /var/log/updaterd.log after retries"),
            UpdateErrorType::Transient
        );
        assert_eq!(
            classify_updater_message("updater closed channel without sending progress"),
            UpdateErrorType::Transient
        );
    }

    #[test]
    fn classify_updater_message_marks_lock_failures_transient() {
        // Actual error from feral-updater.sh line 25
        // Lock failures are transient because lock may be released by the time we retry
        assert_eq!(
            classify_updater_message(
                "Exception: either Lock already held by another instance or some error happened."
            ),
            UpdateErrorType::Transient
        );
    }

    #[test]
    fn classify_updater_message_marks_signing_and_image_failures_permanent() {
        // Cryptographic failures (permanent - server issue)
        assert_eq!(
            classify_updater_message(
                "Error: Signature verification failed for /var/tmp/ota/image.iso."
            ),
            UpdateErrorType::Permanent
        );
        // Image structure failures (permanent - corrupt image)
        assert_eq!(
            classify_updater_message("airootfs.sfs not found in image."),
            UpdateErrorType::Permanent
        );
        // Filesystem operation failures (permanent - disk/permissions issue)
        assert_eq!(
            classify_updater_message(
                "Failed to create snapshot '/.snapshots/@ota_prev'. Aborting."
            ),
            UpdateErrorType::Permanent
        );
        // Generic unknown errors
        assert_eq!(
            classify_updater_message("Unknown error occurred"),
            UpdateErrorType::Permanent
        );
    }

    #[test]
    fn classify_updater_message_marks_unrecognized_as_permanent() {
        assert_eq!(
            classify_updater_message("something unexpected"),
            UpdateErrorType::Permanent
        );
    }

    #[test]
    fn classify_updater_message_classifies_exception_err_by_command() {
        // Network commands (curl, wget) are transient
        assert_eq!(
            classify_updater_message(
                "EXCEPTION ERR: LINE=118 CMD=\"curl -u \"$auth_user:$auth_pass\" --silent --show-error -fL \"$ENDPOINT$IMAGE_URL\" -o \"$ZIP_FILE\"\""
            ),
            UpdateErrorType::Transient
        );
        assert_eq!(
            classify_updater_message(
                "EXCEPTION ERR: LINE=47 CMD=\"curl -su \"$auth_user:$auth_pass\" -f \"$API_URL\"\""
            ),
            UpdateErrorType::Transient
        );
        assert_eq!(
            classify_updater_message(
                "EXCEPTION ERR: LINE=200 CMD=\"wget --timeout=10 https://example.com/file.tar.gz\""
            ),
            UpdateErrorType::Transient
        );

        // Non-network commands are permanent
        assert_eq!(
            classify_updater_message(
                "EXCEPTION ERR: LINE=99 CMD=\"mount -o loop /var/tmp/ota/image.iso /mnt/ota-iso\""
            ),
            UpdateErrorType::Permanent
        );
        assert_eq!(
            classify_updater_message(
                "EXCEPTION ERR: LINE=145 CMD=\"rsync -aAX --delete /mnt/ota-sfs/ /\""
            ),
            UpdateErrorType::Permanent
        );
        assert_eq!(
            classify_updater_message("EXCEPTION ERR: LINE=208 CMD=\"mkinitcpio -P\""),
            UpdateErrorType::Permanent
        );
    }

    #[test]
    fn update_in_progress_guard_prevents_concurrent_updates() {
        use crate::app_state::AppState;
        use crate::app_state::Page;
        use crate::connectivity::Connectivity;
        use crate::persistent_state::PersistentState;
        use crate::setup_lifecycle::SetupLifecycle;
        use std::sync::Arc;
        use std::sync::atomic::AtomicBool;
        use tokio::sync::Mutex;

        let temp_dir = tempfile::tempdir().unwrap();
        let state_file = temp_dir.path().join("state.txt");
        let state_store = PersistentState::new(state_file.to_str().unwrap()).unwrap();

        let app_state = Arc::new(AppState {
            device_id: "test".to_string(),
            branch: "test".to_string(),
            current_version: "1.0.0".to_string(),
            state_store,
            internet: tokio::runtime::Runtime::new()
                .unwrap()
                .block_on(async { Connectivity::spawn().await }),
            page: Mutex::new(Page::None(0)),
            auto_proceed: AtomicBool::new(false),
            lifecycle: SetupLifecycle::new(),
            update_in_progress: Arc::new(AtomicBool::new(false)),
        });

        // First acquisition should succeed
        let guard1 = UpdateGuard::try_acquire(&app_state.update_in_progress);
        assert!(guard1.is_some());

        // Second acquisition should fail while first is held
        let guard2 = UpdateGuard::try_acquire(&app_state.update_in_progress);
        assert!(guard2.is_none());

        // Drop first guard and reacquire should succeed
        drop(guard1);
        let guard3 = UpdateGuard::try_acquire(&app_state.update_in_progress);
        assert!(guard3.is_some());
    }

    #[test]
    fn update_guard_releases_flag_when_scope_ends() {
        let flag = Arc::new(AtomicBool::new(false));

        {
            let _guard = UpdateGuard::try_acquire(&flag).expect("guard acquired");
            assert!(flag.load(Ordering::Acquire));
        }

        assert!(!flag.load(Ordering::Acquire));
        assert!(UpdateGuard::try_acquire(&flag).is_some());
    }

    /// Regression for PR #206 review 4483457995: setup actions and OTA must be serialized at the
    /// operation level via the single ownership token, not a load-only precheck. This models the
    /// race shape directly: once one path holds the guard (an OTA owns the device), every other
    /// entry point (connect_wifi / keep_wifi / do_system_update / qr_switch / startup) fails to
    /// acquire and must bail BEFORE any phase/UI/Wi-Fi side effects. Acquiring at entry (rather than
    /// loading then acquiring later inside check_and_update_system) is what closes the TOCTOU window.
    #[test]
    fn device_ownership_is_exclusive_across_entry_points() {
        let flag = Arc::new(AtomicBool::new(false));

        // An OTA owns the device.
        let ota_guard = UpdateGuard::try_acquire(&flag).expect("first acquire succeeds");

        // Every other entry point's entry-time acquire is rejected, so they bail before side effects.
        assert!(
            UpdateGuard::try_acquire(&flag).is_none(),
            "a second entry point must not acquire ownership while an OTA holds it",
        );

        // Once the OTA releases (completed/failed update), ownership is available again.
        drop(ota_guard);
        assert!(
            UpdateGuard::try_acquire(&flag).is_some(),
            "ownership must be reacquirable after the holder drops it",
        );
    }

    /// The gate must track the real UpdateGuard lifecycle: blocked while a guard is held, allowed
    /// again once it drops (so a finished/failed update re-opens setup actions).
    #[test]
    fn ble_action_gate_follows_update_guard_lifecycle() {
        let flag = Arc::new(AtomicBool::new(false));
        assert!(UpdateGuard::try_acquire(&flag).is_some());

        {
            let _guard = UpdateGuard::try_acquire(&flag).expect("guard acquired");
            assert!(
                UpdateGuard::try_acquire(&flag).is_none(),
                "ownership is exclusive while a guard is held",
            );
        }

        assert!(
            UpdateGuard::try_acquire(&flag).is_some(),
            "ownership is available again after the guard drops",
        );
    }

    #[test]
    fn update_attempt_error_classifies_from_raw_updater_message_before_context_wrap() {
        let raw = "No network connection. Aborting update.";
        let source = anyhow::anyhow!(raw);
        let kind = classify_updater_message(&source.to_string());
        let err = UpdateAttemptError {
            source: source.context("update process failed"),
            kind,
        };

        assert_eq!(err.kind, UpdateErrorType::Transient);
        assert!(err.source.to_string().contains("update process failed"));
        assert_eq!(
            classify_updater_message(&err.source.to_string()),
            UpdateErrorType::Permanent
        );
    }

    #[test]
    fn tv_progress_only_for_blocking_execution() {
        assert!(VersionCheckProgress::uses_tv_progress(
            UpdateExecution::Blocking
        ));
        assert!(!VersionCheckProgress::uses_tv_progress(
            UpdateExecution::NonBlocking
        ));
    }

    /// Mirrors `check_and_update_system`: ephemeral `Sender` clones must not outlive `finish`,
    /// or the progress receiver task never sees the channel close.
    #[tokio::test]
    async fn progress_receiver_exits_after_all_senders_dropped() {
        use std::time::Duration;
        use tokio::sync::mpsc;
        use tokio::time::timeout;

        let (tx, mut rx) = mpsc::channel::<(u32, u32)>(16);
        let per_updater_call = tx.clone();
        drop(per_updater_call);

        let receiver_task = tokio::spawn(async move { while rx.recv().await.is_some() {} });

        drop(tx);
        timeout(Duration::from_secs(1), receiver_task)
            .await
            .expect("receiver should finish within 1s")
            .expect("receiver task join");
    }

    /// No update → re-show the current canonical page. The caller reads the CURRENT page (after the
    /// check) and re-navigates to it, which both corrects a stuck transient progress screen and
    /// re-asserts any page another operation set during the check (never clobbering it with a stale
    /// snapshot). This is why the decision needs no prior/current comparison here.
    #[test]
    fn no_update_needed_restores_current_page() {
        assert_eq!(
            available_system_update_follow_up(&Ok(UpdateCheckResult::NoUpdateNeeded)),
            AvailableSystemUpdateFollowUp::RestoreCurrentPage,
        );
    }

    #[test]
    fn update_started_is_no_op_for_dbus_follow_up() {
        assert_eq!(
            available_system_update_follow_up(&Ok(UpdateCheckResult::UpdateStarted)),
            AvailableSystemUpdateFollowUp::NoOp,
        );
    }

    #[test]
    fn too_old_to_upgrade_is_no_op_for_dbus_follow_up() {
        assert_eq!(
            available_system_update_follow_up(&Ok(UpdateCheckResult::TooOldToUpgrade)),
            AvailableSystemUpdateFollowUp::NoOp,
        );
    }

    #[test]
    fn version_check_failed_is_no_op_for_dbus_follow_up() {
        assert_eq!(
            available_system_update_follow_up(&Ok(UpdateCheckResult::VersionCheckFailed)),
            AvailableSystemUpdateFollowUp::NoOp,
        );
    }

    #[test]
    fn orchestration_error_is_logged_only() {
        assert_eq!(
            available_system_update_follow_up(&Err(anyhow::anyhow!("cdp navigate failed"))),
            AvailableSystemUpdateFollowUp::LogFailure,
        );
    }

    /// Regression for PR #206 review 4482497555: a failed mandatory startup version check must NOT
    /// be fatal. on_startup_with_internet previously turned VersionCheckFailed into an Err that
    /// propagated to main() and exited the process; with Restart=always + StartLimitBurst=5 a
    /// transient distributor/network outage would crash-loop setupd until systemd gave up, killing
    /// BLE advertising and the mobile retry surface. It must Halt (stay alive) instead.
    #[test]
    fn version_check_failed_halts_without_killing_daemon() {
        assert_eq!(
            startup_update_check_outcome(UpdateCheckResult::VersionCheckFailed),
            StartupUpdateOutcome::Halt,
        );
    }

    /// Too-old, update-started, and already-in-progress all left the daemon alive (early Ok) before
    /// the refactor; lock that in so the helper keeps matching the historic control flow.
    #[test]
    fn updater_owned_results_halt_startup() {
        for result in [
            UpdateCheckResult::TooOldToUpgrade,
            UpdateCheckResult::UpdateStarted,
            UpdateCheckResult::UpdateInProgress,
        ] {
            assert_eq!(
                startup_update_check_outcome(result),
                StartupUpdateOutcome::Halt,
                "result {result:?} must halt startup",
            );
        }
    }

    /// Only NoUpdateNeeded continues into topic_id fetch / surface selection.
    #[test]
    fn no_update_needed_continues_startup() {
        assert_eq!(
            startup_update_check_outcome(UpdateCheckResult::NoUpdateNeeded),
            StartupUpdateOutcome::Continue,
        );
    }
}
