use crate::constant;
use crate::encoding;
use crate::log_uploader;
use crate::system;
use crate::wifi_utils::SSIDsCacher;
use bluer::{
    Adapter, Session,
    adv::Advertisement,
    adv::AdvertisementHandle,
    gatt::local::{
        Application, ApplicationHandle, Characteristic, CharacteristicNotifier,
        CharacteristicNotify, CharacteristicNotifyMethod, CharacteristicWrite,
        CharacteristicWriteMethod, ReqError, Service,
    },
};
use futures_util::future::FutureExt;
use std::pin::Pin;
use std::sync::{Arc, Weak};
use std::time::Instant;
use tokio::sync::Mutex;
use tokio_util::sync::CancellationToken;

use anyhow::Result;

#[derive(Copy, Clone, Debug, Eq, PartialEq)]
#[repr(u8)]
pub enum BleStatus {
    Success = constant::BLE_SUCCESS_CODE,
    WrongWifiPassword = constant::BLE_ERR_CODE_WRONG_WIFI_PWD,
    NoInternet = constant::BLE_ERR_CODE_NO_INTERNET,
    ServerUnreachable = constant::BLE_ERR_CODE_SERVER_UNREACHABLE,
    WifiRequired = constant::BLE_ERR_CODE_WIFI_REQUIRED,
    DeviceUpdating = constant::BLE_ERR_CODE_DEVICE_UPDATING,
    VersionCheckFailed = constant::BLE_ERR_CODE_VERSION_CHECK_FAILED,
    InvalidParams = constant::BLE_ERR_CODE_INVALID_PARAMS,
    FileError = constant::BLE_ERR_CODE_FILE_ERROR,
    NetworkError = constant::BLE_ERR_CODE_NETWORK_ERROR,
    UnknownError = constant::BLE_ERR_CODE_UNKNOWN_ERROR,
}

impl BleStatus {
    pub fn code(self) -> u8 {
        self as u8
    }
}

enum BleCommand {
    ScanWifi,
    ConnectWifi,
    KeepWifi,
    GetInfo,
    SetTime,
    FactoryReset,
    SendLogs,
    Unknown(String),
}

impl BleCommand {
    fn from_str(cmd: &str) -> Self {
        match cmd {
            constant::CMD_SCAN_WIFI => BleCommand::ScanWifi,
            constant::CMD_CONNECT_WIFI => BleCommand::ConnectWifi,
            constant::CMD_KEEP_WIFI => BleCommand::KeepWifi,
            constant::CMD_GET_INFO => BleCommand::GetInfo,
            constant::CMD_SET_TIME => BleCommand::SetTime,
            constant::CMD_FACTORY_RESET => BleCommand::FactoryReset,
            constant::CMD_SEND_LOGS => BleCommand::SendLogs,
            other => BleCommand::Unknown(other.to_string()),
        }
    }
}

pub type BTConnectedCallback =
    Option<Box<dyn Fn() -> Pin<Box<dyn Future<Output = ()> + Send>> + Send + Sync>>;
pub type BTDisconnectedCallback =
    Option<Box<dyn Fn() -> Pin<Box<dyn Future<Output = ()> + Send>> + Send + Sync>>;
pub type ConnectWifiCallback = Box<
    dyn Fn(&str, &str) -> Pin<Box<dyn Future<Output = Result<String, BleStatus>> + Send>>
        + Send
        + Sync,
>;
pub type KeepWifiCallback =
    Box<dyn Fn() -> Pin<Box<dyn Future<Output = Result<String, BleStatus>> + Send>> + Send + Sync>;
pub type GetInfoCallback = Option<Box<dyn Fn() -> Vec<String> + Send + Sync>>;
pub type FactoryResetCallback =
    Option<Box<dyn Fn() -> Pin<Box<dyn Future<Output = ()> + Send>> + Send + Sync>>;

pub struct BleCallbacks {
    pub bt_connected: BTConnectedCallback,
    pub bt_disconnected: BTDisconnectedCallback,
    pub factory_reset: FactoryResetCallback,
    pub connect_wifi: ConnectWifiCallback,
    pub keep_wifi: KeepWifiCallback,
    pub get_info: GetInfoCallback,
}

#[derive(Default)]
struct Inner {
    device_id: String,
    advertised: bool,
    session: Option<Session>,
    adapter: Option<Adapter>,
    adv_handle: Option<AdvertisementHandle>,
    app_handle: Option<ApplicationHandle>,
    // Cancellation token for disconnect monitor
    disconnect_monitor_cancel: Option<CancellationToken>,
}

