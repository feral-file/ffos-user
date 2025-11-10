use std::env;

use feral_setupd::cache::{self, Cache};
use feral_setupd::cdp::Cdp;
use feral_setupd::connectivity::Connectivity;
use feral_setupd::constant;
use feral_setupd::dbus_client;
use feral_setupd::dbus_utils::{self, PageStateProvider};
use feral_setupd::updater;
use feral_setupd::wifi_utils::{self, Error as WifiError, SSIDsCacher};
use feral_setupd::cfg;

#[cfg(target_os = "linux")]
use feral_setupd::ble::{self, Ble};
#[cfg(not(target_os = "linux"))]
mod ble_stub {
    use anyhow::Result;
    use std::future::Future;
    use std::pin::Pin;
    use std::sync::Arc;

    use feral_setupd::system;
    use feral_setupd::wifi_utils::SSIDsCacher;
    use tokio::sync::Mutex;

    pub type BTConnectedCallback =
        Option<Box<dyn Fn() -> Pin<Box<dyn Future<Output = ()> + Send>> + Send + Sync>>;
    pub type BTDisconnectedCallback = BTConnectedCallback;
    pub type ConnectWifiCallback = Box<
        dyn Fn(&str, &str) -> Pin<Box<dyn Future<Output = std::result::Result<String, u8>> + Send>>
            + Send
            + Sync,
    >;
    pub type KeepWifiCallback = Box<
        dyn Fn() -> Pin<Box<dyn Future<Output = std::result::Result<String, u8>> + Send>>
            + Send
            + Sync,
    >;
    pub type GetInfoCallback = Option<Box<dyn Fn() -> Vec<String> + Send + Sync>>;
    pub type FactoryResetCallback = BTConnectedCallback;

    #[derive(Default)]
    pub struct Ble {
        device_id: Mutex<String>,
    }

    impl Ble {
        pub fn new() -> Self {
            Self {
                device_id: Mutex::new(system::get_device_id()),
            }
        }

        #[allow(clippy::too_many_arguments)]
        pub async fn start(
            &self,
            _bt_connected_cb: BTConnectedCallback,
            _bt_disconnected_cb: BTDisconnectedCallback,
            _factory_reset_cb: FactoryResetCallback,
            _connect_wifi_cb: ConnectWifiCallback,
            _keep_wifi_cb: KeepWifiCallback,
            _get_info_cb: GetInfoCallback,
            _ssids_cacher: Arc<SSIDsCacher>,
        ) -> Result<()> {
            println!("BLE: Not supported on this platform; skipping Bluetooth initialization");
            Ok(())
        }

        pub async fn get_device_id(&self) -> String {
            self.device_id.lock().await.clone()
        }

        pub async fn stop(&self) -> Result<()> {
            Ok(())
        }
    }
}
#[cfg(not(target_os = "linux"))]
use ble_stub::{self as ble, Ble};
use anyhow::Context;
use anyhow::Result;
use std::sync::Arc;
use std::sync::atomic::{AtomicBool, Ordering};
use std::time::Instant;
use std::time::{SystemTime, UNIX_EPOCH};
use tokio::signal::unix::{SignalKind, signal as unix_signal};
use tokio::{
    sync::Mutex,
    task,
    time::{self, Duration},
};

#[inline]
fn unix_s() -> i64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .unwrap()
        .as_secs() as i64
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
    fn timestamp(&self) -> i64 {
        match self {
            Page::None(ts) => *ts,
            Page::QRCode(ts) => *ts,
            Page::Message(ts, _) => *ts,
            Page::SystemUpgrade(ts) => *ts,
            Page::FactoryReset(ts) => *ts,
            Page::WebApp(ts) => *ts,
            Page::ReflashingRequired(ts, _) => *ts,
        }
    }

    fn page_type(&self) -> &str {
        match self {
            Page::None(_) => "None",
            Page::QRCode(_) => "QRCode",
            Page::Message(_, _) => "Message",
            Page::SystemUpgrade(_) => "SystemUpgrade",
            Page::FactoryReset(_) => "FactoryReset",
            Page::WebApp(_) => "WebApp",
            Page::ReflashingRequired(_, _) => "ReflashingRequired",
        }
    }

    /// Check if the page should be kept when bluetooth disconnects
    fn should_keep_on_bt_disconnect(&self) -> bool {
        match self {
            Page::WebApp(_)
            | Page::SystemUpgrade(_)
            | Page::FactoryReset(_)
            | Page::ReflashingRequired(_, _) => true,
            Page::Message(_, msg) => msg == constant::SETUP_SUCCESSFULLY_MSG,
            _ => false,
        }
    }
}

