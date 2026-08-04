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
  Offline artwork caching is the same kind of pre-CDP special case:
  `downloadPlaylistItem`, `downloadPlaylist`, `clearPlaylistItemCache`,
  `clearPlaylistCache`, and `getOfflineCacheStatus` are routed by
  `commandrouter` into `offlinecache` (never forwarded to
  `window.handleCDPRequest`) because they queue background work and report
  async progress rather than talking to Chromium directly.
- `devicectl` (executor) implements all device-control commands: connect, showPairingQRCode, keyboard/mouse events, screen rotation, shutdown, reboot, analytics toggle, beta features toggle, device status, update, factory reset, upload logs, volume, SSH access, and panel control over DDC/CI (`ddcPanelControl` / `ddcPanelStatus` for brightness, contrast, volume, mute, and power via `ddcutil` with a simple retry/recovery path).
  - `showPairingQRCode`, `factoryReset`, `updateToLatestVersion`, and `uploadLogs` are handled **in-process** (they formerly emitted D-Bus signals to `feral-setupd`): `showPairingQRCode` runs the pre-claim OTA gate and paints/hides the claim QR via `setupui`; `factoryReset` clears the persisted relayer topic and starts `set-factory-boot.service`; `updateToLatestVersion` drives `otagate`; `uploadLogs` zips and uploads logs via presigned POST + S3 PUT (with an optional `supportBundleID` / `support_bundle_id`).
  - Executor manages three sentinel state files: `/home/feralfile/.state/analytics-toggle-off` (presence = analytics disabled), `/home/feralfile/.state/beta-features-toggle-on` (presence = beta features enabled), `/home/feralfile/.state/saved-volume` (persisted volume level).
