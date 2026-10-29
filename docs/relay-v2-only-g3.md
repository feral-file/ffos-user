# G3: firmware that keeps working when the legacy relayer is gone

Status: design, revised after review, 2026-10-29. Scope: feral-controld,
ffos image build, ff-app, ff1-cli. ff-relayer needs no change.

## 1. Goal and non-goals

Goal: a firmware generation ("G3") and matching clients that keep every
supported flow working when the legacy relayer host
(`tv-cast-coordination.autonomy-system.workers.dev`, `/api/*`) is shut
down, using only the v2 relay `wss://relayer.feralfile.io`
(`/v2/channels/{deviceId}/connections`). While the legacy host exists, G3 is
dual-stack, so it can ship before retirement. The app keeps controlling every
firmware generation in the field, legacy-only ones included (§5.1).

Non-goals, decided:

- No topic-to-grant migration. A device claimed under legacy (topic, no
  control owner) becomes v2-reachable only when its owner enrolls it on the
  LAN with the screen QR, exactly as a new device (`ff1-v2-migration.md`,
  "Already claimed devices do not preserve a v1 topic credential").
- No locally generated topic. A legacy topic is an HMAC credential minted by
  the relayer (`ff-relayer/src/utils/hmac.ts`); the device cannot mint one,
  and v2 never reads one. G3 removes topic dependencies instead.
- No v2 enrollment of the first owner and no remote `requestAccess`
  (`ff1-control-v1.md` §5.1/§10 unchanged).
- Legacy-state devices (no owner) are not made remotely controllable over v2.
  v2 accepts envelopes only (§7.2). They stay LAN-only until enrolled.

## 2. Fleet facts this design relies on

Production (`https://ffos.feralfile.com/api/latest/release`, 2026-10-29):
`latest_version=2.0.11`, `min_runtime_version=1.0.17`,
`recovery_version=2.0.1`, `min_upgradeable_version=0.15.0`.

| Generation | Versions | Relayer | Notes |
|---|---|---|---|
| G0 | 1.0.17–1.0.21 | legacy only | BLE setup (feral-setupd); the current app has no BLE |
| G1 | 2.0.0–2.0.11 | legacy only | all released firmware; no `control/`, no `relayV2` |
| G2 | 2.0.12–2.0.13 (develop) | dual-stack | claim QR and mint pairing still need the topic |
| G3 | this design | dual-stack, legacy optional | |

- `feral-updater@.timer` (daily, `RandomizedDelaySec=7200`,
  `Persistent=true`) talks to the version API only, so every generation
  keeps updating after retirement.
- The pre-claim OTA gate is `ModeRequired` (updates only below
  `min_runtime_version`). On G0–G2 it runs only AFTER the claim flow got a
  legacy topic (`devicectl/executor.go`: topic wait precedes the gate). So
  after retirement an unclaimed pre-G3 device never reaches its pre-claim
  gate; it waits for the daily updater. The reset image therefore matters as
  much as the running version (§3.6, §6).
- `controld.json` changes only with a full image (the package rail never
  rewrites it). The image writes it from the `FERAL_CONTROLD_CONFIG_JSON`
  secret.
- Every released controld (G0–G2) `os.Exit(1)`s when a live legacy socket
  drops and the reconnect returns a permanent error, and from 2.0.0 the
  mediator redials the legacy relayer on every SYSMETRICS heartbeat (~2 s).
  404/410 handshakes and non-temporary DNS failures are permanent errors.
  G3 cannot change devices that do not run it; this is why retirement waits
  for the fleet (§6).

Measured on FF1-8EVTK3RE (2.0.13, develop) with both endpoints on
relayer.feralfile.io: v2 connects; legacy gets 410 and is redialed ~30 times
a minute; `relayer_connected` stays false; with claim and topic cleared, the
claim flow showed "finalizing", logged "relayer topic not ready; withholding
claim QR", and never painted.

## 3. Device changes (feral-controld)

One PR on `develop`. Each item is small and independently testable.

### 3.1 Claim QR: the wait ends on topic OR v2

`MaybeShowClaimQROnOnline` waits up to 60 s for a topic, then withholds the
QR. That expiry is also where the §4.6 "connected, no internet" narration is
painted, so the wait itself stays.

Change:

1. Inject `v2Wired func() bool` and `v2Connected func() bool` into the
   executor (from `controlWire.v2`).
2. `waitForRelayerTopic` becomes `waitForRemoteRoute`: it returns true when
   the topic is ready OR `v2Connected()`. Same 60 s bound and poll.
3. On expiry, keep today's behavior, with one exception: if `v2Wired()` and
   the internet probe reports online, paint the QR anyway (first-owner
   enrollment is LAN-only, so the claim does not need a remote route). An
   offline device keeps the existing narration.
