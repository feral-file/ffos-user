mod ble;
mod cdp;
mod cfg;
mod connectivity;
mod constant;
mod dbus_utils;
mod encoding;
mod log_uploader;
mod persistent_state;
mod system;
mod updater;
mod wifi_utils;

use crate::persistent_state::PersistentState;
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
    let app_state = Arc::new(AppState {
        device_id: ble_service.get_device_id().await,
        branch: cfg::branch().await?.to_string(),
        current_version: cfg::current_version().await?.to_string(),
        state_store: PersistentState::new(constant::CACHE_FILEPATH)?,
        internet: Connectivity::spawn().await,
        page: Mutex::new(Page::None(unix_s())),
        auto_proceed: AtomicBool::new(false),
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
    // flow will handle it
    if app_state.auto_proceed.load(Ordering::Acquire) {
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
            return Err(ble::BleStatus::VersionTooOld);
        }
        Ok(UpdateCheckResult::UpdateStarted) => {
            return Err(ble::BleStatus::DeviceUpdating);
        }
        Ok(UpdateCheckResult::VersionCheckFailed) => {
            return Err(ble::BleStatus::VersionCheckFailed);
        }
        Ok(UpdateCheckResult::NoUpdateNeeded) => {} // Continue with normal flow
        Err(e) => {
            // This shouldn't happen with NonBlocking, but handle it anyway
            eprintln!("MAIN: Error during update check: {e:#?}");
            return Err(ble::BleStatus::VersionCheckFailed);
        }
    }

    // Get topic id from controld
    let topic_id = match dbus_utils::get_relayer_info() {
        Ok(info) => info,
        Err(e) => {
            eprintln!("BLE: can't get relayer data from controld: {e:#?}");
            return Err(ble::BleStatus::ServerUnreachable);
        }
    };

    let state_store = &app_state.state_store;
    state_store.set(persistent_state::TOPIC_ID, &topic_id);
    if state_store.get(persistent_state::PAIRED).is_none() {
        state_store.set(persistent_state::PAIRED, "false");
    }
    if let Err(e) = state_store.save() {
        eprintln!("MAIN: Error saving cache: {e:#?}");
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
        AppState, Cdp, WifiError, ble, constant, dbus_utils, internet_setup_successfully_cb,
        persistent_state, show_factory_reset, show_message, show_qrcode, show_webapp, wifi_utils,
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
                if !app_state.internet.is_online(true).await {
                    return Err(ble::BleStatus::WifiRequired);
                }
                internet_setup_successfully_cb(&app_state, &chromium).await
            })
        })
    }

    pub fn create_get_info_cb(app_state: Arc<AppState>) -> ble::GetInfoCallback {
        Some(Box::new(move || vec![super::build_device_info(&app_state)]))
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
                if qrcode_requested {
                    let _ = show_qrcode(&app_state, &chromium).await;
                } else {
                    // When the web app is requested, treat this as confirmation
                    // that the mobile app has successfully paired and received
                    // the topic ID.
                    let state_store = &app_state.state_store;
                    let has_topic = state_store.get(persistent_state::TOPIC_ID).is_some();
                    let already_paired = state_store
                        .get(persistent_state::PAIRED)
                        .map(|v| v == "true")
                        .unwrap_or(false);
                    if has_topic && !already_paired {
                        println!("MAIN: QR switch -> marking device as paired");
                        app_state.state_store.set(persistent_state::PAIRED, "true");
                        if let Err(e) = app_state.state_store.save() {
                            eprintln!("MAIN: Error saving paired state: {e:#?}");
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
        // Check internet connectivity first
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
            super::AvailableSystemUpdateFollowUp::ReturnToWebApp => {
                // Already on the latest build: leave the transient progress screen without
                // showing extra TV copy (user stays on / returns to the bundled web app).
                let _ = show_webapp(app_state, chromium).await;
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

// device_info is <device_id>|<topic_id>|<internet>|<branch>|<version>
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

    format!("{device_id}|{topic_id}|{has_internet}|{branch}|{version}")
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
    // Check and update system using Required mode (only mandatory updates on startup)
    // Use Blocking execution since we can wait for completion during startup
    match check_and_update_system(
        &app_state,
        &chrome,
        UpdateMode::Required,
        UpdateExecution::Blocking,
    )
    .await?
    {
        UpdateCheckResult::TooOldToUpgrade | UpdateCheckResult::UpdateStarted => {
            // Device is either too old or updating; don't proceed with normal flow
            return Ok(());
        }
        UpdateCheckResult::VersionCheckFailed => {
            // Error was already shown to user, but we treat this as a hard failure
            return Err(anyhow::anyhow!("Failed to check version information"));
        }
        UpdateCheckResult::NoUpdateNeeded => {} // Continue with normal flow
    }

    // No update, ensure we have a topic ID if possible and then
    // show art/qrcode depending on topic ID and pairing state.
    let state_store = &app_state.state_store;
    if state_store.get(persistent_state::TOPIC_ID).is_none() {
        match dbus_utils::get_relayer_info() {
            Ok(topic_id) => {
                state_store.set(persistent_state::TOPIC_ID, &topic_id);
                if let Err(e) = state_store.save() {
                    eprintln!("MAIN: Error saving persistent state after relayer info: {e:#?}");
                }
            }
            Err(e) => {
                eprintln!(
                    "MAIN: startup_with_internet: can't get relayer data from controld: {e:#?}"
                );
            }
        }
    }
    let has_topic_id = state_store.get(persistent_state::TOPIC_ID).is_some();
    let is_paired = state_store
        .get(persistent_state::PAIRED)
        .map(|v| v == "true")
        .unwrap_or(false);

    println!("MAIN: startup_with_internet: has_topic_id={has_topic_id} is_paired={is_paired}");

    if has_topic_id && is_paired {
        show_webapp(&app_state, &chrome).await
    } else {
        show_qrcode(&app_state, &chrome).await
    }
}

async fn update(app_state: Arc<AppState>, chrome: Arc<Cdp>) -> Result<()> {
    let latest_version = updater::latest_version().await.unwrap_or_else(|e| {
        eprintln!("MAIN: latest_version for update banner: {e:#?}");
        "Unknown".to_string()
    });
    let base_msg = format!("{} {}", &constant::UPDATING_MSG_PREFIX, latest_version);
    let default_subtext = constant::UPDATING_MSG_SUBTEXT;
    let _ = show_system_upgrade(
        &chrome,
        &app_state,
        &format!("{base_msg}&subtext={default_subtext}"),
    )
    .await;
    let mut rx = updater::spawn_updater()?;
    while let Some(res) = rx.recv().await {
        match res {
            Ok(msg) => {
                let _ =
                    show_system_upgrade(&chrome, &app_state, &format!("{base_msg}&subtext={msg}"))
                        .await;
            }
            Err(e) => {
                let _ =
                    show_system_upgrade(&chrome, &app_state, &format!("{base_msg}&subtext={e}"))
                        .await;
                return Err(e.context("update process failed"));
            }
        }
    }
    Ok(())
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
}

/// Follow-up after `check_and_update_system(UpdateMode::Available, …)` in the D-Bus path.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum AvailableSystemUpdateFollowUp {
    /// Optional check found no newer build; leave the transient progress route quietly.
    ReturnToWebApp,
    /// Blocking orchestration failed (CDP navigate, updater start, etc.).
    LogFailure,
    /// Core already drove TV/updater (update started, reflash, classified fetch error).
    NoOp,
}

fn available_system_update_follow_up(
    result: &Result<UpdateCheckResult>,
) -> AvailableSystemUpdateFollowUp {
    match result {
        Ok(UpdateCheckResult::NoUpdateNeeded) => AvailableSystemUpdateFollowUp::ReturnToWebApp,
        Err(_) => AvailableSystemUpdateFollowUp::LogFailure,
        Ok(_) => AvailableSystemUpdateFollowUp::NoOp,
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
    let progress = VersionCheckProgress::start(execution, chrome.clone(), app_state.clone());

    // Always fetch the remote version first to ensure we have the latest info
    updater::refresh_remote_version(progress.as_updater_progress()).await;
    // First check if device is too old to auto-upgrade
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
            return Ok(UpdateCheckResult::TooOldToUpgrade);
        }
        Ok(false) => {} // Device can be upgraded, continue
        Err(e) => {
            eprintln!("MAIN: Error checking if device is too old: {e:#?}");
            // Continue with update check if this fails
        }
    }

    // Check if update is needed based on mode
    let needs_update = match mode {
        UpdateMode::Required => updater::is_update_required(progress.as_updater_progress()).await,
        UpdateMode::Available => {
            updater::is_update_available(progress.as_updater_progress()).await
        }
    };

    progress.finish().await;

    match needs_update {
        Ok(true) => {
            match execution {
                UpdateExecution::Blocking => {
                    update(app_state.clone(), chrome.clone()).await?;
                }
                UpdateExecution::NonBlocking => {
                    task::spawn(update(app_state.clone(), chrome.clone()));
                }
            }
            Ok(UpdateCheckResult::UpdateStarted)
        }
        Ok(false) => {
            if mode == UpdateMode::Available {
                println!("MAIN: System update requested but no update available");
            }
            Ok(UpdateCheckResult::NoUpdateNeeded)
        }
        Err(ve) => {
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
    }
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

    fn start(execution: UpdateExecution, chrome: Arc<Cdp>, app_state: Arc<AppState>) -> Self {
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
                    show_message(&chrome, &app_state, constant::VERSION_CHECK_PROGRESS_TV_MSG).await
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

        let receiver_task = tokio::spawn(async move {
            while rx.recv().await.is_some() {}
        });

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

    #[test]
    fn no_update_needed_returns_to_webapp() {
        assert_eq!(
            available_system_update_follow_up(&Ok(UpdateCheckResult::NoUpdateNeeded)),
            AvailableSystemUpdateFollowUp::ReturnToWebApp,
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