- `dbus` owns the D-Bus client and defines the inbound `com.feralfile.sysmonitord` constants controld consumes. Since the setupd merge, controld exports no D-Bus RPCs of its own and emits none of the former setupd-targeted signals.
- Setup domain: `softap` raises/lowers the NetworkManager hotspot (`FF1-<device_id>`, WPA2-PSK from the device id); `portal` serves the captive portal on `:80` (picker, `/connect`, `/status`, OS-probe redirects); `provisioning` is the AP-trigger + join state machine (states `online`, `offline_retrying`, `unprovisioned`, `ap_active`, `joining`; the AP raises on link loss, not internet loss (#233): any live local link — ethernet or an associated Wi-Fi station, never the device's own hotspot — suppresses it, the 5m window measures continuous confirmed link absence probed every 15s tick, and a failed probe defers the AP rather than authorizing it; AP bounce on join; ONE additional raise trigger exists besides the window — the boot **relocation check** (M-0b): gated on `Config.BootAssessment` (a device boot, never a `Restart=always` mid-life restart), it requires a confirmed-absent link plus `relocConfirmScans` consecutive uncapped live scans that positively show none of the profile-declared saved SSIDs (hidden-network profiles make the whole set unverifiable and disarm it), and any sighting/inconclusive input falls back to the plain window; entry into `offline_retrying` from a boot assessment or from `joining` also carries narrated Reasons — `boot-offline`/`boot-no-internet`/`boot-link-unknown`/`joined-no-internet`/`joined-conn-unknown` — that the wiring notifier paints via the player's neutral-titled `connecting` state (`setupui` downgrades it to `join_failed` at send time when the player manifest provably predates it, so the narration never silently disappears on an older bundle), while every other entry reason stays deliberately silent) running under a panic-recovering supervisor; `wifictl` wraps `nmcli` scan/join/saved-profile; `otagate` is the single-flight OTA gate (modes `Required`/`Available`, retry ladders, in-memory permanent-failure latch, always local); `setupui` narrates setup progress to ff-player over the manifest-gated, fire-and-forget `setupDisplay` CDP contract.
- `relayer` manages the WebSocket relayer connection (ping every 15s, pong wait 3s). It classifies errors as permanent, transient, or busy.
- `cdp` is the Chrome DevTools Protocol client (WebSocket to `127.0.0.1:9222`). Commands are sent via `Runtime.evaluate` calling `window.handleCDPRequest(payload)`.
- `status` owns the device status collector (`DeviceStatus`) and the status poller. The poller polls CDP for player status and drives notifications to the web app. `DeviceStatus.GetStatus` includes best-effort `displayURL` (Chromium page URL from DevTools `/json`) on `device_status` notifications; player status carries playback/UI state from `checkStatus` only.
- `hub` exposes an HTTP server on `0.0.0.0:1111`: `POST /api/cast` (same command envelope and `commandrouter` as the relayer, including the mint-pairing pre-CDP commands), `GET /api/status` (legacy device/setup status, `contract:"1"`, kept for transitional tooling), `GET /api/v2/status` (the LAN **pairing** surface, identical payload with `contract:"2"` — the versioned route is the firmware gate: old firmware 404s here, so the app treats 404 as not-LAN-pairable), `GET → WS /api/notification`, and `GET /metrics` (Prometheus). All routes go through one shared middleware (`hub/middleware.go`: in-flight storm cap + logging) — the single chokepoint and the designated insertion point for the future LAN-authorization layer (#3471). The listener binds **unconditionally** when `enableHub` is set (default on); it is the BLE-replacement LAN recovery channel. Every route is **unauthenticated** today — treat the enabled hub as a trusted-local-network control surface and add auth at the middleware chokepoint, not per-route.
- `mdns` advertises the device (`_ff1._tcp`, TXT `id`/`name`/`claimed`/`api` — `api=2` mirrors the hub's v2 status contract and is the discovery-time firmware gate the pairing app filters on; a hub-side test pins the two constants equal). Discoverability is **link-keyed** (advertised whenever there is any network link, torn down on link loss), independent of internet/relayer state; a claim-state flip triggers a Stop+Start re-registration so the TXT `claimed` value is refreshed.
- `oom-recovery` (`OOMRecoverer`): on startup, compares `/var/lib/oom_state/chromium-oom-kill-count` against a handled-count file. If unhandled OOM kills exist, it polls (every 2s, up to 60 retries) until the webapp is responsive, then sends `CMD_DISPLAY_DEFAULT_PLAYLIST` to resume playback, then writes the handled-count. Suppresses player notifications during recovery.
- `playlist-refresher`: polls every 5 minutes. If the current player command is `displayPlaylist`, it re-resolves the playlist via `dp1` (URL-based or dynamic queries) and re-sends it to CDP with `refresh: true`. When refresh fails transiently but a validated in-memory `displayAt` cache exists, it force-casts a recomputed active set from cache (`now_display`, not `refresh`) instead of stalling. After a controld restart there is no durable full playlist; if the source fetch fails, controld leaves the current player artwork alone and retries later. Static inline playlists are not rebuilt or restart-resumed from player status because the player only has the filtered active set and no stable full-playlist identity.
- `playlistschedule`: owns DP-1 item-level `displayAt` filtering when at least one playlist item has `displayAt`. On cast/URL display it commits only the refreshable source identity to durable state after the player accepts the filtered active set, keeps the resolved full playlist in memory, arms a timer for the next `displayAt`, and force-casts (`intent.action=now_display`, no `refresh`) on timer fire, sleep-schedule wake, and CDP (re)connect so cutover is not deferred for remaining artwork duration. Soft `refresh: true` stays on the URL/dynamic refresher path only, except the refresher force-casts when it reconstructs scheduler ownership from URL/dynamic sources after a controld restart. The player stays unaware of `displayAt` scheduling.
- `commandrouter` displayPlaylist casts also default CDP `intent.action` to `now_display` when the controller omitted it: the player rejects unknown/missing DP1 actions with `ok: false`. Explicit controller intent is preserved; soft refresh does not go through this default.
- `dp1` processes DP1 playlist format (URL and dynamic queries). Uses `ff-indexer` for content resolution.
- `ff-indexer` fetches Feral File content index via HTTP.
- `mintpairing` owns controller-started browser-session mint pairing. It uses the temporary `ff-art-computer-handoff` Go minter library only for Mint Pairing Broker channels and E2EE browser messages; `feral-controld` remains responsible for driving the player mint-pairing overlay via CDP, sending approval request/outcome notifications over the relayer, and creating ephemeral browser sessions through `POST /api/ephemeral-sessions?topicID=...`.
- `offlinecache` (opt-in via `offlineCache.enabled`) downloads, stores, and replays DP-1 playlist items so `ff-player` can show them with no network access — **software** (HTML/JS) items via a headless-Chromium capture, and **every other single-file mime type** (image/video/audio/SVG/`model/gltf`/PDF/unrecognized) via a browser-free direct HTTP download (`mediacapture.go`); only live/VOD manifest-based streaming (HLS `.m3u8` or DASH `.mpd`) is rejected outright (`classify.go`'s `ClassStreaming`). It owns: a separate headless Chromium (`downloader.go`, its own debug port and user-data-dir, never the kiosk's) for the software capture path only; a content-addressed blob store plus one `items/<id>.json`/`playlists/<id>.json` record per edition (`store.go`, no persisted top-level manifest); a loopback static file server for assets over the CDP `Fetch.fulfillRequest` body ceiling (`staticserver.go`); and a second, event-driven CDP session (`cdpsession.go`) that intercepts `Fetch` on the kiosk (`:9222`) to replay cached items of EITHER class (`replay.go`, `kioskreplay.go`) without ever touching the daemon's existing synchronous `cdp` client. `commandrouter`'s `displayPlaylist` path and `playlist-refresher` call `KioskReplay.SyncPlaylist` to keep replay scope in sync with what is actually cached and on screen; `main.go`'s CDP `onConnect` hook re-attaches the replay session across kiosk restarts (including OOM recovery). A resource-aware admission gate (`admission.go`, on by default, `offlineCache.resourceGate` to tune/disable) defers STARTING queued downloads while the device is under memory/thermal pressure — class-aware (temperature-strict for the headless-Chromium software path, permissive for direct media downloads), fed monitord's raw `sysmetrics` payloads through `mediator.SetSysMetricsObserver` (the mediator stays policy-free glue), fail-open on missing metrics, and never aborting an in-flight capture; the in-flight window is instead bounded by `offlineCache.headlessLimits` (also default-on), which wraps the capture Chromium in a transient systemd scope (`CPUQuota`/`AllowedCPUs`/`MemoryMax`, runtime `systemd-run` — never unit-file properties, which would cross to the full-image rail) and degrades to a plain spawn when scopes are unavailable. See `docs/offline-artwork-capture.md` for the full design and validated capture edge cases.
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
- `offlinecache`'s headless downloader Chromium (`:9223`) and its kiosk replay CDP session are intentionally separate processes/connections from the kiosk (`:9222`) and its existing synchronous `cdp` client. Do not merge them for convenience — a shared browser or connection would let a stuck/slow download block player command handling, and would reintroduce the OOM-pressure and multi-client CDP behavior risks this separation exists to avoid.
- `offlinecache`'s admission gate (`resourceGate`) and headless resource cap (`headlessLimits`) are one coupled system, not two independent knobs, and `OptionsFromConfig`'s `finalizeOptions` is where that coupling is applied on every exit path: the software memory threshold is derived from `memoryMaxBytes` against this device's actual RAM (a static percentage is only safe at one RAM size — see `AdmissionPolicy.SoftwareReserveBytes`), `cpuQuotaPercent` is clamped to what `allowedCpus` can deliver (a quota above `100 x len(allowedCpus)` silently stops limiting anything), and the gate's temperature berth below `WatchdogCriticalCPUTempC` is sized to cover the heat a capture bounded by those limits can add. Widening the quota/cpuset or changing either default without revisiting the other side is the amendment hazard; `WatchdogCriticalCPUTempC`/`WatchdogCriticalMemoryPercent` also mirror `feral-watchdog` constants by hand across a module boundary and must be re-synced if that daemon retunes.

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
