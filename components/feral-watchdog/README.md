# Feral Watchdog

The Feral Watchdog daemon monitors system health and takes corrective actions when issues are detected.

## Native player health and recovery

The watchdog probes the native host's private Unix socket every five seconds,
using version negotiation and `health.get`. It validates socket ownership,
peer UID, bounded frames, correlation and host identity. A renderer failure is
not a host failure: the C++ host owns three bounded renderer retries and keeps
its command interface and error UI alive. No CDP connection or `/json/version`
is used by production watchdog control. Old metric/type names and the
`chromium-kiosk.service` name are retained for operational continuity.

The recovery policy is unchanged: 90 seconds before first connection or after
a restart, 20 seconds of failure after a successful probe, at most three kiosk
restarts within five minutes, then the existing 15-minute Plymouth fallback
hold. One fallback reboot is allowed across boots before parking. A successful
native command-loop probe supplies both host and control-path health; no
controld CDP report is needed to forgive that reboot budget.

Headless, developer-VT, active-update and already-activating-unit gates remain.
DRM counts only `connected`, failing open only when no status is readable.
Returning from a gate re-arms the startup grace. Updater holds are capped at
eight hours. The watchdog alone owns process escalation; controld never
restarts the player to repair a command connection.

CPU protection sends a level-triggered `health.set {thermal:true}` after ten
seconds at or above 93 C. This releases artwork backends without replacing the
playlist; below 88 C it clears the hold. Reaffirming the level covers a host
restart during heat. Systemd monitoring reports `serviceFailure` for kiosk,
controld and sys-monitord failures; clearing it does not navigate or reset
playback. The intentional controld OTA-stop exemption remains tied to both the
marker and updater lock. Screenshots are compositor observations, not health
probes or evidence that an arbitrary artwork rendered correctly.

## Resource Monitoring (RAM, GPU, DISK)

The monitord will send a metric every 2s.

During an update (see "Updates are exempt" above) the resource policies wait
too. Every reboot goes through `rebootSystem`, which defers it with a
rate-limited Warn. The disk policy sits the update out entirely: no reboot,
and no `pacman -Scc` either, since that can delete packages a concurrent
pacman update has downloaded but not yet installed; it resumes where it
stood once the lock is released. The RAM policy still restarts the kiosk
(that frees memory without touching the update) but its reboot step waits.
The GPU reboot is one-shot and is dropped if it fires mid-update.

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
  "vmagent": {
    "url": "http://0.0.0.0:9431/api/v1/import/prometheus"
  }
}
```

The `vmagent.url` field is optional. If not provided, the default URL `http://0.0.0.0:9431/api/v1/import/prometheus` will be used.

The socket defaults to `$XDG_RUNTIME_DIR/ff-player/player.sock`; `FF_PLAYER_SOCKET`
overrides it for isolated tests. The former `cdp.endpoint` config is ignored by
the native health probe.

### Metrics Sent

When a system reboot is triggered, the watchdog sends the following metric to vmagent:

- `ff_crash_reboot{reason="<reason>"} 1`

Where `<reason>` can be:

- `chromium_crash` - Too many Chromium restarts
- `gpu_hang` - GPU hanging detected
- `disk_full` - Disk remains full after cleanup
- `ram_critical` - RAM usage remains critical after kiosk restart
