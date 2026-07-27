# Architecture Direction

This document defines the canonical architectural direction for `ffos-user`.
Agents should treat these rules as stable constraints when adding, refactoring, or removing code.

---

## Canonical Service Boundaries

Each service in `components/` has exactly one responsibility. That boundary must not grow to absorb unrelated concerns.

| Service | Responsibility | Language |
|---|---|---|
| `feral-sys-monitord` | Observe and publish device health (CPU, RAM, disk, GPU, connectivity, system events) | Go |
| `feral-watchdog` | Consume health signals and decide recovery actions (restart, clean disk, reboot) | Go |
| `feral-controld` | Relayer connection and command routing **plus** the full device-setup domain: SoftAP provisioning, captive portal, OTA gate, on-screen setup narration, claiming, factory reset, log upload, and the LAN hub | Go |
| `player-wrapper-ui` | Wrap the media player process | Go |

`feral-setupd` and `launcher-ui` no longer exist. The setup/recovery daemon was merged into `feral-controld` (see "The setupd merge" below); the Chromium kiosk is now launched directly by `users/feralfile/scripts/start-kiosk.sh` (unit `chromium-kiosk.service`), not by a `launcher-ui` process. The repository contains no Rust.

Rules for each boundary:

- `feral-sys-monitord` is a **publisher only**. It must not make recovery decisions, reboot the system, or call other services. Callers pull from it via RPC or subscribe to its signals.
- `feral-watchdog` is the **single owner of recovery policy**. It decides when to restart Chromium, clean disk pressure, or reboot. Raw telemetry collection does not belong here; that belongs in `feral-sys-monitord`.
- `feral-controld` is the **connectivity, command orchestration, and device-setup hub**. Since the setupd merge it deliberately owns first-run provisioning and recovery as well as runtime command routing. It remains the highest-risk service for architectural sprawl: keep the setup domain (`softap`, `portal`, `provisioning`, `wifictl`, `otagate`, `setupui`) and the runtime domain (`relayer`, `commandrouter`, `devicectl`, `cdp`, `hub`) as legible sub-packages, and do not let unrelated device policy that belongs in `feral-watchdog` leak in.
- `player-wrapper-ui` is a **thin process starter**. It contains no business logic. Parameters come from command-line arguments. State and control live in daemons, not in this wrapper.

---

## The setupd merge (fault-containment rationale)

An earlier version of this document treated BLE provisioning as a hard isolation invariant: "BLE provisioning is owned entirely by `feral-setupd`." That boundary was **deliberately removed**. The former Rust `feral-setupd` daemon was folded into `feral-controld`, and BLE provisioning was replaced by SoftAP (a Wi-Fi hotspot + captive portal). The reasons are the new fault-containment story, and they should be treated as intentional direction, not drift:

- **Single language.** The device-services surface is now Go end to end. Rust, `cargo`, and the BLE/GATT stack are gone. There is one build toolchain, one test story, and one set of readability rules.
- **In-process supervision domain.** The setup flow and the runtime command path share one process, one state store, and one connectivity view. The provisioning event loop runs under a panic-recovering supervisor (`provisioning/supervisor.go`, capped exponential backoff) that contains panics *inside* the loop without tearing down the daemon. Cross-process D-Bus round-trips that used to coordinate controld↔setupd (`GetRelayerTopicID`, `show_pairing_qr_code`, `factory_reset`, `system_update`, `upload_logs`) are now direct in-process calls.
- **Relaxed StartLimit.** Because `feral-controld` is now the device's **only** provisioning path — there is no BLE fallback — its unit sets `StartLimitIntervalSec=0` (`users/feralfile/systemd-services/feral-controld.service`). Fast repeated crashes must degrade into slow `Restart=always` retries (`RestartSec=5`), never latch into a permanent `start-limit-hit` dead state that would strand setup on an offline device.
- **Provisioning-first startup.** `main.go` brings the LAN hub, mDNS, and the provisioning domain up **before** the relayer/CDP init, and the relayer connect is best-effort and never fatal. A relayer or CDP failure cannot abort device setup or the LAN recovery channel. `users/feralfile/.start-services.sh` starts `feral-controld.service` first, with `--no-block`, for the same reason.

Process-level crash recovery remains systemd's job; the in-process supervisor only contains panics within the provisioning loop.

---

## Deliberately-open LAN surface (release-scoped, pending security sign-off)

