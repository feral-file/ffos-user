# FF1 API v2 migration and implementation plan

- Status: proposed companion plan
- API contract: [FF1 communication API v2](ff1-v2-api-contract.md)
- Authentication profile: [FF1 v2 controller authentication and access sessions](ff1-v2-controller-authentication.md)

This document contains the current-state analysis, client and firmware
compatibility strategy, rollout and rollback gates, implementation sequence,
and release evidence for API v2. It does not define wire behavior. If this plan
conflicts with the API contract, the API contract governs protocol semantics.

## 1. Current flow and target boundary

Today the app sends HTTP GET requests with JSON bodies to a legacy remote
command endpoint and uses a fleet-wide API key and device topic ID.
`feral-controld` receives commands from a remote relayer WebSocket or the
unauthenticated `0.0.0.0:1111` LAN WebSocket, routes OS commands to `devicectl`,
player commands to Chromium/CDP, and polls status approximately every five
seconds. Device, player, and DDC state use separate ad-hoc shapes. BLE in
`feral-setupd` carries Wi-Fi setup and recovery commands.

V2 changes the external edge, not the internal service boundaries:

```text
enrolled mobile / CLI / integration / guest controller
              |
       Control API v2 JSON
       /                 \
MQTT 5 + broker       HTTPS + WSS on LAN
       \                 /
             feral-controld
          /       |        \
     devicectl   CDP   private D-Bus/services
```

Remote and LAN clients use one protocol model. MQTT is authoritative whenever
a transport choice affects the model. HTTPS is a resource/command adapter. The
LAN WebSocket carries subscription control from client to device and resource/
event push from device to client; it accepts no device-control commands and is
not a second command language. SoftAP bootstrap is the one intentional REST-only
exception because an unprovisioned device has no broker route or controller
credential.

## 2. V1-to-v2 migration

V2 client releases support legacy devices during fleet migration. V2 firmware
does not retain the shared relayer API key or unauthenticated LAN WebSocket as a
permanent fallback. Migration has two distinct gates. Pre-OTA eligibility runs
while the device still exposes only v1 adapters and proves that a v2-capable
controller is present, the exact target is accepted, and rollback is prepared;
it never claims that v2 endpoints already exist. Post-boot promotion runs from
the v2 candidate slot and proves enrollment plus the v2 transport contract
before that slot becomes permanent. Failure to promote within the bounded
probation window returns the device to the signed v1 image.

### 2.1 Executable client/firmware compatibility matrix

| Client artifact | Current/v1 firmware | FF OS v2 firmware | Required executable proof |
|---|---|---|---|
| Released/current ff-app | Existing relayer and current LAN | Unsupported; normal OTA MUST reject this client/firmware combination | Pinned released-app regression on v1 plus `old_app_v2_ota_denied` eligibility test |
| V2 ff-app | Legacy adapter plus target-bound pre-OTA eligibility acknowledgment | Persistent controller enrollment with silent MQTT/WSS access-session issuance, plus enrolled HTTPS and `ff-control.v2` LAN | Legacy preflight fixture; then post-boot one-time enrollment, no-prompt renewal, two independently enrolled mobile installations, offline-LAN command/push, reconnect, and DP-1 fixture run |
| Released/current ff1-cli | Existing supported path | Unsupported; normal OTA MUST reject this client/firmware combination | Pinned released-CLI smoke test plus `old_cli_v2_ota_denied` eligibility test |
| V2 ff1-cli or installed integration | Legacy adapter plus target-bound pre-OTA eligibility acknowledgment where supported | Enrolled MQTT/WSS 443 plus HTTPS and `ff-control.v2` LAN when available | Legacy preflight fixture plus post-boot CLI conformance, enrollment, access-session renewal, and LAN parity suite |
| Web client | Legacy temporary-access handoff where supported | One-time QR claim of a non-renewable MQTT guest session; no LAN certificate | Guest invitation, origin binding, MQTT claim, expiry, revocation, and scope-negative fixtures |
| Temporary agent or integration | Legacy temporary-access handoff where supported | One-time QR claim of a non-renewable MQTT guest session plus optional session-bounded LAN certificate | Guest invitation, key proof, MQTT claim, optional CSR and LAN mTLS/HTTPS/WSS, expiry, revocation, and scope-negative fixtures |
| Independent reference client | Not required | Enrolled or guest advertised subset | Separate minimal client passes published schemas and fixtures without private FF transport code |

