# CDP Monitor

The CDP Monitor is responsible for monitoring the health of the Chromium browser using Chrome DevTools Protocol.

## Monitoring Logic

- Checks Chromium health via CDP every 5 seconds
- If health check fails or returns non-200, checks time since last successful response
- If no successful response for over 20 seconds, restarts `chromium-kiosk.service`
- If 3 restarts occur within 5 minutes, triggers a system reboot

# RAM, GPU, DISK Monitoring

The monitord will send a metric every 2s.

## GPU

If hanging, immediately reboot.

## DISK

If disk usage > 90% (or even > 95%), trigger cleanup pacman cache. After cleanup, freeze the disk checking for 10s.
After 10s from the cleanup.

- If disk usage > 95%: reboot.
- If disk usage > 90%: cleanup again.
- If disk usage < 90%: Normal, reset cleanup state.

## RAM

If RAM usage > 95% for more than 15s, then restart kiosk. If RAM usage > 95% for > 15s again with 60s after a restart, then reboot.
After restart kiosk, freeze the monitor for 5s, wait for the service restarting before checking again.
