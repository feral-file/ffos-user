# feral-file#3551 — Brandon's execution plan: endpoint authorization and E2EE

Status: phase 0 and phase 1 implemented, independently reviewed and hardware-tested (A–K) on local branches, 2026-10-20; not pushed. See §9.

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
- A notification is a JWE `dir/A256GCM` under `nk`, with protected header exactly `{"alg":"dir","enc":"A256GCM","typ":"ff1-ntf+jwe","kid":"<ctl>"}`, over `{v, dev, gen, ctl, ep, seq, iat, type, body}` (as built: spec §9).
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
Reported in `/api/v2/status` (as built: `control:{v:1, state, profile}`, profile = `{v, dev, gen, sig, enc}`; pins are derived from it), in `device_status`, and in a boot log line (fleet count via log streaming).

- **legacy:** new firmware, no grants. Plaintext accepted as today, except for the phase-0 containment rules. The invitation QR is offered.
- **enrolled:** at least one owner grant.
  - LAN accepts envelopes only (as built: plaintext → **403** `enrollment_required`).
  - Relayer plaintext is limited to the **controller tier**, so off-LAN old apps keep playback. On the same Wi-Fi the released app cannot cast at all: its router relays after an HTTP error only for the status probe, so every `displayPlaylist` gets LAN 403 and a cast error (no hang, no crash). Open decision for the change owner: accept and record this, or answer LAN plaintext controller-tier commands in `enrolled` as the relayer does. The same released app on the same LAN also loses all live status: the device stops LAN WebSocket plaintext in `enrolled`, the released LAN transport logs and drops each sealed frame, and its LAN-first transport ignores relayer `device_status`/`player_status` while LAN is up (review round 2, B-12; needs a hardware check).
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
   - It sends `op:requestAccess` with its keys and label, on the LAN or via the relayer (as built: both routes, with per-route caps of 1 pending per LAN source and 2 via the relayer, under ≤4 total; spec §5.2). The device holds it as pending: 5-minute TTL, 10-minute absolute life, rate-limited.
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
  - `/api/screenshot` requires the same header on a claimed device (otherwise any LAN client can show the claim QR and screenshot the topic).
  - `connect` stays open: the released app sends it LAN-first before every cast without the header and with no relayer fallback. It only rewrites the claim record; phase 1 makes it owner-tier.
  - `/api/status` and `/api/v2/status` omit `topic_id` once claimed. The second phone uses "Show Pairing QR Code" (F14).
  - Audit every LAN-served payload for the topic: status, `getDeviceStatus` reply, notifications, `/metrics`, logs, mDNS.
  - Update the `hub/status.go:132` rationale comment.
- **P0-2 ff1-cli:** `ssh enable|disable` sends `X-FF1-Topic` from the keyring. A clear error on 401 tells the user to re-pair.
- **Residual, documented:** `connect`, reset, Wi-Fi setup, `showPairingQRCode` and input stay open on LAN until phase 1. A sole-phone owner who loses that phone re-pairs only via reset.
- **Check:** from a laptop on the Wi-Fi, `curl …/api/v2/status` has no `topic_id`, and `sshAccess` without the header gets 401. ~~App pairing a second phone via the QR still works.~~ **False on the same LAN** (review round 2, B-2): the app's LAN bind sees `claimed:true` with no `topic_id`, reports "already claimed, cannot pair another phone, reset it" and never tries the QR topic. The released app hits this under phase 0 alone; off-LAN it binds only through an ambiguous-delivery fallback. The branch app is being changed to take the QR-topic path for an owned device. Release consequence and decision for the change owner: ship phase 0 only together with that app fix, or accept that a second phone on the same Wi-Fi needs the new app.

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
   - Old app on an enrolled device: off-LAN relayer playback works; on the same LAN casting fails with a clear error (no relay fallback, see §3.1).
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

- **The old app strands on LAN after enrollment** (F13): it cannot cast at all on the same Wi-Fi, not only lose LAN control. Mitigation: the new app is required to enroll, so only other household phones are affected; the floor follows soon after.
- **Hand-rolled Dart JOSE.** Mitigation: a narrow profile, fixtures, and negative vectors.
- **fsync per request.** Inputs run at up to 50/s through the gate. Benchmark on the device in M1; if it is too slow, batch the window persist with a short `exp`.
- **A factory reset onto an old snapshot reopens the hub until the next OTA.** This is a documented residual. Before phase 2, make sure the recovery candidate is new firmware fleet-wide.

## 9. Implementation status (2026-10-20)

