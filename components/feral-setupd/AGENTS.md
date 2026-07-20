# Agent Notes: `feral-setupd`

Scope: `components/feral-setupd/**`

Repository-wide principles from the root `AGENTS.md` also apply here.

## Purpose

`feral-setupd` is the device first-run and recovery daemon.

It is responsible for:
- serving BLE commands used by the mobile app during provisioning
- driving setup and recovery UI transitions through CDP
- coordinating with other services over D-Bus during setup
- persisting small setup-state flags
- invoking and monitoring updater flows

This component should stay focused on setup, pairing, recovery, and adjacent UX orchestration. It should not absorb broad device-policy logic that belongs elsewhere.

## Language and style
- Language: Rust
- Prefer explicit, readable Rust over clever abstractions.
- Keep async task ownership, lock boundaries, and shutdown behavior obvious.
- Add comments for protocol payloads, setup-state invariants, callback ordering, updater trade-offs, and amendment hazards.
- Avoid `unwrap` and `expect` in production paths unless the invariant is truly process-fatal and the reason is documented.

This project is Linux-only at runtime, so local checks should run in the provided Arch Linux Docker environment when possible.

## Architecture

### Module Organization (Post-Refactoring)

The codebase is organized into focused modules:

- **`main.rs`** - Entry point, orchestration, signal handling, integration tests
- **`app_state.rs`** - Core state types (`AppState`, `Page` enum)
- **`phase_logic.rs`** - Phase transition guards and validators
- **`ui.rs`** - Chrome DevTools Protocol navigation functions
- **`update_coordinator.rs`** - OTA update orchestration and retry logic
- **`callbacks.rs`** - BLE and D-Bus callback factories
- **`startup.rs`** - Initialization and startup flows
- **`dbus_handlers.rs`** - D-Bus listener setup

### Startup flow (orchestrated in `src/main.rs`, implemented across modules)

1. **Initialize state**:
   - Create BLE service (`Ble::new()`), build `AppState` (device id, branch,
     version, `PersistentState`, `Connectivity`, etc.).
   - Build the reconnecting CDP handle (`init_cdp` → `CdpHandle`) with a
     best-effort initial connect, and spawn the background reconnect loop
     (`spawn_cdp_reconnect_loop`). A failed CDP connect is **never fatal**: a
     headless device may have no Chromium at boot, and BLE/D-Bus/OTA must run
     with zero CDP (see "CDP resilience" below).
2. **Start BLE**:
   - Register GATT app + start advertising with a command characteristic.
   - Provide callback closures (`BleCallbacks`) for each supported BLE command.
3. **Wait for other services**:
   - Wait (bounded) until `controld` is reachable (D‑Bus). A timeout is **not
     fatal**: BLE is already advertising, and exiting here would kill the one
     recovery path exactly when controld is crash-looping. Every later controld
     interaction is fallible per-call; the startup pairing-topic fetch
     self-heals via `spawn_pairing_topic_retry_loop` (retries until a topic is
     persisted, then repaints the QR only if the stale-topic QR is still on
     screen — see `try_allocate_pairing_topic`).
   - Register D‑Bus listeners for UI switching/other events.
4. **Decide initial UI**:
   - Check internet status (using `Connectivity`).
   - If offline: show pairing QR code and warm the SSID cache for faster BLE
     scanning.
   - If online: continue to the “with internet” path (updates/web app).
5. **Shutdown**:
   - On SIGINT/SIGTERM, stop D‑Bus listeners and stop BLE advertising cleanly.

### BLE command flow (`src/ble.rs`, `src/encoding.rs`, `src/constant.rs`)

- The mobile app writes a payload to the command characteristic.
- The payload is parsed into `cmd`, `reply_id`, and `params[]`.
- A handler runs and responds via notification using the same `reply_id`.
- Responses use `PayloadEncoder` and generally look like:
  `reply_id`, `status_code`, followed by zero-or-more strings.

Commands are defined as string constants in `src/constant.rs`, and the handler
dispatch lives in `BleCommand::from_str` + the `match` inside the write handler.