The published conformance bundle MUST contain a machine-readable
`compatibility/matrix.json`. Each row names the test ID above, immutable client
artifact/version, firmware image/version, expected OTA eligibility, selected
adapter, fixture-suite IDs, expected result, and gate phase. A v1 preflight row
names only the legacy-control, compatible-client, exact-target, and rollback-
prepared checks. An FF OS v2 post-boot row names the release conformance checks
for MQTT WSS/443 authentication, retained state, idempotent command execution,
LAN HTTPS command parity, `ff-control.v2` initial snapshots, live LAN push, and
LAN reconnect convergence. An unsupported row names its negative OTA-
eligibility fixture instead. CI runs simulated adapter and gate cases on every
change; release evidence also records the real device serial, artifact digests,
restricted-network result where applicable, and timestamps. A prose-only
checked box is not migration evidence.

Normal v2 OTA requires a pre-OTA eligibility acknowledgment from a compatible
v2 app or CLI over the device's currently supported v1 path. The closed record
contains `acknowledgmentId`, `deviceId`, `controllerSigningKeyThumbprint`,
`contractVersion`, `clientVersion`, `targetFirmwareVersion`,
`targetImageSha256`, `acknowledgedAt`, and `expiresAt` no more than 24 hours
later. The controller proves possession of `controllerSigningKeyThumbprint`, but this
record grants no v2 enrollment or runtime authority. Its closed `checks` object
contains exactly `legacyControlObserved`, `clientSupportsV2`,
`targetImageAccepted`, and `rollbackCapsulePrepared`; every member MUST be
`true`. Immediately before OTA, the control plane requires the same unexpired
record, exact offered-image hash, still-supported client version, and device
attestation that the rollback capsule exists. Factory reset, target-image
change, client downgrade report, or expiry invalidates it. Both old-client/v2-
firmware negative cases above MUST pass before a cohort opens. A global app-
store flag, firmware-version guess, historical acknowledgment, or account-wide
acknowledgment cannot satisfy the gate.

The v2 image boots once as a candidate with a 30-minute probation deadline.
During probation the controller completes the physical owner-enrollment claim
and FF1 creates a closed promotion record containing `promotionId`, `deviceId`,
`controllerId`, `controllerSigningKeyThumbprint`, `contractVersion`, `clientVersion`,
`runningFirmwareVersion`, `runningImageSha256`, `verifiedAt`, and a closed
`checks` object containing exactly `mqttWss443Authenticated`,
`mqttRetainedStateObserved`, and `mqttIdempotentCommandObserved`; all three are
`true`. Its `controllerSigningKeyThumbprint` MUST equal the unexpired
eligibility record and the RFC 7638 thumbprint of the `signingKeyJwk` accepted
by the owner-enrollment claim. It also contains
closed `lanChecks` with `availability` equal to
`passed|network_unreachable`, required `candidateSelfTestPassed`, the four
booleans `httpsCommandObserved`, `initialSnapshotsObserved`,
`liveStateObserved`, and `reconnectSnapshotObserved`, and conditional
`networkDiagnostic`. `candidateSelfTestPassed` is always `true` and proves the
candidate listener, TPM-bound server certificate, local controller CA, pinned
HTTPS diagnostic request, `ff-control.v2` handshake, and initial snapshot over
a local test route. For `passed`, the four observed booleans are `true` and
`networkDiagnostic` is omitted. For `network_unreachable`, they are `false`
and `networkDiagnostic` is the following closed object, recorded after a
bounded direct attempt to the QR-pinned `lanUri` on an explicitly trusted
network: `code` equal to
`peer_isolation|name_resolution_blocked|route_unreachable`, `promotionId`,
`deviceId`, `controllerId`, `controllerSigningKeyThumbprint`,
`lanSpkiSha256`, `attemptedAt`, `nonce`, and `proof`. Before the attempt FF1
allocates `promotionId` and a one-use 16-byte random `nonce`. `attemptedAt` is
within 60 seconds of FF1 trusted time. `proof` is the authentication profile's
detached `ff1-proof+jws` over the JCS object with `proof` omitted, signed by the
same enrolled controller signing key named by the promotion record. FF1
verifies the signature, exact device/controller/promotion IDs, signing-key
thumbprint, current LAN SPKI pin, timestamp, and unused nonce before accepting
`network_unreachable`, then consumes the nonce. A missing, invalid, or replayed
proof, or a reachable-route TLS, pin, mTLS, HTTPS, or WSS failure, is a
candidate failure and triggers rollback.
`network_unreachable` does not block promotion because LAN discovery is not an
ownership prerequisite; full LAN parity remains a release-level conformance
gate. The update service promotes the slot only after validating this record,
the running image hash, active owner enrollment, and release conformance. A
missing or invalid record at the deadline triggers automatic rollback rather
than leaving a partially migrated device.

