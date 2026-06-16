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
| `feral-controld` | Maintain the relayer connection and route remote commands to local handlers | Go |
| `feral-setupd` | BLE provisioning, setup/recovery UI transitions, updater orchestration | Rust |
| `launcher-ui` | Launch Chromium in kiosk mode at the correct URL and exit | Go |
| `mint-pairing-ui` | Render the mint pairing QR code page when `feral-controld` starts a browser handoff pairing session | HTML/JS |
| `player-wrapper-ui` | Wrap the media player process | Go |

Rules for each boundary:

- `feral-sys-monitord` is a **publisher only**. It must not make recovery decisions, reboot the system, or call other services. Callers pull from it via RPC or subscribe to its signals.
- `feral-watchdog` is the **single owner of recovery policy**. It decides when to restart Chromium, clean disk pressure, or reboot. Raw telemetry collection does not belong here; that belongs in `feral-sys-monitord`.
- `feral-controld` is the **connectivity and command orchestration hub**. It must not absorb device-policy logic that belongs in `feral-watchdog` or setup logic that belongs in `feral-setupd`. It is the highest-risk service for architectural sprawl; resist it.
- `feral-setupd` is the **setup and recovery UX owner**. Its scope is first-run provisioning, pairing, recovery UI, and updater orchestration. It should not grow into a general device-policy daemon.
- `launcher-ui` and `player-wrapper-ui` are **thin process starters**. They contain no business logic. Parameters come from command-line arguments. State and control live in daemons, not in these wrappers.

---

## Cross-Service Communication Contracts

### Primary IPC: D-Bus (session bus, user scope)

D-Bus is the canonical inter-service transport on-device. All cross-service communication between daemons happens over D-Bus unless noted otherwise.

**Signal direction** (one-way, fire-and-forget):

```
feral-sys-monitord  --[sysmetrics]-----------> feral-controld
                    --[sysmetrics]-----------> feral-watchdog
                    --[connectivity_change]--> feral-controld
                    --[connectivity_change]--> feral-watchdog
                    --[sysevent]-------------> feral-watchdog

feral-controld      --[show_pairing_qr_code]-> feral-setupd
                    --[factory_reset]--------> feral-setupd
                    --[system_update]--------> feral-setupd
                    --[upload_logs]----------> feral-setupd
                    --[upload_logs_with_bundle]-> feral-setupd
```

**RPC direction** (request/response):

```
feral-controld  --[GetConnectivityStatus]--> feral-sys-monitord
feral-setupd    --[GetConnectivityStatus]--> feral-sys-monitord
feral-setupd    --[GetRelayerTopicID]------> feral-controld
```

### External transport: WebSocket relayer

`feral-controld` maintains a persistent WebSocket connection to the remote relayer endpoint. Remote commands from the mobile app arrive over this connection. `feral-controld` routes them locally to either the device executor (device-control commands) or Chromium via CDP (web/playback commands). No other service connects to the relayer.

### UI control: Chrome DevTools Protocol (CDP)

Daemons control the Chromium kiosk instance over CDP (HTTP + WebSocket to `127.0.0.1:9222`). `feral-setupd` drives setup UI pages (QR code, messages, and the bundled local webapp). `feral-controld` forwards web commands from the relayer to Chromium via CDP. `feral-watchdog` monitors Chromium health and issues recovery commands via CDP. Neither daemon embeds a web server or serves UI assets directly.

`feral-player.service` is the readiness gate for the bundled local webapp. Chromium kiosk and any daemon that navigates to the local player must wait for that unit to report `READY=1`.

### Local device control: Hub WebSocket (port 1111)

`feral-controld` exposes a local WebSocket server on `0.0.0.0:1111` when `enableHub` is true in config. This hub accepts the same command format as the relayer and is used for local-network control (e.g. from a companion app on the same network). mDNS advertises hub availability. The hub is not a replacement for the relayer; it is an optional local control path.

### Panel control: DDC/CI via `ddcutil`

`feral-controld`’s `devicectl` executor drives the attached panel over DDC/CI using the `ddcutil` CLI. Remote or hub commands `ddcPanelControl` and `ddcPanelStatus` map to brightness, contrast, speaker volume, mute, and power VCPs on the default display; the helper wraps `ddcutil` with a lightweight retry/recovery run when the tool reports display-not-found or missing VCP output.

### Mobile provisioning: BLE GATT (Bluetooth Low Energy)

`feral-setupd` exposes a BLE GATT service used by the mobile app during first-run provisioning. BLE is the setup channel only; it is not used for runtime device control or command routing. No other service registers a GATT service.

### Service discovery: mDNS