#[derive(Debug)]
struct AppState {
    device_id: String,
    branch: String,
    current_version: String,
    app_cache: Cache,
    internet: Connectivity,
    page: Mutex<Page>,
    qemu: bool,

    // This is the flag to indicate whether we should automatically redirect to webapp
    // when internet is available.
    // On a second boot, if the internet is unavailable, users have 2 choices
    // 1. Fix the internet connection, it will automatically check for update & play artwork
    // 2. Scan the QRCode and start everything over again
    // We need this flag to turn off the first flow if the user has chosen to provide a different wifi
    // true = auto proceed, false = user has chosen to provide a different wifi
    auto_proceed: AtomicBool,
}

impl PageStateProvider for AppState {
    fn get_page_state(&self) -> (String, String, i64) {
        let id = self.device_id.clone();
        let page = self.page.blocking_lock();
        (id, page.page_type().to_string(), page.timestamp())
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

    tokio::runtime::Builder::new_multi_thread()
        .enable_all()
        .build()
        .unwrap()
        .block_on(async {
            if let Err(e) = run().await {
                eprintln!("MAIN: Error running feral-setupd: {e:#?}");
                let error: &dyn std::error::Error = e.as_ref();
                sentry::capture_error(error);
            }
        });
}

async fn run() -> Result<()> {
    const PROFILE: &str = env!("BUILD_PROFILE");
    println!("MAIN: Build profile: {PROFILE}");

    let qemu = PROFILE == "qemu";
    if qemu {
        println!("MAIN: Running in qemu mode");
    }
    // Initialize state
    let ble_service = Arc::new(Ble::new());
    let app_state = Arc::new(AppState {
        device_id: ble_service.get_device_id().await,
        branch: cfg::branch().await?.to_string(),
        current_version: cfg::current_version().await?.to_string(),
        app_cache: Cache::new(constant::CACHE_FILEPATH)?,
        internet: Connectivity::spawn().await,
        page: Mutex::new(Page::None(unix_s())),
        auto_proceed: AtomicBool::new(false),
        qemu,
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

    // Initialize CDP
    let chrome = Cdp::connect(constant::CDP_URL)
        .await
        .context("connecting to CDP")?;
    let chrome = Arc::new(chrome);

    // Start bluetooth advertising with callbacks
    let ssids_cacher = Arc::new(SSIDsCacher::new());
    if !qemu {
        ble_service
            .start(
                create_bt_connected_cb(app_state.clone(), chrome.clone()),
                create_bt_disconnected_cb(app_state.clone(), chrome.clone()),
                create_factory_reset_cb(app_state.clone(), chrome.clone()),
                create_connect_wifi_cb(app_state.clone(), chrome.clone()),
                create_keep_wifi_cb(app_state.clone(), chrome.clone()),
                create_get_info_cb(app_state.clone()),
                ssids_cacher.clone(),
            )
            .await
            .context("starting Bluetooth advertising")?;
        println!("MAIN: Bluetooth advertising started successfully");
    }

    // Wait for controld D-Bus connection before proceeding
    wait_for_controld(Duration::from_millis(constant::WAIT_FOR_CONTROLD_TIMEOUT)).await?;

    // If the device used to be able to connect to the internet
    // It's likely that it will have internet again really soon
    // We aggressively poll for internet for a few seconds to
    // go directly to the webapp instead of the QRCode
    let used_to_connect = app_state.app_cache.get(cache::CONNECTED);
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
        // Show the QRCode so the user can do something with the internet
        ssids_cacher.trigger_refresh();
        let _ = show_qrcode(&app_state, &chrome).await;
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
            app_state.app_cache.set(cache::CONNECTED, "true");
            app_state.app_cache.save(constant::CACHE_FILEPATH)?;
        }
        // We now have internet, but we need to check if
        // the internet comes from bluetooth (auto_proceed is set to false)
        // if it's from bluetooth, we shouldn't do anything else as the bluetooth
        // flow will handle it
        if app_state.auto_proceed.load(Ordering::Acquire) {
            on_startup_with_internet(app_state.clone(), chrome.clone()).await?;
        }
    } else {
        if used_to_connect.is_none() {
            app_state.app_cache.set(cache::CONNECTED, "true");
            app_state.app_cache.save(constant::CACHE_FILEPATH)?;
        }
        if qemu {
            let topic_id = match dbus_client::get_relayer_info() {
                Ok(info) => info,
                Err(e) => {
                    eprintln!("QEMU: can't get relayer data from controld: {e:#?}");
                    String::new()
                }
            };
            app_state.app_cache.set(cache::TOPIC_ID, &topic_id);
            app_state.app_cache.save(constant::CACHE_FILEPATH)?;
        }
        on_startup_with_internet(app_state.clone(), chrome.clone()).await?;
    }

    // Listen for QRCode switch signal
    let qrcode_switch_cb = create_qrcode_switch_cb(app_state.clone(), chrome.clone());
    let stop_dbus_listener = Arc::new(AtomicBool::new(false));
    dbus_utils::listen_for_signal(
        constant::DBUS_CONTROLD_OBJECT,
        constant::DBUS_CONTROLD_INTERFACE,
        constant::DBUS_EVENT_QRCODE_SWITCH,
        stop_dbus_listener.clone(),
        qrcode_switch_cb,
    );

    // Start the D-Bus service
    dbus_utils::start_dbus_service(app_state.clone());

    // Wait for Ctrl+C or shutdown event
    wait_for_shutdown().await; // Ignore any errors
    println!("MAIN: Shutting down...");
    println!("MAIN: Stopping DBus listener...");
    stop_dbus_listener.store(true, Ordering::Relaxed);
    println!("MAIN: Stopping BLE service...");
    if !qemu {
        if let Err(e) = ble_service.stop().await {
            eprintln!("MAIN: Error stopping BLE service: {e:#?}");
            return Err(e);
        } else {
            println!("MAIN: BLE service stopped");
        }
    }
    println!("MAIN: Shutting down...");
    Ok(())
}

fn create_bt_connected_cb(
    app_state: Arc<AppState>,
    chromium: Arc<Cdp>,
) -> ble::BTConnectedCallback {
    Some(Box::new(move || {
        let app_state = app_state.clone();
        let chromium = chromium.clone();

        Box::pin(async move {
            let should_show_welcome = {
                let page = app_state.page.lock().await;
                matches!(*page, Page::QRCode(_))
            };
            if should_show_welcome {
                let _ = show_message(&chromium, &app_state, constant::WELCOME_MSG).await;
            }
        })
    }))
}

fn create_bt_disconnected_cb(
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

fn create_connect_wifi_cb(
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
            if let Err(e) = wifi_utils::connect(&ssid, &pwd) {
                eprintln!(
                    "MAIN: Failed to connect to wifi \"{ssid}\" in {:?} ms: {e: }",
                    start_time.elapsed().as_millis()
                );
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
                        constant::BLE_ERR_CODE_WRONG_WIFI_PWD
                    }
                    _ => constant::BLE_ERR_CODE_UNKNOWN_ERROR,
                };
                return Err(err_code);
            }

