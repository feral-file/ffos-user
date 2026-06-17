//! D-Bus setup and coordination functions for feral-setupd.

use crate::app_state::AppState;
use crate::callbacks;
use crate::cdp::Cdp;
use crate::constant;
use crate::dbus_utils;
use anyhow::Result;
use std::sync::Arc;
use std::sync::atomic::AtomicBool;
use tokio::time::{self, Duration};

pub async fn setup_dbus_listeners(
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

pub async fn wait_for_controld(timeout: Duration) -> Result<()> {
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