4. Paint with whatever topic is present (index 1 may be empty: §11).
5. Topic observer (`main.go` `SetTopicObserver`): while the claim QR is on
   screen, refresh its URL in place with `setupUI().RefreshClaimQRURL`
   (already used for invitation refresh, `executor.go:668`), which keeps the
   same invitation. Only when the QR is not showing does it re-run the flow,
   as today. This keeps a phone that already scanned the first QR enrollable.

Without v2 wiring (an old `controld.json`, or a G3 controld that reached an
old image through the package rail) the behavior is exactly today's.

Tests (executor table): {v2 wired yes/no} × {topic before paint / after
paint / never} × {v2 connects / blocked} × {online / offline}. Must hold:
v2 connected paints without a topic; topic after paint refreshes in place and
keeps the invitation; v2 blocked + legacy up paints with the topic; offline
narrates; v2 not wired is unchanged.

### 3.2 Mint pairing: no topic needed on the display-grant path

`mintpairing/service.go` refuses start and join with `topic_not_ready` on an
empty topic (`:866`, `:1099`), and the approval-decision parser requires a
non-empty `topicID` (`:1545`).

Change:

1. Start and join refuse `topic_not_ready` only when display grants are not
   wired (`s.displayGrants == nil`). With display grants, an empty topic is
   allowed.
2. The decision parser accepts an empty `topicID`; the equality check against
   the pending guard (`:1514`) still applies (empty equals empty).
3. A Play 0.4 request (no display keys) on a device with no topic gets
   today's `topic_not_ready` rejection at the point it is classified as a
   session request. It never calls `/api/ephemeral-sessions` with an empty
   topic.

The guard itself is unchanged. Its claim-change fence on this path is
`IssueDisplayGrant`'s approver check (`control/display.go:110-116`): the
approver must be an active owner, or the device must be `legacy`.
`TopicGeneration` still moves when a topic lands.

Tests: start/join/decision with an empty topic and display grants wired
succeed; without display grants they return `topic_not_ready`; a 0.4 request
with no topic is rejected without a relayer call.

### 3.3 The legacy relayer stops cleanly

`relayer/relayer.go`, `mediator/mediator.go`. Legacy instance only
(`deviceID == ""`).

1. Any `PermanentError` from the legacy dial (404/410 handshake, NXDOMAIN,
   bad URL) latches `retired`. So does an empty endpoint, at construction.
2. While latched, `Connect`/`RetryableConnect` return `ErrLegacyRetired`
   without dialing, except an escalating probe (first after 1 min, doubling
   to a 1 h cap, injected clock; it keeps a transient permanent-looking
   answer such as a Cloudflare 403/404 mid-deploy to minutes for a
   legacy-only device), and one dial on every offline→online edge in the
   connectivity-change path (`mediator.go` ~782). A successful dial clears
   the latch. The edge reset matters because a captive portal or filtering
   resolver also returns a non-temporary DNS failure; without it, a
   legacy-claimed device would lose remote control for up to an hour after
   such a network, where today it heals on the next heartbeat.
3. The read-loop reconnect path never calls `os.Exit` for the legacy
   instance. A permanent error latches and returns; v2 and the LAN hub keep
   running. Every permanent class survives a restart, so exiting fixed
   nothing.
4. Mediator reconcile and the connectivity-change path log
   `ErrLegacyRetired` at Debug.

Tests: 410 and NXDOMAIN latch and stop redial; an online edge after an
NXDOMAIN latch dials exactly once (and the mediator arms it before it
reconnects); the probe escalates 1, 2, 4 min … to 1 h; a successful probe heals; empty
endpoint never dials; a live-socket drop followed by a permanent error does
not call `os.Exit` (fake OS).

### 3.4 Config without a `relayer` block

`config.Load` leaves `RelayerConfig` nil and `main.go:206` dereferences it.
Default it to `&RelayerConfig{}` after unmarshal. With 3.3 an empty endpoint
means "no legacy relayer"; status `transports` already drops `"relayer"` for
it (`control_wiring.go`). Image configs keep a `relayer` block until no
controld without this fix can run on them.

### 3.5 Probe host and status follow v2

- `netlogBackendHost`: prefer the `relayV2.endpoint` host, then
  `relayer.endpoint`, then the support-logs host.
- `/api/v2/status`: add `relay_v2_connected` (bool), additive.
  `relayer_connected` keeps meaning the legacy socket.

### 3.6 Telemetry for the retirement gates

Add to the `netmetrics` registry (hub `/metrics`, shipped by vmagent):

| Metric | Meaning |
|---|---|
| `relay_v2_connected` | 1 while the v2 socket is up |
| `relayer_legacy_retired` | 1 while 3.3's latch is set |
| `control_state{state}` | 1 for the current `legacy/enrolled/enforced/locked` |
| `legacy_topic_present` | 1 when a legacy topic is stored |
| `device_claimed` | 1 when claimed |
| `reset_target_version_info{version}` | the image a factory reset would boot |

