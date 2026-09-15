# Agent Notes: `feral-watchdog`

Scope: `components/feral-watchdog/**`

Repository-wide principles from the root `AGENTS.md` also apply here.

## Purpose

`feral-watchdog` is the recovery-policy daemon for device health failures.

It is responsible for:
- monitoring Chromium health via HTTP polling (not WebSocket/CDP)
- consuming system metrics and events from `feral-sys-monitord`
- deciding when to restart kiosk services, clean disk pressure, or reboot
- feeding incident metrics to vmagent when configured
- keeping recovery behavior explicit, bounded, and observable

This daemon owns recovery policy. It should not become the source of raw health telemetry collection, which belongs in `feral-sys-monitord`.

## Language and style
- Language: Go
- Follow standard Go readability guidance.
- Prefer explicit policy checks over deeply nested recovery heuristics.
- Add comments for thresholds, cooldowns, escalation logic, and operational trade-offs.
- Any `nolint` or panic-retained invariant should be deliberate and easy to justify.

## Architecture

### Shape
- `main.go` wires config, DBus, vmagent, handlers, mediator, and background monitors.
- `Mediator` (`mediator.go`) consumes D-Bus signals from `feral-sys-monitord`:
  - `sysmetrics` → routes to disk, memory, and CPU handlers.
  - `sysevent` → `gpu_hanging` triggers `scheduleGPUReboot`; `gpu_recover` triggers `handleGPURecovery`.