pub struct Ble {
    inner: Mutex<Inner>,
}

impl Ble {
    pub fn new() -> Self {
        let device_id = system::get_device_id();
        Self {
            inner: Mutex::new(Inner {
                device_id,
                ..Default::default()
            }),
        }
    }

    pub async fn start(
        self: &Arc<Self>,
        callbacks: BleCallbacks,
        ssids_cacher: Arc<SSIDsCacher>,
    ) -> Result<()> {
        let me = Arc::downgrade(self);
        let mut inner = self.inner.lock().await;
        if inner.advertised {
            return Ok(());
        }

        // Initialize BlueZ session and power on adapter
        let session = Session::new().await?;
        let adapter = session.default_adapter().await?;
        adapter.set_powered(true).await?;
        println!(
            "BLE: Adapter {} powered on for {}",
            adapter.name(),
            inner.device_id
        );

        // Group into a GATT service and register it
        let app_handle = self
            .register_gatt_application(&adapter, me, callbacks, ssids_cacher)
            .await?;

        tokio::time::sleep(std::time::Duration::from_millis(500)).await;

        println!("BLE: GATT app registered; ready to receive commands");

        // Start advertising our service UUID
        let adv = Self::build_advertisement(&inner.device_id);
        let adv_handle = adapter.advertise(adv).await?;
        println!("BLE: Advertising GATT service {}", constant::SERVICE_UUID);

        inner.session = Some(session);
        inner.adapter = Some(adapter);
        inner.adv_handle = Some(adv_handle);
        inner.app_handle = Some(app_handle);
        inner.advertised = true;
        Ok(())
    }

    pub async fn stop(&self) -> Result<()> {
        let (adv, app, adapter, session, cancel_token) = {
            let mut inner = self.inner.lock().await;
            if !inner.advertised {
                return Ok(());
            }
            inner.advertised = false;

            // Cancel the disconnect monitor if it exists
            if let Some(token) = inner.disconnect_monitor_cancel.take() {
                println!("BLE: Cancelling disconnect monitor");
                token.cancel();
            }

            (
                inner.adv_handle.take(),
                inner.app_handle.take(),
                inner.adapter.take(),
                inner.session.take(),
                inner.disconnect_monitor_cancel.take(),
            )
        };

        // Wait a moment for the monitor to acknowledge cancellation
        tokio::time::sleep(std::time::Duration::from_millis(100)).await;

        // Disconnect all devices (to make sure BlueZ doesn't block adv unregistration)
        if let Some(adapter) = adapter {
            for addr in adapter.device_addresses().await? {
                println!("BLE: Disconnecting device {addr:?}");
                let dev = adapter.device(addr)?;
                println!(
                    "BLE: Device {addr:?} is connected: {:?}",
                    dev.is_connected().await?
                );
                if dev.is_connected().await? {
                    let _ = dev.disconnect().await; // ignore errors
                }
            }
            drop(adv);
            drop(app);
            drop(adapter);

            // Give more time for BlueZ to clean up (increased from 1s to 2s)
            tokio::time::sleep(std::time::Duration::from_millis(2000)).await;
        }

        // Finally drop the session
        drop(session);
        drop(cancel_token);
        println!("BLE: All resources cleaned up");
        Ok(())
    }

    /// Restarts advertising after a disconnection
    /// This is called automatically when a device disconnects to ensure immediate re-advertising
    pub async fn restart_advertising(&self) -> Result<()> {
        let mut inner = self.inner.lock().await;

        // If stop() has been called, advertised will be false, so we should not restart
        if !inner.advertised {
            println!("BLE: Not restarting advertising - service has been stopped");
            return Ok(());
        }

        println!("BLE: Connection lost, immediately restarting advertising...");

        // Check if adapter exists before proceeding
        if inner.adapter.is_none() {
            eprintln!("BLE: Cannot restart advertising - adapter not available");
            return Ok(());
        }

        // 1. Force disconnect all devices before restarting advertising
        // This is especially important for iOS devices which may leave connections in a "hanging" state
        println!("BLE: Forcefully disconnecting all devices before restart...");
        if let Some(adapter) = inner.adapter.as_ref() {
            Self::force_disconnect_all_devices(adapter).await;
        }

        // 2. Drop the old advertisement handle
        if let Some(old_handle) = inner.adv_handle.take() {
            drop(old_handle);
            tokio::time::sleep(std::time::Duration::from_millis(1000)).await;
        }

        // 3. Create a new advertisement with the same configuration
        let adv = Self::build_advertisement(&inner.device_id);

        // 4. Register the new advertisement
        // Get adapter reference again for advertising
        let adapter = inner.adapter.as_ref().unwrap();
        match adapter.advertise(adv).await {
            Ok(handle) => {
                inner.adv_handle = Some(handle);
                println!("BLE: Advertising restarted successfully");
            }
            Err(e) => {
                eprintln!("BLE: Failed to restart advertising: {e}");
                return Err(e.into());
            }
        }
        Ok(())
    }

