# FF1 API v2 migration and implementation plan

- Status: design-draft companion plan; not conformance-ready
- API contract: [FF1 communication API v2](ff1-v2-api-contract.md)
- Authentication profile: [FF1 v2 controller authentication and access sessions](ff1-v2-controller-authentication.md)

This document contains the current-state analysis, client and firmware
compatibility strategy, rollout and rollback gates, implementation sequence,
and release evidence for API v2. It does not define wire behavior. If this plan
conflicts with the API contract, the API contract governs protocol semantics.

The linked prose documents define a proposed profile only. Before target
version `2.0.0` becomes normative, the repository MUST publish the complete
JSON Schema 2020-12 bundle, AsyncAPI document, OpenAPI document, positive and
negative fixtures, and automated MQTT/LAN parity validation. No implementation
or release gate may claim FF1 API v2 conformance from prose alone.

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
credential. LAN is broker-independent after trusted time exists in the current
boot; pre-NTP LAN after a power-loss reboot is available only when the required
`transports.lanOfflineAfterPowerLoss` capability is `true`.

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
| V2 ff-app | Legacy adapter plus target-bound pre-OTA eligibility acknowledgment | Persistent controller enrollment with silent MQTT/WSS access-session issuance, plus enrolled HTTPS and `ff-control.v2` LAN | Legacy preflight fixture; then post-boot one-time enrollment, no-prompt renewal, two independently enrolled mobile installations, brokerless same-boot LAN command/push, capability-conditioned post-power-loss LAN, reconnect, and DP-1 fixture run |
| Released/current ff1-cli | Existing supported path | Unsupported; normal OTA MUST reject this client/firmware combination | Pinned released-CLI smoke test plus `old_cli_v2_ota_denied` eligibility test |
| V2 ff1-cli or installed integration | Legacy adapter plus target-bound pre-OTA eligibility acknowledgment where supported | Enrolled MQTT/WSS 443 plus HTTPS and `ff-control.v2` LAN when available | Legacy preflight fixture plus post-boot CLI conformance, enrollment, access-session renewal, and LAN parity suite |
| Web client | Legacy temporary-access handoff where supported | One-time QR claim of a non-renewable MQTT guest session; no LAN certificate | Guest invitation, browser-only WSS Origin comparison, MQTT claim, expiry, revocation, and scope-negative fixtures |
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
The closed promotion record also contains required boolean
`lanOfflineAfterPowerLoss`, exactly equal to the advertised capability and the
signed `LAN_443_FULL_IMAGE` release-conformance record for the running image.
`true` is accepted only when that record links executable, real-hardware
power-cycle evidence for the exact full-image digest; `false` is accepted only
when the fail-closed pre-NTP negative case and same-boot brokerless positive
case pass. A candidate cannot promote with a missing, inferred, or mismatched
value.
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
| legacy web-client HTTP relay request | a web guest session with browser-only WSS Origin checking publishes normal v2 commands through the shared controller contract |
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
available only to v1 firmware during the compatibility window. A successfully
promoted v2 device image contains none of those paths; a rolled-back,
below-minimum, or current-v1 device keeps all of them. Hosted v1 services remain
available until the remaining legacy fleet passes its separate
infrastructure-retirement gate. After a device promotes, a requester scans a
new FF1 guest invitation and receives an FF1-issued session. The player overlay
may be reused as presentation code, but its protocol state is only invitation
open, claimed, closed, or expired.

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

### `LAN_443_FULL_IMAGE` gate

The only accepted FF OS LAN deployment target is the system-level
`ff1-control.socket` plus hardened `systemd-socket-proxyd` raw TCP front end on
443, forwarding to unprivileged `feral-controld`'s loopback-only TLS listener
at `127.0.0.1:8443`. The proxy terminates no TLS and parses no application data;
the system manager performs the privileged bind, the proxy has a dedicated
unprivileged identity with no ambient or bounding capabilities, and
`feral-controld` receives neither root privilege nor `CAP_NET_BIND_SERVICE`.
Listener activation alone is not readiness, and mDNS remains withdrawn until
the public proxy path, TLS identity, authorization store, and LAN interface pass
the end-to-end readiness check. Reset withdraws mDNS before quiescing the
backend and keeps the path fail-closed through all three pending-reset
lifecycles.

This target crosses component-binary and user/system-unit release rails. The
`LAN_443_FULL_IMAGE` gate does not pass, and the LAN implementation step cannot
be enabled or called executable, until a
coordinated full-image candidate boots on actual FF1 hardware and passes IPv4,
IPv6, TLS 1.3, mTLS allow/deny, HTTPS, WebSocket, listener-loss withdrawal,
power-cycle, and all three pending-reset lifecycle tests through public port
443.
The release PR MUST add the matching full-image declaration to `RELEASES.md`,
including the version and exact `ffos` `build-image-to-cf.yml` dispatch
parameters, and that matching `ffos` image build MUST carry the component and
unit revisions together. A package-only release is forbidden. This docs-only
contract PR changes neither shipping rail and therefore MUST NOT add an
implementation release entry.

#### Future executable gate

The LAN implementation MUST add the reusable workflow
`feral-file/ffos/.github/workflows/lan-443-full-image-conformance.yml`. Its
required producer job ID is `produce_lan_443_attestation`. The existing
`build-image-to-cf.yml` workflow MUST call it only after the candidate image is
immutable and MUST pass these outputs from its trusted build job, not
operator-entered replacements:

- `version`;
- `ffos_commit`, the complete 40-character Git commit that built the image;
- `ffos_user_ref`, the complete 40-character `ffos-user` Git commit embedded in
  the image;
- `image_uri` and `image_sha256`, identifying the same immutable image bytes;
  and
- `lan_offline_after_power_loss`, the value the image advertises as
  `transports.lanOfflineAfterPowerLoss`.

The producer MUST download and hash the image independently, flash those exact
bytes to a lab-managed FF1, reboot it, and bind every test to a fresh run nonce.
Before testing the API, it MUST verify the installed-image measurement against
the build's measurement manifest and verify a nonce-bound TPM quote from the
FF1. The quote evidence MUST bind the measured boot state, the hardware
inventory identity for model and revision, and the installed image measurement.
An input digest, version string, device-reported version, VM, container, or
mocked listener is not proof that the subject image ran on FF1 hardware.

The hardware trust anchor is the offline **Feral File FF1 Hardware Inventory
Root**, not a test-run key or a self-asserted model string. The implementation
MUST add its certificate and SPKI SHA-256 pin to the verifier's versioned trust
policy; changing that policy is security-review-only, triggers this gate, and
invalidates evidence issued under a removed root. The root signs a DSSE/in-toto
hardware-inventory statement whose subject is the TPM Endorsement Key public
key SHA-256 and whose closed predicate contains `model: "FF1"`, `revision`, a
random non-serial `inventoryId`, `ekPublicKeySha256`, `akName`,
`akPublicKeySha256`, and `issuedAt`. Issuance requires proof of possession of
the Endorsement Key and Attestation Key and validation of the Endorsement Key
certificate against the trust policy's TCG manufacturer-CA allowlist. The raw
serial number MUST NOT enter release evidence.

The test quote MUST be signed by that inventory statement's Attestation Key
over the SHA-256 PCR bank and PCR selection `0,2,4,7,11,15`. PCR 15 is the FF
OS image measurement extended by the measured early-boot chain with the
builder's subject image SHA-256; this is an FF OS measured-boot customization.
The quote's `qualifyingData` is exactly SHA-256 over the UTF-8 label
`LAN_443_FULL_IMAGE`, one zero byte, the raw run-nonce bytes, the hardware-
inventory-statement SHA-256, the measurement-manifest SHA-256, and the subject
image SHA-256, in that order. The verifier checks the quote signature, PCR
selection and values, freshness, inventory signature and claims, Endorsement
Key chain, Attestation Key binding, and replay nonce.

