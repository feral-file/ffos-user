if ! systemctl --user is-enabled "feral-sys-monitord.service" >/dev/null 2>&1; then
    systemctl --user enable --now "feral-sys-monitord.service"
fi
if ! systemctl --user is-enabled "feral-app-monitord.service" >/dev/null 2>&1; then
    systemctl --user enable --now "feral-app-monitord.service"
fi
if ! systemctl --user is-enabled "feral-watchdog.service" >/dev/null 2>&1; then
    systemctl --user enable --now "feral-watchdog.service"
fi

if ! systemctl --user is-enabled "display-restore.service" >/dev/null 2>&1; then
    systemctl --user enable --now "display-restore.service"
fi

systemctl --user start "chromium-kiosk.service"

# Enable hourly timers for time sync and log rotation
if ! systemctl --user is-enabled "feral-timesyncd.timer" >/dev/null 2>&1; then
    systemctl --user enable --now "feral-timesyncd.timer"
fi

if ! systemctl --user is-enabled "feral-log-rotation.timer" >/dev/null 2>&1; then
    systemctl --user enable --now "feral-log-rotation.timer"
fi

if ! sudo systemctl is-enabled "feral-updater@03:00.timer" >/dev/null 2>&1; then
    sudo systemctl enable --now "feral-updater@03:00.timer"
fi