Rollback is a rehearsed release path, not merely a feature flag. Before each
cohort, the update service retains only the signed prior v1 image. FF1 creates a
device-local, TPM-sealed, one-use rollback capsule containing the previous
version/slot, non-secret display preferences, Wi-Fi configuration needed to
reconnect, and the legacy device topic binding. It explicitly excludes owner or
controller grants/keys, access tokens, device/controller certificates, SSH
keys, support data, DP-1 source credentials, and the fleet key (which belongs to
the signed v1 image, not the capsule). The capsule is encrypted and integrity-
bound to the TPM device identity and migration epoch, is never uploaded, and is
deleted after successful rollback, 30 days, factory reset, ownership transfer,
or explicit network forget. Revocation/reset tombstones take precedence, so a
rollback can never restore erased authorization. The rehearsal verifies that v2
can install and boot the prior image and runs `v2_to_v1_legacy_reconnect`: the
v2 app and v2 CLI rediscover the rolled-back device and reconnect through their
legacy adapters. The control plane changes migration state to `rolled_back`,
disables further v2 OTA for the device, and keeps the v1 relayer available. A
failed rollback becomes `reflash_required`; it is never reported as merely
offline.

Devices below the update service's signed `min_upgradeable_version` are never
offered normal v2 OTA. They remain on v1 and receive the existing QR-guided USB
recovery path; completing a signed USB recovery is the only transition from
`reflash_required` back to eligibility. The release gate tests a representative
below-minimum device, invalid or older USB image rejection, interrupted
recovery, successful recovery, owner/controller re-enrollment, and subsequent
v2 readiness. Factory reset is not presented as a substitute for recovery.

### 2.2 Commands

