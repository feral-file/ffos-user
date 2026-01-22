use uuid::Uuid;

// Functional configuration
pub const SENTRY_URL: &str =
    "https://a1c5bd8607e7493634a05015d17d6aff@o142150.ingest.us.sentry.io/4509869844135936";
pub const CACHE_FILEPATH: &str = "/home/feralfile/.state/setupd";
pub const LOG_FILEDIR: &str = "/home/feralfile/.logs";
pub const TIMEZONE_CMD: &str = "/home/feralfile/scripts/feral-timesyncd.sh";
pub const LOG_UPLOAD_API: &str = "https://support.autonomy.io/v1/issues/";
pub const TIMEZONE_INSTRUCTION: &str = "set-time";
pub const SSID_CACHE_TTL: u64 = 10 * 60 * 1000; // 10 minutes
pub const WIFI_WEBAPP_DELAY: u64 = 3 * 1000; // 3 seconds
pub const INITIAL_INTERNET_CHECK_TIMEOUT: u64 = 5 * 1000; // 5 seconds
pub const AGGRESSIVE_INTERNET_CHECK_INTERVAL: u64 = 2 * 1000; // 2 seconds
pub const RELAXED_INTERNET_CHECK_INTERVAL: u64 = 10 * 1000; // 10 seconds
pub const WAIT_FOR_CONTROLD_TIMEOUT: u64 = 30 * 1000; // 30 seconds

// Updater configuration
pub const UPDATER_LOCAL_CONFIG_PATH: &str = "/home/feralfile/ff1-config.json";
pub const UPDATER_UPSTREAM_CONFIG_URL_SUFFIX: &str = "/api/latest/";
pub const UPDATER_PROCESS_LOG_FILE: &str = "/var/log/updaterd.log";
pub const UPDATER_FAILED_TO_CHECK_VERSION_MSG: &str =
    "Failed to check for updates, please try again later.";

// Bluetooth configuration
pub const SERVICE_UUID: Uuid = Uuid::from_u128(0xf7826da64fa24e988024bc5b71e0893e_u128);
pub const CMD_CHAR_UUID: Uuid = Uuid::from_u128(0x6e400002b5a3f393e0a9e50e24dcca9e_u128);
pub const CMD_CONNECT_WIFI: &str = "connect_wifi";
pub const CMD_SCAN_WIFI: &str = "scan_wifi";
pub const CMD_GET_INFO: &str = "get_info";
pub const CMD_SET_TIME: &str = "set_time";
pub const CMD_KEEP_WIFI: &str = "keep_wifi";
pub const CMD_FACTORY_RESET: &str = "factory_reset";
pub const CMD_SEND_LOGS: &str = "send_log";
pub const MAX_SSIDS: usize = 9;
// Bluetooth communication codes
pub const BLE_SUCCESS_CODE: u8 = 0;
pub const BLE_ERR_CODE_WRONG_WIFI_PWD: u8 = 1;
pub const BLE_ERR_CODE_NO_INTERNET: u8 = 2; // Used when user connects to wifi but no internet
pub const BLE_ERR_CODE_SERVER_UNREACHABLE: u8 = 3;
pub const BLE_ERR_CODE_WIFI_REQUIRED: u8 = 4; // Used when user doesn't have wifi but asks to proceed
pub const BLE_ERR_CODE_DEVICE_UPDATING: u8 = 5;
pub const BLE_ERR_CODE_VERSION_CHECK_FAILED: u8 = 6;
pub const BLE_ERR_CODE_INVALID_PARAMS: u8 = 7;
pub const BLE_ERR_CODE_NETWORK_ERROR: u8 = 9;
pub const BLE_ERR_CODE_VERSION_TOO_OLD: u8 = 10;
pub const BLE_ERR_CODE_UNKNOWN_ERROR: u8 = 255;

// Chrome configuration
pub const CDP_URL: &str = "http://127.0.0.1:9222/json";
pub const CDP_ID_START: u64 = 1_000_000;
pub const WEBAPP_URL: &str = "https://display.feralfile.com";
pub const QRCODE_URL_PREFIX: &str = "file:///opt/feral/ui/launcher/index.html?step=qr";
pub const MSG_URL_PREFIX: &str = "file:///opt/feral/ui/launcher/index.html?step=message&message=";
pub const WELCOME_MSG: &str = "Welcome to the FF1";
pub const WIFI_CONNECTING_MSG_PREFIX: &str = "Connecting to ";
pub const WIFI_FAILED_TO_CONNECT_MSG: &str =
    "Failed to connect to the wifi network, please try again.";
pub const INTERNET_FAILED_TO_CONNECT_MSG: &str =
    "Failed to connect to the internet, please try again.";
pub const UPDATING_MSG_PREFIX: &str = "Updating your FF1 to the version ";
pub const UPDATING_MSG_SUBTEXT: &str =
    "This process may take 5–10 minutes depending on your internet speed.";
pub const SETUP_SUCCESSFULLY_MSG: &str = "Bringing art to your screen…";
pub const FACTORY_RESET_MSG: &str = "Factory resetting…";
pub const REFLASHING_REQUIRED_MSG: &str = "We're sorry—we've moved too far ahead for this version to catch up. Your FF1 is too far behind to auto-upgrade. Scan the code below for step-by-step reflashing instructions, or contact us for help. support@feralfile.com";

// D-Bus configuration
pub const DBUS_CONTROLD_OBJECT: &str = "/com/feralfile/controld";
pub const DBUS_SYSMONITORD_OBJECT: &str = "/com/feralfile/sysmonitord";

pub const DBUS_SYSMONITORD_DESTINATION: &str = "com.feralfile.sysmonitord";
pub const DBUS_CONTROLD_DESTINATION: &str = "com.feralfile.controld";

pub const DBUS_CONTROLD_INTERFACE: &str = "com.feralfile.controld.general";
pub const DBUS_SYSMONITORD_INTERFACE: &str = "com.feralfile.sysmonitord";

// pub const DBUS_EVENT_WIFI_CONNECTED: &str = "wifi_connected";
// pub const DBUS_EVENT_RELAYER_CONFIGURED: &str = "relayer_configured";
pub const DBUS_EVENT_QRCODE_SWITCH: &str = "show_pairing_qr_code";
pub const DBUS_EVENT_FACTORY_RESET: &str = "factory_reset";
pub const DBUS_EVENT_UPLOAD_LOGS: &str = "upload_logs";
pub const DBUS_EVENT_SYSTEM_UPDATE: &str = "system_update";
pub const DBUS_CONNECTIVITY_METHOD: &str = "GetConnectivityStatus";
pub const DBUS_RELAYER_TOPIC_ID_METHOD: &str = "GetRelayerTopicID";

// pub const DBUS_CONTROLD_TIMEOUT: u64 = 30 * 1000; // 30 seconds
pub const DBUS_MAX_RETRIES: usize = 6;
pub const DBUS_ACK_TIMEOUT: u64 = 5 * 1000; // 5 seconds
pub const DBUS_INTERNET_CHECK_TIMEOUT: u64 = 1000; // 1 second
pub const DBUS_RELAYER_CHECK_TIMEOUT: u64 = 31 * 1000; // 31 seconds
pub const DBUS_LISTEN_WAKE_UP_INTERVAL: u64 = 1000; // 1 second
