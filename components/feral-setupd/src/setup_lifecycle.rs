//! Setup lifecycle coordinator for feral-setupd.
//!
//! Manages the device setup state machine. The phase directly represents the device's
//! current state and is exposed to mobile via BLE `get_info`.
//!
//! Durable phases (Pairing, Ready, UpdateFailed) persist across reboots.
//! Transient phases (WifiConnecting, CheckingVersion, Updating) reset to Idle on boot.

use crate::persistent_state::PersistentState;
use std::sync::Mutex as StdMutex;

/// Setup phase exposed to mobile via BLE `get_info`.
///
/// The phase is the single source of truth for device state. What you set is what
/// mobile sees - no derivation or priority rules.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum SetupPhase {
    /// No active operation
    Idle,

    /// WiFi credential exchange in progress
    WifiConnecting,

    /// Checking for system updates
    CheckingVersion,

    /// System update in progress
    Updating,

    /// Has topic_id, waiting for mobile to complete pairing via D-Bus signal.
    /// Invariant: topic_id must exist in persistent state. On restore, if topic_id
    /// is missing, phase is corrected to Idle.
    /// On reboot, mandatory update check runs before showing pairing QR (consistent
    /// with first-time setup flow where update check runs before entering Pairing).
    Pairing,

    /// Fully paired and operational
    Ready,

    /// Update failed permanently, needs factory reset or manual recovery
    UpdateFailed,
}

impl SetupPhase {
    pub fn as_str(&self) -> &'static str {
        match self {
            SetupPhase::Idle => "idle",
            SetupPhase::WifiConnecting => "wifi_connecting",
            SetupPhase::CheckingVersion => "checking_version",
            SetupPhase::Updating => "updating",
            SetupPhase::Pairing => "pairing",
            SetupPhase::Ready => "ready",
            SetupPhase::UpdateFailed => "update_failed",
        }
    }

    fn from_str(s: &str) -> SetupPhase {
        match s {
            "wifi_connecting" => SetupPhase::WifiConnecting,
            "checking_version" => SetupPhase::CheckingVersion,
            "updating" => SetupPhase::Updating,
            "pairing" => SetupPhase::Pairing,
            "ready" => SetupPhase::Ready,
            "update_failed" => SetupPhase::UpdateFailed,
            _ => SetupPhase::Idle,
        }
    }

    /// Returns true if this phase should persist across reboots.
    pub fn is_durable(&self) -> bool {
        matches!(
            self,
            SetupPhase::Pairing | SetupPhase::Ready | SetupPhase::UpdateFailed
        )
    }
}

/// Coordinator for setup lifecycle state.
///
/// The phase is the authoritative device state. Set it directly where state changes;
/// it will be exposed as-is to mobile via BLE.
#[derive(Debug)]
pub struct SetupLifecycle {
    phase: StdMutex<SetupPhase>,
}

impl SetupLifecycle {
    pub fn new() -> Self {
        Self {
            phase: StdMutex::new(SetupPhase::Idle),
        }
    }

    /// Restore phase from persistent storage after daemon restart.
    ///
    /// Durable phases (Pairing, Ready, UpdateFailed) are restored from disk.
    /// Transient phases are not persisted, so daemon always starts in Idle.
    ///
    /// Invariant validation: Pairing phase requires topic_id to exist. If missing,
    /// the phase is corrected to Idle to prevent invalid state.
    ///
    /// Legacy migration: Devices upgrading from pre-phase firmware with
    /// `paired=true` + `topic_id` are migrated to `Ready` phase.
    pub fn restore_from_store(&self, store: &PersistentState) {
        use crate::persistent_state::SETUP_PHASE;

        if let Some(phase_str) = store.get(SETUP_PHASE) {
            let phase = SetupPhase::from_str(&phase_str);

            // Validate Pairing invariant: must have topic_id
            if phase == SetupPhase::Pairing {
                let has_topic = store.get(crate::persistent_state::TOPIC_ID).is_some();
                if !has_topic {
                    eprintln!(
                        "LIFECYCLE: Invalid state on restore: Pairing phase without topic_id, correcting to Idle"
                    );
                    *self.phase.lock().unwrap() = SetupPhase::Idle;
                    return;
                }
            }

            *self.phase.lock().unwrap() = phase;
        } else {
            // Legacy migration: paired=true + topic_id exists → Ready phase
            // This ensures devices upgrading from old firmware (which used paired flag)
            // maintain their paired state and reach the webapp instead of QR setup.
            let is_legacy_paired = store.get("paired").as_deref() == Some("true");
            let has_topic = store.get("topic_id").is_some();

            if is_legacy_paired && has_topic {
                *self.phase.lock().unwrap() = SetupPhase::Ready;
            }
        }
    }