    async fn force_disconnect_all_devices(adapter: &Adapter) {
        let Ok(addresses) = adapter.device_addresses().await else {
            eprintln!("BLE: Failed to get device addresses");
            // Continue anyway, try to restart advertising
            return;
        };

        for addr in addresses {
            let Ok(dev) = adapter.device(addr) else {
                eprintln!("BLE: Failed to get device {addr:?}");
                continue;
            };

            match dev.is_connected().await {
                Ok(true) => {
                    println!("BLE: Disconnecting device {addr:?}");
                    if let Err(e) = dev.disconnect().await {
                        eprintln!("BLE: Failed to disconnect {addr:?}: {e}");
                    }
                }
                Ok(false) => {
                    println!("BLE: Device {addr:?} already disconnected");
                }
                Err(e) => {
                    eprintln!("BLE: Failed to check connection status for {addr:?}: {e}");
                }
            }
        }
        // Give BlueZ time to process disconnections
        println!("BLE: Waiting for BlueZ to process disconnections...");
        tokio::time::sleep(std::time::Duration::from_millis(500)).await;
    }

    pub async fn get_device_id(&self) -> String {
        let st = self.inner.lock().await;
        st.device_id.clone()
    }

    // --- Internal helpers ---

    async fn create_cmd_char(
        &self,
        me: Weak<Self>,
        callbacks: BleCallbacks,
        ssids_cacher: Arc<SSIDsCacher>,
    ) -> Characteristic {
        let BleCallbacks {
            bt_connected,
            bt_disconnected,
            factory_reset,
            connect_wifi,
            keep_wifi,
            get_info,
        } = callbacks;

        // Shared storage for the notifier handle
        let notifier: Arc<Mutex<Option<CharacteristicNotifier>>> = Arc::new(Mutex::new(None));
        let notifier_for_write = notifier.clone();
        let notifier_for_notify = notifier.clone();
        let notifier_for_monitor = notifier.clone();

        // Shared storage for the current disconnect monitor cancellation token
        let cancel_token_storage: Arc<Mutex<Option<CancellationToken>>> =
            Arc::new(Mutex::new(None));
        let cancel_token_for_notify = cancel_token_storage.clone();

        let bt_connected_callback = Arc::new(bt_connected);
        let bt_disconnected_callback = Arc::new(bt_disconnected);
        let factory_reset_callback = Arc::new(factory_reset);
        let connect_wifi_callback = Arc::new(connect_wifi);
        let keep_wifi_callback = Arc::new(keep_wifi);
        let get_info_callback = Arc::new(get_info);

        Characteristic {
            uuid: constant::CMD_CHAR_UUID,
            // Enable notifications on this characteristic
            notify: Some(CharacteristicNotify {
                notify: true,
                method: CharacteristicNotifyMethod::Fun(Box::new(move |new_notifier| {
                    println!("BLE: a device is connected");
                    let handle = notifier_for_notify.clone();
                    let monitor_handle = notifier_for_monitor.clone();
                    let bt_connected_callback = bt_connected_callback.clone();
                    let bt_disconnected_callback = bt_disconnected_callback.clone();
                    let cancel_storage = cancel_token_for_notify.clone();
                    let me_for_monitor = me.clone();

                    async move {
                        // Cancel any existing disconnect monitor from previous connection
                        {
                            let mut old_token = cancel_storage.lock().await;
                            if let Some(token) = old_token.take() {
                                println!("BLE: Cancelling previous disconnect monitor");
                                token.cancel();
                                // Give the old monitor a moment to stop
                                tokio::time::sleep(std::time::Duration::from_millis(100)).await;
                            }
                        }

                        // Replace the old notifier with the new one
                        {
                            let mut old_notifier = handle.lock().await;
                            if old_notifier.is_some() {
                                println!("BLE: Replacing previous notifier");
                            }
                            *old_notifier = Some(new_notifier);
                        }

                        // Create a new cancellation token for this connection
                        let cancel_token = CancellationToken::new();
                        *cancel_storage.lock().await = Some(cancel_token.clone());

                        // Start new disconnect monitor
                        let disconnect_monitor = Self::start_disconnect_monitor(
                            me_for_monitor,
                            monitor_handle.clone(),
                            bt_disconnected_callback.clone(),
                            cancel_token,
                        );
                        tokio::spawn(disconnect_monitor);

                        if let Some(cb) = bt_connected_callback.as_ref() {
                            cb().await;
                        }
                    }
                    .boxed()
                })),
                ..Default::default()
            }),
            write: Some(CharacteristicWrite {
                write: true,
                write_without_response: false,
                method: CharacteristicWriteMethod::Fun(Box::new(move |data, _req| {
                    let notifier = notifier_for_write.clone();
                    let connect_wifi_callback = connect_wifi_callback.clone();
                    let factory_reset_callback = factory_reset_callback.clone();
                    let keep_wifi_callback = keep_wifi_callback.clone();
                    let get_info_callback = get_info_callback.clone();
                    let ssids_cacher = ssids_cacher.clone();
                    async move {
                        let payload = encoding::parse_payload(&data);
                        // No values, or malformed payload
                        if payload.is_none() {
                            eprintln!("BLE: Received malformed payload");
                            return Ok::<(), ReqError>(());
                        }
                        let vals = payload.unwrap();
                        // Not enough values, or malformed payload
                        if vals.len() < 2 {
                            eprintln!("BLE: Received payload with only {} values", vals.len());
                            return Ok::<(), ReqError>(());
                        }
                        // Enough values, parse command
                        let cmd = BleCommand::from_str(&vals[0]);
                        let reply_id = vals[1].clone();
                        let params = vals[2..].to_vec();

                        println!(
                            "BLE cmd: name={} reply_id={} param_count={}",
                            vals[0],
                            reply_id,
                            params.len()
                        );

                        match cmd {
                            BleCommand::ScanWifi => {
                                handle_scan_wifi(notifier, reply_id, ssids_cacher).await
                            }
                            BleCommand::ConnectWifi => {
                                handle_connect_wifi(
                                    notifier,
                                    reply_id,
                                    params,
                                    connect_wifi_callback,
                                )
                                .await
                            }
                            BleCommand::KeepWifi => {
                                handle_keep_wifi(notifier, reply_id, keep_wifi_callback).await
                            }
                            BleCommand::GetInfo => {
                                handle_get_info(notifier, reply_id, get_info_callback).await
                            }
                            BleCommand::SetTime => {
                                handle_set_time(notifier, reply_id, params).await
                            }
                            BleCommand::FactoryReset => {
                                handle_factory_reset(notifier, reply_id, factory_reset_callback)
                                    .await
                            }
                            BleCommand::SendLogs => {
                                handle_submit_logs(notifier, reply_id, params).await
                            }
                            BleCommand::Unknown(raw) => {
                                eprintln!("BLE: Unknown command: {raw}");
                                Ok::<(), ReqError>(())
                            }
                        }
                    }
                    .boxed()
                })),
                ..Default::default()
            }),
            ..Default::default()
        }
    }

