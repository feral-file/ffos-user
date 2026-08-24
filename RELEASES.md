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

## 2.0.3 — full-image

Everything on `develop` since the previous staging merge — four PRs
(#287, #290, #294, #295; staging PR #299):

- **Wake-on-LAN at startup** (#287) — the cross-rail change forcing the
  full-image rail.
- **WAN-outage observability, stages 0–2** (#290): connectivity gauges
  (`netmetrics`), an in-memory flight recorder with a probe ladder
  (`netlog`), outage classification (`status`), and diagnosis egress via
  the log-upload bundle (controld + sys-monitord).
- **dp1-go v0.6.0 bump** (#294) so `inlineManifest` survives to the player.
- **Screenshot API** (#295): bounded native capture in controld.

### Cross-rail changes

Image rail (`users/**`):

- NEW `enable-wake-on-lan.service` (timeout-bounded oneshot,
  `TimeoutStartSec=15`) and `scripts/enable-wake-on-lan.sh`: arms magic-packet
  wake on Ethernet adapters that support it (ethtool + PCI wake source) and
  persists `802-3-ethernet.wake-on-lan magic` on NetworkManager Ethernet
  profiles; Wi-Fi profiles and wireless interfaces are deliberately left
  untouched.
- `.start-services.sh`: starts the unit with `--no-block` right after
  `feral-controld.service`, best-effort, so a wedged NIC/NetworkManager/sudo
  call cannot delay the rest of FF OS startup.
- `.file_permissions.sh`: marks the new helper executable.

The helper's contract is pinned CI-side by `scripts/test-enable-wake-on-lan.sh`
(run via `make verify-scripts`).

Package rail (`components/**`): the controld/sys-monitord changes listed
above (#290, #294, #295).

### Release action

Dispatch `build-image-to-cf.yml` in the `ffos` repo with `version=2.0.3` and
`ffos_user_ref` pointing at the `v2.0.3` tag (the staging merge of this
release).

### Why package-only is NOT permitted

The Wake-on-LAN unit, helper script, and startup-script hook ship ONLY on the
full-image rsync rail. A package-only bump would roll out the new binaries
while silently dropping the entire Wake-on-LAN feature on fielded devices —
no unit file, no helper, nothing starting it.

## 2.0.0 — full-image

Everything since `v1.0.21`: 317 commits on `develop` (319 counting the two
staging merge commits; the trees are identical), PRs #221–#285. **Supersedes the
1.1.0 declaration below**, which was written for the setupd merge (PR #232)
but never cut — no `v1.1.0` tag exists — and the staging bundle has since
grown well past a minor bump. Everything 1.1.0 declared ships here.

Major version because the device's provisioning surface is replaced, not
extended: BLE is gone, two components are deleted, and the kiosk's entry
point changes.

> Not to be confused with the **FF1 communication API v2** documents added in
> this release (`docs/ff1-v2-api-contract.md`, `ff1-v2-controller-authentication.md`,
> `ff1-v2-migration.md`). Those are design drafts whose own target contract
> version is also `2.0.0`, and they say so explicitly: they are *not*
> conformance-ready and nothing in this release implements them. The
> firmware version here and that contract version are unrelated numbers.

### Cross-rail changes

Package rail (`components/**`) and image rail (`users/**`, `scripts/**`) both
move, so this MUST be a full-image release.

Image rail:

- `feral-setupd.service` DELETED.
- `feral-controld.service`: `StartLimitBurst`/`StartLimitIntervalSec=600`
  replaced by `StartLimitIntervalSec=0`. controld is now the device's ONLY
  provisioning path, so fast repeated crashes must degrade into slow
  `Restart=always` retries, never latch into a permanent `start-limit-hit`
  dead state that strands setup on an offline device.
- `start-kiosk.sh`: the kiosk entry point changes from
  `file:///opt/feral/ui/launcher/index.html?step=logo` to
  `http://127.0.0.1:8080/` (the player bundle) — launcher-ui no longer
  exists. Adds a Chromium-cache purge keyed to a content fingerprint of the
  player bundle, so a bundle swap cannot leave the wall on the previous app
  or a ChunkLoadError page (#234).
- `serve-feral-player.sh`: default-required `setupDisplay` player-contract
  gate (`FF_PLAYER_REQUIRE_SETUP_DISPLAY_CONTRACT=1`) — a bundle without it
  must fail readiness rather than leave a fresh offline device with no
  displayed setup credentials; plus a global `Cache-Control: no-cache` from
  darkhttpd behind a bounded, non-fatal `--header` support probe.
- `.start-services.sh`: controld started FIRST with `--no-block`, and every
  remaining start/enable made best-effort with a `WARN` breadcrumb. Under
  `set -euo pipefail` one failed unit previously aborted the rest of boot —
  no kiosk, no watchdog, and none of the update timers that are the device's
  only path to self-heal a bad version.
- `chromium-ready.target`: stale setupd references removed (it already pulls
  in no services).

Package rail — `components/feral-controld` absorbs the entire setup domain
and gains three new subsystems:

- **Setup/provisioning (the setupd merge).** `components/feral-setupd` (the
  Rust BLE provisioning daemon) and `components/launcher-ui` are DELETED; the
  repository contains no Rust. controld now owns SoftAP + captive-portal
  provisioning, `nmcli` scan/join, the OTA gate, claim QR, factory reset, log
  upload, and on-screen setup narration via ff-player's `setupDisplay` CDP
  contract (`softap`, `portal`, `provisioning`, `wifictl`, `otagate`,
  `setupui`, `devicectl`). Rationale in `docs/architecture.md`, "The setupd
  merge"; the flow itself in `docs/setup-flow.md`.
- **Network dead-end recovery.** Escape policy with bounded AP sessions, a
  recheck backoff ladder, a setup-incomplete episode, `setup_error`
  escalation, a `WiredLink` ethernet-only probe seam, a unified network
  health surface, and the app-triggered `startWifiSetup` command. AP raise is
  gated on confirmed *link* loss rather than internet loss, and NM
  autoconnect is suppressed across the rescan AP bounce.
- **Offline artwork cache** (`offlinecache`, `docs/offline-artwork-capture.md`):
  headless-Chromium capture of web-based DP-1 artworks and browser-free
  direct download for single-file mime types, keyed on item source URL, with
  a disk budget (`diskUsed`/`diskLimit`), startup GC, eviction, load-aware
  download scheduling, and CDP replay. Manifest-based streaming (HLS/DASH) is
  explicitly unsupported.
- **`displayAt` scheduling** (`playlistschedule`, `playlist-refresher`) and
  player-session recovery (`playersession`,
  `docs/player-session-recovery.md`), including `refreshArtwork` recovering a
  dead player page via `Page.reload`.
- **Boot hardening.** Boot-scoped OTA gate, parked boot player recovery until
  CDP connects, and a neutral `connecting` narration state so a reboot no
  longer flashes a false "Couldn't connect".
- **LAN hub.** `GET /api/v2/status`, mDNS TXT `api=2` as the discovery-side
  half of the same firmware gate, request bodies bounded at the middleware
  chokepoint, and unmatched paths routed through it.
- **Setup portal.** FF1-style redesign, brand/copy pass, PP Mori served from
  the binary, one shared `/setup.css`, manual entry collapsed behind "Other
  network…", and `no-store` on HTML pages.
- **Resilience.** D-Bus startup is non-fatal with background retry, a corrupt
  state file is survived instead of crash-looping, and the relayer is
  reconciled off the sys-monitord heartbeat.
- `feral-watchdog` and `feral-sys-monitord`: setupd dropped from the monitored
  service set; connectivity generation-swap races fixed.
- CI: Go component tests run under the race detector, `test-scripts.yaml`
  added for the shell contracts, setupd lint/test workflows removed.

### Release action

Dispatch `build-image-to-cf.yml` in the `ffos` repo with `version=2.0.0` and
`ffos_user_ref` pointing at the `v2.0.0` tag (the staging merge of this
release), on an `ffos` ref that includes feral-file/ffos#118 (build/image
manifests pruned of setupd/launcher-ui; captive-DNS + nftables portal
plumbing). The two ship together or not at all — an image whose manifests
still reference `feral-setupd`/`launcher-ui` will not build against this ref.

### Why package-only is NOT permitted

Old units would still try to start the deleted `feral-setupd.service` and
would still point the kiosk at the deleted launcher-ui bundle. A fielded
device that lost Wi-Fi would have NO provisioning surface at all: BLE is gone
with setupd, and the SoftAP/captive-portal path that replaces it only comes
up under the new units and scripts. The old `serve-feral-player.sh` would
also skip the `setupDisplay` contract gate, so setup credentials could fail
to reach the screen with nothing failing loudly.

### Verification

`make verify` (`verify-go` + `verify-scripts`). The cross-rail startup
invariants are pinned by `scripts/test-headless-startup-contract.sh` and
`scripts/test-serve-feral-player.sh`; the DRM display predicate remains in
lockstep across `start-kiosk.sh`, `feral-watchdog/display.go`, and
`feral-controld/drm/drm.go`.

## 1.1.0 — declared, never cut (superseded by 2.0.0)

Declared for the setupd merge (PR #232) and left here as the audit trail for
why the version was skipped: no `v1.1.0` tag was ever created, no image was
dispatched, and the staging bundle outgrew the minor bump before release. Its
full-image content is carried by the 2.0.0 entry above; nothing shipped under
this version.

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
