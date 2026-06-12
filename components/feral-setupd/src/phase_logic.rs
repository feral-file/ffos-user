//! Phase transition logic and guards for setup lifecycle management.

use crate::app_state::AppState;
use crate::setup_lifecycle::SetupPhase;

pub fn get_setup_phase(app_state: &AppState) -> String {
    app_state.lifecycle.get().as_str().to_string()
}

/// Phase the BLE setup wrapper should leave behind when a version check ends without an update
/// (failure or critical error). `check_and_update_system` already restored the pre-check durable
/// phase (Ready/Pairing) before returning, so the wrapper must NOT blindly force Idle or it would
/// demote a device that just recovered from `update_failed` via a BLE retry. Only non-durable
/// phases collapse to Idle.
pub fn ble_terminal_reset_phase(current: SetupPhase) -> SetupPhase {
    if current.is_durable() {
        current
    } else {
        SetupPhase::Idle
    }
}

/// Whether the BLE success path must fetch a fresh relayer topic from controld.
///
/// `GetRelayerTopicID` blocks up to `DBUS_RELAYER_CHECK_TIMEOUT` (31s) and the BLE handler awaits
/// the setup callback before replying to the phone, so an unconditional fetch defeats the bounded
/// BLE response and can fail an already-provisioned device with `ServerUnreachable` even though it
/// already holds the topic it should return. Only contact controld when no usable topic is
/// persisted yet (first-time setup); a Pairing/Ready device returns its persisted topic immediately.
pub fn needs_relayer_topic_fetch(persisted_topic: Option<&str>) -> bool {
    !matches!(persisted_topic, Some(topic) if !topic.is_empty())
}

/// Whether a `show_pairing_qr_code` switch (QR or webapp) must be ignored.
///
/// A permanent update failure latches the durable `UpdateFailed` phase and leaves the failure
/// message on screen until an explicit BLE/D-Bus retry clears it. `handle_permanent_update_failure`
/// releases the update guard, so gating only on `update_in_progress` is not enough: a later QR/webapp
/// switch from controld could navigate Chromium off the failure surface while `setup_phase` is still
/// `update_failed`. Block both switch directions until recovery clears the latch.
pub fn qr_switch_blocked_by_update_failed(phase: SetupPhase) -> bool {
    phase == SetupPhase::UpdateFailed
}

/// Whether a `show_pairing_qr_code(false)` pairing confirmation should record the durable
/// `Pairing -> Ready` transition for the current live phase.
///
/// The confirmation is a one-shot D-Bus signal (controld ACKs it after the callback returns and
/// does not re-emit), so it must be recorded even if an OTA owns the device — otherwise gating the
/// whole switch on the update guard silently drops the pairing confirmation and the device is
/// stuck in `Pairing` while mobile believes pairing succeeded.
///
/// It applies when:
/// - the device is in steady-state `Pairing` (the common case), or
/// - an in-flight OTA op (guard held) has transiently masked `Pairing` with a non-durable phase
///   while a relayer `topic_id` is already persisted. A persisted topic means the device is past
///   topic allocation (it was `Pairing`/`Ready`, not a fresh `Idle`), so promoting to `Ready` is
///   correct. Paired with [`phase_after_inflight_check`], which refuses to demote this `Ready`
///   back to `Pairing` when the masking check restores.
///
/// It must NOT fire for `Idle` without a topic (first run), `Ready` (already paired), or
/// `UpdateFailed` (latched failure surface, awaits explicit retry).
pub fn pairing_confirmation_promotes_to_ready(phase: SetupPhase, has_topic: bool) -> bool {
    match phase {
        SetupPhase::Pairing => true,
        SetupPhase::WifiConnecting | SetupPhase::CheckingVersion | SetupPhase::Updating => {
            has_topic
        }
        SetupPhase::Idle | SetupPhase::Ready | SetupPhase::UpdateFailed => false,
    }
}

