use crate::constant;
use crate::encoding;
use crate::system;
use crate::wifi_utils::SSIDsCacher;
use bluer::{
    Adapter, Session, SessionEvent,
    adv::Advertisement,
    adv::AdvertisementHandle,
    gatt::local::{
        Application, ApplicationHandle, Characteristic, CharacteristicNotifier,
        CharacteristicNotify, CharacteristicNotifyMethod, CharacteristicWrite,
        CharacteristicWriteMethod, ReqError, Service,
    },
};
use futures_util::{future::FutureExt, stream::StreamExt};
use std::pin::Pin;
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::{Arc, Weak};
use std::time::{Duration, Instant};
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
    VersionTooOld = constant::BLE_ERR_CODE_VERSION_TOO_OLD,
    InvalidParams = constant::BLE_ERR_CODE_INVALID_PARAMS,
    UnknownError = constant::BLE_ERR_CODE_UNKNOWN_ERROR,
}

impl BleStatus {
    pub fn code(self) -> u8 {
        self as u8
    }
}

type AsyncUnit = Pin<Box<dyn Future<Output = ()> + Send>>;
type AsyncBleResult = Pin<Box<dyn Future<Output = Result<String, BleStatus>> + Send>>;

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

pub type BTConnectedCallback = Option<Box<dyn Fn() -> AsyncUnit + Send + Sync>>;
pub type BTDisconnectedCallback = Option<Box<dyn Fn() -> AsyncUnit + Send + Sync>>;
pub type ConnectWifiCallback = Box<dyn Fn(&str, &str) -> AsyncBleResult + Send + Sync>;
pub type KeepWifiCallback = Box<dyn Fn() -> AsyncBleResult + Send + Sync>;
pub type GetInfoCallback = Option<Box<dyn Fn() -> Vec<String> + Send + Sync>>;
pub type FactoryResetCallback = Option<Box<dyn Fn() -> AsyncUnit + Send + Sync>>;
pub type SubmitLogsCallback =
    Box<dyn Fn(&str, &str, &str, Option<&str>) -> AsyncUnit + Send + Sync>;

pub struct BleCallbacks {
    pub bt_connected: BTConnectedCallback,
    pub bt_disconnected: BTDisconnectedCallback,
    pub factory_reset: FactoryResetCallback,
    pub submit_logs: SubmitLogsCallback,
    pub connect_wifi: ConnectWifiCallback,
    pub keep_wifi: KeepWifiCallback,
    pub get_info: GetInfoCallback,
}

/// Delay between dropping BlueZ registrations and re-registering, so BlueZ finishes the
/// asynchronous teardown first (controllers with a single advertising slot reject a second
/// instance while the old one is still being unregistered).
const RECOVERY_SETTLE_DELAY: Duration = Duration::from_millis(500);

/// Capped exponential backoff between GATT recovery attempts: 1 s, 2 s, 4 s, ... up to 30 s.
/// Recovery retries forever (until `stop()`); a transiently failing BlueZ must never
/// permanently kill the pairing surface, because only a power cycle would revive it.
fn recovery_backoff(attempt: u32) -> Duration {
    const CAP: Duration = Duration::from_secs(30);
    let secs = 1u64 << attempt.saturating_sub(1).min(5);
    CAP.min(Duration::from_secs(secs))
}

/// How a GATT recovery trigger treats an apparently subscribed central.
#[derive(Copy, Clone, Debug, Eq, PartialEq)]
enum RecoveryKind {
    /// Disconnect-monitor trigger: skip if a central is (still) subscribed, since a live
    /// subscription proves the service table is being served and tearing it down would kill
    /// that central's in-flight setup session.
    BestEffort,
    /// Adapter-watchdog trigger: never skip. A re-added adapter has lost our registrations
    /// with certainty, and after a bluetoothd restart BlueZ can no longer deliver StopNotify,
    /// so a retained notifier keeps claiming "subscribed" forever. Honoring that stale
    /// liveness signal would gate out the one recovery that can fix the empty-service-table
    /// wedge (ff-app #556).
    Forced,
}