    async fn start_disconnect_monitor(
        me: Weak<Self>,
        notifier: Arc<Mutex<Option<CharacteristicNotifier>>>,
        bt_disconnected_cb: Arc<BTDisconnectedCallback>,
        cancel_token: CancellationToken,
    ) {
        println!("BLE: Starting disconnect monitor for connected device");

        let mut check_interval = tokio::time::interval(std::time::Duration::from_secs(1));

        loop {
            tokio::select! {
                _ = cancel_token.cancelled() => {
                    println!("BLE: Disconnect monitor cancelled");
                    break;
                }
                _ = check_interval.tick() => {
                    let is_disconnected = {
                        let mut guard = notifier.lock().await;
                        if let Some(notifier) = guard.as_mut() {
                            notifier.is_stopped()
                        } else {
                            true
                        }
                    };

                    if is_disconnected {
                        println!("BLE: Device disconnected (session stopped)");

                        // Step 1: Immediately restart advertising (most important change)
                        // This happens before UI callback to minimize downtime
                        if let Some(ble) = me.upgrade() {
                            println!("BLE: Triggering automatic advertising restart...");
                            // Spawn in background to avoid blocking the callback
                            let restart_result = ble.restart_advertising().await;
                            if let Err(e) = restart_result {
                                eprintln!("BLE: Auto-restart advertising failed: {e}");
                            }
                        } else {
                            println!("BLE: Ble instance dropped, cannot restart advertising");
                        }

                        // Step 2: Notify UI to update (e.g., show QR code)
                        if let Some(cb) = bt_disconnected_cb.as_ref() {
                            cb().await;
                        }
                        break;
                    }
                }
            }
        }

        println!("BLE: Disconnect monitor stopped");
    }