The measurement manifest is itself a Sigstore bundle with in-toto predicate
type `https://feralfile.com/attestations/ffos-measurement-manifest/v1`, signed
by the protected-ref GitHub OIDC identity for
`feral-file/ffos/.github/workflows/build-image-to-cf.yml`. Its single subject
is the same image SHA-256; its closed predicate contains `pcrBank: "sha256"`,
`pcrSelection: [0,2,4,7,11,15]`, an `expectedPcrs` object with one lowercase
64-hex value per selected PCR, and `eventLogSha256`. The verifier hashes this
bundle to `measurementManifestSha256`, verifies its signature and subject,
replays the measured-boot event log to the quoted PCRs, and requires PCR 15's
image event to contain the subject digest. A differently signed manifest,
unselected PCR, missing event, or self-reported digest fails the gate.

The future `ffos-user` implementation MUST also extend
`.github/workflows/release-guardrail.yaml` with required job ID
`lan_443_full_image` and display name `LAN_443_FULL_IMAGE`, running on every PR
into `staging` or `release`. It invokes
`scripts/verify-lan-443-full-image-release.sh <base-ref>`. The LAN
implementation PR MUST add the verifier and its watched-path manifest before it
enables the endpoint; the manifest MUST include the `feral-controld` LAN edge,
the socket/proxy units and their image-install inputs. A change to the manifest
itself also triggers the gate. A required run that is skipped, neutral,
cancelled, or unable to obtain evidence fails closed.

#### Signed conformance artifact

