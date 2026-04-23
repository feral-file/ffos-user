#!/bin/bash
set -euo pipefail

VOLUME_FILE="/home/feralfile/.state/saved-volume"
if [ ! -f "$VOLUME_FILE" ]; then
    # First boot: set default volume to 63%
    echo "63" > "$VOLUME_FILE"
    PACTL_PERCENT=63
else
    # Read saved volume
    PACTL_PERCENT=$(cat "$VOLUME_FILE")
fi

pamixer --set-volume "$PACTL_PERCENT"

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

systemctl --user start "feral-sys-monitord.service"
systemctl --user start "feral-vmagent.service"
systemctl --user start "display-restore.service"
systemctl --user start "feral-ff-player-static.service"
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
