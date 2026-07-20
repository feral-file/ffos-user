# Review Findings & Fix Record — #3469 SoftAP Setup + setupd→controld Merge

**Original review:** 2026-07-21 — four independent Fable-5 agents (Go correctness, cross-repo security, client apps, image/scripts/docs).
**Fix pass + round-2 independent review:** 2026-07-21 — fixes applied across all four repos, then a second, fresh-context Fable-5 review round over the post-fix diffs.

**Scope (branches, all still uncommitted working trees at time of writing):**
- ffos-user `feat/softap-setup-merge` (feral-controld Go daemon)
- ff-player `feat/setup-display` (React kiosk)
- ff-app `feat/ff1-lan-transport` (Flutter app)
- ffos `feat/softap-image` (Arch image)

**Gate status after fixes:**
- feral-controld: `go vet ./...` + `go test ./...` clean; `golangci-lint --new-from-rev=e609577` → 0 issues.
- ff-player: `tsc --noEmit` clean; `vitest run` 193/193 (was 181; +12 new tests).
- ff-app: `flutter analyze` clean on all touched files; every touched test suite passes (LAN transport, claim, fallback, onboarding). One unrelated pre-existing failure (`ensure_tracked_addresses_alias_test.dart`) confirmed to fail on the clean tree with our changes stashed — not introduced here.
- Image/scripts: `make verify-scripts` passes (both contract scripts OK).

Legend: **FIXED** = change applied + regression test. **FIXED (no test)** = change applied, not unit-testable. **DOCUMENTED** = confirmed real but intentionally not auto-fixed (rationale below). **DECISION** = needs a human/product call. **DELIBERATE** = confirmed intended, no action.

---

## Round-1 findings — status after fixes

### feral-controld (Go)

