# Agent Notes: `feral-controld`

Scope: `components/feral-controld/**`

Repository-wide principles from the root `AGENTS.md` also apply here.

## Purpose

`feral-controld` is the connectivity and command orchestration daemon.

It is responsible for:
- connecting the device to the relayer when network state allows it
- routing incoming relayer commands into local command handlers
- coordinating CDP, D-Bus, device control, playlist refresh, and state updates
- bridging health and connectivity signals from `feral-sys-monitord`
- exposing or coordinating hub and local device-facing control flows
- owning the full device-setup domain merged from the former `feral-setupd`: SoftAP provisioning, captive portal, OTA gate, on-screen setup narration, claiming, factory reset, and log upload

This component is the highest-risk Go daemon for accidental architectural sprawl. Keep responsibilities explicit and resist adding hidden cross-package coupling. Keep the setup domain (`softap`, `portal`, `provisioning`, `wifictl`, `otagate`, `setupui`) and the runtime domain (`relayer`, `commandrouter`, `devicectl`, `cdp`, `hub`) as legible sub-packages.

## Language and style
- Language: Go
- Follow standard Go readability guidance, especially Effective Go and Go Code Review Comments.
- Favor small interfaces owned by the consumer package.
- Prefer explicit orchestration over reflection-heavy or generic abstractions.
- Add comments when control flow or state mutation carries operational knowledge that future edits could break.

## Architecture

### Shape
- `main.go` owns startup, shutdown, wiring, and lifecycle.
- `mediator` is the orchestration hub: handles D-Bus signals from `feral-sys-monitord` and relayer messages, routes them to the right side effects.
- `commandrouter` is the command dispatch layer. It has a 3-way routing split:
  1. Commands where `Type.DeviceCtlCommand()` is true → `devicectl` executor (device control actions).
  2. `CMD_DISPLAY_PLAYLIST` → `dp1` (playlist resolution) then CDP (`window.handleCDPRequest(...)`).
  3. Everything else → CDP directly via `window.handleCDPRequest(...)`.
  Mint pairing is the intentional pre-CDP special case: `startMintPairingSession` and `mintPairingApprovalDecision` are routed by `commandrouter` into `mintpairing` so controld can own broker channels, approval notifications, and relayer session creation without exposing browser tokens to Chromium.
- `devicectl` (executor) implements all device-control commands: connect, showPairingQRCode, keyboard/mouse events, screen rotation, shutdown, reboot, analytics toggle, beta features toggle, device status, update, factory reset, upload logs, volume, SSH access, and panel control over DDC/CI (`ddcPanelControl` / `ddcPanelStatus` for brightness, contrast, volume, mute, and power via `ddcutil` with a simple retry/recovery path).
  - `showPairingQRCode`, `factoryReset`, `updateToLatestVersion`, and `uploadLogs` are handled **in-process** (they formerly emitted D-Bus signals to `feral-setupd`): `showPairingQRCode` runs the pre-claim OTA gate and paints/hides the claim QR via `setupui`; `factoryReset` clears the persisted relayer topic and starts `set-factory-boot.service`; `updateToLatestVersion` drives `otagate`; `uploadLogs` zips and uploads logs via presigned POST + S3 PUT (with an optional `supportBundleID` / `support_bundle_id`).
  - Executor manages three sentinel state files: `/home/feralfile/.state/analytics-toggle-off` (presence = analytics disabled), `/home/feralfile/.state/beta-features-toggle-on` (presence = beta features enabled), `/home/feralfile/.state/saved-volume` (persisted volume level).