    async fn register_gatt_application(
        &self,
        adapter: &Adapter,
        me: Weak<Self>,
        callbacks: BleCallbacks,
        ssids_cacher: Arc<SSIDsCacher>,
    ) -> Result<ApplicationHandle> {
        let svc = Service {
            uuid: constant::SERVICE_UUID,
            primary: true,
            characteristics: vec![self.create_cmd_char(me, callbacks, ssids_cacher).await],
            ..Default::default()
        };

        let app = Application {
            services: vec![svc],
            ..Default::default()
        };

        let app_handle = adapter.serve_gatt_application(app).await?;
        Ok(app_handle)
    }

    fn build_advertisement(device_id: &str) -> Advertisement {
        Advertisement {
            service_uuids: vec![constant::SERVICE_UUID].into_iter().collect(),
            discoverable: Some(true),
            local_name: Some(device_id.to_string()),
            min_interval: Some(std::time::Duration::from_millis(20)),
            max_interval: Some(std::time::Duration::from_millis(100)),
            ..Default::default()
        }
    }
}

async fn handle_scan_wifi(
    notifier: Arc<Mutex<Option<CharacteristicNotifier>>>,
    reply_id: String,
    ssids_cacher: Arc<SSIDsCacher>,
) -> Result<(), ReqError> {
    let start_time = Instant::now();
    let mut encoder = encoding::PayloadEncoder::new();
    encoder.push_str(&reply_id);

    match ssids_cacher.get().await {
        Ok(v) => {
            println!(
                "BLE cmd=scan_wifi: ssids={} duration_ms={}",
                v.len(),
                start_time.elapsed().as_millis()
            );
            encoder.push_code(BleStatus::Success.code());
            for ssid in &v {
                encoder.push_str(ssid);
            }
        }
        Err(e) => {
            eprintln!("BLE: Failed to scan wifi: {e}");
            encoder.push_code(BleStatus::UnknownError.code());
        }
    }

    let payload = encoder.finish();
    notify_central(notifier, payload).await
}

async fn handle_connect_wifi(
    notifier: Arc<Mutex<Option<CharacteristicNotifier>>>,
    reply_id: String,
    params: Vec<String>,
    cb: Arc<ConnectWifiCallback>,
) -> Result<(), ReqError> {
    // Expect at least SSID and password
    if params.len() < 2 {
        eprintln!(
            "BLE: Received wifi payload with only {} values",
            params.len()
        );
        return Ok(());
    }

    let ssid = &params[0];
    let pass = &params[1];

    let mut encoder = encoding::PayloadEncoder::new();
    encoder.push_str(&reply_id);

    match cb(ssid, pass).await {
        Ok(tid) => {
            encoder.push_code(BleStatus::Success.code());
            encoder.push_str(&tid);
        }
        Err(e) => {
            encoder.push_code(e.code());
        }
    }

    let payload = encoder.finish();
    notify_central(notifier, payload).await
}

async fn handle_keep_wifi(
    notifier: Arc<Mutex<Option<CharacteristicNotifier>>>,
    reply_id: String,
    cb: Arc<KeepWifiCallback>,
) -> Result<(), ReqError> {
    let mut encoder = encoding::PayloadEncoder::new();
    encoder.push_str(&reply_id);

    match cb().await {
        Ok(tid) => {
            encoder.push_code(BleStatus::Success.code());
            encoder.push_str(&tid);
        }
        Err(e) => {
            encoder.push_code(e.code());
        }
    }

    let payload = encoder.finish();
    notify_central(notifier, payload).await
}

