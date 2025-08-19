use crate::constant;
use crate::encoding;
use crate::system;
use crate::wifi_utils::SSIDsCacher;
use base64::{Engine as _, engine::general_purpose};
use bluer::{
    Adapter, Session,
    adv::Advertisement,
    adv::AdvertisementHandle,
    gatt::local::{
        Application,
        ApplicationHandle,
        Characteristic,
        CharacteristicNotifier,
        // Add notify imports
        CharacteristicNotify,
        CharacteristicNotifyMethod,
        CharacteristicWrite,
        CharacteristicWriteMethod,
        ReqError,
        Service,
    },
};
use futures_util::future::FutureExt;
use serde_json::json;
use std::pin::Pin;
use std::sync::Arc;
use std::time::Instant;
use tokio::sync::Mutex;

use anyhow::Result;

pub type BTConnectedCallback =
    Option<Box<dyn Fn() -> Pin<Box<dyn Future<Output = ()> + Send>> + Send + Sync>>;
pub type ConnectWifiCallback = Box<
    dyn Fn(&str, &str) -> Pin<Box<dyn Future<Output = Result<String, u8>> + Send>> + Send + Sync,
>;
pub type KeepWifiCallback =
    Box<dyn Fn() -> Pin<Box<dyn Future<Output = Result<String, u8>> + Send>> + Send + Sync>;
pub type GetInfoCallback = Option<Box<dyn Fn() -> Vec<String> + Send + Sync>>;
pub type FactoryResetCallback =
    Option<Box<dyn Fn() -> Pin<Box<dyn Future<Output = ()> + Send>> + Send + Sync>>;

#[derive(Default)]
struct Inner {
    device_id: String,
    advertised: bool,
    adapter: Option<Adapter>,
    adv_handle: Option<AdvertisementHandle>,
    app_handle: Option<ApplicationHandle>,
}

pub struct Ble {
    inner: Mutex<Inner>,
}

impl Ble {
    pub fn new() -> Self {
        let device_id = encoding::get_device_id();
        Self {
            inner: Mutex::new(Inner {
                device_id,
                ..Default::default()
            }),
        }
    }

