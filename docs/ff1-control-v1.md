# FF1 control protocol v1 (feral-file#3551)

Status: normative contract for the 3551 implementation. Supersedes
`ff1-v2-controller-authentication.md` (MQTT/mTLS draft) for controller
authentication. Every implementation (Go in `feral-controld/control`, Dart in
ff-app, TypeScript in ff1-cli, and any relay endpoint adapter for
feral-file/ff-relayer#25) MUST pass the fixtures in
`components/feral-controld/control/testdata/fixtures/`.

FF1 is the only authority. Transports (LAN hub, legacy ff-relayer, v2 opaque
relay, an owner VPN) carry the same bytes and never confer or change authority.

## 1. Encoding conventions

- `b64u` = base64url without padding (RFC 7515 §2).
- JSON is UTF-8. Implementations MUST reject duplicate member names in
  protected headers and in claims.
- Times are integer Unix seconds.
- Integers in claims are JSON numbers in `[0, 2^53-1]`.

## 2. Keys and identifiers

| Name | Type | Holder | Purpose |
|---|---|---|---|
| `devSig` | P-256 ECDSA | FF1 | signs replies (ES256) |
| `devEnc` | P-256 ECDH | FF1 | recipient key for requests (ECDH-ES) |
| `ctlSig` | P-256 ECDSA | controller | signs requests (ES256) |
| `ctlEnc` | P-256 ECDH | controller | reserved for future device→controller key agreement; registered at enrollment |

- A public key is a JWK `{"kty":"EC","crv":"P-256","x":b64u(32),"y":b64u(32)}`.
  Implementations MUST verify the point is on the curve and MUST ignore/strip
  any other member when computing thumbprints.
- `thumb(jwk)` = `b64u(SHA-256(UTF8('{"crv":"P-256","kty":"EC","x":"<x>","y":"<y>"}')))`
  (RFC 7638).
- `controllerId` = `thumb(ctlSig)`.
- `deviceId` = the FF1 hostname, e.g. `FF1-8EVTK3RE` (also the v2 relay
  `playerId`). Compared exactly.
- `gen` = `b64u(16 random bytes)`, created together with `devSig`/`devEnc`. A
  new `gen` means a new control identity; every grant from an earlier `gen` is
  void.
- Device **profile** (public): `{"v":1,"dev":deviceId,"gen":gen,"sig":JWK,"enc":JWK}`.
- Device **pins**: `thumb(sig)` and `thumb(enc)`.

## 3. Request (controller → FF1)

A request is a compact JWE whose plaintext is a compact JWS.

### 3.1 Outer JWE

Protected header, exactly these members:

```json
{"alg":"ECDH-ES","enc":"A256GCM","kid":"<thumb(devEnc)>","typ":"ff1-req+jwe","cty":"ff1-req+jws","epk":{"kty":"EC","crv":"P-256","x":"…","y":"…"}}
```

- Key agreement: ECDH between a fresh ephemeral P-256 key (`epk`) and `devEnc`.
- CEK = Concat KDF (RFC 7518 §4.6.2) with SHA-256, `Z` = the shared secret,
  `keydatalen` = 256, `AlgorithmID` = `"A256GCM"`, `PartyUInfo` and
  `PartyVInfo` empty (no `apu`/`apv`), `SuppPubInfo` = 256 as uint32 big-endian,
  no `SuppPrivInfo`.
- Content encryption: AES-256-GCM, 96-bit random IV, 128-bit tag,
  AAD = ASCII(`b64u(protected header bytes)`). No `zip`. JWE Encrypted Key is
  empty. Compact form: `hdr..iv.ciphertext.tag`.

### 3.2 Inner JWS

Protected header, exactly: `{"alg":"ES256","kid":"<controllerId>","typ":"ff1-req+jws"}`.
Signature is the 64-byte `r||s` (RFC 7518 §3.4), never DER.

### 3.3 Claims

| Claim | Required | Meaning |
|---|---|---|
| `v` | yes | `1` |
| `dev` | yes | target `deviceId` |
| `gen` | yes | target `gen` |
| `ctl` | yes | `controllerId`; MUST equal the inner JWS `kid` |
| `rid` | yes | request id, 1–64 chars `[A-Za-z0-9_-]` |
| `ctr` | yes | per-controller counter, starts at 1, MUST increase per new request |
| `iat` | yes | issue time |
| `exp` | yes | expiry; `exp - iat` in `[1, 120]` |
| `op` | yes | `"cmd"`, `"enroll"`, or `"requestAccess"` |
| `rk` | yes | `b64u(32 random bytes)`, the reply key |
| `cmd` | op=cmd | existing command name, e.g. `"displayPlaylist"` |
| `req` | op=cmd | the existing `request` object, unchanged (`{}` when none) |
| `enroll` | op=enroll/requestAccess | §5 |

### 3.4 Device verification order

The device runs these steps in order and stops at the first failure:

1. Parse the JWE header. It must be exactly the shape in §3.1 and `kid` must equal
   `thumb(devEnc)`. Decrypt the payload.
2. Parse the JWS header. Look up `kid`:
   - for `op=cmd`, a key in the grant store (active or revoked);
   - for `enroll`/`requestAccess`, the key carried in `enroll.sig`, with
     `thumb(enroll.sig) == kid`.
   Verify ES256.
3. Check claims: `v`, `dev`, `gen`, `ctl==kid`, `exp - iat` bound,
   `now <= exp + 30`, `iat <= now + 300` (clock skew allowances).
4. Replay window (§6).
5. Authorization (§7). Then persist the replay window **before** executing.

Failures in steps 1–2 produce a public error (§4.3). Failures from step 3 on
produce an encrypted reply.

## 4. Reply (FF1 → controller)

### 4.1 Format

- Compact JWE, protected header exactly
  `{"alg":"dir","enc":"A256GCM","typ":"ff1-res+jwe","cty":"ff1-res+jws"}`.
- CEK = `rk` from the request. The AES-GCM rules are those in §3.1.
- Plaintext = compact JWS, header `{"alg":"ES256","kid":"<thumb(devSig)>","typ":"ff1-res+jws"}`, signed by `devSig`.

Claims:

```json
{"v":1,"dev":"…","gen":"…","ctl":"…","rid":"…","iat":0,"ok":true,"res":{…}}
{"v":1,"dev":"…","gen":"…","ctl":"…","rid":"…","iat":0,"ok":false,"code":"permission_denied","res":{…}}
```

### 4.2 What the controller checks

The controller MUST check all of these:
- decryption with its own `rk`;
- the ES256 signature against the **pinned** `devSig`;
- `dev`/`gen`/`ctl`/`rid` equal to what it sent.

Any failure means "no valid reply". The reply is never trusted partially.

### 4.3 Encrypted error codes

`invalid_request`, `expired`, `replay`, `permission_denied`, `access_revoked`,
`invitation_invalid`, `pending`, `not_approved`, `rate_limited`, `locked`,
`command_failed`.

For `command_failed`, `res` carries the existing handler error body, for example
`{"error":"sourceUnreachable",…}`, with the same shape the relayer path returns
today.

### 4.4 Public errors

These are sent when no reply key can be trusted. The body is
`{"error":{"code":"<code>"}}` with one of these codes:

| Code | When |
|---|---|
| `bad_envelope` | step 1 failed |
| `unknown_controller` | step 2: `kid` not found |
| `bad_signature` | step 2: signature did not verify |
| `enrollment_required` | plaintext refused, §7.2 |
| `envelope_required` | owner-tier plaintext on the relayer |
| `locked` | §8 |

On the LAN these come back as HTTP 401, except `envelope_required` and
`enrollment_required`, which come back as 403. On the relayer they are the RPC
`message`.

## 5. Enrollment

`enroll` object: `{"sig":JWK,"enc":JWK,"label":string(1..64),"platform":"ios"|"android"|"cli"|"other","inv"?:string}`.

### 5.1 `op=enroll` with `inv`

The first owner.

- Valid only while the device has no active owner grant.
- `inv` is the current invitation: `b64u(16 random bytes)`, single use, 10-minute
  life, compared in constant time. A wrong value counts against a
  5-per-invitation attempt limit, then the invitation is voided.
- On success: create an owner grant, consume the invitation, and remove the
  claim QR.
- `res` = `{"role":"owner","nk":b64u(32),"profile":profile}`.

### 5.2 `op=requestAccess`

A later controller asks for access.

- Creates or refreshes a pending request for `ctl`.
  - At most 4 pending requests; at most 1 new request per 10 s from any source.
  - A pending request lives for 5 minutes.
- `res` = `{"status":"pending","code":approvalCode(ctl),"expiresAt":t}`.
- Every owner gets a sealed `controller_approval_request` notification (§9),
  with body `{"ctl","label","platform","code","expiresAt"}`.

### 5.3 `op=enroll` without `inv`

Succeeds only after an owner has approved exactly `ctl`.

- `res` = `{"role":…,"nk":…,"profile":…,"expiresAt"?:t}`.
- Before approval, the reply is `ok:false` with code `pending` (a live request
  exists) or `not_approved`.

### 5.4 Approval code

`approvalCode(ctl)` = the first 4 bytes of `SHA-256("ff1-approval-code/v1|" + ctl)`
read as uint32 big-endian, mod 1000000, zero-padded to 6 digits. The owner UI
and the requester both display it.

### 5.5 Owner commands

These are `op=cmd`, owner tier:

| Command | `req` | `res` |
|---|---|---|
| `approveController` | `{"ctl","role":"owner"|"controller","expiresAt"?}` | — |
| `revokeController` | `{"ctl"}` | — |
| `listControllers` | `{}` | `{"controllers":[{"ctl","role","label","platform","createdAt","expiresAt"?,"revokedAt"?,"self":bool}],"pending":[…]}` |

An owner cannot revoke the last active owner. That would make the device
unowned without a reset.

## 6. Replay window

Each controller record holds `hi` (the highest accepted `ctr`) and a 64-bit
bitmap of the `ctr` values in `(hi-64, hi]`. A request is accepted only if both
hold:
- `ctr > hi - 64`;
- `ctr` is not already marked in the bitmap.

On acceptance the window is updated and **persisted durably before the command
executes**: write a temp file, fsync it, rename, then fsync the directory.

A revoked record keeps its window. Re-approval keeps it too.

`requestAccess` and pre-approval `enroll` requests are windowed per `ctl` in the
pending record.

## 7. Authorization

### 7.1 Command tiers

Unknown commands fail closed for the controller tier.

| Tier | Commands |
|---|---|
| owner | everything; specifically required for `sshAccess`, `factoryReset`, `startWifiSetup`, `updateToLatestVersion`, `connect`, `setDeviceName`, `showPairingQRCode`, `startMintPairingSession`, `joinMintPairingChannel`, `closeMintPairingSession`, `mintPairingApprovalDecision`, `setContentPolicy`, `analyticsToggle`, `betaFeaturesToggle`, `uploadLogs`, `sendKeyboardEvent`, `tapGesture`, `doubleTapGesture`, `longPressGesture`, `dragGesture`, `clickAndDragGesture`, `zoomGesture`, `approveController`, `revokeController`, `listControllers` |
| controller | the playback commands (`displayPlaylist`, `displayDefaultPlaylist`, `pause`, `resume`, `moveToArtwork`, `refreshArtwork`, `checkStatus`, `setShuffle`, `setLoop`, `updateArtFraming`, `updateDisplaySettings`, `updateDefaultDuration`, `getRecentlyPlayed`, `playRecentlyPlayed`, `pauseCasting`, `resumeCasting`, `nextArtwork`, `previousArtwork`), the offline-cache commands, `getDeviceStatus`, `deviceMetrics`, `rotate`, `setVolume`, `toggleMute`, `ddcPanelControl`, `ddcPanelStatus`, `setSleepSchedule`, `setSleepMode`, `sleepNow`, `wakeNow`, `reboot`, `shutdown`, `runNetworkDiagnostics`, `getContentPolicy` |

The implementation MUST keep this list in one table in code. The table MUST be
cross-checked in a test against `commands/types.go` and every command name the
CDP path forwards, so that a new command cannot ship without a tier.

### 7.2 Device states and plaintext policy

| State | Condition | Plaintext on LAN `/api/cast` | Plaintext via relayer | Envelopes |
|---|---|---|---|---|
| `legacy` | no active owner grant | allowed (phase-0 rules apply) | allowed | `enroll` (with inv) and `requestAccess`→`not_approved` only |
| `enrolled` | ≥1 active owner grant, `control.enforced=false` | refused: `enrollment_required` | controller tier only; owner tier → `envelope_required` | full |
| `enforced` | ≥1 active owner grant, `control.enforced=true` | refused | only the exact Play shape `{"command":"displayPlaylist","request":{"intent":{"action":"now_display"},"dp1_call":{…}}}` | full |
| `locked` | identity or grant store unreadable/corrupt | refused: `locked` | controller tier only | refused: `locked` |

Internal callers (OOM recovery, `playRecentlyPlayed` re-entry) are not subject
to this policy.

## 8. Device state on disk

Directory: `/home/feralfile/.state/control/`, mode 0700. Files are mode 0600.

- `identity.json`: `{"v":1,"gen","sig":privateJWK,"enc":privateJWK,"createdAt"}`
- `grants.json`: `{"v":1,"gen","controllers":[{"ctl","sig","enc","role","label","platform","createdAt","approvedBy","expiresAt"?,"revokedAt"?,"nk","hi","bitmap"}]}`

Rules:
- A missing directory on start means create a new identity, in state `legacy`.
- A present but unreadable or inconsistent directory (for example a `gen`
  mismatch) means state `locked`. It never becomes `legacy` automatically.
- Factory reset deletes the directory before it starts the reset unit.
- OTA carries the directory live: see ffos `feral-system-update.sh`.

## 9. Sealed notifications (FF1 → one controller)

Each active grant has `nk` (32 bytes), issued at enrollment.

A sealed notification is a compact JWE:
- protected header exactly
  `{"alg":"dir","enc":"A256GCM","typ":"ff1-ntf+jwe","kid":"<ctl>"}`;
- key `nk`;
- plaintext claims `{"v":1,"dev","gen","ctl","seq","iat","type","body"}`.

`seq` increases per controller within one daemon run. Controllers accept the
newest `iat`/`seq` and ignore older ones.

Wire carriage, the same object on the LAN notification WebSocket and on the
relayer notification channel:

```json
{"type":"notification","notification_type":"sealed","persist_record_count":0,"message":{"v":1,"ctl":"<ctl>","jwe":"…"}}
```

What is sent in each state:

| State | Sealed copies | LAN WebSocket plaintext | Relayer plaintext |
|---|---|---|---|
| `enrolled` / `enforced` | one per active grant, for every notification type including `controller_approval_request` | none | `enrolled`: unchanged, for old apps; `enforced`: stopped |
| `legacy` | — | unchanged | unchanged |

## 10. Wire carriage of requests

| Route | Request | Reply |
|---|---|---|
| LAN | `POST /api/cast`, body `{"command":"ff1Envelope","request":{"v":1,"jwe":"<compact>"}}` | 200 `{"v":1,"jwe":"<compact>"}` |
| legacy relayer | the same JSON as a topic cast | RPC `message` = `{"v":1,"jwe":"<compact>"}` |
| v2 relay | frame payload = UTF-8 bytes of the compact JWE; chunking is TBD with #25 | same |

## 11. Public discovery

- `/api/v2/status` and `getDeviceStatus` gain
  `"control":{"v":1,"state":"legacy|enrolled|enforced|locked","profile":profile}`.
  The presence of `control` is the capability signal.
- The claim QR, while unowned, appends four pipe fields to the existing six:
  `…|<setup_phase>|<inv>|<thumb(sig)>|<thumb(enc)>|<gen>`.
  - Old app parsers read only indices 0, 1 and 3.
  - The topic stays at index 1 while the device is unowned.
  - The QR built in `components/launcher-ui/index.html` and the one built by
    `formatDeviceConnectURL` MUST stay byte-identical.
- `showPairingQRCode` with `{"show":true,"purpose":"enroll"}` on a `legacy`,
  claimed device displays the invitation QR. This is the migration path for
  2.0.x owners.

## 12. Fixtures

`components/feral-controld/control/testdata/fixtures/` holds:
- `keys.json`: fixed device and controller test keys;
- `positive/*.json`: `{input claims, fixed ephemeral key/IV, expected compact, expected decoded}`;
- `negative/*.json`: a compact value plus the expected error code, covering
  tampered tag, wrong `kid`, wrong `dev`, wrong `gen`, expired, replayed `ctr`,
  out-of-window `ctr`, DER signature, extra header member, `alg` `none`/`RSA`,
  and an oversize body.

Fixtures are generated by `go run ./control/cmd/genfixtures` and MUST also
round-trip through a second, independent implementation: the ff1-cli test suite
using `jose`.