    /// Set the current setup phase.
    ///
    /// This immediately updates what mobile sees via BLE `get_info`.
    /// For durable phases (Pairing, Ready, UpdateFailed), call `persist()` afterwards.
    pub fn set(&self, phase: SetupPhase) {
        *self.phase.lock().unwrap() = phase;
    }

    /// Get the current setup phase.
    pub fn get(&self) -> SetupPhase {
        *self.phase.lock().unwrap()
    }

    /// Persist the current phase to disk if it's durable.
    ///
    /// Durable phases (Pairing, Ready, UpdateFailed) persist across reboots.
    /// Transient phases (WifiConnecting, CheckingVersion, Updating, Idle) do not.
    pub fn persist(&self, store: &PersistentState) -> anyhow::Result<()> {
        use crate::persistent_state::SETUP_PHASE;
        let phase = self.get();
        if phase.is_durable() {
            store.set(SETUP_PHASE, phase.as_str());
        } else {
            // Clear persisted phase so we start fresh on next boot
            store.set(SETUP_PHASE, "");
        }
        store.save()?;
        Ok(())
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use tempfile::NamedTempFile;

    fn test_store() -> PersistentState {
        let file = NamedTempFile::new().unwrap();
        PersistentState::new(file.path().to_str().unwrap()).unwrap()
    }

    #[test]
    fn new_lifecycle_starts_in_idle() {
        let lifecycle = SetupLifecycle::new();
        assert_eq!(lifecycle.get(), SetupPhase::Idle);
    }

    #[test]
    fn set_and_get_phase() {
        let lifecycle = SetupLifecycle::new();

        lifecycle.set(SetupPhase::WifiConnecting);
        assert_eq!(lifecycle.get(), SetupPhase::WifiConnecting);

        lifecycle.set(SetupPhase::Pairing);
        assert_eq!(lifecycle.get(), SetupPhase::Pairing);

        lifecycle.set(SetupPhase::Ready);
        assert_eq!(lifecycle.get(), SetupPhase::Ready);
    }

    #[test]
    fn durable_phases_persist_across_reboot() {
        let store = test_store();
        let lifecycle1 = SetupLifecycle::new();

        // Set Pairing and persist (with topic_id to satisfy invariant)
        store.set(crate::persistent_state::TOPIC_ID, "test-topic-123");
        store.save().unwrap();
        lifecycle1.set(SetupPhase::Pairing);
        lifecycle1.persist(&store).unwrap();

        // Simulate reboot
        let lifecycle2 = SetupLifecycle::new();
        lifecycle2.restore_from_store(&store);
        assert_eq!(lifecycle2.get(), SetupPhase::Pairing);

        // Set Ready and persist
        lifecycle2.set(SetupPhase::Ready);
        lifecycle2.persist(&store).unwrap();

        // Simulate reboot
        let lifecycle3 = SetupLifecycle::new();
        lifecycle3.restore_from_store(&store);
        assert_eq!(lifecycle3.get(), SetupPhase::Ready);

        // Set UpdateFailed and persist
        lifecycle3.set(SetupPhase::UpdateFailed);
        lifecycle3.persist(&store).unwrap();

        // Simulate reboot
        let lifecycle4 = SetupLifecycle::new();
        lifecycle4.restore_from_store(&store);
        assert_eq!(lifecycle4.get(), SetupPhase::UpdateFailed);
    }

    #[test]
    fn transient_phases_do_not_persist_across_reboot() {
        let store = test_store();
        let lifecycle1 = SetupLifecycle::new();

        // Set Updating (transient) and persist
        lifecycle1.set(SetupPhase::Updating);
        lifecycle1.persist(&store).unwrap();

        // Simulate reboot - should start fresh in Idle
        let lifecycle2 = SetupLifecycle::new();
        lifecycle2.restore_from_store(&store);
        assert_eq!(lifecycle2.get(), SetupPhase::Idle);

        // Same for other transient phases
        lifecycle2.set(SetupPhase::CheckingVersion);
        lifecycle2.persist(&store).unwrap();

        let lifecycle3 = SetupLifecycle::new();
        lifecycle3.restore_from_store(&store);
        assert_eq!(lifecycle3.get(), SetupPhase::Idle);
    }

    #[test]
    fn phase_as_str_returns_correct_strings() {
        assert_eq!(SetupPhase::Idle.as_str(), "idle");
        assert_eq!(SetupPhase::WifiConnecting.as_str(), "wifi_connecting");
        assert_eq!(SetupPhase::CheckingVersion.as_str(), "checking_version");
        assert_eq!(SetupPhase::Updating.as_str(), "updating");
        assert_eq!(SetupPhase::Pairing.as_str(), "pairing");
        assert_eq!(SetupPhase::Ready.as_str(), "ready");
        assert_eq!(SetupPhase::UpdateFailed.as_str(), "update_failed");
    }

    #[test]
    fn from_str_parses_phase_strings() {
        assert_eq!(SetupPhase::from_str("idle"), SetupPhase::Idle);
        assert_eq!(
            SetupPhase::from_str("wifi_connecting"),
            SetupPhase::WifiConnecting
        );
        assert_eq!(
            SetupPhase::from_str("checking_version"),
            SetupPhase::CheckingVersion
        );
        assert_eq!(SetupPhase::from_str("updating"), SetupPhase::Updating);
        assert_eq!(SetupPhase::from_str("pairing"), SetupPhase::Pairing);
        assert_eq!(SetupPhase::from_str("ready"), SetupPhase::Ready);
        assert_eq!(
            SetupPhase::from_str("update_failed"),
            SetupPhase::UpdateFailed
        );
        assert_eq!(SetupPhase::from_str("unknown"), SetupPhase::Idle);
    }

    #[test]
    fn legacy_paired_state_migrates_to_ready() {
        let store = test_store();

        // Simulate old firmware state: paired=true, topic_id exists, no setup_phase
        store.set("paired", "true");
        store.set("topic_id", "legacy-topic-id");
        store.save().unwrap();

        // New firmware should restore as Ready
        let lifecycle = SetupLifecycle::new();
        lifecycle.restore_from_store(&store);

        assert_eq!(lifecycle.get(), SetupPhase::Ready);
    }

    #[test]
    fn legacy_unpaired_state_stays_idle() {
        let store = test_store();

        // Old firmware state: paired=false or missing, topic_id may or may not exist
        store.set("paired", "false");
        store.set("topic_id", "some-topic");
        store.save().unwrap();

        let lifecycle = SetupLifecycle::new();
        lifecycle.restore_from_store(&store);

        // Should remain Idle, not migrate to Ready
        assert_eq!(lifecycle.get(), SetupPhase::Idle);
    }

    #[test]
    fn legacy_migration_does_not_override_explicit_phase() {
        let store = test_store();

        // State has both: new setup_phase AND legacy paired flag
        store.set("setup_phase", "pairing");
        store.set("paired", "true");
        store.set("topic_id", "topic");
        store.save().unwrap();

        let lifecycle = SetupLifecycle::new();
        lifecycle.restore_from_store(&store);

        // New phase takes precedence over legacy migration
        assert_eq!(lifecycle.get(), SetupPhase::Pairing);
    }

    #[test]
    fn pairing_without_topic_id_corrects_to_idle() {
        let store = test_store();

        // Invalid state: Pairing phase but no topic_id
        store.set("setup_phase", "pairing");
        store.save().unwrap();

        let lifecycle = SetupLifecycle::new();
        lifecycle.restore_from_store(&store);

        // Should auto-correct to Idle
        assert_eq!(lifecycle.get(), SetupPhase::Idle);
    }

    #[test]
    fn pairing_with_topic_id_restores_correctly() {
        let store = test_store();

        // Valid state: Pairing phase with topic_id
        store.set("setup_phase", "pairing");
        store.set("topic_id", "valid-topic-123");
        store.save().unwrap();

        let lifecycle = SetupLifecycle::new();
        lifecycle.restore_from_store(&store);

        // Should restore Pairing since invariant is satisfied
        assert_eq!(lifecycle.get(), SetupPhase::Pairing);
    }
}