/// Pure decision: whether a recovery trigger should be skipped because a central appears
/// subscribed. Factored out so the gating logic is unit-testable without a BlueZ session.
fn should_skip_recovery(kind: RecoveryKind, central_subscribed: bool) -> bool {
    kind == RecoveryKind::BestEffort && central_subscribed
}

/// Long-lived materials for (re)building the command characteristic and re-registering the
/// GATT application.
///
/// BlueZ forgets every registration a client made when bluetoothd restarts or the Bluetooth
/// controller resets/re-enumerates. The callbacks and shared per-connection state are
/// therefore retained for the daemon's whole lifetime so the application can be re-registered
/// at any point — not just at startup. Without this, only the advertisement could be
/// re-created after such an event, leaving the device connectable but with an empty GATT
/// service table until power cycle (ff-app #556).
struct GattRuntime {
    /// Reply channel to the currently subscribed central (replaced on every new subscribe,
    /// cleared when the disconnect monitor observes the session stopped).
    notifier: Arc<Mutex<Option<CharacteristicNotifier>>>,
    /// Cancellation token of the active disconnect monitor.
    monitor_cancel: Arc<Mutex<Option<CancellationToken>>>,
    bt_connected: Arc<BTConnectedCallback>,
    bt_disconnected: Arc<BTDisconnectedCallback>,
    factory_reset: Arc<FactoryResetCallback>,
    submit_logs: Arc<SubmitLogsCallback>,
    connect_wifi: Arc<ConnectWifiCallback>,
    keep_wifi: Arc<KeepWifiCallback>,
    get_info: Arc<GetInfoCallback>,
    ssids_cacher: Arc<SSIDsCacher>,
}

impl GattRuntime {
    fn new(callbacks: BleCallbacks, ssids_cacher: Arc<SSIDsCacher>) -> Self {
        let BleCallbacks {
            bt_connected,
            bt_disconnected,
            factory_reset,
            submit_logs,
            connect_wifi,
            keep_wifi,
            get_info,
        } = callbacks;
        Self {
            notifier: Arc::new(Mutex::new(None)),
            monitor_cancel: Arc::new(Mutex::new(None)),
            bt_connected: Arc::new(bt_connected),
            bt_disconnected: Arc::new(bt_disconnected),
            factory_reset: Arc::new(factory_reset),
            submit_logs: Arc::new(submit_logs),
            connect_wifi: Arc::new(connect_wifi),
            keep_wifi: Arc::new(keep_wifi),
            get_info: Arc::new(get_info),
            ssids_cacher,
        }
    }
}

#[derive(Default)]
struct Inner {
    device_id: String,
    advertised: bool,
    session: Option<Session>,
    adapter: Option<Adapter>,
    adv_handle: Option<AdvertisementHandle>,
    app_handle: Option<ApplicationHandle>,
    runtime: Option<Arc<GattRuntime>>,
}

pub struct Ble {
    inner: Mutex<Inner>,
    /// Deduplicates concurrent recovery triggers (disconnect monitor + adapter watchdog).
    recovery_in_flight: AtomicBool,
}

impl Ble {
    pub fn new() -> Self {
        let device_id = system::get_device_id();
        Self {
            inner: Mutex::new(Inner {
                device_id,
                ..Default::default()
            }),
            recovery_in_flight: AtomicBool::new(false),
        }
    }

    pub async fn start(
        self: &Arc<Self>,
        callbacks: BleCallbacks,
        ssids_cacher: Arc<SSIDsCacher>,
    ) -> Result<()> {
        let ble_handle = Arc::downgrade(self);
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

        let runtime = Arc::new(GattRuntime::new(callbacks, ssids_cacher));

        // Group into a GATT service and register it
        let app_handle =
            Self::register_gatt_application(&adapter, ble_handle.clone(), &runtime).await?;

        tokio::time::sleep(RECOVERY_SETTLE_DELAY).await;

        println!("BLE: GATT app registered; ready to receive commands");

        // Start advertising our service UUID
        let adv = Self::build_advertisement(&inner.device_id);
        let adv_handle = adapter.advertise(adv).await?;
        println!("BLE: Advertising GATT service {}", constant::SERVICE_UUID);

        let adapter_name = adapter.name().to_string();
        inner.session = Some(session.clone());
        inner.adapter = Some(adapter);
        inner.adv_handle = Some(adv_handle);
        inner.app_handle = Some(app_handle);
        inner.runtime = Some(runtime);
        inner.advertised = true;
        drop(inner);

        // Watch for the adapter disappearing/reappearing (bluetoothd restart or controller
        // re-enumeration). Both wipe our BlueZ registrations, so a re-added adapter must
        // trigger a full GATT re-registration or the device would keep advertising an empty
        // service table.
        self.spawn_adapter_watchdog(session, adapter_name);
        Ok(())
    }

