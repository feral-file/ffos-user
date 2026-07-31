# Release-rail evidence ledger

Consumed by `scripts/check-release-rail.sh`, which runs in the
release-guardrail workflow on every PR into `staging`/`release`. A release whose
diff touches BOTH shipping rails — component binaries (`components/**`, pacman
package rail) AND user units/session scripts (`users/**`, full-image rsync
rail built in the `ffos` repo) — must add an entry here declaring the
full-image release, or the PR cannot merge. See AGENTS.md "Release guardrail:
two shipping rails".

An entry is the durable record that the release was cut on the full-image
rail: it names the version and the exact `ffos` image-build dispatch that
ships the units/scripts alongside the new binaries. Newest first.

## 1.1.0 — full-image

- Cross-rail changes: `components/feral-setupd` (the Rust BLE provisioning
  daemon) and the launcher-ui setup surface are DELETED; `components/feral-controld`
  absorbs device setup end to end (SoftAP + captive-portal provisioning, LAN
  hub recovery on `:1111`, claim QR auto-trigger, on-screen setup narration
  via ff-player's `setupDisplay` contract) — paired with `users/feralfile/**`
  unit and script edits (`feral-setupd.service` removed, `.start-services.sh`
  ordering, `serve-feral-player.sh` setupDisplay contract gate, kiosk/session
  scripts).
- Release action: dispatch `build-image-to-cf.yml` in the `ffos` repo with
  `version=1.1.0` and `ffos_user_ref` pointing at the staging merge tag of
  PR #232, on an `ffos` ref that includes feral-file/ffos#118 (build/image
  manifests pruned of setupd/launcher-ui; captive-DNS + nftables portal
  plumbing). The two PRs ship together or not at all.
- A package-only rollout is NOT permitted: old units would still try to start
  the deleted setupd service and never expose the SoftAP/claim path — a
  fielded device that lost Wi-Fi would have NO provisioning surface at all.

## 1.0.21 — full-image

- Cross-rail changes: daemon behavior in `components/feral-controld`,
  `components/feral-setupd`, and `components/feral-watchdog` (headless
  operation, BLE-recovery independence, CDP/DDC resilience) paired with
  `users/feralfile/**` unit and session-script edits (`.start-services.sh`
  daemon-first ordering, `feral-setupd.service` decoupled from
  player/controld, `start-kiosk.sh` display wait, `chromium-ready.target`
  decoupling).
- Release action: dispatch `build-image-to-cf.yml` in the `ffos` repo with
  `version=1.0.21` and `ffos_user_ref` pointing at the `v1.0.21` tag (the
  staging merge of this PR), so the image carries these units/scripts together
  with the new binaries.
- A package-only 1.0.21 rollout is NOT permitted: it would run the new daemons
  under the old units/scripts, silently reverting the startup-ordering and
  headless-recovery guarantees this release exists to ship.
