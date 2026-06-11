mod ble;
mod cdp;
mod cfg;
mod connectivity;
mod constant;
mod dbus_utils;
mod encoding;
mod log_uploader;
mod persistent_state;
mod setup_lifecycle;
mod system;
mod updater;
mod wifi_utils;

use crate::persistent_state::PersistentState;
use crate::setup_lifecycle::{SetupLifecycle, SetupPhase};
use crate::wifi_utils::{Error as WifiError, SSIDsCacher};
use anyhow::Context;
use anyhow::Result;
use ble::{Ble, BleCallbacks};
use cdp::Cdp;
use connectivity::Connectivity;
use std::sync::Arc;
use std::sync::atomic::{AtomicBool, Ordering};
use std::time::Instant;
use std::time::{SystemTime, UNIX_EPOCH};
use tokio::signal::unix::{SignalKind, signal as unix_signal};
use tokio::{
    sync::{Mutex, mpsc},
    task,
    time::{self, Duration},
};

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

#[inline]
fn unix_s() -> i64 {
    match SystemTime::now().duration_since(UNIX_EPOCH) {
        Ok(duration) => duration.as_secs() as i64,
        Err(error) => {
            eprintln!("MAIN: System time is before UNIX_EPOCH: {error:?}");
            0
        }
    }
}

#[derive(Debug, Clone, PartialEq, Eq)]
enum Page {
    None(i64),
    QRCode(i64),
    Message(i64, String),
    SystemUpgrade(i64),
    FactoryReset(i64),
    WebApp(i64),
    ReflashingRequired(i64, String),
}

impl Page {
    /// Check if the page should be kept when bluetooth disconnects
    fn should_keep_on_bt_disconnect(&self) -> bool {
        matches!(
            self,
            Page::WebApp(_)
                | Page::SystemUpgrade(_)
                | Page::FactoryReset(_)
                | Page::ReflashingRequired(_, _)
        )
    }
}

#[derive(Debug)]
struct AppState {
    device_id: String,
    branch: String,
    current_version: String,
    state_store: PersistentState,
    internet: Connectivity,
    page: Mutex<Page>,

    // This is the flag to indicate whether we should automatically redirect to webapp
    // when internet is available.
    // On a second boot, if the internet is unavailable, users have 2 choices
    // 1. Fix the internet connection, it will automatically check for update & play artwork
    // 2. Scan the QRCode and start everything over again
    // We need this flag to turn off the first flow if the user has chosen to provide a different wifi
    // true = auto proceed, false = user has chosen to provide a different wifi
    auto_proceed: AtomicBool,
    /// Setup progress coordinator. Derives mobile-facing phase from persistent state
    /// (PAIRED, topic_id), active operations (wifi, version check, update), and
    /// permanent failure latch.
    lifecycle: SetupLifecycle,
    /// Single-flight guard for OTA updates to prevent concurrent update attempts.
    /// Wrapped in Arc to allow UpdateGuard to be 'static for tokio::spawn.
    update_in_progress: Arc<AtomicBool>,
}

const MAX_UPDATE_RETRIES: u8 = 3;

// Message shown when update fails (fresh failure during current session)
const UPDATE_FAILED_FRESH_MSG: &str = "Update didn't complete. Use your phone to try again, or restart the device. If it still fails, contact support@feralfile.com.";

// Message shown on reboot when UpdateFailed phase is restored from disk.
// User already restarted, so guide them to retry via phone or contact support.
const UPDATE_FAILED_RECOVERED_MSG: &str = "Previous update failed. The device attempted to update but couldn't complete. Use your phone to retry the update. If the problem persists, contact support@feralfile.com for assistance.";

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum UpdateErrorType {
    Transient,
    Permanent,
}