| ID | Sev | Status | Fix |
|----|-----|--------|-----|
| G1 | Major | **FIXED** | `provisioning/provisioning.go` `ensureAPUp`: on portal-bind failure the radio hotspot is now torn back down in the same call (`ap.Down`), so a portal-less AP can no longer broadcast indefinitely. The retry tick then re-runs scan→AP→portal cleanly (constraint-1 order preserved). Regression: `TestPortalBindFailureTearsAPBackDown`. |
| G2 | Major | **FIXED** | `otagate/runner.go` `tail`: added a systemd unit-liveness probe (`systemctl show -p ActiveState`) + `unitDeadGrace`. An updater killed without an id-tagged terminal line (SIGKILL/OOM/`systemctl stop`) now ends the tail with a transient-classified error instead of EOF-polling forever and wedging the single-flight (which blocked claiming). Regressions: `TestTailDetectsSilentUnitDeath`, `TestTailKeepsPollingWhileUnitAlive`. |
| G3 | Major | **FIXED** | `otagate/runner.go` `Run`: a `defer` restarts `feral-watchdog` on exit (see also F5, which extended this to the success path). Regression: `TestRunRestartsWatchdogOnFailure`. |
| G4 | Minor | **FIXED** | `otagate/otagate.go` `runLocal`: a canceled/deadline ctx (the shared single-flight caller going away) no longer latches a bogus permanent failure — guarded by `ctx.Err() != nil` before `latch`. The ctx-capture caveat is now documented on `do`. Regression: `TestCanceledContextDoesNotLatch`. |
| G5 | Minor | **FIXED** | `setupui/setupui.go`: the narration queue now delivers **distinct** states in order (so the claim flow's `ShowReady()`→`Hide()` both reach the player) while still coalescing same-state bursts (OTA progress) in place. Was a single-slot newest-wins queue that dropped `Ready`. Regressions: `TestReadyThenHideDeliversBoth`, `TestSameStateBurstCoalesces`. |
| G6 | Minor | **FIXED** | `provisioning_wiring.go` `setupNotifier`: tracks a `narrating` flag and only `Hide()`s an overlay it painted. A connectivity flap (offline→online) can no longer erase the executor's claim QR or poison Resync. Regression: `TestSetupNotifierHidesOnlyOwnNarration`. |
| G7 | Minor | **FIXED** | `devicectl/executor.go` `uploadLogsInProcess`: single-flighted via `logUploadInFlight` (atomic), bounded by a 10-minute `logUploadTimeout` context, and now uses `wrapper.NewHTTPClientWithoutTimeout()` so the 30s whole-request timeout no longer kills large archives on slow uplinks (the case support most needs). |

### Security (cross-repo)

| ID | Sev | Status | Fix |
|----|-----|--------|-----|
| S1 | Medium (poss. High) | **FIXED** | `hub/status.go` withholds `topic_id` from `/api/status` once `Claimed == true` (the claim handover is its only purpose). Regression: `TestHandleStatus_ClaimedDeviceWithholdsTopicID`. Device-side half of the S1/C2 pair. See **H1** for the residual claimed-device authz question. |
| S2 | Low | **DOCUMENTED** | ff-app trusts unvalidated mDNS host:port. Within the accepted trusted-LAN model (an on-LAN attacker can hit `:1111` directly). Optional private-range hardening left as a follow-up. |

### ff-app (Flutter)

| ID | Sev | Status | Fix |
|----|-----|--------|-----|
| C1 | Major | **FIXED** | `ff1_wifi_ble_fallback.dart`: both `runWifiThenBleFallback` and `runFf1RecoveryWithFallback` now return a clean failure when `blDevice.remoteId` is empty (a LAN-claimed device was never BLE-paired), instead of hanging on `BluetoothDevice.fromId('')`. |
| C2 | Major | **FIXED** | `ff1_lan_claim_provider.dart` `claim()`: re-checks the fresh `GET /api/status` `claimed` flag and throws `Ff1AlreadyClaimedException` (a two-user LAN race no longer double-claims). Belt-and-suspenders with S1. |
| C3 | Major | **FIXED** | `ff1_lan_endpoint.dart` `baseUrl`: brackets IPv6 literal hosts per RFC 3986. New `ff1_lan_endpoint_test.dart` covers IPv4/hostname/IPv6. |
| C4 | Minor | **FIXED** | `ff1_lan_claim_provider.dart` `ff1DeviceInfoFromLanStatus`: identity candidate order now prefers `fallbackDeviceId` (mDNS TXT `id`, the routing key) over the human `name`. Test updated. |
| C5 | Minor | **FIXED** | Discovery replay gap closed via a new `replayCurrentThenStream` helper (synchronous `onListen` snapshot + stream), replacing the `yield current; yield* stream` gap. New `replay_current_then_stream_test.dart`. |
| C6 | Minor | **FIXED** | `onboarding_add_address_page.dart`: `completeOnboarding()` now runs immediately after the bind (resolving both `ref` reads before the await), so unmounting mid-bind no longer leaves a bound device with onboarding never completed. Regression added. |
| C7 | Minor | **FIXED** | `ff1_lan_rest_client.dart` `sendCommand`: a throwing `resolveDeviceId` is caught and treated as "no LAN endpoint" → relayer fallback, instead of aborting the command. Regression added. |

### ff-player (React)

| ID | Sev | Status | Fix |
|----|-----|--------|-----|
| P1 | Minor | **FIXED** | `SetupOverlay.tsx` `softApQrValue`: emits `WIFI:T:nopass;S:…;;` (no `P:`) when the password is empty, and backslash-escapes `\ ; , : "` in SSID/password. Tests assert the raw QR payload. |
| P2 | Minor | **FIXED** | `CDPRequestHandler.ts` validator uses `Number.isFinite` (rejects NaN/Infinity); `UpdatingPanel` guards + clamps to [0,100] (see round-2 client note). Tests for NaN/Infinity/-Infinity + clamp. |

### Image / scripts / docs

| ID | Sev | Status | Fix |
|----|-----|--------|-----|
| I1 | High | **FIXED** | New `.github/workflows/test-scripts.yaml` runs `make verify-scripts` on the old path triggers (scripts, `.start-services.sh`, `systemd-services/**`, Makefile, self), restoring CI for the two contract scripts. `AGENTS.md:31` corrected to name the workflow. Verified: `make verify-scripts` passes locally. |
| I2 | Medium | **FIXED** | `docs/api-design.md`: rewritten to the real two-layer captive-detection design (image-shipped DNS catch-all so the probe's DNS resolves + reaches the portal at all; portal HTTP 302 so it looks like a captive portal). |
| I3 | Low | **DELIBERATE** | `ffos` `ff_player_ref` default pinned to `feat/setup-display` (round-2 M1 re-flagged this). Intended release-time state with a revert note; **release-operator merge-blocker**, not a code defect. Flip to `main`/tag before it reaches the release rail. |
| I4 | Low | **FIXED** | `ffos/docs/DEVICE_LIFECYCLE.md` mermaid now shows the wired-link suppression + 5-minute sustained-offline window instead of unconditional AP-on-offline. Note: the original review cited a `setup-flow.md` as the "correct" reference — that file does not exist in ffos; the corrected semantics were sourced directly from `provisioning/provisioning.go` (ground truth). |
| I5 | Low | **FIXED** | `scripts/test-headless-startup-contract.sh` now asserts `feral-controld.service` contains `StartLimitIntervalSec=0`, pinning the "never latch start-limit-hit" invariant. Runs green. |

---

## Round-2 findings (fresh independent Fable-5 review of the post-fix diffs)

The round-2 reviewers first re-verified the round-1 fixes in-tree (G1/G2/G3/G4/G5/G6/S1 all confirmed applied and sound), then hunted the changed code fresh. New results:

### Fixed this round

| ID | Sev | Area | Status | Detail |
|----|-----|------|--------|--------|
| R-C1 | Major | ff-app | **FIXED** | **http.Client socket-pool leak** on every LAN-routed online command. The router's `_http` was null, so each `_sendViaLan` built an `FF1LanClient` that created **and owned** a fresh `http.Client` never disposed (`_ownsClient = httpClient == null \|\| ownsClient`). Fix: the router now owns one shared `http.Client` and disposes it via the provider's `ref.onDispose`. `ff1_lan_rest_client.dart` + `ff1_wifi_providers.dart`. Regression: `dispose closes only a router-owned http.Client`. |
| R-G1 | Major | controld | **FIXED** | **mDNS advertiser never starts on link-up-without-internet.** `sys-monitord`'s `connectivity_change` fires only on INTERNET transitions, so a LAN link coming up with a dead WAN (exactly the recovery hub's reason to exist) left the advertiser down — the device stayed undiscoverable. The code even acknowledged this as deferred. Fix: `mediator.go` now reconciles the advertiser against link state on the periodic `SYSMETRICS` signal (which arrives regardless of internet), self-healing within one metrics interval; advertiser started-state is tracked so it never double-registers or churns. Regression: `TestMediator_SysMetricsSelfHealsMDNS` (start-on-link-up + no-churn-when-healthy). |
| R-P2 | Minor | ff-player | **FIXED** | `UpdatingPanel` progress now clamps to [0,100], so an out-of-range value (e.g. 150) renders "100%" not "150%". Tests added. |
| R-F5 | Minor | controld | **FIXED** | **Watchdog left stopped on the OTA success path.** The G3 defer only restarted `feral-watchdog` on failure; on success it relied on "update ⇒ imminent reboot." If that reboot is deferred/staged/fails, the watchdog stays dead on the still-running old build (unattended for months). Fix: `runner.go` restarts the watchdog on **all** exit paths — the brief pre-reboot window is harmless, and it closes the deferred-reboot gap. Regression: `TestRunRestartsWatchdogOnSuccess` (replaces the old "stays stopped" test). |

### Confirmed real, intentionally NOT auto-fixed (documented)

- **H1 — HIGH — ACCEPTED DESIGN (confirmed by the maintainer 2026-07-21).** A *claimed* device serves the full destructive command set (`factoryReset`/`reboot`/`shutdown`/`sshAccess`/`updateToLatest`/input-injection) on the open, unauthenticated `:1111` LAN hub. Verified chain: the hub listener stays bound unconditionally; a claimed device still advertises over mDNS (discoverable); `handleCast` → `cmdHandler.Process` with the hub middleware doing **only** rate-limiting (the authorization check is an explicit stub tracked under issue #3471); the command router makes no source distinction. The maintainer has confirmed this is **intended**: the LAN hub is trusted for claimed devices too, consistent with the trusted-LAN model already accepted for the unclaimed case. No code change; recorded here so the residual is explicit and #3471 can layer authorization on top if that trust boundary ever tightens. No further action.
- **F2 — Major — pre-claim OTA gate silently withholds the claim QR on a version-check failure** (`executor.go` `showPairingQRCodeInProcess`). On `ResultVersionCheckFailed` it logs, paints nothing, and returns `CmdOK` — so a briefly-unreachable distributor at claim time strands onboarding with no on-screen feedback and no way for the app to tell the QR was withheld. **Not auto-fixed** because a clean fix needs a dedicated ff-player "setup error" narration state (a cross-repo contract bump); reusing `join_failed` would show actively misleading "reconnect to the setup hotspot" Wi-Fi instructions, and changing the return to non-OK is an app-facing wire change. **Recommended:** add a `setup_error` state (following the existing `factory_reset` extension-state pattern) + render it in ff-player, then narrate it here.
- **F3 — Minor — OTA single-flight key ignores `Mode`** (`otagate.go`). A `ModeAvailable` user update that joins an in-flight `ModeRequired` pre-claim gate can receive its `ResultNoUpdateNeeded` and skip an available update. **Not fixed** because the reviewer's suggested fix (separate keys per mode) would break the core single-updater invariant — two concurrent `feral-updater-run` units fighting — which is worse. The wrong outcome is rare (a user update tap concurrent with claim) and recoverable by retry. Correct future fix, if wanted: re-evaluate after the shared flight completes rather than split the key.
- **F4 — Minor — two narration surfaces can collide during a sustained-offline window mid pre-claim update.** The "surfaces never overlap" invariant holds at claim *start*, but the pre-claim update runs for minutes; if Wi-Fi drops >5 min mid-update, provisioning transitions to `StateAPActive` and pushes `softap_qr` over the `updating` overlay on the shared `setupui.Service`. **Not fixed** — an edge case that needs a surface-priority/ownership model to resolve cleanly; documented for follow-up.
- **Onboarding already-claimed fallback (Minor)** — `onboarding_add_address_page.dart` catches `Ff1AlreadyClaimedException` in its generic `on Object` and falls through to the BLE/QR path instead of surfacing "already claimed." Falling back is the file's documented intent, so this is a UX nicety (a clearer message), not a correctness defect. Left as-is.

### Confirmed sound / no action

- **Infra M1** = I3 above (deliberate branch pin, release-operator item). **L1** (controld `After=` vs `--no-block` ordering) and **L2** (`web-controller-feasibility.md` fragment-privacy framing on a non-shipping doc) are pre-existing informational notes, not regressions.
- **Round-2 reviewers independently verified sound:** the provisioning state machine (no stranding path; Stop() teardown cannot be skipped by a canceled ctx; idempotent reconcile; sustained-offline tick raises the AP even under persistent errors), the wired-link guard (fails closed, re-arms a fresh window), supervisor panic-restart cleanup, OTA tail termination + ctx-guarded latch, the setupui queue across interleavings, semver/classify, the hub middleware chokepoint + WS reqSlot release, wifictl/softap (padPSK cannot panic), `EnableHub` default-on, factory-reset topic rotation; on the client side IPv6 bracketing, gap-free discovery replay, the claimed re-check, the empty-`remoteId` BLE guards, LAN `reachedButFailed` not falling through to BLE, WIFI: QR nopass/escaping, CDP unknown-state forward-compat; and across image/scripts/docs the new CI workflow, systemd unit semantics, dnsmasq/sysctl/preset/polkit config, and every doc wire-contract claim checked against source (setup states, SSID/PSK, portal routes, hub endpoints, mDNS TXT, claim-URL payload).
- **Security hunt cleared (both rounds):** no command injection (nmcli/systemctl via argv `wrapper.Exec`, no shell), no XSS (portal `html/template` + React-escaped player), no CDP-expression injection (JSON-marshaled payload), no log-zip path traversal (fixed in-image dirs), no secrets in logs, factory-reset topic-rotation TOCTOU sound.

### Pre-existing, out of scope (noted, not part of this merge)

- Executor mouse-cursor state (`cursorPositionX/Y`, `screenInitialized`, `screenWidth`) is read/written without locking while commands dispatch on concurrent goroutines — a data race on concurrent mouse/zoom commands. Predates the merge; neither introduced nor fixed here.

---

## Decision log

**H1 — RESOLVED (2026-07-21): accepted as designed.** The maintainer confirmed that a **claimed** device accepting destructive commands (factory reset, reboot, SSH toggle, input injection) from any peer on its LAN is intended — the LAN is trusted for claimed devices too, consistent with the already-accepted trusted-LAN model for unclaimed devices. #3471 may layer authorization on top later, but nothing is required for this merge. No open decisions remain; every finding is fixed, deliberate, accepted, or documented with a recommended follow-up.
