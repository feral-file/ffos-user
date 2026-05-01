# Agent Notes: `feral-sys-monitord`

Scope: `components/feral-sys-monitord/**`

Repository-wide principles from the root `AGENTS.md` also apply here.

## Purpose

`feral-sys-monitord` is the health-signal publisher for the device.

It is responsible for:
- collecting system resource metrics
- tracking connectivity state
- watching system events
- publishing metrics and events over D-Bus
- serving Prometheus-compatible metrics

This service should stay focused on observation and publication. It should not grow recovery policy that belongs in `feral-watchdog`.

## Language and style
- Language: Go
- Follow standard Go readability guidance.
- Prefer direct, observable flows over abstraction layers that hide data movement.
- Keep emitted metrics and D-Bus payload shapes stable and obvious.
- Add comments when event semantics, sampling assumptions, or publication guarantees are not obvious.

## Architecture

### Shape
- `main.go` wires logger, config, watchdog, connectivity, DBus, monitor, event watcher, mediator, and Prometheus server.
- `metric/` owns resource collection. `SysMetrics` payload fields: `cpu`, `gpu`, `memory`, `screen`, `disk`, `uptime`, `timestamp`. This is the shape JSON-encoded into every `sysmetrics` D-Bus signal body.
- `Connectivity` owns internet-status tracking. Checks connectivity by TCP-dialing `8.8.8.8:443` `8.8.4.4:443` with a 5 s timeout each.
- `SysEventWatcher` owns system event observation. It currently emits exactly **two** events: `gpu_hanging` and `gpu_recover`. These are detected by tailing `journalctl -f -k -g i915`, matching "GPU HANG" for `gpu_hanging` and "GUC: submission enabled" for `gpu_recover`.
- `Mediator` turns monitor outputs into D-Bus signals. It subscribes to `SysResMonitor`, `Connectivity`, and `SysEventWatcher` callbacks and publishes three D-Bus signals on `com.feralfile.sysmonitord` / `/com/feralfile/sysmonitord`:
  - `sysmetrics` — emitted on every metrics collection cycle; body is JSON-encoded `SysMetrics`.
  - `connectivity_change` — emitted when online/offline state changes; body is a single `bool`.
  - `sysevent` — emitted for GPU events; body is the event string (`gpu_hanging` or `gpu_recover`).
- `dbus.go` also exposes two D-Bus RPCs on the same bus/path: `GetConnectivityStatus(refresh bool) bool` and `GetSysMetrics() SysDBusMetrics`.
- `promserver.go` exposes scrapeable Prometheus metrics at `localhost:9001`.

### Architectural direction
- This component is a producer of health information, not a decision-maker for reboot or restart policy.
- Keep metric collection separate from event publication.
- Treat D-Bus emission as an interface contract for downstream consumers such as `feral-controld` and `feral-watchdog`.
- If a new metric or event is added, document what consumes it and why.

### Amendment hazards
- Changing metric payloads or event names can silently break downstream daemons.
- Connectivity signals are operational inputs to other services; avoid casual renames or semantic drift.
- Sampling frequency, payload size, and publication timing can affect system load and consumer correctness.

## Verification for touched work
- Format changed Go files with `gofmt -s -w <changed-go-files>`.
- Run `go test ./...` in `components/feral-sys-monitord`.
- Run `go vet ./...` in `components/feral-sys-monitord`.
- Run changed-diff linting with `golangci-lint run --new-from-rev=HEAD~1 ./...` in `components/feral-sys-monitord`.

## Definition of done
A task in this component is done only when:
1. metrics, connectivity signals, and system events still have clear ownership
2. emitted contracts remain understandable and intentional
3. tests and vet pass for this module, or blockers are documented
4. comments capture any non-obvious publication or sampling constraints
5. docs stay accurate if a signal or payload contract changes

## Review flow
1. Prepare a handoff that states what signal or metric behavior changed and who consumes it.
2. Call out any contract changes, timing assumptions, or downstream compatibility risks.
3. Run the reviewer loop using `prompts/code-review.md`.
4. Only commit or ship after the review loop returns `Verdict: accept`.
