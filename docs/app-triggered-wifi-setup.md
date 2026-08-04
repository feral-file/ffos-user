# App-triggered Wi-Fi setup (`startWifiSetup`)

Execution plan, written to the `PLANS.md` contract. Spans two repositories: `ffos-user`
(`feral-controld`) and `ff-app`. No `users/**` changes, so the device half ships on the **package
rail**.

**Goal.** Let a user re-configure a frame's Wi-Fi from the mobile app. The app's only job is to put the
frame into its existing setup mode and then forget the device; everything after that is the unchanged
out-of-box flow.

---

## 1. Current-state summary

**There is no way to change a v2 frame's Wi-Fi.** The app's only provisioning transport was BLE
(`scan_wifi` / `connect_wifi` / `keep_wifi`), and v2 firmware has no BLE stack. The LAN/relayer channel
reaches the frame fine but has no Wi-Fi vocabulary.

The app already ships a **"Configure Wi-Fi"** entry (`ff-app
lib/widgets/device_configuration/options_button.dart:163-167`). It is **ungated** — it drives the BLE
flow for every device, and on a LAN-claimed v2 frame `FF1Device.fromLanDeviceInfo` leaves `remoteId`
empty, so `connectAndScanNetworks` falls through to `scanForName` and fails with
`FF1BluetoothError('Could not find <name> via Bluetooth scan')`. That is a live bug this plan fixes as
a side effect.

Device-side, the setup AP rises only on the provisioning machine's own triggers: unprovisioned at boot,
the boot relocation check, a 5-minute sustained link loss, or a join-failure re-raise. Nothing external
can ask for it.

Today's only recovery for "moved the frame to a new network" is physical: power off the old router and
wait out the sustained-offline window (`docs/setup-flow.md`, "Offline recovery").

**Firmware split.** `startWifiSetup` ships **with the initial v2 release**, so "is v2" is equivalent to
"supports this command". The app's existing `Ff1LanPairableGate` (mDNS TXT `api=2` **and**
`GET /api/v2/status` returning `contract: "2"`) is therefore already the capability gate — no new
capability field, and no version-string comparison (branch orderings differ across release/demo/qemu).

---

## 2. Constraints and invariants

Non-negotiable, all verified against source:

1. **The reply must precede any radio work.** Raising the AP drops the station link that carries the
   response.
2. **Every AP raise pairs `clearOffline()` + `resetJoinStatus()` + `transition()`.** All four existing
   raise sites do this (`provisioning.go:767`, `:1186`, `:1228`, `:1290`). Omitting `clearOffline`
   leaves a stale `offlineSince`, so an abandoned session triggers a `sustained-offline` raise within
   one 15s tick; omitting `resetJoinStatus` (which is edge-gated on `state != StateAPActive`, so it must
   run *before* the transition) shows the user the success banner of the join performed at install time.
3. **`ensureAPUp` already runs the pre-AP scan** in a 3-attempt retry loop with the station still up
   (`provisioning.go:1537-1550`). Do not add a second scan.