| Current v1 name or behavior | V2 operation or resource |
|---|---|
| `connect`, player `connect`/`disconnect` | MQTT CONNECT/DISCONNECT and retained presence; no command |
| `showPairingQRCode(show: true/false)` | `controllers.create-invitation` or `controllers.close-invitation` |
| `deviceMetrics`, `getDeviceStatus`, player `checkStatus` | `state/device`, `state/network`, `state/health`, `state/display`, `state/playback` |
| `sendKeyboardEvent` numeric code | controls/shortcuts -> paired W3C `input.keyboard`; committed printable Unicode -> permission-gated `input.text` |
| `dragGesture` | `input.pointer` move batch |
| `tapGesture`, `doubleTapGesture`, `longPressGesture` | `input.pointer` balanced button batch with optional timing |
| `clickAndDragGesture` | leased `input.pointer` down, bounded move batches, then up or `input.pointer-cancel` |
| `zoomGesture` | app converts pinch to `input.pointer` wheel; requires DP-1 scroll |
| `rotate` | absolute `display.set-rotation` |
| `shutdown`, `reboot` | `system.power` |
| `analyticsToggle`, `betaFeaturesToggle` | `settings.set-analytics`, `settings.set-beta-features` |
| `updateToLatestVersion` | `system.update` |
| `factoryReset` | `system.factory-reset` plus physical confirmation |
| `uploadLogs` with `apiKey` | `support.create-bundle`; service credentials stay on device/control plane |
| `setVolume`, `toggleMute` | `audio.set-volume`, absolute `audio.set-muted` |
| `sshAccess` | `ssh.set-access` with mandatory bounded TTL |
| `ddcPanelControl` brightness/contrast/power/panel-volume/panel-mute | matching `display.panel` discriminated control; OS mixer remains `audio.*` |
| `ddcPanelStatus` | `state/display.panel` |
| `setSleepSchedule`, `sleepNow`, `wakeNow` | `display.set-sleep-schedule`, `display.sleep`, `display.wake` |
| player `displayPlaylist` with URL or inline JSON | `playlist.display` with the unmodified DP-1 Playlist source union |
| player `DP1Intent.schedule_play` | `playlist.display` with `displayAt`; current playback continues and `scheduledDisplay` is separate |
| player `displayDefaultPlaylist` | `playlist.display-default` |
| legacy player pause/resume commands | `playback.pause`, `playback.resume` |
| `nextArtwork`, `previousArtwork`, `moveToArtwork` | `playback.next`, `playback.previous`, `playback.select` |
| `refreshArtwork` | `playback.refresh` |
| `updateDuration` | FF session override `playback.set-item-duration`; signed DP-1 Playlist is unchanged |
| `updateArtFraming` | `display.set-overrides` when DP-1 permits override |
| `setShuffle`, `setLoop` | `playback.set-shuffle`, `playback.set-loop` |
| internal `setSleepMode` | remains an internal player command; external clients use display sleep/wake |
| `startMintPairingSession` | `sessions.create-invitation`; the initiating enrolled controller selects the guest scope ceiling and TTL, which is the complete approval action |
| `closeMintPairingSession` | `sessions.close-invitation` |
| `mintPairingApprovalDecision` | removed; v2 has no second approval exchange after an enrolled controller creates a guest invitation |
| relayer temporary-session list/revoke | retained `state/sessions` plus `sessions.revoke`; FF1 owns expiry and revocation |
| legacy web-client HTTP relay request | an origin-bound web guest session publishes normal v2 commands through the shared controller contract |
| no v1 equivalent | `diagnostics.ping`, the read-only v2 transport fixture |
| no v1 equivalent | `input.pointer-cancel`, explicit release for a streamed long drag |
| no v1 equivalent | `playlist.get-document` and `playlist.read-chunk` recover a cached signed DP-1 Playlist without retaining credentials |
| no v1 equivalent | `playlist.cancel-scheduled` explicitly removes a pending or scheduled display |

Sleep-schedule migration preserves the current weekday selection. A missing v1
`days` field becomes all seven days; a valid nonempty subset maps unchanged to
the normalized Sunday-first v2 array. An empty or invalid stored selection
blocks automatic migration of that setting and is surfaced for correction; it
never silently becomes daily. Fixtures cover a weekday-only schedule,
unselected full-day sleep, manual overrides, time-zone boundaries, and the
missing-field daily default.

### 2.3 Notifications and discovery

| Current v1 behavior | V2 behavior |
|---|---|
| `player_status` polling notification | retained `state/playback`, changed on source-of-truth mutation |
| `device_status` polling notification | retained `state/device`, `network`, `display`, `health`, `controllers`, and `sessions` |
| relayer/hub connected booleans | MQTT presence plus network state; transport status is not mixed with device state |
| mint approval request/outcome notifications | removed; `sessions.invitation-closed`, `sessions.claimed`, `sessions.revoked`, and `sessions.expired` plus retained session state replace them |
| Mint Pairing Broker polling and encrypted handoff | direct MQTT invitation claim plus FF1-issued JWE credential delivery |
| mDNS advertises unauthenticated port 1111 | `_ff1-control._tcp.local` advertises authenticated HTTPS and API major |
| WebSocket notification query string | MQTT subscriptions or authenticated `ff-control.v2` LAN WebSocket subscriptions |
| `displayURL`, MAC info in general status | removed; debug URLs are private implementation details and network identifiers require explicit scope |