/// Phase to restore after an in-flight no-update / failed version check, given the phase captured
/// at the check's entry (`phase_before_check`) and the live phase now (`current_phase`).
///
/// Normally this restores the durable phase captured before the transient `CheckingVersion` (or
/// `Idle` if that entry phase was not durable), preserving paired state across a no-op check.
///
/// The exception preserves a pairing confirmation that landed *during* the check: if the live phase
/// was promoted to `Ready` while we were checking (see [`pairing_confirmation_promotes_to_ready`]),
/// demoting it back to `Pairing` would silently drop the confirmation. So a `Ready` that appeared
/// mid-check wins over a captured `Pairing`.
pub fn phase_after_inflight_check(
    phase_before_check: SetupPhase,
    current_phase: SetupPhase,
) -> SetupPhase {
    let restore = if phase_before_check.is_durable() {
        phase_before_check
    } else {
        SetupPhase::Idle
    };

    if restore == SetupPhase::Pairing && current_phase == SetupPhase::Ready {
        SetupPhase::Ready
    } else {
        restore
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    /// Regression for PR #206 review 4475817494: a BLE retry from update_failed restores a durable
    /// phase, and a subsequent VersionCheckFailed must NOT demote it back to Idle. The wrapper
    /// keeps durable phases (check_and_update_system already restored them) and only collapses
    /// non-durable phases.
    #[test]
    fn ble_terminal_reset_preserves_durable_phase() {
        assert_eq!(
            ble_terminal_reset_phase(SetupPhase::Ready),
            SetupPhase::Ready,
        );
        assert_eq!(
            ble_terminal_reset_phase(SetupPhase::Pairing),
            SetupPhase::Pairing,
        );
        assert_eq!(
            ble_terminal_reset_phase(SetupPhase::UpdateFailed),
            SetupPhase::UpdateFailed,
        );
    }

    #[test]
    fn ble_terminal_reset_collapses_non_durable_phase() {
        assert_eq!(ble_terminal_reset_phase(SetupPhase::Idle), SetupPhase::Idle);
        assert_eq!(
            ble_terminal_reset_phase(SetupPhase::WifiConnecting),
            SetupPhase::Idle,
        );
        assert_eq!(
            ble_terminal_reset_phase(SetupPhase::CheckingVersion),
            SetupPhase::Idle,
        );
        assert_eq!(
            ble_terminal_reset_phase(SetupPhase::Updating),
            SetupPhase::Idle,
        );
    }

    /// Regression for PR #206 review 4482768497: the BLE no-update success path must not call the
    /// blocking GetRelayerTopicID when a usable topic is already persisted (Pairing/Ready recovery);
    /// it should return the persisted topic immediately. Only first-time setup (no/empty topic)
    /// contacts controld.
    #[test]
    fn relayer_topic_fetch_only_when_no_persisted_topic() {
        assert!(needs_relayer_topic_fetch(None));
        assert!(needs_relayer_topic_fetch(Some("")));
        assert!(!needs_relayer_topic_fetch(Some("topic-abc")));
    }

    /// Regression for PR #206 review 4482768497: a show_pairing_qr_code switch (either direction)
    /// must be ignored while update_failed is latched, so the device stays on the failure surface
    /// until an explicit retry clears it. Every other phase allows the switch.
    #[test]
    fn qr_switch_blocked_only_in_update_failed() {
        assert!(qr_switch_blocked_by_update_failed(SetupPhase::UpdateFailed));
        for phase in [
            SetupPhase::Idle,
            SetupPhase::WifiConnecting,
            SetupPhase::CheckingVersion,
            SetupPhase::Updating,
            SetupPhase::Pairing,
            SetupPhase::Ready,
        ] {
            assert!(
                !qr_switch_blocked_by_update_failed(phase),
                "phase {phase:?} must not block the QR switch",
            );
        }
    }

    /// Regression for PR #206 review 4483882810: a pairing confirmation must be recorded as a
    /// durable Pairing -> Ready transition even when an OTA transiently masks Pairing, but must
    /// never promote a first-run Idle (no topic), an already-Ready device, or a latched UpdateFailed.
    #[test]
    fn pairing_confirmation_promotes_only_paired_devices() {
        // Steady-state Pairing always confirms (topic presence irrelevant; invariant guarantees it).
        assert!(pairing_confirmation_promotes_to_ready(
            SetupPhase::Pairing,
            true
        ));
        assert!(pairing_confirmation_promotes_to_ready(
            SetupPhase::Pairing,
            false
        ));

        // Transient OTA phases masking a paired device (topic on disk) confirm; without a topic they
        // are a first-run device mid-check and must not be promoted.
        for masked in [
            SetupPhase::WifiConnecting,
            SetupPhase::CheckingVersion,
            SetupPhase::Updating,
        ] {
            assert!(
                pairing_confirmation_promotes_to_ready(masked, true),
                "masked-paired phase {masked:?} should confirm"
            );
            assert!(
                !pairing_confirmation_promotes_to_ready(masked, false),
                "masked phase {masked:?} without a topic must not confirm"
            );
        }

        // Never promote these, regardless of topic.
        for phase in [
            SetupPhase::Idle,
            SetupPhase::Ready,
            SetupPhase::UpdateFailed,
        ] {
            for has_topic in [true, false] {
                assert!(
                    !pairing_confirmation_promotes_to_ready(phase, has_topic),
                    "phase {phase:?} (has_topic={has_topic}) must not confirm"
                );
            }
        }
    }

    /// The in-flight check restore must preserve durable phases normally, but must NOT demote a
    /// Ready that a pairing confirmation recorded while the check was running back to Pairing.
    #[test]
    fn inflight_check_restore_preserves_midcheck_ready() {
        // Normal restores: captured durable phase wins; non-durable collapses to Idle.
        assert_eq!(
            phase_after_inflight_check(SetupPhase::Pairing, SetupPhase::CheckingVersion),
            SetupPhase::Pairing
        );
        assert_eq!(
            phase_after_inflight_check(SetupPhase::Ready, SetupPhase::CheckingVersion),
            SetupPhase::Ready
        );
        assert_eq!(
            phase_after_inflight_check(SetupPhase::Idle, SetupPhase::CheckingVersion),
            SetupPhase::Idle
        );

        // A confirmation promoted the live phase to Ready mid-check: keep Ready, do not demote.
        assert_eq!(
            phase_after_inflight_check(SetupPhase::Pairing, SetupPhase::Ready),
            SetupPhase::Ready
        );

        // A non-durable entry phase with a mid-check Ready is not demoted either (Ready is durable
        // truth); but a non-durable entry without that promotion still collapses to Idle.
        assert_eq!(
            phase_after_inflight_check(SetupPhase::Idle, SetupPhase::Ready),
            SetupPhase::Idle,
            "only a captured Pairing is upgraded to a mid-check Ready"
        );
    }
}