            // Return early if there is no internet
            if !app_state.internet.is_online(true).await {
                task::spawn(async move {
                    let _ = show_message(
                        &chromium,
                        &app_state,
                        constant::INTERNET_FAILED_TO_CONNECT_MSG,
                    )
                    .await;
                });
                return Err(constant::BLE_ERR_CODE_NO_INTERNET);
            }
            internet_setup_successfully_cb(&app_state, &chromium).await
        })
    })
}

fn create_keep_wifi_cb(app_state: Arc<AppState>, chromium: Arc<Cdp>) -> ble::KeepWifiCallback {
    Box::new(move || {
        let app_state = app_state.clone();
        let chromium = chromium.clone();
        Box::pin(async move {
            if !app_state.internet.is_online(true).await {
                return Err(constant::BLE_ERR_CODE_WIFI_REQUIRED);
            }
            internet_setup_successfully_cb(&app_state, &chromium).await
        })
    })
}

async fn internet_setup_successfully_cb(
    app_state: &Arc<AppState>,
    chromium: &Arc<Cdp>,
) -> Result<String, u8> {
    // First check if device is too old to auto-upgrade
    match updater::is_too_old_to_upgrade().await {
        Ok(true) => {
            // Device is too old, show reflashing QR code
            if let Ok(Some(flashing_guide)) = updater::flashing_guide_url().await {
                // Gather version information
                let current_version = app_state.current_version.clone();
                let latest_version = updater::latest_version()
                    .await
                    .unwrap_or_else(|_| "Unknown".to_string());
                let min_upgradeable = updater::min_upgradeable_version()
                    .await
                    .unwrap_or(None)
                    .unwrap_or_else(|| "Unknown".to_string());

                task::spawn({
                    let app_state = app_state.clone();
                    let chromium = chromium.clone();
                    async move {
                        let _ = show_reflashing_qrcode(
                            &app_state,
                            &chromium,
                            &flashing_guide,
                            &current_version,
                            &latest_version,
                            &min_upgradeable,
                        )
                        .await;
                    }
                });
            } else {
                // Fallback to showing message without QR code if no flashing guide URL
                task::spawn({
                    let app_state = app_state.clone();
                    let chromium = chromium.clone();
                    async move {
                        let _ =
                            show_message(&chromium, &app_state, constant::REFLASHING_REQUIRED_MSG)
                                .await;
                    }
                });
            }
            return Err(constant::BLE_ERR_CODE_VERSION_CHECK_FAILED);
        }
        Ok(false) => {} // Device can be upgraded, continue with normal flow
        Err(e) => {
            eprintln!("MAIN: Error checking if device is too old: {e:#?}");
            // Continue with normal flow if check fails
        }
    }

    // Update the firmware / software if required
    match updater::is_update_required().await {
        Ok(true) => {
            // Spawn the update process in the background
            // This is to avoid blocking Error code to mobile app
            // The update process will take over chromium and show the update progress
            task::spawn(update(app_state.clone(), chromium.clone()));
            return Err(constant::BLE_ERR_CODE_DEVICE_UPDATING);
        }
        Ok(false) => {} // No update required, proceed with the normal flow
        Err(e) => {
            eprintln!("MAIN: Error checking for update: {e:#?}");
            let _ = show_message(
                chromium,
                app_state,
                constant::UPDATER_FAILED_TO_CHECK_VERSION_MSG,
            )
            .await;
            return Err(constant::BLE_ERR_CODE_VERSION_CHECK_FAILED);
        }
    }

    // Get topic id from controld
    let topic_id = match dbus_client::get_relayer_info() {
        Ok(info) => info,
        Err(e) => {
            eprintln!("BLE: can't get relayer data from controld: {e:#?}");
            return Err(constant::BLE_ERR_CODE_SERVER_UNREACHABLE);
        }
    };

    app_state.app_cache.set(cache::TOPIC_ID, &topic_id);
    match app_state.app_cache.save(constant::CACHE_FILEPATH) {
        Ok(_) => {}
        Err(e) => {
            eprintln!("MAIN: Error saving cache: {e:#?}");
        }
    }

    let app_state = app_state.clone();
    let chromium = chromium.clone();
    task::spawn(async move {
        // This is a workaround to avoid Err Network Changed from Chrome
        // This potentially also avoids the white screen issue
        let _ = show_message(&chromium, &app_state, constant::SETUP_SUCCESSFULLY_MSG).await;
        time::sleep(Duration::from_millis(constant::WIFI_WEBAPP_DELAY)).await;
        let _ = show_webapp(&app_state, &chromium).await;
    });
    Ok(topic_id)
}