#### Legacy temporary-access flow

The v1 client-kind-specific name and approval state machine do not carry into
v2. The controller action becomes **Create guest access**. It sends
`sessions.create-invitation` once, and FF1 displays the resulting one-time QR.
The web client or temporary agent claims that invitation directly. The creating
controller has already selected the scope ceiling and lifetime, so no approval
notification or decision command follows.

V1 Mint Pairing Broker channels, relayer session tokens, approval requests, and
active temporary sessions are not converted into v2 credentials. They remain
available only to v1 firmware during the compatibility window. After a device
migrates, a requester scans a new FF1 guest invitation and receives an
FF1-issued session. The player overlay may be reused as presentation code, but
its protocol state is only invitation open, claimed, closed, or expired.

### 2.4 App architecture

The app keeps its existing `transport -> protocol -> control` layering but makes
transport symmetric:

- `FF1Transport` connects, publishes, subscribes, and exposes connection state;
- `MqttFF1Transport` and `LanFF1Transport` carry identical protocol documents;
  the latter uses HTTPS for reads/commands and `ff-control.v2` WebSocket for
  resource/event subscriptions;
- `FF1ProtocolV2` validates envelopes, schemas, revisions, capabilities,
  idempotent responses, and DP-1 Playlists; and
- `FF1Control` exposes typed operations and resource streams to Riverpod/UI.

The unauthenticated legacy WebSocket and its query-string notification filter
are deleted after migration. The new subscription-control/server-push LAN channel is mTLS-
authenticated, scope-filtered, and revision-aware. Optimistic UI may remain,
but a command response and then the authoritative retained revision reconcile
it.

### 2.5 Hosted observation and fleet APIs

Failures before an MQTT session exists cannot be represented by device
presence. Two authenticated control-plane HTTPS resources complete the support
contract; they are operational APIs, not alternate device-control bindings.

`GET /control/v2/devices/{deviceId}/observations?since=<timestamp>&limit=<1..200>`
requires either the owner principal bound to that exact device or a scoped
support/admin role; a merely authenticated unrelated principal is forbidden.
It returns the closed object `apiVersion: "ff/control/v2"`, `deviceId`,
`generatedAt`, optional `brokerSession`, and `commandDeliveries[]`.
`brokerSession` is `observedAt`, `stage`:
`tls|websocket|mqtt|authorization|connected|disconnected`, `outcome`:
`succeeded|failed`, optional MQTT/TLS `reasonCode`, and optional
`controllerId`. `commandDeliveries` entries are `requestId`,
`brokerAcceptedAt`, optional `deviceRespondedAt`, and status
`broker_accepted|device_succeeded|device_accepted|device_failed|expired_no_response`.
Results are ordered newest first and retained for 30 days.

The broker can attribute only attempts that present a known device/controller
identity. DNS failure, blocked TCP/WSS, and captive portals never reach it; ff-app
reports those from its local transport diagnostics and FF1 exposes its side in
`state/health.controlTransport` over LAN or after recovery. The support view
combines those sources and never claims the broker observed an unreachable
attempt.

`GET /control/v2/fleet/migrations?state=<state>&cursor=<opaque>&limit=<1..500>`
is support/admin-only and returns `apiVersion`, `generatedAt`, `devices[]`, and
optional `nextCursor`. Each closed device entry has `deviceId`, state
`not_eligible|eligible|updating|probation|migrated|rolled_back|offline_unknown|reflash_required`,
optional `acknowledgmentId`, `promotionId`, `contractVersion`, `controllerId`,
`controllerSigningKeyThumbprint`, `acknowledgedAt`, `verifiedAt`, `expiresAt`,
`clientVersion`, `targetFirmwareVersion`, `targetImageSha256`, `lastSeenAt`, and
stable `reason`.
This is the executable query surface behind
the readiness tuple and fleet migration gate, not a manually inferred dashboard.

