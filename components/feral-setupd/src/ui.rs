//! UI navigation functions for Chrome DevTools Protocol (CDP) interactions.

use crate::app_state::{AppState, Page, unix_s};
use crate::cdp::Cdp;
use crate::constant;
use crate::setup_lifecycle::SetupPhase;
use crate::startup;
use crate::update_coordinator::{UpdateCheckResult, UpdateExecution};
use crate::updater;
use anyhow::{Context, Result};
use std::sync::Arc;
use tokio::{
    sync::mpsc,
    task,
    time::{self, Duration},
};

// The url format is like this
// url?step=qr&device_info=<device_info>&version=<version>&device_id=<device_id>
// Note: we need extra device_id and version for the page to display at the bottom
fn build_qrcode_url(app_state: &Arc<AppState>) -> String {
    let device_info = startup::build_device_info(app_state);
    let device_id = app_state.device_id.clone();
    let version = app_state.current_version.clone();

    format!(
        "{}&device_info={device_info}&version={version}&device_id={device_id}",
        constant::QRCODE_URL_PREFIX
    )
}

pub async fn show_qrcode(app_state: &Arc<AppState>, chrome: &Arc<Cdp>) -> Result<()> {
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

pub async fn show_reflashing_qrcode(
    app_state: &Arc<AppState>,
    chrome: &Arc<Cdp>,
    flashing_guide_url: &str,
    current_version: &str,
    latest_version: &str,
    min_upgradeable_version: &str,
) -> Result<()> {
    // Build message with version information
    let message = format!(
        "We've moved too far ahead for this version to catch up. Your Art Computer is too far behind to auto-upgrade. Current version: {current_version} Latest version: {latest_version} Minimum upgradeable version: {min_upgradeable_version}. Scan the code above for step-by-step reflashing instructions, or contact us for help. support@feralfile.com"
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

pub async fn show_webapp(app_state: &Arc<AppState>, chrome: &Arc<Cdp>) -> Result<()> {
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

pub async fn show_message(
    chrome: &Arc<Cdp>,
    app_state: &Arc<AppState>,
    message: &str,
) -> Result<()> {
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
pub async fn navigate_transient_message(chrome: &Arc<Cdp>, message: &str) -> Result<()> {
    let encoded = urlencoding::encode(message);
    let message_url = format!("{}{}", constant::MSG_URL_PREFIX, encoded);
    chrome
        .navigate(&message_url)
        .await
        .with_context(|| format!("navigating to transient {message_url}"))?;
    println!("MAIN: Navigated to transient {message_url}");
    Ok(())
}

pub async fn show_system_upgrade(
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

pub async fn show_factory_reset(chrome: &Arc<Cdp>, app_state: &Arc<AppState>) -> Result<()> {
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

pub async fn show_version_check_failure(
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

/// Drives TV "checking for updates" copy during distributor HTTP retries.
///
/// Only [`UpdateExecution::Blocking`] attaches a channel: BLE uses [`UpdateExecution::NonBlocking`]
/// and passes `None` into the updater so CDP navigations are not on the mobile response path.
/// When active, the sender is dropped and the task drained before final TV screens so progress
/// cannot overwrite reflash, failure, or update UI.
pub struct VersionCheckProgress {
    tx: Option<updater::FetchProgressTx>,
    task: Option<task::JoinHandle<()>>,
}

impl VersionCheckProgress {
    #[must_use]
    pub fn uses_tv_progress(execution: UpdateExecution) -> bool {
        matches!(execution, UpdateExecution::Blocking)
    }

    // No `app_state`: progress navigations are transient and must NOT record the canonical
    // page (see `navigate_transient_message`), so the receiver task only needs the CDP handle.
    pub fn start(execution: UpdateExecution, chrome: Arc<Cdp>) -> Self {
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

    pub fn as_updater_progress(&self) -> Option<updater::FetchProgressTx> {
        self.tx.clone()
    }

    pub async fn finish(self) {
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

#[cfg(test)]
mod tests {
    use super::*;
    use crate::update_coordinator::{RestorePageTarget, restore_page_target};

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
