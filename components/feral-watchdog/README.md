# Feral Watchdog

The Feral Watchdog daemon monitors system health and takes corrective actions when issues are detected.

## CDP Monitor

The CDP Monitor is responsible for monitoring the health of the Chromium browser using Chrome DevTools Protocol.

### Monitoring Logic

- Checks Chromium health via CDP every 5 seconds (`GET /json/version`).
- Treats Chromium/CDP as a recoverable runtime dependency during startup,
  kiosk restart, OTA, and crash recovery; the watchdog stays alive and retries
  CDP access instead of exiting on a single refused connection.
- Hang detection uses two different budgets depending on whether Chromium
  has spoken to the watchdog yet:
  - **Pre-connect (cold boot or post-restart):** until we receive the first
    `200` from `/json/version`, sustained failures are tolerated for up to
    **90 seconds** (`CHROMIUM_STARTUP_GRACE`). This budget covers
    `feral-player.service` (`TimeoutStartSec=45s`) plus
    `chromium-kiosk.service` (`RestartSec=5`) plus Chromium's own bring-up.
  - **Post-connect (steady state):** once we have seen a success, silence
    beyond **20 seconds** (`CHROMIUM_HANG_THRESHOLD`) is treated as a real
    renderer hang and triggers a kiosk restart.
- Before escalating, the monitor consults `systemctl --user is-active
  chromium-kiosk.service`. If the kiosk is already in `activating` (e.g.
  systemd's own `Restart=always` is between attempts, OTA is mid-restart,
  or an operator just ran `systemctl restart`), the monitor defers rather
  than piling a redundant restart on top.
- After issuing a restart the monitor returns to pre-connect mode so the
  next 90 seconds of failed checks do not pile a second restart onto the
  in-progress one. Without this reset, the 20 s hang threshold would trip
  again ~25 s into Chromium's cold start and exhaust the restart budget on
  healthy devices.
- Recovery action: `systemctl --user restart chromium-kiosk.service`.
- If 3 restarts occur within 5 minutes the restart budget is exhausted. The
  monitor then stops `chromium-kiosk.service`, starts the ffos system unit
  `feral-kiosk-fallback.service` (plymouth on the free DRM device: black
  screen, spinner, "Something went wrong...") via `sudo -n systemctl start`,
  and **holds for 15 minutes** (`CHROMIUM_FALLBACK_HOLD`) before rebooting.
  During the hold every failed check is expected and quiet. The reboot stays
  the self-heal rail (a fresh boot clears transient faults and the nightly
  updaters need boots), but the customer sees a stable error screen instead
  of a black screen every ~5 minutes. The restart history is memory-only
  (ffos-user#254), so after the reboot the cycle repeats: ~5 min of restarts,
  then 15 min of fallback. If a check succeeds during the hold (an operator
  restarted the kiosk, an OTA fixed the bundle), the hold is dropped and the
  restart history reset; `chromium-kiosk.service` stops the fallback unit in
  its `ExecStartPre`, so any kiosk start clears the screen. If the fallback
  unit cannot be started (older image without it, sudo refused) or the
  kiosk stop itself fails, the monitor reboots immediately instead of
  holding on a black or frozen screen; if a RAM/GPU
  kiosk restart happens to hold the kiosk lock at that moment nothing is
  stopped and the next tick retries. Inside the hold
  the two exemptions below still apply: a display unplugged during the hold
  abandons it and forgets the exhausted budget (no reboot; once a display
  returns the reconnect grace restarts the kiosk), and a developer on
  another VT defers the reboot until tty1 is active again. RAM/GPU-triggered
  kiosk restarts are refused while the error screen is deliberately up.
- **Developer console is exempt.** cage runs with `-s`, so a developer with a
  keyboard can Ctrl+Alt+F2 to the password-protected `getty@tty2`.
  `start-kiosk.sh` (`wait_for_vt1`) refuses to launch cage while the active VT
  is not `tty1` (seatd would hand cage the developer's VT), so Chromium is
  legitimately absent. While `/sys/class/tty/tty0/active` is not `tty1` the
  monitor suppresses escalation exactly like headless (no restart, no
  restart-history accumulation, no fallback, no reboot) and logs the
  transition once; returning to `tty1` re-arms the startup grace. The gate
  fails open when the file is unreadable. The two predicates MUST stay
  identical.
- **Headless devices are exempt.** On a device with no monitor, the kiosk
  intentionally waits for a display before launching Chromium, so a missing
  `/json/version` is expected, not a failure. Escalation is suppressed (no
  kiosk restart, no restart-history accumulation, no reboot) while no
  `/sys/class/drm/card*-*/status` connector reads `connected`. A `disconnected`
  or `unknown` reading both count as no-display: FF1's amdgpu persistently
  reports `unknown` on empty connectors, and treating that as "connected" kept
  escalation live on genuinely headless devices (restart storm; see
  start-kiosk.sh's matching display wait). Detection **fails open** only when
  no connector status is readable at all (absent or unrecognized sysfs
  layout), so escalation is never silently disabled on unknown environments.
  When a display is (re)connected, the startup grace is re-armed from scratch
  before escalation resumes.

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