## 3. Deployment and validation gates

These are release acceptance gates for later implementation. They do not add or
modify wire behavior. The MQTT vertical spike is time-
boxed to ten working days once staffed, uses a bought/managed EMQX service
(target EMQX Cloud), and does not permit a self-hosted broker substitute.

1. On one real FF1 and ff-app, provision the TPM device identity, complete one
   owner-enrollment QR claim, request an enrolled-controller access session,
   and connect to managed EMQX using MQTT 5 over WSS on port 443. Prove device
   mTLS, invitation/enrollment/access User Name and Password authentication,
   per-principal ACLs, retained messages, the Will Message, Message Expiry, and
   packet limits. An unproven vendor-supported WSS/443 front door fails the
   spike and requires a different managed offering/vendor.
2. On that connection, publish retained capabilities/current state and retained
   connected presence, then prove the broker publishes the retained
   disconnected Will Message after forced connection loss.
3. Execute `diagnostics.ping` as the read-only fixture and at least one declared
   side-effecting command, observing its new state revision. Record correlated
   typed success and typed failure responses for both valid and invalid cases.
4. Interrupt and recover separately from Wi-Fi loss, broker disconnect, device
   reboot, and ff-app restart. After each, resubscribe, recover retained state,
   pass the epoch/revision/event-watermark rules, and prove no QR or user-facing
   authentication prompt occurs.
5. Redeliver an identical QoS 1 side-effecting command and prove one effect and
   the byte-equivalent cached response; send the same `requestId` with a
   different body and prove `duplicate_conflict`.
6. Attempt unauthorized publish and SUBSCRIBE operations from the controller
   and another device/controller identity. Record broker PUBACK/SUBACK or
   DISCONNECT Reason Codes and sanitized control-plane evidence.
7. Record the MoMA evidence for matched/no-matching-subscriber PUBACK,
   disconnected/stale presence, device response, and expired-no-response so
   support can classify the failure without a device-log dive.
8. Repeat WSS/443 connection and command execution from at least one real
   institutional or equivalently restricted network.
9. On the same real FF1, enroll ff1-cli through the MQTT QR flow, then make the
   internet and broker unavailable while the owner mobile remains enrolled:
   discover the already-enrolled device through mDNS, execute the same allowed
   command fixtures over HTTPS, subscribe through
   `ff-control.v2`, receive initial device/display/
   health snapshots and live DDC/network changes, recover through a forced
   socket reconnect and backpressure closure, reject an unenrolled client and
   out-of-scope resource, expose offline/cache readiness, and prove factory
   reset leaves no unauthenticated port-1111 surface or controller
   authorization.
10. Verify the actual FF1 TPM supports the selected P-256 key lifecycle,
   attestation evidence, key renewal, secure deletion, factory reset behavior,
   and a trusted advancing RTC across power loss. If the last item is absent,
   document that enrolled LAN mTLS fails closed after offline reboot until NTP.
11. Register the TPM-backed FF1 controller-issuer public key with the broker,
   validate FF1-signed invitation, enrollment-only, and access-session
   credentials without a per-session control-plane lookup, then test immediate
   device rejection and broker disconnect on controller or session revocation.
12. Run the SoftAP/captive-portal spike on the required phone matrix and measure
   wrong-password recovery, AP/client concurrency, and auto-open behavior.
13. Verify the fixed DP-1 trust profile against real Feral File feeds, institution trust
   roots, key rotation/revocation, and expired-trust offline cache fixtures.
14. Define the support service's device-bound upload-grant exchange. The public
   device command remains unchanged and never receives a service API key.

## 4. Implementation sequence

