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
  `restartChromium` transitions back to pre-connect mode after issuing a restart so the next 90 s of failed checks stay quiet. If Chromium restarts 3 times within 5 minutes, the device reboots instead of restarting kiosk. Recovery action: `systemctl --user restart chromium-kiosk.service`.
- `SystemdMonitor` (`systemd_service.go`) monitors four systemd services every 30 s: `feral-player.service`, `feral-setupd.service`, `feral-controld.service`, `feral-sys-monitord.service`. Restarts any service that is not active.
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
