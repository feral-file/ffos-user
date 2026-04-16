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

This component is the highest-risk Go daemon for accidental architectural sprawl. Keep responsibilities explicit and resist adding hidden cross-package coupling.

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
- `devicectl` (executor) implements all device-control commands: connect, showPairingQRCode, keyboard/mouse events, screen rotation, shutdown, reboot, analytics toggle, beta features toggle, device status, update, factory reset, upload logs, volume, SSH access, and panel control over DDC/CI (`ddcPanelControl` / `ddcPanelStatus` for brightness, contrast, volume, mute, and power via `ddcutil` with a simple retry/recovery path).
  - `showPairingQRCode`, `factoryReset`, `updateToLatest`, `uploadLogs` also send D-Bus signals to `feral-setupd` on controld's own bus (`/com/feralfile/controld`, interface `com.feralfile.controld.general`) via `RetryableSend`.
  - Executor manages three sentinel state files: `/home/feralfile/.state/analytics-toggle-off` (presence = analytics disabled), `/home/feralfile/.state/beta-features-toggle-on` (presence = beta features enabled), `/home/feralfile/.state/saved-volume` (persisted volume level).
- `dbus` owns the D-Bus client and the controld handler. The handler exports `GetRelayerTopicID` RPC. It also defines the member constants for signals sent to setupd.
- `relayer` manages the WebSocket relayer connection (ping every 15s, pong wait 3s). It classifies errors as permanent, transient, or busy.
- `cdp` is the Chrome DevTools Protocol client (WebSocket to `127.0.0.1:9222`). Commands are sent via `Runtime.evaluate` calling `window.handleCDPRequest(payload)`.
- `status` owns the device status collector (`DeviceStatus`) and the status poller. The poller polls CDP for player status and drives notifications to the web app. `DeviceStatus.GetStatus` includes best-effort `displayURL` (Chromium page URL from DevTools `/json`) on `device_status` notifications; player status carries playback/UI state from `checkStatus` only.
- `hub` exposes a local WebSocket server on `0.0.0.0:1111` (only when `enableHub` is true in config). Uses the same `commandrouter` as the relayer. Also serves Prometheus metrics at this address.
- `mdns` advertises the device on the local network. mDNS starts/stops in response to connectivity changes from D-Bus.
- `oom-recovery` (`OOMRecoverer`): on startup, compares `/var/lib/oom_state/chromium-oom-kill-count` against a handled-count file. If unhandled OOM kills exist, it polls (every 2s, up to 60 retries) until the webapp is responsive, then sends `CMD_DISPLAY_DEFAULT_PLAYLIST` to resume playback, then writes the handled-count. Suppresses player notifications during recovery.
- `playlist-refresher`: polls every 5 minutes. If the current player command is `displayPlaylist`, it re-resolves the playlist via `dp1` (URL-based or dynamic queries) and re-sends it to CDP with `refresh: true`.
- `dp1` processes DP1 playlist format (URL and dynamic queries). Uses `ff-indexer` for content resolution.
- `ff-indexer` fetches Feral File content index via HTTP.
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
