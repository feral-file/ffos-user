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

pub type BTConnectedCallback =
    Option<Box<dyn Fn() -> Pin<Box<dyn Future<Output = ()> + Send>> + Send + Sync>>;
pub type BTDisconnectedCallback =
    Option<Box<dyn Fn() -> Pin<Box<dyn Future<Output = ()> + Send>> + Send + Sync>>;
pub type ConnectWifiCallback = Box<
    dyn Fn(&str, &str) -> Pin<Box<dyn Future<Output = Result<String, u8>> + Send>> + Send + Sync,
>;
pub type KeepWifiCallback =
    Box<dyn Fn() -> Pin<Box<dyn Future<Output = Result<String, u8>> + Send>> + Send + Sync>;
pub type GetInfoCallback = Option<Box<dyn Fn() -> Vec<String> + Send + Sync>>;
pub type FactoryResetCallback =
    Option<Box<dyn Fn() -> Pin<Box<dyn Future<Output = ()> + Send>> + Send + Sync>>;

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

impl Default for Inner {
    fn default() -> Self {
        Self {
            device_id: String::new(),
            advertised: false,
            session: None,
            adapter: None,
            adv_handle: None,
            app_handle: None,
            disconnect_monitor_cancel: None,
        }
    }
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
        &self,
        me: Weak<Self>,
        bt_connected_cb: BTConnectedCallback,
        bt_disconnected_cb: BTDisconnectedCallback,
        factory_reset_cb: FactoryResetCallback,
        connect_wifi_cb: ConnectWifiCallback,
        keep_wifi_cb: KeepWifiCallback,
        get_info_cb: GetInfoCallback,
        ssids_cacher: Arc<SSIDsCacher>,
    ) -> Result<()> {
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
        let svc = Service {
            uuid: constant::SERVICE_UUID,
            primary: true,
            characteristics: vec![
                self.create_cmd_char(
                    me,
                    bt_connected_cb,
                    bt_disconnected_cb,
                    factory_reset_cb,
                    connect_wifi_cb,
                    keep_wifi_cb,
                    get_info_cb,
                    ssids_cacher,
                )
                .await,
            ],
            ..Default::default()
        };
        let app = Application {
            services: vec![svc],
            ..Default::default()
        };
        let app_handle = adapter.serve_gatt_application(app).await?;

        tokio::time::sleep(std::time::Duration::from_millis(500)).await;

        println!("BLE: GATT app registered; ready to receive commands");

        // Start advertising our service UUID
        let adv = Advertisement {
            service_uuids: vec![constant::SERVICE_UUID].into_iter().collect(),
            discoverable: Some(true),
            local_name: Some(inner.device_id.clone()),
            min_interval: Some(std::time::Duration::from_millis(20)),
            max_interval: Some(std::time::Duration::from_millis(100)),
            ..Default::default()
        };
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

        // 1. Drop the old advertisement handle
        if let Some(old_handle) = inner.adv_handle.take() {
            drop(old_handle);
            tokio::time::sleep(std::time::Duration::from_millis(1000)).await;
        }

        // 2. Create a new advertisement with the same configuration
        let adv = Advertisement {
            service_uuids: vec![constant::SERVICE_UUID].into_iter().collect(),
            discoverable: Some(true),
            local_name: Some(inner.device_id.clone()),
            min_interval: Some(std::time::Duration::from_millis(20)),
            max_interval: Some(std::time::Duration::from_millis(100)),
            ..Default::default()
        };

        // 3. Register the new advertisement
        // We know adapter is Some because we checked above
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

    pub async fn get_device_id(&self) -> String {
        let st = self.inner.lock().await;
        st.device_id.clone()
    }

    async fn create_cmd_char(
        &self,
        me: Weak<Self>,
        bt_connected_cb: BTConnectedCallback,
        bt_disconnected_cb: BTDisconnectedCallback,
        factory_reset_cb: FactoryResetCallback,
        connect_wifi_cb: ConnectWifiCallback,
        keep_wifi_cb: KeepWifiCallback,
        get_info_cb: GetInfoCallback,
        ssids_cacher: Arc<SSIDsCacher>,
    ) -> Characteristic {
        // Shared storage for the notifier handle
        let notifier: Arc<Mutex<Option<CharacteristicNotifier>>> = Arc::new(Mutex::new(None));
        let notifier_for_write = notifier.clone();
        let notifier_for_notify = notifier.clone();
        let notifier_for_monitor = notifier.clone();
        
        // Shared storage for the current disconnect monitor cancellation token
        let cancel_token_storage: Arc<Mutex<Option<CancellationToken>>> = Arc::new(Mutex::new(None));
        let cancel_token_for_notify = cancel_token_storage.clone();

        let bt_connected_callback = Arc::new(bt_connected_cb);
        let bt_disconnected_callback = Arc::new(bt_disconnected_cb);
        let factory_reset_callback = Arc::new(factory_reset_cb);
        let connect_wifi_callback = Arc::new(connect_wifi_cb);
        let keep_wifi_callback = Arc::new(keep_wifi_cb);
        let get_info_callback = Arc::new(get_info_cb);

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
                    println!("BLE: Received bluetooth data {data:?}");
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
                        println!("BLE: Payload: {vals:?}");
                        let cmd = vals[0].clone();
                        let reply_id = vals[1].clone();
                        let params = vals[2..].to_vec();
                        match cmd.as_str() {
                            constant::CMD_SCAN_WIFI => {
                                handle_scan_wifi(notifier, reply_id, ssids_cacher).await
                            }
                            constant::CMD_CONNECT_WIFI => {
                                handle_connect_wifi(
                                    notifier,
                                    reply_id,
                                    params,
                                    connect_wifi_callback,
                                )
                                .await
                            }
                            constant::CMD_KEEP_WIFI => {
                                handle_keep_wifi(notifier, reply_id, keep_wifi_callback).await
                            }
                            constant::CMD_GET_INFO => {
                                handle_get_info(notifier, reply_id, get_info_callback).await
                            }
                            constant::CMD_SET_TIME => {
                                handle_set_time(notifier, reply_id, params).await
                            }
                            constant::CMD_FACTORY_RESET => {
                                handle_factory_reset(notifier, reply_id, factory_reset_callback)
                                    .await
                            }
                            constant::CMD_SEND_LOGS => {
                                handle_submit_logs(notifier, reply_id, params).await
                            }
                            _ => {
                                eprintln!("BLE: Unknown command: {cmd}");
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
}

async fn handle_scan_wifi(
    notifier: Arc<Mutex<Option<CharacteristicNotifier>>>,
    reply_id: String,
    ssids_cacher: Arc<SSIDsCacher>,
) -> Result<(), ReqError> {
    // Scan available SSIDs using the helper
    let mut payload = Vec::with_capacity(2);
    payload.push(reply_id.as_bytes());

    let start_time = Instant::now();
    let ssids: Vec<String>; // To own the returned value
    let error_code: [u8; 1]; // To own the returned value
    match ssids_cacher.get().await {
        Ok(v) => {
            println!(
                "BLE: Found SSIDs \n{v:?} in {:?} ms",
                start_time.elapsed().as_millis()
            );
            ssids = v;
            payload.push(&[constant::BLE_SUCCESS_CODE]);
            payload.extend(ssids.iter().map(|s| s.as_bytes()));
        }
        Err(e) => {
            eprintln!("BLE: Failed to scan wifi: {e}");
            error_code = [constant::BLE_ERR_CODE_UNKNOWN_ERROR];
            payload.push(&error_code);
        }
    };
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

    let mut payload = Vec::with_capacity(3);
    payload.push(reply_id.as_bytes());

    // Pre-declare variables to own the returned values
    let topic_id: String;
    let error_code: [u8; 1];
    match cb(ssid, pass).await {
        Ok(tid) => {
            topic_id = tid;
            payload.push(&[constant::BLE_SUCCESS_CODE]);
            payload.push(topic_id.as_bytes());
        }
        Err(e) => {
            error_code = [e];
            payload.push(&error_code);
        }
    };
    notify_central(notifier, payload).await
}

async fn handle_keep_wifi(
    notifier: Arc<Mutex<Option<CharacteristicNotifier>>>,
    reply_id: String,
    cb: Arc<KeepWifiCallback>,
) -> Result<(), ReqError> {
    let mut payload = Vec::with_capacity(3);
    payload.push(reply_id.as_bytes());

    // Pre-declare variables to own the returned values
    let topic_id: String;
    let error_code: [u8; 1];
    match cb().await {
        Ok(tid) => {
            topic_id = tid;
            payload.push(&[constant::BLE_SUCCESS_CODE]);
            payload.push(topic_id.as_bytes());
        }
        Err(e) => {
            error_code = [e];
            payload.push(&error_code);
        }
    };
    notify_central(notifier, payload).await
}

async fn handle_get_info(
    notifier: Arc<Mutex<Option<CharacteristicNotifier>>>,
    reply_id: String,
    cb: Arc<GetInfoCallback>,
) -> Result<(), ReqError> {
    let payload = if let Some(cb) = cb.as_ref() {
        cb()
    } else {
        vec![]
    };
    let mut reply = Vec::with_capacity(payload.len() + 1);
    reply.push(reply_id.as_bytes());
    reply.push(&[constant::BLE_SUCCESS_CODE]);
    reply.extend(payload.iter().map(|s| s.as_bytes()));
    notify_central(notifier, reply).await
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
        [constant::BLE_ERR_CODE_UNKNOWN_ERROR]
    } else {
        [constant::BLE_SUCCESS_CODE]
    };
    let mut payload = Vec::with_capacity(3);
    payload.push(reply_id.as_bytes());
    payload.push(&status_code);
    notify_central(notifier, payload).await
}

async fn handle_submit_logs(
    notifier: Arc<Mutex<Option<CharacteristicNotifier>>>,
    reply_id: String,
    params: Vec<String>,
) -> Result<(), ReqError> {
    println!("BLE: Starting log submission process");
    println!("BLE: Reply ID: {reply_id}");

    // Expect userId, apiKey, and title
    if params.len() < 3 {
        eprintln!(
            "BLE: ERROR - Received submit logs payload with only {} values, expected at least 3",
            params.len()
        );

        println!("BLE: Creating error response for invalid parameters");
        let mut payload = Vec::with_capacity(2);
        payload.push(reply_id.as_bytes());
        let error_code = [constant::BLE_ERR_CODE_INVALID_PARAMS];
        payload.push(&error_code);

        println!("BLE: Notifying central with invalid params error");
        return notify_central(notifier, payload).await;
    }

    let user_id = &params[0];
    let api_key = &params[1];
    let title = &params[2];

    println!("BLE: Extracted parameters - User ID: {user_id}, Title: {title}");

    let mut payload = Vec::with_capacity(2);
    payload.push(reply_id.as_bytes());

    // Collect log files
    println!("BLE: Starting log file collection");
    let log_files = match log_uploader::collect_log_files().await {
        Ok(files) => files,
        Err(e) => {
            eprintln!("BLE: Failed to collect log files: {e}");
            let error_code = [constant::BLE_ERR_CODE_FILE_ERROR];
            payload.push(&error_code);
            return notify_central(notifier, payload).await;
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
    println!("BLE: Submitting for user: {user_id}");

    let result = log_uploader::submit_logs_to_api(user_id, api_key, body).await;

    let error_code: [u8; 1];
    match result {
        Ok(_response) => {
            payload.push(&[constant::BLE_SUCCESS_CODE]);
        }
        Err(e) => {
            eprintln!("BLE: ERROR - HTTP submission failed with error code: {e}",);
            error_code = [e];
            payload.push(&error_code);
        }
    };

    notify_central(notifier, payload).await
}

async fn notify_central(
    notifier: Arc<Mutex<Option<CharacteristicNotifier>>>,
    payload: Vec<&[u8]>,
) -> Result<(), ReqError> {
    println!("BLE: Notifying central with payload: {payload:?}");
    let mut guard = notifier.lock().await;
    if let Some(notifier) = guard.as_mut() {
        let payload = encoding::encode_payload(&payload);
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
