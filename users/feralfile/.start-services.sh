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

# Backward compatibility: Disable and stop old services if they are enabled
if systemctl --user is-enabled "feral-sys-monitord.service" >/dev/null 2>&1; then
    systemctl --user disable "feral-sys-monitord.service"
    systemctl --user stop "feral-sys-monitord.service"
fi

if systemctl --user is-enabled "feral-watchdog.service" >/dev/null 2>&1; then
    systemctl --user disable "feral-watchdog.service"
    systemctl --user stop "feral-watchdog.service"
fi

mkdir -p /home/feralfile/.config/systemd/user/
if ! mountpoint -q /home/feralfile/.config/systemd/user/; then
    sudo mount /home/feralfile/systemd-services/ /home/feralfile/.config/systemd/user/ -o bind
fi

systemctl --user daemon-reload
systemctl --user start system-ready.target

# Start the recovery daemons FIRST, before any blocking service start. This
# script runs under set -e, so a failed/timed-out blocking start (player,
# display-restore, …) aborts everything after it — and setupd is the only path
# back into BLE/WiFi provisioning on a device that boots broken, so it must
# already be started by then. Start them directly rather than relying on
# chromium-ready.target to pull them in: they are CDP-optional with
# self-reconnect, so they must run whether or not a display/Chromium ever comes
# up (e.g. a headless boot). setupd starts BLE before its bounded, non-fatal
# controld wait (components/feral-setupd/src/main.rs), so neither daemon's
# readiness gates the other.
# --no-block: controld is Type=notify and can exit before READY on a bad boot
# (e.g. relayer handshake failure on a warm reboot). A blocking start would then
# fail and, under set -e, abort this script before chromium-kiosk/feral-watchdog
# ever start. Restart=always recovers the daemons on their own; the rest of boot
# must never hinge on them reaching READY.
systemctl --user start --no-block "feral-controld.service"
systemctl --user start --no-block "feral-setupd.service"

systemctl --user start "feral-sys-monitord.service"
systemctl --user start "feral-vmagent.service"
systemctl --user start "display-restore.service"
systemctl --user start "feral-player.service"
systemctl --user start "chromium-kiosk.service"
systemctl --user start "ota-update-success-check.service"

if ! systemctl --user is-enabled "feral-log-rotation.timer" >/dev/null 2>&1; then
    systemctl --user enable --now "feral-log-rotation.timer"
fi

if ! sudo systemctl is-enabled "feral-updater@03:00.timer" >/dev/null 2>&1; then
    sudo systemctl enable --now "feral-updater@03:00.timer"
fi

if ! sudo systemctl is-enabled "feral-recovery-update@5:30.timer" >/dev/null 2>&1; then
    sudo systemctl enable --now "feral-recovery-update@5:30.timer"
fi

sleep 5

systemctl --user start "feral-watchdog.service"
