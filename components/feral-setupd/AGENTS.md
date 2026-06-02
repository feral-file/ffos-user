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

### Startup flow (`src/main.rs`)

1. **Initialize state**:
   - Create BLE service (`Ble::new()`), build `AppState` (device id, branch,
     version, `PersistentState`, `Connectivity`, etc.).
   - Connect to the local Chrome instance via CDP (`Cdp::connect`).
2. **Start BLE**:
   - Register GATT app + start advertising with a command characteristic.
   - Provide callback closures (`BleCallbacks`) for each supported BLE command.
3. **Wait for other services**:
   - Wait until `controld` is reachable (D‑Bus) before proceeding.
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

### UI control (`src/cdp.rs`)

`Cdp` is a minimal CDP client used to navigate the local launcher UI:
- QR code page (includes `device_info` query params)
- Message pages (errors, update prompts, etc.)
- Web app page after successful setup/pairing

The web app target is fixed to the bundled local player at `http://127.0.0.1:8080/`. Do not reintroduce `webapp_url` overrides from `ff1-config.json`; readiness belongs to `feral-player.service`.

### Connectivity (`src/connectivity.rs`, `src/dbus_utils.rs`)

`Connectivity` is a cloneable handle that maintains a cached “online/offline”
state using a background refresher. Use:
- `is_online_cached()` for synchronous contexts (e.g. BLE callbacks).
- `is_online(force_refresh = true)` when you need a fresh D‑Bus check.

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
| `system_update` | Optional version check (`UpdateMode::Available`); on `NoUpdateNeeded`, restores the TV page snapshot from before the check (`WebApp` → webapp, QR/message/recovery → prior surface, not unconditional webapp) |
| `upload_logs` | Uploads device logs |
| `upload_logs_with_bundle` | Uploads device logs with a `support_bundle_id` for support evidence unification |

**ACK mechanism**: `listen_for_signal` in `dbus_utils.rs` calls the registered
callback when a signal is received, then immediately emits `{member}_ack` back
on the same object/interface. The sender (`RetryableSend` in controld) retries
up to `DBUS_MAX_RETRIES` (6) times, waiting up to `DBUS_ACK_TIMEOUT` (5 s) per
attempt before resending. If no ack arrives after all retries, the send fails
with an error.

### Updater (`src/updater.rs`)

Runs/monitors the updater systemd unit, tails the updater log file, extracts
progress/messages via regex, and streams progress/error lines back to callers.

Distributor version metadata (`/api/latest/...`) is fetched with bounded retries,
each attempt capped by `UPDATER_VERSION_CHECK_REQUEST_TIMEOUT` so a stalled
connect/TLS/read fails fast (classified as network) instead of hanging the check;
failures are classified (network vs HTTP class vs parse) for TV copy. For
`UpdateExecution::Blocking` only, `check_and_update_system` attaches a progress
channel so the launcher shows a short “checking for updates” line while those
HTTP retries run; the channel is drained before final TV screens. For
`UpdateExecution::NonBlocking` (BLE), updater calls pass `None` so CDP progress
navigations are not on the mobile response path.
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
- Keep BLE parsing, UI navigation, persistence, connectivity, and updater behavior in focused modules.
- Treat BLE command payloads and `device_info` as interface contracts.
- If a change affects setup sequencing, callback ordering, or shared state, preserve the rationale in comments.

## Key data contracts

### `device_info` string

`build_device_info` builds a single string:

`<device_id>|<topic_id>|<internet>|<branch>|<version>`

Notes:
- `branch` is URL-safe encoded by replacing `/` with `%2F`.
- `internet` is `"true"`/`"false"` and uses cached connectivity.

BLE `get_info` returns exactly this single `device_info` string as a 1‑item
vector so it fits the existing BLE encoder.

There is intentionally no separate BLE `get_device_info` command; `get_info`
is the canonical source for `device_info`.

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

CI linting uses Rust toolchain `1.88.0`. The Docker image is pinned to match
that via the `Dockerfile` argument:

- Default: `RUST_TOOLCHAIN=1.88.0`
- Override: `docker build --build-arg RUST_TOOLCHAIN=1.88.0 -t arch-dev .`

## Verification for touched work

- Run these in Linux or the provided Docker environment for the touched crate:
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