`reset_target_version_info` reads a file written by ffos (§4.4). When the
file is missing (before the script's first run, or an image without the
§4.4 change) it reports `version="unknown"`, and Gate B counts unknown as
below G3.

### 3.7 Unchanged

- Dual-stack stays: G3 dials legacy when configured, because legacy-claimed
  devices need it until their owners re-enroll.
- The LAN phase-0 topic gate (`hub/lanauth.go`). Envelopes are served before
  it (`hub.go` `serveEnvelope`).
- Factory reset's session sweep: with 3.3 it fails fast; lower its 410 log
  from Error to Info.
- v2 eligibility (endpoint, authority on disk, stable `FF1-` ID).

## 4. Image and release (ffos)

1. Production, Staging and Development `FERAL_CONTROLD_CONFIG_JSON` contain
   `"relayV2": {"endpoint": "wss://relayer.feralfile.io"}` and
   `"control": {"enabled": true}`. Verify the secret before the G3 build.
2. Build guard in the `controld.json` step of `build-image-to-cf.yml`,
   `pure-build-image-to-cf.yml`, `build-image-from-tags.yml`: when the
   ffos-user ref has `components/feral-controld/relaywire/`, fail unless
   `.relayV2.endpoint` is non-empty (whatever `control.enabled` says:
   authority can come from state on disk). Pinned in `scripts/verify.sh`.
   It applies to every environment, and develop already has `relaywire/`, so
   all three secrets must carry the endpoint before this merges.
3. G3 is a full-image release, declared in ffos-user `RELEASES.md`.
4. `feral-recovery-update.sh` writes the reset target (the
   `@recovery_candidate` version if present, else `@factory_reset`) to
   `/home/feralfile/.state/reset_target_version` after each run.
5. After G3 is on a channel (human operator, recorded in `RELEASES.md`):
   - `update_min_version=true`: `min_runtime_version = G3`. Never
     `update_required_version` (it stops auto-update and demands a reflash).
   - `update_recovery_version=true`: `recovery_version = G3`.
   - Reflash shelf stock with G3.

## 5. Client changes

### 5.1 ff-app: one app for every firmware

The app keeps both stacks for as long as the legacy host exists. Route
choice stays deterministic from the stored device record (last status +
grant), exactly as today; G3 only removes the places where a topic is
treated as identity.

**Firmware the app must drive, and how:**

| Device as the app sees it | Firmware | Commands | Notifications / approvals | Claim / bind |
|---|---|---|---|---|
| No `control` block (`absent`) | G0, G1, or protocol off | plaintext: LAN, then legacy `/api/cast?topicID` | legacy `/api/notification` by topic; LAN WS | today's flow: QR topic, LAN bind needs the topic |
| `control.state=legacy` | G2/G3, unenrolled or legacy-claimed | plaintext: LAN, then legacy by topic (v2 refuses plaintext) | legacy by topic | G3 unclaimed: bind by QR pins, topic optional (5.1.3). Legacy-claimed: the existing "Secure this Art Computer" flow (5.1.8) |
| Owned, this phone holds a grant | G2/G3 | sealed: LAN, then v2 if `transports` has `relayV2`, then legacy sealed by topic if a topic is known | v2 binding when on v2, else legacy by topic | n/a |
| Owned, no grant on this phone | G2/G3 | none until added | none | today's `ownedNeedsCode` (code on screen) |
| `locked` | G2/G3 | LAN recovery only | none | n/a |

The route rule for sealed commands is unchanged on purpose: v2 is used only
when the device's last status listed `relayV2`, because a v2 send to a
device that is not on v2 can be receipted by another peer and become
ambiguous instead of falling back (`ff1_device_control.dart`
`supportsRelayV2`). Every G3 image lists `relayV2` (§4.2), and the record is
refreshed over whatever route works (LAN or legacy) while legacy exists.

**Changes:**

1. **Identity is `deviceId`.** `topicId` becomes optional.
   `removeIncompleteDevices` (`ff1_device_service.dart:371`) deletes only
   rows with no topic AND no confirmed grant AND no `control` record.
   Enrollment results stop writing `topicId ?? ''` as a completeness marker.
2. **"Reachable" replaces "has topic"**: a LAN endpoint is known, or a grant
   is held and the device lists `relayV2`, or a topic is held. Sites:
   `ff1_connection_providers.dart`, auto-connect in `ff1_wifi_providers.dart`,
   active-device promotion, mint pairing provider, `site_pairing_join.dart`,
   role refresh and pending approvals in `ff1_secure_control_flows.dart`, and
   the topic-keyed control notifiers (re-key by `deviceId`). Legacy-only
   devices still have a topic, so they still qualify.
