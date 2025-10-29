# Feral Watchdog

The Feral Watchdog daemon monitors system health and takes corrective actions when issues are detected.

## CDP Monitor

The CDP Monitor is responsible for monitoring the health of the Chromium browser using Chrome DevTools Protocol.

### Monitoring Logic

- Checks Chromium health via CDP every 5 seconds
- If health check fails or returns non-200, checks time since last successful response
- If no successful response for over 20 seconds, restarts `chromium-kiosk.service`
- If 3 restarts occur within 5 minutes, triggers a system reboot

## Resource Monitoring (RAM, GPU, DISK)

The monitord will send a metric every 2s.

### GPU

If hanging, immediately reboot.

### DISK

If disk usage > 90% (or even > 95%), trigger cleanup pacman cache. After cleanup, freeze the disk checking for 10s.
After 10s from the cleanup:

- If disk usage > 95%: reboot.
- If disk usage > 90%: cleanup again.
- If disk usage < 90%: Normal, reset cleanup state.

### RAM

If RAM usage > 95% for more than 15s, then restart kiosk. If RAM usage > 95% for > 15s again with 60s after a restart, then reboot.
After restart kiosk, freeze the monitor for 5s, wait for the service restarting before checking again.

## Vmagent Integration

The watchdog daemon integrates with vmagent to send crash reboot metrics when a system reboot is triggered due to a crash.

### Configuration

Add vmagent configuration to `/home/feralfile/.config/watchdog.json`:

```json
{
  "cdp": {
    "endpoint": "http://localhost:9222"
  },
  "vmagent": {
    "url": "http://0.0.0.0:9431/api/v1/import/prometheus"
  }
}
```

The `vmagent.url` field is optional. If not provided, the default URL `http://0.0.0.0:9431/api/v1/import/prometheus` will be used.

### Metrics Sent

When a system reboot is triggered, the watchdog sends the following metric to vmagent:

- `ff_crash_reboot{reason="<reason>"} 1`

Where `<reason>` can be:

- `chromium_crash` - Too many Chromium restarts
- `gpu_hang` - GPU hanging detected
- `disk_full` - Disk remains full after cleanup
- `ram_critical` - RAM usage remains critical after kiosk restart

### Testing Crash Reboot Metrics

You can test the crash reboot metric functionality without actually rebooting the system:

```bash
# Test chromium crash scenario (default 10s delay)
./feral-watchdog -debug -test=chromium_crash

# Test with custom delay
./feral-watchdog -debug -test=gpu_hang -test-delay=5s

# Test all scenarios
./feral-watchdog -debug -test=chromium_crash -test-delay=3s
./feral-watchdog -debug -test=gpu_hang -test-delay=3s
./feral-watchdog -debug -test=disk_full -test-delay=3s
./feral-watchdog -debug -test=ram_critical -test-delay=3s
```

Available test modes:

- `chromium_crash` - Simulate Chromium crash scenario
- `gpu_hang` - Simulate GPU hang scenario
- `disk_full` - Simulate disk full scenario
- `ram_critical` - Simulate RAM critical scenario

When running in test mode:

- The system will **NOT** actually reboot
- The crash_reboot metric **WILL** be sent to vmagent
- All logs are displayed showing the metric being sent