- `dbus` owns the D-Bus client and defines the inbound `com.feralfile.sysmonitord` constants controld consumes. Since the setupd merge, controld exports no D-Bus RPCs of its own and emits none of the former setupd-targeted signals.
- Setup domain: `softap` raises/lowers the NetworkManager hotspot (`FF1-<device_id>`, WPA2-PSK from the device id); `portal` serves the captive portal on `:80` (picker, `/connect`, `/status`, OS-probe redirects); `provisioning` is the AP-trigger + join state machine (states `online`, `offline_retrying`, `unprovisioned`, `ap_active`, `joining`; the AP raises on link loss, not internet loss (#233): any live local link — ethernet or an associated Wi-Fi station, never the device's own hotspot — suppresses it, the 5m window measures continuous confirmed link absence probed every 15s tick, and a failed probe defers the AP rather than authorizing it; AP bounce on join) running under a panic-recovering supervisor; `wifictl` wraps `nmcli` scan/join/saved-profile; `otagate` is the single-flight OTA gate (modes `Required`/`Available`, retry ladders, in-memory permanent-failure latch, always local); `setupui` narrates setup progress to ff-player over the manifest-gated, fire-and-forget `setupDisplay` CDP contract.
- `relayer` manages the WebSocket relayer connection (ping every 15s, pong wait 3s). It classifies errors as permanent, transient, or busy.
- `cdp` is the Chrome DevTools Protocol client (WebSocket to `127.0.0.1:9222`). Commands are sent via `Runtime.evaluate` calling `window.handleCDPRequest(payload)`.
- `status` owns the device status collector (`DeviceStatus`) and the status poller. The poller polls CDP for player status and drives notifications to the web app. `DeviceStatus.GetStatus` includes best-effort `displayURL` (Chromium page URL from DevTools `/json`) on `device_status` notifications; player status carries playback/UI state from `checkStatus` only.
- `hub` exposes an HTTP server on `0.0.0.0:1111`: `POST /api/cast` (same command envelope and `commandrouter` as the relayer, including the mint-pairing pre-CDP commands), `GET /api/status` (legacy device/setup status, `contract:"1"`, kept for transitional tooling), `GET /api/v2/status` (the LAN **pairing** surface, identical payload with `contract:"2"` — the versioned route is the firmware gate: old firmware 404s here, so the app treats 404 as not-LAN-pairable), `GET → WS /api/notification`, and `GET /metrics` (Prometheus). All routes go through one shared middleware (`hub/middleware.go`: in-flight storm cap + logging) — the single chokepoint and the designated insertion point for the future LAN-authorization layer (#3471). The listener binds **unconditionally** when `enableHub` is set (default on); it is the BLE-replacement LAN recovery channel. Every route is **unauthenticated** today — treat the enabled hub as a trusted-local-network control surface and add auth at the middleware chokepoint, not per-route.
- `mdns` advertises the device (`_ff1._tcp`, TXT `id`/`name`/`claimed`/`api` — `api=2` mirrors the hub's v2 status contract and is the discovery-time firmware gate the pairing app filters on; a hub-side test pins the two constants equal). Discoverability is **link-keyed** (advertised whenever there is any network link, torn down on link loss), independent of internet/relayer state; a claim-state flip triggers a Stop+Start re-registration so the TXT `claimed` value is refreshed.
- `oom-recovery` (`OOMRecoverer`): on startup, compares `/var/lib/oom_state/chromium-oom-kill-count` against a handled-count file. If unhandled OOM kills exist, it polls (every 2s, up to 60 retries) until the webapp is responsive, then sends `CMD_DISPLAY_DEFAULT_PLAYLIST` to resume playback, then writes the handled-count. Suppresses player notifications during recovery.
- `playlist-refresher`: polls every 5 minutes. If the current player command is `displayPlaylist`, it re-resolves the playlist via `dp1` (URL-based or dynamic queries) and re-sends it to CDP with `refresh: true`.
- `dp1` processes DP1 playlist format (URL and dynamic queries). Uses `ff-indexer` for content resolution.
- `ff-indexer` fetches Feral File content index via HTTP.
- `mintpairing` owns controller-started browser-session mint pairing. It uses the temporary `ff-art-computer-handoff` Go minter library only for Mint Pairing Broker channels and E2EE browser messages; `feral-controld` remains responsible for driving the player mint-pairing overlay via CDP, sending approval request/outcome notifications over the relayer, and creating ephemeral browser sessions through `POST /api/ephemeral-sessions?topicID=...`.
- `watchdog` is a **systemd keepalive notifier** only — it sends `sd_notify WATCHDOG=1` every 15 seconds. It does NOT make recovery decisions (that is `feral-watchdog`'s job).
- `state` persists durable local state; treat it as a contract, not casual scratch storage.
- `wrapper` exists to keep code testable around time, OS, exec, random, IO, and serialization.

### Architectural direction
- Keep `main.go` as composition, not business logic.
- Prefer pushing external-system details to focused packages and keeping orchestration legible in mediator and command flows.
- Avoid turning `feral-controld` into a dumping ground for unrelated device policy.
- When adding a new behavior, decide first whether it belongs in:
  - an existing boundary package
  - the mediator as orchestration glue
  - a new focused package
  - or a different daemon entirely

### Amendment hazards
- Connectivity, relayer readiness, and D-Bus events interact. Do not change one of those flows without checking the others.
- State writes, relayer reconnection, and CDP updates should stay understandable in logs and comments.
- If a new path changes command routing or topic/state persistence, document the invariant close to the code.

## Verification for touched work
- Format changed Go files with `gofmt -s -w <changed-go-files>`.
- Run `go test ./...` in `components/feral-controld`.
- Run `go vet ./...` in `components/feral-controld`.
- Run changed-diff linting with `golangci-lint run --new-from-rev=HEAD~1 ./...` in `components/feral-controld`.

## Definition of done
A task in this component is done only when:
1. touched command, mediator, state, or integration paths still have clear ownership
2. tests and vet pass for this module, or blockers are documented
3. comments capture any non-obvious invariants, retries, state transitions, or trade-offs
4. startup and shutdown behavior remain coherent
5. any affected agent docs stay accurate

## Review flow
1. Prepare a short handoff covering the user-visible or system-visible behavior change, files changed, and checks run.
2. Call out any orchestration trade-offs, especially around connectivity, relayer, D-Bus, or persistence.
3. Run the reviewer loop using `prompts/code-review.md`.
4. Only commit or ship after the review loop returns `Verdict: accept`.
