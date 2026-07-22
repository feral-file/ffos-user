# FF1 v2 controller authentication and access sessions

- Status: design draft; not conformance-ready
- Target contract version: `2.0.0`
- Parent contract: [FF1 communication API v2](ff1-v2-api-contract.md)

This prose is a proposed security profile, not a normative conformance
artifact. The capitalized requirement keywords state the intended v2 design.
Before `2.0.0` becomes normative, the repository MUST publish the
machine-readable schemas and API descriptions, positive and negative security
fixtures, and automated transport-parity tests required by the parent contract
and migration plan. No broker, FF1, or controller may claim FF1 API v2
conformance from this prose document alone.

## 1. Scope

This profile defines how a controller becomes authorized to control one FF1 and
how it obtains bounded access to the MQTT and LAN bindings of the FF1 v2 API.
It applies to:

- the Feral File mobile app;
- ff1-cli and other installed integrations;
- web clients; and
- temporary agent processes.

The profile has one trust model for every client. A mobile app, CLI, or
installed integration normally receives a **persistent controller
enrollment**. A web client or temporary agent normally claims a guest session.
Every live MQTT control connection uses a finite access session issued by FF1.
A LAN connection instead uses mutual TLS plus a connection-local LAN
authorization lease; a guest LAN lease remains bounded by its underlying guest
access session. MQTT is the primary invitation and control binding; an HTTPS
invitation adapter preserves the same ceremony when the broker or internet is
unavailable, subject to the trusted-time and
`lanOfflineAfterPowerLoss` rules in sections 12 and 13.2.

Persistent authorization is REQUIRED for enrolled controllers. A user scans
the enrollment QR once for each app or CLI installation; access-session expiry,
app restart, FF1 restart, network change, or MQTT reconnect MUST NOT cause
another QR ceremony. This document does not call that relationship a
"permanent session" because MQTT 5 Session is a transport term with Clean Start
and Session Expiry Interval semantics. The persistent relationship is a
**controller enrollment**, not an MQTT Session or non-expiring bearer token.

The keywords MUST, MUST NOT, REQUIRED, SHOULD, SHOULD NOT, and MAY are used as
described by RFC 2119 and RFC 8174 to express intended requirements for the
eventual normative profile.

## 2. Standards

This profile uses:

- [MQTT 5.0](https://docs.oasis-open.org/mqtt/mqtt/v5.0/mqtt-v5.0.html),
  including User Name, Password, Enhanced Authentication, the AUTH Control
  Packet, Authentication Method, Authentication Data, Response Topic,
  Correlation Data, Message Expiry Interval, and Reason Codes;
- [TLS 1.3 (RFC 8446)](https://www.rfc-editor.org/rfc/rfc8446.html),
  [X.509 (RFC 5280)](https://www.rfc-editor.org/rfc/rfc5280.html), and the
  [TPM 2.0 Library specification](https://trustedcomputinggroup.org/resource/tpm-library-specification/);
- [JWT (RFC 7519)](https://www.rfc-editor.org/rfc/rfc7519.html),
  [JWS (RFC 7515)](https://www.rfc-editor.org/rfc/rfc7515.html),
  [JWE (RFC 7516)](https://www.rfc-editor.org/rfc/rfc7516.html), and
  [JWK thumbprints (RFC 7638)](https://www.rfc-editor.org/rfc/rfc7638.html);
- [JWT confirmation (RFC 7800)](https://www.rfc-editor.org/rfc/rfc7800.html);
- [Bearer Token Usage (RFC 6750)](https://www.rfc-editor.org/rfc/rfc6750.html);
- [Web Origin (RFC 6454)](https://www.rfc-editor.org/rfc/rfc6454.html);
- [JSON Canonicalization Scheme (RFC 8785)](https://www.rfc-editor.org/rfc/rfc8785.html);
  and
- [PKCS #10 (RFC 2986)](https://www.rfc-editor.org/rfc/rfc2986.html).

Invitation and enrollment-only connections use MQTT 5 User Name and Password
fields and do not send an AUTH Control Packet. Access-session connections use
MQTT 5 Enhanced Authentication and the AUTH Control Packet to prove possession
of the controller signing key. The exact Authentication Method
`FF1-JWT-ES256-PoP` and the JSON semantics assigned to Authentication Data are
FF customizations built on the standard MQTT exchange; they do not add or
change an MQTT Control Packet, property, or Reason Code.

MQTT 5 defines those fields but does not define JWT validation, FF1 issuer-key
registration, the ACL vocabulary in this profile, or a broker ACL revocation
barrier. Those are FF protocol customizations built from the listed standards.
A broker selected for the final profile is acceptable only if it can validate
the registered FF1 issuer, enforce the exact Topic Name ACLs, and provide the
authorization-barrier semantics in section 13.4 without changing the public
MQTT wire protocol.

## 3. Vocabulary

| Term | Meaning |
|---|---|
| Controller | One app installation, CLI installation, integration, web client, or agent process with its own P-256 signing and encryption key pairs. |
| Controller enrollment | A persistent, revocable FF1 authorization binding one controller installation's public keys to a role and maximum scopes. Mobile, CLI, and installed integrations normally use enrollment. |
| Invitation | A short-lived, one-time FF1 authorization encoded in a QR payload. It authorizes exactly one enrollment claim or guest-session claim. |
| Access session | A finite, persisted FF1 authorization record used for normal MQTT control and as the underlying authorization for an optional guest LAN connection. |
| LAN authorization lease | A finite, connection-local authorization derived from an enrolled-controller certificate or existing guest access session; it is not persisted or assigned a `sessionId`. |
| Enrolled-controller session | A renewable access session issued to an active controller enrollment. |
| Guest session | A non-renewable access session created by an enrolled controller for a web client, temporary agent, or other bounded requester. |
| Inviting controller | The enrolled controller that authorizes a controller-enrollment or guest-session invitation. |
| Owner controller | The controller that holds role `owner`. The first owner enrollment is created from the FF1 screen. |

`pairing` is not a protocol noun in v2. The former client-kind-specific label is
retired. User interfaces MAY use the verb "pair" in explanatory copy, but
operation names and JSON use `enrollment`, `invitation`, and `session`.

## 4. Trust and key model

### 4.1 FF1 keys

FF1 has three distinct non-exportable ECDSA P-256 keys:

1. a TPM-backed hardware device-identity and proof-of-possession key;
2. a TPM-backed controller-credential issuer key; and
3. a device-local controller CA key for LAN client certificates.

The keys MUST NOT be reused across purposes. The device registry binds the
credential-issuer public key and its `kid` to the TPM-attested device identity.
The MQTT broker validates FF1-issued JWS credentials from this registered public
key without a per-session authentication callback to FF1 or a control-plane
session lookup.

The hardware device-identity key MAY survive factory reset as the stable TPM
proof-of-possession and attestation anchor; this profile preserves it through
reset cleanup. Its runtime X.509 device certificate and broker certificate
registration are separate, reset-scoped credentials. Factory reset MUST revoke
and replace that certificate and registration even when the underlying TPM key
is retained. The retained key alone cannot open the public device MQTT Network
Connection.

FF1 is authoritative for invitation consumption, controller enrollment,
access-session creation, expiry, and revocation. Broker validation is an outer
transport enforcement layer and never changes FF1 state.

### 4.2 Controller keys

Every controller creates two distinct P-256 key pairs:

1. an ECDSA signing key for claim and session-request proof; and
2. an ECDH encryption key for JWE credential delivery.

Private keys MUST be non-exportable when the platform provides suitable secure
storage. A web client uses Web Crypto and MUST keep both private keys
non-extractable. A mobile app, CLI, or installed integration stores enrolled
controller keys in the operating-system credential store or equivalent
hardware-backed store. A guest web client or temporary agent keeps its keys in
session memory and destroys them when the guest session ends.

An enrollment, invitation claim, or session credential is bound to the RFC 7638
thumbprints of both public JWKs. A key MUST NOT be reused for both signing and
ECDH. Controllers never share keys, access tokens, enrollment credentials, or
LAN certificates.

Each mobile app installation, CLI installation, or installed integration is a
different controller with its own `controllerId`, keys, enrollment credential,
label, scopes, sessions, and optional LAN certificate. FF1 supports up to 32
simultaneously active controller enrollments and 16 simultaneously active
access sessions. Reaching either limit returns `busy`; it never evicts or
revokes an existing controller.

### 4.3 Roles

An enrollment has role `owner` or `delegate`.

- The first successfully claimed enrollment is `owner`.
- A later enrollment is `delegate` unless an owner-transfer ceremony explicitly
  replaces the owner.
- An owner enrollment always includes `controllers:manage` and
  `sessions:manage`; neither scope can be removed while the enrollment is owner.
- `controllers:manage` is owner-only and MUST NOT be granted to a delegate.
- A delegate cannot grant a role or scope it does not hold.
- The final active owner enrollment cannot be revoked through the network API.

A guest session has no role. It has only its explicitly granted scopes.

One FF1 has one active owner enrollment and may have multiple delegate
enrollments. A delegate mobile app may receive the same playback, display,
input, state, and guest-session scopes as the owner. The role distinction
controls enrollment administration and owner transfer; it does not force
secondary phones or CLIs into guest sessions.

A Feral File account or owner-contact record MAY be associated with an owner
enrollment outside this protocol. It is not an MQTT credential and is not
required for FF1 to recognize the enrolled owner controller.

## 5. Authorization objects

### 5.1 Persistent controller enrollment

An enrollment is the following closed FF1 record:

```json
{
  "controllerId": "019bf2c1-baae-7379-af80-3a328bec5e57",
  "label": "Living room iPhone",
  "clientKind": "mobile",
  "signingKeyThumbprint": "nZ8Q2xA6S8v3P4rM8BfBNPWYd8kI9JQsp2cKXQwH_3E",
  "encryptionKeyThumbprint": "Zgwp7YvYjHtBtWnO6fOuJnWHZyFJlgGlVxXEQbBUt0A",
  "role": "owner",
  "scopes": ["state:read", "playback:control", "controllers:manage", "sessions:manage"],
  "createdAt": "2026-07-21T08:00:00.000Z",
  "credentialExpiresAt": "2027-07-21T08:00:00.000Z",
  "status": "active"
}
```

`clientKind` is `mobile|cli|integration`. The enrollment is the persistent
authorization and remains active until explicitly revoked, replaced during
owner transfer, or cleared by factory reset. It survives app restart, FF1
restart, access-session expiry, MQTT disconnect, broker outage, and LAN or
internet path changes.

Enrollment credentials expire after at most 366 days, but rotation is silent
and does not repeat enrollment. An active controller MAY invoke
`controllers.renew-credential` at any time, no more than once in 24 hours, and
SHOULD rotate when fewer than 90 days remain. FF1 issues the replacement before
invalidating the prior credential. A mobile app or CLI performs this
rotation without user interaction.

If an installation remains unused until its enrollment credential expires, it
cannot establish a new session. A delegate then returns through one new
controller-enrollment invitation and receives a new controller ID. An owner
returns only through the physical owner-replacement ceremony in section 13.5.
This is exceptional recovery, not routine reauthentication.

An enrollment credential is a compact ES256 JWS signed by FF1. It authorizes
only the enrolled-session request and response Topic Names in section 8.1. It
does not authorize device state subscriptions or control commands.

Its protected header is the closed object
`{"alg":"ES256","kid":"<issuer-key-thumbprint>","typ":"ff1-enrollment+jwt"}`.
It has no unprotected header.

Its required claims are:

| Claim | Value or constraint |
|---|---|
| `iss` | `urn:ff:device:<deviceId>:controller-issuer` |
| `sub` | `urn:ff:controller:<controllerId>` |
| `aud` | exact broker audience |
| `iat`, `nbf`, `exp` | NumericDate; lifetime at most 366 days |
| `jti` | UUIDv7 unique to this credential version |
| `ff_device_id` | exact device ID |
| `ff_controller_id` | exact controller ID |
| `ff_mqtt_client_id` | `ff-enrollment-<controllerId>` |
| `ff_role` | `owner\|delegate` |
| `ff_scope_ceiling` | space-delimited enrollment scopes |
| `scope` | exactly `session:create` |
| `cnf.jkt` | signing-key JWK thumbprint |
| `ff_encryption_jkt` | encryption-key JWK thumbprint |

The broker validates the credential signature and claims but does not treat
`ff_scope_ceiling` as a control ACL. The only broker ACL is the exact
session-request and response pair for that controller. This restricted CONNECT
uses the credential as a bearer value; `cnf.jkt` is enforced by FF1 through the
signed controller proof on every session request in section 8.1. It is not
treated as proof of possession at CONNECT.

### 5.2 Access session

Every access session is a closed FF1 record:

```json
{
  "sessionId": "019bf2e0-6f8b-74ba-94b4-110bdd97d1d6",
  "sessionType": "enrolled_controller",
  "controllerId": "019bf2c1-baae-7379-af80-3a328bec5e57",
  "label": "Living room iPhone",
  "clientKind": "mobile",
  "signingKeyThumbprint": "nZ8Q2xA6S8v3P4rM8BfBNPWYd8kI9JQsp2cKXQwH_3E",
  "encryptionKeyThumbprint": "Zgwp7YvYjHtBtWnO6fOuJnWHZyFJlgGlVxXEQbBUt0A",
  "scopes": ["state:read", "playback:control"],
  "createdAt": "2026-07-21T08:15:00.000Z",
  "expiresAt": "2026-07-21T08:30:00.000Z",
  "status": "active"
}
```

`sessionType` is `enrolled_controller|guest`. An enrolled-controller session has
a maximum lifetime of 15 minutes and is renewable while its enrollment remains
active. A guest session has a requested lifetime from 300 through 86400 seconds,
defaults to 3600 seconds, and is never renewable. FF1 MAY grant a shorter
lifetime than requested.

Session status is `active|expired|revoked`. FF1 MUST reject a command when the
session is not active even if the broker has not yet disconnected the client.

### 5.3 Persistent authorization and finite access credentials

The controller enrollment is the persistent mechanism. No FF1 access token is
permanent. Enrolled controllers keep private keys and an enrollment credential
that can request new bounded access sessions. Guest controllers receive only
one non-renewable access session.

Access-session issuance and renewal are transport maintenance, not a user login
or approval step. A controller requests a new session when it becomes active
and, while continuously connected, before the current session expires. This
split limits the value of a stolen access credential while letting all enrolled
mobile apps, CLIs, and integrations reconnect for the life of their enrollment,
subject only to silent credential rotation, without repeating the QR ceremony.

## 6. Invitation protocol

### 6.1 Invitation types

FF1 issues three invitation types:

| Type | Who may create it | Claim result |
|---|---|---|
| `owner_enrollment` | FF1 physical UI while unclaimed or replacing the owner | owner enrollment plus first enrolled-controller session |
| `controller_enrollment` | enrolled owner with `controllers:manage` | delegate enrollment plus first enrolled-controller session |
| `guest_session` | enrolled controller with `sessions:manage` | one guest session |

Only one owner-enrollment invitation may exist. FF1 permits at most one active
enrollment invitation, five active guest-session invitations, and sixteen
active access sessions.

### 6.2 QR payload

The QR contains this URI:

```text
ff1-control:v2#<base64url(JCS(ControllerInvitation))>
```

`ControllerInvitation` is the following closed object:

```json
{
  "version": 2,
  "deviceId": "FF1-01234567",
  "brokerUri": "wss://control.example.org:443/mqtt",
  "brokerAudience": "https://control.example.org/mqtt",
  "issuerJwk": {
    "kty": "EC",
    "crv": "P-256",
    "x": "...",
    "y": "..."
  },
  "lanUri": "https://FF1-01234567.local",
  "lanSpkiSha256": "0lx5SHsC2KQG2e9mY7Fu5tMOvO4kDqYjQVlHhDM9Slg",
  "invitationId": "019bf2e9-c52c-7150-89fa-19592c207c75",
  "claimId": "019bf2ea-10de-7c31-90d0-56c02851b7da",
  "invitationType": "guest_session",
  "clientKind": "web",
  "label": "Museum kiosk",
  "origin": "https://museum.example",
  "mqttClientId": "ff-invitation-019bf2e9-c52c-7150-89fa-19592c207c75",
  "credential": "eyJhbGciOiJFUzI1NiIsImtpZCI6IkZGMT...",
  "scopeCeiling": ["state:read", "playback:control"],
  "sessionSeconds": 3600,
  "expiresAt": "2026-07-21T08:20:00.000Z"
}
```

`issuerJwk` is the FF1 controller-credential issuer public key and is a closed
public JWK containing exactly `kty`, `crv`, `x`, and `y`. Its RFC 7638
thumbprint MUST equal the credential protected-header `kid`. `clientKind` is
`mobile|cli|integration` for enrollment and `web|agent|integration` for a guest
session. `claimId` is created by FF1 and fixes the one claim/response/ack Topic
Name set. `label` is optional. `origin` is required only for `web` and is
omitted otherwise. `grantedRole` is required and is
`owner|delegate` for an enrollment invitation; it is omitted for
`guest_session`. `sessionSeconds` is required only for `guest_session`.
`brokerAudience` is the exact StringOrURI audience configured by that broker;
it is not inferred from the host name. Optional fields are omitted, never
`null`.

The QR therefore contains everything required to reach the broker or the
pinned LAN claim route and submit one claim. `brokerUri` is REQUIRED. `lanUri`
and `lanSpkiSha256` are both REQUIRED when FF1 advertises the LAN adapter.
Before connecting, the claimant MUST verify the invitation JWS with
`issuerJwk`, recompute the signed invitation digest over the QR object, verify
the explicit ID and lifetime claims, and reject any mismatch. The QR is
the trust transfer from the physically displayed FF1 invitation; it does not
require an FF-specific authentication service. The credential is secret. It
MUST NOT appear in logs, analytics,
Referer headers, controller state, command responses, or screenshots retained
after the invitation closes. A headless client MAY receive the exact URI by an
out-of-band copy operation; the wire contract is unchanged.

### 6.3 Invitation credential

The QR credential is a compact ES256 JWS. Its protected header is the closed
object
`{"alg":"ES256","kid":"<issuer-key-thumbprint>","typ":"ff1-invitation+jwt"}`,
and it has no unprotected header. Its payload has these required claims:

| Claim | Value or constraint |
|---|---|
| `iss` | `urn:ff:device:<deviceId>:controller-issuer` |
| `sub` | `urn:ff:invitation:<invitationId>` |
| `aud` | exact QR `brokerAudience`; when LAN is present, an array also containing `urn:ff:device:<deviceId>:invitation` |
| `iat`, `nbf`, `exp` | NumericDate; lifetime at most 300 seconds |
| `jti` | UUIDv7 unique to the invitation |
| `ff_device_id` | exact device ID |
| `ff_invitation_id` | exact invitation ID |
| `ff_claim_id` | exact QR claim ID |
| `ff_invitation_type` | `owner_enrollment\|controller_enrollment\|guest_session` |
| `ff_mqtt_client_id` | exact QR Client Identifier |
| `ff_invitation_digest` | base64url SHA-256 of JCS(`ControllerInvitation` with `credential` omitted) |
| `scope` | exactly `invitation:claim` |

The digest covers the broker URI and audience, issuer JWK, LAN URI and SPKI pin,
IDs, client kind, label, declared web origin, role, scope ceiling, guest
lifetime, and expiry without duplicating those fields in the JWS. The complete
`ff1-control:` URI MUST be no more than 2331 UTF-8 bytes. Producers reject an
invitation that cannot meet that bound; labels are omitted before any required
field is shortened or removed.

The MQTT User Name is `invitationId`; Password is the ASCII JWS; Client
Identifier is `ff_mqtt_client_id`. The broker ACL permits only the exact claim,
response, and acknowledgement Topic Names for that invitation. The LAN adapter
accepts the same JWS only in `Authorization: Bearer <JWS>` on the exact
invitation routes below.

### 6.4 Claim Topic Names

| Topic Name | Publisher -> subscriber | QoS | Retain | Expiry |
|---|---|---:|---:|---:|
| `ff/v2/devices/{deviceId}/invitations/{invitationId}/claims/{claimId}` | claimant -> FF1 | 1 | no | invitation remainder |
| `ff/v2/devices/{deviceId}/invitations/{invitationId}/responses/{claimId}` | FF1 -> claimant | 1 | yes | invitation remainder |
| `ff/v2/devices/{deviceId}/invitations/{invitationId}/acks/{claimId}` | claimant -> FF1 | 1 | no | invitation remainder |

The claimant uses the QR `claimId` and subscribes to its exact response Topic Name before publishing the
claim. The claim PUBLISH sets Response Topic to that response Topic Name and
Correlation Data to the 16 UUID bytes of `claimId`. The response echoes the
same Correlation Data. The claimant acknowledges successful decryption; FF1
then clears the retained response by publishing a zero-byte retained
Application Message.

MQTT is the default claim binding. A controller MAY use the LAN binding only
when it is on an explicitly trusted network or the user selects offline setup.
It verifies `lanSpkiSha256` before sending the invitation credential. The LAN
server requests but does not require a client certificate during TLS; only
these routes accept an absent certificate:

| HTTPS operation | Request and response |
|---|---|
| `POST /ff/v2/invitations/{invitationId}/claims/{claimId}` | `Authorization: Bearer <JWS>` and the same claim JSON; returns the same response JSON |
| `POST /ff/v2/invitations/{invitationId}/acks/{claimId}` | same authorization and acknowledgement JSON; returns that JSON with `status: acknowledged` |

A presented client certificate is still validated normally. The invitation
exception never converts an invalid certificate into an anonymous request.
MQTT and HTTPS reach one atomic invitation store, so consuming an invitation on
one binding prevents its use on the other.

### 6.5 Claim request

The claim payload is:

```json
{
  "apiVersion": "ff/v2",
  "invitationId": "019bf2e9-c52c-7150-89fa-19592c207c75",
  "claimId": "019bf2ea-10de-7c31-90d0-56c02851b7da",
  "controller": {
    "controllerId": "019bf2eb-506d-7be1-9ab5-2effd08f2bc3",
    "label": "Museum kiosk",
    "clientKind": "web",
    "signingKeyJwk": {
      "kty": "EC",
      "crv": "P-256",
      "x": "...",
      "y": "..."
    },
    "encryptionKeyJwk": {
      "kty": "EC",
      "crv": "P-256",
      "x": "...",
      "y": "..."
    },
    "origin": "https://museum.example"
  },
  "requestedScopes": ["playback:control"],
  "proof": "eyJhbGciOiJFUzI1NiIsImtpZCI6Ii4uLiIsInR5cCI6ImZmMS1wcm9vZitqd3MifQ..MEUCIQ..."
}
```

`clientKind` is `mobile|cli|integration|web|agent`. `origin` is REQUIRED only
for `web`, MUST be a normalized HTTPS origin under RFC 6454, and MUST be omitted
otherwise. Each public JWK is a closed object containing exactly `kty`, `crv`,
`x`, and `y`; `kty` is `EC`, `crv` is `P-256`, each coordinate is a 32-byte
base64url value without padding, and FF1 MUST validate the curve point.
The closed `controller` object additionally permits `csrPem` as an OPTIONAL
member alongside `signingKeyJwk` and `encryptionKeyJwk`. It MUST contain the
same public key as `signingKeyJwk` and requests a LAN client certificate. Web
clients MUST omit it; `agent` and `integration` guests MAY include it. No
top-level `csrPem` member is permitted. `proof` is the detached compact JWS
defined in section 6.5.1.

The requested scopes MUST be a subset of the invitation's `scopeCeiling`. For
MQTT, the MQTT Server validates the invitation credential and exact Topic Name
ACL before forwarding the claim. For HTTPS, FF1 validates the Bearer credential
directly. FF1 then validates its live invitation record, claim schema, proof,
exact claim ID, invited client kind, label, role, origin, and scope ceiling
before atomically consuming the invitation. The first valid claim wins.
Repeating the byte-equivalent valid claim returns the same response; another
claim returns `invitation_consumed`.

An `owner_enrollment` invitation and claim MUST include `controllers:manage`
and `sessions:manage`. A `controller_enrollment` invitation and claim MUST NOT
include `controllers:manage`.

#### 6.5.1 Controller proof

Every `proof` in this profile is a detached JWS in compact serialization as
defined by RFC 7515 Appendix F. Its protected header is the closed object
`{"alg":"ES256","kid":"<signing-key-thumbprint>","typ":"ff1-proof+jws"}`.
There is no unprotected header and no `b64: false` extension. The detached JWS
has the form `BASE64URL(protected-header)..BASE64URL(signature)`; the omitted
payload is the RFC 8785 JCS UTF-8 bytes of the complete containing request with
the proof member or members omitted. The verifier reconstructs the normal
base64url-encoded JWS payload before signature verification.

### 6.6 Claim response

Success is:

```json
{
  "apiVersion": "ff/v2",
  "invitationId": "019bf2e9-c52c-7150-89fa-19592c207c75",
  "claimId": "019bf2ea-10de-7c31-90d0-56c02851b7da",
  "status": "granted",
  "credentialEnvelope": "eyJraWQiOiIwMTliZi4uLiIsImVuYyI6IkEyNTZHQ00iLCJhbGciOiJFQ0RILUVTIn0...",
  "expiresAt": "2026-07-21T09:15:00.000Z"
}
```

`credentialEnvelope` is a compact JWE using `ECDH-ES` and `A256GCM`, encrypted
to `encryptionKeyJwk`. Its protected header contains exactly `alg`, `enc`,
`epk`, `kid`, and `typ`; their values are `ECDH-ES`, `A256GCM`, the ephemeral
public EC JWK, the recipient encryption-key thumbprint, and
`ff1-credential+jwe`. It has no unprotected header, compression, or external
additional authenticated data.

For an enrollment claim, the JWE plaintext is this closed object:

```json
{
  "apiVersion": "ff/v2",
  "credentialType": "controller_enrollment",
  "deviceId": "FF1-01234567",
  "controllerId": "019bf2eb-506d-7be1-9ab5-2effd08f2bc3",
  "role": "delegate",
  "scopes": ["state:read", "playback:control"],
  "enrollmentCredential": "eyJhbGciOiJFUzI1NiIsImtpZCI6IkZGMT...",
  "credentialExpiresAt": "2027-07-21T08:15:00.000Z",
  "session": {
    "sessionId": "019bf2f0-340d-7761-8035-6cc79b6172b9",
    "accessCredential": "eyJhbGciOiJFUzI1NiIsImtpZCI6IkZGMT...",
    "expiresAt": "2026-07-21T08:30:00.000Z"
  }
}
```

For a guest claim, `credentialType` is `guest_session`, and
`enrollmentCredential`, `credentialExpiresAt`, and `role` are omitted. The
remaining `session`, `deviceId`, `controllerId`, and `scopes` members have the
same meanings. Either plaintext MAY contain a `lan` object when requested and
available; it contains exactly `certificatePem`, `chainPem` as a nonempty
string array, and `expiresAt`. The top-level success `expiresAt` is always the
returned access session's expiry.

Failure is the same closed object with `status: "rejected"`, no credential
envelope, and a required closed `error` object containing `code` and
`retryable`. It is `true` only for `internal_error`. `error.code` is one of
`invalid_claim|invitation_expired|invitation_consumed|scope_denied|origin_denied|internal_error`.
The response never reveals another claimant's identity.

### 6.7 Claim acknowledgement

After decrypting and validating the credential envelope, the claimant publishes
or posts this closed acknowledgement:

```json
{
  "apiVersion": "ff/v2",
  "invitationId": "019bf2e9-c52c-7150-89fa-19592c207c75",
  "claimId": "019bf2ea-10de-7c31-90d0-56c02851b7da",
  "status": "received"
}
```

For HTTPS, success returns the same object with `status: "acknowledged"`. For
MQTT, a valid acknowledgement causes FF1 to clear the retained claim response.
No acknowledgement changes the already-created enrollment or access session.

## 7. Access-session credentials

An access-session credential is a compact ES256 JWS signed by FF1. Its protected
header is the closed object
`{"alg":"ES256","kid":"<issuer-key-thumbprint>","typ":"ff1-access+jwt"}`, and it has
no unprotected header. Its payload has these required claims:

| Claim | Value or constraint |
|---|---|
| `iss` | `urn:ff:device:<deviceId>:controller-issuer` |
| `sub` | `urn:ff:session:<sessionId>` |
| `aud` | exact broker audience |
| `iat`, `nbf`, `exp` | NumericDate selected by FF1 |
| `jti` | UUIDv7 unique to this issued credential |
| `ff_device_id` | exact device ID |
| `ff_session_id` | exact session ID |
| `ff_session_type` | `enrolled_controller\|guest` |
| `ff_controller_id` | controller ID from the claim or enrollment |
| `ff_mqtt_client_id` | `ff-session-<sessionId>` |
| `ff_role` | `owner\|delegate` for enrolled sessions; omitted for guest |
| `scope` | space-delimited granted scopes in lexical order |
| `cnf.jwk` | controller signing public JWK |
| `ff_origin` | normalized HTTPS origin used only for the web MQTT-over-WSS browser defense in depth; otherwise omitted |

`cnf.jwk` follows RFC 7800. It is the closed public JWK
`{"kty":"EC","crv":"P-256","x":"<base64url>","y":"<base64url>"}` and
MUST NOT contain `d` or any other member. It is the public key corresponding to
the controller signing private key accepted during invitation claim or
enrollment. Supplying the public JWK, instead of only its thumbprint, lets a
broker validate proof of possession from the FF1-signed credential without a
per-session authentication service.

### 7.1 Access-session CONNECT

An access-session controller sends CONNECT with:

- Client Identifier = the credential's exact `ff_mqtt_client_id`;
- User Name = `sessionId`;
- Password absent;
- Authentication Method = the exact case-sensitive UTF-8 string
  `FF1-JWT-ES256-PoP`; and
- Authentication Data = the UTF-8 encoding of this closed JSON object:

```json
{
  "accessCredential": "eyJhbGciOiJFUzI1NiIsImtpZCI6IkZGMT..."
}
```

Authentication Data is MQTT Binary Data but its value in this profile MUST be
well-formed UTF-8 JSON with no byte-order mark. `FF1-JWT-ES256-PoP` is an
FF-defined Authentication Method. Its name and the JSON objects in this section
are the only custom semantics; CONNECT, AUTH, CONNACK, Authentication Method,
Authentication Data, and all Reason Codes retain their MQTT 5 meanings.

The broker validates the registered FF1 issuer, ES256 signature, audience,
`iat`, `nbf`, `exp`, exact User Name, exact Client Identifier, device ID,
session type, `cnf.jwk`, and ACL mapping before issuing a challenge. For a web
access session over MQTT-over-WSS, it additionally applies the exact Origin
comparison in section 10 as browser-only defense in depth. It rejects an access
credential whose controller or session fields conflict, whose `cnf` object has
any confirmation member other than `jwk`, or whose public JWK is not the key
recorded by FF1 in the signed credential.

### 7.2 Enhanced Authentication exchange

The access-session connection uses this single-round MQTT 5 Enhanced
Authentication exchange:

1. After accepting the access credential provisionally, the broker sends AUTH
   with Reason Code `0x18` (Continue authentication), Authentication Method
   `FF1-JWT-ES256-PoP`, and Authentication Data containing this closed UTF-8
   JSON object:

   ```json
   {
     "challenge": "wb8pZq5qQkDYXrVhC5J2Qqx4vtBWFjkbGvk3GvMIbTo",
     "audience": "https://control.example.org/mqtt",
     "expiresAt": "2026-07-21T08:15:30.000Z"
   }
   ```

2. The controller verifies that Authentication Method is unchanged, `audience`
   equals both its configured broker audience and the access credential's exact
   `aud`, and `expiresAt` has not passed. It then sends AUTH with Reason Code
   `0x18`, the same Authentication Method, and Authentication Data containing
   this closed UTF-8 JSON object:

   ```json
   {
     "proof": "eyJhbGciOiJFUzI1NiIsInR5cCI6ImZmMS1tcXR0LWF1dGgrand0In0.eyJpc3MiOiIuLi4ifQ.MEUCIQ..."
   }
   ```

3. The broker validates the proof and atomically consumes the challenge. On
   success it sends CONNACK with Reason Code `0x00` (Success), Session Present
   `0`, and Authentication Method `FF1-JWT-ES256-PoP`. Authentication Data is
   absent from the successful CONNACK. The broker installs the credential's
   exact Topic Name ACL only after this validation succeeds.

A provisional connection does not take ownership of its Client Identifier.
When an authenticated Network Connection already uses the same Client
Identifier, the broker MUST leave that existing Network Connection, MQTT
Session, and ACL unchanged until the new connection's proof succeeds. Only
after proof succeeds may the broker apply the MQTT Client Identifier collision
rule, send the existing client DISCONNECT `0x8E` (Session taken over), and
accept the new connection. A failed provisional connection MUST NOT disconnect
or otherwise mutate the existing connection, Session, or ACL.

The controller MUST send no Control Packet other than the required AUTH or a
DISCONNECT between CONNECT and CONNACK. The broker MUST send no second challenge
for this method. The controller MUST NOT use AUTH Reason Code `0x19`
(Re-authenticate); access renewal creates a new access session and Network
Connection.

The challenge is 32 cryptographically random octets encoded base64url without
padding. It is unpredictable, belongs to exactly one Network Connection and
access-credential `jti`, expires no later than 30 seconds after issuance and no
later than the access credential, and becomes invalid when the connection
closes. The broker stores enough state until expiry to reject reuse and MUST
atomically mark the challenge consumed before sending successful CONNACK. A
proof `jti` also MUST NOT be accepted more than once.

### 7.3 Controller proof

`proof` is a compact ES256 JWS signed by the private key corresponding to the
access credential's `cnf.jwk`. Its protected header is the closed object
`{"alg":"ES256","typ":"ff1-mqtt-auth+jwt"}`. It has no unprotected header.
Its payload is a closed JWT Claims Set with these required claims:

| Claim | Value or constraint |
|---|---|
| `iss` | `urn:ff:controller:<controllerId>` using the access credential's controller ID |
| `sub` | `urn:ff:session:<sessionId>` |
| `aud` | exact broker audience from both the access credential and challenge |
| `iat` | NumericDate no more than 5 seconds in the future |
| `exp` | NumericDate later than `iat`, no more than 30 seconds after `iat`, and no later than the challenge or access credential expiry |
| `jti` | a new UUIDv7 |
| `ff_device_id` | exact access-credential device ID |
| `ff_session_id` | exact access-credential session ID |
| `ff_mqtt_client_id` | exact CONNECT Client Identifier |
| `ff_auth_challenge` | exact base64url challenge string received from the broker |

The broker verifies the JWS with only `cnf.jwk`, requires every binding above,
and rejects extra protected-header or payload members. The access credential is
therefore sender-constrained: possessing the JWS without the controller signing
private key is insufficient to establish an access-session connection.

### 7.4 Failure and completion

The broker uses these exact MQTT 5 outcomes before successful CONNACK:

| Condition | Broker outcome |
|---|---|
| Authentication Method is unsupported or not exactly `FF1-JWT-ES256-PoP` | CONNACK `0x8C` (Bad authentication method), then close the Network Connection |
| Required profile field is absent, Password is present, Authentication Data is not the required closed JSON, or the access credential, claims, key, or ACL mapping is invalid | CONNACK `0x87` (Not authorized), then close the Network Connection |
| A web access session omits WebSocket Origin or the presented value does not exactly equal `ff_origin` | CONNACK `0x87` (Not authorized), then close the Network Connection |
| Challenge expires, proof validation fails, or a challenge or proof `jti` is replayed | CONNACK `0x87` (Not authorized), then close the Network Connection |
| CONNECT is malformed | CONNACK `0x81` (Malformed Packet) when a valid CONNACK can be encoded, then close the Network Connection |
| CONNECT causes an MQTT Protocol Error | CONNACK `0x82` (Protocol Error) when a valid CONNACK can be encoded, then close the Network Connection |
| AUTH is malformed | CONNACK `0x81` (Malformed Packet) when a valid CONNACK can be encoded, then close the Network Connection |
| AUTH omits or changes Authentication Method, uses a Reason Code other than `0x18`, repeats a single-use property, or violates the exchange order | CONNACK `0x82` (Protocol Error) when a valid CONNACK can be encoded, then close the Network Connection |

These failures occur before successful CONNACK, so the broker MUST NOT send a
DISCONNECT Control Packet. If malformed input prevents the broker from encoding
a valid failure CONNACK, it closes the Network Connection without sending an
MQTT Control Packet.

An access-session client treats any CONNACK other than `0x00`, or a successful
CONNACK without the exact Authentication Method, as connection failure and
closes the Network Connection. It never publishes or subscribes before
successful CONNACK. A client abort MAY send DISCONNECT before closing as MQTT 5
allows. Reason String and User Property are diagnostic only and never alter the
outcome above.

After connection, the broker disconnects the client at access credential `exp`
with DISCONNECT Reason Code `0x87` (Not authorized). If a client nevertheless
sends AUTH `0x19`, the broker sends DISCONNECT `0x87` and closes the Network
Connection. FF1 independently rejects a revoked, expired, or otherwise inactive
session for every delivered command.

The credential's publish ACL is the exact prefix
`ff/v2/devices/{deviceId}/sessions/{sessionId}/commands/`; its subscribe ACLs
are derived from the granted scopes and its exact controller response Topic
Name. FF1 obtains the authoritative session ID from the command Topic Name and
never from a controller-supplied JSON identity field.

An access credential is delivered only in a JWE encrypted to the controller
encryption key. Tokens MUST NOT be persisted by web or guest clients. Native enrolled
controllers MAY persist only the enrollment credential and controller private keys; access
credentials SHOULD remain memory-only.

## 8. Enrolled-controller session creation

### 8.1 Session request

An enrolled controller uses its enrollment credential to connect with:

- User Name = `controllerId`;
- Password = enrollment-credential JWS; and
- Client Identifier = `ff-enrollment-<controllerId>`.

That connection authorizes no device control. It authorizes only:

| Topic Name | Publisher -> subscriber | QoS | Retain | Expiry |
|---|---|---:|---:|---:|
| `ff/v2/devices/{deviceId}/controllers/{controllerId}/session-requests/{requestId}` | controller -> FF1 | 1 | no | 60 seconds |
| `ff/v2/devices/{deviceId}/controllers/{controllerId}/session-responses/{requestId}` | FF1 -> controller | 1 | no | 60 seconds |

The request is:

```json
{
  "apiVersion": "ff/v2",
  "requestId": "019bf2ef-398a-7716-8ec1-c71bc5f41f72",
  "controllerId": "019bf2c1-baae-7379-af80-3a328bec5e57",
  "requestedScopes": ["state:read", "playback:control"],
  "createdAt": "2026-07-21T08:15:00.000Z",
  "nonce": "vARjK7Fyr7K-8f01AN0K7g",
  "proof": "eyJhbGciOiJFUzI1NiIsImtpZCI6Ii4uLiIsInR5cCI6ImZmMS1wcm9vZitqd3MifQ..MEQCID..."
}
```

`createdAt` MUST be no more than 30 seconds in the future or 60 seconds in the
past under FF1 trusted time. `nonce` is 16 random bytes encoded base64url without padding. `proof` is the
detached compact JWS from section 6.5.1. FF1 rejects a nonce already used by
that controller within 10 minutes. The controller first subscribes to the exact
response Topic Name, then publishes with that Topic Name as Response Topic and
the 16 UUID bytes of `requestId` as Correlation Data. FF1 echoes the Correlation
Data.

`requestId` is idempotent for that controller. FF1 returns the byte-equivalent
stored response for an identical canonical request until the issued session
expires; the idempotency lookup occurs before nonce-reuse rejection. The same
request ID with a different canonical request returns `duplicate_conflict`.
After reconnect, a controller reuses the original request ID and nonce rather
than creating an orphan access session.

### 8.2 Session response

FF1 verifies the enrollment status, enrollment-credential expiry, proof,
requested scope subset, and trusted time. It returns a JWE-encrypted
access-session credential and expiry in this closed response:

```json
{
  "apiVersion": "ff/v2",
  "requestId": "019bf2ef-398a-7716-8ec1-c71bc5f41f72",
  "status": "granted",
  "sessionId": "019bf2f0-340d-7761-8035-6cc79b6172b9",
  "credentialEnvelope": "eyJraWQiOiIwMTliZi4uLiIsImVuYyI6IkEyNTZHQ00iLCJhbGciOiJFQ0RILUVTIn0...",
  "expiresAt": "2026-07-21T08:30:00.000Z"
}
```

A rejected response omits `sessionId`, `credentialEnvelope`, and `expiresAt`,
sets `status: "rejected"`, and contains the closed `error` object from section
6.6 with code
`invalid_claim|duplicate_conflict|expired|forbidden|scope_denied|clock_unsynchronized|internal_error`.
The controller disconnects the enrollment-only connection and reconnects using
section 7 after success.

The successful `credentialEnvelope` plaintext is this closed object:

```json
{
  "apiVersion": "ff/v2",
  "credentialType": "access_session",
  "deviceId": "FF1-01234567",
  "controllerId": "019bf2c1-baae-7379-af80-3a328bec5e57",
  "sessionId": "019bf2f0-340d-7761-8035-6cc79b6172b9",
  "accessCredential": "eyJhbGciOiJFUzI1NiIsImtpZCI6IkZGMT...",
  "scopes": ["state:read", "playback:control"],
  "expiresAt": "2026-07-21T08:30:00.000Z"
}
```

An enrolled controller SHOULD request the next access session before the
current one expires. Session creation does not extend or mutate the current
MQTT Session; the controller performs a new MQTT CONNECT with Clean Start `1`
and Session Expiry Interval `0`.

## 9. Controller operations

The parent API defines these commands. This table summarizes their
authentication semantics; the exact closed `params`, `result`, and failure
schemas are in parent sections 11.6 and 11.7.

| Operation | Request | Result | Required authorization |
|---|---|---|---|
| `controllers.create-invitation` | `label`?: string(1..64); `clientKind`: `mobile\|cli\|integration`; `requestedScopes`: unique scope array; `expiresInSeconds`: int[60..300], default 300 | invitation ID, expiry, QR display status, sessions revision | enrolled owner with `controllers:manage` |
| `controllers.close-invitation` | `invitationId` | invitation ID, closed status, sessions revision | creator or owner with `controllers:manage` |
| `controllers.renew-credential` | `controllerId`; new signing and encryption public JWKs; `oldKeyProof` and `newKeyProof` | new enrollment credential envelope, `credentialExpiresAt`, optional LAN-certificate expiry, and `controllers` revision | same active controller; at most once per 24 hours |
| `controllers.set-scopes` | `controllerId`; unique nonempty `scopes` | controller ID, scopes, controllers-state revision | owner with `controllers:manage`; subset of caller grant |
| `controllers.revoke` | `controllerId`; `revokeCreatedGuestSessions`?: boolean, default false | after broker barrier ACK: controller ID, `status: revoked\|already_revoked`, and either the `controllers` revision alone or exact `controllers` plus `sessions` revisions when session state changed | owner with `controllers:manage`; not final owner; target cannot be the authenticated caller controller (`interaction_not_allowed`); `dependency_unavailable` while the durable barrier is pending |
| `sessions.create-invitation` | `label`?: string(1..64); `clientKind`: `web\|agent\|integration`; `requestedScopes`: unique scope array; `sessionSeconds`: int[300..86400], default 3600; `origin`?: normalized HTTPS origin | invitation ID, expiry, guest lifetime, QR display status, sessions revision | enrolled controller with `sessions:manage` |
| `sessions.close-invitation` | `invitationId` | invitation ID, closed status, sessions revision | creator or `sessions:manage` |
| `sessions.revoke` | `sessionId` | after broker barrier ACK: session ID, `status: revoked\|already_revoked`, sessions-state revision | creator or `sessions:manage`; target cannot be the current access session (`interaction_not_allowed`); `dependency_unavailable` while the durable barrier is pending |

Creating a guest invitation is the complete approval action. The invited
requester does not send a second approval request, and an enrolled controller
does not receive or relay the requester's credential. QR possession, one-time
claim, the preselected scope ceiling, and FF1 issuance form one ceremony.

For `controllers.renew-credential`, both proof fields use section 6.5.1 over
the complete command request envelope with both proof fields omitted. `oldKeyProof` is
signed by the currently enrolled signing key; `newKeyProof` is signed by the
submitted new signing key. The result envelope is encrypted to the submitted
new encryption key and uses the enrollment-claim plaintext shape with no
`session` member. FF1 commits the new credential and cached idempotent response
before replying. The immediately previous credential and LAN certificate have
a ten-minute rollover grace and are then invalidated; no older version is
accepted. Renewal does not change the controller ID, role, scope ceiling, or
other controllers' enrollments and sessions.

## 10. Scope policy

`controllers:manage` is owner-only and permits persistent controller enrollment administration.
`sessions:manage` permits creating guest-session invitations and viewing or
revoking guest sessions.

Guest sessions MUST NOT receive any of:

- `controllers:manage`;
- `sessions:manage`;
- `network:read`;
- `playlist:read`;
- `device:settings`;
- `system:update`;
- `system:power`;
- `system:reset`;
- `ssh:manage`; or
- `support:upload`.

The complete guest-eligible set is `state:read`, `playback:control`,
`display:control`, and `input:control`. The default guest grant is
`playback:control`. Every other
guest-eligible scope requires the inviting controller to request it explicitly
and to hold the same scope. A guest never obtains broader scopes than its
inviting controller.

For a web access session over MQTT-over-WSS, the broker MUST retain the Origin
value from the WebSocket opening handshake, require it to be present, and
compare its RFC 6454 ASCII serialization exactly with `ff_origin`. A missing or
mismatched value MUST be rejected as specified in section 7.4. This comparison
is browser-only defense in depth against cross-origin use of a credential.
Origin is client-supplied and can be forged by a non-browser client; it MUST NOT
be treated as proof of identity or key possession, a scope or ACL authorization
boundary, or a substitute for the `FF1-JWT-ES256-PoP` proof. Authorization MUST
depend on the signed credential, proof-of-possession key, exact session, device,
and Client Identifier, granted scopes, expiry, revocation state, and broker
Topic Name ACL. An agent guest has no origin claim and uses the same
credential, proof-of-possession, session, scope, expiry, revocation, and ACL
authorization model.

## 11. Session state and events

`state/controllers` contains persistent enrollments and is visible only to an
enrolled owner with `controllers:manage`. `state/sessions` contains active
invitations and access sessions and is visible to an enrolled controller with
`sessions:manage` or an owner with `controllers:manage`. Guest credentials,
invitation QR payloads, JWK coordinates, JWE payloads, and DP-1 Playlist source
URIs never appear in either resource.

The exact closed retained projections for all owner-enrollment,
controller-enrollment, guest-invitation, enrolled-controller-session, and
guest-session variants are defined in parent API section 12.7. Retained state
contains only open invitations and active sessions. Terminal invitation and
session records remain internal authorization records; their public projection
is removal in the next atomic `state/sessions` revision plus the applicable
event below. Restart invalidation removes every open invitation, restores every
TPM-sealed unexpired access session, and preserves its pending-revocation
marker, if any, while leaving persistent controller enrollments intact.

Events are:

- `controllers.enrolled`;
- `controllers.revoked`;
- `sessions.invitation-closed`;
- `sessions.claimed`;
- `sessions.revoked`;
- `sessions.expired`; and
- `security.authorization-denied`.

Events contain identifiers, client kind, non-secret labels, scopes, outcome,
and timestamps. They never contain credentials or the QR payload.

## 12. LAN binding

An enrollment claim MAY also return an X.509 LAN client certificate whose
public key is the claimed controller signing key. LAN HTTPS and the authenticated local
push channel use mutual TLS and map the certificate to the same controller
enrollment and scope ceiling.

An enrolled certificate has SAN
`URI:urn:ff:device:<deviceId>:controller:<controllerId>`. A guest certificate
has SAN `URI:urn:ff:device:<deviceId>:session:<sessionId>`. FF1 validates the
exact SAN, issuing controller CA, public-key thumbprint, local enrollment or
session record, scope ceiling, and certificate validity before accepting a
runtime request.

Each controller certificate is X.509 v3, is signed with ECDSA and SHA-256 by
the device-local controller CA, and contains only the applicable URI SAN. It
has critical Basic Constraints with `CA=false`, critical Key Usage containing
only `digitalSignature`, and Extended Key Usage `clientAuth`. DNS, IP, email,
wildcard, and additional URI SANs are forbidden. The certificate public key is
the controller signing key from the accepted CSR.

Opening an authenticated LAN connection creates a connection-local LAN
authorization lease bounded by the controller certificate, enrollment or
guest-session status, and TLS connection. This lease is not an access-session
record: it has no `sessionId`, is not persisted or retained in
`state/sessions`, and does not count toward the sixteen-access-session limit.
For an enrolled controller, FF1 issues a lease deadline no later than 900
seconds after TLS authentication. FF1 rejects later HTTP requests on that TLS
connection and closes a local-push WebSocket at the issued deadline; a
still-active enrollment may establish a new TLS connection and lease silently.
For a guest, the lease ends no later than the earlier of the existing guest
session's expiry and the client certificate's `notAfter`; the underlying guest
session remains the persisted access-session record and counts normally. A
guest `agent|integration` MAY receive a temporary LAN
certificate whose `notAfter` equals its session expiry. A `web` guest is
MQTT-only because v2 does not depend on browser client-certificate installation.

An enrolled controller can remain usable over LAN while the internet and broker
are unavailable after FF1 has established trusted time for the current boot.
After a power-loss reboot, pre-NTP LAN availability is guaranteed only when the
required parent-contract capability
`capabilities.state.transports.lanOfflineAfterPowerLoss` is `true`. When it is
`false`, FF1 rejects runtime LAN mTLS until NTP succeeds. FF1 evaluates
enrollment revocation locally. A controller certificate has a maximum validity
of 366 days and is renewed before expiry by an active enrolled controller.
Apart from the QR-authorized invitation routes, there is no unauthenticated LAN
fallback.

A native controller stores both its MQTT and LAN connection profiles. It uses
LAN only on an explicitly trusted SSID or equivalent trusted-network rule. If
that rule does not match, network identity is unavailable, or the LAN endpoint
cannot be reached promptly, the controller uses MQTT. mDNS is an endpoint hint,
not an enrollment prerequisite or an authorization signal.

## 13. Expiry, restart, revocation, and reset

### 13.1 Expiry authority

FF1 chooses every invitation and access-session expiry and signs it into the
credential. The broker MUST NOT extend that expiry. The control plane MAY cache
the FF1 issuer public key and revocation notifications but MUST NOT create or
extend an FF1 access session.

FF1 checks session status and expiry again for every command after broker
delivery. A broker-accepted PUBLISH is not proof that FF1 still authorizes the
session.

### 13.2 Trusted time

FF1 MUST have trusted, non-decreasing time before it issues or accepts a
time-bounded credential. It persists a last-known-good UTC floor and advances
time from a monotonic source during a boot. The required parent-contract
capability `capabilities.state.transports.lanOfflineAfterPowerLoss` is the only
declaration of pre-NTP runtime LAN behavior after power loss:

- `true` requires a trusted advancing RTC that persists across power loss,
  starts at or above the persisted UTC floor, and enforces certificate and LAN
  authorization-lease expiry. FF1 MUST accept authenticated LAN mTLS immediately
  after an offline reboot.
- `false` requires runtime LAN mTLS to fail closed after power loss until NTP
  establishes trusted time in that boot. Brokerless LAN remains available
  after that synchronization and continues from monotonic time if the broker
  or internet later disappears.

FF1 MUST NOT advertise `true` unless executable evidence on the shipping FF1
hardware and full image proves RTC persistence, advancement, non-rollback, and
expiry enforcement across a power cycle. Before NTP on a qualifying `true`
device, only existing valid enrolled-controller or session-bounded runtime LAN
authorization is available; remote MQTT and new credential, session, or
LAN-certificate issuance remain blocked and clock status is `degraded`. If
trusted time is unavailable, FF1 also fails closed for runtime LAN and remote
session issuance, reports `clock_unsynchronized`, and leaves SoftAP recovery
available.

### 13.3 Restart

All invitation and access-session records are stored atomically and sealed to
the TPM-backed device identity. A `feral-controld` restart invalidates every
open invitation. It restores every unexpired access-session record, including
guest and enrolled-controller sessions, with the same ID, status, scopes, and
absolute expiry, and restores every durable pending-revocation marker. Except
for a target already blocked by such a marker, those sessions remain valid
until their signed `exp` or an explicit revocation completes under section
13.4. A pending target remains locally rejected and its barrier retry takes
priority after restart. A restart MUST NOT create an implicit broker revocation
or require a controller to obtain a replacement access session.

The exceptions are lifecycles `pending_broker_cleanup` and
`pending_identity_rotation`. In either lifecycle restart restores no controller
enrollment or access-session authorization and accepts no controller command or
new owner claim. Broker-cleanup recovery executes only the device-wide barrier;
identity-rotation recovery executes only the post-barrier identity transaction
in section 13.5.

### 13.4 Revocation

FF1 and the broker authorization adapter implement a revocation barrier. A
barrier request is authenticated as the TPM-backed FF1 device identity and is
idempotently identified by a UUIDv7 `operationId`. Its target is exactly one of
an access-session ID, a controller ID together with the exact derived session
IDs being revoked, or a device-reset target containing the controller-issuer
generation identified by the issuer JWS `kid`, current publisher generation,
and every controller/session authorization ID. The adapter MUST, atomically and
durably before acknowledging the request:

1. install a deny tombstone for every target identifier;
2. apply that deny state before every CONNECT, PUBLISH, SUBSCRIBE, and outbound
   delivery authorization decision;
3. reject a later CONNECT or authorization attempt for a target credential;
4. remove every target subscription and all queued outbound state or event
   delivery for the target; and
5. disconnect every active target client with MQTT 5 DISCONNECT Reason Code
   `0x87` (Not authorized).

The adapter ACK is the barrier completion boundary. It is an acknowledgement
from the broker's authenticated authorization-management interface, not an
MQTT PUBACK or another public MQTT packet. Repeating the same `operationId` and
target MUST return the same completed ACK. A tombstone MUST remain effective
for at least as long as any corresponding credential, broker Session, or
queued delivery can exist; an issuer-generation tombstone remains effective
until that issuer registration and all associated broker state have been
deleted.

When revocation begins, FF1 first durably marks the target as pending revocation
and immediately rejects its commands and new session issuance locally. It MUST
NOT yet report a successful revocation, remove the target from retained public
state, emit `sessions.revoked` or `controllers.revoked`, or return a successful
revocation result. Only after the adapter ACK may FF1 commit `revoked`, update
`state/sessions` and/or `state/controllers`, publish the corresponding event,
and return success.

`controllers.revoke` MUST reject the authenticated caller's own controller ID,
and `sessions.revoke` MUST reject the access-session ID carrying the command.
Both failures are `interaction_not_allowed` and occur before any pending marker
or barrier operation is created. This keeps the synchronous response path
authorized until an other-controller or other-session revocation completes.

If FF1 cannot obtain the ACK, the command returns `dependency_unavailable` with
`retryable: true` and exact details
`{"dependency":"broker_authorization_barrier","pending":true}`. The durable
pending revocation remains locally enforced and FF1 retries it before it may
authorize that target again. A later request for the same target returns the
same retryable failure while the barrier is pending and the declared
`already_revoked` result only after barrier completion.

Revoking an enrollment also revokes its enrollment credential, LAN certificate,
and every active access session created from it. It does not revoke guest
sessions the controller created unless the revocation request explicitly sets
`revokeCreatedGuestSessions: true`; owner policy SHOULD set it for a lost or
compromised controller.

### 13.5 Factory reset and owner transfer

Each device broker registration has a management-plane
`publisherGeneration`: UUIDv7 bound to the TPM-authenticated device identity,
device MQTT Client Identifier, and certificate registration. The broker permits
device publishes and reconnect only while that generation is active. This
value is broker authorization metadata, not an MQTT property or Application
Message member.

Factory reset uses a device-wide instance of the section 13.4 barrier. Its
target contains the prior controller-issuer generation and every controller and
access-session authorization plus the current `publisherGeneration`. In the
normal online sequence, FF1 first obtains
PUBACK for its final `system.factory-reset-starting` Application Message. It
then quiesces heartbeat and state publication, freezes and discards the old
event outbox, and sends a normal MQTT DISCONNECT on the public device Network
Connection so the Will Message is not published. Only then does it use the
separately TPM-authenticated broker-management connection for the barrier. No
local identity or user-secret erasure begins before that ACK.

Before ACK, the adapter MUST install the authorization deny tombstones, remove
controller subscriptions and queued outbound deliveries, disconnect active
controllers with `0x87`, fence the old device-publisher generation and any
reconnect using it, cancel its pending Will Message and queued device
publishes, and then purge every retained Application Message in the old device
subtree. The adapter ACKs only when the fence prevents any old publisher action
from recreating purged state.

Barrier ACK does not complete reset. FF1 atomically persists that ACK and
transitions to `pending_identity_rotation`, durably assigning a UUIDv7
`identityOperationId`. In this lifecycle the public MQTT device publisher,
controller authentication, and owner invitation or claim remain blocked. Over
the separately authenticated management path, FF1 proves possession of the
stable TPM hardware device-identity key and requests a replacement runtime
X.509 device certificate under `identityOperationId`. The replacement MUST have
a fresh certificate serial, certificate fingerprint, and certificate-
registration ID and is securely installed locally but MUST NOT yet be used by
the public publisher.

The registry then performs one atomic switch: revoke and deregister the old
runtime certificate registration, activate the replacement certificate
registration, and bind it to a fresh `publisherGeneration`. Its ACK is the
identity-commit boundary. Repeating `identityOperationId` MUST return the same
replacement certificate identity, publisher generation, and committed ACK;
neither an ambiguous response nor a crash may create two active registrations.

Only after the identity-commit ACK may FF1 delete any local old runtime
certificate, finish erasing the prior controller issuer and controller CA,
enrollments, invitations, access sessions, cached credentials and user data,
and durably commit cleanup completion. Only after that local commit may it
clear the cleanup tombstone, report reset `completed`, connect the public MQTT
device publisher with the replacement certificate and generation, or display
or accept a new `owner_enrollment` invitation.

An offline screen-initiated reset is the sole pre-ACK erasure exception. It
deletes the local prior runtime device certificate, controller issuer and
controller CA, enrollments, invitations, access sessions, cached credentials,
user data, network credentials, and old event outbox. It preserves only the
stable TPM-backed device identity and a durable tombstone signed by that
identity containing the old device ID, runtime certificate serial and
registration ID, controller-issuer `kid`, target authorization identifiers,
publisher generation, reset `confirmationId`, and barrier `operationId`. The
lifecycle is
`pending_broker_cleanup`: reset is not complete, no controller/session
authorization is restored, and new owner authorization is blocked. If network
credentials were erased, network reconfiguration uses the recovery SoftAP with
a fresh setup authorization.

On crash or offline recovery, the barrier is the first broker-management
action. It fences the prior publisher generation before purging its retained
Application Messages; FF1 MUST NOT reconnect the public device publisher or
flush an old event outbox before the ACK. Recovery from
`pending_identity_rotation` instead resumes the same idempotent
`identityOperationId` and completes the registry switch and local cleanup; it
does not restore controller authorization or reconnect the public publisher.
If a crash occurs after either remote ACK, the durable tombstone identifies the
completed boundary so the corresponding operation is not reversed or
duplicated. No controller authorization or queued or retained controller data
survives a completed reset.

Owner transfer is a physical ceremony. It revokes the former owner enrollment
and all derived sessions before FF1 displays a new `owner_enrollment`
invitation. Network commands cannot silently replace the final owner.

## 14. Required controller flows

### 14.1 First mobile owner

1. An online, unclaimed FF1 creates an `owner_enrollment` invitation and shows
   its QR.
2. The mobile app creates its controller signing and encryption keys and scans
   the QR.
3. The app claims through MQTT by default, or through the pinned HTTPS adapter
   during explicit LAN setup when the section 13.2 trusted-time prerequisite is
   satisfied.
4. FF1 consumes the invitation once, creates the owner enrollment, and returns
   the enrollment credential plus the first access-session credential encrypted
   to the mobile key.
5. The app uses the bounded access session for control and renews sessions from
   its enrollment without repeating the QR ceremony. The 15-minute access
   lifetime is not shown as a login or approval prompt.

### 14.2 Additional mobile app, CLI, or installed integration

1. An enrolled owner invokes `controllers.create-invitation` with the intended
   scope ceiling. FF1 shows the QR.
2. The new mobile app, CLI, or integration creates its own controller signing
   and encryption keys and consumes the QR once through the same claim protocol.
3. FF1 creates a delegate enrollment and issues the first access session.
4. The controller persists only its private keys, enrollment credential, and
   optional LAN certificate. It renews bounded access sessions and enrollment
   credentials silently as needed.
5. Existing controller enrollments and sessions remain valid. Every enrolled
   mobile app, CLI, or integration may connect concurrently, subject to the
   advertised access-session limit.

### 14.3 Web client or temporary agent

1. An enrolled controller with `sessions:manage` invokes
   `sessions.create-invitation` with the guest scopes and lifetime. This command
   is the authorization decision.
2. FF1 shows the guest invitation QR. A headless agent may receive the exact QR
   URI through an out-of-band copy.
3. The guest creates ephemeral signing and encryption keys and consumes the
   invitation once. There is no second approval request.
4. FF1 creates no controller enrollment. It returns one non-renewable guest
   access credential, and an optional session-bounded LAN certificate for an
   agent or integration.
5. FF1 rejects the guest at expiry. On revocation it rejects the guest locally
   and reports success only after the broker authorization barrier disconnects
   it and removes its queued delivery state. Creating another guest session
   requires a new invitation from an enrolled controller.

## 15. Required pre-normative security checks

Before target version `2.0.0` becomes normative, its machine-readable
conformance suite MUST prove:

1. a QR invitation can be consumed exactly once;
2. different controller keys cannot use a captured claim or credential
   envelope;
3. an invitation cannot grant a role, scope, origin, device, or lifetime beyond
   the values signed by FF1;
4. a guest cannot create another guest session or enroll a controller;
5. a pending-revocation session cannot execute a command, and after barrier ACK
   it receives no newly authorized, queued, or retained state/event delivery
   and cannot reconnect;
6. an enrollment credential alone cannot read state or execute a command;
7. access-session expiry, reconnect, app restart, and FF1 restart never require
   a new QR for an active controller enrollment;
8. multiple independently enrolled mobile apps and CLIs can control the same
   FF1 concurrently without sharing credentials or invalidating one another;
9. enrollment-credential rotation preserves the enrollment and requires no
   user interaction;
10. FF1 restart invalidates open invitations, restores TPM-sealed unexpired
    access sessions, and preserves active enrollments without extending any
    absolute expiry, except that either pending-reset lifecycle restores no
    controller or session authorization;
11. offline reset cannot complete or enroll a new owner before both the broker
    barrier and identity-commit ACK; barrier ACK transitions to
    `pending_identity_rotation`, and completion requires old runtime-certificate
    deregistration, replacement-certificate activation, a fresh publisher
    generation, and durable local cleanup;
12. one controller cannot subscribe to another controller's response Topic
    Name or another device subtree;
13. logs, state, events, and diagnostics contain no invitation credential,
    access credential, enrollment credential, JWE plaintext, private key, or QR
    payload;
14. an enrolled LAN connection rejects HTTP requests and closes local push at
    its issued LAN authorization-lease deadline, which is no later than 900
    seconds after authentication, even when the X.509 certificate remains
    valid, and an active enrollment reconnects without a QR;
15. every retained invitation/session projection variant validates as a closed
    object, terminal transitions remove it atomically, and restart publishes no
    previously open invitation while restoring every unexpired active session
    and pending-revocation marker unless reset is in either pending-reset
    lifecycle;
16. a captured access credential cannot complete MQTT authentication without
    the private key corresponding to its RFC 7800 `cnf.jwk`;
17. wrong-key, audience, device, session, Client Identifier, expired-challenge,
    challenge-replay, and proof-replay cases install no ACL and produce the
    section 7.4 MQTT Reason Code;
18. the JSON Schema, AsyncAPI, OpenAPI, positive fixtures, negative security
    fixtures, and MQTT/LAN parity tests are published and executable, so no
    prose-only implementation is reported as conformant;
19. self-controller and current-access-session revocation fail with
    `interaction_not_allowed` before creating a barrier operation; and
20. crash recovery in each pending-reset lifecycle resumes the same durable
    operation ID, never activates two runtime certificate registrations, and
    cannot report `completed`, reconnect the publisher, or enroll an owner
    before identity commit and durable local cleanup;
21. with `lanOfflineAfterPowerLoss: true`, an NTP-synchronized FF1 is power
    cycled with internet and broker blocked, its trusted RTC advances without
    rollback, and an enrolled controller completes LAN mTLS, a command, initial
    local-push snapshots, and lease/certificate-expiry rejection before NTP;
22. with `lanOfflineAfterPowerLoss: false`, brokerless LAN works after NTP in
    the current boot, then a power cycle with NTP blocked causes runtime mTLS to
    fail before HTTP or WebSocket authorization while SoftAP recovery remains
    available; LAN becomes available again only after NTP succeeds; and
23. a device image lacking the executable RTC proof in check 21 cannot
    advertise `lanOfflineAfterPowerLoss: true`, while advertising `false` does
    not by itself fail v2 LAN conformance.