3. **LAN bind without a topic** (`ff1_lan_bind_provider.dart` ~575-600): for
   an unclaimed device whose status carries a `control` profile, accept the
   QR pins matching the status profile as identity proof, as the owned branch
   already does (~536-555). An `absent` device keeps the topic requirement.
4. **LAN routing by `deviceId`**: pass `deviceId` through `FF1CommandSender`
   so `ff1_lan_rest_client.dart` `_endpointForTopic` resolves without a
   topic.
5. **Legacy stays wired while it exists.** Keep `FF1_RELAYER_URL` configured
   in every build until Gate B (§6). The legacy notification socket, the
   mint-pairing legacy sidecar, and the ephemeral-sessions client stay for
   G0–G2 and legacy-state devices. The code must run correctly when the URL
   is empty (post-Gate-B builds): no legacy transport, no sessions client,
   no reset sweep. Make `FF1_RELAY_V2_ENDPOINT` required.
6. **Retired vs offline.** A legacy 410 (server-declared retirement) latches
   "legacy retired" for the app session: no retry, enrollment falls back to
   LAN, and the device row says why. DNS failure, connection refused and
   timeouts keep today's backoff (they are also what a phone with no signal
   sees).
7. **Approvals with an empty `topicID`** are accepted (matches 3.2).
8. **Messages** for a device the app cannot reach remotely:
   - `absent` device, legacy retired: "Update this Art Computer" (it updates
     itself overnight).
   - `control.state=legacy` and claimed: offer the existing "Secure this Art
     Computer" flow. It is the only migration path and already works without
     the legacy host: `FF1SecureControlFlows.showCode()` sends plaintext
     `showPairingQRCode{purpose:"enroll"}` by topic, LAN first
     (`ff1_control_enrollment_service.dart` `requestMigrationInvitation`;
     open on LAN per `hub/lanauth.go`), the device paints an invitation QR,
     and `completeWithScan` enrolls. Limit: it needs the phone that holds the
     stored topic; a phone without it (e.g. after a reinstall) cannot start
     it, and the remedy is a factory reset.

### 5.2 ff1-cli

Default the v2 endpoint to `wss://relayer.feralfile.io` instead of deriving
it from the legacy `baseUrl` (`src/config.ts` `resolveV2Endpoint`); keep the
explicit `FF1_RELAY_V2_ENDPOINT` override. Legacy `/api/cast` stays for
topic-only devices until Gate B.

### 5.3 ff-relayer

No code change. `tv-ws-usage` analytics record path and topic per request;
use them to count distinct topics per route per day.

## 6. Retirement gates (after G3 and the floors are live)

| Step | Action | Gate |
|---|---|---|
| A | `LEGACY_CONTROLLER_API_ENABLED=false` | app floor raised; Play integrators on 0.5; app `/api/cast` traffic ~0 |
| B | `LEGACY_DEVICE_API_ENABLED=false`, or host removed | running version ≥ G3 AND `reset_target_version` ≥ G3 for nearly all devices; legacy-dependent devices (claimed, topic present, `control_state=legacy`) under an agreed threshold; known remote installs contacted; shelf stock reflashed |

Remaining populations between G3 and B: legacy-claimed G3 devices waiting
for an owner at the LAN; devices whose OTA keeps failing (free space, slow
links, probation rollback); devices that are off; devices whose reset target
is still old (failed or missing recovery candidate); controllers outside the
force-update floor (old CLI, Play 0.4, app fail-open); networks that reach
the old host but block relayer.feralfile.io. §3.6 and §5.3 measure them.

If the host is removed rather than flag-retired, point the old hostname at
the v2 worker (all legacy flags false) so remaining old devices get a clean
410 instead of an edge error.

## 7. Verification

- Unit tests under each item; `make verify-go` for feral-controld.
- Bench (FF1 on develop, new app build):
  1. Both endpoints on relayer.feralfile.io, claim cleared: QR paints after
     v2 connects; the app binds by pins and enrolls on LAN; off-LAN cast,
     now-playing and a Play 0.5 pairing work over v2.
  2. Legacy endpoint on the live legacy host: topic lands after the QR is
     painted; the QR refreshes in place; the already-scanned invitation still
     enrolls.
  3. v2 blocked (hosts entry), legacy up: QR paints with the topic; behavior
     equals today's.
  4. Legacy host returns 410, then NXDOMAIN, mid-session: no controld restart,
     no redial storm (dial lines per minute), v2 stays up.
  5. `controld.json` without `relayer`: controld starts, v2 up.
  6. G3 controld package on a G1 image: no v2; QR waits for the topic as
     before.
  7. App against a G1 (2.0.11) device and a legacy-state G3 device: cast,
     now-playing and pairing work on LAN and off-LAN through legacy.
- Image: the build guard fails a config without `relayV2` for a ref that has
  `relaywire/`.