On success, the producer publishes one immutable
`lan-443-full-image-conformance.v1.sigstore.json` and one content-addressed
evidence bundle. The first is a
[Sigstore bundle](https://docs.sigstore.dev/about/bundle/) containing a
DSSE-signed [in-toto Statement v1](https://in-toto.io/Statement/v1). Its
`predicateType` is
`https://feralfile.com/attestations/lan-443-full-image-conformance/v1`; the
single subject is the exact FF OS image and its SHA-256 digest. The keyless
signature identity MUST be a GitHub Actions OIDC identity for
`feral-file/ffos/.github/workflows/lan-443-full-image-conformance.yml` at a
protected release ref. The bundle MUST carry the certificate chain,
transparency-log inclusion material, and signed timestamp needed for offline
policy verification. A signature from a developer, another repository,
another workflow, a pull-request ref, or a rerun with a different subject does
not satisfy the gate.

The signed statement has this closed logical shape; lowercase SHA-256 values
are exactly 64 hexadecimal characters, Git commits are exactly 40 lowercase
hexadecimal characters, IDs are decimal strings, and times are UTC RFC 3339
timestamps:

```json
{
  "_type": "https://in-toto.io/Statement/v1",
  "subject": [
    {
      "name": "ffos://image/2.0.0",
      "digest": {
        "sha256": "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
      }
    }
  ],
  "predicateType": "https://feralfile.com/attestations/lan-443-full-image-conformance/v1",
  "predicate": {
    "gate": "LAN_443_FULL_IMAGE",
    "result": "pass",
    "version": "2.0.0",
    "startedAt": "2026-07-22T00:00:00Z",
    "finishedAt": "2026-07-22T00:20:00Z",
    "runNonce": "019c0000-0000-7000-8000-000000000001",
    "ffos": {
      "commit": "0123456789abcdef0123456789abcdef01234567",
      "workflow": ".github/workflows/lan-443-full-image-conformance.yml",
      "runId": "1234567890",
      "runAttempt": "1"
    },
    "ffosUser": {
      "ref": "89abcdef0123456789abcdef0123456789abcdef",
      "runtimeTreeSha256": "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"
    },
    "hardware": {
      "model": "FF1",
      "revision": "1",
      "inventoryStatementSha256": "123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef0",
      "tpmQuoteSha256": "23456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef01",
      "measurementManifestSha256": "3456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef012"
    },
    "capabilities": {
      "lanOfflineAfterPowerLoss": false
    },
    "evidence": {
      "uri": "https://conformance.example.invalid/sha256/456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123",
      "sha256": "456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123"
    },
    "tests": [
      {
        "id": "image.boot_measurement",
        "result": "pass",
        "evidencePaths": [
          "tests/image.boot_measurement.json"
        ]
      },
      {
        "id": "listener.ipv4.public_443",
        "result": "pass",
        "evidencePaths": [
          "tests/listener.ipv4.public_443.json"
        ]
      },
      {
        "id": "listener.ipv6.public_443",
        "result": "pass",
        "evidencePaths": [
          "tests/listener.ipv6.public_443.json"
        ]
      },
      {
        "id": "tls.version_1_3",
        "result": "pass",
        "evidencePaths": [
          "tests/tls.version_1_3.json"
        ]
      },
      {
        "id": "tls.server_identity",
        "result": "pass",
        "evidencePaths": [
          "tests/tls.server_identity.json"
        ]
      },
      {
        "id": "tls.server_identity_negative",
        "result": "pass",
        "evidencePaths": [
          "tests/tls.server_identity_negative.json"
        ]
      },
      {
        "id": "mtls.authorized",
        "result": "pass",
        "evidencePaths": [
          "tests/mtls.authorized.json"
        ]
      },
      {
        "id": "mtls.unauthorized",
        "result": "pass",
        "evidencePaths": [
          "tests/mtls.unauthorized.json"
        ]
      },
      {
        "id": "https.command",
        "result": "pass",
        "evidencePaths": [
          "tests/https.command.json"
        ]
      },
      {
        "id": "websocket.subscription",
        "result": "pass",
        "evidencePaths": [
          "tests/websocket.subscription.json"
        ]
      },
      {
        "id": "mdns.readiness_withdrawal",
        "result": "pass",
        "evidencePaths": [
          "tests/mdns.readiness_withdrawal.json"
        ]
      },
      {
        "id": "privilege.controld_unprivileged",
        "result": "pass",
        "evidencePaths": [
          "tests/privilege.controld_unprivileged.json"
        ]
      },
      {
        "id": "privilege.proxy_hardened",
        "result": "pass",
        "evidencePaths": [
          "tests/privilege.proxy_hardened.json"
        ]
      },
      {
        "id": "reset.pending_broker_cleanup",
        "result": "pass",
        "evidencePaths": [
          "tests/reset.pending_broker_cleanup.json"
        ]
      },
      {
        "id": "reset.pending_identity_rotation",
        "result": "pass",
        "evidencePaths": [
          "tests/reset.pending_identity_rotation.json"
        ]
      },
      {
        "id": "reset.pending_authority_bootstrap",
        "result": "pass",
        "evidencePaths": [
          "tests/reset.pending_authority_bootstrap.json"
        ]
      },
      {
        "id": "reset.authority_bootstrap_idempotency",
        "result": "pass",
        "evidencePaths": [
          "tests/reset.authority_bootstrap_idempotency.json"
        ]
      },
      {
        "id": "reset.completed_authority_bootstrap",
        "result": "pass",
        "evidencePaths": [
          "tests/reset.completed_authority_bootstrap.json"
        ]
      },
      {
        "id": "brokerless.same_boot_control_push",
        "result": "pass",
        "evidencePaths": [
          "tests/brokerless.same_boot_control_push.json"
        ]
      },
      {
        "id": "power_loss.pre_ntp_fail_closed",
        "result": "pass",
        "evidencePaths": [
          "tests/power_loss.pre_ntp_fail_closed.json"
        ]
      },
      {
        "id": "power_loss.ntp_recovery",
        "result": "pass",
        "evidencePaths": [
          "tests/power_loss.ntp_recovery.json"
        ]
      }
    ]
  }
}
```

`tests` MUST contain exactly one passing entry for each of:

- `image.boot_measurement`;
- `listener.ipv4.public_443` and `listener.ipv6.public_443`;
- `tls.version_1_3`, `tls.server_identity`,
  `tls.server_identity_negative`, `mtls.authorized`, and `mtls.unauthorized`;
- `https.command`, `websocket.subscription`, and `mdns.readiness_withdrawal`;
- `privilege.controld_unprivileged` and `privilege.proxy_hardened`;
- `reset.pending_broker_cleanup`, `reset.pending_identity_rotation`,
  `reset.pending_authority_bootstrap`,
  `reset.authority_bootstrap_idempotency`, and
  `reset.completed_authority_bootstrap`; and
- `brokerless.same_boot_control_push` for both capability values; then either
  `power_loss.rtc_advancement_nonrollback`,
  `power_loss.pre_ntp_control_push`, `power_loss.certificate_expiry`, and
  `power_loss.authorization_lease_expiry` when `lanOfflineAfterPowerLoss` is
  `true`, or both `power_loss.pre_ntp_fail_closed` and
  `power_loss.ntp_recovery` when it is `false`.

The evidence URI MUST be content-addressed, immutable, and retained for at
least the supported lifetime of the image. Its bytes hash to `evidence.sha256`
and contain a closed `manifest.json` with `schemaVersion: "1"`, the gate,
subject image digest, run nonce, FF OS and `ffos-user` commits, hardware
inventory-attestation path, TPM quote path, measurement-manifest path, and a
`files` array. Each file entry has only `path`, `mediaType`, `sizeBytes`, and
`sha256`; paths are unique, relative, and cannot traverse out of the bundle.
Every `tests[].evidencePaths` value MUST name a manifest entry.

Each referenced test JSON is a closed object containing only
`schemaVersion: "1"`, `testId`, `runNonce`, `imageSha256`, `startedAt`,
`finishedAt`, `assertions`, `result`, and `evidencePaths`. `assertions` is a
non-empty array of closed objects containing only `id`, `operator`, `expected`,
`observed`, `result`, and `evidencePaths`. `expected` and `observed` are a JSON
null, boolean, signed 64-bit integer, string of at most 4,096 bytes, or a
lexically sorted array of unique strings, or a numerically sorted array of
unique signed 64-bit integers. The operator is exactly
`equals|set_equals|greater_than_or_equal|less_than_or_equal`. `equals` requires
identical JSON types and values; `set_equals` requires two sorted unique-string
or unique-integer arrays with identical element types and members; the
comparison operators accept signed 64-bit integers only. No operator reads an
untyped log to decide its result.
Every evidence path names a manifest entry. A test passes only when its image
and nonce equal the signed predicate, its assertion-ID set exactly matches the
registry below, the verifier independently applies each registered operator to
the typed expected and observed values, every assertion passes, and the test's
result is `pass`. Unknown or duplicate assertions, an unregistered operator,
missing evidence, or a top-level result inconsistent with an assertion fails
the gate.

Registry tuples below are `id / operator / operand type / expected`. A literal
is used as written. `$subject.imageSha256` resolves from the verified signed
statement; `$derived.qualifyingData` is recomputed by the exact concatenation
rule above; `$policy.pcrSelection` is `[0,2,4,7,11,15]`; and
`$fixture.<name>` resolves only from closed `fixtures.json` in the
content-addressed evidence manifest. The resolved value is stored in
`expected`. There are no other variables or implicit assertions.

`fixtures.json` is a closed object containing exactly `tls`, `mtls`, `https`,
`websocket`, `mdns`, and `reset`. `tls` is closed and contains exactly `deviceUriSan`:
the canonical `urn:ff:device:<deviceId>`, `dnsSan`: the canonical
`<deviceId>.local`, and `lanSpkiSha256`: 43-character unpadded base64url
SHA-256 of the active leaf DER SubjectPublicKeyInfo, plus
`leafCertificateSha256`: lowercase 64-character SHA-256 of the complete active
leaf DER certificate. `mtls` is closed and
contains exactly `deviceId`, `controllerId`, `sessionId`, and lexically sorted
unique `scopes`. `https` is closed and contains exactly lowercase 64-character
`requestSha256`, lowercase 64-character `responseSha256`, and signed 64-bit
integer `resultRevision`. `websocket` is closed and contains exactly lowercase
64-character `snapshotSha256` and `eventSha256`. `mdns` is closed and contains
exactly `activeSpki`, equal to `tls.lanSpkiSha256`. `reset` is closed and
contains exactly `deviceId`, UUIDv7 `replacementRuntimeRegistrationId`,
lowercase 64-character `replacementCertificateSha256`, UUIDv7
`publisherGeneration`, `authorityOperationId`, `authorityGeneration`, and
`authorityRegistrationId`, 43-character unpadded base64url `oldIssuerKid` and
`newIssuerKid`, 43-character unpadded base64url `oldCaSpkiSha256` and
`newCaSpkiSha256`, lowercase 64-character `oldCaCertificateSha256` and
`newCaCertificateSha256`, and `controllerCaUriSan`. The verifier requires
`reset.deviceId` equal `mtls.deviceId`, old and new issuer kids unequal, old
and new CA SPKI hashes unequal, old and new CA certificate hashes unequal, and
`controllerCaUriSan` equal `urn:ff:device:` + `reset.deviceId` +
`:controller-ca:` + `reset.authorityGeneration`. The verifier rejects a
missing, extra, malformed, or unequal field before resolving any assertion. It
also derives and requires `tls.deviceUriSan` equal to
`urn:ff:device:` concatenated with `mtls.deviceId` and `tls.dnsSan` equal to
`mtls.deviceId` concatenated with `.local`; fixtures cannot select a different
self-consistent device identity.

| Test | Exact required assertion tuples |
|---|---|
| `image.boot_measurement` | `inventory_signature_valid / equals / boolean / true`; `inventory_claims_valid / equals / boolean / true`; `ek_manufacturer_chain_valid / equals / boolean / true`; `ak_binding_valid / equals / boolean / true`; `qualifying_data / equals / string / $derived.qualifyingData`; `pcr_bank / equals / string / "sha256"`; `pcr_selection / set_equals / integer[] / $policy.pcrSelection`; `event_log_replay_valid / equals / boolean / true`; `pcr15_image_sha256 / equals / string / $subject.imageSha256` |
| `listener.ipv4.public_443` | `address_family / equals / string / "ipv4"`; `public_port / equals / integer / 443`; `proxy_destination / equals / string / "127.0.0.1:8443"`; `end_to_end_tls_succeeded / equals / boolean / true` |
| `listener.ipv6.public_443` | `address_family / equals / string / "ipv6"`; `public_port / equals / integer / 443`; `proxy_destination / equals / string / "127.0.0.1:8443"`; `end_to_end_tls_succeeded / equals / boolean / true` |
| `tls.version_1_3` | `negotiated_version / equals / string / "TLSv1.3"`; `tls12_rejected / equals / boolean / true`; `tls11_rejected / equals / boolean / true` |
| `tls.server_identity` | `certificate_verify_valid / equals / boolean / true`; `configured_chain_valid / equals / boolean / true`; `subject_empty / equals / boolean / true`; `device_id / equals / string / $fixture.mtls.deviceId`; `uri_san / equals / string / $fixture.tls.deviceUriSan`; `dns_san / equals / string / $fixture.tls.dnsSan`; `san_count / equals / integer / 2`; `basic_constraints_ca / equals / boolean / false`; `key_usage / set_equals / string[] / ["digitalSignature"]`; `extended_key_usage / set_equals / string[] / ["clientAuth","serverAuth"]`; `mqtt_leaf_certificate_sha256 / equals / string / $fixture.tls.leafCertificateSha256`; `lan_leaf_certificate_sha256 / equals / string / $fixture.tls.leafCertificateSha256`; `tpm_private_key_proof_valid / equals / boolean / true`; `qr_pin / equals / string / $fixture.tls.lanSpkiSha256`; `leaf_spki / equals / string / $fixture.tls.lanSpkiSha256`; `mdns_fp / equals / string / $fixture.tls.lanSpkiSha256`; `renewal_spki_unchanged / equals / boolean / true`; `renewal_profile_valid / equals / boolean / true` |
| `tls.server_identity_negative` | `invalid_certificate_verify_rejected_pre_application / equals / boolean / true`; `invalid_configured_chain_rejected_pre_application / equals / boolean / true`; `wrong_spki_rejected_pre_application / equals / boolean / true`; `wrong_hostname_rejected_pre_application / equals / boolean / true`; `wrong_uri_san_rejected_pre_application / equals / boolean / true`; `wrong_dns_san_rejected_pre_application / equals / boolean / true`; `wrong_key_usage_rejected_pre_application / equals / boolean / true`; `wrong_extended_key_usage_rejected_pre_application / equals / boolean / true`; `expired_rejected_pre_application / equals / boolean / true`; `not_yet_valid_rejected_pre_application / equals / boolean / true`; `mdns_fp_mismatch_rejected / equals / boolean / true`; `mdns_fp_pin_update_count / equals / integer / 0`; `tls_observed_pin_update_count / equals / integer / 0`; `credential_bytes_sent / equals / integer / 0`; `mqtt_fallback_selected / equals / boolean / true`; `reset_requires_owner_reenrollment / equals / boolean / true`; `replacement_key_requires_owner_reenrollment / equals / boolean / true` |
| `mtls.authorized` | `certificate_chain_valid / equals / boolean / true`; `device_binding / equals / string / $fixture.mtls.deviceId`; `controller_binding / equals / string / $fixture.mtls.controllerId`; `session_binding / equals / string / $fixture.mtls.sessionId`; `lease_valid / equals / boolean / true`; `scope_set / set_equals / string[] / $fixture.mtls.scopes`; `request_succeeded / equals / boolean / true` |
| `mtls.unauthorized` | `absent_certificate_rejected_pre_application / equals / boolean / true`; `revoked_certificate_rejected_pre_application / equals / boolean / true`; `expired_certificate_rejected_pre_application / equals / boolean / true`; `wrong_device_rejected_pre_application / equals / boolean / true`; `out_of_scope_rejected_pre_application / equals / boolean / true` |
| `https.command` | `request_sha256 / equals / string / $fixture.https.requestSha256`; `response_sha256 / equals / string / $fixture.https.responseSha256`; `effect_count / equals / integer / 1`; `result_revision / equals / integer / $fixture.https.resultRevision` |
| `websocket.subscription` | `upgrade_succeeded / equals / boolean / true`; `snapshot_sha256 / equals / string / $fixture.websocket.snapshotSha256`; `event_sha256 / equals / string / $fixture.websocket.eventSha256`; `out_of_scope_rejected / equals / boolean / true` |
| `mdns.readiness_withdrawal` | `absent_before_readiness / equals / boolean / true`; `ready_spki / equals / string / $fixture.mdns.activeSpki`; `absent_after_listener_loss / equals / boolean / true`; `absent_pending_broker_cleanup / equals / boolean / true`; `absent_pending_identity_rotation / equals / boolean / true`; `absent_pending_authority_bootstrap / equals / boolean / true` |
| `privilege.controld_unprivileged` | `uid_is_nonzero / equals / boolean / true`; `effective_cap_net_bind_service / equals / boolean / false`; `permitted_cap_net_bind_service / equals / boolean / false`; `ambient_cap_net_bind_service / equals / boolean / false`; `bounding_cap_net_bind_service / equals / boolean / false`; `listener_address / equals / string / "127.0.0.1:8443"` |
| `privilege.proxy_hardened` | `uid_is_nonzero / equals / boolean / true`; `no_new_privileges / equals / boolean / true`; `effective_capabilities / set_equals / string[] / []`; `permitted_capabilities / set_equals / string[] / []`; `ambient_capabilities / set_equals / string[] / []`; `bounding_capabilities / set_equals / string[] / []`; `proxy_destination / equals / string / "127.0.0.1:8443"` |
| `reset.pending_broker_cleanup` | `state / equals / string / "pending_broker_cleanup"`; `mdns_absent / equals / boolean / true`; `public_listener_rejected / equals / boolean / true`; `owner_enrollment_rejected / equals / boolean / true`; `broker_cleanup_acked / equals / boolean / false` |
| `reset.pending_identity_rotation` | `state / equals / string / "pending_identity_rotation"`; `mdns_absent / equals / boolean / true`; `public_listener_rejected / equals / boolean / true`; `owner_enrollment_rejected / equals / boolean / true`; `old_publisher_fenced / equals / boolean / true`; `old_authorization_fenced / equals / boolean / true`; `new_identity_active / equals / boolean / false` |
| `reset.pending_authority_bootstrap` | `state / equals / string / "pending_authority_bootstrap"`; `device_id / equals / string / $fixture.reset.deviceId`; `runtime_registration_id / equals / string / $fixture.reset.replacementRuntimeRegistrationId`; `runtime_certificate_sha256 / equals / string / $fixture.reset.replacementCertificateSha256`; `publisher_generation / equals / string / $fixture.reset.publisherGeneration`; `authority_operation_id / equals / string / $fixture.reset.authorityOperationId`; `authority_generation / equals / string / $fixture.reset.authorityGeneration`; `candidate_issuer_kid / equals / string / $fixture.reset.newIssuerKid`; `candidate_ca_spki_sha256 / equals / string / $fixture.reset.newCaSpkiSha256`; `candidate_ca_certificate_sha256 / equals / string / $fixture.reset.newCaCertificateSha256`; `candidate_ca_subject_empty / equals / boolean / true`; `candidate_ca_uri_san / equals / string / $fixture.reset.controllerCaUriSan`; `candidate_ca_san_count / equals / integer / 1`; `candidate_ca_san_critical / equals / boolean / true`; `candidate_ca_self_signature_valid / equals / boolean / true`; `candidate_ca_signature_algorithm / equals / string / "ecdsa-with-SHA256"`; `candidate_ca_basic_constraints_ca / equals / boolean / true`; `candidate_ca_basic_constraints_critical / equals / boolean / true`; `candidate_ca_path_length / equals / integer / 0`; `candidate_ca_key_usage / set_equals / string[] / ["cRLSign","keyCertSign"]`; `candidate_ca_key_usage_critical / equals / boolean / true`; `candidate_ca_extended_key_usage / set_equals / string[] / []`; `identity_acked / equals / boolean / true`; `new_identity_active / equals / boolean / true`; `authority_registration_acked / equals / boolean / false`; `issuer_tpm_proof_valid / equals / boolean / true`; `ca_tpm_proof_spki_sha256 / equals / string / $fixture.reset.newCaSpkiSha256`; `ca_tpm_proof_valid / equals / boolean / true`; `old_issuer_rejected / equals / boolean / true`; `reset_completion_attempted / equals / boolean / true`; `reset_completion_rejected / equals / boolean / true`; `owner_invitation_display_attempted / equals / boolean / true`; `owner_invitation_display_rejected / equals / boolean / true`; `owner_claim_attempted / equals / boolean / true`; `owner_claim_rejected / equals / boolean / true`; `controller_credential_issue_attempted / equals / boolean / true`; `controller_credential_issue_rejected / equals / boolean / true`; `access_session_issue_attempted / equals / boolean / true`; `access_session_issue_rejected / equals / boolean / true`; `lan_certificate_issue_attempted / equals / boolean / true`; `lan_certificate_issue_rejected / equals / boolean / true`; `publisher_reconnect_attempted / equals / boolean / true`; `publisher_reconnect_rejected / equals / boolean / true`; `status_projection_closed_schema / equals / boolean / true`; `registry_retryable_failure_projection_valid / equals / boolean / true`; `registry_conflict_projection_valid / equals / boolean / true`; `registry_terminal_failure_projection_valid / equals / boolean / true`; `tpm_failure_projection_valid / equals / boolean / true`; `operation_ids_unchanged_after_failure / equals / boolean / true`; `mdns_absent / equals / boolean / true`; `public_listener_rejected / equals / boolean / true` |
| `reset.authority_bootstrap_idempotency` | `crash_after_candidate_seal_recovered / equals / boolean / true`; `lost_registration_ack_recovered / equals / boolean / true`; `crash_after_registration_ack_before_activation_recovered / equals / boolean / true`; `tpm_activation_failure_remained_pending / equals / boolean / true`; `crash_after_activation_before_completion_projection_recovered / equals / boolean / true`; `offline_softap_network_restore_same_operation / equals / boolean / true`; `authority_operation_id / equals / string / $fixture.reset.authorityOperationId`; `authority_generation / equals / string / $fixture.reset.authorityGeneration`; `issuer_kid / equals / string / $fixture.reset.newIssuerKid`; `ca_spki_sha256 / equals / string / $fixture.reset.newCaSpkiSha256`; `ca_certificate_sha256 / equals / string / $fixture.reset.newCaCertificateSha256`; `authority_registration_id / equals / string / $fixture.reset.authorityRegistrationId`; `registration_ack_byte_equivalent / equals / boolean / true`; `conflicting_replay_rejected / equals / boolean / true`; `acked_pending_state_observed / equals / boolean / true`; `candidate_issuance_disabled_after_ack / equals / boolean / true`; `active_authority_registrations / equals / integer / 1`; `old_issuer_rejected / equals / boolean / true` |
| `reset.completed_authority_bootstrap` | `state / equals / string / "completed"`; `device_id / equals / string / $fixture.reset.deviceId`; `runtime_registration_id / equals / string / $fixture.reset.replacementRuntimeRegistrationId`; `runtime_certificate_sha256 / equals / string / $fixture.reset.replacementCertificateSha256`; `publisher_generation / equals / string / $fixture.reset.publisherGeneration`; `authority_operation_id / equals / string / $fixture.reset.authorityOperationId`; `authority_generation / equals / string / $fixture.reset.authorityGeneration`; `authority_registration_id / equals / string / $fixture.reset.authorityRegistrationId`; `active_issuer_kid / equals / string / $fixture.reset.newIssuerKid`; `active_ca_spki_sha256 / equals / string / $fixture.reset.newCaSpkiSha256`; `active_ca_certificate_sha256 / equals / string / $fixture.reset.newCaCertificateSha256`; `cleanup_acked / equals / boolean / true`; `identity_acked / equals / boolean / true`; `authority_registration_acked / equals / boolean / true`; `local_authority_activation_acked / equals / boolean / true`; `old_identity_rejected / equals / boolean / true`; `old_issuer_kid / equals / string / $fixture.reset.oldIssuerKid`; `old_issuer_rejected / equals / boolean / true`; `old_ca_spki_sha256 / equals / string / $fixture.reset.oldCaSpkiSha256`; `old_ca_certificate_sha256 / equals / string / $fixture.reset.oldCaCertificateSha256`; `old_ca_rejected / equals / boolean / true`; `active_authority_registrations / equals / integer / 1`; `active_local_issuers / equals / integer / 1`; `active_local_controller_cas / equals / integer / 1`; `publisher_reconnect_after_authority_activation / equals / boolean / true`; `owner_invitation_after_authority_activation / equals / boolean / true`; `authorized_mtls_before_mdns / equals / boolean / true` |
| `brokerless.same_boot_control_push` | `broker_route_unreachable / equals / boolean / true`; `trusted_ntp_in_current_boot / equals / boolean / true`; `https_command_succeeded / equals / boolean / true`; `websocket_snapshot_succeeded / equals / boolean / true`; `websocket_event_succeeded / equals / boolean / true`; `lease_expiry_rejected / equals / boolean / true` |
| `power_loss.rtc_advancement_nonrollback` | `power_removed / equals / boolean / true`; `rtc_advance_error_ms_nonnegative / greater_than_or_equal / integer / 0`; `rtc_advance_error_ms_maximum / less_than_or_equal / integer / 5000`; `authenticated_rollback_rejected / equals / boolean / true`; `expiry_clock_rollback_ms / equals / integer / 0` |
| `power_loss.pre_ntp_control_push` | `ntp_route_unreachable / equals / boolean / true`; `mtls_succeeded_before_ntp / equals / boolean / true`; `https_command_succeeded / equals / boolean / true`; `websocket_snapshot_succeeded / equals / boolean / true`; `websocket_event_succeeded / equals / boolean / true` |
| `power_loss.certificate_expiry` | `ntp_route_unreachable / equals / boolean / true`; `pre_expiry_mtls_succeeded / equals / boolean / true`; `post_expiry_mtls_rejected / equals / boolean / true`; `post_expiry_application_requests / equals / integer / 0` |
| `power_loss.authorization_lease_expiry` | `ntp_route_unreachable / equals / boolean / true`; `pre_expiry_http_succeeded / equals / boolean / true`; `pre_expiry_websocket_succeeded / equals / boolean / true`; `post_expiry_http_rejected / equals / boolean / true`; `post_expiry_websocket_rejected / equals / boolean / true` |
| `power_loss.pre_ntp_fail_closed` | `ntp_route_unreachable / equals / boolean / true`; `mtls_rejected_pre_application / equals / boolean / true`; `http_application_requests / equals / integer / 0`; `websocket_application_requests / equals / integer / 0`; `recovery_softap_available / equals / boolean / true` |
| `power_loss.ntp_recovery` | `closed_before_authenticated_ntp / equals / boolean / true`; `authenticated_ntp_succeeded / equals / boolean / true`; `https_command_succeeded / equals / boolean / true`; `websocket_snapshot_succeeded / equals / boolean / true`; `websocket_event_succeeded / equals / boolean / true`; `new_qr_claim_count / equals / integer / 0` |

Endpoint values, certificate fingerprints, process credentials, capability
sets, clock samples, reset projections, packet results, and fixture digests are
recorded as assertion observations plus redacted raw evidence. The evidence
must be sufficient to reproduce the pass decision without credentials,
controller private keys, device serial numbers, or unredacted input data.

#### Release-ledger linkage and verifier behavior

The implementation release adds exactly one machine-readable comment inside
its `RELEASES.md` version section, after the existing human-readable full-image
declaration:

```text
<!-- LAN_443_FULL_IMAGE: {"version":"2.0.0","attestationUri":"https://conformance.example.invalid/sha256/56789abcdef0123456789abcdef0123456789abcdef0123456789abcdef01234","attestationSha256":"56789abcdef0123456789abcdef0123456789abcdef0123456789abcdef01234","imageSha256":"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef","ffosCommit":"0123456789abcdef0123456789abcdef01234567","ffosUserRef":"89abcdef0123456789abcdef0123456789abcdef","workflowRunId":"1234567890","workflowRunAttempt":"1","lanOfflineAfterPowerLoss":false} -->
```

The JSON object is closed; the verifier rejects missing, duplicate, or unknown
fields and requires exactly one newly added marker for the release. It then:

1. downloads the allowlisted immutable URI without accepting a mutable tag,
   hashes the exact Sigstore-bundle bytes, and compares
   `attestationSha256`;
2. verifies the Sigstore trust root, transparency inclusion, signing time,
   repository, workflow path, protected ref, and signed in-toto payload;
3. requires every ledger value to equal the signed predicate, requires the
   subject and build output to equal `imageSha256`, and requires the version to
   equal the surrounding `RELEASES.md` heading;
4. requires the attested `ffosUserRef` to be the exact source commit used by
   the image build; after that commit, only the evidence-only `RELEASES.md`
   addition may differ in `ffos-user`, otherwise the image is rebuilt and the
   hardware run repeated;
5. verifies the evidence-bundle digest and manifest, the TPM quote's nonce and
   inventory chain, the measured-boot values against the signed measurement
   manifest for the subject image, every required test ID and evidence path,
   the conditional power-loss branch, and `result: "pass"`; and
6. queries the named workflow run and attempt with a read-only GitHub App token
   and requires a completed successful producer job whose immutable build
   outputs match the statement. Missing access, expired evidence, digest or
   field mismatch, an incomplete test set, a failed assertion, or a superseded
   run fails the required check.

The implementation release cannot merge into `staging` or `release`, enable
the public listener, publish mDNS for it, or advertise LAN capability until
this verifier succeeds. The generic full-image rail check remains necessary
but is not evidence that `LAN_443_FULL_IMAGE` passed. This design-only PR adds
none of the future workflow, verifier, watched-path manifest, artifacts, or
release-ledger marker.

1. On one real FF1 and ff-app, provision the TPM device identity, complete one
   owner-enrollment QR claim, request an enrolled-controller access session,
   and connect to managed EMQX using MQTT 5 over WSS on port 443. Prove device
   mTLS, invitation/enrollment User Name and Password authentication, and
   access-session MQTT Enhanced Authentication using `FF1-JWT-ES256-PoP`, a
   broker challenge, RFC 7800 `cnf.jwk`, and an ES256 controller proof. Also
   prove per-principal ACLs, retained messages, the Will Message, Message
   Expiry, and packet limits. An unproven vendor-supported WSS/443 front door
   fails the spike and requires a different managed offering/vendor.
2. On that connection, publish retained capabilities/current state and retained
   connected presence, then prove the broker publishes the retained
   disconnected Will Message after forced connection loss.
3. Execute `diagnostics.ping` as the read-only fixture and at least one declared
   side-effecting command, observing its new state revision. Record correlated
   typed success and typed failure responses for both valid and invalid cases.
4. Interrupt and recover separately from Wi-Fi loss, broker disconnect, device
   reboot, and ff-app restart. After each, resubscribe, recover retained state,
   pass the epoch/revision/event-watermark rules, and prove no QR or user-facing
   authentication prompt occurs. A `feral-controld` restart invalidates open
   invitations but restores TPM-sealed, unexpired access sessions with their
   original IDs, pending-revocation markers, and absolute expiries. The
   `pending_broker_cleanup`, `pending_identity_rotation`, and
   `pending_authority_bootstrap` lifecycles are the exceptions: restore no
   controller authorization and run only the matching cleanup recovery phase.
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
   authorization. Prove an offline screen reset remains
   `pending_broker_cleanup`, blocks owner enrollment, and cannot report
   completion before the broker-barrier, identity-registry, and authority-
   registration ACKs plus durable local authority activation/cleanup. Verify pre-
   erasure deletes the old runtime certificate, issuer/CA, enrollments,
   invitations, sessions, cached credentials, user/network data, and event
   outbox while preserving only the stable TPM device-identity key and signed
   cleanup tombstone. Reconfigure network through a fresh recovery-SoftAP setup
   authorization. On recovery, prove the barrier first fences the old publisher
   generation and cancels its pending Will Message/queued publishes, then
   purges its retained Application Messages before ACK. Prove that ACK changes
   status to `pending_identity_rotation`, not `completed`; the idempotent
   identity transaction revokes the old runtime-certificate registration,
   activates a replacement certificate with a fresh serial/registration and
   publisher generation, and transitions to `pending_authority_bootstrap`, not
   `completed`. Prove the authority transaction seals one fresh TPM issuer/CA
   candidate under a durable operation and generation, binds its issuer `kid`
   to the exact replacement runtime identity and publisher generation, receives
   an idempotent registry ACK, and atomically activates that issuer and CA
   locally before public reconnect or owner claim.
   Run this broker-loss case after NTP in the current boot for every device.
   Then power-cycle with NTP still blocked. A device advertising
   `lanOfflineAfterPowerLoss: true` MUST immediately repeat the authenticated
   LAN command/push/expiry cases from trusted RTC time. A device advertising
   `false` MUST fail runtime mTLS before HTTP or WebSocket processing, keep
   recovery SoftAP available, and restore LAN only after NTP succeeds.
10. Verify the actual FF1 TPM supports the selected P-256 key lifecycle,
   attestation evidence, key renewal, secure deletion, and factory reset
   behavior, including idempotent creation and sealing of a fresh issuer/CA
   candidate pair under one durable authority operation. Separately test
   whether the shipping hardware and full image have
   a trusted RTC that persists, advances, rejects rollback, and enforces
   certificate and LAN-lease expiry across power loss. Advertise
   `lanOfflineAfterPowerLoss: true` only when that executable evidence passes;
   otherwise advertise `false` and pass the required fail-closed case in gate
   9. Distinguish the reset-stable hardware device-identity/PoP key from its
   reset-scoped runtime X.509 certificate and broker registration.
11. Register the TPM-backed FF1 controller-issuer public key with the broker,
   validate FF1-signed invitation, enrollment-only, and access-session
   credentials without a per-session control-plane lookup, validate the access
   credential's `cnf.jwk` and `FF1-JWT-ES256-PoP` challenge response, reject a
   wrong key, binding mismatch, expired challenge, replayed challenge, and
   replayed proof with the specified MQTT Reason Codes. Then prove the required
   broker authorization adapter atomically installs durable target deny
   tombstones, applies them before every authorization/delivery decision,
   removes subscriptions and queued delivery, disconnects active clients with
   `0x87`, and acknowledges only after that barrier. Before ACK, prove FF1
   rejects the target but exposes no terminal revocation state, event, or
   success result; after ACK, prove the target receives no newly authorized,
   queued, or retained state/events and cannot reconnect. Prove revoking the
   authenticated caller controller or current access session returns
   `interaction_not_allowed` without creating a barrier. For online reset,
   prove the final reset-starting PUBLISH receives PUBACK before heartbeat/state
   quiescence, event-outbox discard, and normal public-device DISCONNECT, and
   prove the separately authenticated management barrier runs only afterward.
   After barrier ACK, prove `pending_identity_rotation` resumes the same durable
   `identityOperationId` across lost responses and crashes and atomically
   switches the runtime certificate registration and publisher generation.
   Prove the identity ACK transitions to `pending_authority_bootstrap`; repeated
   crashes and lost ACKs then preserve one `authorityOperationId`,
   `authorityGeneration`, issuer `kid`, CA certificate, and authority
   registration, while candidates issue no credential or LAN certificate.
   Reach `completed` only after authority-registration ACK and atomic local
   issuer/CA activation plus cleanup.
12. Run the SoftAP/captive-portal spike on the required SAE-capable phone matrix
   and measure wrong-password recovery, AP/client concurrency, and auto-open
   behavior. Inspect the advertised RSN profile and association behavior: it
   offers only SAE with CCMP-128 and mandatory PMF, rejects WPA2-PSK,
   WPA2/WPA3 transition or compatibility mode, PMF-optional/non-capable
   association, and downgrade attempts, and never starts an open or weaker
   fallback. Verify that a client without SAE cannot join, the screen explains
   that WPA3-Personal is required, setup remains in the current epoch, and FF1
   does not weaken the network. Associate a target client and a second
   participant that knows the session SoftAP passphrase but does not know the
   target's screen setup capability. Give the second participant captures of
   the target's SAE exchange, 4-Way Handshake, and encrypted HTTP frames. Prove
   it cannot derive the target's PMK or pairwise keys, recover `FF-Setup`,
   decrypt or inject target traffic, or replay captured encrypted frames past
   per-station keys and replay counters; also prove client isolation prevents
   direct inter-station traffic. Direct requests with a missing, malformed,
   guessed, wrong-epoch, stale, or expired setup capability all return the same
   401 `unauthenticated` Problem Details response and make no state change. The
   closed-schema fixtures also include all three pending-reset states,
   conditional `barrierOperationId`/`identityOperationId`/
   `authorityOperationId`/`authorityGeneration` fields, the required cleanup
   object and exact phase failures, the completed response only after authority
   registration and local authority activation/cleanup, rejection of the erased
   setup secret, and blocked owner enrollment and issuance in every pending
   phase.
13. Verify the fixed DP-1 trust profile against real Feral File feeds, institution trust
   roots, key rotation/revocation, and expired-trust offline cache fixtures.
14. Define the support service's device-bound upload-grant exchange. The public
   device command remains unchanged and never receives a service API key.

## 4. Implementation sequence

No implementation is part of this contract-definition change. When separately
authorized, the future sequence must preserve cached-art playback and permit
rollback at every fleet gate:

1. Publish the complete JSON Schema 2020-12 bundle, AsyncAPI 3.0 document, and
   OpenAPI 3.1.1 adapter from this draft. Their publication, together with the
   positive and negative fixtures and automated MQTT/LAN parity suite, is the
   gate that makes target version `2.0.0` normative. CI validates every example
   and rejects schema drift between MQTT and HTTPS. Schemas, fixtures,
   generated types, and the minimal reference client are published under the
   repository's Apache-2.0 license without private Feral File transport
   dependencies.
2. Implement protocol types and conformance tests in both repositories without
   changing production transport. Add golden vectors for JCS hashes, UUID
   correlation bytes, expiry, RFC 9457 errors, and every command/state/event.
3. Implement `feral-controld` retained state and idempotent dispatcher behind a
   feature flag. Keep service ownership boundaries unchanged, while versioning
   the internal factory-reset D-Bus contract for prepare/physical-decision/
   execute and adding only the source-of-truth signals required by retained
   state.
4. Complete TPM enrollment and broker spike, including WSS/443, Will Message,
   ACL negative tests, the vendor-neutral authorization-barrier adapter,
   old-device-publisher generation fencing, pending-Will/queued-publish
   cancellation, retained Application Message purge, idempotent runtime-
   certificate registration replacement, fresh publisher-generation activation,
   idempotent controller-authority registration and TPM issuer/CA activation,
   NTP loss, packet limits, rate limiting, clean restart, restored unexpired
   access sessions, and duplicate QoS 1 delivery.
5. Prove the unified controller-authentication profile against the managed
   broker: device-signed JWT validation without a per-session authentication
   service, one-time invitation consumption, access-session broker challenge
   and controller-key proof, JWE delivery, silent enrolled-session and
   enrollment-credential renewal,
   multiple independent controller enrollments, browser-only WebSocket Origin
   defense-in-depth checks for web guests, exact ACLs, expiry, revocation,
   atomic scope-reduction ceiling generations/barriers and their complete
   MQTT/LAN negative matrix, and normal `playlist.display`
   through a guest session. Direct broker validation of the registered FF1
   issuer, including its exact runtime-identity/publisher-generation binding and
   prior-generation denial, is a gate.
6. Add authenticated LAN HTTPS/WebSocket/mDNS with the sole approved port-443
   topology: system-level `ff1-control.socket` and hardened raw
   `systemd-socket-proxyd` forwarding to unprivileged `feral-controld` on
   `127.0.0.1:8443`. Coordinate the component and user/system-unit changes as a
   full-image release, add its `RELEASES.md` declaration only in that release
   PR, build the matching `ffos` image, and pass `LAN_443_FULL_IMAGE` before
   enabling the endpoint. The gate must verify the exact dual-use runtime
   certificate URI/DNS SAN, KU/EKU and TPM proof, stable-SPKI renewal,
   QR/enrollment-bound pin and exact hostname validation, fail-closed
   wrong-SPKI/hostname/profile/time cases, no mDNS/TOFU pin update, and MQTT
   fallback. Then run one shared protocol conformance suite
   against MQTT and LAN. The same command vector must produce the same response
   and final state revision, and both subscription bindings must deliver
   schema-equivalent snapshots/events.
7. Add ff-app v2 transport/protocol/control implementations and legacy routing.
   Exercise one-time owner enrollment, silent enrolled-session and credential
   renewal, a second mobile enrollment, a concurrent CLI enrollment, guest
   invitation and claim, remote Playlist display, brokerless same-boot LAN
   display, capability-conditioned post-power-loss LAN display, sleep,
   update, recovery, keyboard, tap, double tap, long press, drag, wheel/pinch,
   guest expiry and revocation, self-revocation rejection, reconnect, and stale
   presence.
8. Ship the v2-capable app first, then canary firmware. Current firmware records
   only the target-bound pre-OTA eligibility acknowledgment. Boot the v2 image
   once in probation, complete owner enrollment and MQTT promotion checks, and
   promote the slot only after the closed post-boot record validates. A blocked
   LAN records `network_unreachable` and does not prevent MQTT-based ownership;
   failure to complete required promotion checks triggers rollback.
9. For each successfully promoted v2 device image, remove the shared API key,
   old relayer, Mint Pairing Broker, port-1111 hub, `GetRelayerTopicID`,
   JSON-body GET commands, legacy `DP1Intent`/`dynamicQueries`, and BLE together
   after the device migration, rollback, and SoftAP gates pass. A device that
   rolls back, is below the minimum upgrade version, or remains on current v1
   keeps its complete v1 code paths. Keep the hosted relayer and other v1
   infrastructure until the remaining legacy fleet passes a separate
   infrastructure-retirement gate.

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
- cached verified DP-1 playback works with no internet or broker;
  authenticated LAN control works brokerlessly after NTP in the current boot,
  and works before NTP after a power-loss reboot only when the advertised
  `lanOfflineAfterPowerLoss` capability is `true`;
- DP-1 invalid/signature/license/repro/source failures preserve their namespaced
  errors and never render an unverified Playlist;
- pointer/keyboard input is blocked unless the active DP-1 PlaylistItem permits it, is
  never delivered after expiry, and always releases pressed buttons and keys;
- one controller cannot publish, subscribe, request a response, or discover
  sensitive state for another device/controller;
- multiple enrolled mobile apps and CLIs can connect concurrently, renew
  independently, and survive another controller's expiry, rotation, or
  revocation without a QR prompt;
- an owner cannot create a controller invitation with any scope outside its
  current authoritative caller grant ceiling, and reducing that grant before
  claim prevents the stale invitation from enrolling broader authority;
- a `controllers.set-scopes` reduction is locally fail-closed at its durable
  cutoff and succeeds only after the broker scope-reduction barrier ACK;
  selected target/guest MQTT ACLs, subscriptions, queued/retained delivery,
  connections, and reconnect are denied, selected LAN connections/leases are
  terminated or recomputed, over-ceiling target-created guest sessions are
  revoked, every target-created over-ceiling `controller_enrollment` or
  `guest_session` invitation is closed, and every API section 7.5 negative-matrix
  case produces no protected payload or side effect; self-target returns
  `interaction_not_allowed` before a barrier;
- credential renewal with a restricted PKCS #10 `lanCsrPem` returns a
  replacement certificate that matches the new signing key and completes
  mTLS; the previous certificate is accepted only during its ten-minute grace,
  then fails both new and already-open LAN use; CSR/JWK mismatch is atomic
  `invalid_claim`, while omission returns no replacement and leaves the prior
  certificate valid only for the same grace; the negative fixtures also cover
  noncanonical or malformed PEM/DER, forbidden subject/attributes/extensions,
  unsupported encodings, invalid CSR signature, and proof-covered PEM mutation
  without any rotation or revision;
- the immediately previous enrollment credential can create a session only
  inside the same grace, using its matching previous proof/encryption keys,
  the live enrollment grant/status, and an access expiry no later than the
  grace deadline; a new request at or after that deadline receives `expired`
  with no session fields even if the broker still accepts the self-contained
  JWS only on its restricted transport topics before signed expiry;
- after successful revocation barrier ACK, the target cannot reconnect or
  receive newly authorized, queued, or retained state/events; before ACK, no
  terminal revocation projection, event, or success result is exposed;
- controller self-revocation and current-access-session revocation return
  `interaction_not_allowed` without creating a pending marker or barrier;
- offline reset remains `pending_broker_cleanup` and cannot enroll a new owner;
  barrier ACK transitions to `pending_identity_rotation`, not `completed`;
  identity commit atomically revokes the old runtime-certificate registration
  and activates its replacement with a fresh publisher generation, then
  transitions to `pending_authority_bootstrap`, not `completed`; authority
  registration binds one fresh TPM-backed issuer generation to that exact
  runtime identity/publisher generation, and only atomic local issuer/CA
  activation plus cleanup permits completion, public reconnect, or owner claim;
- publisher fencing prevents an old Will Message, reconnect, or queued publish
  from recreating purged state, and completed reset leaves every old issuer,
  certificate registration, credential, retained message, and queued delivery
  unusable or absent;
- authority-bootstrap retry, lost-ACK, and crash fixtures preserve one operation
  ID, authority generation, issuer `kid`, CA certificate, and registry binding;
  candidates issue nothing before commit, the prior issuer stays denied, and
  exactly one issuer and one controller CA are active after completion;
- certificate expiry/rotation and last-owner protection are tested end-to-end;
- every LAN handshake validates TLS 1.3 proof, certificate time and the exact
  runtime-device profile, exact `<deviceId>.local` DNS SAN, and the QR-bound
  leaf SPKI pin; normal runtime-certificate renewal preserves SPKI, while wrong
  SPKI/hostname/profile/time and an untrusted mDNS `fp` fail closed without
  TOFU or pin update and use MQTT fallback; reset or TPM key replacement
  requires physical owner re-enrollment before a new pin is accepted;
- enrolled LAN HTTP and local push expire at the issued authorization-lease
  deadline, bounded to at most 900 seconds, even when the client certificate is
  still valid, followed by a silent mutually authenticated reconnect;
- both required power-loss capability cases pass: `true` proves trusted RTC
  advancement plus immediate pre-NTP LAN mTLS and expiry enforcement on the
  exact full image, while `false` proves same-boot brokerless LAN followed by
  pre-NTP mTLS failure after power loss and recovery only after NTP; no image
  advertises `true` without linked executable hardware evidence;
- SoftAP advertises and accepts only SAE with CCMP-128 and mandatory PMF, never
  starts a WPA2, transition, PMF-optional, or open fallback, and fails closed
  with a clear screen instruction when a client lacks SAE; a second associated
  participant that knows the SoftAP passphrase but not the screen capability
  cannot use a captured target SAE exchange, 4-Way Handshake, and data frames
  to derive target keys, recover `FF-Setup`, decrypt, inject, or replay target
  traffic, and every missing,
  guessed, wrong-epoch, stale, or expired capability request fails identically
  without a state change;
- `LAN_443_FULL_IMAGE` passes: the public 443 socket/proxy path boots from the
  coordinated full image and
  passes IPv4, IPv6, TLS 1.3, mTLS allow/deny, HTTPS, WebSocket,
  readiness-withdrawal, and reset tests without root or
  `CAP_NET_BIND_SERVICE` on `feral-controld`; the required release job verifies
  the Sigstore/in-toto subject, workflow identity, TPM-bound actual-hardware
  evidence, complete test set, image/source digests, and the exact
  `RELEASES.md` marker for the successful `ffos` run and attempt;
- every `state/sessions` invitation and session variant passes closed-schema
  and terminal-removal fixtures; restart fixtures invalidate open invitations
  except unclaimable invitations selected by a pending scope reduction, and
  restore unexpired sessions, pending-revocation markers, and pending
  scope-reduction targets without
  changing their absolute expiry, except any pending-reset lifecycle restores
  no controller authorization and resumes its durable operation ID;
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
| Per-device/controller identity, TPM, rotation, revocation barriers, ACLs | API sections 7 and 8; authentication section 13 |
| MQTT device mTLS; restricted invitation/enrollment credentials; sender-constrained access sessions | API sections 4.1.1 and 7; authentication section 7 |
| Brokerless authenticated LAN HTTPS using the same contract, with explicit power-loss time capability | API sections 5, 7, 8, and 9; authentication sections 12, 13.2, and 15; plan section 3 |
| Authenticated realtime LAN state/event push with polling fallback | API section 5 |
| mDNS discovery and rejection of unenrolled clients | API sections 5, 7, and 8 |
| Least-privilege public TCP 443 and coordinated full-image release | API section 5; architecture and API-direction v2 transition sections; plan gate `LAN_443_FULL_IMAGE` and section 4 |
| DP-1 v1.1.0 Playlist validation, trust, and offline cache | API sections 10, 11.1, and 12.3 |
| Mobile keyboard/touchpad remote control | API section 11.3 |
| Unified enrolled and guest controller access without relayer or per-session auth service | Controller authentication profile; API sections 7.4, 9, 11.7, and 12.7 |
| Persistent one-scan mobile/CLI authorization with multiple independent controllers | Controller authentication sections 4.2, 5.1, 5.3, and 14; API sections 7.2, 8, and 9 |
| Setup/claim recovery boundary, SAE-only SoftAP, and capability isolation | API section 14; plan section 3 gate 12 and minimum acceptance checks |
| Offline/cache readiness | API section 12.5 |
| MoMA delivery/connection diagnosis | API sections 4.2 and 12.5; plan section 2.5 |
| Executable client/firmware matrix and negative OTA gates | Plan section 2.1 |
| Per-device pre-OTA eligibility, post-boot promotion, rollback, and USB recovery | Plan sections 2.1 and 2.5 |
| One-time enrollment transition for verifiable existing owners | Authentication profile and plan section 4 |
| Open schemas, fixtures, reference client, integration-friendly license | Plan section 4 |
| No broker dependency for art already on the wall | API sections 10, 11.1, and 12.5 |
