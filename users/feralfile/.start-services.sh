#!/bin/bash
set -euo pipefail

VOLUME_FILE="/home/feralfile/.state/saved-volume"
DEFAULT_VOLUME=63

# The volume file may exist but be empty or corrupt (e.g. power loss mid-write).
# An empty/invalid PACTL_PERCENT makes pamixer fail, and under set -e that would
# abort this script before any service starts. Never trust the file contents;
# fall back to the default and rewrite the file so the state self-heals.
PACTL_PERCENT=$(cat "$VOLUME_FILE" 2>/dev/null || true)
if ! [[ "$PACTL_PERCENT" =~ ^[0-9]+$ ]] || [ "$PACTL_PERCENT" -gt 100 ]; then
    PACTL_PERCENT=$DEFAULT_VOLUME
    echo "$DEFAULT_VOLUME" > "$VOLUME_FILE"
fi

# Volume is best-effort: audio failure must not block service startup.
pamixer --set-volume "$PACTL_PERCENT" || echo "WARN: pamixer failed to set volume to $PACTL_PERCENT"

# Reset chromium OOM recovery state on each boot
if [ -d /var/lib/oom_state ]; then
    echo "0" > /var/lib/oom_state/chromium-oom-kill-count
    echo "0" > /var/lib/oom_state/chromium-oom-kill-handled-count
    echo "0" > /var/lib/oom_state/chromium-oom-kill-last-event
fi

# Backward compatibility: Disable and stop old services if they are enabled.
# Best-effort for the same F-03 reason as the starts below: this cleanup runs
# BEFORE controld — the only recovery path — ever starts, and a disable/stop
# failure (polkit denial, a unit that refuses to stop) under set -e would
# abort the whole boot over a unit we are about to re-start anyway.
if systemctl --user is-enabled "feral-sys-monitord.service" >/dev/null 2>&1; then
    systemctl --user disable "feral-sys-monitord.service" || echo "WARN: failed to disable legacy feral-sys-monitord.service"
    systemctl --user stop "feral-sys-monitord.service" || echo "WARN: failed to stop legacy feral-sys-monitord.service"
fi

if systemctl --user is-enabled "feral-watchdog.service" >/dev/null 2>&1; then
    systemctl --user disable "feral-watchdog.service" || echo "WARN: failed to disable legacy feral-watchdog.service"
    systemctl --user stop "feral-watchdog.service" || echo "WARN: failed to stop legacy feral-watchdog.service"
fi

mkdir -p /home/feralfile/.config/systemd/user/
if ! mountpoint -q /home/feralfile/.config/systemd/user/; then
    sudo mount /home/feralfile/systemd-services/ /home/feralfile/.config/systemd/user/ -o bind
fi

systemctl --user daemon-reload
# Best-effort for the same F-03 reason as the service starts below: a target
# start failing (broken unit graph after a bad OTA) must degrade, not abort
# the script before controld — the only recovery path — ever starts.
systemctl --user start system-ready.target || echo "WARN: system-ready.target failed to start"

# Start the recovery daemon FIRST, before any blocking service start. This
# script runs under set -e, so a failed/timed-out blocking start (player,
# display-restore, …) aborts everything after it — and controld is the only path
# back into WiFi/SoftAP provisioning and LAN recovery on a device that boots
# broken, so it must already be started by then. Start it directly rather than
# relying on chromium-ready.target to pull it in: it is CDP-optional with
# self-reconnect, so it must run whether or not a display/Chromium ever comes up
# (e.g. a headless boot).
# --no-block: controld is Type=notify and can exit before READY on a bad boot
# (e.g. relayer handshake failure on a warm reboot). A blocking start would then
# fail and, under set -e, abort this script before chromium-kiosk/feral-watchdog
# ever start. Restart=always recovers the daemon on its own; the rest of boot
# must never hinge on it reaching READY. The guard is still needed: --no-block
# only skips waiting for the job — job SUBMISSION can fail synchronously (unit
# file missing after a bad OTA, user manager D-Bus unreachable), and under
# set -e that would abort here, before kiosk/watchdog/timers (F-03).
systemctl --user start --no-block "feral-controld.service" || echo "WARN: feral-controld.service failed to start"

# Every blocking start/enable below is best-effort (|| echo WARN): this script
# runs under set -euo pipefail, so without that ONE failed unit (e.g.
# feral-player on a bad bundle) aborts the script here and nothing after it
# starts — no kiosk, no watchdog, and none of the update timers that are the
# device's only path to self-heal a bad version (F-03). Each unit has its own
# recovery rail (Restart= policies, the watchdog, the nightly updaters); boot
# must degrade, never stop. The WARN echoes are the field breadcrumb for
# WHICH unit failed (same idiom as the pamixer line above). This invariant is
# pinned by scripts/test-headless-startup-contract.sh.
systemctl --user start "feral-sys-monitord.service" || echo "WARN: feral-sys-monitord.service failed to start"
systemctl --user start "feral-vmagent.service" || echo "WARN: feral-vmagent.service failed to start"
systemctl --user start "display-restore.service" || echo "WARN: display-restore.service failed to start"
systemctl --user start "feral-player.service" || echo "WARN: feral-player.service failed to start"
systemctl --user start "chromium-kiosk.service" || echo "WARN: chromium-kiosk.service failed to start"
systemctl --user start "ota-update-success-check.service" || echo "WARN: ota-update-success-check.service failed to start"

# The enable blocks are best-effort for the same F-03 reason: `enable --now`
# also STARTS the unit and can fail (unit file missing after a full-image
# rsync — exactly the post-OTA state in which these blocks run at all instead
# of short-circuiting on is-enabled — or a sudo/polkit denial), and under
# set -e that would abort before the remaining timers and the watchdog. The
# system timers additionally ship as presets in the ffos image
# (system-preset/90-default.preset), so a failure here is a warning, not a
# lost safety net; feral-log-rotation.timer is user-scope and has ONLY this
# rail.
if ! systemctl --user is-enabled "feral-log-rotation.timer" >/dev/null 2>&1; then
    systemctl --user enable --now "feral-log-rotation.timer" || echo "WARN: failed to enable feral-log-rotation.timer"
fi

if ! sudo systemctl is-enabled "feral-updater@03:00.timer" >/dev/null 2>&1; then
    sudo systemctl enable --now "feral-updater@03:00.timer" || echo "WARN: failed to enable feral-updater@03:00.timer"
fi

if ! sudo systemctl is-enabled "feral-recovery-update@5:30.timer" >/dev/null 2>&1; then
    sudo systemctl enable --now "feral-recovery-update@5:30.timer" || echo "WARN: failed to enable feral-recovery-update@5:30.timer"
fi

sleep 5

systemctl --user start "feral-watchdog.service" || echo "WARN: feral-watchdog.service failed to start"