    pub async fn stop(&self) -> Result<()> {
        let (adv, app, adapter, session, runtime) = {
            let mut inner = self.inner.lock().await;
            if !inner.advertised {
                return Ok(());
            }
            inner.advertised = false;
            (
                inner.adv_handle.take(),
                inner.app_handle.take(),
                inner.adapter.take(),
                inner.session.take(),
                inner.runtime.take(),
            )
        };

        // Cancel the active disconnect monitor (if any) so it cannot trigger a recovery
        // mid-shutdown. The token lives in the runtime's shared storage — the same slot the
        // notify handler fills on every new connection.
        if let Some(runtime) = &runtime {
            if let Some(token) = runtime.monitor_cancel.lock().await.take() {
                println!("BLE: Cancelling disconnect monitor");
                token.cancel();
            }
        }

        // Wait a moment for the monitor to acknowledge cancellation
        tokio::time::sleep(Duration::from_millis(100)).await;

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
            tokio::time::sleep(Duration::from_millis(2000)).await;
        }

        // Finally drop the session and callback runtime
        drop(session);
        drop(runtime);
        println!("BLE: All resources cleaned up");
        Ok(())
    }

    pub async fn get_device_id(&self) -> String {
        let st = self.inner.lock().await;
        st.device_id.clone()
    }

    async fn is_advertised(&self) -> bool {
        self.inner.lock().await.advertised
    }

    // --- GATT stack recovery ---

    /// Watches BlueZ adapter lifecycle events for the adapter we registered on.
    ///
    /// A bluetoothd restart or a controller reset/re-enumeration surfaces as
    /// `AdapterRemoved` + `AdapterAdded` and silently wipes both our GATT application and
    /// advertisement registrations. Re-registering on `AdapterAdded` makes the pairing
    /// surface self-healing instead of requiring a power cycle.
    fn spawn_adapter_watchdog(self: &Arc<Self>, session: Session, adapter_name: String) {
        let ble_weak = Arc::downgrade(self);
        tokio::spawn(async move {
            let events = match session.events().await {
                Ok(events) => events,
                Err(e) => {
                    eprintln!(
                        "BLE: Failed to subscribe to adapter events; \
                         no automatic recovery after bluetoothd restarts: {e}"
                    );
                    return;
                }
            };
            // Drop our session clone: the stream stays alive through the session stored in
            // `Inner` and ends when `stop()` drops it, cleanly terminating this task. If
            // bluer ever kept the subscription alive past that, the worst case is one idle
            // task parked on next() until process exit — it re-checks `advertised` before
            // acting on any event, so it can never trigger recovery after stop().
            drop(session);
            let mut events = Box::pin(events);

            while let Some(event) = events.next().await {
                let Some(ble) = ble_weak.upgrade() else {
                    return;
                };
                if !ble.is_advertised().await {
                    println!("BLE: Adapter watchdog exiting - service stopped");
                    return;
                }
                match event {
                    SessionEvent::AdapterRemoved(name) if name == adapter_name => {
                        eprintln!(
                            "BLE: Adapter {name} removed \
                             (bluetoothd restart or controller reset)"
                        );
                        // The adapter's death killed every connection, but a dead bluetoothd
                        // can no longer deliver StopNotify, so a retained notifier would
                        // keep reporting "subscribed" forever. Invalidate it now so neither
                        // the AdapterAdded recovery below nor the disconnect monitor is
                        // gated by that stale liveness signal.
                        if let Some(runtime) = ble.runtime().await {
                            runtime.notifier.lock().await.take();
                        }
                    }
                    SessionEvent::AdapterAdded(name) if name == adapter_name => {
                        println!("BLE: Adapter {name} re-added; scheduling GATT recovery");
                        // Forced: the re-added adapter has lost our registrations with
                        // certainty, so the "central is subscribed" skip must not apply
                        // (see should_skip_recovery for the stale-liveness hazard).
                        ble.spawn_recovery("adapter re-added", RecoveryKind::Forced);
                    }
                    _ => {}
                }
            }
            println!("BLE: Adapter event stream ended; watchdog stopped");
        });
    }

    async fn runtime(&self) -> Option<Arc<GattRuntime>> {
        self.inner.lock().await.runtime.clone()
    }

    /// Triggers an asynchronous full GATT-stack recovery (deduplicated across triggers).
    ///
    /// Called when a central disconnects and when the adapter reappears. Retries with capped
    /// exponential backoff until recovery succeeds or the service is stopped.
    ///
    /// Dedup can drop a trigger that races an in-flight recovery; convergence still holds
    /// because (a) the in-flight retry loop runs until a full recovery succeeds, (b) an
    /// `AdapterRemoved` always clears the notifier before the `Forced` trigger fires, so a
    /// concurrent recovery cannot skip on stale liveness, and (c) the disconnect monitor
    /// keeps ticking and re-triggers on a cleared notifier slot.
    fn spawn_recovery(self: &Arc<Self>, reason: &str, kind: RecoveryKind) {
        if self.recovery_in_flight.swap(true, Ordering::AcqRel) {
            println!("BLE: Recovery already in flight; ignoring trigger ({reason})");
            return;
        }
        let ble = self.clone();
        let reason = reason.to_string();
        tokio::spawn(async move {
            let mut attempt: u32 = 0;
            loop {
                attempt += 1;
                match ble.recover_gatt_stack(&reason, kind).await {
                    Ok(()) => break,
                    Err(e) => {
                        eprintln!("BLE: GATT recovery attempt {attempt} failed ({reason}): {e:#}");
                        if !ble.is_advertised().await {
                            break;
                        }
                        tokio::time::sleep(recovery_backoff(attempt)).await;
                    }
                }
            }
            ble.recovery_in_flight.store(false, Ordering::Release);
        });
    }

    /// Re-registers the FULL GATT stack: application (service table) AND advertisement.
    ///
    /// This deliberately replaces the previous advertisement-only restart. If BlueZ lost our
    /// registrations while a central was connected (bluetoothd restart, controller reset
    /// while nmcli switches Wi-Fi networks during setup), restarting only the advertisement
    /// resurrects a connectable device with an EMPTY service table: centrals connect
    /// instantly, discover zero services, and stay wedged until power cycle (ff-app #556).
    /// Dropping and re-registering both is cheap, idempotent from BlueZ's point of view, and
    /// (for `BestEffort` triggers) only runs while no central is subscribed.
    ///
    /// Holds the `inner` mutex for the full duration (a few seconds of D-Bus round-trips and
    /// settle delays). This is intentional: it serializes recovery against `start()`/`stop()`
    /// and concurrent recoveries; callers of `is_advertised`/`get_device_id` simply block
    /// briefly.
    async fn recover_gatt_stack(self: &Arc<Self>, reason: &str, kind: RecoveryKind) -> Result<()> {
        let ble_handle = Arc::downgrade(self);
        let mut inner = self.inner.lock().await;

        // If stop() has been called, advertised will be false, so we should not recover
        if !inner.advertised {
            println!("BLE: Skipping GATT recovery ({reason}) - service has been stopped");
            return Ok(());
        }
        let Some(runtime) = inner.runtime.clone() else {
            println!("BLE: Skipping GATT recovery ({reason}) - runtime not available");
            return Ok(());
        };

        {
            let mut guard = runtime.notifier.lock().await;
            let central_subscribed = guard.as_ref().is_some_and(|n| !n.is_stopped());
            if should_skip_recovery(kind, central_subscribed) {
                println!(
                    "BLE: Skipping GATT recovery ({reason}) - a central is connected \
                     and subscribed"
                );
                return Ok(());
            }
            // A forced recovery is about to tear the registration down regardless; any
            // retained notifier is unusable by definition, so drop it to keep late replies
            // from targeting a dead session.
            if kind == RecoveryKind::Forced {
                guard.take();
            }
        }

        let Some(adapter) = inner.adapter.clone() else {
            anyhow::bail!("adapter not available");
        };

        println!("BLE: Recovering GATT stack ({reason})...");

        // Re-assert power: a re-initialized adapter may come back unpowered.
        if let Err(e) = adapter.set_powered(true).await {
            eprintln!("BLE: Failed to power adapter during recovery: {e}");
        }

        // Force disconnect stale connections before restarting. This is especially important
        // for iOS devices which may leave connections in a "hanging" state that would block
        // advertising registration.
        Self::force_disconnect_all_devices(&adapter).await;

        // Drop BOTH registrations. If BlueZ still has them this unregisters cleanly; if
        // BlueZ already lost them the background unregister calls fail harmlessly. Either
        // way we can then register from scratch.
        drop(inner.app_handle.take());
        drop(inner.adv_handle.take());

        // bluer unregisters asynchronously; give BlueZ a moment so re-registration does not
        // race the teardown.
        tokio::time::sleep(RECOVERY_SETTLE_DELAY).await;

        let app_handle = Self::register_gatt_application(&adapter, ble_handle, &runtime).await?;
        // Store immediately: if advertising below fails, the registered app survives and the
        // retry loop drops + re-registers it on the next attempt.
        inner.app_handle = Some(app_handle);
        println!("BLE: GATT app re-registered");

        let adv = Self::build_advertisement(&inner.device_id);
        let adv_handle = adapter.advertise(adv).await?;
        inner.adv_handle = Some(adv_handle);
        println!("BLE: GATT stack recovered; advertising restarted ({reason})");
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
        tokio::time::sleep(Duration::from_millis(500)).await;
    }

    // --- Internal helpers ---

    fn build_cmd_char(me: Weak<Self>, runtime: &Arc<GattRuntime>) -> Characteristic {
        Characteristic {
            uuid: constant::CMD_CHAR_UUID,
            // Enable notifications on this characteristic
            notify: Some(Self::make_cmd_notify_handler(
                me,
                runtime.notifier.clone(),
                runtime.monitor_cancel.clone(),
                runtime.bt_connected.clone(),
                runtime.bt_disconnected.clone(),
            )),
            write: Some(Self::make_cmd_write_handler(
                runtime.notifier.clone(),
                runtime.connect_wifi.clone(),
                runtime.factory_reset.clone(),
                runtime.submit_logs.clone(),
                runtime.keep_wifi.clone(),
                runtime.get_info.clone(),
                runtime.ssids_cacher.clone(),
            )),
            ..Default::default()
        }
    }

    /// Builds the notification handler for the command characteristic.
    ///
    /// Responsibilities:
    /// - Stores the latest notifier handle for future replies.
    /// - Cancels any existing disconnect monitor when a new device connects.
    /// - Starts a fresh disconnect monitor tied to the new connection.
    /// - Kicks off the "BT connected" callback in the background (CDP navigation must not
    ///   delay the StartNotify D-Bus reply, or BlueZ fails the central's subscribe).
    fn make_cmd_notify_handler(
        ble_handle: Weak<Self>,
        notifier: Arc<Mutex<Option<CharacteristicNotifier>>>,
        cancel_storage: Arc<Mutex<Option<CancellationToken>>>,
        bt_connected_callback: Arc<BTConnectedCallback>,
        bt_disconnected_callback: Arc<BTDisconnectedCallback>,
    ) -> CharacteristicNotify {
        CharacteristicNotify {
            notify: true,
            method: CharacteristicNotifyMethod::Fun(Box::new(move |new_notifier| {
                println!("BLE: a device is connected");
                let notifier = notifier.clone();
                let bt_connected_callback = bt_connected_callback.clone();
                let bt_disconnected_callback = bt_disconnected_callback.clone();
                let cancel_storage = cancel_storage.clone();
                let ble_handle = ble_handle.clone();

                async move {
                    // Cancel any existing disconnect monitor from previous connection.
                    // Cancellation is cooperative, so an old monitor may overlap the new one
                    // for up to a tick; that is harmless because a spurious recovery trigger
                    // is deduplicated and skips itself once it sees the new central
                    // subscribed (see should_skip_recovery).
                    {
                        let mut old_token = cancel_storage.lock().await;
                        if let Some(token) = old_token.take() {
                            println!("BLE: Cancelling previous disconnect monitor");
                            token.cancel();
                            // Give the old monitor a moment to stop
                            tokio::time::sleep(Duration::from_millis(100)).await;
                        }
                    }

                    // Replace the old notifier with the new one
                    {
                        let mut old_notifier = notifier.lock().await;
                        if old_notifier.is_some() {
                            println!("BLE: Replacing previous notifier");
                        }
                        *old_notifier = Some(new_notifier);
                    }

                    // Create a new cancellation token for this connection
                    let cancel_token = CancellationToken::new();
                    *cancel_storage.lock().await = Some(cancel_token.clone());

                    // Start new disconnect monitor
                    tokio::spawn(Self::start_disconnect_monitor(
                        ble_handle,
                        notifier.clone(),
                        bt_disconnected_callback,
                        cancel_token,
                    ));

                    // Run the connected callback in the background: it navigates the TV via
                    // CDP and a slow or wedged Chromium must not block the subscribe path.
                    tokio::spawn(async move {
                        if let Some(cb) = bt_connected_callback.as_ref() {
                            cb().await;
                        }
                    });
                }
                .boxed()
            })),
            ..Default::default()
        }
    }

    /// Builds the write handler for the command characteristic.
    ///
    /// Responsibilities:
    /// - Parses and validates the incoming BLE payload.
    /// - Decodes the command name, reply ID, and parameters.
    /// - Dispatches to the appropriate handler (`scan_wifi`, `connect_wifi`, etc.).
    /// - Logs malformed or unknown commands but always returns a graceful `Ok(())`.
    ///
    /// Slow commands (Wi-Fi scan/connect/keep, factory reset) are spawned so the ATT write
    /// is acknowledged immediately: BlueZ delivers WriteValue over D-Bus and fails the
    /// central's write on its method timeout (~25 s), while nmcli + the connectivity wait +
    /// the version check can run far longer. The mobile app never waited on the write
    /// acknowledgement anyway — results are delivered via the `reply_id` notification
    /// (the same pattern `send_log` already used).
    fn make_cmd_write_handler(
        notifier_for_write: Arc<Mutex<Option<CharacteristicNotifier>>>,
        connect_wifi_callback: Arc<ConnectWifiCallback>,
        factory_reset_callback: Arc<FactoryResetCallback>,
        submit_logs_callback: Arc<SubmitLogsCallback>,
        keep_wifi_callback: Arc<KeepWifiCallback>,
        get_info_callback: Arc<GetInfoCallback>,
        ssids_cacher: Arc<SSIDsCacher>,
    ) -> CharacteristicWrite {
        CharacteristicWrite {
            write: true,
            write_without_response: false,
            method: CharacteristicWriteMethod::Fun(Box::new(move |data, _req| {
                let notifier = notifier_for_write.clone();
                let connect_wifi_callback = connect_wifi_callback.clone();
                let factory_reset_callback = factory_reset_callback.clone();
                let submit_logs_callback = submit_logs_callback.clone();
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
                            tokio::spawn(async move {
                                let _ = handle_scan_wifi(notifier, reply_id, ssids_cacher).await;
                            });
                            Ok(())
                        }
                        BleCommand::ConnectWifi => {
                            tokio::spawn(async move {
                                let _ = handle_connect_wifi(
                                    notifier,
                                    reply_id,
                                    params,
                                    connect_wifi_callback,
                                )
                                .await;
                            });
                            Ok(())
                        }
                        BleCommand::KeepWifi => {
                            tokio::spawn(async move {
                                let _ =
                                    handle_keep_wifi(notifier, reply_id, keep_wifi_callback).await;
                            });
                            Ok(())
                        }
                        BleCommand::FactoryReset => {
                            tokio::spawn(async move {
                                let _ = handle_factory_reset(
                                    notifier,
                                    reply_id,
                                    factory_reset_callback,
                                )
                                .await;
                            });
                            Ok(())
                        }
                        BleCommand::GetInfo => {
                            handle_get_info(notifier, reply_id, get_info_callback).await
                        }
                        BleCommand::SetTime => handle_set_time(notifier, reply_id, params).await,
                        BleCommand::SendLogs => {
                            handle_submit_logs(notifier, reply_id, params, submit_logs_callback)
                                .await
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
        }
    }

    async fn start_disconnect_monitor(
        ble_handle: Weak<Self>,
        notifier: Arc<Mutex<Option<CharacteristicNotifier>>>,
        bt_disconnected_cb: Arc<BTDisconnectedCallback>,
        cancel_token: CancellationToken,
    ) {
        println!("BLE: Starting disconnect monitor for connected device");

        let mut check_interval = tokio::time::interval(Duration::from_secs(1));

        loop {
            tokio::select! {
                _ = cancel_token.cancelled() => {
                    println!("BLE: Disconnect monitor cancelled");
                    break;
                }
                _ = check_interval.tick() => {
                    let is_disconnected = {
                        let mut guard = notifier.lock().await;
                        match guard.as_mut() {
                            Some(notifier) if notifier.is_stopped() => {
                                // Clear the dead reply channel so late replies are skipped
                                // instead of erroring against a torn-down session. A new
                                // connection installs its own notifier before this monitor
                                // is replaced, and the re-check under the lock ensures we
                                // never clear a live one.
                                *guard = None;
                                true
                            }
                            Some(_) => false,
                            None => true,
                        }
                    };

                    if is_disconnected {
                        println!("BLE: Device disconnected (session stopped)");

                        // Step 1: Recover the full GATT stack (application + advertisement)
                        // in the background. See recover_gatt_stack for why re-registering
                        // only the advertisement is not enough. The recovery no-ops if a new
                        // central has already connected and subscribed.
                        if let Some(ble) = ble_handle.upgrade() {
                            println!("BLE: Scheduling GATT stack recovery...");
                            ble.spawn_recovery("central disconnected", RecoveryKind::BestEffort);
                        } else {
                            println!("BLE: Ble instance dropped, cannot recover GATT stack");
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
        adapter: &Adapter,
        me: Weak<Self>,
        runtime: &Arc<GattRuntime>,
    ) -> Result<ApplicationHandle> {
        let svc = Service {
            uuid: constant::SERVICE_UUID,
            primary: true,
            characteristics: vec![Self::build_cmd_char(me, runtime)],
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
            min_interval: Some(Duration::from_millis(20)),
            max_interval: Some(Duration::from_millis(100)),
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
    _params: Vec<String>,
) -> Result<(), ReqError> {
    println!("Do nothing");
    Ok(())
}

async fn handle_factory_reset(
    notifier: Arc<Mutex<Option<CharacteristicNotifier>>>,
    reply_id: String,
    cb: Arc<FactoryResetCallback>,
) -> Result<(), ReqError> {
    println!("BLE: Factory resetting");
    // Callback handles both showing the page and executing the reset
    if let Some(cb) = cb.as_ref() {
        cb().await;
    }
    // Always return success since the callback handles the reset
    // The device will reboot anyway if successful
    let status_code = [BleStatus::Success.code()];
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
    cb: Arc<SubmitLogsCallback>,
) -> Result<(), ReqError> {
    println!("BLE log-submit: start reply_id={reply_id}");

    // Expect userId, apiKey, title, and optionally support_bundle_id.
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

    let user_id = params[0].clone();
    let api_key = params[1].clone();
    let title = params[2].clone();
    let support_bundle_id = params
        .get(3)
        .map(String::as_str)
        .map(str::trim)
        .filter(|value| !value.is_empty());

    println!(
        "BLE log-submit: user={user_id} title={title} support_bundle_id={}",
        support_bundle_id.unwrap_or("")
    );

    // Trigger the callback without waiting for completion
    let cb_future = cb(&user_id, &api_key, &title, support_bundle_id);
    tokio::spawn(async move {
        cb_future.await;
    });

    // Return success immediately
    let mut encoder = encoding::PayloadEncoder::new();
    encoder.push_str(&reply_id);
    encoder.push_code(BleStatus::Success.code());

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

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn recovery_backoff_grows_exponentially_and_caps() {
        assert_eq!(recovery_backoff(1), Duration::from_secs(1));
        assert_eq!(recovery_backoff(2), Duration::from_secs(2));
        assert_eq!(recovery_backoff(3), Duration::from_secs(4));
        assert_eq!(recovery_backoff(5), Duration::from_secs(16));
        // Attempt 6 would be 32 s; capped at 30 s and stays there forever.
        assert_eq!(recovery_backoff(6), Duration::from_secs(30));
        assert_eq!(recovery_backoff(100), Duration::from_secs(30));
        // Defensive: attempt 0 (should not happen) must not underflow.
        assert_eq!(recovery_backoff(0), Duration::from_secs(1));
    }

    #[test]
    fn recovery_gating_skips_only_best_effort_with_live_central() {
        // Disconnect-triggered recovery must not tear down a session a new central is using.
        assert!(should_skip_recovery(RecoveryKind::BestEffort, true));
        assert!(!should_skip_recovery(RecoveryKind::BestEffort, false));
        // Adapter-re-added recovery must run even if a retained notifier still claims a
        // central is subscribed: after a bluetoothd restart StopNotify can never arrive, so
        // that signal is stale and honoring it would leave the empty-service-table wedge in
        // place (ff-app #556).
        assert!(!should_skip_recovery(RecoveryKind::Forced, true));
        assert!(!should_skip_recovery(RecoveryKind::Forced, false));
    }

    #[test]
    fn ble_status_codes_match_mobile_protocol() {
        // These codes are a wire contract with the mobile app; they must never drift.
        assert_eq!(BleStatus::Success.code(), 0);
        assert_eq!(BleStatus::WrongWifiPassword.code(), 1);
        assert_eq!(BleStatus::NoInternet.code(), 2);
        assert_eq!(BleStatus::ServerUnreachable.code(), 3);
        assert_eq!(BleStatus::WifiRequired.code(), 4);
        assert_eq!(BleStatus::DeviceUpdating.code(), 5);
        assert_eq!(BleStatus::VersionCheckFailed.code(), 6);
        assert_eq!(BleStatus::InvalidParams.code(), 7);
        assert_eq!(BleStatus::VersionTooOld.code(), 10);
        assert_eq!(BleStatus::UnknownError.code(), 255);
    }

    #[test]
    fn ble_command_parses_all_known_commands() {
        assert!(matches!(
            BleCommand::from_str(constant::CMD_SCAN_WIFI),
            BleCommand::ScanWifi
        ));
        assert!(matches!(
            BleCommand::from_str(constant::CMD_CONNECT_WIFI),
            BleCommand::ConnectWifi
        ));
        assert!(matches!(
            BleCommand::from_str(constant::CMD_KEEP_WIFI),
            BleCommand::KeepWifi
        ));
        assert!(matches!(
            BleCommand::from_str(constant::CMD_GET_INFO),
            BleCommand::GetInfo
        ));
        assert!(matches!(
            BleCommand::from_str(constant::CMD_SET_TIME),
            BleCommand::SetTime
        ));
        assert!(matches!(
            BleCommand::from_str(constant::CMD_FACTORY_RESET),
            BleCommand::FactoryReset
        ));
        assert!(matches!(
            BleCommand::from_str(constant::CMD_SEND_LOGS),
            BleCommand::SendLogs
        ));
        assert!(matches!(
            BleCommand::from_str("bogus"),
            BleCommand::Unknown(_)
        ));
    }
}
