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

- `b64u` = base64url without padding (RFC 7515 §2). Decoders MUST reject
  `=` padding, whitespace and line breaks, and non-zero trailing bits.
- JSON is UTF-8. Implementations MUST reject invalid UTF-8 and duplicate
  member names, at any depth, in protected headers, claims, JWKs and
  `grants.json`. Members are compared after unescaping, so `"typ"` and
  `"\u0074yp"` are duplicates.
- Times are integer Unix seconds.
- Integers in claims are JSON numbers in `[0, 2^53-1]`, written in canonical
  form: no fraction, exponent, sign, leading zero or quotes. `1.0`, `1e3` and
  `"1"` are invalid.
- Every header, claims object and JWK has a closed member set. A member this
  document does not list is an error, not an extension point.

## 2. Keys and identifiers

| Name | Type | Holder | Purpose |
|---|---|---|---|
| `devSig` | P-256 ECDSA | FF1 | signs replies (ES256) |
| `devEnc` | P-256 ECDH | FF1 | recipient key for requests (ECDH-ES) |
| `ctlSig` | P-256 ECDSA | controller | signs requests (ES256) |
| `ctlEnc` | P-256 ECDH | controller | reserved for future device→controller key agreement; registered at enrollment |

- A public key is a JWK with exactly the members
  `{"kty":"EC","crv":"P-256","x":b64u(32),"y":b64u(32)}`, in any order.
  Implementations MUST verify the point is on the curve (and not the point at
  infinity) and MUST reject a JWK with any other member, a private `d` above
  all. Implementations SHOULD emit the members in the thumbprint order below.
- `thumb(jwk)` = `b64u(SHA-256(UTF8('{"crv":"P-256","kty":"EC","x":"<x>","y":"<y>"}')))`
  (RFC 7638). It is always 43 characters.
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

Protected header, exactly these members (any order) and no others (no `zip`,
`apu`, `apv`, `crit`, `jku`, ...):

```json
{"alg":"ECDH-ES","enc":"A256GCM","kid":"<thumb(devEnc)>","typ":"ff1-req+jwe","cty":"ff1-req+jws","epk":{"kty":"EC","crv":"P-256","x":"…","y":"…"}}
```

- Key agreement: ECDH between a fresh ephemeral P-256 key (`epk`) and `devEnc`.
  `epk` follows the JWK rules of §2.
- CEK = Concat KDF (RFC 7518 §4.6.2) with SHA-256, `Z` = the shared secret,
  `keydatalen` = 256, `AlgorithmID` = `"A256GCM"`, `PartyUInfo` and
  `PartyVInfo` empty (no `apu`/`apv`), `SuppPubInfo` = 256 as uint32 big-endian,
  no `SuppPrivInfo`. With the all-zero `Z` this yields
  `ohtGU230iroWUJBOQYR7JT-iHsoYc-MLxzoRXrqr6Yw` (a known-answer check).
- Content encryption: AES-256-GCM, 96-bit random IV, 128-bit tag,
  AAD = ASCII(`b64u(protected header bytes)`). No `zip`. JWE Encrypted Key is
  empty. Compact form: `hdr..iv.ciphertext.tag`.
- Size limits: the compact form is at most 4 MiB (4194304 bytes) and the
  protected-header segment at most 4 KiB, checked before any decoding.
  Transports SHOULD cap request bodies at the same 4 MiB.

### 3.2 Inner JWS

Protected header, exactly: `{"alg":"ES256","kid":"<controllerId>","typ":"ff1-req+jws"}`.
`kid` is a thumbprint (43 characters).

The signature is the 64-byte `r||s` (RFC 7518 §3.4), never DER. Verifiers
accept high-S signatures (ES256 does not require low-S, and WebCrypto does
not normalize). Signers MAY sign deterministically (RFC 6979; the Go
reference does, which makes the fixtures byte-reproducible); verifiers MUST
NOT depend on it.

### 3.3 Claims

