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
     `thumb(enroll.sig) == kid`. A payload from an unknown `kid` larger than
     16 KiB is `unknown_controller` before its JSON is read (enrollment
     claims are a few hundred bytes).
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
   persist the replay window **before** executing. A request refused here
   (`permission_denied`, `access_revoked`, `not_approved`, `rate_limited`,
   `invitation_invalid`, and the `enc`-mismatch `invalid_request` of §5.3)
   spends its `ctr` too, wherever a window exists (§6), so a captured
   refused envelope never becomes live after a later promotion or
   re-approval.

Failures in steps 1–3 produce a public error (§4.4). Failures from step 4 on
produce an encrypted reply whose `ctl` is the verified JWS `kid` (even when
the request's `ctl` claim differs).

Code mapping for step 4: any member, shape, `v`, `dev`, `gen`, `ctl`,
lifetime or future-`iat` failure is `invalid_request`; `now > exp + 30` is
`expired`. Step 5 failures are `replay`. A future `iat` is the only step-4
failure a later clock could turn valid, so it is checked against the window
after step 5 and spends `ctr` like a step-6 refusal; the other step-4
failures can never become valid and spend nothing.

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
`invitation_invalid`, `pending`, `not_approved`, `rate_limited`,
`command_failed`, `internal_error`. (`locked` is only ever a public error,
§4.4: a locked device has no trusted identity to sign a reply with.)

A command the device executed answers `ok:true` with the handler's reply as
`res`, even when that reply itself says `"ok":false` (a player refusal is a
result, not a protocol failure); the controller reads `res` exactly as it
reads a legacy relayer reply.

For `command_failed` (the handler raised an error instead of replying),
`res` carries the error body the relayer path returns for it today:
`{"ok":false,"error":"<id>","command":"<name>","message":"<text>"}` with
`<id>` one of `rate_limited`, `sourceUnreachable`, `contentBlocked`,
`playlistInvalid`, or `failed` for anything else. The owner commands of §5.5 use `res` = `{"error":"<reason>"}` with
`<reason>` one of `invalid_request`, `not_pending`, `not_found`,
`last_owner`, `too_many_controllers`, or `internal_error` (no entropy for
a new `nk`; the grant store is unchanged, but the replay window was
written).

`internal_error` means the device could not confirm a durable write.
- For a command, the replay window (§6) was not confirmed and the command
  was **not** executed. Its `ctr` may or may not be consumed, so the
  controller retries with a new `ctr`. (Both released-branch clients
  currently surface it to the user instead of retrying, except ff1-cli on
  the Play path.)
- For a control-plane operation (enroll, requestAccess, approve, register,
  revoke),
  the write may have been **applied** (renamed into place but its directory
  fsync failed); the device's memory follows it. The client MUST re-list
  (`listControllers`) or re-enroll, which is idempotent, before retrying,
  and must not assume the operation failed. Retrying an applied approval
  does not rotate `nk`.

`access_revoked` covers both a revoked and an expired grant.

### 4.4 Public errors

These are sent when no reply key can be trusted. The body is
`{"error":{"code":"<code>"}}` with one of these codes, and never any other
detail (a cause string would make the device an oracle). The one exception
is a fixed, code-determined `message` on the two plaintext refusals,
`enrollment_required` and `envelope_required`, telling an out-of-date app or
ff-cli to update: `{"error":{"code":"…","message":"this FF1 needs signed
requests: update the Feral File app or ff-cli"}}`. Clients branch on `code`
only and MUST accept the extra member.

| Code | When |
|---|---|
| `bad_envelope` | §3.4 step 1 or 3 failed, the JWS is structurally malformed, or the device could not seal any reply (no entropy) |
| `unknown_controller` | step 2: `kid` not found |
| `bad_signature` | step 2: signature did not verify |
| `enrollment_required` | plaintext refused, §7.2 |
| `envelope_required` | owner-tier plaintext on the relayer |
| `locked` | §8; checked before step 1 |
| `invalid_message` | plaintext whose top level repeats a member name, exactly or in another letter case (§7.2) |
| `screen_protected` | `GET /api/screenshot` while an invitation is live (§7.2) |
| `busy` | too many envelopes being authenticated at once (§10); retry |

On the LAN these come back as HTTP 401, except `envelope_required`,
`enrollment_required` and `screen_protected`, which come back as 403,
`invalid_message`, which comes back as 400, and `busy`, which comes back as
503. On the relayer an envelope's
public error is the RPC `message`, `{"error":{"code":"<code>"}}`.

A **plaintext** command refused on the relayer (§7.2) is answered with a
different shape, because the clients that send plaintext are the released
ones: `{"ok":false,"error":"<code>","command":"<name>","message":"<text>"}`.
`ok:false` and a flat string `error` are load-bearing: released ff-app reads
`error` as a string (an object crashes its reply parsing into a silent
failure), and released ff1-cli and Play treat any reply without `ok:false`
as success.

## 5. Enrollment

`enroll` object, exactly: `{"sig":JWK,"enc":JWK,"label":string(1..64 characters),"platform":"ios"|"android"|"cli"|"other","inv"?:string}`.
A `label` containing a C0/C1 control character or an invisible formatting
or bidi character (the set the device strips from device names:
U+061C, U+200B-U+200F, U+202A-U+202E, U+2060, U+2066-U+2069, U+2028, U+2029,
U+FEFF) is `invalid_request`: an owner must not be shown one controller's
label rendered as another's.
`inv` is allowed only with `op=enroll`. `thumb(enroll.sig)` MUST equal `ctl`
(else `invalid_request`).

### 5.1 `op=enroll` with `inv`

The first owner.

- Valid only in state `legacy` (no active owner grant); otherwise the reply is
  `invitation_invalid`.
- Refused with `invitation_invalid` on the device-ID relay (`RouteRelayV2`),
  before the invitation is compared, so v2 is no oracle for it. The LAN and
  the legacy relayer keep it: the on-screen invitation is the security, and
  the legacy relayer is how a claimed 2.0.x device off the owner's LAN (or
  on client-isolated Wi-Fi) is migrated ("Secure this FF1", then scan the
  screen). On v2 a first owner has no way to seal anyway: there is no
  plaintext profile read (§7.2).
- `inv` is the current invitation: `b64u(16 random bytes)`, single use, 10-minute
  life, compared in constant time. There is **no attempt limit**: 128 random
  bits need none, and voiding the invitation after some number of misses
  would let any LAN peer block first-owner and migration enrollment forever.
  A wrong value is `invitation_invalid` and changes nothing but the spent
  `ctr` of a `ctl` that has a record or a pending request (§3.4 step 6).
  Issuing a new
  invitation replaces the old one.
- A client MUST enroll with the invitation **before** it sends `connect` on
  an unclaimed device. `connect` claims the device on the relayer, which takes
  the claim QR down and with it voids the invitation (below); a client that
  connects first has to have the QR shown again to enroll.
- A new invitation is minted only when a claim QR carrying it is actually
  painted, and voided at once if the paint does not take. A repaint after the
  QR was hidden or replaced therefore carries a new invitation; a repaint of
  a QR that is still on screen reuses its invitation unless less than
  2 minutes of it remain.
- The invitation lives only as long as the QR that carries it: it is voided
  whenever that QR is hidden or replaced (pairing ended with
  `showPairingQRCode{show:false}`, a legacy relayer claim, another narration
  taking the panel, or a repaint without the invitation fields). The device
  checks at least every 15 s that the QR is still on screen.
- On success: create an owner grant (`approvedBy` = `"invitation"`), consume
  the invitation, and remove the claim QR.
- `res` = `{"role":"owner","nk":b64u(32),"profile":profile}`.
- After `internal_error` the grant may have been written and the invitation
  spent anyway (§4.3). The first owner retries `enroll` with a new `ctr`,
  with or without the same `inv`: from the `ctl` that holds the
  invitation-approved grant with the same `enc` either form returns that
  grant (§5.3); anyone else gets `invitation_invalid`. A client recovers
  with that idempotent `enroll`.

### 5.2 `op=requestAccess`

A later controller asks for access. **This is a LAN-only fallback**: the
default way to add a controller is owner-initiated registration (§5.6), and
clients no longer offer `requestAccess`.

- In state `legacy` the reply is `not_approved`: there is no owner to approve,
  and the first controller uses the invitation instead.
- If `ctl` already holds an active grant: `ok:true`,
  `res` = `{"status":"approved","role":role}`, no notification.
- Off the LAN (the legacy relayer, the v2 relay) a `ctl` without an active
  grant gets `not_approved` and creates or refreshes nothing; the `ctr` of a
  known record is spent. Off the LAN a requester's only pins would come from
  the reply path it is asking to be approved over, so the approval code
  (§5.4) would bind nothing.
- If an owner denied `ctl` (§5.5 `revokeController` on its pending request),
  the reply is `not_approved` until the moment the denied request would have
  expired.
- Otherwise it creates or refreshes a pending request for `ctl`. Every limit
  below answers `rate_limited` and changes nothing but the spent `ctr`
  (§3.4 step 6):
  - At most 4 pending requests in total.
  - At most 1 **new** request per 10 s from any source (a refresh is not new).
  - Counted over the other pending requests: at most 1 pending request per
    LAN source. A LAN source is
    the peer's IPv4 address (IPv4-mapped included), the /64 of a global or
    ULA IPv6 address (one host can hold a whole /64), or, for every
    link-local IPv6 address (`fe80::/10`, any zone), one single source. The
    source is used only for this count; it is never authority. The hub
    listens on IPv4 only today, so the IPv6 rules are dormant; on a
    dual-stack hub, SLAAC would make a whole LAN one /64 source with a cap
    of one pending request.
  - A refresh of an existing pending `ctl` at most once per 10 s.
  - A pending request lives for 5 minutes from its last refresh, and never
    more than 10 minutes from its creation, however often it refreshes.
    A pre-approval `enroll` does not refresh it: a client that waits longer
    than 5 minutes for approval MUST re-send `requestAccess` before
    `expiresAt`, ignoring a `rate_limited` refresh.
  - Pending requests and denials are held in memory only. A restart drops
    them and the requester asks again.
  - Residual: an attacker who controls many LAN source prefixes can still
    fill the 4 global slots for up to 10 minutes at a time. Every such
    request is owner-visible (a sealed `controller_approval_request` with
    its label, platform and code), the owner can deny each one, and a full
    table only delays a legitimate requester, never grants anything.
- `res` = `{"status":"pending","code":approvalCode(ctl, thumb(devSig)),"expiresAt":t}`.
- On create, every active owner gets a sealed `controller_approval_request`
  notification (§9), with body `{"ctl","label","platform","code","expiresAt"}`.
  A refresh re-notifies only when the label or platform changed, so an owner
  UI that closes its prompt at the first notification's `expiresAt` must
  still list the request (`listControllers`) for its whole life, up to 10
  minutes.
- `not_approved` means either "no owner yet" (`legacy`) or "an owner denied
  this `ctl` recently"; a client tells them apart by the status
  `control.state`, not by the code.

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

