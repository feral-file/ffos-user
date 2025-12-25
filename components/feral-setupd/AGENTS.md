# Agent Notes: `feral-setupd`

Scope: `components/feral-setupd/**`

## What This Service Does (High-Level)

`feral-setupd` is the device “first-run / recovery” daemon for FF1. It owns the
setup UX and orchestration:

- Starts and serves a BLE GATT service so the mobile app can provision Wi‑Fi and
  trigger maintenance actions.
- Drives the on-device UI via CDP (Chromium DevTools Protocol) by navigating the
  launcher UI to QR code / message / web app pages.
- Coordinates with other services over D‑Bus (e.g. waits for `controld`,
  listens for page switch events).
- Tracks small persistent flags (topic id, “ever connected”, “paired”) in a tiny
  text file on disk.
- Can trigger/monitor the updater and forward progress/error output.

This project is Linux-only at runtime (BlueZ, D‑Bus, systemd/log paths), so local
checks should run in the provided Arch Linux Docker environment.

## Runtime Architecture (How It Works)

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

### Connectivity (`src/connectivity.rs`, `src/dbus_utils.rs`)

`Connectivity` is a cloneable handle that maintains a cached “online/offline”
state using a background refresher. Use:
- `is_online_cached()` for synchronous contexts (e.g. BLE callbacks).
- `is_online(force_refresh = true)` when you need a fresh D‑Bus check.

### Persistent state (`src/persistent_state.rs`)

Small key/value file store used for setup flags (e.g. `topic_id`, `connected`,
`paired`). Keep it human-readable and small; treat I/O errors as actionable in
daemon paths.

### Updater (`src/updater.rs`)

Runs/monitors the updater systemd unit, tails the updater log file, extracts
progress/messages via regex, and streams progress/error lines back to callers.

## Key Data Contracts

### `device_info` string

`build_device_info` builds a single string:

`<device_id>|<topic_id>|<internet>|<branch>|<version>`

Notes:
- `branch` is URL-safe encoded by replacing `/` with `%2F`.
- `internet` is `"true"`/`"false"` and uses cached connectivity.

BLE `get_info` returns exactly this single `device_info` string as a 1‑item
vector so it fits the existing BLE encoder.

## Development & CI Parity (Docker + Toolchain)

## Keep This File Updated

If you change behavior, commands, toolchain versions, or data contracts in code,
also update `AGENTS.md` in the same PR so future work stays consistent (e.g.
changing BLE commands/payloads, `device_info` format, Docker toolchain pinning,
or required lint/test steps).

### Toolchain

CI linting uses Rust toolchain `1.88.0`. The Docker image is pinned to match
that via the `Dockerfile` argument:

- Default: `RUST_TOOLCHAIN=1.88.0`
- Override: `docker build --build-arg RUST_TOOLCHAIN=1.88.0 -t arch-dev .`

### Pre-commit lint gate (required)

Before committing changes to this component, run these in Linux (Docker) and
stop if anything fails or produces diffs/warnings that require action:

- `cargo fmt -- --check`
  - If it fails, run `cargo fmt` and re-check.
- `cargo check`
- `cargo clippy`

If a command reports warnings that indicate code changes are needed, fix them
before committing (don’t “paper over” warnings unless the team explicitly agrees
to allow that warning class).

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