| Claim | Required | Meaning |
|---|---|---|
| `v` | yes | `1` |
| `dev` | yes | target `deviceId` |
| `gen` | yes | target `gen` |
| `ctl` | yes | `controllerId`; MUST equal the inner JWS `kid` |
| `rid` | yes | request id, 1–64 chars `[A-Za-z0-9_-]` |
| `ctr` | yes | per-controller counter, starts at 1, MUST increase per new request. One sequence per `ctl` across every `op` |
| `iat` | yes | issue time |
| `exp` | yes | expiry; `exp - iat` in `[1, 120]` |
| `op` | yes | `"cmd"`, `"enroll"`, or `"requestAccess"` |
| `rk` | yes | `b64u(32 random bytes)`, the reply key |
| `cmd` | op=cmd only | existing command name, e.g. `"displayPlaylist"`, 1–64 bytes |
| `req` | op=cmd only | the existing `request` object, unchanged (`{}` when none); MUST be a JSON object |
| `enroll` | op=enroll/requestAccess only | §5 |

No other claim is allowed. `cmd`/`req` with an enrollment op, or `enroll`
with `op=cmd`, is `invalid_request`.

### 3.4 Device verification order

The device runs these steps in order and stops at the first failure:

1. Parse the JWE header. It must be exactly the shape in §3.1 and `kid` must equal
   `thumb(devEnc)`. Decrypt the payload.
2. Parse the JWS header. Look up `kid`:
   - first in the grant store (active or revoked records), for any `op`;
   - otherwise, only when the (not yet trusted) claims say
     `op=enroll`/`requestAccess`, the key carried in `enroll.sig`, with
     `thumb(enroll.sig) == kid`.
   Verify ES256. A signature that is not exactly 64 bytes (DER, truncated)
   fails verification here.
3. Read `rk` and `rid`. Without a usable reply key and request id no
   encrypted reply is possible, so if the claims are not a well-formed JSON
   object (§1), `rk` is not `b64u(32 bytes)`, or `rid` breaks §3.3, the
   failure is public (`bad_envelope`).
4. Check claims: the member rules of §3.3, `v`, `dev`, `gen`, `ctl==kid`,
   `exp - iat` bound, `now <= exp + 30`, `iat <= now + 300` (clock skew
   allowances).
5. Replay window (§6).
6. Authorization (§7) and the operation's own preconditions (§5). Then
   persist the replay window **before** executing.