It does **not** defeat one that sends its own request instead. The code is
about 20 bits; after reading the phone's `ctl` and label from the request
sealed to its fake key, the attacker generates keys until
`approvalCode(ctl_attacker, realPin)` equals the code the phone shows (about
a million tries, seconds on one core), then asks with the phone's label. The
owner sees one matching request. A longer code does not help against this
offline search. So the code protects only a requester whose pin came from
the screen: the claim QR carries the pins in every state (§11) and a second
phone MUST pin from it (§14.1).

### 5.5 Owner commands

These are `op=cmd`, owner tier, executed by the control authority itself (they
never reach the command router):

| Command | `req` (exact members) | `res` |
|---|---|---|
| `approveController` | `{"ctl","role":"owner"|"controller","expiresAt"?}` | `{}` |
| `revokeController` | `{"ctl"}` | `{}` |
| `registerController` | `{"sig":JWK,"enc":JWK,"label","platform","role":"owner"\|"controller","expiresAt"?}` | `{"ctl","role","expiresAt"?}` |
| `releaseOwnership` | `{}` | `{"released":true}` |
| `listControllers` | `{}` | `{"controllers":[{"ctl","role","label","platform","createdAt","expiresAt"?,"revokedAt"?,"self":bool}, …, {"ctl","role":"display","label","origin","originAttested":bool,"platform":"web","createdAt","expiresAt"?,"revokedAt"?}],"pending":[{"ctl","label","platform","code","expiresAt"}]}` |

- `approveController` with a live pending request for `ctl` grants it: the
  grant takes its keys, label and platform from that request. Without one it
  is a role (and expiry) change of an **active** grant in place: keys, `nk`,
  window and `createdAt` are kept and `approvedBy` becomes the approver. With
  neither (unknown or revoked `ctl`, nothing pending) it fails `not_pending`.
  Demoting an owner fails `last_owner` when no other active owner remains.
  `expiresAt` must be in the future and is allowed only for
  `role:"controller"`: an owner grant never expires, so the last owner cannot
  silently lapse. A pending-request approval of a `ctl` that already has a
  record (a revoked one) updates that record in place: it keeps its
  `createdAt` and its replay window and gets a fresh `nk`. A role change
  without a pending request keeps `nk` too. A role change that omits
  `expiresAt` clears any expiry the grant had: the request states the whole
  grant, so a client that wants to keep an expiry sends it again. At most 32 active grants and 128
  records (revoked ones included) exist (`too_many_controllers`). At the
  record cap, a new grant evicts the record that has been inactive (revoked
  or expired) longest, once it has been inactive for more than 450 s
  (`iat` up to 300 s ahead + `exp - iat` up to 120 + 30 grace: no envelope
  it signed can still be accepted); active records are never evicted.
- `revokeController` on a `ctl` that has only a pending request denies (drops)
  that request; further `requestAccess` from that `ctl` answers
  `not_approved` until the denied request would have expired (§5.2).
  An expired grant is revoked durably (it gets `revokedAt`, so a wall-clock
  step back cannot revive it). Otherwise the grant must be active
  (`not_found`).