fn create_get_info_cb(app_state: Arc<AppState>) -> ble::GetInfoCallback {
    Some(Box::new(move || {
        app_state
            .app_cache
            .get(cache::TOPIC_ID)
            .map(|topic_id| vec![topic_id.to_string()])
            .unwrap_or_default()
    }))
}

fn create_qrcode_switch_cb(
    app_state: Arc<AppState>,
    chromium: Arc<Cdp>,
) -> dbus_utils::ListenCallback {
    Box::new(move |msg| {
        let chromium = chromium.clone();
        let app_state = app_state.clone();
        let mut qrcode_requested = false;
        match msg.read1::<bool>() {
            Ok(true) => qrcode_requested = true,
            Err(e) => println!("MAIN: Error reading message: {e: }"),
            _ => {}
        }
        task::spawn(async move {
            if qrcode_requested {
                let _ = show_qrcode(&app_state, &chromium).await;
            } else {
                let _ = show_webapp(&app_state, &chromium).await;
            }
        });
    })
}

fn create_factory_reset_cb(
    app_state: Arc<AppState>,
    chromium: Arc<Cdp>,
) -> ble::FactoryResetCallback {
    Some(Box::new(move || {
        let chromium = chromium.clone();
        let app_state = app_state.clone();
        Box::pin(async move {
            let _ = show_factory_reset(&chromium, &app_state).await;
        })
    }))
}

