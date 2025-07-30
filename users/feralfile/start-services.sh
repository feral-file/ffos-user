systemctl --user start "chromium-kiosk.service"

ENV_MODE="test"
if [[ -r /home/feralfile/.config/environment ]]; then
    ENV_MODE="$(cat /home/feralfile/.config/environment 2>/dev/null | xargs)"
fi

if [[ "$ENV_MODE" == "live" ]]; then
    for timer in 03:00; do
        if ! systemctl --user is-enabled "feral-updater@$timer.timer" >/dev/null 2>&1; then
            systemctl --user enable --now "feral-updater@$timer.timer"
        fi
    done
fi

# Enable hourly timers for time sync and log rotation
if ! systemctl --user is-enabled "feral-timesyncd.timer" >/dev/null 2>&1; then
    systemctl --user enable --now "feral-timesyncd.timer"
fi

if ! systemctl --user is-enabled "feral-log-rotation.timer" >/dev/null 2>&1; then
    systemctl --user enable --now "feral-log-rotation.timer"
fi