    pub async fn start(
        &self,
        bt_connected_cb: BTConnectedCallback,
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

        // Start advertising our service UUID
        let adv = Advertisement {
            service_uuids: vec![constant::SERVICE_UUID].into_iter().collect(),
            discoverable: Some(true),
            local_name: Some(inner.device_id.clone()),
            ..Default::default()
        };
        let adv_handle = adapter.advertise(adv).await?;
        println!("BLE: Advertising GATT service {}", constant::SERVICE_UUID);

        // Group into a GATT service and register it
        let svc = Service {
            uuid: constant::SERVICE_UUID,
            primary: true,
            characteristics: vec![
                self.create_cmd_char(
                    bt_connected_cb,
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
        println!("BLE: GATT app registered; ready to receive commands");

        inner.adapter = Some(adapter);
        inner.adv_handle = Some(adv_handle);
        inner.app_handle = Some(app_handle);
        inner.advertised = true;
        Ok(())
    }

    pub async fn stop(&self) -> Result<()> {
        let (adv, app, adapter) = {
            let mut inner = self.inner.lock().await;
            if !inner.advertised {
                return Ok(());
            }
            inner.advertised = false;
            (
                inner.adv_handle.take(),
                inner.app_handle.take(),
                inner.adapter.take(),
            )
        };
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
            tokio::time::sleep(std::time::Duration::from_millis(
                constant::BLE_SHUTDOWN_DELAY,
            ))
            .await;
        }
        Ok(())
    }

    pub async fn get_device_id(&self) -> String {
        let st = self.inner.lock().await;
        st.device_id.clone()
    }

    async fn create_cmd_char(
        &self,
        bt_connected_cb: BTConnectedCallback,
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

        let bt_connected_callback = Arc::new(bt_connected_cb);
        let factory_reset_callback = Arc::new(factory_reset_cb);
        let connect_wifi_callback = Arc::new(connect_wifi_cb);
        let keep_wifi_callback = Arc::new(keep_wifi_cb);
        let get_info_callback = Arc::new(get_info_cb);
        Characteristic {
            uuid: constant::CMD_CHAR_UUID,
            // Enable notifications on this characteristic
            notify: Some(CharacteristicNotify {
                notify: true,
                method: CharacteristicNotifyMethod::Fun(Box::new(move |notifier| {
                    println!("BLE: a device is connected");
                    let handle = notifier_for_notify.clone();
                    let bt_connected_callback = bt_connected_callback.clone();
                    async move {
                        // Store the notifier for later use in the write callback
                        *handle.lock().await = Some(notifier);
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
    println!("BLE: Received {} parameters", params.len());

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
    println!("BLE: API key length: {}", api_key.len());

    println!("BLE: Preparing response payload");
    let mut payload = Vec::with_capacity(2);
    payload.push(reply_id.as_bytes());

    // Collect log files
    println!("BLE: Starting log file collection");
    let log_files = match collect_log_files().await {
        Ok(files) => files,
        Err(e) => {
            eprintln!("BLE: Failed to collect log files: {e}");
            let error_code = [constant::BLE_ERR_CODE_FILE_ERROR];
            payload.push(&error_code);
            return notify_central(notifier, payload).await;
        }
    };

    // Create request body
    println!("BLE: Creating log submission request body");
    let body = create_log_submission_body(
        title,
        "Device log submission via BLE",
        vec!["device-logs".to_string(), "ble-submission".to_string()],
        log_files,
    );

    println!("BLE: Request body created successfully");
    // Submit logs via HTTP
    println!("BLE: Initiating HTTP submission to API");
    println!("BLE: Submitting for user: {user_id}");

    let result = submit_logs_to_api(user_id, api_key, body).await;

    let error_code: [u8; 1];
    match result {
        Ok(response) => {
            println!("BLE: HTTP submission completed successfully");
            println!("BLE: API response received");
            println!("BLE: Adding success code to payload");
            payload.push(&[constant::BLE_SUCCESS_CODE]);
        }
        Err(e) => {
            eprintln!("BLE: ERROR - HTTP submission failed with error code: {e}",);
            println!("BLE: Creating error response for API submission failure");
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
        match notifier.notify(payload).await {
            Ok(_) => (),
            Err(e) => {
                eprintln!("BLE: Failed to notify central: {e}");
            }
        }
    } else {
        eprintln!("BLE: Notifier not yet available; skipping reply");
    }
    Ok(())
}

// Helper function to collect log files
pub async fn collect_log_files() -> Result<Vec<(String, Vec<u8>)>, std::io::Error> {
    use tokio::fs;

    let logs_dir = constant::LOG_FILEDIR;
    let mut log_files = Vec::new();

    let mut dir = fs::read_dir(logs_dir).await?;
    while let Some(entry) = dir.next_entry().await? {
        let path = entry.path();
        if path.extension().and_then(|s| s.to_str()) == Some("log") {
            let file_name = path
                .file_name()
                .and_then(|s| s.to_str())
                .unwrap_or("unknown.log")
                .to_string();

            match fs::read(&path).await {
                Ok(contents) => {
                    println!("BLE: Collected log file: {file_name}");
                    log_files.push((file_name, contents));
                }
                Err(e) => {
                    eprintln!("BLE: Failed to read log file {file_name}: {e}");
                }
            }
        }
    }

    Ok(log_files)
}

pub async fn submit_logs_to_api(
    user_id: &str,
    api_key: &str,
    body: serde_json::Value,
) -> Result<(), u8> {
    use reqwest;

    println!("API: Starting log submission to remote API");

    // Log request body size
    if let Ok(serialized_body) = serde_json::to_string(&body) {
        println!("API: Request body size: {} bytes", serialized_body.len());
    }

    let client = reqwest::Client::new();

    println!("API: Building POST request");

    let request_builder = client
        .post(constant::LOG_UPLOAD_API)
        .header("x-api-key", api_key)
        .header("x-device-id", user_id)
        .header("Content-Type", "application/json")
        .json(&body);

    println!("API: Sending HTTP request...");
    let start_time = std::time::Instant::now();

    let response = request_builder.send().await;
    let elapsed = start_time.elapsed();

    println!("API: HTTP request completed in {elapsed:?}");

    match response {
        Ok(resp) => {
            let status = resp.status();
            println!("API: Received HTTP response with status: {status}");

            if status.is_success() {
                println!("API: SUCCESS - Logs submitted successfully");
                println!("API: Response headers: {:?}", resp.headers());
                Ok(())
            } else {
                println!("API: ERROR - HTTP request failed with status: {status}");

                // Try to get response body for debugging
                match resp.text().await {
                    Ok(response_text) => {
                        eprintln!("API: Error response body: {response_text}");
                        println!("API: Failed to submit logs: HTTP {status}, {response_text}");
                    }
                    Err(body_err) => {
                        eprintln!("API: Failed to read error response body: {body_err}");
                        eprintln!("API: Failed to submit logs: HTTP {status}");
                    }
                }

                println!("API: Returning network error code");
                Err(constant::BLE_ERR_CODE_NETWORK_ERROR)
            }
        }
        Err(e) => {
            eprintln!("API: ERROR - Network error occurred: {e}");

            // More detailed error information
            if e.is_timeout() {
                eprintln!("API: Error type: Request timeout");
            } else if e.is_connect() {
                eprintln!("API: Error type: Connection error");
            } else if e.is_request() {
                eprintln!("API: Error type: Request building error");
            } else {
                eprintln!("API: Error type: Other network error");
            }

            println!("API: Returning network error code");
            Err(constant::BLE_ERR_CODE_NETWORK_ERROR)
        }
    }
}

pub fn create_log_submission_body(
    title: &str,
    message: &str,
    tags: Vec<String>,
    log_files: Vec<(String, Vec<u8>)>,
) -> serde_json::Value {
    let attachments: Vec<serde_json::Value> = log_files
        .into_iter()
        .map(|(file_name, data)| {
            json!({
                "title": file_name,
                "data": general_purpose::STANDARD.encode(data)
            })
        })
        .collect();

    let result = json!({
        "attachments": attachments,
        "title": title,
        "message": message,
        "tags": tags
    });

    println!("Log submission body: {:#}", result);
    
    result
}
