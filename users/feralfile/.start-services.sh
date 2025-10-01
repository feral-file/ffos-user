# Backward compatibility: Disable and stop old services if they are enabled
if systemctl --user is-enabled "feral-sys-monitord.service" >/dev/null 2>&1; then
    systemctl --user disable "feral-sys-monitord.service"
    systemctl --user stop "feral-sys-monitord.service"
fi

if systemctl --user is-enabled "feral-app-monitord.service" >/dev/null 2>&1; then
    systemctl --user disable "feral-app-monitord.service"
    systemctl --user stop "feral-app-monitord.service"
fi

if systemctl --user is-enabled "feral-watchdog.service" >/dev/null 2>&1; then
    systemctl --user disable "feral-watchdog.service"
    systemctl --user stop "feral-watchdog.service"
fi

mkdir -p /home/feralfile/.config/systemd/user/
sudo mount /home/feralfile/systemd-services/ /home/feralfile/.config/systemd/user/ -o bind

systemctl --user daemon-reload
systemctl --user start system-ready.target

systemctl --user start "feral-sys-monitord.service"
systemctl --user start "feral-app-monitord.service"
systemctl --user start "feral-vmagent.service"
systemctl --user start "display-restore.service"
systemctl --user start "chromium-kiosk.service"
systemctl --user start "ota-update-success-check.service"

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

sleep 5

systemctl --user start "feral-watchdog.service"