async fn handle_get_info(
    notifier: Arc<Mutex<Option<CharacteristicNotifier>>>,
    reply_id: String,
    cb: Arc<GetInfoCallback>,
) -> Result<(), ReqError> {
    let infos = if let Some(cb) = cb.as_ref() {
        cb()
    } else {
        vec![]
    };

    let mut encoder = encoding::PayloadEncoder::new();
    encoder.push_str(&reply_id);
    encoder.push_code(BleStatus::Success.code());
    for info in &infos {
        encoder.push_str(info);
    }

    let payload = encoder.finish();
    notify_central(notifier, payload).await
}

async fn handle_set_time(
    _notifier: Arc<Mutex<Option<CharacteristicNotifier>>>,
    _reply_id: String,
    params: Vec<String>,
) -> Result<(), ReqError> {
    println!("BLE: Setting time");
    if params.len() < 2 {
        eprintln!(
            "BLE: Received timezone payload with only {} values",
            params.len()
        );
        return Ok(());
    }
    if let Err(e) = system::set_time(&params[0], &params[1]).await {
        eprintln!("BLE: Failed to set time: {e:#?}");
    }
    Ok(())
}

async fn handle_factory_reset(
    notifier: Arc<Mutex<Option<CharacteristicNotifier>>>,
    reply_id: String,
    cb: Arc<FactoryResetCallback>,
) -> Result<(), ReqError> {
    println!("BLE: Factory resetting");
    if let Some(cb) = cb.as_ref() {
        cb().await;
    }
    let status_code = if let Err(e) = system::factory_reset().await {
        eprintln!("BLE: Failed to factory reset: {e:#?}");
        [BleStatus::UnknownError.code()]
    } else {
        [BleStatus::Success.code()]
    };
    let mut encoder = encoding::PayloadEncoder::new();
    encoder.push_str(&reply_id);
    encoder.push_bytes(&status_code);
    let payload = encoder.finish();
    notify_central(notifier, payload).await
}

async fn handle_submit_logs(
    notifier: Arc<Mutex<Option<CharacteristicNotifier>>>,
    reply_id: String,
    params: Vec<String>,
) -> Result<(), ReqError> {
    println!("BLE log-submit: start reply_id={reply_id}");

    // Expect userId, apiKey, and title
    if params.len() < 3 {
        eprintln!(
            "BLE log-submit: invalid params len={}, expected at least 3",
            params.len()
        );

        let mut encoder = encoding::PayloadEncoder::new();
        encoder.push_str(&reply_id);
        encoder.push_code(BleStatus::InvalidParams.code());

        return notify_central(notifier, encoder.finish()).await;
    }

    let user_id = &params[0];
    let api_key = &params[1];
    let title = &params[2];

    println!("BLE log-submit: user={user_id} title={title}");

    let mut encoder = encoding::PayloadEncoder::new();
    encoder.push_str(&reply_id);

    // Collect log files
    println!("BLE log-submit: collecting log files");
    let log_files = match log_uploader::collect_log_files().await {
        Ok(files) => files,
        Err(e) => {
            eprintln!("BLE log-submit: failed to collect log files: {e}");
            encoder.push_code(BleStatus::FileError.code());
            return notify_central(notifier, encoder.finish()).await;
        }
    };

    // Create request body
    let body = log_uploader::create_log_submission_body(
        title,
        "Device log submission via BLE",
        vec!["device-logs".to_string(), "ble-submission".to_string()],
        log_files,
    );

    // Submit logs via HTTP
    println!("BLE log-submit: submitting logs for user={user_id}");

    let result = log_uploader::submit_logs_to_api(user_id, api_key, body).await;

    match result {
        Ok(_response) => {
            encoder.push_code(BleStatus::Success.code());
        }
        Err(e) => {
            eprintln!("BLE: ERROR - HTTP submission failed with error code: {e}",);
            encoder.push_code(BleStatus::NetworkError.code());
        }
    }

    notify_central(notifier, encoder.finish()).await
}

async fn notify_central(
    notifier: Arc<Mutex<Option<CharacteristicNotifier>>>,
    payload: Vec<u8>,
) -> Result<(), ReqError> {
    println!(
        "BLE: Notifying central with encoded payload ({} bytes)",
        payload.len()
    );
    let mut guard = notifier.lock().await;
    if let Some(notifier) = guard.as_mut() {
        notifier.notify(payload).await.map_err(|e| {
            eprintln!("BLE: Failed to notify central: {e}");
            ReqError::Failed
        })?;
        Ok(())
    } else {
        eprintln!("BLE: Notifier not yet available; skipping reply");
        // Return Ok here as this is not a critical error - the device might not be ready yet
        Ok(())
    }
}
