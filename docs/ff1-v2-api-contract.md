# FF1 communication API v2

- Status: design draft; not conformance-ready
- Target contract version: `2.0.0`
- Primary transport: MQTT 5.0 over TLS 1.3
- LAN adapter: HTTPS plus authenticated WebSocket local push over TLS 1.3 with the same JSON representations
- Authentication profile: [FF1 v2 controller authentication and access sessions](ff1-v2-controller-authentication.md)
- Companion rollout plan: [FF1 API v2 migration and implementation plan](ff1-v2-migration.md)

This prose is a proposed protocol profile, not a normative conformance
artifact. The capitalized requirement keywords state the intended v2 design.
Before `2.0.0` becomes normative, the repository MUST publish the complete JSON
Schema 2020-12 bundle, AsyncAPI document, OpenAPI document, positive and
negative fixtures, and automated MQTT/LAN parity validation described in the
migration plan. No implementation may claim FF1 API v2 conformance from these
prose documents alone.

## 1. Purpose and scope

This document defines the proposed public FF1 device communication boundary
for the Feral File mobile app, CLI clients, and explicitly authorized
institutional controllers. MQTT is the authoritative transport model. LAN HTTPS
and WebSocket bindings carry the same JSON contract.

The contract covers:

- discovery, controller enrollment, one-time invitations, access sessions, and
  revocation;
- MQTT connection, authentication, authorization, presence, capabilities,
  commands, responses, state, and events;
- the mechanically equivalent LAN HTTPS resource surface and authenticated
  local resource/event push;
- DP-1 Playlist display and playback control;
- display, panel, audio, sleep, settings, update, power, support, and SSH;
- mobile remote control, including keyboard and relative touchpad input;
- enrolled mobile, CLI, and integration controllers plus web guest sessions
  with browser-only WebSocket Origin checks and temporary-agent guest sessions;
- unprovisioned SoftAP setup and recovery as a deliberately separate bootstrap
  profile.

Cloud account, collection, commerce, editorial, and internal service APIs are
outside this contract. FF1 exposes one runtime external-control boundary.

The keywords MUST, MUST NOT, REQUIRED, SHOULD, SHOULD NOT, and MAY are used as
described by RFC 2119 and RFC 8174.

## 2. Inputs and standards

This specification uses:

- the [DP-1 core v1.1.0 specification](https://github.com/display-protocol/dp1/blob/main/core/v1.1.0/spec.md)
  and its normative JSON Schema;
- [MQTT 5.0](https://docs.oasis-open.org/mqtt/mqtt/v5.0/mqtt-v5.0.html),
  including Enhanced Authentication, the AUTH Control Packet, Authentication
  Method, Authentication Data, Response Topic, Correlation Data, Message
  Expiry Interval, Session Expiry Interval, Will Delay Interval, Content Type,
  Payload Format Indicator, Maximum Packet Size, and Reason Codes;
- [TLS 1.3 (RFC 8446)](https://www.rfc-editor.org/rfc/rfc8446.html),
  [X.509 (RFC 5280)](https://www.rfc-editor.org/rfc/rfc5280.html), and the
  [TPM 2.0 Library specification](https://trustedcomputinggroup.org/resource/tpm-library-specification/);
- [JWT (RFC 7519)](https://www.rfc-editor.org/rfc/rfc7519.html);
- [JWS (RFC 7515)](https://www.rfc-editor.org/rfc/rfc7515.html),
  [JWE (RFC 7516)](https://www.rfc-editor.org/rfc/rfc7516.html),
  [JWA (RFC 7518)](https://www.rfc-editor.org/rfc/rfc7518.html),
  [JWT confirmation (RFC 7800)](https://www.rfc-editor.org/rfc/rfc7800.html),
  [JWK thumbprints (RFC 7638)](https://www.rfc-editor.org/rfc/rfc7638.html), and
  [Web Origin (RFC 6454)](https://www.rfc-editor.org/rfc/rfc6454.html);
- [JSON Schema 2020-12](https://json-schema.org/draft/2020-12),
  [JSON Canonicalization Scheme (RFC 8785)](https://www.rfc-editor.org/rfc/rfc8785.html),
  [RFC 3339 timestamps](https://www.rfc-editor.org/rfc/rfc3339.html), and
  [Problem Details (RFC 9457)](https://www.rfc-editor.org/rfc/rfc9457.html);
- [OpenAPI 3.1.1](https://spec.openapis.org/oas/v3.1.1.html),
  [AsyncAPI 3.0](https://www.asyncapi.com/docs/reference/specification/v3.0.0),
  and the AsyncAPI MQTT binding;
- [mDNS (RFC 6762)](https://www.rfc-editor.org/rfc/rfc6762.html) and
  [DNS-SD (RFC 6763)](https://www.rfc-editor.org/rfc/rfc6763.html);
- [The WebSocket Protocol (RFC 6455)](https://www.rfc-editor.org/rfc/rfc6455.html); and
- [W3C UI Events `KeyboardEvent.code`](https://www.w3.org/TR/uievents-code/).

## 3. Contract conventions

### 3.1 Versioning and media type

- The MQTT namespace and HTTPS path begin with `ff/v2` and `/ff/v2`.
- Every JSON document in the common MQTT/LAN control contract contains
  `apiVersion: "ff/v2"`. Embedded DP-1 uses its own `dpVersion`; top-level RFC
  9457 errors use their standard members; and the path-versioned SoftAP
  bootstrap API intentionally omits `apiVersion` exactly as defined in section
  14. The QR invitation object uses its explicit integer `version` as defined by
  the authentication profile.
- The JSON media type is `application/json`; MQTT publishes set Payload Format
  Indicator to `1` and Content Type to `application/json`.
- The eventual machine-readable JSON Schema 2020-12 bundle is the normative
  authority for FF-defined JSON. It MUST encode every FF-defined object with
  `additionalProperties: false` unless the object is an explicitly namespaced
  `extensions` object. Until that bundle is published, the closed shapes in
  this draft are strict design requirements but are not sufficient to test or
  claim conformance. Embedded DP-1 documents remain governed by the official
  DP-1 schemas and are not made stricter recursively by the FF envelope.
- Compatible additions go under `extensions[reverseDnsName]`. A new base field,
  changed meaning, enum removal, or changed constraint requires a new contract
  version and capability negotiation. This makes the base contract strict
  without preventing extensions.
- Deprecated fields remain accepted for one announced compatibility window but
  MUST NOT be emitted by new producers. The retained capability document lists
  `deprecatedAfter` and `removedIn` for any such field or operation.

### 3.2 Vocabulary and JSON primitives

This contract uses DP-1 nouns directly. It does not introduce alternate wire
objects for art or playlists.

| Term | Contract meaning |
|---|---|
| Playlist | The DP-1 top-level Playlist document. |
| PlaylistItem | One entry in a DP-1 Playlist. IDs remain optional as DP-1 specifies. |
| Player | The FF1 runtime that renders a DP-1 Playlist. |
| Work or Channel | A DP-1 extension object only when an advertised extension defines it. Neither is an FF command-envelope type. |
| display | The action that makes a Playlist current on the Player, immediately or at `displayAt`. The operation is `playlist.display`. |
| playback | Runtime state and controls for the active Playlist and PlaylistItem. It does not name Playlist delivery. |
| display preferences | The DP-1 `display` object and permitted device-level overrides. Physical panel state remains a separate FF resource. |

Human-readable text writes the protocol name as **DP-1**. Lowercase `dp1`
appears only where the wire contract defines it, including capability members
and namespaced error codes.

- FF-generated request, event, operation, session, controller, and confirmation
  IDs are lowercase UUIDv7 strings. DP-1 Playlist and PlaylistItem IDs retain
  DP-1's optional UUID format and are not rewritten to v7.
- `deviceId` is the immutable, already-deployed FF1 hostname matching
  `^FF1-[A-Za-z0-9][A-Za-z0-9-]{0,58}$`. It is the account-bound public
  identifier, never a MAC address or transport topic identifier. Manufacturing
  identity MAY have a separate TPM-attested UUID, but it does not replace
  `deviceId` on the public API.
- FF timestamps are UTC RFC 3339 with exactly three fractional digits, for example
  `2026-07-21T08:15:30.123Z`.
- Durations and relative offsets are integer milliseconds. TTLs are integer
  seconds.
- Byte counts and monotonically increasing counters that may exceed JavaScript's
  safe integer range are unsigned decimal strings.
- JSON numbers MUST be finite and satisfy their operation-specific bounds.
- Optional fields are omitted, never `null`.
- Canonical request equality and every DP-1 Playlist `sha256` use RFC 8785 JCS
  UTF-8 bytes followed by SHA-256. Document transfer chunks contain those same
  JCS bytes, not the source server's incidental whitespace or member ordering.
  A SHA-256 value is lowercase 64-character hexadecimal.

### 3.3 State revision

Every retained state representation has:

```json
{
  "apiVersion": "ff/v2",
  "deviceId": "FF1-01234567",
  "epoch": "019bf2b4-2589-7d73-8a69-4525a5136629",
  "revision": "42",
  "eventWatermark": "83",
  "generatedAt": "2026-07-21T08:15:30.123Z",
  "state": {}
}
```

`epoch` changes on daemon restart or factory reset. `revision` starts at `1`
and increases by one for every emitted change to that resource.
`eventWatermark` is the highest device-wide event sequence committed before or
with this snapshot. Consumers compare revisions only within the same epoch. A
new epoch replaces the complete cached snapshot. A producer MUST publish state
from its source of truth even when no controller is connected.

## 4. MQTT transport profile

### 4.1 Broker connection

- Protocol: MQTT 5.0 only, over TLS 1.3.
- Remote URI: `wss://<control-host>:443/mqtt`. Native MQTT TLS on port 8883 MAY
  also be exposed. Every v2 remote endpoint MUST provide WSS on port
  443; clients MUST NOT depend on port 8883.
- Device Client Identifier: `ff-device-<deviceId>`.
- Access-session Client Identifier: `ff-session-<sessionId>`. Each controller
  has its own key and each access session has its own Client Identifier.
- Invitation and enrollment-only Client Identifiers are defined by the
  controller-authentication profile linked above.
- Clean Start is `1`; Session Expiry Interval is `0`. Commands are never queued
  for an offline device. Will Delay Interval is `0`; with Session Expiry
  Interval zero, a larger Will Delay cannot postpone publication beyond Session
  end under MQTT 5 rules.
- Keep Alive is 30 seconds; Receive Maximum is 32; Maximum Packet Size is
  262144 bytes.
- Shared subscriptions, wildcard publishes, retained commands, MQTT 3.x, and
  broker-specific RPC features are prohibited.

#### 4.1.1 CONNECT and CONNACK profile

Every client sends Protocol Name `MQTT`, Protocol Version `5`, a nonempty Client
Identifier above, Clean Start `1`, and these CONNECT properties:

| CONNECT property | Required value |
|---|---|
| Session Expiry Interval | `0` |
| Receive Maximum | `32` |
| Maximum Packet Size | `262144` |
| Topic Alias Maximum | `0` |
| Request Problem Information | `1` |
| Request Response Information | `0` |

The device sets Will Flag `1`, Will QoS `1`, and Will Retain `1`. Its Will
Properties are Will Delay Interval `0`, Payload Format Indicator `1`, Message
Expiry Interval `86400`, and Content Type `application/json`; its Will Topic and
Will Payload are the presence Topic Name and disconnected JSON from section
4.3. A controller sets Will Flag `0`.

Authentication fields and Control Packets depend on the connection profile:

| Client profile | CONNECT authentication | AUTH exchange |
|---|---|---|
| FF1 device | TLS client-certificate authentication; omit User Name, Password, Authentication Method, and Authentication Data | prohibited |
| invitation claimant | restricted User Name and Password from the controller-authentication profile; omit Authentication Method and Authentication Data | prohibited |
| enrollment-only controller | restricted User Name and Password from the controller-authentication profile; omit Authentication Method and Authentication Data | prohibited |
| access-session controller | User Name is `sessionId`; omit Password; Authentication Method is `FF1-JWT-ES256-PoP`; Authentication Data carries the access credential | required challenge/response before CONNACK |

The access-session exchange uses the MQTT 5 Authentication Method and
Authentication Data properties and AUTH Control Packets exactly as specified
in the controller-authentication profile. `FF1-JWT-ES256-PoP` and the JSON
semantics of its Authentication Data are FF customizations built on standard
MQTT 5 Enhanced Authentication. They do not change any MQTT Control Packet,
property, or Reason Code.

The connection is usable only when CONNACK has Reason Code `0x00` (Success) and
Session Present `0`. For an access-session connection, that successful CONNACK
MUST also contain Authentication Method `FF1-JWT-ES256-PoP`, matching CONNECT.
An absent CONNACK property has the MQTT 5 default. The
effective broker profile MUST provide Maximum QoS at least `1`, Retain Available
`1`, Wildcard Subscription Available `1`, and a Maximum Packet Size at least
`262144`; a client treats a lower advertised value as an incompatible endpoint
and closes with DISCONNECT Reason Code `0x83` (Implementation specific error).
Publishers honor the server's Receive Maximum. If Server Keep Alive is present,
it replaces the CONNECT Keep Alive. Clients do not send Topic Alias, even if the
server advertises a nonzero Topic Alias Maximum.

Reason String and User Property received in CONNACK, PUBACK, SUBACK, or
DISCONNECT are diagnostic only. They MUST NOT change behavior defined by a
Reason Code and MUST be redacted before logging. A client considers Server
Reference only when CONNACK or DISCONNECT carries Reason Code `0x9C` (Use
another server) or `0x9D` (Server moved). It follows the reference only when it
is an allowlisted `wss://` URI on port 443 with the expected trust domain;
otherwise the connection fails visibly without redirection.

#### 4.1.2 SUBSCRIBE and session profile

Because Session Expiry Interval is zero, every client sends new SUBSCRIBE
packets after each successful CONNACK and does not rely on stored subscriptions
or queued QoS 1 Application Messages. The required Topic Filters and
Subscription Options are:

| Client | Topic Filter | QoS | No Local | Retain As Published | Retain Handling |
|---|---|---:|---:|---:|---:|
| device | `ff/v2/devices/{deviceId}/sessions/+/commands/+/+` | 1 | 1 | 1 | 2 |
| device | its own invitation claim/acknowledgement filters from the controller-authentication profile | 1 | 1 | 1 | 2 |
| device | its own enrolled-session request filters from the controller-authentication profile | 1 | 1 | 1 | 2 |
| controller | exact `presence`, exact `capabilities`, and one exact `state/{resource}` per authorized resource | 1 | 1 | 1 | 0 |
| controller | its exact `responses/{controllerId}` and one exact `events/{class}/{name}` Topic Name per authorized event type | 1 | 1 | 1 | 2 |

Retain Handling `0` is required for current retained resources on a new
subscription; `2` prevents retained delivery on command, response, and event
filters. Retain As Published `1` preserves the RETAIN flag so a receiver can
reject a retained command. Each SUBACK entry MUST be Granted QoS 1 (`0x01`);
Granted QoS 0 or any failure Reason Code makes that protocol function
unavailable and is surfaced to the client. Topic Filters are authorized before
SUBACK; a controller never subscribes to another controller's response Topic
Name or another device subtree. `state/+` is allowed only to a principal
authorized for every state resource, and `events/+/+` only to a principal
authorized for every event type; MQTT SUBACK cannot partially authorize the
matches of one Topic Filter.

The managed broker choice remains conditional on proving the required WSS/443
endpoint; no client may depend on a vendor's nonstandard public port.

Invitation and enrolled-session Topic Names, credentials, exact subscriptions,
and lifecycle state are specified in the controller-authentication profile.
Normal access sessions use the common controller Topic Names below.

### 4.2 Topics and delivery rules

`{deviceId}`, `{sessionId}`, and `{controllerId}` are single validated topic
segments. They
never contain `/`, `+`, `#`, percent escapes, or Unicode.

| Topic | Publisher -> subscriber | QoS | Retain | Expiry |
|---|---|---:|---:|---:|
| `ff/v2/devices/{deviceId}/presence` | device -> controllers | 1 | yes | 24 hours |
| `ff/v2/devices/{deviceId}/capabilities` | device -> controllers | 1 | yes | 24 hours |
| `ff/v2/devices/{deviceId}/state/{resource}` | device -> controllers | 1 | yes | 24 hours |
| `ff/v2/devices/{deviceId}/sessions/{sessionId}/commands/{class}/{name}` | controller -> device | 1 | no | request TTL |
| `ff/v2/devices/{deviceId}/responses/{controllerId}` | device -> controller | 1 | no | 60 seconds |
| `ff/v2/devices/{deviceId}/events/{class}/{name}` | device -> controllers | 1 | no | event-specific, at most 1 hour |

Every row is an MQTT Application Message carried in a PUBLISH Control Packet at
QoS 1. The sender assigns a nonzero Packet Identifier and follows MQTT's DUP and
PUBACK retransmission rules; Packet Identifier and DUP are transport state and
never substitute for `requestId` or `eventId`. The RETAIN flag is `1` only for
the first three rows. Every JSON PUBLISH has Payload Format Indicator `1` and
Content Type `application/json`. It has Message Expiry Interval equal to the
table value, rounded up to seconds. Senders do not use Topic Alias, Subscription
Identifier, or User Property in PUBLISH. A broker-added Subscription Identifier
is ignored because this profile does not request one.

Allowed state resources are `device`, `network`, `playback`, `display`,
`health`, `support`, `controllers`, and `sessions`. Events do not replace
current state. A reconnecting client subscribes before reading retained
resources, buffers interleaved events, then compares each event to the relevant
resource's `eventWatermark`: playlist or playback -> playback, display ->
display, system -> health, support -> support, sessions -> sessions, and
controllers or security -> controllers. It discards an
event at or below that watermark and
delivers a newer one in sequence order. Events are notifications only; clients
never mutate cached state from event data.

Capabilities and unchanged state are republished with the same payload and
revision at least every six hours to renew their MQTT Message Expiry. This
prevents an offline factory reset or abandoned identity from leaving sensitive
retained data indefinitely. Expired state is unknown, never assumed current.

Command PUBLISH packets MUST additionally carry these MQTT 5 properties:

- Response Topic = the authenticated controller's response topic;
- Correlation Data = the raw 16 UUID bytes of `requestId`;
- Message Expiry Interval = `ceil(expiresAt - senderCurrentTime)` at PUBLISH;
- Payload Format Indicator = `1`; and
- Content Type = `application/json`.

The Topic Name `sessionId` MUST equal the authenticated access credential's
`ff_session_id`. The MQTT Server grants that credential only its exact session
command prefix. FF1 extracts the session ID, requires the Response Topic's
controller ID to match the local session record, and rechecks session status,
expiry, and operation scope before dispatch.

Response Topic is a UTF-8 Topic Name with no wildcard; Correlation Data is MQTT
Binary Data. The device rejects a missing or unauthorized Response Topic or a
Correlation Data value that does not match the body. On receipt it defines the effective
deadline as the earlier of JSON `expiresAt` and
`receiverCurrentTime + receivedRemainingMessageExpiry`; this accounts for the
broker decrementing Message Expiry in transit without comparing it to the
original TTL. It also rejects any command with the RETAIN flag. Responses copy
Correlation Data and do not set Response Topic. If an authorized Response Topic
and a valid 16-byte UUID Correlation Data are present but JSON validation fails,
the device uses that UUID as `requestId` in `schema_invalid`; otherwise it
discards the message and emits only a sanitized security audit event.

Delivery diagnosis uses standard evidence. PUBACK Reason Code `0x10` (No
matching subscribers) proves no current subscription matched; `0x00` (Success)
proves broker acceptance but does not prove FF1 execution or require that a
broker report matching subscribers. `0x87` (Not authorized), `0x90` (Topic Name
invalid), `0x97` (Quota exceeded), and other failure Reason Codes are MQTT
transport failures, not FF response envelopes. A correlated response proves FF1
received and completed or durably accepted the command. If no response arrives
before expiry, retained presence distinguishes disconnected/stale from a
connected device timeout, and the client reports those as different failures.
An application-level defense-in-depth authorization denial is a correlated
`forbidden` response; it does not invent an MQTT Reason Code. This is the
support-facing “MoMA test” and does not require device-log access.

### 4.3 Presence

The device CONNECT contains this retained QoS 1 Will Message on its presence
Topic Name:

```json
{
  "apiVersion": "ff/v2",
  "deviceId": "FF1-01234567",
  "status": "disconnected",
  "reason": "connection_lost",
  "sessionId": "019bf2b6-bd0e-71c6-8ce4-83cc4bb40510",
  "publishedAt": "2026-07-21T08:15:30.123Z"
}
```

After successful CONNACK the device publishes retained `status: "connected"`
with the same fields plus `leaseSeconds: 90`, and renews it every 30 seconds.
For a normal shutdown it publishes retained `status: "disconnected"`,
`reason: "clean_disconnect"`, waits for PUBACK, then sends DISCONNECT Reason
Code `0x00` (Normal disconnection), which suppresses its Will Message. Loss of
the Network Connection or another MQTT-defined Will condition causes the broker
to publish the Will Message.

Client interpretation is strict:

- `connected`: latest valid presence says connected and its lease has not
  elapsed;
- `disconnected`: latest valid presence says disconnected;
- `stale`: latest says connected but the lease elapsed; and
- `unknown`: no valid retained presence exists.

The Will timestamp is the CONNECT time because a broker cannot rewrite its JSON
payload. `status: disconnected` is therefore decisive regardless of its age.

### 4.4 Idempotency, ordering, and limits

- `requestId` is also the idempotency key. For every side-effecting command, the
  device durably stores the canonical body hash, in-progress intent, and terminal
  response by `(principalId, requestId)` for 24 hours, across daemon/device
  restart. Capacity is 10,000 unexpired entries per principal; when full the
  device rejects new commands with `busy` rather than evicting a live guarantee.
- Read-only `diagnostics.ping` responses are cached for 60 seconds. A
  `playlist.get-document` response is cached for a five-minute retrieval
  lifetime; `playlist.read-chunk` results are deterministic from pinned bytes;
  and a closed transfer tombstone remains for one hour. These operations never
  need a durable side-effect journal. Within the applicable cache lifetime,
  identical and conflicting duplicates follow the same rule below.
- An identical duplicate receives the stored response and MUST NOT repeat the
  effect. The same key with a different body returns `duplicate_conflict`.
- Before executing a relative operation such as next/previous, the device
  resolves and journals its absolute target. Recovery reapplies or observes that
  target rather than advancing again. Disruptive and long-running commands
  journal their operation/confirmation ID before returning accepted.
- Input uses its stricter stream sequence, device epoch, short expiry, and clean
  MQTT session instead of writing high-frequency events to flash. The device
  retains the latest 512 sequence hashes/results per active stream for 10
  seconds and stream tombstones for one hour. A stream from an old device epoch
  is always rejected. Before accepting input after daemon/renderer restart, the
  executor sends a safety release for every keyboard code and pointer button;
  failure keeps the input capability unavailable rather than risking stuck
  state.
- One principal may have at most four active input streams and the device at
  most 32. A stream is active from its first accepted request until ten seconds
  after its last request with no held key/button; a pointer stream remains
  active through its lease. Retirement creates the one-hour tombstone. At most
  2048 unexpired tombstones per principal and 16384 per device are held in RAM,
  never flash. At either active-stream or tombstone capacity, a new `streamId`
  fails deterministically with `busy`; existing streams and their safety
  releases continue, and no live tombstone is evicted. Expired tombstones are
  removed oldest-first. These bounds prevent UUID rotation from exhausting the
  daemon while preserving duplicate detection.
- A request is valid only when `issuedAt` is no more than 30 seconds in the
  future and `expiresAt` is later than `issuedAt` and no more than 5 minutes
  later. Input commands further restrict TTL below.
- State order uses `epoch` and `revision`. Command completion order is not
  inferred from MQTT arrival order.
- JSON command bodies are limited to 131072 bytes. An inline DP-1 Playlist is
  limited to 126976 serialized UTF-8 bytes so the required command envelope and
  Playlist wrapper fit that body limit. Larger DP-1 Playlists must use an HTTPS URI. The
  full MQTT packet limit is 262144 bytes.
- Rate limits are token buckets and are fully represented in
  `capabilities.state.limits.rateLimits`; the advertised values are
  authoritative at runtime,
  not lower-than-hidden defaults. `pointer` covers `input.pointer` and cancel;
  `keyboardText` covers keyboard and text; `playlist` covers
  `playlist.display`, `playlist.cancel-scheduled`, and
  `playlist.display-default`; `playback` covers `playback.*`; `playlistRead`
  covers `playlist.get-document`, `playlist.read-chunk`, and
  `playlist.close-transfer`;
  `displayDevice` covers display, audio, settings, and diagnostics; and
  `privileged` covers system, support, SSH, controller-enrollment, invitation,
  and session-administration
  operations. A request consumes one token. Exhaustion returns `rate_limited`
  with `details.retryAfterMs`. A device MUST NOT advertise an omitted
  command-class limit and enforce a lower one.

## 5. LAN HTTPS and local push adapter

The LAN API listens on TCP 443 and uses TLS 1.3. The server always presents its
certificate and requests a client certificate during the TLS handshake. Every
runtime REST or WebSocket route requires a valid enrolled-controller or
session-bounded client certificate and enforces its device, controller, session,
and scope binding. Only the invitation claim and acknowledgement routes in the
controller-authentication profile accept an absent client certificate; they
require the QR credential and pinned server SPKI. The service is advertised as
`_ff1-control._tcp.local` using mDNS/DNS-SD. TXT keys are
`id=<deviceId>`, `api=2`, `auth=mtls`, and
`fp=<base64url-sha256-spki>`.
Discovery is only a hint; identity comes from the authenticated certificate.

The mapping is mechanical:

| MQTT operation | LAN operation |
|---|---|
| read or subscribe to retained `presence` | `GET /ff/v2/devices/{deviceId}/presence` or subscribe to WebSocket resource `presence` |
| read or subscribe to retained `capabilities` | `GET /ff/v2/devices/{deviceId}/capabilities` or subscribe to WebSocket resource `capabilities` |
| read or subscribe to retained `state/{resource}` | `GET /ff/v2/devices/{deviceId}/state/{resource}` or subscribe to WebSocket resource `state/{resource}` |
| publish the authenticated `sessions/{sessionId}/commands/{class}/{name}` | `POST /ff/v2/devices/{deviceId}/commands/{class}/{name}` |
| subscribe to response Topic Name | the POST response body |
| subscribe to exact event Topic Names | subscribe to the same exact event types on the LAN WebSocket |

GET responses validate against the same JSON representation as MQTT retained
payloads. POST request and response bodies validate against the same JSON
envelopes as MQTT; transport metadata never leaks into them. An accepted
long-running command returns HTTP 202; a completed command returns 200. Failed
envelopes use the `error.status` HTTP status. Capabilities and state use
`ETag: "<epoch>:<revision>"`; presence uses the quoted lowercase SHA-256 of the
JCS-canonical presence JSON because it has no revision envelope.
`If-None-Match` is supported. No command uses GET, no GET carries a body, and no
device-control command is accepted over WebSocket. REST remains the only LAN
command binding.

### 5.1 WebSocket endpoint and handshake

Every FF1 v2 device MUST provide authenticated local push at:

`wss://<device-host>/ff/v2/devices/{deviceId}/stream`

The opening handshake follows RFC 6455 on the same TLS 1.3 listener as the REST
API. The client offers `Sec-WebSocket-Protocol: ff-control.v2`; the server
MUST select exactly `ff-control.v2`. Controller identity, device binding, role,
and scopes are fixed from the validated client certificate before HTTP Upgrade
and cannot be asserted in a WebSocket message. An invalid presented certificate
fails TLS; no client certificate receives HTTP 401 on this route; a valid
certificate without device access receives 403; a wrong device path receives
404; and a missing or unsupported subprotocol receives 400 without upgrading.

Native clients omit `Origin`. If a client sends `Origin`, it MUST equal the
HTTPS origin named by the request Host; otherwise the server returns 403. The
comparison is browser-only defense in depth and is not client authentication or
authorization; LAN authorization remains the mTLS identity and scope grant.
The server negotiates no WebSocket extension, including compression. One
complete application message is one UTF-8 JSON Text Message; RFC 6455
fragmentation may carry it, but the reassembled message is limited to 262144
UTF-8 bytes. Binary
Message closes with status 1003, invalid UTF-8 or JSON with 1007, and an
oversized Message with 1009. Normal server shutdown uses 1001. At the earlier
of the client certificate's `notAfter` and the connection-local LAN
authorization-lease deadline, or immediately on controller/session/scope
revocation, the server stops
enqueueing resource/event Messages and sends Close 1008 before terminating the
connection. The same authorization-lease deadline applies to REST requests on that
TLS connection: a request received at or after the deadline returns HTTP 401
with `code: "unauthenticated"` and the client establishes a new mutually
authenticated TLS connection. An enrolled controller does this silently; it
does not repeat the QR ceremony. An open HTTP or WebSocket connection never
extends authorization beyond its LAN authorization lease or certificate validity.

### 5.2 Control and result messages

Every WebSocket JSON object is closed and requires `apiVersion: "ff/v2"` and a
`type`. Client control messages also require a UUIDv7 `messageId` that is never
reused on that connection. The server retains up to 4096 canonical control-
message hashes and results for the connection: an identical duplicate returns
the same result, while the same ID with different content returns
`duplicate_conflict`. At ledger capacity, a new control message returns `busy`
and the client reconnects rather than allowing ID reuse or unbounded storage.

A subscription request is exactly:

```json
{
  "apiVersion": "ff/v2",
  "messageId": "019bf2f1-ec99-77a5-bbd8-d041936b22aa",
  "type": "subscribe",
  "resources": ["presence", "capabilities", "state/display", "state/health"],
  "events": ["display.panel-apply-failed"],
  "sendInitial": true
}
```

`resources` is a unique array containing 1..10 values from `presence`,
`capabilities`, and the eight `state/{resource}` names in section 4.2. `events`
is a unique array of 0..32 exact event `type` values from section 13.
`sendInitial` MUST be `true`. Each requested event requires its corresponding
watermark resource in `resources`: playlist or playback -> `state/playback`,
display -> `state/display`, system -> `state/health`, support ->
`state/support`, sessions -> `state/sessions`, and
controllers/security -> `state/controllers`. The server authorizes the complete request against section
7.4; one denied resource or event rejects the whole request. One connection may
hold at most 16 subscriptions.

The successful correlated result is:

```json
{
  "apiVersion": "ff/v2",
  "messageId": "019bf2f1-ec99-77a5-bbd8-d041936b22aa",
  "type": "result",
  "success": true,
  "result": {
    "subscriptionId": "019bf2f2-68e1-7709-926e-4fcf8f21867a"
  }
}
```

For failure, `success` is false, `result` is absent, and `error` is the exact
section 6.3 error object with `instance` equal to `urn:uuid:<messageId>`.
Success requires `result` and forbids `error`; failure requires `error` and
forbids `result`. Schema errors use `schema_invalid`, denied selections use
`forbidden`, and the subscription limit uses `busy`.

### 5.3 Initial snapshot and live messages

Before returning the successful result, the server atomically registers the
subscription's change listener and begins buffering matching changes. It then
sends the result, followed by current authorized resources in this fixed order:
`presence`, `capabilities`, then `state/{resource}` lexicographically. Each
initial resource Message is:

```json
{
  "apiVersion": "ff/v2",
  "type": "resource",
  "subscriptionId": "019bf2f2-68e1-7709-926e-4fcf8f21867a",
  "resource": "state/display",
  "initial": true,
  "data": {
    "apiVersion": "ff/v2",
    "deviceId": "FF1-01234567",
    "epoch": "019bf2b4-2589-7d73-8a69-4525a5136629",
    "revision": "44",
    "eventWatermark": "83",
    "generatedAt": "2026-07-21T08:15:31.123Z",
    "state": {
      "mode": "awake",
      "rotationDegrees": 0,
      "effectiveDisplayPreferences": {
        "scaling": "fit",
        "margin": 0,
        "background": "#000000"
      },
      "sleepSchedule": {
        "enabled": false,
        "sleepTime": "23:00",
        "wakeTime": "07:00",
        "timeZone": "Asia/Ho_Chi_Minh",
        "days": ["sun", "mon", "tue", "wed", "thu", "fri", "sat"],
        "override": "none"
      },
      "panel": {
        "available": true,
        "brightnessPercent": 80,
        "power": "on",
        "lastApply": "succeeded"
      },
      "audio": {
        "available": true,
        "volumePercent": 40,
        "muted": false
      }
    }
  }
}
```

`data` is the exact JSON object used as the corresponding MQTT Application
Message payload and validates against the same schema; JSON serialization bytes
may differ. After initial resources, the server drains presence changes captured
after the initial presence read, capability/state versions newer by
`(epoch, revision)`, and events above the matching snapshot's
`eventWatermark`, then enters live delivery. A subsequent selected resource
change sends the same `resource` Message with `initial: false` and the complete
new representation; patches and transport-specific projections are
forbidden. Thus device information, network, DDC panel, health, playback, and
connection-state changes use the same source-of-truth mutation and revision on
MQTT, REST GET, and LAN push.

A matching transient event is:

```json
{
  "apiVersion": "ff/v2",
  "type": "event",
  "subscriptionId": "019bf2f2-68e1-7709-926e-4fcf8f21867a",
  "data": {
    "apiVersion": "ff/v2",
    "eventId": "019bf2ed-ad56-7d25-845a-c11af13139e0",
    "deviceId": "FF1-01234567",
    "epoch": "019bf2b4-2589-7d73-8a69-4525a5136629",
    "sequence": "84",
    "occurredAt": "2026-07-21T08:15:31.123Z",
    "expiresAt": "2026-07-21T08:25:31.123Z",
    "type": "display.panel-apply-failed",
    "data": {
      "control": "brightness",
      "requested": 80,
      "code": "ddc_timeout"
    }
  }
}
```

The nested `data` is the exact MQTT event payload. Events remain advisory and
never mutate client state; a later full resource snapshot is authoritative.

### 5.4 Unsubscribe, reconnect, heartbeat, and backpressure

Unsubscribe is the exact closed object `apiVersion`, UUIDv7 `messageId`,
`type: "unsubscribe"`, and `subscriptionId`. Success uses the result envelope
above with `result: {"subscriptionId": <same UUID>, "status": "unsubscribed"}`.
An identical duplicate `messageId` returns its stored success; a new message ID
for an unknown or already removed subscription returns `not_found`.

Subscriptions exist only for the WebSocket connection. On closure or network
loss, the server discards them; there is no durable WebSocket session or event
replay. A reconnecting client opens a new connection and subscribes with
`sendInitial: true`, so current snapshots plus epoch/revision/watermark rules
converge every selected resource after the outage. Transient events that expire
or occur while the client is disconnected are intentionally unrecoverable and
MUST NOT be treated as operation completion. An operation whose completion is
recoverable across reconnect MUST expose its current or recent terminal outcome
in the corresponding retained state for the retention period defined by that
state schema. Event watermarks order and deduplicate received events but do not
create replay. The state of this socket tells a controller whether its own LAN
path is connected; MQTT `presence` continues to describe the device's broker
connection and is not rewritten as per-controller LAN presence.

The server sends an RFC 6455 Ping Control Frame after 30 seconds without
traffic. The client replies with Pong within 10 seconds; otherwise the server
sends Close 1008 with reason `pong_timeout` when possible and terminates the
connection. JSON ping/pong messages are not part of the protocol.

Each connection has one outbound application-message queue limited to 256
Messages or 1 MiB, whichever is reached first. Pending resource Messages MAY be
coalesced only to the newest complete snapshot with the same
`(subscriptionId, resource)` key; events and results are never merged. If
enqueueing a Message would exceed either limit, the server stops enqueueing
application Messages, sends Close 1008 with reason `backpressure` through the
WebSocket control-frame path, and terminates the connection. This destroys all
subscriptions; the client reconnects, creates new subscriptions with
`sendInitial: true`, and converges from authoritative snapshots. ETag polling
remains a supported client fallback, but the WebSocket push endpoint is a
required FF1 v2 device capability.

## 6. Request, response, and error envelopes

### 6.1 Request

```json
{
  "apiVersion": "ff/v2",
  "requestId": "019bf2ba-879a-714e-a42c-f8f27bd9ff72",
  "issuedAt": "2026-07-21T08:15:30.123Z",
  "expiresAt": "2026-07-21T08:16:00.123Z",
  "params": {}
}
```

All five fields are required. `params` is the operation-specific closed object.
Authentication identity is never accepted from the JSON body; it comes from
the MQTT connection or LAN client certificate.

### 6.2 Response

```json
{
  "apiVersion": "ff/v2",
  "requestId": "019bf2ba-879a-714e-a42c-f8f27bd9ff72",
  "completedAt": "2026-07-21T08:15:30.247Z",
  "status": "succeeded",
  "result": {}
}
```

`status` is exactly `succeeded`, `accepted`, or `failed`. `result` is required
for succeeded or accepted and forbidden for failed. `error` is required for
failed and forbidden otherwise. `accepted` means the device durably recorded
the operation, not that the operation completed. Its result contains an
`operationId` or `confirmationId`, and progress is published as state/events.

For shutdown, reboot, or factory reset, the device publishes the accepted
response and waits for the MQTT PUBACK, or writes the complete HTTPS response,
before beginning the disruptive action.

### 6.3 Error

The nested error object reuses RFC 9457 member names and adds stable `code`,
`retryable`, and optional closed `details`:

```json
{
  "apiVersion": "ff/v2",
  "requestId": "019bf2ba-879a-714e-a42c-f8f27bd9ff72",
  "completedAt": "2026-07-21T08:15:30.247Z",
  "status": "failed",
  "error": {
    "type": "https://api.feralfile.com/problems/interaction-not-allowed",
    "title": "Interaction not allowed",
    "status": 403,
    "detail": "The active DP-1 PlaylistItem does not permit pointer drag.",
    "instance": "urn:uuid:019bf2ba-879a-714e-a42c-f8f27bd9ff72",
    "code": "interaction_not_allowed",
    "retryable": false,
    "details": {"interaction": "drag"}
  }
}
```

Stable base codes and HTTP mappings are:

This nesting is an FF adaptation: a runtime HTTP failure remains the same
response envelope and `application/json` body as MQTT rather than becoming a
top-level `application/problem+json` document. The standalone SoftAP API uses
RFC 9457 directly because it has no MQTT-equivalent envelope.

| Code | HTTP | Retryable by default |
|---|---:|---:|
| `invalid_request`, `schema_invalid`, `invalid_claim` | 400 | no |
| `unauthenticated` | 401 | no |
| `forbidden`, `interaction_not_allowed`, `scope_denied`, `origin_denied` | 403 | no |
| `not_found` | 404 | no |
| `conflict`, `duplicate_conflict`, `confirmation_required`, `invitation_consumed` | 409 | no |
| `precondition_failed`, `clock_unsynchronized` | 412 | no |
| `not_supported`, `capability_unavailable` | 422 | no |
| `expired`, `invitation_expired` | 422 | no |
| `rate_limited` | 429 | yes |
| `busy` | 503 | yes |
| `dependency_unavailable` | 503 | yes |
| `timeout` | 504 | yes |
| `internal_error` | 500 | yes |

DP-1 errors preserve the specification names under an explicit namespace:
`dp1.playlistInvalid`, `dp1.sigInvalid`, `dp1.licenseDenied`,
`dp1.reproMismatch`, and `dp1.sourceUnreachable`. Their default HTTP status is
422 except `dp1.sourceUnreachable`, which is 503 and retryable.

## 7. Authentication, TPM, and authorization

### 7.1 Device identity

At manufacture or first secure boot, FF1 generates distinct ECDSA P-256 device
TLS, controller-credential issuer, and device-local controller-CA private keys
inside TPM 2.0. The private keys are non-exportable. Enrollment proves
possession, issues an X.509 device certificate whose SAN is
`URI:urn:ff:device:<deviceId>`, and binds that identity to the exact MQTT Client
ID. Renewal uses the existing TPM key or an attested replacement key.

The broker authenticates the device with standard TLS client-certificate
authentication and maps the certificate identity to an ACL. TPM attestation is
an enrollment concern; it is not placed in MQTT JSON and does not create a
custom device MQTT authentication algorithm. The device profile does not use
MQTT Enhanced Authentication; controller access-session connections do.

### 7.2 Controller identity

The proposed identity, QR invitation, enrollment, access-session, guest-session,
credential, and revocation rules are defined in
[FF1 v2 controller authentication and access sessions](ff1-v2-controller-authentication.md).

Every controller has distinct P-256 signing and encryption key pairs. Mobile,
CLI, and installed integrations normally hold a persistent, revocable
controller enrollment. Web clients and
temporary agents normally claim a non-renewable guest session. Enrollment is
not a permanent MQTT Session or a non-expiring access token. Every MQTT control
connection uses a finite FF1-issued access session. An enrolled LAN connection
uses its client certificate and a connection-local authorization lease instead;
a guest LAN lease is additionally bounded by the existing guest access session.

An FF1 accepts multiple independent mobile, CLI, and integration enrollments.
Each installation scans once, stores its own enrollment material, and obtains
access sessions silently. Expiry of an access session never requires a QR scan
or invalidates another controller.

Normal remote access uses MQTT 5 Enhanced Authentication:

- User Name (UTF-8 Encoded String) = `sessionId`;
- Password is absent;
- Authentication Method (UTF-8 Encoded String) = `FF1-JWT-ES256-PoP`;
- CONNECT Authentication Data (Binary Data) = UTF-8 JSON containing the
  FF1-issued access-session JWS;
- server-authenticated TLS 1.3; and
- Client Identifier = `ff-session-<sessionId>` and equal to the token claim.

The broker then sends an unpredictable challenge in an AUTH Control Packet
with Reason Code `0x18` (Continue authentication). The controller responds in
an AUTH Control Packet with Reason Code `0x18` and an ES256 proof signed by the
private key corresponding to the credential's RFC 7800 `cnf.jwk`. The proof is
bound to the broker audience, device, access session, exact Client Identifier,
and challenge. The broker sends successful CONNACK only after validating and
atomically consuming that challenge. Section 7 of the authentication profile
defines the exact Authentication Data and failure mapping.

Invitation claimants and enrolled controllers requesting a new access session
also use standard MQTT User Name and Password fields, but broker ACLs restrict
those credentials to their exact claim or session-issuance Topic Names. They
cannot read device state or execute a control command and do not use MQTT
Enhanced Authentication.

The broker schedules a forced disconnect at the access credential's `exp` with
DISCONNECT Reason Code `0x87` (Not authorized). FF1 independently checks the
session record and expiry for every delivered command. Revocation therefore
takes effect on FF1 even before broker disconnection completes.

V2 adds no general payload encryption above TLS. JWE is used only to deliver an
enrollment or access credential to the controller encryption public key during the
authentication profile. Clients MUST NOT add opaque encrypted JSON to normal
command, response, state, or event Topic Names.

An enrollment MAY also contain a unique LAN client certificate issued by the
device-local controller CA. LAN uses mTLS, the same controller enrollment and
scope ceiling, and no unauthenticated runtime fallback. Agent or integration
guest sessions MAY receive a session-bounded LAN certificate; web guests are
MQTT-only in v2.0.0.

### 7.3 Time prerequisite

The device synchronizes NTP before remote token or certificate authentication
is enabled. It persists a last-known-good UTC floor and never moves trusted time
backward. During the same boot, trusted time advances from the last NTP sample
using a monotonic clock. Across power loss, offline certificate validation is
allowed only if the platform provides a trusted advancing RTC at or above the
persisted floor; otherwise it fails closed until NTP succeeds.
`state/device.clock.status` reports `unsynchronized`, remote MQTT remains
disconnected, and new enrollment, session, and LAN-certificate issuance is
blocked. Runtime LAN mTLS fails during the
TLS handshake with an appropriate certificate/time alert, before any HTTP
`clock_unsynchronized` response can be sent. SoftAP recovery remains available.
This preserves RFC 5280 validity checks rather than treating a stale floor as
current time.

### 7.4 ACLs and scopes

The broker and LAN server enforce the same operation scopes. Guest sessions
receive an explicit subset under the controller-authentication profile. Web
guests are MQTT-only; agent and integration guests may receive a
session-bounded LAN certificate. Every active MQTT access session may read the
requested device's presence and capabilities and use its own response Topic
Name. Every active LAN authorization lease may GET those two resources and
subscribe to them through local push; LAN has no response Topic Name. This
authenticated baseline does not imply `state:read` and exposes no operational
or other-controller state.

| Scope | Grants |
|---|---|
| `state:read` | non-sensitive `device`, `playback`, `display`, and `health` state; read-only diagnostics; exactly the playback and display events granted in the event ACL table below |
| `network:read` | `network` state, including SSID/IP addresses, and network diagnostics |
| `playback:control` | display a DP-1 Playlist, control active playback, and read `playback` state/events; not full Playlist retrieval |
| `playlist:read` | non-retained retrieval of the complete signed DP-1 Playlist; potentially discloses source URIs or credentials embedded by its signer |
| `display:control` | DP-1 display overrides, rotation, schedule, panel, `audio.*`, plus `display` state/events |
| `input:control` | `input.*` while DP-1 permits the interaction, plus `playback` state needed to identify the active PlaylistItem |
| `device:settings` | `settings.*` plus `device` state |
| `system:update` | `system.update` plus `health` state and update events |
| `system:power` | `display.sleep`, `display.wake`, shutdown, reboot, `display`/`health` state, and corresponding events |
| `system:reset` | initiate factory reset, plus `health` state and confirmation/reset events; physical confirmation still required |
| `support:upload` | support bundle creation/upload plus `support` state/events |
| `ssh:manage` | short-lived SSH access; LAN owner controllers only |
| `controllers:manage` | owner-only: create enrollment invitations, list, change, and revoke controller enrollments plus `controllers` state/events and security events |
| `sessions:manage` | create guest-session invitations and list or revoke guest sessions plus `sessions` state/events |

An MQTT controller may publish only to its authorized devices and subscribe
only to those devices' state/events and its own response Topic Name. Over MQTT,
devices may subscribe only to their own access-session command,
invitation-claim, and enrolled-session request subtrees and publish only to
their own presence, capability, state, event, response, invitation, and
session-response subtrees. The device repeats authorization after broker
delivery; broker ACLs are not the only enforcement layer. LAN uses the scope-
equivalent REST and local-push mappings from section 5.

Event subscriptions use one exact Topic Name per granted event type. This is
required because `system:update`, `system:power`, and `system:reset` authorize
different names under the shared `events/system/` prefix. Every granted event
type includes read access to the retained resource carrying its
`eventWatermark`, so the reconnect algorithm is always satisfiable. A broad
`events/+/+` subscription is permitted only when the principal holds every
event-type grant.

The event ACL is exhaustive. A principal is authorized when it holds at least
one scope in the `Any one of` column; the broker grants only the exact Topic
Name formed from that event type, and the LAN server applies the same row to a
WebSocket subscription. No prefix grant is implied.

| Event type | Any one of | Required watermark resource |
|---|---|---|
| `playback.error` | `state:read`, `playback:control` | `state/playback` |
| `playlist.display-missed` | `state:read`, `playback:control` | `state/playback` |
| `display.panel-apply-failed` | `state:read`, `display:control` | `state/display` |
| `system.update-progress` | `system:update` | `state/health` |
| `system.update-completed` | `system:update` | `state/health` |
| `system.confirmation-requested` | `system:reset` | `state/health` |
| `system.confirmation-resolved` | `system:reset` | `state/health` |
| `system.factory-reset-starting` | `system:reset` | `state/health` |
| `system.power-starting` | `system:power` | `state/health` |
| `support.progress` | `support:upload` | `state/support` |
| `sessions.invitation-closed` | `sessions:manage`, `controllers:manage` | `state/sessions` |
| `sessions.claimed` | `sessions:manage`, `controllers:manage` | `state/sessions` |
| `sessions.revoked` | `sessions:manage` | `state/sessions` |
| `sessions.expired` | `sessions:manage` | `state/sessions` |
| `controllers.enrolled` | `controllers:manage` | `state/controllers` |
| `controllers.revoked` | `controllers:manage` | `state/controllers` |
| `security.authorization-denied` | `controllers:manage` | `state/controllers` |

An empty `scopes` array in a capability entry means the authenticated transport
baseline—an active MQTT access session or LAN authorization lease—not
unauthenticated access.

Factory reset always requires an on-screen physical confirmation and clears the
device certificate, controller CA, controller-credential issuer key,
controller enrollments, invitations, access-session records, cached access
tokens, Wi-Fi
credentials, owner enrollment, DP-1 cache, local support bundles, and retained broker
topics. SSH is
LAN-only, owner-only, and capped at one hour. Support upload never accepts a
service API key from a controller payload.

State Topic Name ACLs are the union of the table grants: `device` requires
`state:read|device:settings`; `playback` requires
`state:read|playback:control|input:control`; `display` requires
`state:read|display:control|system:power`; `health` requires
`state:read|system:update|system:power|system:reset`; `network` requires
`network:read`; `support` requires
`support:upload`; `controllers` requires `controllers:manage`; and `sessions`
requires `sessions:manage|controllers:manage`. Resources are never
per-principal filtered after subscription.

## 8. Controller enrollment and access sessions

The proposed protocol is
[FF1 v2 controller authentication and access sessions](ff1-v2-controller-authentication.md).
It defines one QR invitation and claim protocol for:

- the first owner enrollment;
- later mobile, CLI, and installed-integration enrollments; and
- guest sessions for web clients and temporary agents.

The QR contains a five-minute FF1-signed invitation credential, FF1 issuer
public JWK, broker URI and audience, device ID, invitation ID, claim ID, Client Identifier, invited
client kind, scope ceiling, and intended result.
It is sufficient to submit one MQTT claim without LAN discovery or a Feral
File-specific handoff service. The same claim has a pinned HTTPS adapter for an
explicitly trusted or offline LAN. FF1 atomically consumes the first valid claim
across both bindings and delivers credentials in JWE encrypted to the claimant's
controller encryption key.

An enrollment is the persistent, revocable public-key authorization for both
controller keys. It survives app and FF1 restart, access-session expiry, and
transport reconnect. Enrolled controllers request successive 15-minute access
sessions without user interaction or another QR scan.
Guest sessions are non-renewable, last from 5 minutes through 24 hours, and are
created by an enrolled controller holding `sessions:manage`. FF1 selects,
enforces, and revokes every invitation and session expiry.

The retained `state/controllers` representation is visible only with
`controllers:manage` and contains persistent enrollments:

```json
{
  "apiVersion": "ff/v2",
  "deviceId": "FF1-01234567",
  "epoch": "019bf2b4-2589-7d73-8a69-4525a5136629",
  "revision": "7",
  "eventWatermark": "83",
  "generatedAt": "2026-07-21T08:15:30.123Z",
  "state": {
    "controllers": [{
      "controllerId": "019bf2c1-baae-7379-af80-3a328bec5e57",
      "label": "Anh's iPhone",
      "clientKind": "mobile",
      "signingKeyThumbprint": "nZ8Q2xA6S8v3P4rM8BfBNPWYd8kI9JQsp2cKXQwH_3E",
      "encryptionKeyThumbprint": "Zgwp7YvYjHtBtWnO6fOuJnWHZyFJlgGlVxXEQbBUt0A",
      "role": "owner",
      "scopes": ["state:read", "playback:control", "input:control", "controllers:manage", "sessions:manage"],
      "credentialExpiresAt": "2027-07-21T08:00:00.000Z",
      "lanCertificate": {
        "fingerprint": "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
        "expiresAt": "2027-07-21T08:00:00.000Z"
      },
      "createdAt": "2026-07-21T08:00:00.000Z",
      "lastUsedAt": "2026-07-21T08:15:30.000Z",
      "status": "active"
    }, {
      "controllerId": "019bf2c9-46fb-7b5f-905a-7c71fae5b0df",
      "label": "Gallery CLI",
      "clientKind": "cli",
      "signingKeyThumbprint": "mgmXJ2HoYq5fNMYT7U1GqzJf7wED2O0mK3NjCDJVjR4",
      "encryptionKeyThumbprint": "e8pN7vK6YwM2GcQ5aD0hXrJ4sB1tLuF9zZqW3iAoE6I",
      "role": "delegate",
      "scopes": ["state:read", "playback:control"],
      "credentialExpiresAt": "2027-07-21T08:05:00.000Z",
      "createdAt": "2026-07-21T08:05:00.000Z",
      "lastUsedAt": "2026-07-21T08:14:10.000Z",
      "status": "active"
    }]
  }
}
```

Every controller entry requires `controllerId`, `label`, `clientKind`,
`signingKeyThumbprint`, `encryptionKeyThumbprint`, `role`, `scopes`, `createdAt`,
`credentialExpiresAt`, and `status`. `lastUsedAt` and `lanCertificate` are
optional; the latter requires SHA-256 `fingerprint` and `expiresAt`. Role is
`owner|delegate`; status is `active|revoked`. Up to 32 entries may be active at
once. Each entry represents one installation and has independent keys, scopes,
credential rotation, revocation, and access sessions. Revoked entries remain
visible for audit until factory reset but their key is never accepted.

`state/sessions` is defined in section 12.7, with underlying authorization
records and invalidation rules in the authentication profile. It contains only
non-secret invitation and session metadata. Invitation credentials, QR
payloads, access credentials, enrollment credentials, JWE plaintext, and
private-key material never appear in retained state or controller events.

## 9. Capabilities

The retained capability document is authoritative; clients MUST capability-gate
operations and DP-1 extensions rather than infer support from firmware version.

```json
{
  "apiVersion": "ff/v2",
  "deviceId": "FF1-01234567",
  "epoch": "019bf2b4-2589-7d73-8a69-4525a5136629",
  "revision": "3",
  "eventWatermark": "83",
  "generatedAt": "2026-07-21T08:15:30.123Z",
  "state": {
    "contractVersions": ["2.0.0"],
    "transports": {
      "mqtt": {"wss443": true, "maxPacketBytes": 262144},
      "lanHttps": true,
      "lanPush": {
        "webSocket": true,
        "subprotocol": "ff-control.v2",
        "maxMessageBytes": 262144,
        "maxSubscriptions": 16
      }
    },
    "controllerAccess": {
      "primaryInvitationTransport": "mqtt",
      "invitationTransports": ["mqtt", "lan_https"],
      "credentialIssuer": "ff1_tpm_es256",
      "invitationTypes": ["owner_enrollment", "controller_enrollment", "guest_session"],
      "invitationMaxSeconds": 300,
      "enrolledSessionMaxSeconds": 900,
      "guestSessionMaxSeconds": 86400,
      "maxControllerEnrollments": 32,
      "maxActiveSessions": 16,
      "guestClientKinds": ["web", "agent", "integration"],
      "guestScopeCeiling": ["state:read", "playback:control", "display:control", "input:control"]
    },
    "dp1": {
      "coreVersions": ["1.1.0"],
      "extensions": [
        {"id": "playlists", "versions": ["0.1.0"]},
        {"id": "com.feralfile.input", "versions": ["1.0.0"]}
      ],
      "maxInlineBytes": 126976,
      "maxDocumentBytes": 2097152,
      "signatureAlgorithms": ["ed25519", "ecdsa-p256"]
    },
    "commands": [
      {"name": "playlist.display", "scopes": ["playback:control"]},
      {"name": "playlist.get-document", "scopes": ["playlist:read"]},
      {"name": "playlist.cancel-scheduled", "scopes": ["playback:control"]},
      {"name": "playlist.read-chunk", "scopes": ["playlist:read"]},
      {"name": "playlist.close-transfer", "scopes": ["playlist:read"]},
      {"name": "playlist.display-default", "scopes": ["playback:control"]},
      {"name": "playback.pause", "scopes": ["playback:control"]},
      {"name": "playback.resume", "scopes": ["playback:control"]},
      {"name": "playback.next", "scopes": ["playback:control"]},
      {"name": "playback.previous", "scopes": ["playback:control"]},
      {"name": "playback.select", "scopes": ["playback:control"]},
      {"name": "playback.refresh", "scopes": ["playback:control"]},
      {"name": "playback.set-item-duration", "scopes": ["playback:control"]},
      {"name": "playback.set-shuffle", "scopes": ["playback:control"]},
      {"name": "playback.set-loop", "scopes": ["playback:control"]},
      {"name": "display.set-overrides", "scopes": ["display:control"]},
      {"name": "display.set-rotation", "scopes": ["display:control"]},
      {"name": "display.set-sleep-schedule", "scopes": ["display:control"]},
      {"name": "display.sleep", "scopes": ["system:power"]},
      {"name": "display.wake", "scopes": ["system:power"]},
      {"name": "display.panel", "scopes": ["display:control"]},
      {"name": "audio.set-volume", "scopes": ["display:control"]},
      {"name": "audio.set-muted", "scopes": ["display:control"]},
      {"name": "input.keyboard", "scopes": ["input:control"]},
      {"name": "input.text", "scopes": ["input:control"]},
      {"name": "input.pointer", "scopes": ["input:control"]},
      {"name": "input.pointer-cancel", "scopes": ["input:control"]},
      {"name": "settings.set-analytics", "scopes": ["device:settings"]},
      {"name": "settings.set-beta-features", "scopes": ["device:settings"]},
      {"name": "diagnostics.ping", "scopes": ["state:read"]},
      {"name": "system.update", "scopes": ["system:update"]},
      {"name": "system.power", "scopes": ["system:power"]},
      {"name": "system.factory-reset", "scopes": ["system:reset"]},
      {"name": "support.create-bundle", "scopes": ["support:upload"]},
      {"name": "ssh.set-access", "scopes": ["ssh:manage"]},
      {"name": "controllers.create-invitation", "scopes": ["controllers:manage"]},
      {"name": "controllers.close-invitation", "scopes": ["controllers:manage"]},
      {"name": "controllers.renew-credential", "scopes": []},
      {"name": "controllers.set-scopes", "scopes": ["controllers:manage"]},
      {"name": "controllers.revoke", "scopes": ["controllers:manage"]},
      {"name": "sessions.create-invitation", "scopes": ["sessions:manage"]},
      {"name": "sessions.close-invitation", "scopes": ["sessions:manage"]},
      {"name": "sessions.revoke", "scopes": ["sessions:manage"]}
    ],
    "hardware": {
      "panelDdc": true,
      "audio": true,
      "tpm": "2.0",
      "keyboard": true,
      "relativePointer": true
    },
    "limits": {
      "commandBytes": 131072,
      "rateLimits": {
        "pointer": {"perSecond": 30, "burst": 10},
        "keyboardText": {"perSecond": 10, "burst": 10},
        "playlist": {"perSecond": 5, "burst": 5},
        "playback": {"perSecond": 10, "burst": 10},
        "playlistRead": {"perSecond": 20, "burst": 8},
        "displayDevice": {"perSecond": 5, "burst": 5},
        "privileged": {"perSecond": 1, "burst": 2}
      }
    }
  }
}
```

The actual `commands` array lists every enabled operation. Optional hardware or
software paths are absent, not advertised with a false command. Capability and
state documents themselves remain closed objects; vendor additions use their
`extensions` member.

For contract version 2.0.0, `transports.lanPush` is required and closed:
`webSocket` is exactly true, `subprotocol` is exactly `ff-control.v2`,
`maxMessageBytes` is 262144, and `maxSubscriptions` is 16. A device that cannot
provide this channel does not advertise v2 LAN conformance; client polling is a
fallback behavior, not permission for the device to omit push.

`controllerAccess` is required and validates against the proposed controller
authentication and access-session profile. A client MUST NOT infer invitation
types, credential issuer, session limits, or guest permissions from firmware
version or broker vendor. `primaryInvitationTransport` is always `mqtt`;
`lan_https` is present only when the pinned invitation adapter is available.
`maxControllerEnrollments` limits persistent controller installations;
`maxActiveSessions` separately limits concurrent live access credentials.

## 10. DP-1 profile

DP-1 defines the Playlist carried by this API. It does not define the command
envelope or transport.

- `playlist.display` accepts an unmodified DP-1 core v1.1.0 Playlist inline or
  by URI. `dpVersion`, optional Playlist UUID, items, signatures, defaults,
  display, provenance, and licensing retain DP-1 meanings.
- The device validates the official DP-1 JSON Schema, canonicalizes with JCS,
  verifies at least one trusted `feed` or `curator` signature, resolves
  `defaults -> ref -> item.local`, and applies a device override only where DP-1
  resolution says `userOverrides.<field>` is true. An omitted
  `userOverrides.<field>` resolves to DP-1's default `true`; FF does not treat
  omission as denial.
- `interaction.keyboard` contains W3C `KeyboardEvent.code` strings.
  `interaction.mouse` contains DP-1's `click`, `scroll`, `drag`, and `hover`
  booleans. The input commands below enforce those permissions.
- A mutable external provenance resource at `provenance.contract.uri` requires
  `provenance.contract.metaHash` as DP-1 specifies. A URI command may
  additionally pin the complete Playlist using `sha256`.
- Core `dynamicQueries` (plural) is not valid v2. The singular `dynamicQuery`
  shape is accepted only when the device advertises the draft DP-1 `playlists`
  extension and the document validates against that extension's composed
  schema.
- Display scheduling, loop, shuffle, and remote navigation MUST NOT be inserted
  into a signed DP-1 Playlist. They are FF control operations; scheduled
  display uses `displayAt`.
- The device stores and reports the original signed document and its hash. It
  never rewrites the signed DP-1 Playlist to reflect device overrides.
- `com.feralfile.input` v1.0.0 is an explicitly FF-owned item extension for
  committed Unicode text permission. It does not change core keyboard or mouse
  fields and is ignored by players that do not advertise it.

The FF1 v2.0 trust profile is fixed:

- accepted algorithms are `ed25519` and `ecdsa-p256`; another DP-1 algorithm is
  rejected until capabilities and this profile are versioned;
- multi-signature Playlists require at least one cryptographically valid
  `feed` or `curator` signature whose key fingerprint is in the signed FF trust
  bundle or device-local institution trust store; if an `agent` signature is
  present, DP-1 requires it to verify as well;
- `did:key` is resolved locally. HTTPS JWKS resolution is allowed only from the
  playlist origin's `/.well-known/jwks.json`, with normal TLS validation, a
  5-second timeout, and a cached key matched by exact `kid`. Arbitrary DID web
  resolution and cross-origin JWKS are not supported in v2.0;
- the signed trust bundle contains monotonically increasing version,
  `validFrom`, `validUntil`, trusted key fingerprints/roles, and revoked kids.
  Rollback below the highest TPM-sealed version is rejected;
- a newly received Playlist is not accepted after its required trust data
  expires while offline. A previously verified, hash-identical cached Playlist
  remains playable offline using its persisted verification record unless a
  locally available signed revocation says otherwise; and
- a single-signature Playlist is accepted only for a trusted Ed25519 feed key.
  Unsigned `license: open` Playlists are not accepted.

Signature timestamps more than five minutes in the future are rejected. A
failed cryptographic signature, untrusted/revoked key, stale trust bundle for a
new Playlist, or unsupported algorithm returns `dp1.sigInvalid` with a stable
sanitized `details.reason`. This policy makes acceptance consistent across
FF OS, app fixtures, CLI, and offline cache tests.

Playlist URI fetching is constrained control-plane input, not a general FF1
HTTP proxy. The URI must be HTTPS with no user-info or fragment; response media
type must be JSON; decoded content is limited to 2 MiB; timeout is 15 seconds;
and at most three HTTPS redirects are followed. Loopback, link-local, multicast,
and private-address targets are rejected before and after every DNS resolution
unless a future advertised LAN-content capability defines a separately pinned
origin. DNS rebinding and redirect-to-private failures return
`dp1.sourceUnreachable`. PlaylistItem source schemes remain governed by DP-1
and the device's advertised renderer capabilities.

FF extends control around DP-1 without creating an FF-specific DP-1 dialect.

## 11. Command catalogue

This section is exhaustive for v2.0.0. The MQTT operation name is
`{class}.{name}` and maps to topic suffix `{class}/{name}` and HTTPS path suffix
`commands/{class}/{name}`. `params: {}` means an object with no properties.

The notation `string(1..64)` means a UTF-8 string with inclusive Unicode scalar
length. `int[a..b]` is an inclusive JSON integer. `oneOf(A,B)` means exactly one
listed shape. Every params and result object is closed. In addition to listed
errors, every operation may return the common authentication, authorization,
schema, expiry, duplicate, rate, dependency, and internal errors from section
6.3.

A synchronous state mutation that changes exactly one retained resource returns
this exact `revision` object:

```json
{
  "resource": "playback",
  "epoch": "019bf2b4-2589-7d73-8a69-4525a5136629",
  "value": "43"
}
```

The resource enum is one of the state resources in section 4.2. A synchronous
mutation that atomically changes more than one retained resource MUST omit
`revision` and return `revisions`: a nonempty array of the same closed revision
objects, sorted lexically by `resource`, with each changed resource appearing
exactly once. No result contains both `revision` and `revisions`.

An `itemSelector` is exactly one of `{"itemId": <DP-1 PlaylistItem UUID>}` or
`{"index": <int[0..99999]>}`. DP-1 PlaylistItem IDs are optional, so every
operation and state representation remains usable by index when a valid core
document omits them. An item result is
`{"index": <int>, "itemId"?: <DP-1 PlaylistItem UUID>}`.

### 11.1 Playlist display and playback

| Operation | Exact `params` | Exact successful `result` | Operation-specific failures |
|---|---|---|---|
| `playlist.display` | `playlist`: oneOf(`{"type":"inline","document":<DP-1 Playlist>}`, `{"type":"uri","uri":https-uri(1..2048),"sha256"?:sha256}`); `displayAt`?: receiver time +5 seconds through +30 days; `replaceScheduled`?: boolean, allowed only with `displayAt` and default false; `start`?: `itemSelector` | succeeded: `playlistId`?: DP-1 UUID; `sha256`: hash of original DP-1 Playlist; `display`: `active\|scheduled`; `displayAt`?: timestamp, required iff scheduled; `revision`. Accepted: `operationId`; `source`: exact closed `{"type":"inline"}` or `{"type":"uri"}`; `sha256`?: request pin or computed hash; `displayAt`?: timestamp | DP-1 errors, `precondition_failed` for a bad pin, `capability_unavailable` for an unadvertised extension, `not_found` for start item, `conflict` for another pending display or existing schedule without replacement |
| `playlist.get-document` | `target`: `active\|scheduled` | `playlistId`?: DP-1 UUID; `sha256`; exactly one of `inlineDocument`: original signed DP-1 Playlist, or `transfer`: transfer manifest defined below | `not_found` if that target has no verified Playlist |
| `playlist.cancel-scheduled` | oneOf pending `{"target":"pending","operationId":<UUIDv7>}`, scheduled `{"target":"scheduled","expectedSha256"?:<sha256>}` | `target`: `pending\|scheduled`; `operationId`?: required for pending; `sha256`?: known hash; `revision` | `not_found`; `precondition_failed` for a different scheduled hash |
| `playlist.display-default` | `{}` | `playlistId`?: DP-1 UUID; `sha256`; `revision` | `not_found` if no cached default is available |
| `playback.pause` | `{}` | `paused: true`; `revision` | `conflict` if nothing is active |
| `playback.resume` | `{}` | `paused: false`; `revision` | `conflict` if nothing is active |
| `playback.next` | `{}` | `item`: item result; `revision` | `conflict` if no next item under loop mode |
| `playback.previous` | `{}` | `item`: item result; `revision` | `conflict` if no previous item under loop mode |
| `playback.select` | `item`: `itemSelector` | `item`: item result; `revision` | `not_found` |
| `playback.refresh` | `item`?: `itemSelector`; omitted means current item | `item`: item result; `revision` | `not_found`, `dp1.sourceUnreachable` |
| `playback.set-item-duration` | `item`: `itemSelector`; `durationMs`: int[1000..86400000] | `item`: item result; `durationMs`; `revision` | `not_found` |
| `playback.set-shuffle` | `enabled`: boolean | `enabled`; `revision` | none |
| `playback.set-loop` | `mode`: `none\|playlist\|one` | `mode`; `revision` | none |

`displayAt` is FF scheduling metadata, not a mutation of DP-1. One verified
scheduled display and one pending display are allowed per device. A future
display does not stop or replace the active Playlist: after verification it
appears as `scheduledDisplay` while the current `mode`, `activePlaylist`, and
`currentItem` remain unchanged. If a schedule exists, `replaceScheduled: false`
returns `conflict`; true verifies and durably stores the new Playlist before
atomically replacing the old schedule. A second display while any display is
pending returns `conflict`. At `displayAt`, the device atomically makes the
scheduled Playlist active and clears `scheduledDisplay`.
`playlist.cancel-scheduled` cancels either the scheduled display or its
still-pending request. A display
missed by more than five minutes is cleared, emits
`playlist.display-missed`, and does not start automatically.
During a replacement fetch, both the existing `scheduled` target and new
`pending` target exist; `playlist.cancel-scheduled.target` selects exactly one,
and a pending cancellation must match its `operationId`.

`playlist.get-document` requires `playlist:read`, is non-mutating, and selects
the active or scheduled document explicitly. It is the live DP-1 recovery path
after reconnect. A document at most 126976 bytes is returned as
`inlineDocument`. A larger document returns the closed transfer manifest:
`transferId`: UUIDv7, `sizeBytes`: int[126977..2097152], `chunkBytes`: exactly
49152, `chunkCount`: int[3..43], `sha256`, and `expiresAt`: five minutes after
creation. The device pins the exact cached JCS UTF-8 bytes for that controller and
transfer lifetime.

`playlist.read-chunk` has exact params `transferId`: UUIDv7 and `index`:
int[0..chunkCount-1]. Its exact result is `transferId`, `index`, `chunkCount`,
`encoding: "base64url"`, `data`: unpadded base64url, and `chunkSha256`: SHA-256
of decoded bytes. Requests may be retried and made in parallel up to four; the
same index always returns identical bytes. `playlist.close-transfer` has exact
params `transferId` and succeeds with `transferId`, `status: "closed"`; closing
again is idempotent. Both require the controller that opened the transfer and
`playlist:read`; another controller gets `not_found`. Expired transfers return
`expired`. The client concatenates decoded chunks in index order, requires
exact `sizeBytes`, verifies the manifest `sha256`, then parses and validates the
original signed DP-1 Playlist JSON value.

No retrieval response is retained, put in state/events, or logged. Because the
unmodified signed DP-1 Playlist can itself contain source URIs or signer-embedded
credentials, `playlist:read` is a sensitive disclosure scope and is never implied
by `state:read` or `playback:control`. The device MUST cache the complete JCS
UTF-8 bytes of the original signed document for every active or scheduled
Playlist; it never depends on the origin URI remaining reachable. The pull-
chunk protocol is an FF application-layer extension needed because MQTT
Maximum Packet Size bounds one Control Packet; every MQTT Application Message
payload remains JSON. HTTPS invokes the same open/read/close commands and JSON
schemas rather than inventing a byte-download API.

`playback.set-item-duration` is an FF session playback override. DP-1 core has no
duration-control permission, so the command does not edit the signed playlist
and is an FF control extension, like shuffle and playlist-level loop mode.

`playlist.display-default` uses the device's last verified default or cached
owner Playlist, so broker availability is never required to recover playback.

### 11.2 Display preferences, sleep, panel, and audio

| Operation | Exact `params` | Exact successful `result` | Operation-specific failures |
|---|---|---|---|
| `display.set-overrides` | `item`?: `itemSelector`; at least one of `scaling`: `fit\|fill\|stretch\|auto`, `margin`: number[0..4096] or DP-1 CSS unit string, `background`: DP-1 color string; omitted `item` means current | `item`: item result; effective supplied fields; `revision` | `interaction_not_allowed` when any corresponding resolved DP-1 `userOverrides` value is false; `not_found` |
| `display.set-rotation` | `degrees`: `0\|90\|180\|270` | `degrees`; `revision` | `capability_unavailable` |
| `display.set-sleep-schedule` | `enabled`: boolean; `sleepTime`: `HH:MM`; `wakeTime`: `HH:MM`; `timeZone`: IANA zone(1..64); `days`: unique nonempty array of `sun\|mon\|tue\|wed\|thu\|fri\|sat` | normalized `enabled`, `sleepTime`, `wakeTime`, `timeZone`, and Sunday-first `days`; `revision` | `invalid_request` when times are equal, zone unknown, `days` is empty, duplicated, or invalid |
| `display.sleep` | `{}` | `mode: "sleeping"`; `revision` | `dependency_unavailable` if the player cannot enter sleep; DDC failure is reported in state/event but does not roll back player sleep |
| `display.wake` | `{}` | `mode: "awake"`; `revision` | `dependency_unavailable` if the player cannot wake; DDC failure is reported in state/event |
| `display.panel` | oneOf brightness `{control:"brightness",percent:int[0..100]}`, contrast `{control:"contrast",percent:int[0..100]}`, panel volume `{control:"speaker-volume",percent:int[0..100]}`, panel mute `{control:"speaker-mute",muted:boolean}`, power `{control:"power",state:"on\|standby\|off"}` | `control`; requested value; `observed` value if readable; `revision` | `capability_unavailable`, `dependency_unavailable` |
| `audio.set-volume` | `percent`: int[0..100] | `percent`; `revision` | `capability_unavailable` |
| `audio.set-muted` | `muted`: boolean | `muted`; `revision` | `capability_unavailable` |

`display.set-rotation` rotates the physical viewport and is deliberately
separate from DP-1 display preferences. Absolute setters replace current
relative `rotate` and `toggleMute` operations so QoS 1 retries remain
idempotent.

Panel `speaker-volume` and `speaker-mute` use DDC VCPs. They are distinct from
`audio.set-volume` and `audio.set-muted`, which control the FF1 OS mixer. A
capability may expose either or both audio paths; clients never infer one from
the other.

On a selected day, the device follows the configured wake/sleep window. On an
unselected day it remains asleep for the complete civil day. A daily schedule
supplies all seven values; the result and retained state always contain the
explicit normalized array. Manual sleep or wake creates an override until
the next clock occurrence of the opposite schedule boundary, after which the
weekday-aware schedule resumes. The display state reports that boundary and
whether panel DDC application is pending or failed.

### 11.3 Mobile keyboard and touchpad

Remote input is allowed only while an active DP-1 PlaylistItem explicitly permits the
corresponding interaction. Input requests have a maximum TTL of 5 seconds for
keyboard and 2 seconds for pointer. The device drops an expired batch rather
than replaying stale input.

#### `input.keyboard`

Exact params:

```json
{
  "deviceEpoch": "019bf2b4-2589-7d73-8a69-4525a5136629",
  "streamId": "019bf2d1-e77b-7c35-925c-70e5577e4a41",
  "sequence": "18",
  "events": [{
    "type": "down",
    "code": "KeyA",
    "key": "A",
    "modifiers": ["shift"],
    "repeat": false,
    "offsetMs": 0
  }, {
    "type": "up",
    "code": "KeyA",
    "key": "A",
    "modifiers": ["shift"],
    "repeat": false,
    "offsetMs": 50
  }]
}
```

- `deviceEpoch` is the current retained state epoch; mismatch returns
  `conflict`. `streamId` is a UUIDv7 created when the remote-control view opens.
- `sequence` is an unsigned decimal string, strictly increasing per stream.
- `events` contains 1..32 entries.
- `type` is `down|up`; `code` is a W3C UI Events code advertised by the active
  DP-1 PlaylistItem; `key` is 1..32 Unicode scalars; `modifiers` is a unique sorted subset
  of `alt|control|meta|shift`; `repeat` is boolean; `offsetMs` is int[0..250]
  relative to the preceding event.

Total keyboard `offsetMs` is at most 3000ms. Before applying any event, the
device verifies that `receiverCurrentTime + totalOffsetMs` is no later than the
request's effective deadline; otherwise the complete batch fails with
`expired`. If the deadline is nevertheless reached during execution, no later
event is delivered, every pressed code is released, and the response is
`expired`.

The exact successful result is `streamId`, `sequence`, `processed`: int[1..32],
and `activeItem`: item result. A repeated or lower sequence returns the already
stored result when identical, otherwise `conflict`. A code absent from the
active item's `interaction.keyboard` returns `interaction_not_allowed`. The
device emits matching browser key-down/up semantics; numeric ASCII key codes
from v1 are not accepted.

Every key down MUST have a matching key up for the same code later in the same
batch. The device validates the complete batch before applying it, and on any
mid-batch renderer failure attempts to release every code already pressed before
returning `dependency_unavailable`. Keyboard state never persists across a
request, item change, renderer restart, or controller disconnect.

#### `input.text`

Mobile IMEs produce committed text that has no truthful physical
`KeyboardEvent.code`. This FF extension preserves Unicode input without
inventing W3C codes. Exact params are `deviceEpoch`: current UUIDv7,
`streamId`: UUIDv7, `sequence`: unsigned decimal string, and `text`: 1..1024
Unicode scalars and at most 4096 UTF-8 bytes,
with no unpaired surrogate. The exact succeeded result is `streamId`,
`sequence`, `insertedScalars`, and `activeItem`: item result. The device uses the
browser renderer's text-insertion primitive; backspace, enter, escape, arrows,
and shortcuts remain `input.keyboard`.

`input.text` requires `input:control` and this signed PlaylistItem-level DP-1 extension:

```json
{
  "extensions": {
    "com.feralfile.input": {"version": "1.0.0", "text": true}
  }
}
```

The device must advertise `com.feralfile.input` before accepting it. This is an
FF DP-1 extension, not DP-1 core; absence or `text: false` returns
`interaction_not_allowed`. Text content is never recorded in logs, state, or
events.

Keyboard and text operations may share a stream ledger. Because they use
different MQTT topics, a controller MUST wait for the preceding response before
switching operation type on one stream; alternatively it uses distinct stream
IDs. No cross-topic ordering is assumed.

#### `input.pointer`

Exact params:

```json
{
  "deviceEpoch": "019bf2b4-2589-7d73-8a69-4525a5136629",
  "streamId": "019bf2d1-e77b-7c35-925c-70e5577e4a41",
  "sequence": "19",
  "leaseMs": 1500,
  "events": [
    {"type": "move", "dx": 12.5, "dy": -4.0, "offsetMs": 0},
    {"type": "button", "button": "left", "state": "down", "offsetMs": 0},
    {"type": "move", "dx": 8.0, "dy": 2.0, "offsetMs": 16},
    {"type": "button", "button": "left", "state": "up", "offsetMs": 0}
  ]
}
```

- The common fields have the same ordering semantics as keyboard.
- `leaseMs` is int[250..2000]. `events` contains 0..16 entries and at most
  1500ms total `offsetMs`; zero events are valid only to renew an already-held
  button lease.
- A move event has `type: "move"`, finite `dx` and `dy` in [-4096,4096], and
  `offsetMs` int[0..1500]. Values are CSS pixels relative to the current pointer.
- A button event has `type: "button"`, `button: "left|middle|right"`,
  `state: "down|up"`, and `offsetMs` int[0..1500].
- A wheel event has `type: "wheel"`, finite `deltaX` and `deltaY` in
  [-4096,4096] CSS pixels, and `offsetMs` int[0..1500].

Before applying any event, the device verifies that
`receiverCurrentTime + totalOffsetMs` is no later than the effective deadline;
otherwise the complete batch fails with `expired`. If the deadline is reached
during execution, no later event is delivered, every held button is released,
and the response is `expired`.

The exact successful result is `streamId`, `sequence`, `processed`: int[0..16],
`activeItem`: item result, `cursor: {"x":number,"y":number}`,
`buttonsDown`: a unique sorted array of `left|middle|right`, and
`holdExpiresAt` when nonempty. A stream belongs to its authenticated principal
and may hold at most one button. Duplicate down or an up for a button not held
is rejected before the batch is applied. An accepted batch renews the held
button until `receiverCurrentTime + leaseMs`, allowing long drags to span many
bounded move batches.

The device synthesizes button up on lease expiry, explicit
`input.pointer-cancel`, item or playlist change, permission/scope revocation,
renderer restart, daemon shutdown, or any partial batch failure. A LAN transport
closure also releases immediately; MQTT does not expose another client's
disconnect to the device, so its lease is the mandatory remote backstop. A
failed release is returned as `dependency_unavailable` and the release is
retried until the renderer exits or confirms it. This bounded lease, not
unbounded client buffering, prevents a dropped mobile request from leaving a
stuck button.

`input.pointer-cancel` has exact params `deviceEpoch`: current UUIDv7,
`streamId`: UUIDv7, and `sequence`: unsigned decimal string. Its exact succeeded
result is `streamId`, `sequence`,
`released`: a unique sorted button array, and `cursor`. It requires
`input:control`, is idempotent, and succeeds with an empty `released` array when
the stream no longer holds a button.

Permission mapping is exact:

| Pointer sequence | Required DP-1 `interaction.mouse` flag |
|---|---|
| move without a pressed button | `hover` |
| down then up without intervening move; one or multiple clicks | `click` |
| move while a button is down | `drag` |
| wheel | `scroll` |

A button down is provisionally accepted when either `click` or `drag` is true.
Any movement while held requires `drag`. A release with no intervening movement
requires `click`; if only drag was permitted the device still performs the
safety release but returns `interaction_not_allowed` and suppresses the click
where the renderer supports suppression.

The app implements tap, double tap, long press, cursor movement, click-and-drag,
and pinch UI by producing these atomic batches. The relative-coordinate and
timed-batch format is an FF protocol extension because DP-1 specifies permission
flags, not a remote-input wire format. Pinch has no separate wire operation. A client represents pinch as a wheel
event, which requires DP-1 `scroll`. The device clamps the cursor to the visual viewport and never treats
input as a system-level HID event; it is delivered only to the active PlaylistItem
renderer.

### 11.4 Settings and health

| Operation | Exact `params` | Exact successful `result` | Operation-specific failures |
|---|---|---|---|
| `settings.set-analytics` | `enabled`: boolean | `enabled`; `revision` for `device` | none |
| `settings.set-beta-features` | `enabled`: boolean | `enabled`; `revision` for `device` | none |

#### `diagnostics.ping`

Exact params are `nonce`: string(1..64). The exact succeeded result is the same
`nonce`, `deviceTime`: timestamp, `uptimeMs`: unsigned decimal string,
`clockStatus`: `synchronized|degraded|unsynchronized`, and `connectionId`:
UUIDv7 for the current MQTT or TLS connection. It requires `state:read`, does
not mutate state, and is the read-only correlated RPC for connectivity
diagnosis.

Device, player, and DDC status are retained state and LAN GET resources, not
commands. A command that merely requests a status poll is not part of v2.

### 11.5 Update, power, reset, support, and SSH

| Operation | Exact `params` | Exact response | Restrictions and failures |
|---|---|---|---|
| `system.update` | `target`: exactly `latest`; `channel`: `stable\|beta` | accepted: `operationId`, `targetVersion`, `channel` | `system:update`; AC/network/storage preconditions; beta requires enabled preference; progress in state/events |
| `system.power` | `action`: `shutdown\|reboot`; `delaySeconds`: int[0..60] | accepted: `operationId`, `action`, `executeAt` | `system:power`; response is acknowledged before action |
| `system.factory-reset` | `{}` | accepted: `confirmationId`, `expiresAt` | `system:reset`; always requires device-screen confirmation within 60 seconds; no remote confirmation command; `busy` while another reset confirmation is pending |
| `support.create-bundle` | `supportBundleId`: caller-generated UUIDv7; `title`: string(1..120); `components`: unique nonempty subset of `chromium\|controld\|setupd\|sys-monitor\|watchdog\|system`; `upload`: boolean | accepted: `operationId`, same `supportBundleId` | `support:upload`; ID is bound to this owner/device and joins app/device evidence; upload uses device-side scoped grant/presigned URL; no controller API key |
| `ssh.set-access` | enable shape: `enabled:true`, `publicKey`: OpenSSH public key(1..8192), `ttlSeconds`: int[60..3600]; disable shape: `enabled:false` only | enabled: `enabled:true`, `expiresAt`, `fingerprint`; disabled: `enabled:false` | `ssh:manage`; LAN owner certificate only; one active key; factory reset clears it |

`system.update` is never an instruction to download an arbitrary URL or execute
an arbitrary version string. The signed update service resolves `latest` for
the selected permitted channel. A progress event is advisory; retained health
state is authoritative after reconnect.

A support bundle ID is a cross-source correlation key, not an authorization
token. The control plane permits app and device components to join only when
owner principal and device binding match; reuse by another principal/device or
with conflicting metadata returns `conflict`. FF1 never accepts or waits for an
`app` collection component. Application evidence is uploaded independently
under the same bundle ID through a hosted interface outside this device
contract. Hosted aggregation cannot change or delay the FF1 operation's
terminal status.

Before transmitting the accepted `system.factory-reset` response, the device
atomically commits both its byte-equivalent idempotency record and a pending
entry in `state/health.factoryResetConfirmations`, then publishes the updated
retained health state. It next transmits the accepted response, displays the
confirmation prompt, and emits `system.confirmation-requested`. A crash at any
later boundary therefore restores the same response and pending confirmation.
On physical approval the device first updates that retained entry to
`approved`, then publishes `system.confirmation-resolved` with
`decision: "approved"`, then publishes
`system.factory-reset-starting`, clears retained MQTT resources by
publishing zero-length retained messages, waits for PUBACKs up to two seconds,
sends a normal MQTT DISCONNECT so the Will cannot recreate retained presence,
then resets. On denial or expiry it first updates the entry to `rejected` or
`expired`, emits `system.confirmation-resolved`, and does nothing else.

External request IDs MUST NOT be interpreted as physical approval.

If an offline, screen-initiated reset cannot reach the broker, local erasure
still proceeds. Retained messages expire within 24 hours; the next device
enrollment atomically revokes the prior identity and controller grants and
purges the old device topic subtree before any new controller is authorized.
For an online reset, the control plane records the confirmation event and
performs the same identity revocation/topic purge even if the connection drops
between a device tombstone and its clean DISCONNECT.

### 11.6 Controller administration

| Operation | Exact `params` | Exact successful `result` | Restrictions and failures |
|---|---|---|---|
| `controllers.create-invitation` | `label`?: string(1..64); `clientKind`: `mobile\|cli\|integration`; `requestedScopes`: unique subset of known controller scopes; `expiresInSeconds`: int[60..300], default 300 | `invitationId`, `expiresAt`, `qrDisplayed: true`, `revision` for `sessions` | owner plus `controllers:manage`; one active enrollment invitation; 32 active enrollments maximum; `busy`, `scope_denied` |
| `controllers.close-invitation` | `invitationId`: UUIDv7 | same ID, `status: "closed\|already_closed"`, revision for `sessions` | creator or owner with `controllers:manage`; `not_found` only when the ID never belonged to this device |
| `controllers.renew-credential` | `controllerId`: UUIDv7; `newSigningKeyJwk`: public ECDSA P-256 JWK; `newEncryptionKeyJwk`: public ECDH P-256 JWK; `oldKeyProof`: detached compact JWS by old signing key; `newKeyProof`: detached compact JWS by new signing key | `controllerId`, `credentialEnvelope`, `credentialExpiresAt`, optional `lanCertificateExpiresAt`, `revision` for `controllers` | same active controller; at most once per 24 hours; proof and envelope rules are in the authentication profile; `expired`, `invalid_claim`, `conflict`, `rate_limited` |
| `controllers.set-scopes` | `controllerId`: UUIDv7; `scopes`: unique nonempty subset of controller scopes | `controllerId`, `scopes`, `revision` for `controllers` | owner with `controllers:manage`; cannot grant it to a delegate or remove either management scope from an owner; target must exist; grant must be within caller ceiling |
| `controllers.revoke` | `controllerId`: UUIDv7; `revokeCreatedGuestSessions`?: boolean, default false | `controllerId`, `status: "revoked"`; `revision` for `controllers` when no session projection changes, otherwise `revisions`: exactly `controllers` and `sessions` in lexical resource order | cannot revoke the last owner manager; active MQTT sessions are disconnected |

Listing is `state/controllers`, not a command. The invitation credential is
shown only in the FF1 QR and is intentionally absent from the command response.
A physically present user may scan it; a remote caller may only cause the
short-lived QR to appear.

Only an owner may change or revoke another owner. No command can change a
controller's role or remove/revoke the final active owner; owner-role transfer
requires a new physical owner-enrollment invitation. These checks are based on
the stored role and authenticated principal, never a request-body assertion.

### 11.7 Guest-session administration

Web clients, temporary agents, and other bounded requesters use the guest-session
profile defined in
[FF1 v2 controller authentication and access sessions](ff1-v2-controller-authentication.md).
There is no client-kind-specific invitation protocol, relayer, web HTTP relay
endpoint, Mint Pairing Broker, or relayer session token.

| Operation | Exact `params` | Exact successful `result` | Operation-specific failures |
|---|---|---|---|
| `sessions.create-invitation` | `label`?: string(1..64); `clientKind`: `web\|agent\|integration`; `requestedScopes`: unique subset of guest-eligible scopes; `sessionSeconds`: int[300..86400], default 3600; `origin`?: normalized HTTPS origin, required for `web` | `invitationId`, `expiresAt`, `sessionSeconds`, `qrDisplayed: true`, revision for `sessions` | `capability_unavailable`, `busy`, `scope_denied`, `origin_denied` |
| `sessions.close-invitation` | `invitationId`: UUIDv7 | same ID, `status: "closed\|already_closed"`, revision for `sessions` | `not_found` only when the ID never belonged to this device; creator or `sessions:manage` |
| `sessions.revoke` | `sessionId`: UUIDv7 | same ID, `status: "revoked\|already_revoked"`, revision for `sessions` | `not_found` only when the ID never belonged to this device; creator or `sessions:manage` |

Creating the invitation is the approval action. There is no later
request/decision exchange. The claimant proves possession of its key, consumes
the QR once, and receives its FF1-issued credential encrypted to that key. The
QR credential, claimant key coordinates, MQTT credential, and any later DP-1
Playlist URI are never returned to the inviting controller.

## 12. State resources

Each resource uses the revision envelope from section 3.3. The `state` member is
the exact closed object below. A field marked optional is omitted when unknown.
Sensitive resources are separate topics so broker ACLs do not depend on payload
filtering.

### 12.1 `state/device`

```json
{
  "model": "FF1",
  "hardwareRevision": "1.0",
  "serialNumber": "FF1-01234567",
  "firmware": {
    "installedVersion": "2.0.0",
    "latestVersion": "2.0.1",
    "channel": "stable"
  },
  "clock": {
    "status": "synchronized",
    "timeZone": "Asia/Ho_Chi_Minh",
    "lastSyncAt": "2026-07-21T08:10:00.000Z"
  },
  "preferences": {
    "analyticsEnabled": false,
    "betaFeaturesEnabled": false
  }
}
```

Required fields are `model`, `hardwareRevision`, `serialNumber`, `firmware`,
`clock`, and `preferences`. `latestVersion` and `lastSyncAt` are optional.
`clock.status` is `synchronized|degraded|unsynchronized`; `firmware.channel` is
`stable|beta`. MAC addresses, Chromium debug URLs, access tokens, service API
keys, and Wi-Fi credentials are forbidden in this resource.

### 12.2 `state/network`

This resource requires `network:read` and is not part of the broad
`state:read` subscription.

```json
{
  "connectivity": "internet",
  "interface": "wifi",
  "ssid": "Studio",
  "addresses": ["192.0.2.10", "2001:db8::10"],
  "signalPercent": 82,
  "lastChangedAt": "2026-07-21T08:12:00.000Z"
}
```

`connectivity` is `none|lan|internet|captive_portal`; `interface` is
`wifi|ethernet|none`; `ssid` and `signalPercent` are present only for Wi-Fi;
signal is int[0..100]. Addresses are canonical IP literals, at most eight, and
never include link-local IPv6 zone IDs. Passwords, BSSIDs, and MAC addresses are
forbidden.

### 12.3 `state/playback`

```json
{
  "mode": "playing",
  "activePlaylist": {
    "id": "f47ac10b-58cc-4372-a567-0e02b2c3d479",
    "dpVersion": "1.1.0",
    "sha256": "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
    "signatureStatus": "verified"
  },
  "itemCount": 12,
  "currentItem": {
    "id": "64f6379f-e948-4de3-93e7-f8f1a994bff5",
    "index": 2,
    "title": "Example work",
    "artist": "Example artist",
    "durationMs": 30000,
    "elapsedMs": 12000
  },
  "itemWindow": [{
    "id": "64f6379f-e948-4de3-93e7-f8f1a994bff5",
    "index": 2,
    "title": "Example work",
    "artist": "Example artist",
    "durationMs": 30000
  }],
  "windowStart": 2,
  "paused": false,
  "shuffle": false,
  "loop": "playlist"
}
```

Required fields are `mode`, `itemCount`, `itemWindow`, `windowStart`, `paused`,
`shuffle`, and `loop`. `mode` is
`idle|loading|playing|paused|sleeping|error`. While no verified
playlist is active, idle/loading state uses `itemCount: 0`, `itemWindow: []`,
and `windowStart: 0`.

`pendingDisplay` is optional in any mode and required when mode is `loading`. It is
the closed object `operationId`: UUIDv7, `source`: exact closed
`{"type":"inline"}` or `{"type":"uri"}`, optional
`sha256` known from a request pin or completed fetch, and optional `displayAt`.
Thus an existing Playlist may keep playing while a new pending display is
fetched; `loading` means there is no verified active Playlist to keep showing.

`activePlaylist` is required for `playing|paused|sleeping`, optional
for `error`, and absent for `idle|loading`. It requires `dpVersion`, `sha256`,
and `signatureStatus`; its `id` is present only when the DP-1 Playlist has one.
The retained resource never includes the original document, a PlaylistItem
source, or a Playlist URI. The device preserves the complete original signed
bytes so an authorized `playlist.get-document` can provide live DP-1 recovery.
`currentItem` is required for
`playing|paused|sleeping`, optional for `error`, and absent for
`idle|loading`. It and each item-window entry require `index`; their
`id` is optional for the same DP-1 reason. `itemWindow` contains at most 100
metadata-only items centered on the current index and MUST fit the packet limit.
Neither it nor any other retained playback field contains a PlaylistItem
`source`, inline DP-1 Playlist projection, token, or private URI credentials. A
controller uses `playlist.get-document` for the signed full list. `signatureStatus`
is `verified|verification_pending|failed`;
playback cannot enter playing with `failed`.

`scheduledDisplay` is independently optional in every mode. It is the closed
object `playlistId`?: DP-1 UUID, `dpVersion: "1.1.0"`, `sha256`, `displayAt`,
`itemCount`, and optional `start`: item result. It contains no document or URI.
At most one exists, and its presence never changes the meaning of `mode` or the
active fields. If mode is `error`, state additionally has the RFC
9457-compatible `lastError` object without a request instance. `elapsedMs` and
`durationMs` are optional when the source has no defined duration.

### 12.4 `state/display`

```json
{
  "mode": "awake",
  "rotationDegrees": 0,
  "effectiveDisplayPreferences": {
    "scaling": "fit",
    "margin": 0,
    "background": "#000000"
  },
  "sleepSchedule": {
    "enabled": true,
    "sleepTime": "23:00",
    "wakeTime": "07:00",
    "timeZone": "Asia/Ho_Chi_Minh",
    "days": ["sun", "mon", "tue", "wed", "thu", "fri"],
    "override": "none",
    "nextBoundaryAt": "2026-07-21T16:00:00.000Z"
  },
  "panel": {
    "available": true,
    "brightnessPercent": 70,
    "contrastPercent": 50,
    "speakerVolumePercent": 40,
    "speakerMuted": false,
    "power": "on",
    "lastApply": "succeeded"
  },
  "audio": {
    "available": true,
    "volumePercent": 40,
    "muted": false
  }
}
```

`mode` is `awake|sleeping|transitioning`; rotation is `0|90|180|270`.
`effectiveDisplayPreferences` is the resolved DP-1 `display` value after
permitted device overrides, not a rewrite of DP-1. Schedule `override` is
`none|manual_sleep|manual_wake`. `days` is a unique nonempty array in
Sunday-first order and is always present; all seven values means every day.
`nextBoundaryAt` is absent when disabled.
Optional panel readings are omitted individually when unsupported. `power` is
`on|standby|off|unknown`; `lastApply` is
`succeeded|pending|failed|not_attempted`. DDC is eventual: player sleep success
and panel apply state are reported independently.

### 12.5 `state/health`

```json
{
  "overall": "healthy",
  "controlTransport": {
    "status": "connected",
    "lastAttemptAt": "2026-07-21T08:14:55.000Z",
    "stage": "mqtt",
    "outcome": "succeeded"
  },
  "eventDelivery": {
    "queuedCount": 0,
    "droppedSinceBoot": "0"
  },
  "services": {
    "controld": "healthy",
    "setupd": "healthy",
    "sysMonitord": "healthy",
    "watchdog": "healthy",
    "player": "healthy"
  },
  "resources": {
    "memoryUsedPercent": 42,
    "storageUsedPercent": 61,
    "temperatureCelsius": 47.5
  },
  "cache": {
    "offlineReady": true,
    "activePlaylistVerified": true,
    "requiredAssetCount": 12,
    "readyAssetCount": 12,
    "verifiedPlaylistCount": 4,
    "artworkCount": 92,
    "bytes": "428713984",
    "lastSuccessfulPlaybackAt": "2026-07-21T08:15:00.000Z"
  },
  "update": {
    "status": "idle"
  },
  "factoryResetConfirmations": [{
    "confirmationId": "019bf2ea-7608-72be-9c2d-c78e547c1374",
    "status": "rejected",
    "requestedAt": "2026-07-21T08:10:00.000Z",
    "expiresAt": "2026-07-21T08:11:00.000Z",
    "resolvedAt": "2026-07-21T08:10:20.000Z"
  }],
  "lastError": {
    "code": "player_restart",
    "occurredAt": "2026-07-20T02:00:00.000Z",
    "summary": "Player recovered after restart"
  }
}
```

`overall` and each service are `healthy|degraded|unhealthy|unknown`.
`controlTransport` is required. Its status is `connected|disconnected`; stage is
`dns|tcp|tls|websocket|mqtt|authorization`; outcome is `succeeded|failed`; and
an unsuccessful attempt also has a sanitized stable `code` and optional MQTT
reason code. It records the latest attempt locally, is readable over LAN during
broker failure, and is published remotely after recovery. It contains no host
IP, credential, certificate body, SSID, or token.
`eventDelivery` is required. `queuedCount` is int[0..1024],
`droppedSinceBoot` is an unsigned decimal string reset on daemon epoch change,
and optional `oldestOccurredAt` is present only when the queue is nonempty.
Percentages are int[0..100], temperature is finite [-40..125], and resource
fields are individually optional. Cache readiness fields are required;
`offlineReady` is true only when the active/default verified playlist and every
asset required for autonomous playback are locally readable. `update.status` is
`idle|checking|downloading|verifying|installing|reboot_required|failed`; a
non-idle update also has `operationId`, optional `targetVersion`, and
`progressPercent` int[0..100]. `factoryResetConfirmations` is required and is a
newest-first array of 0..16 entries keyed by unique `confirmationId`. An entry
has `status: pending|approved|rejected|expired`, `requestedAt`, and `expiresAt`;
`resolvedAt` is forbidden for pending and required for every terminal status.
At most one entry is pending. Rejected and expired entries remain for 24 hours.
An approved entry remains until reset execution clears retained resources and
the device identity; there is no post-reset controller continuity to recover.
If the sixteen-entry bound is reached, a new reset request returns `busy` until
the oldest rejected or expired entry reaches its retention boundary. The entry
is committed before its requested/resolved event, making reconnect recovery
independent of transient-event delivery. `lastError` is optional, sanitized, and contains
no stack trace, secret, DP-1 source URL, user ID, SSID, or access credential.

### 12.6 `state/support`

```json
{
  "active": {
    "operationId": "019bf2e4-bbef-747e-9897-83c1416f4b91",
    "supportBundleId": "019bf2e4-e9ce-772e-930a-d6907b82b14e",
    "status": "uploading",
    "progressPercent": 70,
    "expiresAt": "2026-07-28T08:15:30.123Z"
  },
  "lastCompleted": {
    "supportBundleId": "019bf2a5-fab6-7ce4-b8be-ce5070c02ee4",
    "status": "uploaded",
    "completedAt": "2026-07-20T08:15:30.123Z"
  }
}
```

`active` and `lastCompleted` are independently optional. Status is
`collecting|redacting|uploading|uploaded|stored_local|failed`. A bundle ID is
safe to expose; an upload URL, bearer grant, service key, and local file path are
forbidden.

### 12.7 `state/sessions`

```json
{
  "invitations": [{
    "invitationId": "019bf2e9-c52c-7150-89fa-19592c207c75",
    "invitationType": "guest_session",
    "createdBy": "019bf2c1-baae-7379-af80-3a328bec5e57",
    "clientKind": "agent",
    "label": "Exhibition setup agent",
    "scopeCeiling": ["playback:control"],
    "sessionSeconds": 3600,
    "createdAt": "2026-07-21T08:15:00.000Z",
    "status": "open",
    "expiresAt": "2026-07-21T08:20:30.123Z"
  }],
  "sessions": [{
    "sessionId": "019bf2e0-6f8b-74ba-94b4-110bdd97d1d6",
    "sessionType": "guest",
    "controllerId": "019bf2eb-506d-7be1-9ab5-2effd08f2bc3",
    "clientKind": "web",
    "label": "Lobby display console",
    "origin": "https://museum.example",
    "scopes": ["playback:control"],
    "createdBy": "019bf2c1-baae-7379-af80-3a328bec5e57",
    "createdAt": "2026-07-21T07:30:00.000Z",
    "expiresAt": "2026-07-21T08:30:00.000Z",
    "status": "active"
  }]
}
```

`invitations` and `sessions` are required arrays, newest first and unique by ID.
They contain at most one open enrollment invitation, five open guest
invitations, and sixteen active sessions. Every invitation entry requires
`invitationId`, `invitationType`, `clientKind`, unique nonempty `scopeCeiling`,
`createdAt`, `expiresAt`, and `status: "open"`; `label` is optional. Its
conditional members are exact:

| `invitationType` | Required conditional members | Forbidden conditional members |
|---|---|---|
| `owner_enrollment` | `grantedRole: "owner"`; `clientKind: "mobile"\|"cli"\|"integration"` | `createdBy`, `origin`, `sessionSeconds` |
| `controller_enrollment` | `createdBy`: controller UUIDv7; `grantedRole: "delegate"`; `clientKind: "mobile"\|"cli"\|"integration"` | `origin`, `sessionSeconds` |
| `guest_session` | `createdBy`: controller UUIDv7; `clientKind: "web"\|"agent"\|"integration"`; `sessionSeconds`: int[300..86400]; `origin` only and always when `clientKind` is `web` | `grantedRole`; `origin` for non-web clients |

Every session entry requires `sessionId`, `sessionType`, `controllerId`,
`clientKind`, unique nonempty `scopes`, `createdAt`, `expiresAt`, and
`status: "active"`; `label` is optional. Its conditional members are exact:

| `sessionType` | Required conditional members | Forbidden conditional members |
|---|---|---|
| `enrolled_controller` | `role: "owner"\|"delegate"`; `clientKind: "mobile"\|"cli"\|"integration"` | `createdBy`, `origin` |
| `guest` | `createdBy`: controller UUIDv7; `clientKind: "web"\|"agent"\|"integration"`; `origin` only and always when `clientKind` is `web` | `role`; `origin` for non-web clients |

Only open invitations and active sessions are retained. On claim, close,
expiry, or revocation, FF1 atomically removes the terminal entry, advances the
`sessions` revision, and emits the corresponding section 13 event. Terminal
underlying records use `claimed|closed|expired` for invitations and
`expired|revoked` for sessions but never appear in this retained projection.
After `feral-controld` restart, all previously open invitations and active
sessions are invalidated and the first new snapshot omits them; persistent
controller enrollments remain in `state/controllers`.

MQTT credentials, invitation QR data, JWE, JWK coordinates, key thumbprints,
user agents, DP-1 Playlist URIs, and DP-1 Playlist documents are forbidden.
The underlying authorization records and invalidation rules are defined in
the controller-authentication profile. `state/controllers` was defined in
section 8.

## 13. Events

Every event uses this closed envelope:

```json
{
  "apiVersion": "ff/v2",
  "eventId": "019bf2ed-ad56-7d25-845a-c11af13139e0",
  "deviceId": "FF1-01234567",
  "epoch": "019bf2b4-2589-7d73-8a69-4525a5136629",
  "sequence": "83",
  "occurredAt": "2026-07-21T08:15:30.123Z",
  "expiresAt": "2026-07-21T09:15:30.123Z",
  "type": "system.update-progress",
  "data": {}
}
```

Sequence is device-wide within an epoch. The MQTT topic's `{class}.{name}` MUST
equal `type`. `expiresAt` is exactly `occurredAt +` the event type's Expiry
below. Events use QoS 1, are deduplicated by `eventId`, and are non-retained.
The exhaustive v2.0.0 event data schemas are:

For each event, the daemon atomically allocates its sequence and commits that
value as the mapped state resource's `eventWatermark`, incrementing that
resource revision even when no other field changed. It publishes the retained
state snapshot before the non-retained event. During disconnect, only the newest
state snapshot per resource needs retry because it is retained. Events enter a
local outbox bounded to 1024 entries and 4 MiB of canonical JSON, whichever is
reached first. On overflow the oldest event is dropped and
`state/health.eventDelivery.droppedSinceBoot` increments; retained state and its
watermark remain authoritative. An event is never published at or after its
absolute `expiresAt`; before that point its PUBLISH Message Expiry Interval is
`ceil(expiresAt - senderCurrentTime)`, so reconnect never resets the TTL. Live
clients still buffer across topics; the watermark rule in section 4.2 is the
deterministic barrier. There is no unsupported cross-topic arrival-order
assumption.

| Type | Exact `data` | Expiry |
|---|---|---:|
| `playback.error` | `code`: DP-1 or stable error code; `summary`: string(1..256); `playlistId`?: UUID; `itemId`?: UUID; `retryable`: boolean | 1 hour |
| `playlist.display-missed` | `playlistId`?: DP-1 UUID; `sha256`: SHA-256; `displayAt`: timestamp; `detectedAt`: timestamp | 1 hour |
| `display.panel-apply-failed` | `control`: `brightness\|contrast\|speaker-volume\|speaker-mute\|power`; `requested`: boolean, number, or enum string; `code`: stable error code | 10 minutes |
| `system.update-progress` | `operationId`: UUIDv7; `status`: update status except idle; `progressPercent`: int[0..100]; `targetVersion`?: semver | 1 hour |
| `system.update-completed` | `operationId`: UUIDv7; `status`: `succeeded\|failed`; `installedVersion`?: semver; `errorCode`?: stable code | 1 hour |
| `system.confirmation-requested` | `confirmationId`: UUIDv7; `action`: exactly `factory_reset`; `expiresAt`: timestamp | 2 minutes |
| `system.confirmation-resolved` | `confirmationId`: UUIDv7; `decision`: `approved\|rejected\|expired` | 10 minutes |
| `system.factory-reset-starting` | `confirmationId`: UUIDv7; `executeAt`: timestamp | 2 minutes |
| `system.power-starting` | `operationId`: UUIDv7; `action`: `shutdown\|reboot`; `executeAt`: timestamp | 2 minutes |
| `support.progress` | `operationId`: UUIDv7; `supportBundleId`: UUIDv7; `status`: support status; `progressPercent`: int[0..100] | 1 hour |
| `sessions.invitation-closed` | `invitationId`: UUIDv7; `invitationType`: `owner_enrollment\|controller_enrollment\|guest_session`; `outcome`: `claimed\|closed\|expired`; `resolvedAt`: timestamp | 10 minutes |
| `sessions.claimed` | `invitationId`: UUIDv7; `sessionId`: UUIDv7; `controllerId`: UUIDv7; `clientKind`: `mobile\|cli\|integration\|web\|agent`; `sessionType`: `enrolled_controller\|guest`; `scopes`: string[]; `expiresAt`: timestamp | 1 hour |
| `sessions.revoked` | `sessionId`: UUIDv7; `revokedBy`: controller UUIDv7 or `physical_ui`; `revokedAt`: timestamp | 10 minutes |
| `sessions.expired` | `sessionId`: UUIDv7; `expiredAt`: timestamp | 10 minutes |
| `controllers.enrolled` | `controllerId`: UUIDv7; `label`: string(1..64); `clientKind`: `mobile\|cli\|integration`; `scopes`: string[] | 1 hour |
| `controllers.revoked` | `controllerId`: UUIDv7; `revokedBy`: controller UUIDv7 or `physical_ui` | 1 hour |
| `security.authorization-denied` | `principalId`: UUIDv7; `operation`: operation name; `code`: `forbidden\|rate_limited`; `occurredAt`: timestamp | 10 minutes |

High-frequency input does not emit events. The direct command response is its
only acknowledgement; current pointer position is included there. Events never
contain secrets, full DP-1 Playlist documents, keystrokes, pointer coordinates, SSIDs,
IP addresses, or controller access tokens.

## 14. SoftAP setup and offline recovery profile

An unprovisioned device cannot use MQTT or controller mTLS. This profile is the
only non-MQTT-first part of the design and is intentionally isolated from the
runtime API.

FF1 starts a client-isolated SoftAP with no forwarding to other interfaces. The
SSID is `FF1-<last-six-serial-characters>` and each setup session has a new
random 128-bit WPA2/WPA3 transition-mode passphrase. The screen shows a Wi-Fi
join QR and `http://192.168.4.1/#s=<256-bit-base64url-secret>`. The fragment is
not sent in an HTTP request. Portal JavaScript places it in
`Authorization: FF-Setup <secret>` for API calls. The portal accepts only its
fixed Host and Origin, never places the secret in a cookie or URL, and sets
`Referrer-Policy: no-referrer` and `Cache-Control: no-store`.

HTTP rather than HTTPS is a documented bootstrap exception: a browser cannot
silently trust a device-generated certificate before enrollment. Confidentiality
comes from the per-session encrypted Wi-Fi link; authorization comes from the
screen secret. This does not meet the runtime LAN security profile and exposes no
playback/control API.

All setup paths begin `/ff/setup/v2`. Success JSON is closed and errors use RFC
9457 `application/problem+json` with the stable codes from section 6.3.

| HTTP operation | Exact request | Exact success response |
|---|---|---|
| `GET /status` | no body | exact `SetupStatus` defined below |
| `GET /networks` | no body | `networks`: array up to 50 of `ssid` string(1..32), `security`: `open\|wpa2\|wpa3\|wpa2_wpa3`, `signalPercent`: int[0..100] |
| `PUT /network` | `ssid`: UTF-8 string(1..32 bytes), `security` enum, `passphrase`: absent for open or 8..63 ASCII chars, `hidden`: boolean | 202: `attemptId`, `status: "testing"`, `statusUrl`, `expiresAt` |
| `GET /network-attempts/{attemptId}` | no body | `attemptId`, `status`: `testing\|connected\|internet_verified\|failed`, `failure`?: exact `AttemptFailure` below, `retryAfterSeconds`?: int[1..30] |
| `PUT /provisional-time` | `time`: RFC3339 timestamp; `source: "controller"` | `status: "provisional"`, `time` |
| `POST /updates/retry` | `operationId`: UUIDv7 from failed update | 202: same `operationId`, `status: "checking"`, `statusUrl` |
| `POST /support-bundles` | `supportBundleId`: caller-generated UUIDv7; `components`: unique subset from support command; `title`: string(1..120) | 202: `operationId`, same `supportBundleId`, `statusUrl` |
| `GET /support-bundles/{supportBundleId}` | no body | `status`: `collecting\|ready\|failed`; when ready, `downloadPath` on the same origin and `sha256` |
| `GET /support-bundles/{supportBundleId}/download` | no body | when ready, `application/zip` bytes whose digest equals the status `sha256` |
| `POST /factory-reset-requests` | `{}` | 202: `confirmationId`, `expiresAt`, `statusUrl` |
| `GET /factory-reset-requests/{confirmationId}` | no body | `status`: `pending\|approved\|rejected\|expired` |

`SetupStatus` is the closed object `deviceId`, `serialSuffix`, `setupEpoch`:
UUIDv7, `state`, `firmwareVersion`, `clockStatus`, optional `attempt`, and
optional `update`. `state` is
`awaiting_network|scanning|testing_network|online_unclaimed|online_claimed|recovery|updating|error`.

`attempt` is the closed object `attemptId`: UUIDv7, `ssid`, `status`:
`testing|connected|internet_verified|failed`, `startedAt`, `updatedAt`, and
optional `failure`. `AttemptFailure` is the closed object `code`:
`authentication|not_found|no_dhcp|no_internet|timeout|radio_error`,
`retryable`: boolean, and user-safe `detail`: string(1..256). A retry after
authentication failure is a new `PUT /network`, because the old passphrase was
erased.

`update` is the closed object `operationId`: UUIDv7, `status`:
`checking|downloading|verifying|installing|rebooting|succeeded|failed`,
`progressPercent`: int[0..100], `lastProgressAt`, `expectedReboot`: boolean,
optional `targetVersion`, and optional `failure`. Update failure is the closed
object `code`:
`check_failed|download_failed|signature_invalid|install_failed|reboot_timeout|rolled_back`,
`retryable`: boolean, and user-safe `detail`: string(1..256). Failure is required
only for failed. `POST /updates/retry` is allowed only for a matching failed,
retryable operation; otherwise it returns `conflict`. A retried operation keeps
its ID so cold-start clients do not create duplicate updates.

The portal polls no faster than once per second. `setupEpoch` persists across
planned Wi-Fi and update reboots until setup completes, so browser and native
clients can distinguish resume from a different/reset device. The setup secret
is TPM-sealed for that epoch, expires after 30 minutes, and is cleared on setup
completion or factory reset. If it cannot be resumed, the screen shows a new QR and epoch rather than
accepting an old secret.

A network is persisted only after DHCP succeeds and either internet/NTP is
verified or the screen user explicitly chooses LAN-only mode. A failed attempt
restarts or keeps the AP with a newly shown session as hardware permits; the
screen is the source of truth while the phone is disconnected. The passphrase
is write-only and is erased from portal memory after submission. Provisional
phone time only enables diagnostics and TLS bootstrap; NTP must replace it
before remote authentication.

Factory reset requires the same physical confirmation as runtime reset. Support
bundle download requires the live setup authorization header, expires after 15
minutes, and is allowed only after a screen action starts recovery support.
After internet is established, FF1 closes the setup server and displays an
`owner_enrollment` QR from the controller-authentication profile. The claimant
uses MQTT over WSS/443; FF1 never reuses the setup secret.

## 15. Standard use versus FF customization

| Area | Decision |
|---|---|
| MQTT request/response | Standard MQTT 5 Response Topic, Correlation Data, QoS 1, expiry, content type, and reason codes. No broker-specific RPC. |
| Device authentication | Standard TLS 1.3 mutual X.509; the private key is TPM-backed. TPM attestation is used at enrollment, not invented as an MQTT AUTH exchange. |
| Controller authentication | Invitation and enrollment-only connections use standard MQTT User Name and Password fields with restricted FF1-signed credentials. Access-session connections use standard MQTT 5 Enhanced Authentication and AUTH Control Packets; `FF1-JWT-ES256-PoP` and its JSON challenge/proof semantics are an FF customization. LAN uses standard mTLS. |
| Playlist | Exact DP-1 core v1.1.0 and capability-gated registered/draft DP-1 extensions. `playlist.display`, `displayAt`, the 2 MiB admission limit, and JSON pull-chunk retrieval are FF control-profile extensions outside DP-1. FF commands do not mutate or wrap signed DP-1 fields. |
| JSON/signatures/time/errors | JSON Schema 2020-12, JCS, SHA-256, and RFC 3339. Runtime errors reuse RFC 9457 members inside the common FF envelope; SoftAP uses top-level RFC 9457. |
| Discovery | mDNS/DNS-SD standard service discovery. |
| LAN push | Standard RFC 6455 handshake, Text Message, Ping/Pong, and Close behavior; FF custom `ff-control.v2` subscribe/result/resource/event JSON messages. |
| Keyboard | W3C UI Events code values and DOM-style down/up semantics. |
| Committed mobile text | FF `com.feralfile.input` DP-1 extension plus `input.text`, because W3C physical codes cannot represent IME/emoji text. |
| Topic/resource names | FF customization; broker-neutral and mechanically mapped to HTTPS reads/commands and LAN WebSocket subscriptions. |
| Command/state envelopes and revision epochs | FF customization needed for strict cross-transport parity, deduplication, and reconnect ordering. |
| Relative pointer batch | FF customization; DP-1 only standardizes whether click/scroll/drag/hover are permitted. |
| Controller enrollment and guest sessions | Standard MQTT 5 CONNECT/request-response and Enhanced Authentication plus JWT/JWS/JWE, RFC 7800 confirmation keys, and controller-key proof. FF customizes the Authentication Method data, invitation, scope, session-state, and ACL profile. The same one-time QR claim creates a persistent enrollment or a bounded guest session. |
| Persisted-time fallback | FF operational rule needed for constrained offline devices; it does not change certificate or token wire formats. |
| SoftAP portal | FF bootstrap exception because browser-first provisioning precedes trusted TLS and MQTT identity. |