// The url format is like this
// url?step=qr&device_info=<device_id>|<topic_id>|<internet>|<branch>|<version>
async fn build_qrcode_url(app_state: &Arc<AppState>) -> String {
    let device_id = app_state.device_id.clone();
    let topic_id = app_state.app_cache.get(cache::TOPIC_ID).unwrap_or_default();
    let has_internet = if app_state.internet.is_online(false).await {
        "true"
    } else {
        "false"
    };
    let branch = app_state.branch.clone().replace('/', "%2F");
    let version = app_state.current_version.clone();

    format!(
        "{}&device_info={device_id}|{topic_id}|{has_internet}|{branch}|{version}&version={version}&device_id={device_id}",
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
    let qrcode_url = build_qrcode_url(app_state).await;
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
        "We're sorry—we've moved too far ahead for this version to catch up. Your FF1 is too far behind to auto-upgrade. Current version: {current_version} Latest version: {latest_version} Minimum upgradeable version: {min_upgradeable_version}. Scan the code above for step-by-step reflashing instructions, or contact us for help. support@feralfile.com"
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

async fn on_startup_with_internet(app_state: Arc<AppState>, chrome: Arc<Cdp>) -> Result<()> {
    // First check if device is too old to auto-upgrade
    match updater::is_too_old_to_upgrade().await {
        Ok(true) => {
            // Device is too old, show reflashing QR code and stop here
            if let Ok(Some(flashing_guide)) = updater::flashing_guide_url().await {
                // Gather version information
                let current_version = app_state.current_version.clone();
                let latest_version = updater::latest_version()
                    .await
                    .unwrap_or_else(|_| "Unknown".to_string());
                let min_upgradeable = updater::min_upgradeable_version()
                    .await
                    .unwrap_or(None)
                    .unwrap_or_else(|| "Unknown".to_string());

                show_reflashing_qrcode(
                    &app_state,
                    &chrome,
                    &flashing_guide,
                    &current_version,
                    &latest_version,
                    &min_upgradeable,
                )
                .await?;
            } else {
                // Fallback to showing message without QR code if no flashing guide URL
                show_message(&chrome, &app_state, constant::REFLASHING_REQUIRED_MSG).await?;
            }
            return Ok(());
        }
        Ok(false) => {} // Device can be upgraded, continue with normal flow
        Err(e) => {
            eprintln!("MAIN: Error checking if device is too old: {e:#?}");
            // Continue with normal flow if check fails
        }
    }

    // If the update process is triggered and it takes over the chromium
    // We should not proceed with the normal flow any more
    // The device needs to automatically restart to apply the update
    // So we just return
    match updater::is_update_required().await {
        Ok(true) => {
            update(app_state.clone(), chrome.clone()).await?;
            return Ok(());
        }
        Err(e) => {
            let _ = show_message(
                &chrome,
                &app_state,
                constant::UPDATER_FAILED_TO_CHECK_VERSION_MSG,
            )
            .await;
            return Err(e);
        }
        Ok(false) => {} // No update required, proceed with the normal flow
    };

    // No update, show art/qrcode depending on whether we have a cache
    let has_cache = app_state.app_cache.get(cache::TOPIC_ID).is_some();
    if !app_state.qemu && has_cache {
        show_webapp(&app_state, &chrome).await
    } else {
        show_qrcode(&app_state, &chrome).await
    }
}

async fn update(app_state: Arc<AppState>, chrome: Arc<Cdp>) -> Result<()> {
    let latest_version = updater::latest_version().await.unwrap_or_default();
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

async fn show_webapp(app_state: &Arc<AppState>, chrome: &Arc<Cdp>) -> Result<()> {
    let mut page = app_state.page.lock().await;
    // For webapp, we only navigate if the page is not it already
    if matches!(*page, Page::WebApp(_)) {
        return Ok(());
    }

    // This is to avoid Err Network Changed from Chrome
    time::sleep(Duration::from_millis(constant::WIFI_WEBAPP_DELAY)).await;

    let webapp_url = match cfg::webapp_url().await? {
        Some(url) => url,
        None => constant::WEBAPP_URL.to_string(),
    };
    chrome
        .navigate(&webapp_url)
        .await
        .with_context(|| format!("navigating to {webapp_url}"))?;
    println!("MAIN: Navigated to {webapp_url}");
    *page = Page::WebApp(unix_s());
    Ok(())
}

async fn show_message(chrome: &Arc<Cdp>, app_state: &Arc<AppState>, message: &str) -> Result<()> {
    let message_url = format!("{}{}", constant::MSG_URL_PREFIX, message);
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
            match dbus_client::check_connection(
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
    use feral_setupd::{cache, dbus_client};
    use parking_lot::Mutex as ParkingMutex;
    use std::sync::Arc;
    use tempfile::NamedTempFile;
    use tokio::sync::Mutex;
    use serial_test::serial;

    struct MockDbusClient {
        online: ParkingMutex<bool>,
    }

    impl MockDbusClient {
        fn new(initial: bool) -> Arc<Self> {
            Arc::new(Self {
                online: ParkingMutex::new(initial),
            })
        }
    }

    impl dbus_client::DbusClient for MockDbusClient {
        fn internet_availability(&self) -> bool {
            *self.online.lock()
        }

        fn get_relayer_info(&self) -> anyhow::Result<String> {
            Ok("topic".to_string())
        }

        fn check_connection(&self, _: &str, _: &str) -> anyhow::Result<()> {
            Ok(())
        }
    }

    #[test]
    fn page_helpers_behave_as_expected() {
        let ts = 42;

        let qrcode = Page::QRCode(ts);
        assert_eq!(qrcode.timestamp(), ts);
        assert_eq!(qrcode.page_type(), "QRCode");
        assert!(!qrcode.should_keep_on_bt_disconnect());

        let success_message = Page::Message(ts, constant::SETUP_SUCCESSFULLY_MSG.to_string());
        assert!(success_message.should_keep_on_bt_disconnect());

        let other_message = Page::Message(ts, "hello".to_string());
        assert!(!other_message.should_keep_on_bt_disconnect());

        let reset = Page::FactoryReset(ts);
        assert!(reset.should_keep_on_bt_disconnect());

        let reflashing = Page::ReflashingRequired(ts, "msg".to_string());
        assert!(reflashing.should_keep_on_bt_disconnect());
    }

    #[tokio::test]
    #[serial]
    async fn build_qrcode_url_includes_expected_fields() {
        dbus_client::set_dbus_client(MockDbusClient::new(true));

        let temp = NamedTempFile::new().unwrap();
        let cache = cache::Cache::new(temp.path().to_str().unwrap()).unwrap();
        cache.set(cache::TOPIC_ID, "topic123");

        let connectivity = Connectivity::spawn().await;
        let app_state = Arc::new(AppState {
            device_id: "device-42".to_string(),
            branch: "feature/new".to_string(),
            current_version: "3.2.1".to_string(),
            app_cache: cache,
            internet: connectivity,
            page: Mutex::new(Page::None(0)),
            auto_proceed: AtomicBool::new(false),
            qemu: false,
        });

        let url = build_qrcode_url(&app_state).await;
        let expected_branch = "feature%2Fnew";
        let expected = format!(
            "{}&device_info=device-42|topic123|true|{}|3.2.1&version=3.2.1&device_id=device-42",
            constant::QRCODE_URL_PREFIX, expected_branch
        );
        assert_eq!(url, expected);

        dbus_client::clear_dbus_client();
    }

    #[tokio::test]
    #[serial]
    async fn app_state_page_state_provider_returns_values() {
        dbus_client::set_dbus_client(MockDbusClient::new(false));

        let temp = NamedTempFile::new().unwrap();
        let cache = cache::Cache::new(temp.path().to_str().unwrap()).unwrap();

        let connectivity = Connectivity::spawn().await;
        let app_state = Arc::new(AppState {
            device_id: "device-id".to_string(),
            branch: "main".to_string(),
            current_version: "1.0.0".to_string(),
            app_cache: cache,
            internet: connectivity,
            page: Mutex::new(Page::QRCode(123)),
            auto_proceed: AtomicBool::new(false),
            qemu: false,
        });

        let state = app_state.clone();
        let (id, page, ts) = tokio::task::spawn_blocking(move || state.get_page_state())
            .await
            .unwrap();
        assert_eq!(id, "device-id");
        assert_eq!(page, "QRCode");
        assert_eq!(ts, 123);

        dbus_client::clear_dbus_client();
    }
}
