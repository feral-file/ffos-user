# feral-file#3551 — Brandon's execution plan: endpoint authorization and E2EE

Status: draft, 2026-10-20. Built from the issue thread through Anh's 2026-10-19 09:45 comment, ff-relayer#25, Anh's open relay PRs (ff-relayer#26, ffos-user#407, ff-app#1138, ff1-cli#133, play#23), and a read of ffos-user develop @48ab2ad, ffos develop @c9ca7fa, ff-app main, ff1-cli main @71f6495, ff-relayer main @49251b4 and the play repo.

## 0. The split

- **Mine:** FF1 owns authorization on every route. Per-controller keys and grants, a signed and encrypted envelope, the first-owner and later-controller ceremonies, migration of claimed 2.0.x devices, and the client side in ff-app and ff1-cli. All of it ships and is tested over the **existing** relayer and LAN.
- **Anh's:** the opaque v2 relay and the merged pairing broker (ff-relayer#25 / #26).
- **Shared:** the interface (§2). It is frozen early, with one thin end-to-end integration, and both parts aim for one coordinated FF1/app/CLI/browser release. If #25 slips, my part ships on the legacy relayer, and moving to v2 later needs no new keys and no re-pairing.

Out of scope for me: relay framing, broker storage, Play browser grants (§2.7 is a joint boundary), topic rotation, account move.

## 1. Facts the plan rests on (from code)

| # | Fact | Where |
|---|---|---|
| F1 | Every route (LAN cast, relayer RPC, browser session, OOM recovery) collapses into one `commands.Command{Type,Arguments,RawArguments}`. Nothing records where it came from. | ffos-user `commands/types.go`, `mediator.go:840-845`, `hub.go:296` |
| F2 | The hub has a reserved seam: `hub/middleware.go:141`. The relayer has none; `mediator.handleRelayerMessage` is the natural one. | |
| F3 | Unknown command names fall through to the player over CDP. | `commandrouter/handler.go:647`, docs/controld-inbound-controller-messages.md:1459 |
| F4 | Gate dedupe keys on type+args, so an identical in-flight command from LAN and relayer shares one result. | `commandrouter/gate.go:30-33` |
| F5 | `connect` overwrites the claim with no proof. `mintPairingApprovalDecision` is unauthenticated on both transports. | `executor.go:677-733`, `mintpairing/service.go:1427` |
| F6 | The LAN notification WS (`/api/notification`) is unauthenticated and pushes device and player status. | `hub.go:466`, `status/status.go:598-645` |
| F7 | Legacy relayer: a topic holder can cast anything, attach as a fake portal (**first reply wins**), and read plaintext notification history. No body-size limit. 5 s RPC timeout. | ff-relayer `PortalRelayer.ts:320,458-460,2386-2428,2431-2487` |
| F8 | A 4xx on the relayer handshake is `PermanentError`. Old firmware cannot be moved off the legacy relayer. | `relayer.go:280-343,940` |
| F9 | `.state` survives a full-image OTA **as the snapshot taken when the OTA began**. Writes during download and install are lost on promotion. `feral-tailscale` takes `/run/feral-updater.lock` for exactly this reason. | ffos `feral-system-update.sh:128,360-362`, `feral-tailscale:101-118` |
| F10 | Factory reset boots `@recovery_candidate` / `@factory_reset` (typically 2.0.0 or 2.0.1), so `.state` comes from an old image and has no grants. The next OTA carries it forward. The update decision is inequality-based, so a downgrade hands new-format `.state` to old code. | ffos `factory_reset.sh:70-76`, `feral-updater.sh:89` |
| F11 | `controld.json` is replaced by every full-image OTA. controld has **no remote config**. Version knobs are only in the distributor manifest. | ffos build workflows; `otagate/version.go` |
| F12 | `feralfile` has NOPASSWD sudo. Nothing uses the TPM at runtime. Secure Boot is off. Software device keys lose nothing real. | `sudoers.d/feralfile`, `auto-install.sh:288-302` |
| F13 | The app is LAN-first and treats **any** hub HTTP error as "reached, never relay". | ff-app `ff1_lan_rest_client.dart:14-21,577-649` |
| F14 | The app reads `topic_id` only from `/api/v2/status` and the claim QR. Any paired phone can put the claim QR on screen ("Show Pairing QR Code"), so a second phone can join without status serving the topic. | `ff1_lan_bind_provider.dart:368-497`, `device_config_screen.dart:1749` |
| F15 | The app has no JOSE library. pointycastle already provides P-256, ECDH, HKDF, AES-GCM, JWK and canonical JSON. Secure storage options are pinned (`unlocked_this_device`, `resetOnError:false`). | `ff1_cli_topic_handoff_service.dart:95-401`, `dp1_publish_identity.dart:51-85` |
| F16 | ff-cli runs on macOS, Linux and Windows (CI matrix). Its topic lives in the OS keyring. It sends exactly three device commands: `displayPlaylist` (LAN→relayer), `getDeviceStatus` and `sshAccess` (both LAN-only). The handoff is one-way today, but the broker allows minter→browser messages. | ff1-cli `ci.yml`, `ff1-device.ts`, `ssh-access.ts`, `handoff-client.ts` |
| F17 | v2 relay: binary frame `u16 len | JSON hdr | bytes`, ≤64 KiB after the relay adds `from`, endpoint chunks ≤60 KiB. ACK means transport receipt only. No ping. Up to 4 player candidates per id. Request IDs, replay and chunking live inside the ciphertext. | ff-relayer#26 `src/protocol/relay.ts`, docs/transport-v2.md |
| F18 | Go has no JOSE library in go.mod. ff1-cli uses `node:crypto` only. | |

## 2. Interface to agree with Anh (milestone M0, before device code)

Published as a versioned spec plus **cross-language fixtures**, which ff-relayer#26 migration step 1 asks for. Proposed home: `ffos-user/docs/protocol/ff1-control-v1.md` + `ffos-user/protocol/fixtures/ff1-control-v1/*.json`, consumed by the Go, Dart and TS tests.

### 2.1 Keys and identity
- **Device:** two software P-256 keys, `devSig` (ES256) and `devEnc` (ECDH). Also a random 128-bit `gen` (control generation) created with them.
  - Public **profile:** `{v:1, deviceId, gen, sig: JWK, enc: JWK}`.
  - **Pins:** RFC 7638 thumbprints of `sig` and `enc`.
  - `deviceId` stays the hostname `FF1-XXXXXXXX`, the same value v2 uses as `playerId`.
- **Controller:** `ctlSig` and `ctlEnc`, P-256 software keys. `controllerId` = RFC 7638 thumbprint of `ctlSig`.
  - Separate encryption key so the device can push to it. This matches the ffos-user v2 auth doc §4.2 and avoids reusing a signing key for ECDH.

### 2.2 Request (controller → device)
Compact JWE: `alg=ECDH-ES`, `enc=A256GCM`, `kid=thumb(devEnc)`, `typ=ff1-req+jwe`. The payload is a compact JWS: `alg=ES256`, `kid=controllerId`, `typ=ff1-req+jws`. Claims:

```
{ v:1, dev, gen, ctl, rid (uuid), ctr (uint64, per controller, increasing),
  iat, exp (≤ iat+120s), op: "cmd"|"enroll"|"requestAccess",
  cmd?, req? (today's request object, unchanged), rk (32-byte reply key, b64url),
  enroll?: {inv?, sigJwk, encJwk, label, platform} }
```

### 2.3 Reply (device → controller)
Compact JWE `alg=dir`, `enc=A256GCM` under `rk`. The payload is an ES256 JWS by `devSig` over:

```
{ v:1, dev, gen, ctl, rid, ok, code?, res? }
```

The client verifies against its pinned `devSig`. This also neutralizes F7's fake-portal "first reply wins": a forged reply fails verification and costs only availability.

### 2.4 Private notifications (device → controller)
- Enrollment hands each controller a random 32-byte `nk`.
- A notification is a JWE `dir/A256GCM` under `nk`, with protected header `{typ:ff1-ntf+jwe, ctl, seq}`, over `{v, dev, gen, ctl, seq, type, body}`.
- The device emits one per live grant (≤32) on both the LAN WS and the relayer notification channel, as `notification_type:"sealed"` with `persist_record_count` low.
- Revocation simply stops emission for that controller. No WS authentication is needed, because the socket carries ciphertext only.
- Cost is one AES-GCM per controller, not ECDH.

### 2.5 Wire carriage
| Route | Request | Reply |
|---|---|---|
| LAN | `POST /api/cast` body `{"command":"ff1Envelope","request":{"v":1,"jwe":"…"}}` | 200 `{"v":1,"jwe":"…"}`; public errors 401/403 `{"error":{"code":"…"}}` |
| Legacy relayer | same JSON as a topic cast (relayer forwards opaque, F7) | RPC `{message:{v:1,jwe}}` |
| v2 relay (Anh) | frame payload = UTF-8 bytes of the compact JWE; controller may fan out to `players` and must accept only a reply that verifies; pin the connectionId after the first valid reply | same |
| VPN / Tailscale | identical to LAN | |

Envelopes over ~60 KiB need endpoint chunking on v2. To agree with Anh: either chunk the compact string with an outer `{xfer, i, n}` header, or require large `dp1_call` payloads to go by URL. My proposal is outer chunking: every chunk is ciphertext and the reassembled JWE is authenticated.

### 2.6 Replay and freshness
- Per-controller sliding window: high-water `ctr` plus a 64-bit bitmap, so out-of-order LAN and relay delivery is tolerated.
- Durably persisted **before execution** (fsync + rename + dir fsync, the `contentpolicy.persistAtomic` pattern).
- `exp` is enforced against device time, with a skew allowance.
- A lost reply is never auto-replayed with the same `ctr`. The router's existing "duplicateSafe only" rule (F13) decides whether a fresh request may be sent.

### 2.7 Browser grants (joint boundary, decide with Anh)
- Through phase 2: Play sessions keep minting through the legacy relayer `/api/ephemeral-sessions`, and stay displayPlaylist-only, enforced by the relayer.
- My part makes the **approval** that triggers a mint owner-only (an envelope from an owner grant).
- On v2, the browser becomes a device-issued controller grant with a display-only role, using the same envelope. That design belongs with #25, but it must reuse §2.1-2.6 unchanged.

## 3. Device states and command tiers

### 3.1 States
Reported in `/api/v2/status` (`control:{v:1, state, gen, pins}`), in `device_status`, and in a boot log line (fleet count via log streaming).

- **legacy:** new firmware, no grants. Plaintext accepted as today, except for the phase-0 containment rules. The invitation QR is offered.
- **enrolled:** at least one owner grant.
  - LAN accepts envelopes only (plaintext → 401 `enrollment_required`).
  - Relayer plaintext is limited to the **controller tier**, so off-LAN old apps keep playback.
  - Owner-tier commands require an envelope on every route.
- **enforced:** switched on by a `controld.json` flag in the floor release (F11).
  - Relayer plaintext is limited to the exact `displayPlaylist` shape (Play sessions).

### 3.2 Command tiers
Proposed; Sean confirms. Unknown names fail closed for non-owners, and the check sits **before** the CDP fallthrough (F3).

- **Owner:** `sshAccess`, `factoryReset`, `startWifiSetup`, `updateToLatestVersion`, `connect`, `setDeviceName`, `showPairingQRCode`, `start/join/closeMintPairingSession`, `mintPairingApprovalDecision`, `setContentPolicy`, `analyticsToggle`, `betaFeaturesToggle`, `uploadLogs`, keyboard and gesture input, plus the new `approveController`, `revokeController` and `listControllers`.
- **Controller:**
  - playback, offline cache, recently played;
  - `getDeviceStatus`, `deviceMetrics`, `checkStatus`;
  - `rotate`, `setVolume`, `toggleMute`, `ddcPanelControl`, `ddcPanelStatus`;
  - sleep schedule, sleep and wake, `reboot`, `shutdown`, `runNetworkDiagnostics`, `getContentPolicy`.

## 4. Ceremonies

1. **First owner (new or reset device).**
   - The unowned device shows the claim QR with extra pipe segments: `…|<phase>|<inv>|<pinSig>|<pinEnc>|<gen>`. The old app's parser reads only indices 0, 1 and 3 and ignores the rest (`ff1_device_info.dart:16-30`), so it keeps working.
   - The topic stays in the QR while the device is unowned, so old apps can still claim.
   - The new app pins the profile from `/api/v2/status` against the QR pins and sends `op:enroll` with `inv`.
   - The device consumes `inv` (10-minute lifetime, single use, constant-time compare), writes an owner grant, and returns `{role, nk}`. It clears the QR and moves to `enrolled`.
2. **Existing claimed 2.0.x owner (migration).**
   - The new app sees `control.state=legacy` on a claimed device and shows "Secure this FF1".
   - It sends `showPairingQRCode{show:true, purpose:"enroll"}`; the device displays the invitation. Scan, then enroll as in step 1.
   - **Possession of the topic never mints a grant.**
3. **Later phone.**
   - It sends `op:requestAccess` (LAN only in phase 1) with its keys and label. The device holds it as pending: ≤4 pending, 5-minute TTL, rate-limited.
   - The device pushes a sealed `controller_approval_request` to every owner.
   - The owner sees a sheet copied from the mint-pairing prompt layer, showing a 6-digit code derived from `controllerId`. The requester shows the same code.
   - The owner approves with an `approveController{ctl, role, expiresAt?}` envelope. The requester re-sends `enroll` and receives `{role, nk}`.
4. **ff-cli.** Extend the existing "Pair with ff-cli" handoff:
   - app → CLI: `{type:'ff1_controller_handoff', v:2, deviceId, profile, topicId (legacy routing)}`;
   - CLI → app (new direction, which the broker already supports): `{sigJwk, encJwk, label}`;
   - the app shows a code derived from `controllerId` that the CLI also prints;
   - owner approves, and the CLI enrolls.
   - v1 payloads are still accepted for old firmware.
5. **Revoke / list.** The owner app lists grants with revoke. The device drops the grant and keeps `ctr` (so re-approval cannot replay). Sealed notifications stop.
6. **Lost sole owner phone.** Factory reset, either from the app on another owner device or by the power-cycle gesture. Sean confirms.

## 5. Phases and PRs (all against `develop` / `main`; no dispatch by me)

### Phase 0 — containment
One FF OS release (controld package rail) plus an ff-cli patch; no app change.

- **P0-1 ffos-user, hub middleware at the seam (F2):**
  - On a claimed device, LAN `sshAccess` requires the header `X-FF1-Topic` equal to the current topic (constant-time compare).
  - On a claimed device, LAN `connect` requires the same header.
  - First claim of an unclaimed device stays open.
  - `/api/status` and `/api/v2/status` omit `topic_id` once claimed. The second phone uses "Show Pairing QR Code" (F14).
  - Audit every LAN-served payload for the topic: status, `getDeviceStatus` reply, notifications, `/metrics`, logs, mDNS.
  - Update the `hub/status.go:132` rationale comment.
- **P0-2 ff1-cli:** `ssh enable|disable` sends `X-FF1-Topic` from the keyring. A clear error on 401 tells the user to re-pair.
- **Residual, documented:** reset, Wi-Fi setup and input stay open on LAN until phase 1. A sole-phone owner who loses that phone re-pairs only via reset.
- **Check:** from a laptop on the Wi-Fi, `curl …/api/v2/status` has no `topic_id`, and `sshAccess` without the header gets 401. App pairing a second phone via the QR still works.

### Phase 1 — protocol
One release train: FF OS, ff-app, ff-cli. PRs merge independently behind `control.enabled` (controld.json, default off on develop until the train).

- **M0 interface (with Anh):**
  - spec + fixtures (§2), covering positive vectors and negatives: tampered, wrong `gen`, wrong `dev`, replayed `ctr`, expired, wrong `kid`, oversize;
  - Go reference implementation;
  - thin E2E early: the app-simulator envelope over the legacy relayer and over Anh's v2 relay to a dev controld on FF1-8EVTK3RE.
- **M1 ffos-user, `components/feral-controld/control/` package:**
  - keys and profile in `/home/feralfile/.state/control/` (0600);
  - grant store with role, label, created/expires and replay window, using the atomic+fsync write;
  - corrupt store → fail closed, `state=locked`, never "unowned";
  - envelope codec on `github.com/go-jose/go-jose/v4` (ECDH-ES, A256GCM, dir, ES256), or a stdlib implementation if review prefers zero dependencies; the fixtures decide either way;
  - unit and race tests.
- **M2 ffos-user, admission:**
  - the hub middleware and `mediator.handleRelayerMessage` both route `ff1Envelope` to `control.Admit`, which returns `(Command, Principal)`;
  - `Principal{controllerId, role, route}` is threaded through `Process` via ctx;
  - tier check before CDP fallthrough;
  - dedupe key includes `controllerId` (F4);
  - state-based plaintext policy per route (§3.1);
  - `connect`, `mintPairingApprovalDecision` and the mint session commands become owner-tier (F5);
  - OOM recovery keeps bypassing as internal.
- **M3 ffos-user, ceremonies:**
  - invitation QR (executor `formatDeviceConnectURL`, overlay);
  - `enroll`, `requestAccess`, `approve`, `revoke`, `list`;
  - sealed notifications on both channels;
  - factory reset retires authority before starting the reset unit, next to the existing claim clearing (`executor.go:2983-3060`);
  - `control` block in status and `device_status`.
- **M4 ffos, OTA durability (F9):**
  - `feral-system-update.sh` stops the controld user unit and rsyncs live `.state/control/` into `@ota_new` right before `bootctl set-oneshot`, under the updater lock;
  - test with `scripts/test-*` style plus a bench OTA.
  - This needs the full image. The first update that carries it still runs under the old script, which is harmless because old firmware has no grants.
  - Check that the PR #170 evidence preserver never copies `.state/control`.
- **M5 ff-app:**
  1. `lib/infra/ff1/control/` Dart codec on pointycastle (Concat KDF, GCM, ES256 r‖s), conformance-tested against the fixtures.
  2. Key store: flutter_secure_storage with the pinned options, keyed by `deviceId+gen`.
  3. ObjectBox migration adding `gen`, pins, `controllerId`, `role` and `nk` to `FF1DeviceEntity`; `nk` goes in secure storage.
  4. `FF1SecureCommandClient` wraps the router and relayer `sendCommand` shape, plus the direct callers (bind settle `ff1_lan_bind_provider.dart:825-853`, `startWifiSetup` `ff1_device_actions_providers.dart:521-575`). It picks legacy or secure by the `control` field.
  5. 401 mapping: `enrollment_required`, `access_revoked`, `generation_changed`.
  6. Enrollment UX: scan page, "Secure this FF1", request access, owner approval sheet, controllers list.
  7. Sealed notification decode in the LAN-first and relayer transports. Approval requests now also arrive on LAN, so revisit the relayer-only forward at `ff1_lan_first_transport.dart:268-283`.
  8. Handoff v2.
  9. Remote-config floor `features.secure_control.min_ffos_version`.
- **M6 ff1-cli:**
  - `jose` (panva) for the codec, fixtures in tests;
  - key file at `~/.config/ff-cli/controllers/<deviceId>.json`, mode 0600 (the keyring is optional; headless Linux has no secret service);
  - `device pair` v2;
  - envelopes for `displayPlaylist`, `getDeviceStatus` and `sshAccess` on LAN and the relayer, with a legacy path for old firmware;
  - `ff-cli device controllers` (list) for owner CLIs.
- **M7 docs:** supersede `docs/ff1-v2-controller-authentication.md` (MQTT/mTLS draft) and update `docs/architecture.md` "Deliberately-open LAN surface", the controld inbound-messages doc, the RELEASES ledger, and ff-cli CONFIGURATION.

### Phase 2 — floor
After the fleet counts in §3.1 show enrolled devices.

- App `required_version` floor.
- FF OS release flips `control.enforced=true`.
- Relayer topic-client allowlist at `PortalRelayer.fetch` (envelope + exact displayPlaylist). That code is Anh's repo, but the request is mine.
- Topic rotation with session carry-over stays on #25.

### Phase 3 — tail
`min_upgradeable_version` decision, legacy relayer retirement, account move, and v2 cutover. All on #25; my envelope is unchanged.

## 6. Acceptance (what I demonstrate before the train)

1. **The card's own check.** From a laptop with no credentials, a cast is refused and status has no `topic_id`. App and ff-cli keep working.
2. **Owner vs controller.** An owner approves a controller. The controller cannot reset, enable SSH, approve, or update. Revoke blocks it on an open LAN WS and on the relayer. Re-approval does not reset `ctr`.
3. **Replay and forgery.**
   - A captured request is rejected, including after a restart and after a bench OTA.
   - Tampered, wrong-`gen` and wrong-`dev` requests are rejected.
   - A fake portal on the topic cannot produce an accepted reply.
4. **Mixed versions.**
   - Old app on an enrolled device: relayer playback works; LAN gives a clear failure.
   - New app and CLI on old firmware: the legacy path works.
   - Play sessions cast through every state.
5. **Transport parity.** One enrolled controller works over LAN, legacy relayer, Tailscale IP, and Anh's v2 relay (with the relay disabled → LAN only), with no grant change.
6. **Lifecycle.** Factory reset (both paths) leaves no grants, and the next OTA keeps it that way. Downgrade and re-upgrade are documented. A corrupt store fails closed.
7. **Captures.** No command names or fields appear in plaintext on LAN or the relayer in the enrolled state.

Bench: FF1-8EVTK3RE (disruptive tests allowed). Release builds and dispatch by a human operator per CLAUDE.md.

## 7. Decisions needed before M1

| Who | Decision | My default |
|---|---|---|
| Sean | Owner may approve another owner (phone migration, owner ff-cli for `ssh enable` / #3560) | yes |
| Sean | Lost sole owner = factory reset | yes |
| Sean | Tier table §3.2 (esp. reboot/shutdown, input, uploadLogs) | as listed |
| Anh | Spec/fixture home and M0 date; chunking §2.5; browser grant boundary §2.7 | §2 as written |
| Anh | Notifications as per-controller `nk` (§2.4) vs Anh's slice's encrypted polling | `nk` push |
| Anh | Reuse any of the unpublished slice code (grant store, approval UI) | ask for draft PRs |
| Me | go-jose dependency vs stdlib codec | go-jose; fixtures make it swappable |

## 8. Risks

- **The old app strands on LAN after enrollment** (F13). Mitigation: the new app is required to enroll, so only other household phones are affected; the floor follows soon after.
- **Hand-rolled Dart JOSE.** Mitigation: a narrow profile, fixtures, and negative vectors.
- **fsync per request.** Inputs run at up to 50/s through the gate. Benchmark on the device in M1; if it is too slow, batch the window persist with a short `exp`.
- **A factory reset onto an old snapshot reopens the hub until the next OTA.** This is a documented residual. Before phase 2, make sure the recovery candidate is new firmware fleet-wide.