`feral-controld` advertises the device on the local network via mDNS when hub is enabled and internet is connected. The advertisement includes the device ID, name, and hub port (1111).

---

## Launcher UI and Daemon Logic Ownership

The boundary between launcher/UI code and daemon logic:

- **Daemons own all state, policy, and side effects.** Daemons decide what page to show, when to update, and what to do on errors.
- **Chromium (via CDP) renders the UI.** Pages are HTML/JS served from `file:///opt/feral/ui/launcher/`, `file:///opt/feral/ui/mint-pairing/`, or from the bundled local player at `http://127.0.0.1:8080/`. Daemons navigate by calling CDP, not by modifying files on disk at runtime.
- **`launcher-ui` is a one-shot process starter.** It constructs a URL from command-line arguments (key=value pairs), launches Chromium with `cage` as the Wayland compositor, and waits. It contains no business logic and no daemon lifecycle. Arguments come from the systemd unit; they do not change at runtime.
- **UI does not call daemons directly** except through the Hub WebSocket (when local control UI in Chromium sends commands to controld on port 1111). All other control flows originate in daemons and push into Chromium via CDP.
- **`player-wrapper-ui`** follows the same pattern: thin process wrapper, no policy.

When adding new behavior that spans UI and daemon logic:
1. The state and decision logic goes in a daemon (usually `feral-controld` or `feral-setupd`).
2. The daemon issues a CDP call to navigate or execute JavaScript in Chromium.
3. The UI renders what it is told.

---

## Persistence and State Ownership

Each service owns its own state files exclusively. No service should read or write another service's state file.

| Owner | File | Contents |
|---|---|---|
| `feral-controld` | `/home/feralfile/.state/controld.state` | Relayer topic ID, connected device (ID, name, platform) |
| `feral-controld` | `/home/feralfile/.state/screen-orientation` | Last committed screen orientation value |
| `feral-setupd` | `/home/feralfile/.state/setupd` | Setup flags: `paired`, `topic_id`, `connected` |
| updater scripts | `/home/feralfile/ff1-config.json` | Device branch, current version, update channel URLs (read-only at runtime by services) |
| system | `/etc/hostname` | Device hostname (read-only at runtime; used by `controld` for mDNS identity) |
| earlyoom/oom-state | `/var/lib/oom_state/chromium-oom-kill-count` | Chromium OOM kill count (read by `controld` OOM recoverer) |
| earlyoom/oom-state | `/var/lib/oom_state/chromium-oom-kill-handled-count` | Handled OOM kill count (written by `controld` OOM recoverer) |
| `feral-watchdog` | `/home/feralfile/.state/failed_recovery_version` | Version of a recovery candidate that failed to boot |

Rules:
- State writes must be atomic. Use write-to-temp-then-rename (`FILE.tmp` → `FILE`).
- State files are human-readable JSON. Add fields additively; never rename or remove fields without a migration path.
- State is not a message bus. Services that need to react to changes in another service's state must use D-Bus signals, not file polling.
- `ff1-config.json` is read-only at runtime for all services. Only updater scripts write it. It does not control the local player URL.
- SSH authorized keys (`/home/feralfile/.ssh/authorized_keys`) are managed by `feral-controld` on behalf of the `sshAccess` command.

---

## Migration and Compatibility Expectations

### Btrfs snapshot system

The device uses a two-version (v1 and v2) btrfs snapshot system. Agents must not modify update or factory reset scripts without reading `docs/SNAPSHOT_SYSTEM_V2_FLOW.md`. The key invariant:

- The btrfs default subvolume (`@snapshots/@`) is only changed **after** a successful boot from a candidate subvolume. Candidates boot exactly once via `bootctl set-oneshot`.
- The marker file `var/lib/factory_reset/support_v2_root_snapshot` inside a snapshot distinguishes v2 from v1 layout. Both layouts must remain supported in the rollback initcpio hook.

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
4. CDP access (port 9222) is used only by `feral-controld`, `feral-setupd`, and `feral-watchdog`.
5. BLE provisioning is owned entirely by `feral-setupd`. No other service registers a GATT service.
6. State files under `/home/feralfile/.state/` are single-owner. No two services write the same file.
7. `launcher-ui` exits after Chromium exits. It does not restart Chromium; `feral-watchdog` or systemd does that.
8. The Hub WebSocket (port 1111) is optional (`enableHub` config flag). No service depends on it being present.
9. `feral-controld` exposes D-Bus RPC to other services (`GetRelayerTopicID`). It must not remove this method without a coordinated update to all callers.
10. `feral-sys-monitord` exposes D-Bus RPC (`GetConnectivityStatus`, `GetSysMetrics`). These are relied on by `feral-controld` and `feral-setupd` at startup.