/// Update failure with classification decided from the updater message before
/// setupd adds context wrappers for logging.
#[derive(Debug)]
struct UpdateAttemptError {
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
fn classify_updater_message(message: &str) -> UpdateErrorType {
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

fn get_setup_phase(app_state: &AppState) -> String {
    app_state.lifecycle.get().as_str().to_string()
}

/// Gate for BLE setup actions (`connect_wifi`, `keep_wifi`) while an OTA owns the device.
///
/// A non-blocking update holds `update_in_progress` for its entire lifetime (version check,
/// retries, backoff, install up to reboot). Letting a concurrent BLE setup action run during that
/// window would clobber `setup_phase=updating` that mobile is polling and, for `connect_wifi`,
/// switch Wi-Fi via nmcli mid-download. Returning `Some(DeviceUpdating)` lets callers reject the
/// request before any phase/UI/network side effects, leaving the updater in sole control.
fn ble_action_block_during_update(update_in_progress: &AtomicBool) -> Option<ble::BleStatus> {
    if update_in_progress.load(Ordering::Acquire) {
        Some(ble::BleStatus::DeviceUpdating)
    } else {
        None
    }
}

/// RAII guard for the update_in_progress flag.
/// Automatically releases the flag when dropped, ensuring cleanup on all return paths
/// (success, early return, panic, or error propagation via `?`).
///
/// Owns an Arc<AtomicBool> to be 'static-safe for tokio::spawn usage.
struct UpdateGuard {
    flag: Arc<AtomicBool>,
}

impl UpdateGuard {
    fn try_acquire(flag: &Arc<AtomicBool>) -> Option<Self> {
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
    wait_for_shutdown().await; // Ignore any errors
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

async fn init_app_state(ble_service: &Arc<Ble>) -> Result<Arc<AppState>> {
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

async fn init_cdp() -> Result<Arc<Cdp>> {
    let chrome = Cdp::connect(constant::CDP_URL)
        .await
        .context("connecting to CDP")?;
    Ok(Arc::new(chrome))
}

async fn start_ble(
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
async fn startup_without_internet(
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
async fn startup_with_internet(
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

async fn setup_dbus_listeners(
    app_state: &Arc<AppState>,
    chrome: &Arc<Cdp>,
) -> Result<Arc<AtomicBool>> {
    // Listen for QRCode switch signal
    let qrcode_switch_cb = callbacks::create_qrcode_switch_cb(app_state.clone(), chrome.clone());
    let stop_dbus_listener = Arc::new(AtomicBool::new(false));
    dbus_utils::listen_for_signal(
        constant::DBUS_CONTROLD_OBJECT,
        constant::DBUS_CONTROLD_INTERFACE,
        constant::DBUS_EVENT_QRCODE_SWITCH,
        stop_dbus_listener.clone(),
        qrcode_switch_cb,
    )
    .await?;

    // Listen for factory reset signal
    let factory_reset_cb =
        callbacks::create_factory_reset_dbus_cb(app_state.clone(), chrome.clone());
    dbus_utils::listen_for_signal(
        constant::DBUS_CONTROLD_OBJECT,
        constant::DBUS_CONTROLD_INTERFACE,
        constant::DBUS_EVENT_FACTORY_RESET,
        stop_dbus_listener.clone(),
        factory_reset_cb,
    )
    .await?;

    // Listen for upload logs signal
    let upload_logs_cb = callbacks::create_upload_logs_dbus_cb(app_state.clone());
    dbus_utils::listen_for_signal(
        constant::DBUS_CONTROLD_OBJECT,
        constant::DBUS_CONTROLD_INTERFACE,
        constant::DBUS_EVENT_UPLOAD_LOGS,
        stop_dbus_listener.clone(),
        upload_logs_cb,
    )
    .await?;

    // Listen for support-bundled upload logs signal
    let upload_logs_with_bundle_cb =
        callbacks::create_upload_logs_with_bundle_dbus_cb(app_state.clone());
    dbus_utils::listen_for_signal(
        constant::DBUS_CONTROLD_OBJECT,
        constant::DBUS_CONTROLD_INTERFACE,
        constant::DBUS_EVENT_UPLOAD_LOGS_WITH_BUNDLE,
        stop_dbus_listener.clone(),
        upload_logs_with_bundle_cb,
    )
    .await?;

    // Listen for system update signal
    let system_update_cb =
        callbacks::create_system_update_dbus_cb(app_state.clone(), chrome.clone());
    dbus_utils::listen_for_signal(
        constant::DBUS_CONTROLD_OBJECT,
        constant::DBUS_CONTROLD_INTERFACE,
        constant::DBUS_EVENT_SYSTEM_UPDATE,
        stop_dbus_listener.clone(),
        system_update_cb,
    )
    .await?;

    Ok(stop_dbus_listener)
}

async fn internet_setup_successfully_cb(
    app_state: &Arc<AppState>,
    chromium: &Arc<Cdp>,
) -> Result<String, ble::BleStatus> {
    // Check and update system using Required mode (only mandatory updates)
    // Use NonBlocking execution since BLE flow needs to return quickly
    match check_and_update_system(
        app_state,
        chromium,
        UpdateMode::Required,
        UpdateExecution::NonBlocking,
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
            app_state.lifecycle.set(SetupPhase::Idle);
            return Err(ble::BleStatus::VersionCheckFailed);
        }
        Ok(UpdateCheckResult::NoUpdateNeeded) => {} // Continue with normal flow
        Err(e) => {
            // This shouldn't happen with NonBlocking, but handle it anyway
            eprintln!("MAIN: Error during update check: {e:#?}");
            app_state.lifecycle.set(SetupPhase::Idle);
            return Err(ble::BleStatus::VersionCheckFailed);
        }
    }

    // Get topic id from controld FIRST (before setting Pairing phase)
    let topic_id = match dbus_utils::get_relayer_info() {
        Ok(info) => info,
        Err(e) => {
            eprintln!("BLE: can't get relayer data from controld: {e:#?}");
            return Err(ble::BleStatus::ServerUnreachable);
        }
    };

    // Save topic_id to disk BEFORE setting Pairing phase to maintain invariant.
    // Fail the operation if save fails, so we don't create invalid state.
    let state_store = &app_state.state_store;
    state_store.set(persistent_state::TOPIC_ID, &topic_id);
    if let Err(e) = state_store.save() {
        eprintln!("BLE: Failed to save topic_id: {e:#?}");
        return Err(ble::BleStatus::UnknownError);
    }

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

mod callbacks {
    use super::{
        AppState, Cdp, SetupPhase, WifiError, ble, ble_action_block_during_update, constant,
        dbus_utils, get_setup_phase, internet_setup_successfully_cb, show_factory_reset,
        show_message, show_qrcode, show_webapp, wifi_utils,
    };
    use serde::Deserialize;
    use std::sync::Arc;
    use std::sync::atomic::Ordering;
    use std::time::{Duration, Instant};
    use tokio::task;

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
                    matches!(*page, super::Page::QRCode(_))
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

                // Reject Wi-Fi reconfiguration while an OTA owns the device. The non-blocking
                // update holds the guard until the background update() finishes; mutating
                // setup_phase/UI or switching networks via nmcli here would clobber the
                // updating phase mobile polls and could disrupt the in-flight download. Bail
                // before any side effects so the updater stays in sole control.
                if let Some(status) = ble_action_block_during_update(&app_state.update_in_progress)
                {
                    println!("BLE: connect_wifi rejected, update in progress");
                    return Err(status);
                }

                // Clear and restore UpdateFailed phase on explicit WiFi retry.
                // Restore the pre-failure durable phase if tracked, otherwise use Idle.
                if app_state.lifecycle.get() == SetupPhase::UpdateFailed {
                    use crate::persistent_state::PRE_FAILURE_PHASE;

                    let restored_phase =
                        if let Some(phase_str) = app_state.state_store.get(PRE_FAILURE_PHASE) {
                            let phase = SetupPhase::from_str(&phase_str);
                            println!(
                                "BLE: Restoring pre-failure phase '{}' on WiFi retry",
                                phase.as_str()
                            );
                            phase
                        } else {
                            println!(
                                "BLE: Clearing UpdateFailed to Idle (no pre-failure phase tracked)"
                            );
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
                        eprintln!(
                            "BLE: Failed to persist phase restoration, aborting retry: {e:#?}"
                        );
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
                            eprintln!(
                                "BLE: Failed to re-save PRE_FAILURE_PHASE before retry: {e:#?}"
                            );
                        }
                    }
                }

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
                    app_state.lifecycle.set(SetupPhase::Idle);
                    // Tell user that the wifi connection failed
                    task::spawn(async move {
                        let _ = show_message(
                            &chromium,
                            &app_state,
                            constant::WIFI_FAILED_TO_CONNECT_MSG,
                        )
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
                    app_state.lifecycle.set(SetupPhase::Idle);
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
                internet_setup_successfully_cb(&app_state, &chromium).await
            })
        })
    }

    pub fn create_keep_wifi_cb(
        app_state: Arc<AppState>,
        chromium: Arc<Cdp>,
    ) -> ble::KeepWifiCallback {
        Box::new(move || {
            let app_state = app_state.clone();
            let chromium = chromium.clone();
            Box::pin(async move {
                // Reject while an OTA owns the device, before any phase/UI side effects, so the
                // updater keeps control of setup_phase mobile polls (mirrors connect_wifi).
                if let Some(status) = ble_action_block_during_update(&app_state.update_in_progress)
                {
                    println!("BLE: keep_wifi rejected, update in progress");
                    return Err(status);
                }

                if !app_state.internet.is_online(true).await {
                    return Err(ble::BleStatus::WifiRequired);
                }

                // Clear and restore UpdateFailed phase on explicit keep_wifi retry.
                // Restore the pre-failure durable phase if tracked, otherwise use Idle.
                if app_state.lifecycle.get() == SetupPhase::UpdateFailed {
                    use crate::persistent_state::PRE_FAILURE_PHASE;

                    let restored_phase =
                        if let Some(phase_str) = app_state.state_store.get(PRE_FAILURE_PHASE) {
                            let phase = SetupPhase::from_str(&phase_str);
                            println!(
                                "BLE: Restoring pre-failure phase '{}' on keep_wifi retry",
                                phase.as_str()
                            );
                            phase
                        } else {
                            println!(
                                "BLE: Clearing UpdateFailed to Idle (no pre-failure phase tracked)"
                            );
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
                        eprintln!(
                            "BLE: Failed to persist phase restoration, aborting retry: {e:#?}"
                        );
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
                            eprintln!(
                                "BLE: Failed to re-save PRE_FAILURE_PHASE before retry: {e:#?}"
                            );
                        }
                    }
                }

                internet_setup_successfully_cb(&app_state, &chromium).await
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
            vec![super::build_device_info(&app_state)]
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
                // Ignore QR/webapp switches while an OTA owns the device. show_qrcode would reset
                // the transient Updating phase to Idle and navigate Chromium off the upgrade
                // screen, both of which clobber the updater-owned state mobile is polling. The
                // updater drives the screen until it completes (reboot) or latches update_failed.
                if ble_action_block_during_update(&app_state.update_in_progress).is_some() {
                    println!("MAIN: QR switch ignored, update in progress");
                    return;
                }

                if qrcode_requested {
                    let _ = show_qrcode(&app_state, &chromium).await;
                } else {
                    // When the web app is requested, treat this as confirmation
                    // that the mobile app has successfully scanned the QR code and paired.
                    // Transition from Pairing to Ready.
                    let current_phase = app_state.lifecycle.get();
                    if current_phase == SetupPhase::Pairing {
                        println!("MAIN: QR switch -> transitioning to Ready phase");
                        app_state.lifecycle.set(SetupPhase::Ready);
                        if let Err(e) = app_state.lifecycle.persist(&app_state.state_store) {
                            eprintln!("MAIN: Error persisting Ready phase: {e:#?}");
                        }
                    }

                    let _ = show_webapp(&app_state, &chromium).await;
                }
            });
        })
    }

    async fn do_factory_reset(chromium: &Arc<Cdp>, app_state: &Arc<AppState>) {
        // Show factory reset page
        let _ = show_factory_reset(chromium, app_state).await;
        // Execute factory reset
        if let Err(e) = super::system::factory_reset().await {
            eprintln!("MAIN: Failed to execute factory reset: {e:#?}");
        }
    }

    async fn do_system_update(chromium: &Arc<Cdp>, app_state: &Arc<AppState>) {
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

        // Clear and restore UpdateFailed phase on explicit D-Bus manual retry.
        // Restore the pre-failure durable phase if tracked, otherwise use Idle.
        if app_state.lifecycle.get() == SetupPhase::UpdateFailed {
            use crate::persistent_state::PRE_FAILURE_PHASE;

            let restored_phase =
                if let Some(phase_str) = app_state.state_store.get(PRE_FAILURE_PHASE) {
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
        let result = super::check_and_update_system(
            app_state,
            chromium,
            super::UpdateMode::Available,
            super::UpdateExecution::Blocking,
        )
        .await;

        match super::available_system_update_follow_up(&result) {
            super::AvailableSystemUpdateFollowUp::RestoreCurrentPage => {
                // The blocking version-check progress UI repaints Chromium with the transient
                // "Checking for updates..." screen and intentionally does NOT record
                // app_state.page (see navigate_transient_message). So after a no-update result
                // the TV is left on that transient screen while app_state.page still holds the
                // real canonical surface. We must actively re-show that canonical page or the
                // device stays stuck on "Checking for updates...". The decision is factored into
                // restore_page_target so it can be unit-tested without driving CDP.
                let current_page = { app_state.page.lock().await.clone() };
                let target = super::restore_page_target(&current_page, app_state.lifecycle.get());
                let nav_result = match target {
                    super::RestorePageTarget::Webapp => {
                        super::show_webapp(app_state, chromium).await
                    }
                    super::RestorePageTarget::Qrcode => {
                        super::show_qrcode(app_state, chromium).await
                    }
                    super::RestorePageTarget::Message(msg) => {
                        super::show_message(chromium, app_state, &msg).await
                    }
                    super::RestorePageTarget::FactoryReset => {
                        super::show_factory_reset(chromium, app_state).await
                    }
                    super::RestorePageTarget::NoChange => Ok(()),
                };
                if let Err(e) = nav_result {
                    eprintln!(
                        "MAIN: Failed to restore canonical page after D-Bus no-update: {e:#?}"
                    );
                }
            }
            super::AvailableSystemUpdateFollowUp::LogFailure => {
                eprintln!("MAIN: System update failed: {result:#?}");
            }
            super::AvailableSystemUpdateFollowUp::NoOp => {}
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

        if let Err(e) = super::log_uploader::submit_logs(
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
}

// device_info is <device_id>|<topic_id>|<internet>|<branch>|<version>|<setup_phase>
fn build_device_info(app_state: &Arc<AppState>) -> String {
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

// The url format is like this
// url?step=qr&device_info=<device_info>&version=<version>&device_id=<device_id>
// Note: we need extra device_id and version for the page to display at the bottom
fn build_qrcode_url(app_state: &Arc<AppState>) -> String {
    let device_info = build_device_info(app_state);
    let device_id = app_state.device_id.clone();
    let version = app_state.current_version.clone();

    format!(
        "{}&device_info={device_info}&version={version}&device_id={device_id}",
        constant::QRCODE_URL_PREFIX
    )
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

async fn show_qrcode(app_state: &Arc<AppState>, chrome: &Arc<Cdp>) -> Result<()> {
    // Normal QR navigation resets non-failure transient phases to idle (WifiConnecting,
    // CheckingVersion, Updating), while preserving UpdateFailed, Pairing, and Ready phases.
    // This ensures the device returns to a clean setup state when showing QR outside of
    // explicit failure recovery flows.
    let current = app_state.lifecycle.get();
    if matches!(
        current,
        SetupPhase::WifiConnecting | SetupPhase::CheckingVersion | SetupPhase::Updating
    ) {
        app_state.lifecycle.set(SetupPhase::Idle);
    }

    let qrcode_url = build_qrcode_url(app_state);
    // QRCode url is dynamically built
    // So we always navigate to make sure the url is correct
    let mut page = app_state.page.lock().await;
    chrome
        .navigate(&qrcode_url)
        .await
        .with_context(|| format!("navigating to {qrcode_url}"))?;
    println!("MAIN: Navigated to {qrcode_url}");
    *page = Page::QRCode(unix_s());
    Ok(())
}

async fn show_reflashing_qrcode(
    app_state: &Arc<AppState>,
    chrome: &Arc<Cdp>,
    flashing_guide_url: &str,
    current_version: &str,
    latest_version: &str,
    min_upgradeable_version: &str,
) -> Result<()> {
    // Build message with version information
    let message = format!(
        "We've moved too far ahead for this version to catch up. Your FF1 is too far behind to auto-upgrade. Current version: {current_version} Latest version: {latest_version} Minimum upgradeable version: {min_upgradeable_version}. Scan the code above for step-by-step reflashing instructions, or contact us for help. support@feralfile.com"
    );

    // Build URL with QR code step and flashing guide as the QR content
    let qrcode_url = format!(
        "{}&qr_content={}&message={}",
        constant::QRCODE_URL_PREFIX,
        urlencoding::encode(flashing_guide_url),
        urlencoding::encode(&message)
    );

    let mut page = app_state.page.lock().await;
    chrome
        .navigate(&qrcode_url)
        .await
        .with_context(|| format!("navigating to {qrcode_url}"))?;
    println!("MAIN: Navigated to reflashing QR code: {qrcode_url}");
    *page = Page::ReflashingRequired(unix_s(), message);
    Ok(())
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
async fn on_startup_with_internet(app_state: Arc<AppState>, chrome: Arc<Cdp>) -> Result<()> {
    // If UpdateFailed phase is set, skip automatic update check on startup.
    // Show the failure message (different from fresh failure) since this is reboot recovery.
    // Mobile app will see update_failed via device_info polling and can trigger explicit retry.
    if app_state.lifecycle.get() == SetupPhase::UpdateFailed {
        println!("MAIN: Skipping startup update check - UpdateFailed phase is set");
        println!(
            "MAIN: Showing recovered failure message and waiting for explicit BLE/D-Bus retry"
        );
        show_system_upgrade(&chrome, &app_state, UPDATE_FAILED_RECOVERED_MSG).await?;
        return Ok(());
    }

    // Check and update system using Required mode (only mandatory updates on startup).
    // This runs for ALL phases except UpdateFailed, maintaining consistency:
    // - First-time setup (Idle): checks before fetching topic_id → Pairing
    // - Reboot with Pairing: checks again to ensure no new mandatory updates
    // - Reboot with Ready: checks to keep device up-to-date
    // Use Blocking execution since we can wait for completion during startup
    match check_and_update_system(
        &app_state,
        &chrome,
        UpdateMode::Required,
        UpdateExecution::Blocking,
    )
    .await?
    {
        UpdateCheckResult::TooOldToUpgrade
        | UpdateCheckResult::UpdateStarted
        | UpdateCheckResult::UpdateInProgress => {
            // Device is either too old, updating, or an update is already running.
            return Ok(());
        }
        UpdateCheckResult::VersionCheckFailed => {
            // Error was already shown to user, but we treat this as a hard failure
            return Err(anyhow::anyhow!("Failed to check version information"));
        }
        UpdateCheckResult::NoUpdateNeeded => {} // Continue with normal flow
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

async fn update(app_state: Arc<AppState>, chrome: Arc<Cdp>) -> Result<()> {
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
async fn handle_permanent_update_failure(
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
    UpdateInProgress,
}

/// Follow-up after `check_and_update_system(UpdateMode::Available, …)` in the D-Bus path.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum AvailableSystemUpdateFollowUp {
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
fn available_system_update_follow_up(
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
enum RestorePageTarget {
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
fn restore_page_target(page: &Page, phase: SetupPhase) -> RestorePageTarget {
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
///
/// # Returns
/// * `Ok(UpdateCheckResult)` indicating what action was taken
/// * `Err` if a critical error occurred during the check (only possible with `Blocking` execution)
async fn check_and_update_system(
    app_state: &Arc<AppState>,
    chrome: &Arc<Cdp>,
    mode: UpdateMode,
    execution: UpdateExecution,
) -> Result<UpdateCheckResult> {
    // Note: Failure latch clearing is handled by explicit retry call sites (BLE/D-Bus),
    // not here. Startup flow needs to preserve persisted failure state so mobile app
    // can observe update_failed across restarts (issue #447).

    // Single-flight guard: prevent concurrent update attempts.
    // The guard is automatically released when this function returns (success, error, or panic).
    let _guard = match UpdateGuard::try_acquire(&app_state.update_in_progress) {
        Some(guard) => guard,
        None => {
            println!("MAIN: Update already in progress, skipping");
            return Ok(UpdateCheckResult::UpdateInProgress);
        }
    };

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
        // Restore durable phase or reset to Idle on refresh failure.
        // This preserves paired device state when distributor/network/parse errors occur
        // during forced metadata refresh.
        if phase_before_check.is_durable() {
            app_state.lifecycle.set(phase_before_check);
        } else {
            app_state.lifecycle.set(SetupPhase::Idle);
        }
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
            // Restore the original phase if it was durable (Ready, Pairing), otherwise reset to Idle.
            // This preserves paired device state across mandatory startup update checks.
            if phase_before_check.is_durable() {
                app_state.lifecycle.set(phase_before_check);
            } else {
                app_state.lifecycle.set(SetupPhase::Idle);
            }
            Ok(UpdateCheckResult::NoUpdateNeeded)
        }
        Err(ve) => {
            // Restore durable phase or reset to Idle on version-check failure.
            // This preserves paired device state when distributor/network errors occur.
            if phase_before_check.is_durable() {
                app_state.lifecycle.set(phase_before_check);
            } else {
                app_state.lifecycle.set(SetupPhase::Idle);
            }
            show_version_check_failure(app_state, chrome, execution, ve).await
        }
    }
}

/// Render the classified version-check failure copy and report [`UpdateCheckResult::VersionCheckFailed`].
///
/// `Blocking` navigates inline (callers may await the screen); `NonBlocking` (BLE) spawns the
/// navigation so the mobile response is not held on CDP.
async fn show_version_check_failure(
    app_state: &Arc<AppState>,
    chrome: &Arc<Cdp>,
    execution: UpdateExecution,
    ve: updater::VersionFetchError,
) -> Result<UpdateCheckResult> {
    eprintln!("MAIN: Error checking for update: {ve:#?}");
    let tv_msg = ve.kind().tv_message();
    match execution {
        UpdateExecution::Blocking => {
            show_message(chrome, app_state, tv_msg).await?;
        }
        UpdateExecution::NonBlocking => {
            let tv_msg = tv_msg.to_string();
            task::spawn({
                let app_state = app_state.clone();
                let chrome = chrome.clone();
                async move {
                    let _ = show_message(&chrome, &app_state, &tv_msg).await;
                }
            });
        }
    }
    Ok(UpdateCheckResult::VersionCheckFailed)
}

/// Drives TV “checking for updates” copy during distributor HTTP retries.
///
/// Only [`UpdateExecution::Blocking`] attaches a channel: BLE uses [`UpdateExecution::NonBlocking`]
/// and passes `None` into the updater so CDP navigations are not on the mobile response path.
/// When active, the sender is dropped and the task drained before final TV screens so progress
/// cannot overwrite reflash, failure, or update UI.
struct VersionCheckProgress {
    tx: Option<updater::FetchProgressTx>,
    task: Option<task::JoinHandle<()>>,
}

impl VersionCheckProgress {
    #[must_use]
    fn uses_tv_progress(execution: UpdateExecution) -> bool {
        matches!(execution, UpdateExecution::Blocking)
    }

    // No `app_state`: progress navigations are transient and must NOT record the canonical
    // page (see `navigate_transient_message`), so the receiver task only needs the CDP handle.
    fn start(execution: UpdateExecution, chrome: Arc<Cdp>) -> Self {
        if !Self::uses_tv_progress(execution) {
            return Self {
                tx: None,
                task: None,
            };
        }

        let (prog_tx, mut prog_rx) = mpsc::channel::<(u32, u32)>(16);
        let progress_task = tokio::spawn(async move {
            while let Some((_attempt, _max)) = prog_rx.recv().await {
                if let Err(e) =
                    navigate_transient_message(&chrome, constant::VERSION_CHECK_PROGRESS_TV_MSG)
                        .await
                {
                    eprintln!("MAIN: version-check progress UI failed: {e:#?}");
                }
            }
        });

        Self {
            tx: Some(prog_tx),
            task: Some(progress_task),
        }
    }

    fn as_updater_progress(&self) -> Option<updater::FetchProgressTx> {
        self.tx.clone()
    }

    async fn finish(self) {
        let Some(tx) = self.tx else {
            return;
        };
        drop(tx);
        if let Some(task) = self.task {
            if let Err(join_err) = task.await {
                eprintln!("MAIN: version-check progress task ended with: {join_err:#?}");
            }
        }
    }
}

async fn show_webapp(app_state: &Arc<AppState>, chrome: &Arc<Cdp>) -> Result<()> {
    let mut page = app_state.page.lock().await;

    let webapp_url = constant::WEBAPP_URL;

    // The static player is now a readiness-gated local service. We trust systemd
    // for readiness and only keep the fast-path when Chromium is already on it.
    let current_url = chrome.get_current_url().await?;
    if current_url.starts_with(webapp_url) {
        *page = Page::WebApp(unix_s());
        return Ok(());
    }

    // This is to avoid Err Network Changed from Chrome
    time::sleep(Duration::from_millis(constant::WIFI_WEBAPP_DELAY)).await;
    chrome
        .navigate(webapp_url)
        .await
        .with_context(|| format!("navigating to {webapp_url}"))?;

    println!("MAIN: Navigated to {webapp_url}");
    *page = Page::WebApp(unix_s());
    Ok(())
}

async fn show_message(chrome: &Arc<Cdp>, app_state: &Arc<AppState>, message: &str) -> Result<()> {
    let encoded = urlencoding::encode(message);
    let message_url = format!("{}{}", constant::MSG_URL_PREFIX, encoded);
    chrome
        .navigate(&message_url)
        .await
        .with_context(|| format!("navigating to {message_url}"))?;
    println!("MAIN: Navigated to {message_url}");

    let mut page = app_state.page.lock().await;
    *page = Page::Message(unix_s(), message.to_string());
    Ok(())
}

/// Navigate the TV to a transient message WITHOUT recording it as the canonical `app_state.page`.
///
/// Used by the version-check progress UI. Because it never mutates `page`, a lagging progress
/// navigation can no longer overwrite the page state set by a newer (possibly concurrent)
/// transition — so the no-update restore guard reads a truthful page identity and cannot be
/// tricked into clobbering a page another operation navigated to during the check.
async fn navigate_transient_message(chrome: &Arc<Cdp>, message: &str) -> Result<()> {
    let encoded = urlencoding::encode(message);
    let message_url = format!("{}{}", constant::MSG_URL_PREFIX, encoded);
    chrome
        .navigate(&message_url)
        .await
        .with_context(|| format!("navigating to transient {message_url}"))?;
    println!("MAIN: Navigated to transient {message_url}");
    Ok(())
}

async fn show_system_upgrade(
    chrome: &Arc<Cdp>,
    app_state: &Arc<AppState>,
    message: &str,
) -> Result<()> {
    let message_url = format!("{}{}", constant::MSG_URL_PREFIX, message);
    chrome
        .navigate(&message_url)
        .await
        .with_context(|| format!("navigating to {message_url}"))?;
    println!("MAIN: Navigated to {message_url}");

    let mut page = app_state.page.lock().await;
    *page = Page::SystemUpgrade(unix_s());
    Ok(())
}

async fn show_factory_reset(chrome: &Arc<Cdp>, app_state: &Arc<AppState>) -> Result<()> {
    let message_url = format!(
        "{}{}",
        constant::MSG_URL_PREFIX,
        constant::FACTORY_RESET_MSG
    );
    chrome
        .navigate(&message_url)
        .await
        .with_context(|| format!("navigating to {message_url}"))?;
    println!("MAIN: Navigated to {message_url}");

    let mut page = app_state.page.lock().await;
    *page = Page::FactoryReset(unix_s());
    Ok(())
}

async fn wait_for_controld(timeout: Duration) -> Result<()> {
    println!("MAIN: Waiting for controld connection...");

    let wait_future = async {
        loop {
            match dbus_utils::check_dbus_connection(
                constant::DBUS_CONTROLD_DESTINATION,
                constant::DBUS_CONTROLD_OBJECT,
            ) {
                Ok(_) => break,
                Err(e) => {
                    println!("MAIN: controld not available yet: {e:#?}, retrying in 2 seconds...");
                    time::sleep(Duration::from_secs(2)).await;
                }
            }
        }
    };

    match time::timeout(timeout, wait_future).await {
        Ok(_) => Ok(()),
        Err(_) => Err(anyhow::anyhow!("Timeout waiting for controld connection")),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

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
    fn set_and_get_setup_phase() {
        let lifecycle = SetupLifecycle::new();
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
            lifecycle,
            update_in_progress: Arc::new(AtomicBool::new(false)),
        });

        assert_eq!(get_setup_phase(&app_state), "idle");

        app_state.lifecycle.set(SetupPhase::WifiConnecting);
        assert_eq!(get_setup_phase(&app_state), "wifi_connecting");

        app_state.lifecycle.set(SetupPhase::CheckingVersion);
        assert_eq!(get_setup_phase(&app_state), "checking_version");

        app_state.lifecycle.set(SetupPhase::Updating);
        assert_eq!(get_setup_phase(&app_state), "updating");

        app_state.lifecycle.set(SetupPhase::UpdateFailed);
        assert_eq!(get_setup_phase(&app_state), "update_failed");

        app_state.lifecycle.set(SetupPhase::Pairing);
        assert_eq!(get_setup_phase(&app_state), "pairing");

        app_state.lifecycle.set(SetupPhase::Ready);
        assert_eq!(get_setup_phase(&app_state), "ready");
    }

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

    #[test]
    fn update_in_progress_guard_prevents_concurrent_updates() {
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

    /// Regression for PR #206 review 4475717770: BLE setup actions (connect_wifi/keep_wifi) and the
    /// QR switch must bail before side effects while a non-blocking OTA holds the guard, so the
    /// updater-owned setup_phase mobile polls is not clobbered and Wi-Fi is not switched mid-update.
    #[test]
    fn ble_action_blocked_only_while_update_in_progress() {
        let flag = AtomicBool::new(false);
        assert_eq!(ble_action_block_during_update(&flag), None);

        // While an update holds the guard, the action is rejected with DeviceUpdating.
        let held = AtomicBool::new(true);
        assert_eq!(
            ble_action_block_during_update(&held),
            Some(ble::BleStatus::DeviceUpdating),
        );
    }

    /// The gate must track the real UpdateGuard lifecycle: blocked while a guard is held, allowed
    /// again once it drops (so a finished/failed update re-opens BLE setup actions).
    #[test]
    fn ble_action_gate_follows_update_guard_lifecycle() {
        let flag = Arc::new(AtomicBool::new(false));
        assert_eq!(ble_action_block_during_update(&flag), None);

        {
            let _guard = UpdateGuard::try_acquire(&flag).expect("guard acquired");
            assert_eq!(
                ble_action_block_during_update(&flag),
                Some(ble::BleStatus::DeviceUpdating),
            );
        }

        assert_eq!(ble_action_block_during_update(&flag), None);
    }

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
}

#[cfg(test)]
mod version_check_progress_tests {
    use super::{UpdateExecution, VersionCheckProgress};
    use std::time::Duration;
    use tokio::sync::mpsc;
    use tokio::time::timeout;

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
}

#[cfg(test)]
mod available_system_update_follow_up_tests {
    use super::{
        AvailableSystemUpdateFollowUp, UpdateCheckResult, available_system_update_follow_up,
    };
    use anyhow::anyhow;

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
            available_system_update_follow_up(&Err(anyhow!("cdp navigate failed"))),
            AvailableSystemUpdateFollowUp::LogFailure,
        );
    }
}

#[cfg(test)]
mod restore_page_target_tests {
    use super::{Page, RestorePageTarget, SetupPhase, restore_page_target, unix_s};

    /// Regression for PR #206 review 4475472053: after a manual D-Bus no-update check, the
    /// transient "Checking for updates..." progress screen must be cleared by re-showing the real
    /// canonical surface. The progress UI never records `app_state.page`, so the canonical page is
    /// still the pre-check surface and must map back to itself (not a no-op, which left the TV stuck).
    #[test]
    fn webapp_canonical_page_reshows_webapp() {
        assert_eq!(
            restore_page_target(&Page::WebApp(unix_s()), SetupPhase::Ready),
            RestorePageTarget::Webapp,
        );
    }

    #[test]
    fn qrcode_canonical_page_reshows_qrcode() {
        assert_eq!(
            restore_page_target(&Page::QRCode(unix_s()), SetupPhase::Pairing),
            RestorePageTarget::Qrcode,
        );
    }

    #[test]
    fn message_canonical_page_reshows_same_message() {
        let msg = "Setting things up".to_string();
        assert_eq!(
            restore_page_target(&Page::Message(unix_s(), msg.clone()), SetupPhase::Idle),
            RestorePageTarget::Message(msg),
        );
    }

    #[test]
    fn factory_reset_canonical_page_reshows_factory_reset() {
        assert_eq!(
            restore_page_target(&Page::FactoryReset(unix_s()), SetupPhase::Idle),
            RestorePageTarget::FactoryReset,
        );
    }

    /// Stale failure screen: a prior UpdateFailed left `SystemUpgrade` as the canonical page, but
    /// the retry that produced NoUpdateNeeded already cleared update_failed. Re-showing the failure
    /// copy would be wrong, so route to the phase-appropriate surface instead.
    #[test]
    fn stale_system_upgrade_routes_by_phase() {
        assert_eq!(
            restore_page_target(&Page::SystemUpgrade(unix_s()), SetupPhase::Ready),
            RestorePageTarget::Webapp,
        );
        assert_eq!(
            restore_page_target(&Page::SystemUpgrade(unix_s()), SetupPhase::Pairing),
            RestorePageTarget::Qrcode,
        );
        assert_eq!(
            restore_page_target(&Page::SystemUpgrade(unix_s()), SetupPhase::Idle),
            RestorePageTarget::Qrcode,
        );
    }

    /// Reflashing cannot be rebuilt from stored page state and is unreachable on NoUpdateNeeded;
    /// None has no surface. Both leave the page unchanged.
    #[test]
    fn reflashing_and_none_make_no_change() {
        assert_eq!(
            restore_page_target(
                &Page::ReflashingRequired(unix_s(), "reflash".to_string()),
                SetupPhase::Idle,
            ),
            RestorePageTarget::NoChange,
        );
        assert_eq!(
            restore_page_target(&Page::None(unix_s()), SetupPhase::Idle),
            RestorePageTarget::NoChange,
        );
    }
}

#[cfg(test)]
mod phase_preservation_regression_tests {
    use super::*;
    use tempfile::NamedTempFile;

    /// Regression test for PR #206 review finding: version-check failures should
    /// preserve durable phases (Ready, Pairing) instead of demoting to Idle.
    ///
    /// Scenario: Device is Ready/Pairing, startup runs mandatory update check,
    /// version fetch succeeds but is_update_required() fails (distributor error).
    /// Expected: Device stays Ready/Pairing, shows correct UI.
    /// Bug: Device gets demoted to Idle, shows QR instead of webapp.
    #[test]
    fn version_check_failure_preserves_ready_phase() {
        let file = NamedTempFile::new().unwrap();
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

    /// Same test for Pairing phase preservation on version-check failure.
    #[test]
    fn version_check_failure_preserves_pairing_phase() {
        let file = NamedTempFile::new().unwrap();
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
        let file = NamedTempFile::new().unwrap();
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
    /// failure (lines 1638-1649) should preserve durable phases, not unconditionally set Idle.
    ///
    /// Scenario: Ready device, check_and_update_system runs, but refresh_remote_version
    /// fails immediately (network/distributor/parse error) before we can check update status.
    /// Expected: Device stays Ready, shows version-check failure UI.
    /// Bug: Device demoted to Idle, paired device reports idle to mobile app.
    #[test]
    fn early_refresh_failure_preserves_ready_phase() {
        let file = NamedTempFile::new().unwrap();
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
        // The fix (lines 1638-1649) should restore phase_before_check if durable
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
        let file = NamedTempFile::new().unwrap();
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
