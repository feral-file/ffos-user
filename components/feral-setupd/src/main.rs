mod ble;
mod cache;
mod cdp;
mod cfg;
mod connectivity;
mod constant;
mod dbus_utils;
mod encoding;
mod system;
mod updater;
mod wifi_utils;

use crate::dbus_utils::PageStateProvider;
use crate::wifi_utils::{Error as WifiError, SSIDsCacher};
use anyhow::Context;
use anyhow::Result;
use ble::Ble;
use cache::Cache;
use cdp::Cdp;
use connectivity::Connectivity;
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

async fn wait_for_connectd_dbus() {
    println!("MAIN: Waiting for connectd D-Bus connection...");
    loop {
        match dbus_utils::check_dbus_connection(
            constant::DBUS_CONNECTD_DESTINATION,
            constant::DBUS_CONNECTD_OBJECT,
        ) {
            Ok(_) => {
                println!("MAIN: connectd D-Bus connection established successfully");
                break;
            }
            Err(e) => {
                println!("MAIN: connectd D-Bus not available yet: {e}, retrying in 2 seconds...");
                time::sleep(Duration::from_secs(2)).await;
            }
        }
    }
}

#[tokio::main]
async fn main() -> Result<()> {
    // Initialize dependencies
    let chrome = Cdp::connect(constant::CDP_URL)
        .await
        .context("connecting to CDP")?;
    let chrome = Arc::new(chrome);
    let ble_service = Arc::new(Ble::new());
    let app_state = Arc::new(AppState {
        device_id: ble_service.get_device_id().await,
        branch: cfg::branch().await?.to_string(),
        current_version: cfg::current_version().await?.to_string(),
        app_cache: Cache::new(constant::CACHE_FILEPATH)?,
        internet: Connectivity::spawn().await,
        page: Mutex::new(Page::None(unix_s())),
        auto_proceed: AtomicBool::new(false),
    });
    println!("MAIN: App state initialized: {app_state:?}");

    // Start bluetooth advertising with callbacks
    let ssids_cacher = Arc::new(SSIDsCacher::new());
    ble_service
        .start(
            create_bt_connected_cb(app_state.clone(), chrome.clone()),
            create_factory_reset_cb(app_state.clone(), chrome.clone()),
            create_connect_wifi_cb(app_state.clone(), chrome.clone()),
            create_keep_wifi_cb(app_state.clone(), chrome.clone()),
            create_get_info_cb(app_state.clone()),
            ssids_cacher.clone(),
        )
        .await
        .context("starting Bluetooth advertising")?;
    println!("MAIN: Bluetooth advertising started successfully");

    // Wait for connectd D-Bus connection before proceeding
    wait_for_connectd_dbus().await;

    let used_to_connect = app_state.app_cache.get(cache::CONNECTED);

    // If the device used to be able to connect to the internet
    // It's likely that it will have internet again really soon
    // We aggressively poll for internet for a few seconds to
    // go directly to the webapp instead of the QRCode
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

    let has_internet = app_state.internet.is_online(true).await;
    if !has_internet {
        // Show the QRCode so the user can do something with the internet
        ssids_cacher.trigger_refresh();
        let _ = show_qrcode(&app_state, &chrome).await;
        app_state.auto_proceed.store(true, Ordering::Release);
        let app_state = app_state.clone();
        let chrome = chrome.clone();
        tokio::spawn(async move {
            // If the device used to be able to connect to the internet
            // We should be more aggressive with the polling (we want to take action as soon as users fix the internet)
            // Otherwise, we should be more conservative as users might plug in the LAN cable, but this is rare
            let urgency = if used_to_connect.is_some() {
                Duration::from_millis(constant::AGGRESSIVE_INTERNET_CHECK_INTERVAL)
            } else {
                Duration::from_millis(constant::RELAXED_INTERNET_CHECK_INTERVAL)
            };
            app_state.internet.wait_until_online(urgency, None).await;
            if used_to_connect.is_none() {
                app_state.app_cache.set(cache::CONNECTED, "true");
                app_state.app_cache.save(constant::CACHE_FILEPATH).unwrap();
            }
            // If the user has chosen to provide a different wifi
            // This will be false and we should not proceed
            if app_state.auto_proceed.load(Ordering::Acquire) {
                on_startup_with_internet(app_state, chrome).await;
            }
        });
    } else {
        if used_to_connect.is_none() {
            app_state.app_cache.set(cache::CONNECTED, "true");
            app_state.app_cache.save(constant::CACHE_FILEPATH).unwrap();
        }
        on_startup_with_internet(app_state.clone(), chrome.clone()).await;
    }

    // Listen for QRCode switch signal
    let qrcode_switch_cb = create_qrcode_switch_cb(app_state.clone(), chrome.clone());
    let stop_dbus_listener = Arc::new(AtomicBool::new(false));
    dbus_utils::listen_for_signal(
        constant::DBUS_CONNECTD_OBJECT,
        constant::DBUS_CONNECTD_INTERFACE,
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
    match ble_service.stop().await {
        Ok(_) => println!("MAIN: BLE service stopped"),
        Err(e) => println!("MAIN: Error stopping BLE service: {e}"),
    }
    println!("MAIN: Shutting down...");
    Ok(())
}

fn create_bt_connected_cb(
    app_state: Arc<AppState>,
    chromium: Arc<Cdp>,
) -> ble::BTConnectedCallback {
    Some(Box::new(move || {
        let chromium = chromium.clone();
        let app_state = app_state.clone();
        Box::pin(async move {
            let _ = show_message(&chromium, &app_state, constant::WELCOME_MSG).await;
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
                    "MAIN: Failed to connect to wifi \"{ssid}\" in {:?} ms: {e}",
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
            eprintln!("MAIN: Error checking for update: {e}");
            let _ = show_message(
                chromium,
                app_state,
                constant::UPDATER_FAILED_TO_CHECK_VERSION_MSG,
            )
            .await;
            return Err(constant::BLE_ERR_CODE_VERSION_CHECK_FAILED);
        }
    }

    // Get topic id from connectd
    let topic_id = match dbus_utils::get_relayer_info() {
        Ok(info) => info,
        Err(e) => {
            eprintln!("BLE: can't get relayer data from connectd: {e}");
            return Err(constant::BLE_ERR_CODE_SERVER_UNREACHABLE);
        }
    };

    app_state.app_cache.set(cache::TOPIC_ID, &topic_id);
    match app_state.app_cache.save(constant::CACHE_FILEPATH) {
        Ok(_) => {}
        Err(e) => {
            eprintln!("MAIN: Error saving cache: {e}");
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
            Err(e) => println!("MAIN: Error reading message: {e}"),
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

async fn on_startup_with_internet(app_state: Arc<AppState>, chrome: Arc<Cdp>) {
    // If the update process is triggered and it takes over the chromium
    // We should not proceed with the normal flow any more
    // The device needs to automatically restart to apply the update
    // So we just return
    match updater::is_update_required().await {
        Ok(true) => {
            let _ = update(app_state.clone(), chrome.clone()).await;
            return;
        }
        Err(e) => {
            eprintln!("MAIN: Error checking for update: {e}");
            let _ = show_message(
                &chrome,
                &app_state,
                constant::UPDATER_FAILED_TO_CHECK_VERSION_MSG,
            )
            .await;
            return;
        }
        Ok(false) => {}
    }

    // Otherwise, proceed with the normal flow
    let has_cache = app_state.app_cache.get(cache::TOPIC_ID).is_some();
    if has_cache {
        let _ = show_webapp(&app_state, &chrome).await;
    } else {
        let _ = show_qrcode(&app_state, &chrome).await;
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
                eprintln!("MAIN: Update process failed: {e:#}");
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