No implementation is part of this contract-definition change. When separately
authorized, the future sequence must preserve cached-art playback and permit
rollback at every fleet gate:

1. Publish the normative JSON Schema bundle, AsyncAPI 3.0 document, and OpenAPI
   3.1.1 adapter from this contract. CI validates every example and rejects
   schema drift between MQTT and HTTPS. Schemas, fixtures, generated types, and
   the minimal reference client are published under the repository's
   Apache-2.0 license without private Feral File transport dependencies.
2. Implement protocol types and conformance tests in both repositories without
   changing production transport. Add golden vectors for JCS hashes, UUID
   correlation bytes, expiry, RFC 9457 errors, and every command/state/event.
3. Implement `feral-controld` retained state and idempotent dispatcher behind a
   feature flag. Keep service ownership boundaries unchanged, while versioning
   the internal factory-reset D-Bus contract for prepare/physical-decision/
   execute and adding only the source-of-truth signals required by retained
   state.
4. Complete TPM enrollment and broker spike, including WSS/443, Will Message, ACL
   negative tests, revocation, certificate rotation, NTP loss, packet limits,
   rate limiting, clean restart, and duplicate QoS 1 delivery.
5. Prove the unified controller-authentication profile against the managed
   broker: device-signed JWT validation without a per-session authentication
   service, one-time invitation consumption, controller-key proof, JWE
   delivery, silent enrolled-session and enrollment-credential renewal,
   multiple independent controller enrollments, WebSocket Origin binding for
   web guests, exact ACLs, expiry, revocation, and normal `playlist.display`
   through a guest session. Direct broker validation of the registered FF1
   issuer is a gate.
6. Add authenticated LAN HTTPS/WebSocket/mDNS and run one shared protocol
   conformance suite against MQTT and LAN. The same command vector must produce
   the same response and final state revision, and both subscription bindings
   must deliver schema-equivalent snapshots/events.
7. Add ff-app v2 transport/protocol/control implementations and legacy routing.
   Exercise one-time owner enrollment, silent enrolled-session and credential
   renewal, a second mobile enrollment, a concurrent CLI enrollment, guest
   invitation and claim, remote and offline-LAN Playlist display, sleep,
   update, recovery, keyboard, tap, double tap, long press, drag, wheel/pinch,
   guest expiry and revocation, reconnect, and stale presence.
8. Ship the v2-capable app first, then canary firmware. Current firmware records
   only the target-bound pre-OTA eligibility acknowledgment. Boot the v2 image
   once in probation, complete owner enrollment and MQTT promotion checks, and
   promote the slot only after the closed post-boot record validates. A blocked
   LAN records `network_unreachable` and does not prevent MQTT-based ownership;
   failure to complete required promotion checks triggers rollback.
9. Remove the shared API key, old relayer, Mint Pairing Broker, port-1111 hub,
   JSON-body GET commands,
   legacy `DP1Intent`/`dynamicQueries`, and BLE only after fleet-specific rollback
   thresholds and the SoftAP gate pass.

Already claimed devices do not preserve a v1 topic credential or temporary
session as v2 authorization. The pre-OTA acknowledgment proves only controller
key possession and client compatibility; it cannot create or imply ownership.
After the candidate boots, the owner completes the same physical one-time
owner-enrollment QR claim as a new v2 device. Institutional and CLI-only owners
use the same invitation claim; there is no app-only migration credential. The
eligibility and promotion records are stored in the update/control plane with
the separate closed, expiring, target-bound shapes from section 2.1. Fleet
state is
`not_eligible|eligible|updating|probation|migrated|rolled_back|offline_unknown|reflash_required`.

Minimum acceptance checks are:

- every operation rejects unknown fields and has golden success, each declared
  operation-specific failure, authorization denial, expiry, duplicate, and rate
  tests;
- MQTT QoS 1 redelivery produces one effect and the byte-equivalent stored
  response; same ID/different body fails;
- state converges after process restart, network flap, broker loss, and out-of-
  order event delivery using epoch/revision rules;
