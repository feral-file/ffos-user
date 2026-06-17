//! Core application state for feral-setupd.

use crate::connectivity::Connectivity;
use crate::persistent_state::PersistentState;
use crate::setup_lifecycle::SetupLifecycle;
use std::sync::Arc;
use std::sync::atomic::AtomicBool;
use std::time::{SystemTime, UNIX_EPOCH};
use tokio::sync::Mutex;

#[inline]
pub fn unix_s() -> i64 {
    match SystemTime::now().duration_since(UNIX_EPOCH) {
        Ok(duration) => duration.as_secs() as i64,
        Err(error) => {
            eprintln!("MAIN: System time is before UNIX_EPOCH: {error:?}");
            0
        }
    }
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Page {
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
    pub fn should_keep_on_bt_disconnect(&self) -> bool {
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
pub struct AppState {
    pub device_id: String,
    pub branch: String,
    pub current_version: String,
    pub state_store: PersistentState,
    pub internet: Connectivity,
    pub page: Mutex<Page>,

    // This is the flag to indicate whether we should automatically redirect to webapp
    // when internet is available.
    // On a second boot, if the internet is unavailable, users have 2 choices
    // 1. Fix the internet connection, it will automatically check for update & play artwork
    // 2. Scan the QRCode and start everything over again
    // We need this flag to turn off the first flow if the user has chosen to provide a different wifi
    // true = auto proceed, false = user has chosen to provide a different wifi
    pub auto_proceed: AtomicBool,
    /// Setup progress coordinator. Derives mobile-facing phase from persistent state
    /// (PAIRED, topic_id), active operations (wifi, version check, update), and
    /// permanent failure latch.
    pub lifecycle: SetupLifecycle,
    /// Single-flight guard for OTA updates to prevent concurrent update attempts.
    /// Wrapped in Arc to allow UpdateGuard to be 'static for tokio::spawn.
    pub update_in_progress: Arc<AtomicBool>,
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::connectivity::Connectivity;
    use crate::persistent_state::PersistentState;
    use crate::setup_lifecycle::{SetupLifecycle, SetupPhase};
    use std::sync::Arc;

    #[test]
    fn set_and_get_setup_phase() {
        use crate::phase_logic::get_setup_phase;

        let lifecycle = SetupLifecycle::new();
        let temp_dir = tempfile::tempdir().unwrap();
        let state_file = temp_dir.path().join("state.txt");
        let state_store = PersistentState::new(state_file.to_str().unwrap()).unwrap();

        let app_state = Arc::new(AppState {
            device_id: "test".to_string(),
            branch: "test".to_string(),
            current_version: "1.0.0".to_string(),
            state_store,
            internet: tokio::runtime::Runtime::new()
                .unwrap()
                .block_on(async { Connectivity::spawn().await }),
            page: Mutex::new(Page::None(0)),
            auto_proceed: AtomicBool::new(false),
            lifecycle,
            update_in_progress: Arc::new(AtomicBool::new(false)),
        });

        assert_eq!(get_setup_phase(&app_state), "idle");

        app_state.lifecycle.set(SetupPhase::WifiConnecting);
        assert_eq!(get_setup_phase(&app_state), "wifi_connecting");

        app_state.lifecycle.set(SetupPhase::CheckingVersion);
        assert_eq!(get_setup_phase(&app_state), "checking_version");

        app_state.lifecycle.set(SetupPhase::Updating);
        assert_eq!(get_setup_phase(&app_state), "updating");

        app_state.lifecycle.set(SetupPhase::UpdateFailed);
        assert_eq!(get_setup_phase(&app_state), "update_failed");

        app_state.lifecycle.set(SetupPhase::Pairing);
        assert_eq!(get_setup_phase(&app_state), "pairing");

        app_state.lifecycle.set(SetupPhase::Ready);
        assert_eq!(get_setup_phase(&app_state), "ready");
    }
}