Failures in steps 1–3 produce a public error (§4.4). Failures from step 4 on
produce an encrypted reply whose `ctl` is the verified JWS `kid` (even when
the request's `ctl` claim differs).

Code mapping for step 4: any member, shape, `v`, `dev`, `gen`, `ctl`,
lifetime or future-`iat` failure is `invalid_request`; `now > exp + 30` is
`expired`. Step 5 failures are `replay`.

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
`command_failed`, `internal_error`.

For `command_failed`, `res` carries the existing handler error body, for example
`{"error":"sourceUnreachable",…}`, with the same shape the relayer path returns
today. The owner commands of §5.5 use `res` = `{"error":"<reason>"}` with
`<reason>` one of `invalid_request`, `not_pending`, `not_found`,
`last_owner`, `too_many_controllers`.

`internal_error` means the device could not durably record the request's
replay window (§6). The request was **not** executed. Its `ctr` may or may
not be consumed, so the controller retries with a new `ctr`.

`access_revoked` covers both a revoked and an expired grant.

### 4.4 Public errors

These are sent when no reply key can be trusted. The body is
`{"error":{"code":"<code>"}}` with one of these codes, and never any other
detail (a cause string would make the device an oracle):

| Code | When |
|---|---|
| `bad_envelope` | §3.4 step 1 or 3 failed, or the JWS is structurally malformed |
| `unknown_controller` | step 2: `kid` not found |
| `bad_signature` | step 2: signature did not verify |
| `enrollment_required` | plaintext refused, §7.2 |
| `envelope_required` | owner-tier plaintext on the relayer |
| `locked` | §8; checked before step 1 |

On the LAN these come back as HTTP 401, except `envelope_required` and
`enrollment_required`, which come back as 403. On the relayer they are the RPC
`message`.

## 5. Enrollment

`enroll` object, exactly: `{"sig":JWK,"enc":JWK,"label":string(1..64 characters),"platform":"ios"|"android"|"cli"|"other","inv"?:string}`.
`inv` is allowed only with `op=enroll`. `thumb(enroll.sig)` MUST equal `ctl`
(else `invalid_request`).

### 5.1 `op=enroll` with `inv`

The first owner.

- Valid only in state `legacy` (no active owner grant); otherwise the reply is
  `invitation_invalid`.
- `inv` is the current invitation: `b64u(16 random bytes)`, single use, 10-minute
  life, compared in constant time. A wrong value counts against a
  5-per-invitation attempt limit, then the invitation is voided. Issuing a new
  invitation replaces the old one and resets the count.
- On success: create an owner grant (`approvedBy` = `"invitation"`), consume
  the invitation, and remove the claim QR.
- `res` = `{"role":"owner","nk":b64u(32),"profile":profile}`.

### 5.2 `op=requestAccess`

A later controller asks for access.

- In state `legacy` the reply is `not_approved`: there is no owner to approve,
  and the first controller uses the invitation instead.
- If `ctl` already holds an active grant: `ok:true`,
  `res` = `{"status":"approved","role":role}`, no notification.
- Otherwise it creates or refreshes a pending request for `ctl`.
  - At most 4 pending requests; at most 1 **new** request per 10 s from any
    source (a refresh is not new). Either limit gives `rate_limited`.
  - A pending request lives for 5 minutes from its last refresh.
  - Pending requests are held in memory only. A restart drops them and the
    requester asks again.
- `res` = `{"status":"pending","code":approvalCode(ctl, thumb(devSig)),"expiresAt":t}`.
- On every create and refresh, every active owner gets a sealed
  `controller_approval_request` notification (§9), with body
  `{"ctl","label","platform","code","expiresAt"}`.

### 5.3 `op=enroll` without `inv`

Succeeds only after an owner has approved exactly `ctl`.

- `res` = `{"role":…,"nk":…,"profile":…,"expiresAt"?:t}`.
- It is idempotent: a controller whose reply was lost enrolls again and gets
  the same grant and the same `nk`. `enroll.enc` MUST equal the approved
  `enc` (else `invalid_request`).
- Before approval, the reply is `ok:false` with code `pending` (a live request
  exists; `res` as in §5.2) or `not_approved`.

### 5.4 Approval code

`approvalCode(ctl, sigPin)` = the first 4 bytes of
`SHA-256("ff1-approval-code/v1|" + ctl + "|" + sigPin)` read as uint32
big-endian, mod 1000000, zero-padded to 6 digits, where `sigPin` is
`thumb(devSig)`. The device computes it with its own pin; the requester
computes it with the pin it holds (from the claim QR or the profile it
pinned). The owner UI shows the device's code and the requester shows its
own; the owner approves only when they match.

Binding the pin defeats a LAN man-in-the-middle that serves a new phone a
fake profile and relays the phone's still-valid `requestAccess` JWS to the
real device: the phone's code is then computed over the attacker's pin and
differs from the code the real device sends the owner.

### 5.5 Owner commands

These are `op=cmd`, owner tier, executed by the control authority itself (they
never reach the command router):

| Command | `req` (exact members) | `res` |
|---|---|---|
| `approveController` | `{"ctl","role":"owner"|"controller","expiresAt"?}` | `{}` |
| `revokeController` | `{"ctl"}` | `{}` |
| `listControllers` | `{}` | `{"controllers":[{"ctl","role","label","platform","createdAt","expiresAt"?,"revokedAt"?,"self":bool}],"pending":[{"ctl","label","platform","code","expiresAt"}]}` |

- `approveController` requires a live pending request for `ctl` (else
  `not_pending`); the grant takes its keys, label and platform from that
  request. `expiresAt` must be in the future and is allowed only for
  `role:"controller"`: an owner grant never expires, so the last owner cannot
  silently lapse. Approving a `ctl` that already has a record (revoked, or
  active for a role change) updates that record in place: it keeps its
  `createdAt` and its replay window, and gets a fresh `nk`. At most 32 active
  grants and 128 records (revoked ones included) exist
  (`too_many_controllers`).
- `revokeController` on a `ctl` that has only a pending request denies (drops)
  that request. Otherwise the grant must be active (`not_found`).
- `listControllers` lists every record, revoked ones included.

An owner cannot revoke the last active owner (`last_owner`). That would make
the device unowned without a reset. An owner may revoke itself while another
active owner exists.

## 6. Replay window

Each controller record holds `hi` (the highest accepted `ctr`) and a 64-bit
bitmap where bit `i` marks `ctr = hi - i` (bit 0 is `hi`). A request is
accepted only if all hold:
- `ctr >= 1`;
- `ctr > hi - 64`;
- `ctr` is not already marked in the bitmap.

On acceptance the window is updated and **persisted durably before the command
executes**: write a temp file, fsync it, rename, then fsync the directory.
If that write fails the request is not executed (`internal_error`, §4.3).

A revoked record keeps its window. Re-approval keeps it too.

`requestAccess` and pre-approval `enroll` requests from a `ctl` with no record
are windowed per `ctl` in the (in-memory) pending record; the window becomes
the grant's on approval. A `ctl` with a record (for example a revoked one) is
always windowed by the record. Because pending records are not persisted, a
captured `requestAccess` can be replayed after a restart within its
`exp + 30`; that only re-creates a pending request whose reply the attacker
cannot read.

## 7. Authorization

### 7.1 Command tiers

Unknown commands fail closed: they are owner-only.

| Tier | Commands |
|---|---|
| owner | everything not listed below; specifically `sshAccess`, `factoryReset`, `startWifiSetup`, `updateToLatestVersion`, `connect`, `setDeviceName`, `showPairingQRCode`, `startMintPairingSession`, `joinMintPairingChannel`, `closeMintPairingSession`, `mintPairingApprovalDecision`, `setContentPolicy`, `analyticsToggle`, `betaFeaturesToggle`, `uploadLogs`, `sendKeyboardEvent`, `tapGesture`, `doubleTapGesture`, `longPressGesture`, `dragGesture`, `clickAndDragGesture`, `zoomGesture`, `cursorUpdate`, `disconnect`, `approveController`, `revokeController`, `listControllers` |
| controller | playback: `displayPlaylist`, `displayDefaultPlaylist`, `refreshArtwork`, `refreshPlaylist`, `checkStatus`, `moveToArtwork`, `updateIndex`, `setShuffle`, `setLoop`, `updateArtFraming`, `updateDisplaySettings`, `updateDefaultDuration`, `getRecentlyPlayed`, `playRecentlyPlayed`; offline cache: `downloadPlaylistItem`, `downloadPlaylist`, `clearPlaylistItemCache`, `clearPlaylistCache`, `getOfflineCacheStatus`; device: `getDeviceStatus`, `deviceMetrics`, `rotate`, `setVolume`, `toggleMute`, `ddcPanelControl`, `ddcPanelStatus`, `setSleepSchedule`, `setSleepMode`, `sleepNow`, `wakeNow`, `reboot`, `shutdown`, `runNetworkDiagnostics`, `getContentPolicy` |
| internal | never accepted from any external principal, owner included (`permission_denied`): `resolveRecentlyPlayed` (router-internal), `ping`, `mintPairingDisplay`, `setupDisplay` (controld → player paint/probe commands that would otherwise fall through to the player over CDP), and `ff1Envelope` (an envelope inside an envelope) |

The names are the real catalog: `commands/types.go`, the literal names
`commandrouter` special-cases, and ff-player's `CastCommand` plus
`CDPRequestHandler` commands. `pause`, `resume`, `pauseCasting`,
`resumeCasting`, `nextArtwork` and `previousArtwork` exist in neither controld
nor the player (ff-app's legacy cast enum still names some of them; the
player answers them `{ok:false}`), so they are unknown and owner-only.

The implementation MUST keep this list in one table in code
(`control/tiers.go`). A test cross-checks it against every command constant
in `commands/types.go` (read from source), every literal command name in
`commandrouter`, and the player command list, and fails on a missing or a
phantom entry, so a new command cannot ship without a tier.

### 7.2 Device states and plaintext policy

| State | Condition | Plaintext on LAN `/api/cast` | Plaintext via relayer | Envelopes |
|---|---|---|---|---|
| `legacy` | no active owner grant | allowed (phase-0 rules apply) | allowed | `enroll` (with inv); `requestAccess` and `enroll` without inv → `not_approved` |
| `enrolled` | ≥1 active owner grant, `control.enforced=false` | refused: `enrollment_required` | controller tier only; anything else → `envelope_required` | full |
| `enforced` | ≥1 active owner grant, `control.enforced=true` | refused: `enrollment_required` | only the exact Play shape `{"command":"displayPlaylist","request":{"intent":{"action":"now_display"},"dp1_call":{…}}}`: `request` has exactly the members `intent` and `dp1_call`, `intent` is exactly `{"action":"now_display"}`, `dp1_call` is an object; anything else → `envelope_required` | full |
| `locked` | identity or grant store unreadable/corrupt | refused: `locked` | controller tier only, else `locked` | refused: public `locked` |

"LAN" means any caller of the hub (`/api/cast`), whatever network it came
from: Wi-Fi, Ethernet, an owner VPN or a Tailscale address all get the LAN
column. Only the relayer path gets the relayer column.

Internal callers (OOM recovery, `playRecentlyPlayed` re-entry) are not subject
to this policy. An unrecognized state is treated as `locked`.

## 8. Device state on disk

Directory: `/home/feralfile/.state/control/`, mode 0700. Files are mode 0600.

- `identity.json`: `{"v":1,"gen","sig":privateJWK,"enc":privateJWK,"createdAt"}`
  (`deviceId` is not stored; it is the hostname). A private JWK has exactly
  `kty`, `crv`, `x`, `y`, `d`, and `d` MUST reproduce `x`,`y`.
- `grants.json`: `{"v":1,"gen","controllers":[{"ctl","sig","enc","role","label","platform","createdAt","approvedBy","expiresAt"?,"revokedAt"?,"nk","hi","bitmap"}]}`
  - `approvedBy` is `"invitation"` or the approving owner's `ctl`;
  - `nk` is `b64u(32)`;
  - `bitmap` is the §6 bitmap as 16 lowercase hex digits (a JSON number would
    lose bits above 2^53 in non-Go readers).

Rules:
- A missing directory on start means create a new identity, in state `legacy`.
  The files are written into `control.new/` and the directory is renamed into
  place, so a crash never leaves a half-created directory. A leftover
  `control.new/` is discarded.
- A present but unreadable or inconsistent directory means state `locked`.
  Inconsistent includes: a `gen` mismatch, a duplicate or unknown member, a
  record whose `ctl` is not `thumb(sig)`, a duplicate `ctl`, an invalid role,
  platform, label or `approvedBy`, a replay window that violates §6 (`hi = 0`
  with bits set, `hi > 0` without bit 0, bits for counters below 1). It never
  becomes `legacy` automatically, and the files are left untouched.
- Every write goes through the same checks first, so the device cannot persist
  a file it would lock on.
- Factory reset deletes the directory before it starts the reset unit.
- OTA carries the directory live: see ffos `feral-system-update.sh`.

## 9. Sealed notifications (FF1 → one controller)

Each active grant has `nk` (32 bytes), issued at enrollment (and re-issued on
re-approval).

A sealed notification is a compact JWE:
- protected header exactly
  `{"alg":"dir","enc":"A256GCM","typ":"ff1-ntf+jwe","kid":"<ctl>"}`;
- key `nk`;
- plaintext claims, exactly `{"v":1,"dev","gen","ctl","ep","seq","iat","type","body"}`.
  The controller MUST check that `ctl` equals the header `kid` and its own
  `controllerId`, and that `dev`/`gen` match its pins.

`ep` is `b64u(8 random bytes)`, drawn once per daemon run (not persisted).
`seq` starts at 1 per controller per run and increases per notification.
Controllers order notifications by (`ep`, `seq`): within one `ep` they keep
the highest `seq` and ignore older ones; a notification with an `ep` they
have not seen starts a new run and is accepted.

Notifications are **not signed**: they are authenticated only by `nk`, which
the controller itself holds. A controller MUST NOT take any action
automatically because of a notification (approve a request, change a
setting, send a command). Notifications only inform UI; every action is a
user-initiated, signed request.

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
  The presence of `control` is the capability signal. A `locked` device omits
  `profile`: it cannot vouch for an identity it could not read.
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
- `keys.json`: the fixed device (`device`: `dev`, `gen`, private `sig`/`enc`,
  `profile`, `pins`) and four controllers (`controllers.owner`, `.controller`,
  `.revoked`, `.stranger`) with private `sig`/`enc`, `ctl`, `label`,
  `platform`, and for the first three `role`, `nk`, `createdAt` (and
  `revokedAt`); `stranger` has no grant. `now` is the fixed clock.
- `positive/*.json`, `kind` `request` or `notification`. A request vector has
  `setup` (below), `from` (the signing controller), the exact signed
  `payload` and its parsed `claims`, the fixed `ephemeralKey` (private JWK)
  and `iv`, the inner `jws`, the outer `compact`, `rk`, the device's
  verdict in `expect` (`execute`, `command`, `principal`, `replyOk`,
  `replyCode`), the device's `reply` (`compact` and decoded `claims`) and any
  `notifications` it emitted (`to`, `compact`, `claims`, `wire`).
- `positive/approval-code.json` (`kind:"approvalCode"`): `cases` of
  `{ctl, sigPin, code}` for §5.4, the last one with a non-device pin.
- `negative/*.json`:
  - `kind:"request"`: a `compact` (or `compactRepeat` `{char,count}` for the
    oversize vector, built by repeating `char`) plus `setup`, and either
    `expect.public` (the §4.4 code) or `expect.replyCode` (open the device's
    reply with the vector's `rk`). Covered: tampered tag, wrong JWE `kid`,
    wrong `dev`, wrong `gen`, expired, replayed `ctr`, out-of-window `ctr`,
    DER signature, extra and duplicate header members, `alg` `none`/`RS256`
    (JWS) and `RSA-OAEP-256` (JWE), off-curve `epk`, oversize body, forged
    signature, unknown `kid`, revoked controller, `exp - iat > 120`,
    `ctl ≠ kid`, owner-tier command from a controller.
  - `kind:"reply"` / `kind:"notification"`: vectors a controller MUST reject:
    wrong `rk`, tampered, signed by a key other than the pinned `devSig`,
    unbound `rid` (`bind` gives the dev/gen/ctl/rid the controller sent), extra
    header member, DER signature, wrong `nk`, header `kid` ≠ claims `ctl`.

`setup` describes the device a request vector is opened against: `now`,
`grants` (which `keys.json` controllers are in `grants.json`), optional
`windows` (`{name:{hi,bitmap}}`), `invitation` (issue one invitation at
start), and `randSeed`, the 32-byte seed of the device's ChaCha8 stream (Go
`math/rand/v2`) that supplies reply IVs, `nk` and the invitation.

Determinism: every key, IV and `rk` is derived from a fixed label, JWS
signatures are RFC 6979 deterministic, and the device's randomness is the
seeded stream, so `go run ./control/cmd/genfixtures` (from
`components/feral-controld`) reproduces every file byte for byte; a Go test
fails when the committed files are stale. A client implementation MUST
decrypt and verify every positive vector and compare the decoded claims; it
reproduces `compact` byte for byte only if its signer is RFC 6979
deterministic (WebCrypto is not), so byte equality is optional for the
request JWS and required for nothing else on the client side.

Fixtures MUST also round-trip through a second, independent implementation:
the Go tests cross-check every profile against go-jose v4 in both directions,
and the ff1-cli test suite does the same with `jose`.