- cached verified DP-1 playback and authenticated LAN control work with no
  internet or broker;
- DP-1 invalid/signature/license/repro/source failures preserve their namespaced
  errors and never render an unverified Playlist;
- pointer/keyboard input is blocked unless the active DP-1 PlaylistItem permits it, is
  never delivered after expiry, and always releases pressed buttons and keys;
- one controller cannot publish, subscribe, request a response, or discover
  sensitive state for another device/controller;
- multiple enrolled mobile apps and CLIs can connect concurrently, renew
  independently, and survive another controller's expiry, rotation, or
  revocation without a QR prompt;
- revocation, certificate expiry/rotation, factory reset, and last-owner
  protection are tested end-to-end;
- enrolled LAN HTTP and local push expire at the issued authorization-lease
  deadline, bounded to at most 900 seconds, even when the client certificate is
  still valid, followed by a silent mutually authenticated reconnect;
- every `state/sessions` invitation and session variant passes closed-schema,
  terminal-removal, and restart-invalidation fixtures;
- v1 weekday sleep selections survive migration exactly, including the
  missing-field daily default and non-daily subsets; and
- logs and all state/events pass secret, credential, PII, DP-1 source, and remote
  input redaction tests.

## 5. Requirement traceability

| Requirement | Coverage |
|---|---|
| One versioned open contract; stable resource names | API sections 1, 3, 4.2, and 9 |
| MQTT 5 remote in the first compatibility epoch | API sections 4 and 7; plan sections 2.1 and 3 |
| MQTT/WSS on port 443 hard gate | API section 4.1 and plan section 3 |
| Typed commands, successes, errors, and correlation | API sections 4.2, 6, and 11 |
| Creation time, expiry, and clock skew | API sections 3.2, 4.2, 4.4, and 7.3 |
| QoS 1 duplicate safety and idempotency | API sections 4.1.2, 4.2, and 4.4 |
| QoS, retained, Session, and Will policy | API sections 4.1 through 4.3 |
| Revision/order behavior after reconnect | API sections 3.3, 4.2, and 13 |
| Connected/disconnected/stale/unknown presence | API section 4.3 |
| Capability discovery; no firmware guessing | API section 9 |
| Packet limits, rate limits, restricted commands | API sections 4.4, 7.4, 9, and 11 |
| Per-device/controller identity, TPM, rotation, revocation, ACLs | API sections 7 and 8 |
| MQTT User Name/Password or mTLS without custom AUTH | API sections 4.1.1 and 7 |
| Brokerless authenticated LAN HTTPS using the same contract | API sections 5, 7, and 8 |
| Authenticated realtime LAN state/event push with polling fallback | API section 5 |
| mDNS discovery and rejection of unenrolled clients | API sections 5, 7, and 8 |
| DP-1 v1.1.0 Playlist validation, trust, and offline cache | API sections 10, 11.1, and 12.3 |
| Mobile keyboard/touchpad remote control | API section 11.3 |
| Unified enrolled and guest controller access without relayer or per-session auth service | Controller authentication profile; API sections 7.4, 9, 11.7, and 12.7 |
| Persistent one-scan mobile/CLI authorization with multiple independent controllers | Controller authentication sections 4.2, 5.1, 5.3, and 14; API sections 7.2, 8, and 9 |
| Setup/claim recovery boundary | API section 14 |
| Offline/cache readiness | API section 12.5 |
| MoMA delivery/connection diagnosis | API sections 4.2 and 12.5; plan section 2.5 |
| Executable client/firmware matrix and negative OTA gates | Plan section 2.1 |
| Per-device pre-OTA eligibility, post-boot promotion, rollback, and USB recovery | Plan sections 2.1 and 2.5 |
| One-time enrollment transition for verifiable existing owners | Authentication profile and plan section 4 |
| Open schemas, fixtures, reference client, integration-friendly license | Plan section 4 |
| No broker dependency for art already on the wall | API sections 10, 11.1, and 12.5 |
