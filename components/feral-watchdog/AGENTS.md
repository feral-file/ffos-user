# Agent Notes: feral-watchdog

Repository-wide AGENTS.md and its review/release rules apply. Read README.md
for the current recovery contract; the native cutover replaces CDP monitoring.

## Ownership and invariants

- This daemon owns recovery policy. Sys-monitord supplies raw health telemetry;
  controld supplies external control; the native host owns artwork recovery.
- Probe the actual private player command socket, never CDP or a web server.
  Validate peer UID, frame bounds, protocol version, identity and deadlines.
  A responsive host reporting renderer failure is healthy for process recovery.
- Keep 90-second cold/restart grace, 20-second steady failure threshold,
  three restarts/five minutes, 15-minute fallback hold, and persisted one-reboot
  cap. Preserve the unavailable/busy/shown fallback distinction.
- Preserve headless, VT, updater-lock and activating-unit suppression. The DRM
  predicate must stay in lockstep with controld and start-kiosk.sh. Update holds
  remain capped at eight hours and must never acquire the updater lock.
- `chromium-kiosk.service` and existing metric names are operational identifiers,
  not evidence of CDP ownership. It now launches the native CEF host under Cage.
- RAM critical remains 95%: restart after 15 seconds, reboot after 60 seconds.
  GPU recovery keeps its existing delayed escalation and cancellation contract.
- CPU protection is a level-triggered native hold after ten seconds at 93 C,
  cleared below 88 C. Reaffirm levels after host restarts; preserve durable
  playback. Service-health recovery must never replace the current playlist.
- The controld OTA-stop exemption requires both `/run/feral-controld-ota-stop`
  and the updater lock; keep the marker aligned with ffos ota-control-state.sh.
- SystemdWatchdog only sends sd_notify keepalives. Incident metrics are
  observations, not a second recovery-policy source.

## Implementation and verification

Use Go, small coordinators, cancellable loops and explicit observation/action
boundaries. Document threshold changes, shutdown semantics and trade-offs.
Format touched files; run `go test ./...`, `go vet ./...`, and
`golangci-lint run --new-from-rev=HEAD~1 ./...` in this module. Run race tests for
concurrent changes and update README when behavior changes. Run one fresh-context
completion review with `prompts/code-review.md`; its verdict is observability,
not commit or release authority. Anh owns disposition and release signoff.