4. **A live local link suppresses the AP** (#233). A raised `ap_active` is torn down by any `online`
   reading — `reconcile` calls `ensureAPDown` for `StateOnline` (`provisioning.go:1496`) — which is
   routine on a wired frame, where `sys-monitord` re-emits its first probe unconditionally on restart.
5. **`LinkChecker.ExternalLink` is not an ethernet probe.** `linkProbe` accepts `ethernet` **and**
   `wifi` (`status/linkcheck.go:151`), and the repo's own test `"station association counts"`
   (`status/linkcheck_test.go:55-59`) asserts `want: true` for an associated station. Using it as a
   wired guard would reject every target device.
6. **The captive portal binds all interfaces** (`net.Listen("tcp", ":80")`, `portal/portal.go:208`) and
   has no Origin or source check on `POST /connect`.
7. **All machine transitions run on the single loop goroutine** (`provisioning.go:530-531`).
8. **LAN endpoint lookup in the app is cache-only** (`ff1_lan_rest_client.dart:328-337`) and the mDNS
   browse is windowed — live only during a 60s post-foreground warm-up or while something holds it
   (`ff1_lan_browse_controller.dart`). The options sheet is normally reached well after that window.
9. **Wire surfaces change additively** (`docs/api-design.md`, "Current-v1 backward-compatibility
   posture", item 4).

---

## 3. Risks and unknowns

| Risk | Assessment |
|---|---|
| **Ethernet frame** | Raising the AP there would be torn down immediately by the next `online` reading (constraint 4), so the flow silently fails. Worse, supporting it would need the online-exit suppressed, which breaks #233 *and* exposes an unauthenticated, CSRF-able credential form on a routable wired address (constraint 6). **Mitigation: reject.** |
| **Wrong link probe** | Constraint 5 — using `ExternalLink` ships a 0%-functional feature with a false error message ("unplug the ethernet cable") shown to users who have no cable. **Mitigation: new `WiredLink` seam.** |
| **User taps and abandons** | The frame would sit in AP mode indefinitely; only going online or a portal join lowers a raised AP. **Mitigation: one 30-minute timeout.** |
| **Stale LAN cache misroutes a v2 frame to BLE** | Constraint 8 — this reproduces the very bug being fixed, one branch over. **Mitigation: `hold()` + bounded `findByDeviceId` before consulting the gate.** |
| **Frame is asleep** | Panel is dark, so the on-screen QR is unreadable. Accepted (see §6, open items); the user can wake it with the remote. |
| **Frame reachable only via relayer** | `getDeviceStatus` has no `contract` field, so v2 cannot be identified off-LAN. **Mitigation: add it (§4.2) — free now, expensive after v2 ships.** |
| **Claim state survives the Wi-Fi change** | The frame stays claimed, so after re-onboarding it appears in the app's scan list as a claimed device. The app only auto-prompts to pair for *unclaimed* frames, so re-adding is one tap, not zero. Accepted — clearing the claim would discard the relayer topic, which is factory-reset semantics. |
| **Unauthenticated `:1111`** | `startWifiSetup` joins `factoryReset`, `shutdown`, and `sshAccess` on an open surface. It is bounded (30-minute timeout, frame visibly narrates setup on screen) and does not persist. The v2 controller-authentication profile (`docs/ff1-v2-controller-authentication.md`) is the durable answer and is a design draft today. |

---

## 4. Design

### 4.1 Device — new command `startWifiSetup`

Registered in `commands/types.go` (`CMD_START_WIFI_SETUP Type = "startWifiSetup"`), handled in
`devicectl`, reachable over the relayer and the LAN hub `POST /api/cast` via the shared
`commandrouter` (`docs/api-design.md` invariant 6).

Request: `{}` (no parameters).

Reply — sent **before** the machine is asked to raise anything (constraint 1):

```json
{ "ok": true, "ssid": "FF1-<device_id>" }
```

Rejections `{ "ok": false, "code": "...", "message": "..." }`:

| Code | When |
|---|---|
| `wired_link_active` | a live ethernet link, or the probe errored (fail closed) |
| `busy` | the provisioning machine is in `joining` or `starting` |

**New seam — `status.LinkChecker.WiredLink(ctx) (bool, error)`**, beside `ExternalLink`
(`status/linkcheck.go:88-93`), flushing only `typ == "ethernet"`. `linkProbe` already parses
`GENERAL.TYPE` per device block, so this is a small addition with the same surface-errors contract.
Constraint 5 is why it cannot reuse `ExternalLink`. Survey semantics (pinned in
`docs/network-recovery-ux.md` constraint 6, back-ported here): the survey is valid when the nmcli
output contains at least one ethernet **or Wi-Fi** device row (the existing `surveyed` rule —
corrupt or empty output proves nothing and surfaces as an error); given a valid survey, the wire
verdict is computed from ethernet rows only, and a valid survey with no ethernet row is
confirmed-no-wire (`false, nil`).

**Entry sequence**, on the loop goroutine, mirroring every existing raise site (constraint 2):

```go
m.clearOffline()
m.resetJoinStatus()
m.transition(ctx, StateAPActive, provisioning.Detail{Reason: "user-requested"})
```

`ensureAPUp` then does the rest — pre-AP scan (constraint 3), hotspot, portal, and the `scanning` →
`softap_qr` narration. `Reason: "user-requested"` carries no SSID and no PSK, so `setupNotifier`'s
`StateAPActive` switch (`provisioning_wiring.go:277-290`) falls to its `default` and renders nothing
until the credentials-bearing announcement arrives — which is the existing, correct behavior.

**Safety timeout.** The bespoke 30-minute timeout this plan originally specified is **subsumed** by the
`network-recovery-ux.md` §4.2 session mechanism (amendment 1 of that plan's §4.5): the
`user-requested` policy row carries the same 30-minute bound, plus the portal-activity deferral and
the 2-hour absolute cap this plan did not have. On expiry the machine tears the AP down and resumes
normal state handling. **No explicit profile reactivation**: the saved profile is never touched, so
NetworkManager autoconnect restores the previous network on its own (bench-verified: 6s
reassociation). This exists only so a user who taps and changes their mind cannot strand the frame.

### 4.2 Device — additive: `contract` on relayer `getDeviceStatus`

Add `contract` to the relayer `getDeviceStatus` reply so the app can identify a v2 frame when mDNS is
unavailable (multicast-filtering APs, cross-VLAN). Additive per constraint 9. **Do this in the v2
release itself** — adding it later costs a full OTA convergence cycle before the app can rely on it.

### 4.3 App — capability gate and flow

Gate the existing "Configure Wi-Fi" entry:

| Verdict | Route |
|---|---|
| mDNS TXT `api=2` **and** `/api/v2/status` → `contract: "2"` (or relayer `contract: "2"` per §4.2) | new AP-trigger flow |
| anything else — no `api` key, `/api/v2/status` 404s, probe failed or timed out | **existing BLE flow, unchanged** |
| BLE finds nothing either | honest message: connect your phone to the same Wi-Fi as the frame and retry |

Two properties this gate must have:

- **Resolve a live endpoint first** — `browseController.hold()` plus a bounded `findByDeviceId` before
  consulting `Ff1LanPairableGate` (constraint 8). Existing pattern: `ff1_lan_recovery.dart:77-84`.
- **Fail closed to BLE.** Only a positive v2 confirmation takes the new path. BLE is the status-quo
  behavior, and routing an old frame into the new flow lands it on a command it does not implement.
  Note that a missing TXT set is *not* evidence of old firmware — Android's `NsdManager` delivers empty
  TXT records, which `Ff1LanPairableGate` already handles by falling back to the HTTP probe.

Flow, three steps:

1. **Warning dialog.** States that the frame will leave the current network and show a setup screen;
   that the user must be **physically at the frame** to scan the code on its screen; and that the frame
   will be removed from the app and reappear automatically once setup completes.
2. **Send `startWifiSetup`.** New `FF1WifiCommandRequest` subclass plus an `FF1WifiControl` wrapper.
   A **timeout is not a failure** — raising the AP severs the link that would carry the reply, so an
   ambiguous send is treated as success.
3. **`removeDevice(deviceId)`** (`ff1_bluetooth_device_providers.dart:118`).

Nothing else: no verification loop, no polling, no countdown, no persisted in-flight state. The
`factoryResetAndRemoveDevice` path (`ff1_bluetooth_device_providers.dart:130+`) is the template — it is
already "send a command, then remove the device", including LAN/relayer fallback.

### 4.4 What happens afterwards — all existing behavior

1. The frame shows the existing `softap_qr` screen (`WIFI:` QR plus SSID and passphrase).
2. The user's camera joins the hotspot from that QR.
3. The captive portal opens, or the user types `http://ff1.config`.
4. The frame joins the chosen network; the AP comes down (existing `applyJoin`).
5. Online → existing OTA gate → claim QR.
6. The app rediscovers the frame over mDNS `_ff1._tcp`.

### 4.5 Rejected alternatives

- **Complete the whole flow inside the app** (app joins the hotspot programmatically and drives the
  portal). Needs iOS `NEHotspotConfiguration` — an Apple entitlement with external lead time — plus
  Android Wi-Fi permissions the app does not hold, and it duplicates a portal UI that already works.
  Deferred; it can be layered on later without changing the device contract.
- **Support the ethernet case.** Requires suppressing the online-exit from `ap_active`, breaking #233
  and exposing the portal on a routable interface (§3).
- **A new capability field or a version comparison.** Unnecessary — `startWifiSetup` ships with v2, so
  the existing v2 gate is the capability gate.

---

## 5. Test and verification plan

**Device (`components/feral-controld`, table-driven with the existing fakes):**

- `WiredLink`: an ACTIVATED `ethernet` row returns true; **a Wi-Fi-only station returns false** (this is
  the assertion that catches constraint 5 — a naive test written against `ExternalLink` would pass
  while the feature was broken); a probe error surfaces rather than failing to false; a valid survey
  with **no ethernet row at all** is confirmed-no-wire (`false, nil`), not an error; **corrupt or
  empty output** (the unsurveyed case) surfaces as an error, never as a confirmed verdict.
- Admission: rejects on wired link; rejects on probe error; rejects `busy` from `joining`/`starting`;
  accepts from `online`, `offline_retrying`, `unprovisioned` — and from `ap_active` as an idempotent
  refresh (implementation amendment: a cast can arrive over the hotspot's own subnet or during a
  failed raise, and rejecting there would wedge the app's retry; the accept re-latches the
  `user-requested` session policy and re-arms its fresh 30-minute clock).
- Ordering: the reply is produced before the AP raise is requested.
- Entry: `clearOffline` and `resetJoinStatus` both observed, `resetJoinStatus` before the transition;
  no second pre-AP scan issued.
- Timeout: no join within 30 minutes tears the AP down; a completed join cancels it.
- Regression: an abandoned session does not produce an immediate `sustained-offline` re-raise (this is
  the failure mode omitting `clearOffline` causes).
- `contract` present in the relayer `getDeviceStatus` reply.

**App (`ff-app`):**

- Gate routing: v2-over-LAN → new flow; v2-over-relayer (`contract`) → new flow; no `api` key → BLE;
  `/api/v2/status` 404 → BLE; probe timeout → BLE; empty TXT → HTTP probe, not an immediate BLE verdict.
- The gate resolves a live endpoint before deciding (fails without `hold()` + `findByDeviceId`).
- A send timeout is treated as success and still removes the device.
- `removeDevice` is called exactly once on success and never on a rejection verdict.
- Regression: the legacy BLE path is untouched for old-firmware devices.

**Gates:** `gofmt -s -w`, then `go vet ./...`, `go test ./...`, and
`golangci-lint run --new-from-rev=HEAD~1 ./...` inside `components/feral-controld`.

---

## 6. Staged rollout

1. **Device — `WiredLink` seam** plus its tests. Independent and inert on its own.
2. **Device — `startWifiSetup`**: command, admission, entry sequence, 30-minute timeout, and the
   `contract` field on relayer `getDeviceStatus`. Ships in the v2 release. Package rail.
3. **App — capability gate** on the existing "Configure Wi-Fi" entry. Independently valuable: it stops
   v2 frames from being routed into a BLE flow that cannot serve them, even before step 4 lands.
4. **App — the three-step flow**: warning dialog, command, `removeDevice`.

**Documentation to update in the same change:** `docs/api-design.md` (the command and its verdicts,
plus the `contract` addition to `getDeviceStatus`), `docs/setup-flow.md` and its state diagram (a new
`* → ap_active: user-requested` edge), and `ffos/docs/DEVICE_LIFECYCLE.md`.

**Open items, accepted for this iteration:**

- A frame inside its sleep window shows a dark panel, so the on-screen code is unreadable. The smallest
  fix would be to drive the existing `wakeNow` path on admission; not doing it is acceptable because the
  user can wake the panel at the frame.
- Re-adding the frame after setup is one tap rather than zero, because the claim survives the network
  change (§3).
- `startWifiSetup` is unauthenticated on `:1111`, alongside the commands already there. The v2
  controller-authentication profile is the durable answer.