- `registerController` is owner-initiated registration (§5.6.4).
- `releaseOwnership` gives the device up (the app's Forget I exist). A first
  write revokes every display record (§5.7) that has no `revokedAt`; then one
  write sets `revokedAt` on every grant record that has none, owners (the
  signer included) and controllers alike, expired ones too, as
  `revokeController` does, and every pending request and owner deny is
  dropped. Display records go first because after the grant write the owner
  can no longer retry; a failed display write releases nothing.
  The device is `legacy` (§7.2) and still claimed: identity, claim and topic
  are unchanged. Every former holder, the signer included, gets
  `access_revoked` for its next request; a client that retries after an
  `internal_error` counts a verified `access_revoked` as success, because
  the device runs the release side effects once the grant write commits,
  even when the reply is `internal_error`. Those side effects are: the
  device closes SSH, resets owner Tailscale access, and paints the claim QR
  with a fresh invitation (§5.1; not timed, and memory only: after a reboot
  `showPairingQRCode{purpose:"enroll"}` paints it again). A new owner,
  including a former controller with the same keys, enrolls with it; the
  new grant replaces the revoked record.
- `listControllers` lists every record, revoked ones included: the
  `grants.json` records first, in their shape, then every display record
  (§5.7) in the display shape (`role:"display"`, `origin`,
  `originAttested`, `platform:"web"`, no `self`). Clients MUST tolerate both
  shapes and the `display` role in one array.
- `revokeController` on a `ctl` held by the display store revokes that
  display record (§5.7): two writes, the owner's window in `grants.json`
  first, then `display_grants.json`; if the second fails the reply is
  `internal_error` and the owner re-lists. An already revoked display
  record is `not_found`.
- `approveController` and `registerController` accept only
  `role:"owner"|"controller"`: `"display"` is `invalid_request`, and so is
  a `ctl` that the display store holds (a `ctl` is never in both stores).

An owner cannot revoke the last active owner (`last_owner`). That would make
the device unowned without a reset. An owner may revoke itself while another
active owner exists. `releaseOwnership` is the one exception: it makes the
device unowned on purpose.

**Self-revoke.** `revokeController` whose `ctl` is the signer's own is the
one owner-tier form a **controller** grant may send: a controller that
removes an FF1 (app Remove, `ff-cli device unpair`) revokes itself first,
best effort, so no orphan grant is left behind. Any other `ctl` from a
controller is `permission_denied`. A sole owner revoking itself still fails
`last_owner`.

### 5.6 Owner-initiated registration

The default way to add a controller (a second phone, ff-cli, a browser): the
controller shows its keys, and an owner who scans them registers them on the
FF1. Like adding an SSH key, nothing is requested from the device by the
newcomer and no approval code is compared.

1. The controller generates its `sig`/`enc` keys, an ephemeral P-256
   **minter key**, a 16-byte pairing secret `ps`, and a pairing
   `channelId` (`crypto.randomUUID()`). It joins that channel on the relay
   carrier (`/v2/channels/{channelId}/connections`) and shows the
   controller QR (§5.6.1) on its screen or terminal, plus the same string
   for pasting.
2. The owner's app scans it, picks the FF1, the role and an optional expiry,
   and sends `registerController` (§5.6.4) under its owner grant.
3. Only after a **verified** `ok:true` reply whose `res.ctl` is
   `thumb(QR sig)` does the owner's app join the channel and send the sealed
   result (§5.6.2) to the controller's connection. If the owner declines, it
   sends `outcome:"rejected"`.
4. The controller opens and checks the result (§5.6.3), pins `profile`, and
   sends a protected `op=enroll` without `inv` (§5.3), which returns its
   role, `nk` and expiry. It shows the FF1 as ready only after its own
   verified enroll.
5. The controller enforces its own timeout: the relay keeps nothing. A
   timeout means "not completed", never "rejected"; the grant, if the owner
   registered one, stays and a later retry with the same keys completes it.

A leaked QR lets others join the channel, see ciphertext or disrupt the
exchange, but not forge a result: forging needs `ps`, which is only in the
QR and inside the ciphertext. A QR image forwarded to a remote owner through
a chat app carries `ps` through that app; that is the accepted residual
(§13).

#### 5.6.1 Controller QR

`ff1-ctl:1|<channelId>|<minterKey>|<sig.x>|<sig.y>|<enc.x>|<enc.y>|<ps>|<platform>|<label>`

- `channelId`: 16–64 characters of `[A-Za-z0-9_-]` (a UUID fits). The
  relay accepts 1–128, so the controller generating it and the owner
  parsing it both check this range.
- `minterKey`: `b64u` of the 65-byte uncompressed SEC 1 point
  (`0x04‖X‖Y`) of the minter public key. Compressed or off-curve points are
  invalid.
- `sig.x` … `enc.y`: the JWK `x`/`y` coordinates (`b64u` of 32 bytes); each
  pair MUST be a point on P-256.
- `ps`: `b64u` of exactly 16 bytes, unpadded.
- `platform`: as in §5.
- `label`: the rest of the string, as in §5 (1–64 characters, no control or
  formatting characters). It is last so it may contain `|`: a parser splits
  the part after the prefix **at most 9 times**.
- A parser MUST reject anything else. The QR is about 350 bytes; at error
  correction level H it is QR version 20 or below.

#### 5.6.2 Sealed registration result

Plaintext (the canonical member order shown; extra members are invalid):

`{"v":1,"type":"ff1_controller_registration","channelId","ps","ctl","outcome":"approved"|"rejected","profile"?,"deviceName"?,"role"?,"expiresAt"?,"topicId"?}`

- `channelId`, `ps`, `ctl` echo the QR (`ctl` = `thumb(QR sig)`).
- `outcome:"approved"` requires `profile` (§2, exactly
  `{"v","dev","gen","sig","enc"}`), `deviceName` (1–64 characters, label
  rules) and `role`; `expiresAt` only for `role:"controller"`. `topicId`
  is the FF1's legacy relayer topic when the owner's record has one, so the
  new controller can use the protected legacy route until the FF1 is on
  v2; it is dropped once firmware is v2-only.
- `outcome:"rejected"` carries none of the optional members.

Wire message, sent as the relay frame's payload:
`{"v":1,"type":"ff1_controller_registration_sealed","epk":b64u(SEC1 65),"iv":b64u(12),"ct":b64u(ciphertext‖tag)}`

- The owner generates a fresh P-256 key `epk` per message.
- `Z` = ECDH(epk, minterKey), the 32-byte x coordinate.
- `salt` = SHA-256(UTF-8 `"ff1-ctl:1|" + channelId`).
- `K` = HKDF-SHA256(IKM `Z`, `salt`, info UTF-8
  `"ff1-controller-registration/v1"`, 32 bytes).
- `AAD` = UTF-8 `"ff1-controller-registration/v1|" + channelId + "|" +
  minterKey + "|" + epk` (the `b64u` strings as they appear in the QR and the
  message).
- `ct` = AES-256-GCM(`K`, `iv`, plaintext, `AAD`), 128-bit tag appended.

#### 5.6.3 What the controller checks

In order, rejecting the message on any failure (and keeping the channel
open for a genuine one until its timeout):

1. The wire message is exactly the five members, `v` 1, the sealed type,
   a valid `epk`, a 12-byte `iv`; it decrypts under the derived key and AAD.
2. The plaintext is exactly the members above, `v` 1, the result type.
3. `channelId` is its own; `ps` equals its own (constant time); `ctl` is
   `thumb` of its own `sig`.
4. For `approved`: `profile` parses; when the user named a device (ff-cli
   `--device`), `profile.dev` matches it; `deviceName` and `role` are valid;
   `expiresAt`, if present, is in the future and the role is `controller`.
5. Only then: pin `profile`, store `topicId` if present, and enroll (§5.3).
   An unsolicited result never replaces a controller's existing grant
   record for another device or generation.

#### 5.6.4 `registerController`

Owner tier. `req` = `{"sig":JWK,"enc":JWK,"label","platform","role","expiresAt"?}`
with the §5 key, label and platform rules; `ctl` = `thumb(sig)`. It is
idempotent per `ctl`:

- No record: a new grant (`approvedBy` = the owner, a fresh `nk`). If the
  same `ctl` has a live LAN `requestAccess`, the grant takes that request's
  replay window, so its spent `ctr`s stay spent.
- An **active** grant: role, expiry, label and platform are replaced; keys,
  `nk`, window and `createdAt` are kept.
- A **revoked or expired** record: reactivated with a fresh `nk`, keeping its
  window and `createdAt` (§6), so its old envelopes stay replays.
- A record whose `enc` differs from `req.enc`: `invalid_request`. `sig` is
  pinned by `ctl`, and a second `enc` for it is never swapped in silently.
- Caps, eviction, the owner-expiry rule and `last_owner` (demoting the last
  owner, including the caller registering itself as a controller) are as for
  `approveController`.
- It clears a pending `requestAccess` and a standing deny for the same `ctl`.
- `approveController` keeps its two uses: granting a pending LAN request and
  changing an active grant's role in place (§5.5). Adding new keys is
  `registerController`.

### 5.7 Display grants

A **display grant** is what a Play site (`@feralfile/play` 0.5 and later)
holds after an owner approved it through mint pairing: a display-only
controller record, issued by the FF1, that the site uses to cast as sealed
envelopes over the v2 relay. It replaces the relayer session token of
earlier Play versions (§15.3). The approval UX is the mint pairing one, in
every control state but `locked`: `legacy` included, where the device
accepts the decision from whoever it accepts mint pairing decisions from
today, and from an active owner's sealed `mintPairingApprovalDecision`
otherwise.

**Store.** Display records live in their own file, `display_grants.json`
(§8), never in `grants.json`. The role `display` and the platform `web`
exist only there: the role parsers of `approveController`,
`registerController`, `enroll` and the controller QR keep accepting
`owner|controller`, so the role is never wire-assignable. A file that cannot
be trusted (unreadable or invalid) disables display admission
and issuance only; it never locks the device. One written under another `gen`
is discarded as empty (§8). A controld that predates
this section ignores the file.

Record: `{ctl, sig, enc, origin, label, platform:"web", originAttested,
createdAt, approvedBy, expiresAt?, revokedAt?, window}`. `ctl` =
`thumb(sig)`; `origin` is the site's full origin (≤ 2 KiB);
`originAttested` is true when the broker attested the origin (a site-created
channel the FF1 joined) and false on a device-initiated channel; `label` is
the origin's host, punycoded and cut to the §5 label rules; `approvedBy` is
the approving owner's `ctl`, or `""` when the grant was issued in `legacy`;
`expiresAt` 0 means kept until removed. There is no `nk`: a display grant
is never bound (§9) and gets no notification.

Caps: at most 16 active display records and 64 records, with the same
eviction as `grants.json` (§5.5). Over the active cap the request is
refused `too_many_sites`. Issuing a grant revokes any other active display
record with the same `origin` **and** the same `originAttested`, so a
re-pair keeps one active grant per origin; an asserted origin never evicts
an attested one. A `ctl` is never in both stores: issuance refuses a `ctl`
that `grants.json` or the display store already holds (pairing keys are
fresh, so a repeat is never legitimate), and every insert into
`grants.json` refuses a `ctl` the display store holds (`invalid_request`).

**Wire (mint pairing).** The site sends
`"displayGrant": {"v":1,"sig":<P-256 public JWK>,"enc":<P-256 public JWK>}`
in both its clear create-call `mintRequest` (the announced request, which
the FF1 reads when it joins a site-created channel) and its encrypted
`mint_request`. The presence of the member, well formed or not, marks a 0.5
request; a request without it is answered with a session exactly as before.
When the FF1 answers the announced request those keys are used alone; when
it holds both copies they MUST be identical (keys in only one of them is a
mismatch too), else no grant is issued and no session either. A 0.5 request
the FF1 cannot honor at all (no control authority or no v2 endpoint on
this FF1, keys that do not parse or disagree) is refused
`display_grant_unavailable` **before** the owner is asked; the same checks
run again at issuance. The
approval request the owner app receives carries `"grantsDisplayAccess":true`
for a 0.5 request. On approval the FF1 issues the grant **before** the
result is sent and answers `mint_succeeded` with a top-level

```json
"displayGrant":{"v":1,"ctl":"…","profile":{"v":1,"dev":"FF1-…","gen":"…","sig":JWK,"enc":JWK},"relayV2Endpoint":"wss://…","expiresAt":"<RFC 3339>"}
```

and **no** `session` (`expiresAt` absent = kept). The expiry follows the
owner's decision as a session's would: timed → now + the effective session
TTL; kept → none; kept for a requester that did not declare
`supportsPersistentSessions` → the longest timed life. A 0.5 request never
gets a session. It is refused `mint_rejected` with reason `too_many_sites`
(the cap) or `display_grant_unavailable` (malformed or mismatching keys, no
v2 endpoint or no control authority on this FF1, a locked device, or a
write the FF1 could not confirm durable; in that last case the committed
record is revoked best-effort first and the rejection is the only
`retryable` one). The outcome notification to the owner
carries `ctl` on completion and the same `reason` on a refusal. The grant is
revoked wherever a session would have been: a claim or pairing superseded
before delivery, or a delivery the broker provably refused. A claim change
never revokes other display grants.

The approver is read where the decision arrives (the sealed envelope's
principal; none in `legacy`) and re-checked at issuance under the
authority's lock: an empty approver is accepted only while the device is
still `legacy`, otherwise the approver must be an active owner, because a
first owner can enroll between the decision and the issuance.

**Authorization.** A display `ctl` is looked up after `grants.json` (§3.4
step 2) and is admitted in every state but `locked` (§7.2), `legacy`
included, with its own replay window persisted in `display_grants.json`
before execution (§6). It may send exactly two things:
`op:"cmd"`, `cmd:"displayPlaylist"` whose `req` is the exact Play shape of
§7.2 (`isPlayCastShape`), executed as the principal
`{controllerId, role:"display"}`; and its own self-revoke
(`revokeController{ctl:<own>}`, the site's unpair). Every other command,
every other `req` shape, and every non-`cmd` op (`enroll`,
`requestAccess`) is `permission_denied` with the `ctr` spent. A revoked or
expired display record is `access_revoked` whatever it sent, checked before
what it asked for (after the replay check and a future `iat`). The command
router re-checks
the same rule (`control.Permits`) before any handler runs. No status read,
no `connect`, no notifications: `GrantActive`, `Seal`, `SealAll`, the v2
binding layer and the owner count never see display records.

**Site rules** are in §14 (browser). A site that moves to 0.5 re-pairs
once; a 0.5 site on firmware without this section receives a session it
cannot use and reports `device_unsupported`.

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
An authenticated refusal after the window check (§3.4 step 6) is persisted
the same way before its reply; a `ctl` with neither a record nor a pending
request has no window to spend, and a replay of its refused request is no
more than any peer can send with a fresh key.

A revoked record keeps its window. Re-approval keeps it too.

A controller whose counter is behind the device's window (a restored backup
or a synced config directory) gets a verified `replay`. The window has no
upper bound, so it MAY recover by jumping its `ctr` forward; it MUST NOT
delete a working grant over it (ff1-cli advises re-pairing, never unpair).

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
| owner | everything not listed below; specifically `sshAccess`, `factoryReset`, `startWifiSetup`, `updateToLatestVersion`, `setDeviceName`, `showPairingQRCode`, `startMintPairingSession`, `joinMintPairingChannel`, `closeMintPairingSession`, `mintPairingApprovalDecision`, `setContentPolicy`, `analyticsToggle`, `betaFeaturesToggle`, `uploadLogs`, `sendKeyboardEvent`, `tapGesture`, `doubleTapGesture`, `longPressGesture`, `dragGesture`, `clickAndDragGesture`, `zoomGesture`, `cursorUpdate`, `disconnect`, `approveController`, `revokeController` (a controller may name only its own `ctl`, §5.5), `listControllers`, `registerController`, `releaseOwnership` |
| controller | playback: `displayPlaylist`, `displayDefaultPlaylist`, `refreshArtwork`, `refreshPlaylist`, `checkStatus`, `moveToArtwork`, `updateIndex`, `setShuffle`, `setLoop`, `updateArtFraming`, `updateDisplaySettings`, `updateDefaultDuration`, `getRecentlyPlayed`, `playRecentlyPlayed`; offline cache: `downloadPlaylistItem`, `downloadPlaylist`, `clearPlaylistItemCache`, `clearPlaylistCache`, `getOfflineCacheStatus`; claim: `connect` (see below); device: `getDeviceStatus`, `deviceMetrics`, `rotate`, `setVolume`, `toggleMute`, `ddcPanelControl`, `ddcPanelStatus`, `setSleepSchedule`, `setSleepMode`, `sleepNow`, `wakeNow`, `reboot`, `shutdown`, `runNetworkDiagnostics`, `getContentPolicy` |
| internal | never accepted from any external principal, owner included (`permission_denied`): `resolveRecentlyPlayed` (router-internal), `ping`, `mintPairingDisplay`, `setupDisplay` (controld → player paint/probe commands that would otherwise fall through to the player over CDP), and `ff1Envelope` (an envelope inside an envelope) |
| display (§5.7) | not a tier but a role outside the table: `displayPlaylist` in the exact Play shape of §7.2, and the self-revoke; everything else `permission_denied`. `control.Permits(principal, cmd, req)` decides it, in the authority and in the router's re-check |

`connect` is controller tier because every ff-app cast, released and new,
starts with it. Only an owner claims or rebinds: under the protocol (any
state but `legacy`) a non-owner `connect` to a claimed device is answered
`ok` and changes nothing, and one to an unclaimed device is refused. In
`legacy` it behaves as before the protocol.

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
| `legacy` | no active owner grant | allowed (phase-0 rules apply) | allowed | `enroll` (with inv); `requestAccess` and `enroll` without inv → `not_approved`; a `grants.json` `cmd` → `not_approved`; a display grant's cast (§5.7) is admitted |
| `enrolled` | ≥1 active owner grant, `control.enforced=false` | refused: `enrollment_required` | controller tier only; anything else → `envelope_required` | full |
| `enforced` | ≥1 active owner grant, `control.enforced=true` | refused: `enrollment_required` | only the exact Play shape `{"command":"displayPlaylist","request":{"intent":{"action":"now_display"},"dp1_call":{…}}}`: `request` has exactly the members `intent` and `dp1_call`, `intent` is exactly `{"action":"now_display"}`, `dp1_call` is an object; anything else → `envelope_required` | full |
| `locked` | identity or grant store unreadable/corrupt | refused: `locked` | controller tier only, else `locked` | refused: public `locked` (display grants included) |

Every plaintext message, on either route and in every state, is refused with
`invalid_message` when its top level repeats a member name, exactly or in
another letter case (`"request"` twice, or `"request"` and `"Request"`):
decoders disagree about which copy counts, so the policy could judge one
`request` while the handler ran another. An allowed command runs with its
arguments decoded from exactly the `request` bytes the policy judged.

In `legacy`, `GET /api/screenshot` answers `403 screen_protected` while an
invitation is live, and also when an invitation was issued, consumed or
cleared, or is live, at the end of the capture (a capture takes seconds; a
QR painted meanwhile must not leave in the image): the claim QR then carries it, an unclaimed device has no
topic gate, and a screenshot would hand any LAN host the first-owner
invitation. Otherwise the phase-0 rules apply.

The `legacy` state is open on the LAN by design (§11 migration and first
claim depend on it): plaintext casts, input and `showPairingQRCode` remain
available to any LAN caller until an owner enrolls, exactly as in phase 0.
What protects the first enrollment is that the invitation is shown only on
the physical screen and never served over the network.

Outside `legacy`, two more LAN surfaces close:
- `GET /api/screenshot` answers `403 enrollment_required`. It has no envelope
  form (it is a raw PNG GET), and the screen can carry pairing material.
- `/api/status` and `/api/v2/status` serve `topic_id:""` even before the
  relayer claim lands: an owner enrolls first and claims second, and in
  between the topic would still be a relayer bearer credential for any LAN
  peer.

"LAN" means any caller of the hub (`/api/cast`), whatever network it came
from: Wi-Fi, Ethernet, an owner VPN or a Tailscale address all get the LAN
column. Only the legacy (topic) relayer path gets the relayer column.

**Device-ID relay (v2, ff-relayer#26).** Its address is the public device ID
and it authenticates nobody, so the relayer column above never applies to
it. On v2 (`RouteRelayV2`) every plaintext command is refused in every state:
`enrollment_required` in `legacy`, `envelope_required` in `enrolled` and
`enforced`, `locked` in `locked`. The check runs before the state switch and
before the retired `factoryReset` exception, which stays for the LAN and the
legacy relayer only. Envelopes are admitted exactly as on any other route.
`requestAccess` is refused on v2 (§5.2) and so is first-owner `enroll` with
`inv` (§5.1). `enroll` without `inv` (§5.3) is admitted on v2 as on any
route; a client sends it there only with a profile pinned from a §5.6
registration result or confirmed by a grant it holds (§10). There is no plaintext exception on v2, not even a profile read:
a controller learns the profile from the screen QR pins (first owner, on the
LAN or the legacy relayer) or from its registration result (§5.6), so v2
carries envelopes only.

**Dual-stack transport config.** The v2 endpoint is its own `controld.json`
key, `relayV2.endpoint`; `relayer.endpoint` and `relayer.apiKey` keep their
legacy meaning. `controld.json` changes only with a full image (the package
rail never rewrites it), so a controld updated on an old image keeps a
working legacy relayer. The device dials v2 only when **all** hold:
`relayV2.endpoint` is configured, a control authority exists (judged from
the device's state, not from `control.enabled`), and the device ID is the
stable MAC-derived one (a hostname that fell back to `FF1`, or the
`FF1-UNKNOWN` placeholder, never dials v2).
Otherwise it stays legacy-only.

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
- `display_grants.json` (§5.7, optional): `{"v":1,"gen","grants":[{"ctl","sig","enc","origin","label","platform":"web","originAttested","createdAt","approvedBy","expiresAt"?,"revokedAt"?,"hi","bitmap"}]}`
  - `approvedBy` is the approving owner's `ctl` or `""` (issued in
    `legacy`); the label, window and time rules of `grants.json` apply,
    `origin` is 1–2048 bytes of UTF-8 without the label's control or
    formatting characters.
  - Parsed as strictly as `grants.json`, but a missing file is an empty
    store, a file whose `gen` is not the identity's is **discarded** as an
    empty store (its records belong to a dead identity; the next write
    overwrites it), and **any other failure disables display grants
    only** (admission and issuance), logged once at start, never
    `locked`: a rollback to a controld that does not know the file must
    not lock the device, and neither must the file itself. A disabled
    store recovers when the file is deleted (the next start is an empty
    store) or the device is factory-reset. The factory reset retires it
    with the directory; the OTA carry (`control/` as a whole) keeps it.

Rules:
- A missing directory on start means create a new identity, in state `legacy`.
  The files are written into `control.new/` and the directory is renamed into
  place, so a crash never leaves a half-created directory. A leftover
  `control.new/` is discarded.
- A present but unreadable or inconsistent directory means state `locked`.
  Inconsistent includes: a `gen` mismatch, a duplicate or unknown member, a
  record whose `ctl` is not `thumb(sig)`, a duplicate `ctl`, an invalid role,
  platform, label (the same rule as enroll, §5.1) or `approvedBy`, a replay window that violates §6 (`hi = 0`
  with bits set, `hi > 0` without bit 0, bits for counters below 1), an
  owner record with `expiresAt` (§5.5: an owner never expires, and one that
  lapsed would reopen the device as `legacy`). It never
  becomes `legacy` automatically, and the files are left untouched.
- Every write goes through the same checks first, so the device cannot persist
  a file it would lock on. That holds across builds only while the label
  rule is stable: the invisible set in §5 is frozen for v1. The control
  package holds its own copy (`control/store.go`, pinned by
  `label_test.go`), not the device-name strip set, so a change to device
  names cannot lock stored grants on the next boot.
- Factory reset retires the directory before anything else in its sequence
  (before the claim is cleared and before the reset unit starts):
  1. it durably writes the tombstone `/home/feralfile/.state/control.retired`;
  2. it deletes the directory.
  If the tombstone cannot be written, nothing changes, on disk or in memory,
  and the reset is refused; if the delete fails the reset is refused too
  (with the device already retired). While the tombstone
  exists the device is `locked` (retired) whatever the configuration says,
  so a reset that rolls back onto the running subvolume after the directory
  was deleted can never come back as "no control state", which with the
  protocol disabled in config would mean open. A tombstone that cannot be
  `stat`ed for a reason other than "does not exist" means plain `locked`, not
  retired. A retired device (a real tombstone) accepts exactly one plaintext
  command on every route, `factoryReset`, so the owner can retry the reset;
  everything else, relayer controller-tier plaintext included, is refused as
  `locked`. A completed reset boots
  a subvolume without the tombstone, and the missing directory then means a
  new identity in `legacy`.

### 8.1 Configuration never reopens a device

`controld.json` is replaced on every full-image OTA from a release-level
secret, so its `control` block is a release setting, not a device one. The
device decides whether the protocol runs from its own state first:

| Control state on disk (`control/`, `control.new/`, or `control.retired`; anything `stat` cannot rule out counts as present) | `control.enabled` | Result |
|---|---|---|
| present | any value, absent, or malformed | protocol on, policy from the persisted state |
| absent | `true` | protocol on: new identity in `legacy`, invitations offered |
| absent | `false`, absent, or malformed | protocol off: transports exactly as before the protocol |

`control.enforced` only ever tightens `enrolled` to `enforced`, and it is a
per-release setting read on every boot, not a device latch: a release built
without it (or with a malformed value, which reads as `false`) runs an
enforced device as `enrolled` again. That relaxes the plaintext policy to
§7.2 `enrolled`, never further. What configuration can never do is reopen a
device that has state: a malformed `control` block is logged as an error and
the protocol stays on. If
the hostname cannot be read while state exists, the protocol still runs
(envelopes then fail their `dev` check and the plaintext policy applies).
If it cannot be read and no state exists, the protocol does not start (the
device stays as before the protocol, `legacy`-equivalent) and an error is
logged; the next start with a readable hostname creates the identity.

The OTA carry-forward (ffos `feral-system-update.sh`) MUST carry
`control.retired` along with `control/`.
- OTA carries the directory live: see ffos `feral-system-update.sh`.
- Capability floor: a device with control state MUST NOT run a controld
  that predates this protocol, which would ignore the state and reopen the
  device. A `feral-controld` package built from a ref that contains
  `components/feral-controld/control/` provides `feral-controld-control-v1`.
  With control state present, the OTA refuses an image whose controld lacks
  that marker (permanent refusal), and the package rail holds back a repo
  controld that lacks it. The marker, not the version number, is the
  check: `pkgver` is an operator input and orders dev builds unreliably, so
  any rollback to another marker-carrying build stays possible.

## 9. Sealed notifications (FF1 → one controller)

Each active grant has `nk` (32 bytes), issued at enrollment (and re-issued on
re-approval).

A sealed notification is a compact JWE:
- protected header exactly
  `{"alg":"dir","enc":"A256GCM","typ":"ff1-ntf+jwe","kid":"<ctl>"}`;
- key `nk`;
- plaintext claims, exactly `{"v":1,"dev","gen","ctl","ep","seq","iat","type","body"}`;
  a controller MUST reject an `ep` that is not exactly `b64u(8 bytes)` (no
  padding).
  The controller MUST check that `ctl` equals the header `kid` and its own
  `controllerId`, and that `dev`/`gen` match its pins.

`ep` is `b64u(8 random bytes)`, drawn once per daemon run (not persisted).
`seq` starts at 1 per controller per run and increases per notification.
Revoking and re-approving a controller within a run continues its `seq`.
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
{"type":"notification","messageID"?:"<id>","notification_type":"sealed","persist_record_count":0,"message":{"v":1,"ctl":"<ctl>","jwe":"…"}}
```

Sealed copies are never persisted: `persist_record_count` is always `0`.
Every copy, of every inner type and for every controller, shares the one
relayer type `sealed`, and the relayer keeps the newest N rows per type, so
stored rows would be whichever controller's copy came last. A reconnecting
controller refetches state by envelope (`getDeviceStatus` and the other
status queries) instead. `messageID` is copied from the plaintext
notification the copy stands for (absent for the device's own
`controller_approval_request`): an opaque random id the device minted (mint
pairing uses its approval request id). Nothing of the notification's content
leaves the ciphertext. Live delivery works on the deployed relayer too (the
type string is forwarded as is).

What is sent in each state:

| State | Sealed copies | LAN WebSocket plaintext | Relayer plaintext |
|---|---|---|---|
| `enrolled` / `enforced` | one per active grant for every notification type, except `controller_approval_request`, which goes to active owners only | none | `enrolled`: unchanged, for old apps; `enforced`: stopped |
| `legacy` | — | unchanged | unchanged |
| `locked` | — (no readable grants) | unchanged | unchanged |

Every notification is sealed separately for each channel it goes out on, so
the LAN and relayer copies of one event carry different `seq` values.

**v2 relay.** The relayer columns above apply to the legacy (topic) relayer
only. On the device-ID relay nothing is ever sent in plaintext, in any state
(a `legacy` or `locked` device sends nothing there), and
`persist_record_count` is ignored: v2 stores nothing. Sealed copies are
addressed per controller, not fanned out to every peer: the device learns a
peer→`ctl` binding only from a request whose replay window just accepted a
fresh `ctr` (never from signature verification alone, so a replayed envelope
cannot move a controller's notifications), keeps at most 2 peers per `ctl`,
and drops the binding on peer disconnect, revocation or expiry. "Fresh
`ctr`" covers every admitted envelope with a principal, including those the
authority answers itself (`listControllers`, `registerController`, an
approved `enroll`), not only commands the caller executes. Revocations
reach the binding layer as `Admission.RevokedCtls` from **every** route
(LAN, legacy relayer, v2), and grants that gain authority as
`Admission.ReactivatedCtls`: the binding layer drops any binding it holds
for either, so a reactivated controller gets notifications again only
after a fresh request. A binding whose grant is inactive when a copy is
sealed is dropped too; a transient seal failure (no entropy) is not a
reason to drop it.
A display grant (§5.7) is never bound: `Seal`, `SealAll` and the binding
layer see `grants.json` only, so a display peer receives no sealed
notification on any channel. A
controller that wants notifications sends one envelope (any
command) after connecting, on the same connection it reads notifications
from. Admission
notifications (`controller_approval_request`) are published on a context
detached from the request, so a relay that routes by the requester's source
cannot hand the owners' copies to the requester. A new binding is sent the
newest copy of each status type (`device_status`, `player_status`,
`ddc_status`, `offline_cache_status`) after the reply to the request that
created it, and v2 deduplicates those types against what it last sent, apart
from the legacy channels. **Mint pairing on v2.** `mint_pairing_approval_request`
and `mint_pairing_approval_outcome` are sealed to **owner** bindings only
(an active non-owner binding is skipped, not dropped) and are never a
snapshot: they are not kept for later bindings and not deduplicated,
whatever their legacy `persist_record_count` says, because a decided
request must not replay to a binding made later. The snapshot set stays
the four status types. The producer counts a copy queued to at least one
owner binding (queued, not acknowledged) as delivered, so with the legacy
relayer down or retired the owner still gets the approval request and the
browser is not told `approval_unavailable`. On the legacy relayer `SealAll`
still sends the mint pairing types to every active grant, controllers
included; "owner only" is a v2 rule. A `legacy` device has no owner
binding, so after the legacy relayer is retired it cannot deliver an
approval: existing display grants keep casting, and a new pairing is
rejected by the device with `approval_expired` once its approval deadline
(5 min by default) passes, until the device is Secured. The device cannot
tell earlier that nobody received the request: its own send still succeeds
at the relay. A browser whose own wait is shorter than that deadline
may report its own wait timeout first (Play maps both its own wait and
`approval_expired` to `approval_timeout`). Sends never wait on a peer: each binding has a
small queue that drops when full, and each send is bounded. A reply goes to
the peer and socket generation its request came from. A closed device socket
drops every binding it held, so after a reconnect a controller is bound again
only by a fresh sealed request.

ff-relayer persists plaintext notifications per topic and replays them to
every new subscriber: `device_status`, `player_status`, `ddc_status` and
`offline_cache_status` (`persist_record_count:1`) and both mint pairing types
(`mint_pairing_approval_request`/`_outcome`, 10). Once relayer plaintext
stops (`enforced`), that history would be replayed indefinitely. So on
**every boot in the `enforced` state**, once the relayer connects, the device
sends each of those six types once in plaintext with body `{}` and
`persist_record_count:1`; the relayer prunes a type to the newest message's
count, so this evicts every stored row of it. The deployed relayer has no
`offline_cache_status` type either and stores it under id 0, so that purge
row prunes the shared id-0 bucket. A purge row is itself stored, replayed to
later subscribers and broadcast live like any notification: a controller
MUST treat an empty-body row of any purged type as "no data", not as a
blank state (ff-app drops all four status types; `ddc_status` can be `{}`
legitimately, which also means no data). The purge runs at boot only:
a device that becomes `enforced` mid-run purges on its next boot. It is best-effort (a failed send is logged, not retried) and idempotent,
so it runs on every enforced boot rather than once per transition and keeps
no marker.

## 10. Wire carriage of requests

| Route | Request | Reply |
|---|---|---|
| LAN | `POST /api/cast`, body `{"command":"ff1Envelope","request":{"v":1,"jwe":"<compact>"}}` | 200 `{"v":1,"jwe":"<compact>"}` |
| legacy relayer | the same JSON as a topic cast | RPC `message` = `{"v":1,"jwe":"<compact>"}` |
| v2 relay | frame payload = `{"messageID":<client id>,"message":{"command":"ff1Envelope","request":{"v":1,"jwe":"<compact>"}}}`, split into `{"relayChunk":{…}}` frames above 32 KiB (whole message ≤ 4 MiB, reassembled within 180 s) | `{"type":"RPC","messageID":<same>,"message":{"v":1,"jwe":"<compact>"}}`, or `message` = the public `{"error":{"code"}}` |

The `request` carrier is parsed strictly (§1): exactly the members `v` (the
integer 1) and `jwe` (a non-empty string), no duplicate, case-variant or
extra member; anything else is `bad_envelope`.

The LAN hub caps the whole request body at 4 MiB, so on the LAN the compact
must leave room for the ~50-byte cast wrapper. The v2 relay caps the whole
JSON message at 4 MiB the same way. Because the request is base64url-encoded
twice (JWS, then JWE), the compact is about 16/9 of the plaintext `req`: the
effective plaintext limit on any sealed route is about 2.25 MiB. Clients
should check the sealed size before sending and say so, rather than fail
generically.

On v2 the device holds one socket to `/v2/channels/{deviceId}/connections`
(WebSocket subprotocol `ff-relay.v2`), independent of its legacy relayer
socket. Peers on the channel have no roles or tokens; every request from one
enters the gate as `RouteRelayV2` (§7.2) and must be an `ff1Envelope`.
The `messageID` is chosen by the client and only routes the reply; the
authenticated correlation is `rid` (§3.3). Relay acknowledgements are
transport receipts, never success.

v2 errors are typed by what they prove. A request is **proven undelivered**
only when the relay refused it for every target and no chunk of it was
acknowledged; only then may a client resend it or fall back to another
route. Anything else (a timeout after an acknowledgement, a socket drop
after sending) is **ambiguous** and is never resent (§14.4). A client's
first envelope on a device channel goes to `to:"peers"` and it then pins
the peer whose reply verifies under the pinned `devSig`.

**Liveness.** Every v2 endpoint (the device and each client, on device and
pairing channels) sends the text frame `ping` at least every 30 s; the
relay answers `pong` without waking its state (an auto-response) and closes
a connection that has shown no frame and no `ping` for 90 s. A full channel
refuses newcomers (429) rather than evicting a live connection, so a device
that keeps pinging is never pushed out. Endpoints ignore a text `pong`.

**Ambiguity is the default.** Once any frame of a request has been written
to the socket, only an explicit relay refusal for every destination known
at send time, with no chunk acknowledged, proves it undelivered. A peer
leaving, a socket closing, a deadline firing while a receipt is pending,
or any other silence is ambiguous: the device acknowledges before it
executes, so a departed device may already have run the command.

A v2 refusal at connect or admission time (HTTP 429, close code 1008, the
relay's `backpressure`) is **transient**: the device and every client retry
with backoff and never treat it as permanent. The device-ID channel is
public, so strangers can contend for it. (The legacy relayer's handshake
semantics are unchanged: released firmware treats a 4xx there as permanent,
which is why the legacy host's shutdown waits for the device population,
§15.4 Gate B.)

First-owner `enroll` with `inv` (§5.1) is never carried on v2, and the
device refuses it there. `enroll` without `inv` (§5.3) MAY be carried on v2
by a controller whose profile is already trustworthy: pinned from a verified
§5.6 registration result (sealed to the controller under `ps`, which exists
only in the controller QR, so the profile is the owner's verified view of the
FF1), or confirmed by a grant the controller already holds (a role refresh,
whose ordinary sealed commands already use v2 under the same profile). A
profile from any other source (an unconfirmed LAN status, a pairing reply
that verifies only under itself) MUST NOT be enrolled over v2: the device
channel is addressed by the public device ID and anyone can join it, so a v2
reply that verifies under an unconfirmed profile proves nothing.

Route order for a §5.6 newcomer: a legacy topic it held from **before** the
pairing first. That topic reaches the real FF1's authenticated legacy
socket, and the real FF1 cannot open an enroll sealed to a profile forged by
someone holding `ps`, so the forgery fails there instead of being answered
(§14.1). Only when that topic answers 1003/410, or the legacy origin answers
404 (a shut-down host), or none is held: the LAN, then v2. A DNS failure
(NXDOMAIN) is never such an answer: a LAN peer can spoof it. A client that
cannot tell whether it holds such a topic (its credential store is
unreadable) MUST NOT send this `enroll` over v2, where a forged profile is
answered from anywhere; it MAY use the LAN, the exposure every client had
before v2. A topic taken from the registration result itself carries no
such assurance and goes last. The device admits `enroll` without `inv` on
v2 exactly as on the LAN: it succeeds only for a `ctl` an owner already
approved or registered. (Until 2026-10 this was a blanket client rule that kept every
`enroll` off v2, which would have made adding a controller impossible off the
LAN once the legacy relayer is retired.)

At most 4 envelopes are authenticated at a time (decrypt, verify, parse);
an envelope that cannot get a slot within 1 s is answered with the public
error `busy` (LAN 503) and should be retried. No lock other than that bound
is held while an envelope is unauthenticated.

Under the protocol, a relayer frame with no `command` (the relayer's own
error frames such as `{"error":{"code":2000,…}}`) is never answered, an
envelope without a `messageID` is not opened, and a refused plaintext frame
without one gets no reply: an answer would draw the same relayer error again
and loop. An allowed plaintext command without a `messageID` (and any
command with the protocol off) still runs and replies as before the
protocol; the relayer answers that once with an error frame, which is then
dropped. On the relayer, an envelope whose `request` is not a JSON object
never reaches the authority (the relayer decoder drops it), so it gets no
reply rather than `bad_envelope`.

## 11. Public discovery

- `/api/v2/status` and `getDeviceStatus` gain
  `"control":{"v":1,"state":"legacy|enrolled|enforced|locked","profile":profile}`.
  The presence of `control` is the capability signal. A `locked` device omits
  `profile`: it cannot vouch for an identity it could not read.
  `control.transports` lists the routes this device is configured to carry
  envelopes on, in this order: `"lan"` (always), `"relayer"` (a legacy
  relayer endpoint is configured), `"relayV2"` (the device dials the v2 relay,
  §7.2 dual-stack rules). It reports configuration, not whether a socket is
  up right now. Clients treat an absent `transports` as `["lan","relayer"]`.
- With the protocol on, the claim QR appends four pipe fields to the
  existing six in every state but `locked`:
  `…|<setup_phase>|<inv>|<thumb(sig)>|<thumb(enc)>|<gen>`. `<inv>` is the
  invitation when the device is `legacy` and either unclaimed or asked with
  `purpose:"enroll"` (below), and empty otherwise; the pins are always there
  so a second phone pins from the screen (§5.4, §14.1).
  - Old app parsers read only indices 0, 1 and 3.
  - The topic stays at index 1 whenever the device has one, claimed or
    not: once status withholds `topic_id` (§11 status, phase 0), the screen
    QR is how a second phone learns the topic.
  - The layout is fixed for every transport:
    `id|topic-or-empty|internet|branch|version|phase|inv|thumb(sig)|thumb(enc)|gen`.
    A device without a topic (v2-only) leaves index 1 empty rather than
    dropping the field, so every index keeps its meaning. Parsers MUST accept
    an empty index 1 when the pins are present and MUST read the branch at
    index 3. Released app parsers drop a QR with an empty index 1 **silently**
    (the scan does nothing). During dual-stack the slot is always filled, so
    this matters only for v2-only firmware, which ships after the released
    app floor has moved past those parsers.
  - The only builder is `formatDeviceConnectURL` in devicectl
    (`components/launcher-ui`, the other builder this once had to match, was
    deleted with feral-setupd).
  - The invitation is issued when the QR is painted, replaced when it has
    less than 2 minutes left, and refreshed in place one minute before it
    expires while the claim QR is still on screen. A QR that is no longer on
    screen mints nothing.
  - When the first owner enrolls, the invitation is consumed and the claim
    QR is taken down; from then on the auto-claim flow treats the device as
    settled and does not repaint the topic-carrying QR.
- A second phone, ff-cli or a browser joins an owned device
  (`enrolled`/`enforced`) through owner-initiated registration (§5.6): it
  shows its controller QR and an owner registers it, from anywhere. Clients
  no longer offer `requestAccess`; the device keeps it as a LAN-only
  fallback (§5.2) for a phone standing next to the FF1 with no owner at
  hand, pinned from the screen QR.
- `showPairingQRCode` with `{"show":true,"purpose":"enroll"}` on a `legacy`,
  claimed device displays the invitation QR. This is the migration path for
  2.0.x owners. On a device that is not `legacy` the same request paints the
  ordinary claim QR without invitation fields.

## 12. Fixtures

`components/feral-controld/control/testdata/fixtures/` holds:
- `keys.json`: the fixed device (`device`: `dev`, `gen`, private `sig`/`enc`,
  `profile`, `pins`) and five controllers (`controllers.owner`, `.controller`,
  `.revoked`, `.stranger`, `.display`) with private `sig`/`enc`, `ctl`, `label`,
  `platform`, and for the first three `role`, `nk`, `createdAt` (and
  `revokedAt`); `stranger` has no grant; `display` is a Play site's display
  grant (§5.7: role `display`, platform `web`, an `origin`, no `nk`) and is
  only ever in `display_grants.json`. `now` is the fixed clock.
- `positive/*.json`, `kind` `request` or `notification`. A request vector has
  `setup` (below), `from` (the signing controller), the exact signed
  `payload` and its parsed `claims`, the fixed `ephemeralKey` (private JWK)
  and `iv`, the inner `jws`, the outer `compact`, `rk`, the device's
  verdict in `expect` (`execute`, `command`, `principal`, `replyOk`,
  `replyCode`), the device's `reply` (`compact` and decoded `claims`) and any
  `notifications` it emitted (`to`, `compact`, `claims`, `wire`).
- `positive/approval-code.json` (`kind:"approvalCode"`): `cases` of
  `{ctl, sigPin, code}` for §5.4, the last one with a non-device pin.
- Registration (§5.6). Request vectors `cmd-owner-registerController`
  (new grant; `res.ctl`), `cmd-owner-registerController-reactivate` and
  `cmd-controller-self-revoke`. `positive/controller-qr.json` and
  `negative/controller-qr.json` (`kind:"controllerQR"`): `cases` of a `qr`
  string and, for valid ones, its parsed `channelId`, `minterKey`, `ctl`,
  `sig`, `enc` (canonical JWK JSON), `ps`, `platform`, `label`.
  `positive/controller-registration*.json` and `negative/registration-*.json`
  (`kind:"registration"`): the controller's `qr` and private `minterKey`,
  `controller` (the `keys.json` controller whose keys the QR carries),
  `channelId`, `ps`, `now`, optional `wantDev`, the owner's
  `ownerEphemeralKey` and `iv`, the exact `plaintext`, and the `wire`
  message. A client MUST reproduce `wire` byte for byte from the owner
  inputs, open every positive vector to `plaintext`, and reject every
  negative one.
- `negative/*.json`:
  - `kind:"request"`: a `compact` (or `compactRepeat` `{char,count}` for the
    oversize vector, built by repeating `char`) plus `setup`, and either
    `expect.public` (the §4.4 code) or `expect.replyCode` (open the device's
    reply with the vector's `rk`). Covered: tampered tag, wrong JWE `kid`,
    wrong `dev`, wrong `gen`, expired, replayed `ctr`, out-of-window `ctr`,
    DER signature, extra and duplicate header members, `alg` `none`/`RS256`
    (JWS) and `RSA-OAEP-256` (JWE), off-curve `epk`, oversize body, forged
    signature, unknown `kid`, revoked controller, `exp - iat > 120`,
    `ctl ≠ kid`, owner-tier command from a controller, `registerController`
    from a controller, `revokeController` of another `ctl` from a controller;
    display grants (§5.7): `getDeviceStatus`, `connect`, a non-Play-shape
    `displayPlaylist`, `registerController` and `op:"enroll"` from the
    display `ctl` (`permission_denied`), an expired display grant
    (`access_revoked`), and `approveController{role:"display"}` from the
    owner (`command_failed`, `res.error` `invalid_request`). The positive
    display vectors are `cmd-display-displayPlaylist` (also `-legacy`),
    `cmd-display-self-revoke` and `cmd-owner-listControllers-display`.
  - `kind:"reply"` / `kind:"notification"`: vectors a controller MUST reject:
    wrong `rk`, tampered, signed by a key other than the pinned `devSig`,
    unbound `rid` (`bind` gives the dev/gen/ctl/rid the controller sent), extra
    header member, DER signature, wrong `nk`, header `kid` ≠ claims `ctl`,
    `ep` that is not `b64u(8 bytes)` (short, or padded).

`setup` describes the device a request vector is opened against: `now`,
`grants` (which `keys.json` controllers are in `grants.json`), optional
`displayGrants` (`[{name, expiresAt?}]`, the display records of §5.7,
issued at `now`), optional
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

## 13. Security notes

- The relayer's `messageID:"system"` topic message is not a controller path
  on the legacy relayer: ff-relayer generates every cast's `messageID` server
  side (`crypto.randomUUID()` in `handleCast`), and the system message is
  relayer-originated on connect, so a controller cannot forge one. On the v2
  relay the `messageID` is chosen by the client, so a v2 frame with
  `messageID:"system"` MUST be a no-op: the topic is honored only from the
  legacy relayer route, and a reply is sent back on the route its request
  came from. The topic is a bearer credential on the legacy relayer, so the
  device never logs it raw (logs carry an 8-hex SHA-256 fingerprint, and
  system frames are logged redacted).
- Play browser sessions never subscribe to the notification channel, so
  sealing notifications (§9) cannot strand a Play session; a display grant
  (§5.7) is never bound or sealed to either.
- Residual, accepted: on a claimed device still in `legacy`, any LAN peer
  can send `showPairingQRCode{show:true,purpose:"enroll"}` and a person at
  the screen can then take ownership with the invitation it shows. This is
  the same class as the open legacy `factoryReset` and `connect`: in
  `legacy` the LAN is open by design (§7.2), and the invitation itself only
  ever appears on the physical screen.
- Residual, accepted: the enforced-boot purge (§9) evicts the relayer's
  stored rows of each persisted plaintext type. Any copy a subscriber
  already received, and a purge that failed to send, are outside the
  device's reach.
- Residual, accepted: every authenticated denial is a synchronous durable
  write (§3.4 step 6). A holder of a revoked key can drive a few hundred
  small writes a second and serialize admissions behind them; it gains
  nothing and needs a key that once held a grant.
- Residual, accepted: a sealed `controller_approval_request` published while
  the relayer is disconnected is dropped and re-sent only on a refresh that
  changes label or platform; the owner sees the request on its next
  `listControllers`.
- Residual, accepted: replies are unpadded, so their length tells a passive
  observer the outcome class (ok, replay, a denial code, pending). It reveals
  nothing a controller's own next action would not.
- Residual, accepted: pending-request lifetimes, the 10 s new-request
  limit and denials use the wall clock. A request that arrived while the
  clock was far ahead, followed by an NTP correction, can rate-limit new
  requests until the clock catches up; a restart clears it (memory only).
- Residual, accepted: the setup access point's captive portal (`/connect`,
  `/rescan`, `/status`) is outside this policy. Its WPA2 passphrase derives
  from the device id, so anyone in radio range while the access point is up
  can set Wi-Fi credentials: network position only, and on an enrolled
  device the hub still applies the LAN policy.
- Residual, accepted: a `locked` or retired device sends status
  notifications in plaintext again on every channel (§9 table), including
  persisted relayer rows an earlier enforced boot purged. A device in that
  state has no grants to seal to.
- Remote shell granted while the device had no owner (open `sshAccess` on
  an unclaimed device, or by a topic holder in `legacy`) is revoked when the
  first owner enrolls: the device runs the `sshAccess{enabled:false}` steps.
- Residual, accepted: the LAN-only `requestAccess` fallback (§5.2) keeps the
  approval-code grinding residual of §5.4 when a requester pins from LAN
  status instead of the screen QR. Clients no longer offer it; owner-initiated
  registration (§5.6) compares no code and binds the profile inside the
  owner's sealed result.
- Residual, accepted: a controller QR (§5.6.1) forwarded to a remote owner
  through a chat app carries `ps` and the minter key through that app, which
  could then forge a registration result for those keys during the
  exchange. The relay alone cannot. The controller still enrolls only under
  a profile whose device then answers its sealed `enroll` with a grant.
  If that party is also a LAN man-in-the-middle answering as the device, the
  fake enrollment completes over the LAN; if it also takes the real device
  off the relayer, the relayer's 1003 for the controller's held topic (what
  a reset and an offline device both look like) lets a client store the
  forged result's topic in its place, so the impersonation continues over
  the relayer after the controller leaves that LAN. With the §10 v2 enroll,
  the same party no longer needs the LAN once the controller has no
  pre-pairing topic (or that topic is retired): holding `ps`, it forges the
  result under its own profile and answers the controller's sealed `enroll`
  on the public device channel. The reach is unchanged: the controller believes in a
  fake FF1 (what it sends goes to the forger), and the real FF1 grants that
  party nothing.
- Residual, accepted: a request a controller signs **before** its keys are
  registered is answered `unknown_controller` and never enters a window, so a
  copy captured on the relay can be replayed if the owner registers those
  keys within its `exp` + 30 s. The §5.6 flow sends nothing before the
  protected `enroll`, and ff-cli sends no command while a pairing is open.
- Residual, accepted: the v2 device channel is public and the relay cannot
  tell the device from a stranger (a legacy topic is not bound to a device
  ID and anyone can mint one). Per-address limits (IPv4 address, IPv6 /64),
  idle close and refusal at capacity raise the cost, but a party with a
  handful of addresses that keep pinging can still fill a channel. While
  the legacy relayer exists the effect is a 429 at connect, which is proven
  undelivered, so clients fall back to the legacy relayer and the device's
  legacy session is unaffected. After the legacy relayer is gone, a device
  whose channel is held full has no remote route until the squatters leave;
  the LAN is unaffected. Until 2026-10-30 this section required a relay
  connection slot attributable to the device before retirement; that was
  withdrawn: the squat needs no
  credential and grants nothing, and a device credential on the relay would
  make the relay hold device identity for the first time. If it is ever
  exploited, the remedy is a relay change (a reserved slot proved with
  `devSig`), not a firmware change.
- Residual, accepted: `releaseOwnership` (§5.5) changes who holds a grant,
  nothing else. The identity and the root subvolume stay, so a former owner
  who had root through SSH or owner Tailscale may have left something
  behind; hand a device to a stranger with a factory reset. Closing SSH on
  release is cleanup, not a guarantee: in `legacy` a topic holder may open
  it again with plaintext `sshAccess`. Topic holders (pre-protocol app, Play
  0.4 sessions) keep `legacy` control after a release, as on any `legacy`
  device; a factory reset is what cuts everyone off.
- Residual, accepted: in a 2.0.x migration whoever scans the enroll QR
  first becomes the first owner; a lost sole owner still means a factory
  reset.
- Display grants (§5.7) and the controller-API retirement: before the
  legacy controller endpoints (`/api/cast` with a session token,
  `/api/notification`) are turned off, integrators must be on Play ≥ 0.5,
  the fleet must run firmware with §5.7 **and** the v2 mint pairing
  delivery of §9 (older firmware carries approval requests over
  `/api/notification` only), the app force-update floor must be raised,
  and the share of unsecured (`legacy`) devices must be acceptable: on
  those a new 0.5 pairing is rejected `approval_expired` after the
  device's approval deadline (§9; the browser's own timeout may come
  first) while existing grants keep casting. A grant issued in `legacy` carries
  the trust of today's session token (display only) and stays valid after
  the device is Secured.
- Residual, accepted: the mint pairing broker is trusted for the announced
  display-grant keys as it is today for session tokens. A compromised
  broker can substitute keys and obtain at most a display-only, revocable
  grant for the origin it attested.
- `/api/status` (v1) also carries the `control` block. The enforced purge
  gives up after 10 minutes without a relayer connection. The invitation
  watch can miss one re-arm when another narration takes the panel between
  its check and the repaint; the invitation still expires on time.

## 14. Controller requirements

Every controller (ff-app, ff1-cli, a browser or any later client) MUST follow
these rules. The device cannot enforce them; review findings on both clients
showed what goes wrong without them.

1. **Pin sources.** A device profile is pinned only from:
   - the physical claim QR (first owner, migration);
   - an owner's sealed registration result (§5.6), checked per §5.6.3;
   - an owner's end-to-end encrypted handoff (the earlier short-code ff1-cli
     pairing that carried a profile; superseded by §5.6). The short-code
     handoff stays only as a topic handoff for devices without the protocol
     (no `control` block in status) or with no owner yet (`legacy`), where it
     carries no profile. That choice is made from unauthenticated status, so a
     client that already holds or held a grant for the device never takes
     it unless the user forces it, and a topic that arrives inside a
     registration result is never used before the controller's verified
     enroll;
   - for `requestAccess`, the pins on the screen QR (every state, §11); a
     second phone MUST pin from them when the QR carries them, because the
     approval code alone can be ground by a network attacker (§5.4);
   - only when the QR carries no pins (firmware before them), the
     unauthenticated LAN status, or off the LAN an `enrolled` device's
     relayer `getDeviceStatus` reply;
   - for a Play site, the `profile` inside the encrypted `mint_succeeded`
     `displayGrant` (§5.7). The mint pairing channel is end-to-end
     encrypted to the browser's channel key, but that key was distributed
     by the broker, so this pin source trusts the broker exactly as the
     session token did; the owner's approval is the check.

   Whenever the scanned QR carries pins, a client MUST compare them with
   any profile it reads from the network and MUST NOT proceed on a
   mismatch, whichever path it is on.

   The handoff authenticates the receiving client to the sending phone, not
   the sender to the receiver: whoever joins the handoff channel first with
   its short code can supply the profile. A wrong profile only makes the
   pairing fail (the device's replies do not verify), so the receiver MUST
   NOT replace a grant it already holds until the new pairing is enrolled.
   Residual, accepted: a channel joiner who is also a LAN man-in-the-middle
   can answer as the device and complete a fake enrollment; the receiver
   then replaces its real grant. The owner's approval code comparison is
   the check against it.

   In the network-pinned case the requester's approval code (§5.4) is the only
   protection, and it does not stop an attacker who can grind a key. An impostor can still answer
   "approved" to that requester and read its commands until the requester gets a
   reply that verifies under the real device's key. It gains no authority over
   the real FF1.
2. **Confirmed generation.**
   - A controller seals only to the grant whose `gen` was most recently confirmed
     by a reply that verified under the pinned `devSig`.
   - A `gen` reported by status or a notification never selects a grant.
   - Once a new grant is confirmed, the controller deletes its other grants for
     that device.
3. **Reset evidence and recovery.** Public errors (`bad_envelope`,
   `unknown_controller`, `bad_signature`) are unauthenticated and are never
   evidence of a reset. The relayer reporting "no device on this topic" (1003)
   is also weak: it is what an offline device looks like.

   So a controller that holds a grant for a device MUST NOT re-enroll with that
   device using a network-pinned profile. Its only recovery actions are:
   - scanning the physical claim QR (§5.1, pins from the screen);
   - a user-confirmed Remove of its local grant, after which it is a new
     controller. Remove always asks, whatever the grant's role (ff-app
     Remove and ff1-cli `unpair` alike);
   - a new owner-initiated registration (§5.6; for ff1-cli `device pair`):
     the profile comes from the owner's sealed result, not the network, and
     new keys are staged and replace the old grant only once enrolled.

   A request refused with `access_revoked` in a reply that verified under the
   pinned `devSig` is not a reset: an owner can re-register the same keys
   (§5.6.4 reactivates the record), so the controller MAY show its
   controller QR with its existing keys. Its UI MUST then show that it has
   no access (not "can play"), with "Show this code to an owner" and Remove
   reachable outside any one-time prompt. A device reset shows up as a new
   `gen` in the screen QR pins; the controller says "if it was reset, ask an
   owner to add you again".

   The device does not tell a controller its role changed (§5.5 is in
   place, with no notification). A controller learns its new role from the
   next idempotent `enroll` reply or a verified `permission_denied`.

   Until the user does one of these, it keeps sealing to the confirmed grant.
4. **Retries.** Every resend is a new envelope with a new `ctr`, `rid` and `rk`.
   A request whose outcome is unknown is resent only where the command is safe to
   repeat. A `replay` in a reply that verified under the pinned `devSig`
   means the controller's counter fell behind the device's window (a
   restored backup, a second copy of the keys): the controller moves its
   counter past the window (at least 64 ahead) under its counter lock and
   retries once, keeping its keys. A public error is never evidence of this.
   On v2 a `replay` is not evidence of it either, because the same sealed
   bytes may have reached the device twice (a fan-out plus a copy to a
   connection that joined late): there it is reported as an unknown outcome
   and never triggers the counter jump or a resend.
5. **Time.** A controller learns the device's clock only from verified replies.
   Notifications may be replayed by the relayer (§9), so they never move the
   clock.
6. **Notifications are advisory.** A controller never acts automatically on a
   notification (§9). An owner approves only after comparing codes.
7. **Removal.** A controller that removes an FF1 sends a best-effort sealed
   self-revoke (`revokeController` with its own `ctl`, §5.5) before deleting
   its keys, and deletes them whatever the outcome. An owner's Remove is
   blocked when it is the last active owner ("add another owner first");
   the device refuses that self-revoke with `last_owner` anyway.
8. **Registration.** A controller adds itself only through §5.6: it shows
   its QR, waits for a sealed result that passes §5.6.3, and treats a
   timeout as "not completed". An owner app sends `approved` only after a
   verified `registerController` reply whose `res.ctl` is `thumb` of the
   scanned `sig`. A `topicId` in the result is stored for the legacy relayer
   route only.
9. **Retired device.** A device whose status reports `control.state:"locked"`
   after a factory reset the controller asked for may be retired (§8): it
   refuses every envelope and accepts only a plaintext `factoryReset`. No
   current client sends one (both seal whenever they hold a grant), so in
   practice the retry is the factory reset on the device itself. Plain
   `locked` (corrupt state) refuses even the plaintext one.
10. **Browser (display grant, §5.7).** A Play site generates fresh
    non-extractable keys for every pairing and keeps keys, counter and
    grant as one record; a re-pair replaces the record only on a
    `mint_succeeded` that carries a `displayGrant`, then self-revokes the
    old grant best-effort with the old keys. It connects to the v2 relay
    per cast and closes right after (never an idle socket), fans the
    envelope out and pins the first reply that verifies under the pinned
    `profile.sig`. Rules 4 and 5 apply: every attempt is a new envelope, and
    a `replay` on v2 is an unknown outcome, which the browser (no LAN path
    to resync) answers by moving its counter past the window without a
    resend. A verified `access_revoked` or `expired` deletes the grant and
    re-pairs on the user's next cast; an unverified public
    `unknown_controller` or `bad_envelope` seen after the deadline is
    reported as "unrecognized" and **never** deletes the grant (rule 3:
    any channel peer can forge it); only a user action re-pairs. A
    `mint_succeeded` without `displayGrant` (older firmware) is
    `device_unsupported`: the session it carries is discarded.

## 15. Rollout and retirement

These are release rules. The device cannot enforce most of them; a human
release operator applies them.

### 15.1 Turning the protocol on

- `control.enabled` MUST NOT be set in `controld.json` for a channel before
  an ffos image with the OTA carry step (`ota-control-state.sh`: carries
  `control/`, `control.new/` and `control.retired`, §8.1) is on that
  channel's OTA path. An image without it drops `.state/control` on update
  (owners lose their grants) or, after a downgrade and re-upgrade,
  resurrects stale grants. The ffos image workflows force `del(.control)`
  when the ffos or ffos-user ref lacks the carry step or the `control`
  package; a workflow file from a ref that predates that check does not, so
  dispatching one with `.control.enabled=true` in the environment secret
  ships an image that creates state and loses it on its next OTA.
- Raise the app force-update floor before setting `control.enabled=true` in
  production. A released app cannot cast on the same LAN as an `enrolled`
  device (LAN plaintext is refused and the released app does not fall back
  to the relayer after an HTTP error); off the LAN it keeps controller-tier
  playback through the relayer until `enforced`.
- Release notes of the first control-enabled release state: once it runs, the
  device refuses to OTA to an image without the control marker (§8.1, a
  permanent refusal); the only way back is a factory reset.
- `control.enforced=true` ships in a later release, after fleet telemetry of
  `control.state` shows owners have enrolled. `enforced` cuts the released
  app off the relayer too, except Play's exact `displayPlaylist` shape
  (§7.2).
- Display grants (§5.7) ship in this order: the app and ff-cli that list
  display rows (role `display`, platform `web`) before any FF OS with
  §5.7; Play 0.5 is published only once FF OS with §5.7 **and** a
  configured `relayV2.endpoint` is broadly deployed, because 0.5 answers
  `device_unsupported` on anything else.

### 15.2 Rollout levers and rollback

- A required update is driven by `min_runtime_version` (otagate
  `isUpdateRequired`), raised once and deliberately. Never
  `min_upgradeable_version`: below it a device is too old to upgrade and
  needs a USB reflash (otagate `isTooOldToUpgrade`).
- No channel rolls back below a dual-stack image (one that runs the
  protocol on the LAN, the legacy relayer and v2).
- Factory-reset images (`@recovery_candidate`/`@factory_reset`) and shelf
  stock are pre-protocol: a reset onto one has no grants and an open hub
  until its next OTA. Raise `update_recovery_version` to a dual-stack image
  before the legacy relayer is retired.

### 15.3 v2 integration and legacy retirement

- The device dials v2 only when the control wiring is non-nil, and only
  together with the `RouteRelayV2` wiring, the route-aware `system`
  handling and reply target (§13), the per-controller notification binding
  (§9) and the topic-independent claim QR (§11). A build that dials v2
  without all of them is open to anyone who knows the device ID.
- Legacy retirement is one step, decided 2026-10-30: the legacy host is
  shut down, not flag-retired (§15.4). The relayer's split flags remain for
  a relay that is still up with a route switched off, which is how a test
  deployment simulates retirement.
- Turning the legacy relayer off is gated on fleet telemetry of
  `control.state`, not on firmware version: a `legacy` device has no
  controller grants and v2 admits no plaintext (§7.2), so retirement strands every unmigrated
  device off the LAN. It also waits for Play to move from relayer session
  tokens to device-issued display grants (§5.7, Play 0.5), with the
  retirement prerequisites listed in §13, and for the §15.4 gates
  (controller traffic and device population).
- There is no topic-to-grant migration and no locally generated topic. A
  legacy topic is an HMAC credential minted by the relayer, and v2 never
  reads one. A device claimed under legacy (topic, no owner) becomes
  v2-reachable only when its owner enrolls it on the LAN with the screen
  QR: the "Secure this Art Computer" flow sends plaintext
  `showPairingQRCode{purpose:"enroll"}` by topic, LAN first, and enrolls
  from the invitation QR (§5.1). It needs the phone that holds the topic;
  without it the remedy is a factory reset.
- A client sends a sealed command on v2 only when the device's last status
  listed `relayV2` in `control.transports` (§11), or under the ff-cli
  exception in §15.4: a v2 send to a device that is not on v2 can be
  receipted by another peer and become ambiguous instead of falling back.
  Every G3 image lists `relayV2`: the ffos image workflows refuse a
  `controld.json` without `relayV2.endpoint` for an ffos-user ref that
  carries the v2 relay client (`relaywire/`). The record is refreshed over
  whatever route works while the legacy relayer exists.
- One v2 host. Every environment's `relayV2.endpoint`, the app's and the
  CLI's defaults, and the endpoint a display grant hands a Play site name
  `wss://relayer.feralfile.io` (worker `ff-relayer-v2`). The legacy worker
  also serves `/v2/` on its own Durable Object namespace; it is never a v2
  target, and no client routes to a host the device reports (status
  `control.transports` lists `relayV2` without a URL).

### 15.4 Legacy relayer shutdown

Firmware generations this section refers to:

| Generation | Versions | Relayer |
|---|---|---|
| G0 | 1.0.17–1.0.21 | legacy only (BLE setup) |
| G1 | 2.0.0–2.0.11 | legacy only; no `control`, no `relayV2` |
| G2 | develop builds after 2.0.11 without G3 (unreleased as of 2026-10-30) | dual-stack; the claim QR and mint pairing still need a legacy topic |
| G3 | feral-file#3551 | dual-stack, legacy optional |

G0–G2 controld exits when a live legacy socket drops and the reconnect
returns a permanent error, and from 2.0.0 its mediator redials the legacy
relayer on every SYSMETRICS heartbeat (about 30 times a minute). 404/410
handshakes and non-temporary DNS failures are permanent there. Their
pre-claim OTA gate runs only after the claim flow got a legacy topic, so
after the shutdown an unclaimed pre-G3 device waits for the daily updater
(`feral-updater@.timer` talks to the version API only), and the image a
factory reset boots matters as much as the running one.

**The step.** After the transition the worker `tv-cast-coordination` is
deleted. From then on its URL answers HTTP 404 with the text body
`error code: 1042` (a deleted `workers.dev` worker, WebSocket upgrades
included), or NXDOMAIN once the account's `workers.dev` zone is gone. The
old hostname is not pointed anywhere: it is under another account's zone,
so it cannot be aliased to `ff-relayer-v2`, and a flag-retired worker left
running would be a second host to keep.

**What reads as retired:**

- **Device (G3).** Any permanent legacy dial error (404/410 handshake,
  NXDOMAIN, bad URL) or an empty endpoint latches the legacy relayer
  retired; v2 and the LAN hub keep running (ffos-user `architecture.md`,
  "External transport").
- **Controllers.** A 410 body (`legacy_*_retired`), or an HTTP 404 from the
  legacy origin on `/api/cast`, `/api/notification` or `/api/connection`.
  A live relayer never answers 404 there (its "no device" is `400` id
  1003); it does answer `404 {"error":{"id":1006}}` on the
  ephemeral-session routes for a missing session, so a 404 there is never
  retirement. The latch lasts the session: no retry, the held-topic leg of
  a §5.6 enroll moves on as after a 1003 (§10), and the device row says
  why. A DNS failure, a refused connection or a timeout is **never**
  retirement: a LAN peer that saw the controller QR can spoof NXDOMAIN
  over plain DNS (§10, §14.1).
- **App, for NXDOMAIN.** The remote config flag
  `features.ff1.legacy_relayer_retired`, fetched over TLS from a host the
  team controls, latches the same state on every install.
- **ff-cli** has no remote config. The release after the shutdown turns
  its legacy relayer off. Until then, a sealed command (`op=cmd`) whose
  held topic is gone (the set above, or a 1003) may try v2 even when the
  record does not list `relayV2`: with the legacy host gone there is no
  fallback a v2 ambiguity could cost.

**Gates.** Both are preconditions of the one step; they are kept apart
because they are measured differently.

| Gate | What it covers | Condition |
|---|---|---|
| A (controllers) | `/api/cast`, `/api/notification`, `/api/ephemeral-sessions` | app floor raised past the G3 app release (device identity by `deviceId`, LAN bind by QR pins without a topic, retirement read as above); the G3 ff-cli release published (v2 default, retirement read as above, the held-topic-gone v2 exception); Play integrators on 0.5; `tv-ws-usage` `/api/cast` traffic ~0 |
| B (devices) | `/api/connection` (topic issuance) | from the device gauges (ffos-user `api-design.md`, `/metrics`): running version ≥ G3 AND `reset_target_version_info` ≥ G3 (`unknown` counts as below) for nearly all devices; legacy-dependent devices (claimed, topic present, `control_state=legacy`) under an agreed threshold; known remote installs contacted; shelf stock reflashed |

`tv-ws-usage` (one sampled row per legacy request that reached a topic)
shows controller traffic falling. It cannot measure devices: a device that
stays connected for days writes one row, and the v2 worker has no
analytics binding. The device gauges are the only source for Gate B.

G3 is a full-image release (declared in ffos-user `RELEASES.md`), because
`controld.json` changes only with a full image. The image secrets
(`FERAL_CONTROLD_CONFIG_JSON`) keep a `relayer` block, even after the
shutdown, until no pre-G3 controld can run on them: pre-G3 controld
dereferences it without a nil check and would crash-loop on an image built
from an older ffos-user ref. Before Gate B, once G3 is on a channel (human
operator, recorded in ffos-user `RELEASES.md`): `update_min_version=true` (`min_runtime_version`
= G3; never `update_required_version`, which stops auto-update and demands
a reflash), `update_recovery_version=true` (`recovery_version` = G3), and
shelf stock reflashed with G3.

**Flag order.** `features.ff1.legacy_relayer_retired` is the last action of
the shutdown, not part of its preparation:

1. Gate A and Gate B both hold.
2. The worker `tv-cast-coordination` is deleted.
3. Only then is the flag set to `true`.

The flag is immediate and fleet-wide: every app install latches "legacy
retired" on its next config read and stops dialing the legacy route for
the rest of the session, including for an FF1 still on legacy-only
firmware or still `control_state=legacy`. Set before the gates hold, it
would cut off-LAN control for exactly the population Gate B counts; set
before the worker is gone, it would hide a working route. If it is set by
mistake, setting it back to `false` restores the route on the next config
read for installs that have not latched yet; latched sessions recover on
relaunch.

**After the shutdown:**

- G3 devices latch retired, probe on the escalating schedule, and keep v2
  and the LAN hub.
- G0–G2 devices: the daemon exits once on the live-socket drop and systemd
  restarts it; from 2.0.0 it then redials the dead host on every heartbeat
  for as long as it runs that firmware. Playback, the LAN hub and OTA keep
  working; remote control is gone until the device is on G3 and enrolled.
- App: latches on the 404 or the flag. Under NXDOMAIN without the flag its
  legacy notification socket redials every 5 s while foregrounded (the
  backoff does not grow; measured 2026-10-30), and a phone holding a
  pre-pairing topic cannot add a controller. Set the flag at the latest
  when the zone disappears.
- ff-cli: the same 404 counts. Under NXDOMAIN an ff-cli older than the
  release with the legacy relayer off keeps failing on the held topic: a
  §5.6 enroll stops there, and a command whose FF1 never listed `relayV2`
  is not tried over v2. Accepted residual (2026-10-30); the remedy is
  updating ff-cli.
- Play 0.4 sites: their session casts fail; they move to 0.5 (Gate A).

Populations left between G3 and Gate B: legacy-claimed G3 devices waiting
for an owner at the LAN; devices whose OTA keeps failing (free space, slow
links, probation rollback); devices that are off; devices whose reset
target is still old; controllers outside the force-update floor (old CLI,
Play 0.4, app fail-open); networks that reach the old host but block
relayer.feralfile.io.