Slow commands (`scan_wifi`, `connect_wifi`, `keep_wifi`, `factory_reset`) are
spawned from the write handler so the ATT write is acknowledged immediately.
BlueZ delivers `WriteValue` over D-Bus and fails the central's write on its
method timeout (~25 s), while nmcli + connectivity wait + version check can run
longer. The mobile contract is unaffected: results are still delivered via the
`reply_id` notification (the pattern `send_log` always used).

### BLE GATT stack resilience (`src/ble.rs`)

BlueZ silently forgets every registration a client made (GATT application AND
advertisement) when `bluetoothd` restarts or the Bluetooth controller
resets/re-enumerates. Historically only the advertisement was re-registered
after a central disconnected, which resurrected a connectable device with an
EMPTY service table — the mobile app connected instantly but discovered zero
services until a power cycle (ff-app #556, observed right after failed setup
attempts, plausibly because Wi-Fi switching on a shared Wi-Fi/BT chip can reset
the controller).

Invariants to preserve when touching this code:

- Recovery is FULL-stack: `recover_gatt_stack` drops and re-registers both the
  GATT application and the advertisement. Never re-register only one of them.
- `GattRuntime` retains the BLE callbacks and the shared notifier/monitor slots
  for the daemon's whole lifetime so the command characteristic can be rebuilt
  at any point, not just at startup.
- Recovery triggers: (1) the per-connection disconnect monitor (`BestEffort`),
  (2) the adapter watchdog (`spawn_adapter_watchdog`), which listens for
  `AdapterRemoved` / `AdapterAdded` session events (bluetoothd restart,
  controller re-enumeration) and triggers a `Forced` recovery.
- `BestEffort` recovery no-ops while a central is connected AND subscribed (a
  live notifier proves the service table is being served; tearing it down would
  kill an in-flight setup session). `Forced` recovery never honors that signal:
  after a bluetoothd restart, `StopNotify` can never arrive, so a retained
  notifier claims "subscribed" forever (gating predicate:
  `should_skip_recovery`) — the watchdog's `AdapterRemoved` handler also
  invalidates the notifier slot for the same reason. Both kinds no-op after
  `stop()`.
- Recovery retries forever with capped exponential backoff (`recovery_backoff`,
  1 s → 30 s cap) and is deduplicated via `recovery_in_flight`; a transiently
  failing BlueZ must never permanently kill the pairing surface.
- `stop()` cancels the active disconnect monitor through the shared
  `monitor_cancel` slot before tearing resources down.

### UI control (`src/cdp.rs`)

`Cdp` is a minimal, low-level CDP client used to navigate the local launcher UI:
- QR code page (includes `device_info` query params)
- Message pages (errors, update prompts, etc.)
- Web app page after successful setup/pairing

The web app target is fixed to the bundled local player at `http://127.0.0.1:8080/`. Do not reintroduce `webapp_url` overrides from `ff1-config.json`; readiness belongs to `feral-player.service`.

### CDP resilience (`src/cdp.rs`, `src/startup.rs`, `src/ui.rs`)

`feral-setupd` starts unconditionally at boot (its unit is no longer gated on
`chromium-ready.target` and no longer `PartOf` it), so the CDP endpoint at
`CDP_URL` may be **absent at startup**, appear later (monitor plugged in), and
disappear/reappear whenever the kiosk restarts. All CDP work therefore goes
through `CdpHandle`, a reconnecting front over `Cdp`:

- **Never fatal on CDP.** `init_cdp` returns a handle even if the first connect
  fails; `run()` no longer `?`-propagates a CDP error. `CdpHandle::navigate`
  is infallible by contract (drops silently when disconnected) — the whole UI
  layer and every BLE/D-Bus reply rely on this. `show_webapp`'s
  `get_current_url` fast-path must **not** propagate failure (it did — old Fatal
  path 2); a failure is treated as "not on webapp" and falls through to a
  best-effort navigate.