This release ships three LAN-facing surfaces **without** authentication. They are accepted, release-scoped decisions, not oversights. Each is named here for security sign-off, and each has a single declared end state: **v2 screen-anchored LAN pairing** (issue [#3471](https://github.com/feral-file/ffos-user/issues/3471)), where a short code shown on the device's display anchors trust, the device stores authorized controller keys, the owner reviews/revokes them on-device, and factory reset clears them.

1. **Unauthenticated command API on `:1111`.** `POST /api/cast` on the LAN hub accepts the same command envelope as the relayer and routes it through the same `commandrouter`. Any host on the local network can drive the device. The hub's single shared middleware (`hub/middleware.go`) is the **designated insertion point** for the v2 authorization layer — it is the one chokepoint every hub route passes through.
2. **Prometheus `/metrics` on the LAN.** The hub serves playback metrics in Prometheus text format at `:1111/metrics`, unauthenticated, on the same surface.
3. **System-wide unprivileged-port floor.** `feral-controld` runs as a `systemd --user` service (uid 1000) where `CAP_NET_BIND_SERVICE` is inert, so the captive portal binds `:80` by lowering the system-wide unprivileged-port floor: `net.ipv4.ip_unprivileged_port_start=80`. This sysctl is set in the base image (`ffos` repo, `archiso-ff1/airootfs/etc/sysctl.d/10-unprivileged-port-start.conf`), not in this repository, and it applies to **every** process on the device, not just the portal.

Until v2 lands, deployments must treat the LAN as a trust boundary: enable these surfaces only on networks where local clients are trusted. The `contract` field on `GET /api/status` (value `"1"`) exists for the dual-running window that retiring the open surface requires — see `docs/api-design.md`.

---

## Cross-Service Communication Contracts

### Primary IPC: D-Bus (session bus, user scope)

D-Bus remains the canonical inter-service transport on-device between the **remaining** daemons.

**Signal direction** (one-way, fire-and-forget):

```
feral-sys-monitord  --[sysmetrics]-----------> feral-controld
                    --[sysmetrics]-----------> feral-watchdog
                    --[connectivity_change]--> feral-controld
                    --[connectivity_change]--> feral-watchdog
                    --[sysevent]-------------> feral-watchdog
```

**RPC direction** (request/response):

```
feral-controld  --[GetConnectivityStatus]--> feral-sys-monitord
```

The former controld→setupd signals (`show_pairing_qr_code`, `factory_reset`, `system_update`, `upload_logs`, `upload_logs_with_bundle`) and the `GetRelayerTopicID` RPC no longer cross a process boundary: those handlers now live inside `feral-controld` and are invoked directly. `com.feralfile.controld`'s `dbus` package now exports only the inbound `feral-sys-monitord` constants it consumes.

### External transport: WebSocket relayer

`feral-controld` maintains a persistent WebSocket connection to the remote relayer endpoint. Remote commands from the mobile app arrive over this connection. `feral-controld` routes them locally to either the device executor (device-control commands) or Chromium via CDP (web/playback commands). No other service connects to the relayer.

### UI control: Chrome DevTools Protocol (CDP)

Daemons control the Chromium kiosk instance over CDP (HTTP + WebSocket to `127.0.0.1:9222`). `feral-controld` forwards web commands from the relayer to Chromium via CDP, and drives the on-screen **setup narration** through the player's `setupDisplay` CDP contract (see `setupui`, below). `feral-watchdog` monitors Chromium health via HTTP polling of `/json/version` and uses CDP navigation to steer Chromium back to the player during recovery. Neither daemon embeds a web server for UI assets.

`feral-player.service` is the readiness gate for the bundled local webapp. Chromium kiosk and any daemon that navigates to the local player must wait for that unit to report `READY=1`. The kiosk boots the bundled player at `http://127.0.0.1:8080/`.

### On-screen setup narration: `setupDisplay` (CDP → ff-player)

`feral-controld`'s `setupui` package pushes setup progress to the bundled player through a single CDP command, `setupDisplay`, delivered via `window.handleCDPRequest(...)` over `Runtime.evaluate`. The contract is:

- **Manifest-gated.** `setupui` reads the player capability manifest at `/opt/feral/feral-player/ffos-player-contract.json` and only narrates if `contracts.setupDisplay` (version `1`) is present. An older player yields a permanent no-narration fallback; there is no separate setup page.
- **Fire-and-forget.** Pushes never block, never return a fatal error, and never panic. A burst collapses to at most one trailing send; the last state is re-pushed on CDP reconnect.
- **Namespace-extensible.** New narration states can be added without breaking older players (e.g. `factory_reset` is an extension state outside the contract's required set), which is what keeps the v2 pairing-approval overlay additive.

### Local device control: LAN hub (port 1111)

`feral-controld` exposes an HTTP server on `0.0.0.0:1111` (`hub` package). Unlike the earlier optional WebSocket hub, the listener now binds **unconditionally** at boot (gated only by the `enableHub` config flag, which defaults on): it is the BLE-replacement LAN recovery channel and must stay reachable whenever there is a link. It exposes `POST /api/cast` (relayer command envelope), `GET /api/status` (legacy, contract `"1"`), `GET /api/v2/status` (the LAN pairing surface, contract `"2"` — old firmware 404s here, which is how the app gates pairability), `GET → WS /api/notification`, and `GET /metrics`, all through one shared middleware. See `docs/api-design.md` for the wire surface and the open-LAN caveats above.

### Service discovery: mDNS

`feral-controld` advertises the device via mDNS (`_ff1._tcp`, `mdns` package). Unlike the hub listener, mDNS **discoverability** is link-keyed: it comes up whenever there is any network link (ethernet or Wi-Fi), independent of internet reachability or relayer connectivity, and is torn down when the link drops. The TXT record carries `id`, `name`, and `claimed` (always published, even when `false`). A claim-state flip triggers a Stop+Start re-registration so resolvers see the new `claimed` value.

### Panel control: DDC/CI via `ddcutil`

`feral-controld`'s `devicectl` executor drives the attached panel over DDC/CI using the `ddcutil` CLI. Remote or hub commands `ddcPanelControl` and `ddcPanelStatus` map to brightness, contrast, speaker volume, mute, and power VCPs on the default display; the helper wraps `ddcutil` with a lightweight retry/recovery run when the tool reports display-not-found or missing VCP output.

### Mobile provisioning: SoftAP + captive portal

First-run and offline-recovery provisioning is done over a NetworkManager Wi-Fi hotspot (`softap`) and a device-served captive portal (`portal`), coordinated by the `provisioning` state machine and executed by `wifictl`. There is no BLE/GATT surface. The AP SSID is `FF1-<device_id>`, WPA2-PSK derived from the device id, and the portal binds `:80`. See `docs/api-design.md` and `docs/setup-flow.md` for the full contract and flow.

---

## Kiosk and Daemon Logic Ownership

The boundary between kiosk/UI code and daemon logic:

- **Daemons own all state, policy, and side effects.** Daemons decide what to show, when to update, and what to do on errors.
- **Chromium (via CDP) renders the UI.** The kiosk loads the bundled local player at `http://127.0.0.1:8080/`. Daemons navigate or execute JavaScript by calling CDP, not by modifying files on disk at runtime. Setup progress is pushed through the `setupDisplay` contract; there is no separate setup web page and no `file:///.../launcher/` surface.
- **Mint pairing display belongs to the player UI.** `feral-controld` owns broker/session state and drives the bundled player through the `mintPairingDisplay` CDP command. The player renders a transient overlay above active artwork playback; `ffos-user` does not ship a separate mint-pairing QR page.
- **The kiosk is a one-shot launcher script.** `users/feralfile/scripts/start-kiosk.sh` launches Chromium after waiting for a display and for `feral-player.service`. It contains no business logic and no daemon lifecycle.
- **UI does not call daemons directly** except through the LAN hub (when a local client sends commands to controld on port 1111). All other control flows originate in daemons and push into Chromium via CDP.

When adding new behavior that spans UI and daemon logic:
1. The state and decision logic goes in a daemon (usually `feral-controld`).
2. The daemon issues a CDP call (`window.handleCDPRequest`) to narrate, navigate, or execute JavaScript in Chromium.
3. The UI renders what it is told.

---

## Persistence and State Ownership

Each service owns its own state files exclusively. No service should read or write another service's state file.

| Owner | File | Contents |
|---|---|---|
| `feral-controld` | `/home/feralfile/.state/controld.state` | Relayer topic ID, connected device (ID, name, platform) |
| `feral-controld` | `/home/feralfile/.state/screen-orientation` | Last committed screen orientation value |
| `feral-controld` | `/home/feralfile/.state/display-at-playlist.json` | Refreshable source identity for the active item-level `displayAt` cast. The resolved full playlist is memory-only and is refetched after a controld-only restart; if refetch fails, the player keeps its current artwork until a later retry succeeds |
| `feral-controld` | `/home/feralfile/.state/analytics-toggle-off` | Presence = analytics disabled |
| `feral-controld` | `/home/feralfile/.state/beta-features-toggle-on` | Presence = beta features enabled |
| `feral-controld` | `/home/feralfile/.state/saved-volume` | Persisted volume level |
| updater scripts | `/home/feralfile/ff1-config.json` | Device branch, current version, update channel URLs (read-only at runtime by services) |
| system | `/etc/hostname` | Device hostname (read-only at runtime; used by `controld` for mDNS identity, the SoftAP SSID/PSK, and the device id) |
| earlyoom/oom-state | `/var/lib/oom_state/chromium-oom-kill-count` | Chromium OOM kill count (read by `controld` OOM recoverer) |
| earlyoom/oom-state | `/var/lib/oom_state/chromium-oom-kill-handled-count` | Handled OOM kill count (written by `controld` OOM recoverer) |
| `feral-watchdog` | `/home/feralfile/.state/failed_recovery_version` | Version of a recovery candidate that failed to boot |

The former `feral-setupd` state file (`/home/feralfile/.state/setupd`, with `setup_phase` / `pre_failure_phase` / `topic_id` / `connected`) is gone. The merged OTA gate (`otagate`) tracks update state in memory (`Mode`/`Result` enums and an in-memory permanent-failure latch) rather than persisting a durable setup-phase machine; the relayer topic lives in `controld.state`; the live setup/provisioning state is derived from the `provisioning` machine, not a file.

Rules:
- State writes must be atomic. Use write-to-temp-then-rename (`FILE.tmp` → `FILE`).
- State files are human-readable text (JSON for `controld.state`).
- State is not a message bus. Services that need to react to changes in another service's state must use D-Bus signals, not file polling.
- `ff1-config.json` is read-only at runtime for all services. Only updater scripts write it. It does not control the local player URL.
- SSH authorized keys (`/home/feralfile/.ssh/authorized_keys`) are managed by `feral-controld` on behalf of the `sshAccess` command.

---

## Migration and Compatibility Expectations

### Btrfs snapshot system

The device uses a two-version (v1 and v2) btrfs snapshot system. Agents must not modify update or factory reset scripts without reading `docs/SNAPSHOT_SYSTEM_V2_FLOW.md`. The key invariant:

- The btrfs default subvolume (`@snapshots/@`) is only changed **after** a successful boot from a candidate subvolume. Candidates boot exactly once via `bootctl set-oneshot`.
- The marker file `var/lib/factory_reset/support_v2_root_snapshot` inside a snapshot distinguishes v2 from v1 layout. Both layouts must remain supported in the rollback initcpio hook.

Factory reset is a security-relevant special case: `feral-controld` starts `set-factory-boot.service` (via `systemctl`) which stages a one-shot boot into the pristine factory snapshot and reboots. It **abandons** the running subvolume rather than wiping it. Because the persisted relayer topic survives on the old subvolume until the reboot completes, `controld` clears the topic in-process at reset time (`clearPersistedRelayerTopic`) to close the window where a resold or interrupted device could remain commandable on the old topic.

### Service state files

State files use JSON with forward-compatible field addition. Adding new fields is safe (unknown fields are silently ignored on read). Renaming or removing fields requires both a migration and a coordinated release.

### D-Bus contracts

D-Bus interface names, method names, signal names, and payload shapes are cross-service contracts. Changing any of them requires updating all producers and consumers in the same PR and updating `docs/api-design.md`.

### Package versions

Component versions follow semantic versioning. The `ffos` build repo pins the `ffos-user` ref used for each build. Breaking changes to service APIs or behavior must be coordinated with a version bump and a matching ref update in `ffos`.

---

## Invariants Agents Must Not Break

1. `feral-sys-monitord` emits signals; it never takes recovery actions or calls other services.
2. `feral-watchdog` consumes signals; it never emits its own D-Bus health signals.
3. `feral-controld` is the only service that connects to the remote relayer.
4. CDP access (port 9222) is limited to `feral-controld` (command forwarding and setup narration) and `feral-watchdog` (recovery navigation; its health checks poll HTTP `/json/version`). No other service touches CDP.
5. Provisioning has exactly one owner (`feral-controld`, via SoftAP + captive portal). There is no BLE/GATT surface and no second provisioning path.
6. State files under `/home/feralfile/.state/` are single-owner. No two services write the same file.
7. The Chromium kiosk is launched by `start-kiosk.sh`; it does not restart Chromium after a crash — `feral-watchdog` or systemd does.
8. The LAN hub listener (port 1111) binds unconditionally when `enableHub` is on (its default); it is the LAN recovery channel and must not be re-gated on internet or relayer state. mDNS discoverability, by contrast, is link-keyed.
9. `feral-controld`'s startup brings the hub, mDNS, and provisioning up before the relayer/CDP init, and the relayer connect is never fatal. Do not reorder so that a relayer or CDP failure can abort setup.
10. `feral-sys-monitord` exposes D-Bus RPC (`GetConnectivityStatus`, `GetSysMetrics`), relied on by `feral-controld`. Do not remove without a coordinated update to all callers.
11. The unauthenticated `:1111` command API, `:1111/metrics`, and the system-wide `ip_unprivileged_port_start=80` sysctl are accepted, release-scoped surfaces whose end state is v2 screen-anchored pairing (#3471). Add LAN authorization at the hub's shared middleware chokepoint, not by diverging individual routes.