- `ChromiumMonitor` (`chromium.go`) is a long-running background goroutine that polls `http://localhost:9222/json/version` via HTTP (not WebSocket/CDP). Check interval: 5 s. Hang detection has two modes:
  - **Pre-connect** (`hasEverConnected == false`): a 90 s `CHROMIUM_STARTUP_GRACE` budget covers cold boot and post-restart bring-up. Sized to absorb `feral-player.service` (`TimeoutStartSec=45s`) + `chromium-kiosk.service` `RestartSec=5` + Chromium cold start. The shorter hang threshold MUST NOT be used here, or healthy devices will restart kiosk on every boot.
  - **Post-connect** (`hasEverConnected == true`): a 20 s `CHROMIUM_HANG_THRESHOLD` budget detects genuine renderer hangs after at least one successful `/json/version`.
  Before escalating, the monitor checks `systemctl --user is-active chromium-kiosk.service` and defers if the result is `activating`, so it does not stack restarts on top of systemd's own `Restart=always` policy or an externally initiated restart (OTA, operator).
  `restartChromium` transitions back to pre-connect mode after issuing a restart so the next 90 s of failed checks stay quiet. Recovery action: `systemctl --user restart chromium-kiosk.service`.
  - **Fallback hold** (`CHROMIUM_FALLBACK_HOLD = 15 min`, `KIOSK_FALLBACK_UNIT = feral-kiosk-fallback.service`): when the third restart lands within `CHROMIUM_MAX_RESTARTS_WINDOW` (5 min) the monitor no longer reboots immediately. `CommandHandler.showKioskFallback` runs `systemctl --user stop chromium-kiosk.service` then `sudo -n systemctl start feral-kiosk-fallback.service` (the ffos unit that runs plymouthd on the free DRM device and shows "Something went wrong..."), and `ChromiumMonitor.fallbackSince` is set. `showKioskFallback` returns a three-state result: shown arms the hold; unavailable (the unit is absent on an image predating it — the watchdog ships on the package rail, the unit on the image rail — or sudo refused) makes the monitor reboot immediately, exactly as before this policy, because the kiosk has already been stopped; busy (a RAM/GPU kiosk restart held the lock, nothing was stopped) arms nothing and the next tick retries. `checkHangState` evaluates the hold before the ordinary gates but keeps both suppressions inside it: no connected display abandons the hold (headless latched, `fallbackShown` cleared; the hold never ends in a reboot, though the ordinary ladder applies again once a display returns); a developer VT other than `tty1` re-anchors `fallbackSince` and defers the reboot until `tty1` is active again. While the hold runs, failed checks return the expected/headless tag and nothing escalates; once it elapses on `tty1` with a display the monitor clears `fallbackSince`, clears `fallbackShown` and calls `rebootSystem(CrashReasonChromiumCrash)` exactly once (a failed reboot command falls back into the normal restart ladder instead of re-firing every 5 s). A successful check during the hold clears `fallbackSince`, `fallbackShown` and empties `restartHistory` (recovery by operator/OTA; the kiosk unit's `ExecStartPre` stops the fallback unit). `CommandHandler` serialises every kiosk operation through `kioskOpInFlight` and refuses `restartKiosk` (RAM and GPU handlers included) while `fallbackShown` is set, so nothing runs the kiosk's `ExecStartPre` and erases the error screen mid-hold. Trade-off: the reboot remains the self-heal rail, but the customer sees a stable error screen for 15 min instead of a black screen every ~5 min; the restart history is memory-only (ffos-user#254) so the cycle repeats after the reboot, which is bounded and mostly visible-error. Persisting the budget is #254/#255 scope.
  - **Display gating** (`display.go`, `isDisplayConnected`) sits ahead of both modes. On a headless device the kiosk deliberately waits for a display before launching Chromium, so `/json/version` is legitimately absent. While **no** `/sys/class/drm/card*-*/status` connector reads `connected` (the same source `display-restore.sh` uses), the monitor skips the entire escalation path: no kiosk restart and **no** restart-history accumulation, so a long headless period cannot later trip the reboot budget. `disconnected` and `unknown` both count as no-display: FF1's amdgpu persistently reports `unknown` on empty connectors, and an earlier fail-open-on-`unknown` rule kept escalation live on genuinely headless devices, driving a Chromium restart storm (high CPU temperature) in lockstep with start-kiosk.sh's display wait — the two gates MUST keep the same predicate. Hotplug raises HPD and flips the connector to `connected`, so waiting on `unknown` cannot mask a real monitor. Detection **fails open** only when no connector status is readable at all (absent/unrecognized sysfs layout), so the watchdog is never silently disabled on unknown environments. When a display reappears, escalation resumes with a **fresh** startup-grace window (`monitorStart`/`hasEverConnected` re-anchored) so a just-plugged monitor gets the full 90 s, not an instant restart. The sysfs root is injectable (`ChromiumMonitor.drmSysfsRoot`) for tests.
  - **Developer-console gating** (`display.go`, `isKioskVTActive`) runs right after the display gate. cage is started with `-s`, so a developer can Ctrl+Alt+F2 to the password-protected `getty@tty2` (ffos#126); `start-kiosk.sh`'s `wait_for_vt1` refuses to launch cage while `/sys/class/tty/tty0/active` is not `tty1` (seatd would hand cage the developer's VT), so Chromium is legitimately absent. While the active VT is not `tty1` the monitor suppresses escalation exactly like headless (latched in `ChromiumMonitor.devConsole`, logged once at Info), and the return to `tty1` re-anchors the startup grace like a display reconnect. Fails open when the file is unreadable. The predicate MUST stay identical to `wait_for_vt1`. The file path is injectable (`ChromiumMonitor.ttyActiveFile`) for tests.
- `SystemdMonitor` (`systemd_service.go`) monitors three systemd services every 30 s: `feral-player.service`, `feral-controld.service`, `feral-sys-monitord.service`. It logs state transitions and emits incident metrics to vmagent on failure; it does **not** call `systemctl restart` on these services. `feral-controld.service` was formerly `PartOf=chromium-ready.target` and torn down with it; it is now decoupled and runs unconditionally at boot, so an `inactive` reading for it is a genuine fault reported as a failure (per-service metric + incident notification), the same as `failed` (it is the sole entry in `servicesReportInactiveAsFailure`). The 90 s recent-exit grace in `checkSystemdUserServiceStatus` (a `failed` unit that exited under 90 s ago is treated as still starting) still applies. `feral-player.service` and `feral-sys-monitord.service` keep their prior handling: `failed` is a reported failure, but `inactive` is logged at `Error` only and not escalated to a metric. The obsolete `chromium-ready.target` gating (`isUnitActive`, expected-teardown Info window) has been removed. (`feral-setupd.service` is no longer monitored — that daemon was merged into `feral-controld`.)
- `SystemdWatchdog` (`systemd_watchdog.go`) sends `sd_notify WATCHDOG=1` every 10 s. This is a **keepalive notifier only** — it does not make any recovery decisions.
- Recovery and resume navigation always target the bundled local player at `http://127.0.0.1:8080/`. Do not reintroduce remote player URL overrides; the static player unit owns readiness.
- RAM handler (`ram.go`): critical threshold 95%. Sustained above threshold for 15 s → restart kiosk (`systemctl --user restart chromium-kiosk.service`). Sustained for 60 s → reboot device.
- Disk handler, GPU handler, CPU handler: resource-specific handlers encapsulate their own threshold and escalation logic.
- vmagent integration is a reporting side effect, not the policy source.

### Architectural direction
- Keep policy logic close to the relevant handler instead of scattering it through goroutines.
- Distinguish clearly between:
  - observation
  - decision
  - action
  - incident reporting
- If a threshold or escalation path changes, document the reason and the recovery trade-off.

### Amendment hazards
- Recovery thresholds, cooldowns, and reboot paths can easily become surprising if changed without comments.
- Changes to D-Bus event handling must stay aligned with the contracts emitted by `feral-sys-monitord`.
- Long-running monitors must keep clean cancellation and shutdown behavior.

## Verification for touched work
- Format changed Go files with `gofmt -s -w <changed-go-files>`.
- Run `go test ./...` in `components/feral-watchdog`.
- Run `go vet ./...` in `components/feral-watchdog`.
- Run changed-diff linting with `golangci-lint run --new-from-rev=HEAD~1 ./...` in `components/feral-watchdog`.

## Definition of done
A task in this component is done only when:
1. recovery policy remains explicit and understandable
2. tests and vet pass for this module, or blockers are documented
3. comments preserve the why behind thresholds, cooldowns, or escalation rules
4. shutdown behavior for background monitors remains correct
5. the README or agent docs stay accurate when behavior changes

## Review flow
1. Prepare a handoff that states which recovery policy changed and what system behavior it affects.
2. Call out threshold changes, reboot or restart semantics, and reporting side effects.
3. Run the reviewer loop using `prompts/code-review.md`.
4. Only commit or ship after the review loop returns `Verdict: accept`.