- **Every CDP I/O is hard-timeout-bounded.** All HTTP `/json` fetches funnel
  through `fetch_json_targets` (`CDP_HTTP_FETCH_TIMEOUT`), the WS dial through
  `connect_ws` (`CDP_WS_CONNECT_TIMEOUT`), and commands through `send_cmd`'s 3 s
  reply timeout. Never add a raw `reqwest::get`/`connect_async` on a CDP path: a
  wedged DevTools endpoint that accepts TCP but never responds would hang
  `init_cdp` before `start_ble` and stall `show_webapp` while it holds the page
  lock (PR #218 review).
- **Lazy connect + reconnect.** `spawn_cdp_reconnect_loop` retries connecting on
  `CDP_RECONNECT_INTERVAL` while disconnected. A transport-dead error
  (`Error::is_transport_dead`: socket/HTTP failures, not command timeouts —
  Chromium often renders without replying) drops the cached connection so the
  loop rebuilds it. Chromium restarts mint a new page target (new ws URL), so
  reconnect always re-reads `/json`; the loop also proactively drops a silently
  stale socket via `connection_is_current` (HTTP-only ws-URL compare) every
  `CDP_LIVENESS_CHECK_INTERVAL`.
- **UI resync on (re)connect.** On every successful (re)connect the loop calls
  `ui::resync_canonical_page`, which maps `app_state.page` + phase through
  `restore_page_target` and re-shows the canonical surface — otherwise a kiosk
  that (re)started after setupd would sit on the launcher logo forever. It only
  repaints (never runs flow logic), is skipped while `update_in_progress` (the
  updater owns the screen), and re-asserts the failure surface for a latched
  `UpdateFailed` (whose canonical `SystemUpgrade` page `restore_page_target`
  would otherwise route to QR).

### Connectivity (`src/connectivity.rs`, `src/dbus_utils.rs`)

`Connectivity` is a cloneable handle that maintains a cached “online/offline”
state using a background refresher. Use:
- `is_online_cached()` for synchronous contexts (e.g. BLE callbacks, `build_device_info`).
- `is_online(force_refresh = true)` when you need a synchronous fresh D‑Bus check.
- `trigger_refresh_async()` from BLE `get_info` while `setup_phase` is
  `checking_version` or `updating`. This schedules a background D‑Bus refresh
  without blocking the BLE notification path; the updated value is visible on
  the next poll.

### Persistent state (`src/persistent_state.rs`)

Small key/value file store used for setup flags (e.g. `topic_id`, `connected`,
`paired`). Keep it human-readable and small; treat I/O errors as actionable in
daemon paths.

### D-Bus signals received (`src/dbus_utils.rs`, `src/constant.rs`)

`feral-setupd` listens for signals sent by `feral-controld` on controld's
own bus. They arrive on:
- Bus name: `com.feralfile.controld`
- Object path: `/com/feralfile/controld`
- Interface: `com.feralfile.controld.general`

| Signal member | What setupd does |
|---|---|
| `show_pairing_qr_code` | Navigates CDP to the QR code page |
| `factory_reset` | Starts the factory-reset flow |
| `system_update` | Optional version check (`UpdateMode::Available`); on `NoUpdateNeeded`, re-shows the **current** canonical page (read after the check, once the progress task is drained) so the TV leaves the transient "Checking for updates..." URL. Re-showing `current` (not the pre-check snapshot) both fixes a stuck transient screen and re-asserts any page another operation navigated to during the check — never clobbering it (`SystemUpgrade`/`None` have no surface → no-op) |
| `upload_logs` | Uploads device logs |
| `upload_logs_with_bundle` | Uploads device logs with a `support_bundle_id` for support evidence unification |

**ACK mechanism**: `listen_for_signal` in `dbus_utils.rs` calls the registered
callback when a signal is received, then immediately emits `{member}_ack` back
on the same object/interface. The sender (`RetryableSend` in controld) retries
up to `DBUS_MAX_RETRIES` (6) times, waiting up to `DBUS_ACK_TIMEOUT` (5 s) per
attempt before resending. If no ack arrives after all retries, the send fails
with an error.

### Update coordination (`src/update_coordinator.rs` + `src/updater.rs`)

Runs/monitors the updater systemd unit, tails the updater log file, extracts
progress/messages via regex, and streams progress/error lines back to callers.

`check_and_update_system` uses an RAII `UpdateGuard` on `update_in_progress`
to serialize OTA attempts. Blocking startup failures after permanent update
recovery must keep setupd alive on QR so mobile polling can observe
`setup_phase=update_failed`; do not propagate those failures out of `run()`.

Distributor version metadata (`/api/latest/...`) is fetched with bounded retries,
each attempt capped by `UPDATER_VERSION_CHECK_REQUEST_TIMEOUT` so a stalled
connect/TLS/read fails fast (classified as network) instead of hanging the check;
failures are classified (network vs HTTP class vs parse) for TV copy. For
`UpdateExecution::Blocking` only, `check_and_update_system` attaches a progress
channel so the launcher shows a short "checking for updates" line while those
HTTP retries run; the channel is drained (`finish()`) before any final TV screen.
Progress navigations are **transient** (`navigate_transient_message`) — they update
the TV but never record the canonical `app_state.page`, so a lagging progress write
can never overwrite a page another operation set. Because the screen may still be on
the transient URL afterwards, the no-update path re-shows the current canonical page
(read after the task is drained) to leave it. For `UpdateExecution::NonBlocking`
(BLE), updater calls pass `None` so CDP progress navigations are not on the mobile
response path.
`check_and_update_system` begins with a forced `refresh_remote_version`; when that
live fetch fails it surfaces the classified error and returns `VersionCheckFailed`
rather than falling back to stale cached metadata (the decision helpers then read
the freshly refreshed cache). The hourly background refresher instead ignores
failures and keeps serving the last-known cache.
Only `refresh_remote_version`, `is_too_old_to_upgrade`, `is_update_required`, and
`is_update_available` accept that channel; helpers like `latest_version` read
metadata without driving the progress UI.
Two enums control update behaviour:

- `UpdateMode::Required` — check only against the distributor's minimum
  supported version; update is mandatory if the running build is below it.
- `UpdateMode::Available` — check against the latest published version; update
  is optional/user-triggered.

- `UpdateExecution::Blocking` — run update operations in the foreground. Used
  during startup and D-Bus callback flows where we can wait.
- `UpdateExecution::NonBlocking` — spawn update operations in background tasks
  and return immediately. Used in BLE flows where a response must be sent to
  the mobile app without delay.

## Architectural direction
- Keep `src/main.rs` as lifecycle and orchestration glue, not a dumping ground for unrelated logic.
- Keep modules focused: each module has a single, clear responsibility.
- Keep BLE parsing, UI navigation, persistence, connectivity, and updater behavior in focused modules.
- Treat BLE command payloads and `device_info` as interface contracts.
- If a change affects setup sequencing, callback ordering, or shared state, preserve the rationale in comments.
- Tests are distributed across modules: unit tests live with their modules, integration tests in `main.rs`.

## Key data contracts

### `device_info` string

`build_device_info` builds a single string:

`<device_id>|<topic_id>|<internet>|<branch>|<version>|<setup_phase>`

Notes:
- `branch` is URL-safe encoded by replacing `/` with `%2F`.
- `internet` is `"true"`/`"false"` and uses cached connectivity.
- `setup_phase` reflects the current setup progress. Possible values: `idle`,
  `wifi_connecting`, `checking_version`, `updating`, `update_failed`,
  `pairing`, `ready`.
- When `setup_phase` is `updating` or `checking_version`, `get_info` triggers
  an asynchronous background refresh of internet connectivity (non-blocking).
  The fresh value becomes available on the next poll (mobile polls every few
  seconds during setup).

BLE `get_info` returns exactly this single `device_info` string as a 1‑item
vector so it fits the existing BLE encoder.

There is intentionally no separate BLE `get_device_info` command; `get_info`
is the canonical source for `device_info`.

The sixth field (`setup_phase`) is an additive extension; older firmware that
omits it should be treated as `idle` by mobile clients.

#### `setup_phase` state machine

Typical success path: `idle` → `wifi_connecting` → `checking_version` →
`updating` → `pairing` → `ready`.

Failure and recovery paths:
- Wi‑Fi connect or no-internet failures reset to `idle` before returning BLE errors.
- Version check with no update needed resets `checking_version` → `idle`.
- Permanent update failure sets `update_failed` and stays on the failure message UI.
  The mobile app can observe `update_failed` via polling and trigger retry via BLE/D-Bus.
- Normal QR navigation resets non-failure phases to `idle`.

Phase invariants and startup behavior:
- `Pairing` phase requires `topic_id` to exist in persistent state. On restore, if
  `topic_id` is missing, the phase is auto-corrected to `Idle`.
- On startup, mandatory update check runs for ALL phases except `UpdateFailed`,
  maintaining consistency: first-time setup checks before entering `Pairing`,
  reboot with `Pairing` checks again for new mandatory updates, and reboot with
  `Ready` checks to keep device up-to-date.

Classify updater log messages before wrapping them in `anyhow` context so
permanent vs transient retry policy stays accurate.

### Log upload support bundle

BLE `send_log` keeps the existing first three parameters (`user_id`, `api_key`,
`title`) and accepts an optional fourth `support_bundle_id`. D-Bus keeps the
original `upload_logs(user_id, api_key, title)` signal and uses additive
`upload_logs_with_bundle(payload []byte)` when controld needs to attach FF1
logs to a support bundle. The bundled payload is JSON and includes `user_id`,
`api_key`, `title`, and `support_bundle_id`.

## Keep this file updated

If you change behavior, commands, toolchain versions, or data contracts in code,
also update `AGENTS.md` in the same PR so future work stays consistent (e.g.
changing BLE commands/payloads, `device_info` format, Docker toolchain pinning,
or required lint/test steps).

When non-obvious logic changes, prefer intent-rich comments that preserve the
reasoning, invariants, and trade-offs for future amendment sessions. This is
especially important for BLE payload handling, DBus callbacks, UI navigation
decisions, updater behavior, and shared-state synchronization.

### Toolchain

CI linting uses Rust toolchain `1.88.0` on Ubuntu.

Local verification matching CI:
- Run `scripts/verify-setupd-docker.sh` (uses `ubuntu:latest` + Rust 1.88.0)
- Runs all checks: fmt, clippy, check, test

The `components/feral-setupd/Dockerfile` provides an Arch Linux development
environment for other use cases, but is not used for CI verification.

## Verification for touched work

- Run `scripts/verify-setupd-docker.sh` from repo root (matches CI environment)
- Or run these manually in Linux for the touched crate:
  - `cargo fmt --all -- --check`
  - `cargo check --all-targets --all-features`
  - `cargo clippy --all-targets --all-features -- -D warnings`
  - `cargo test --all-targets --all-features`

If a command reports warnings that indicate code changes are needed, fix them before committing unless the team explicitly agrees to keep that warning class.

## Definition of done
A task in this component is done only when:
1. setup, pairing, and recovery ownership remains clear
2. touched crate checks pass, or blockers are documented
3. comments preserve the why behind non-obvious setup sequencing, payload contracts, or updater behavior
4. BLE, D-Bus, and UI navigation contracts remain intentional
5. this file stays accurate when flows or toolchain expectations change

## Review flow
1. Prepare a handoff that states which setup or recovery behavior changed and how the flow is affected.
2. Call out BLE payload changes, D-Bus callback assumptions, persistence changes, or updater trade-offs.
3. Run the reviewer loop using `prompts/code-review.md`.
4. Only commit or ship after the review loop returns `Verdict: accept`.

## Reusing Docker Containers (don’t respawn each time)

One-shot `docker run --rm ...` is convenient but slow. Prefer a long-lived dev
container and use `docker exec` for repeated lint/test runs.

### Create once (if missing)

1. Build the image (only when Dockerfile changes):
   - `make docker-build` (or `docker build -t arch-dev .`)
2. Create a persistent container:
   - `docker run -dit --name feral-setupd-dev -v "$(pwd)":/workspace -w /workspace arch-dev sleep infinity`

Optional: mount your host cargo cache to speed up crate downloads:
- `-v "$HOME/.cargo":/usr/local/cargo`

### Reuse (fast path)

- Start (if stopped): `docker start feral-setupd-dev`
- Run commands:
  - `docker exec -it feral-setupd-dev sh -lc "cargo fmt -- --check"`
  - `docker exec -it feral-setupd-dev sh -lc "cargo check"`
  - `docker exec -it feral-setupd-dev sh -lc "cargo clippy"`
  - `docker exec -it feral-setupd-dev sh -lc "cargo test"`

### Cleanup (only when needed)

- Remove the container if it gets wedged:
  - `docker rm -f feral-setupd-dev`