| Repo | Branch | Scope | State |
|---|---|---|---|
| ffos-user | `feat/3551-phase0-lan-containment` | P0-1 | done; `connect` left open (released app sends it before every LAN cast); `/api/screenshot` also topic-gated |
| ff1-cli | `feat/3551-phase0-topic-header` | P0-2 | done |
| ffos-user | `feat/3551-control-protocol` (includes phase 0) | M0 spec + fixtures, M1–M3, watchdog/otagate follow-ups | done; two review rounds |
| ffos | `feat/3551-ota-control-state` | M4 (carry `control/`, `control.new/`, `control.retired` with `--checksum`; watchdog marker `/run/feral-controld-ota-stop`) | done; reviewed |
| ff1-cli | `feat/3551-controller-auth` (on phase 0) | M6 | done; repo-required review |
| ff-app | `feat/3551-controller-auth` | M5 core + UI | done; repo-required review; second-pass review pending |

Changes against the plan, decided during implementation:
- **Phase 0:** `connect` is not gated.
- **Approval code:** binds the requester's pinned `devSig` (spec §5.4), which defeats a LAN MITM serving a fake profile.
- **Fail-closed configuration (§8.1):** control state on disk keeps the protocol on regardless of `controld.json`. Factory reset writes the `control.retired` tombstone before erasing.
- **Invitations:**
  - no attempt cap;
  - the invitation lives only while its QR is on screen;
  - `/api/screenshot` is refused while an invitation is live, and is re-checked after capture;
  - a client must enroll before it sends `connect`.
- **Plaintext with ambiguous or duplicate top-level keys** is refused. Arguments are normalized from the judged bytes.
- **Relayer plaintext denials** use the `{"ok":false,"error":...}` shape that released clients surface.
- **ff-cli** never pins from LAN status; its pins come only from the owner's handoff.

**Release constraint (downgrade safety): `control.enabled` MUST NOT be set in `controld.json` for any channel before the ffos M4 image (`feat/3551-ota-control-state`) is on that channel's OTA path.** An image without the carry step would drop `.state/control` on update (owners lose their grants) or, on a later downgrade/upgrade, resurrect stale grants. The M4 image also refuses to install a pre-carry target ("Refusing update: target image cannot carry controller state", classified permanent by otagate).

Release-notes line for the first control-enabled release: **once a control-enabled release runs on a device, that device can never OTA to an older image; the only way back is a factory reset.** Operational hole the code cannot close: dispatching an image workflow file that predates the strip gate, with `.control.enabled=true` already in that environment's secret, ships an image that creates state and loses it on its next OTA.

Still open before a release train:
1. Hardware: FF1-8EVTK3RE was unreachable during implementation. Still to run:
   - dev controld with `control.enabled`;
   - the fsync benchmark (`control.test -test.bench OpenPersist`);
   - a bench OTA carrying control state;
   - factory reset, both paths;
   - captures;
   - a physical-phone app pass, including the on-device Flutter MCP pass ff-app AGENTS.md requires.
2. Anh: agree §2 with #25 (v2 chunking, browser grant boundary). Run the thin E2E over the v2 relay.
3. Sean: owner-approves-owner, lost phone = reset, and the tier table (§7).
4. Push branches and open PRs: ffos/ffos-user against `develop`, ff-app/ff1-cli against `main`. No workflow dispatch by agents.

### Hardware test, FF1-8EVTK3RE (2026-10-20)

The build under test was the device branch before `5f363a3`, running FF OS 2.0.12 with `control.enabled`.

Passed:
- **A. Phase 0.**
- **B. Benchmark:** fsync costs 3.4–5.1 ms per admitted request, about 200–290 req/s fully serialized.
- **C. Owner enrollment by migration QR:** the screenshot was refused while the invitation was live.
- **D. Enrolled policy.**
- **E. Real ff-cli as second controller:** approval code matched; controller denied ssh; promote then ssh; revoke; re-approve.
- **F. Replay and forgery,** also after a restart.
- **G. Persistence.**
- **H. Enforced mode:** the relayer dedupes duplicate keys before the device sees them.
- **I. Fail-closed config.**
- **J. LAN capture:** no command or field names in clear.
- **K. OTA carry step and watchdog marker on real systemd.**

Partial:
- **L. Factory reset:** the tombstone is written before the delete, as designed. The device then came back in the factory image with no Wi-Fi credentials, so the post-reset checks were not run.

Still to retest on hardware once the device is reprovisioned:
- the device fixes after `5f363a3`: the M1 lock split, `busy`, per-channel seals, the audit log and the enforced purge;
- the post-reset state;
- a real full-image OTA.

The ff-app UI has not been exercised on a physical phone.

### Review rounds

| Repo | Reviews | Last state |
|---|---|---|
| ffos-user | review, verification, independent | fixed through `ac7d892` |
| ffos | review, independent | fixed through `5b80c5c` |
| ff1-cli | repo-required, independent | fixed through `0c75adc` |
| ff-app | repo-required, second pass, third pass, confirmed-gen review | fixed through `d33224e7` |

The client rules that came out of the reviews are normative in spec §14.
