# API and Protocol Direction

This document defines the canonical API and protocol design direction for `ffos-user`.
Agents should treat these rules as stable constraints when adding, changing, or removing any interface.

---

## Version posture and API v2 transition

Unless explicitly marked v2, the registries and wire shapes below document the
currently deployed v1 interfaces. The proposed target is the
[FF1 communication API v2](ff1-v2-api-contract.md) and its
[controller-authentication profile](ff1-v2-controller-authentication.md); its
compatibility gates and coordinated removal sequence live in the
[migration plan](ff1-v2-migration.md). V2 remains a design draft, not a
second production contract.

For v2, `feral-controld` remains the only runtime external-control owner. It
initiates MQTT 5 connections and owns the LAN HTTPS/WebSocket adapter, while
focused protocol, state, and authentication packages implement the shared
contract without hiding command policy in transport code. `feral-setupd`
continues to own setup and recovery UX, including recovery SoftAP. Cross-service
setup and reset coordination uses an explicitly versioned D-Bus interface:
`feral-controld` owns external admission, confirmation records, broker cleanup,
identity rotation, controller-authority bootstrap, and protocol completion;
`feral-setupd` owns physical confirmation and durable local reset execution.

The v1 relayer envelope, Mint handoff, port-1111 Hub, and
`GetRelayerTopicID` remain unchanged only through the migration compatibility
gates. They are removed together from each successfully promoted v2 device
image and are never alternative v2 semantics. A rolled-back, below-minimum, or
current-v1 device keeps all four paths. The hosted relayer and other v1
infrastructure remain until the remaining legacy fleet passes its separate
infrastructure-retirement gate. The v2 `_ff1-control._tcp.local` lifecycle is
independent of the broker, internet, and `enableHub`: advertise only when a
LAN-usable interface and the complete TLS backend are ready; withdraw on
listener unavailability and before any of the three pending-reset lifecycles.
mDNS is discovery, never proof of identity or authority.

The FF OS deployment binding for v2 public TCP 443 is a system-level
`ff1-control.socket` plus hardened `systemd-socket-proxyd`, forwarding an
unmodified raw TCP stream from LAN-usable IPv4 and IPv6 addresses to
unprivileged `feral-controld` on loopback `127.0.0.1:8443`. `feral-controld`
owns TLS/mTLS and HTTP/WebSocket, runs neither as root nor with
`CAP_NET_BIND_SERVICE`, and advertises mDNS only after an end-to-end readiness
check. This least-privilege front end is deployment customization, not a new
protocol binding.

---

## D-Bus Naming and Versioning Conventions

### Bus name pattern

```
com.feralfile.<service>
```

Examples:
- `com.feralfile.controld`
- `com.feralfile.sysmonitord`
- `com.feralfile.watchdog`

### Object path pattern

```
/com/feralfile/<service>
```

Examples:
- `/com/feralfile/controld`
- `/com/feralfile/sysmonitord`

### Interface pattern

```
com.feralfile.<service>
```

Or, for logical grouping within a service:

```
com.feralfile.<service>.<category>
```

Example: `com.feralfile.controld.general` (controld's general-purpose RPC interface).

### Versioning

There is currently no version suffix in any D-Bus name. Adding a version suffix (e.g. `com.feralfile.controld.v2`) is the correct escape hatch if a breaking change cannot be avoided. Do not break existing names in place. If a version bump is needed, keep the old interface active until all callers are updated and deployed together.

### Complete interface registry

| Bus name | Object path | Interface | Type | Members |
|---|---|---|---|---|
| `com.feralfile.controld` | — | — | Bus name only (no exported RPCs currently) | — |
| `com.feralfile.sysmonitord` | `/com/feralfile/sysmonitord` | `com.feralfile.sysmonitord` | RPC | `GetConnectivityStatus(refresh bool) → (bool, error)`, `GetSysMetrics() → (*SysDBusMetrics, error)` |
| `com.feralfile.sysmonitord` | `/com/feralfile/sysmonitord` | `com.feralfile.sysmonitord` | Signal emitter | `sysmetrics`, `connectivity_change`, `sysevent` |
| `com.feralfile.watchdog` | — | — | Bus name only (no exported RPCs currently) | — |

Before the setupd merge, `feral-controld` exported a `GetRelayerTopicID` RPC and emitted `show_pairing_qr_code` / `factory_reset` / `system_update` / `upload_logs` / `upload_logs_with_bundle` signals to `feral-setupd` on its own bus. Those handlers are now in-process inside `feral-controld`, so it exports no RPCs and emits none of those signals; its `dbus` package holds only the inbound `com.feralfile.sysmonitord` constants it consumes.

---

## Request and Response Schema Rules

### D-Bus RPC

- Methods return typed Go values plus `*dbus.Error` as the final return value.
- A nil `dbus.Error` means success. A non-nil `dbus.Error` means failure; the error message is a human-readable string.
- Callers must treat an error response as a signal to retry, fall back, or log — not silently ignore.
- Boolean parameters that control behavior (e.g. `refresh bool` on `GetConnectivityStatus`) should be explicit positional args, not buried in a map.

### D-Bus Signals

Signals carry either:
- A single primitive value (e.g. `connectivity_change` carries a single `bool`)
- A JSON-serialized byte slice for structured data (e.g. `sysmetrics` carries `[]byte` which is a JSON-encoded metrics struct)

Do not add ad-hoc fields to signal bodies without updating all consumers. Prefer the byte-slice JSON pattern for structured payloads so the schema can evolve with additive fields.

### Current-v1 relayer WebSocket protocol

All messages are JSON. The message envelope is:

**Inbound (device receives from relayer):**
```json
{
  "messageID": "system" | "<arbitrary-id>",
  "message": {
    "command": "<command-type>",
    "request": { "<key>": <value> }
  }
}
```

- `messageID == "system"`: a system message. The `message.topicID` field, if present, must be saved to state; on the first (empty → non-empty) assignment the mediator also fires the topic observer that re-triggers the auto-claim flow. (The BLE-era `GetRelayerTopicID` RPC and its pending callers are retired — the topic is consumed in-process.)
- Any other `messageID`: a command message. Route to `commandrouter`.

**Outbound (device sends to relayer):**
```json
{
  "type": "<response-type>",
  "messageID": "<echoed-from-request>",
  "message": <any>
}
```

**Relayer keepalive control messages:**
- `controld` sends both a transport-level WebSocket `Ping` frame and an application-level `{"type":"ping"}` message on the relayer WebSocket.
- The relayer should reply to the transport ping with a WebSocket `Pong` frame and to the application ping with `{"type":"pong"}` once the new keepalive path is deployed.
- During rollout, either pong path may keep the connection alive so older relayer builds do not time out before the protocol upgrade lands.
- `pong` is handled internally by `relayer` and is not dispatched to `commandrouter` or command handlers.

**Command routing logic (inside controld):**
- If `Command.DeviceCtlCommand()` returns true → route to the device executor (`devicectl`).
- If `command == "startMintPairingSession"` → handle inside `feral-controld`
  as a commandrouter pre-CDP special case that creates or reuses the Mint
  Pairing Broker session and drives the player overlay through
  `mintPairingDisplay`.
- If `command == "mintPairingApprovalDecision"` → handle inside
  `feral-controld` as a commandrouter pre-CDP special case that validates and
  completes a pending browser-session approval request.
- Otherwise → route to Chromium via CDP (`Runtime.evaluate`).

**Device-control relayer commands**

The following command names are routed to `devicectl` and use the standard relayer/hub envelope (`command` plus `request`):

| Command | Request fields | Notes |
|---|---|---|
| `dragGesture` | `cursorOffsets` | Array of `{dx, dy}` step deltas. |
| `tapGesture` | `button` | `button` selects left, right, or middle; missing or empty defaults to left. |
| `doubleTapGesture` | `button` | Same button selection as `tapGesture`. |
| `longPressGesture` | `button` | Same button selection as `tapGesture`. |
| `clickAndDragGesture` | `cursorOffsets` | Press, move, then release. The executor treats release failure as an error because Chromium can remain pressed. Batches are capped at 16 offsets to keep a single request from monopolizing the executor. |
| `zoomGesture` | `scaleSteps` | Array of positive float scale factors. The executor dispatches non-Ctrl `mouseWheel` input at the current cursor anchor so Chromium does not apply browser/page zoom. |
| `setSleepSchedule` | `enabled`, optional `sleepTime`, `wakeTime` (HH:MM), optional `days` | Persists the FF1 sleep/wake window plus active weekdays and enables or disables automatic transitions. See the active-days contract below. |
| `sleepNow` | — | Manual override toward sleep until the next schedule boundary (when the schedule is enabled). |
| `wakeNow` | — | Manual override toward awake until the next schedule boundary (when the schedule is enabled). |

`devicectl` also exposes two device-control commands for panel control over DDC/CI via `ddcutil`: `ddcPanelControl` (set brightness, contrast, speaker volume, mute, or power using a single JSON request body that selects the action) and `ddcPanelStatus` (query the same VCPs and return a structured status object). Both share the standard relayer/hub envelope; detailed field shapes live alongside the executor in `devicectl/ddc.go`.

**Sleep schedule vs. FFP panel power (contract):** `setSleepSchedule`, `sleepNow`, and `wakeNow` apply **FF1 player sleep mode** over CDP **synchronously** for the purpose of command success: if the handler returns success, the player has been asked to enter or leave sleep mode on that request path. **FFP panel power** (DDC standby / on) is aligned **asynchronously** in a dedicated worker so slow or flaky `ddcutil` calls do not block relayer or hub deadlines. DDC failures are **best-effort** (logged; command success is still possible). Rapid sleep/wake transitions are **coalesced** so an older in-flight DDC call cannot overwrite a newer intended state. **`device_status.message.sleepSchedule`** (and the `sleepSchedule` object returned on these commands) reflects the **schedule and player sleep intent**; **DDC-derived fields** (for example from `ddcPanelStatus`) may **temporarily disagree** until alignment completes (**eventual consistency**). **`device_status` refresh** after a transition may run before DDC finishes, so consumers must not assume panel power and player sleep mode flip in the same notification. On **process exit**, `feral-controld` does **not** wait for queued or in-flight DDC alignment work.

**Sleep schedule active days (contract):** `setSleepSchedule` accepts an optional `days` array of lowercase three-letter weekday tokens (`sun`, `mon`, `tue`, `wed`, `thu`, `fri`, `sat`; matching is case- and whitespace-insensitive, duplicates are dropped, and the stored list is canonicalized Sunday-first). On days **in** the selection the panel follows the normal wake/sleep window; on days **not** in the selection the panel sleeps for the **entire civil day** (so deselecting Sat and Sun sleeps at Friday's sleep time and wakes at Monday's wake time). Field semantics on `setSleepSchedule`: **omitted or `null` `days` preserves the currently stored selection** (same preserve-when-absent rule as empty `sleepTime` / `wakeTime`); an **explicit empty array is rejected** as invalid arguments (a schedule with no awake day is not representable); a **full seven-day selection normalizes back to omitted**, which also means the pre-days record shape (no `days` key) is exactly the every-day schedule, so older apps and older firmware interoperate without migration (older firmware ignores the unknown key). **Manual overrides are day-blind:** `sleepNow` / `wakeNow` expire at the next clock occurrence of the opposite schedule boundary regardless of day selection — for example `wakeNow` on an unselected Saturday lasts until Saturday's sleep time, not until Monday — after which the day-aware schedule resumes.

Successful `setSleepSchedule`, `sleepNow`, and `wakeNow` responses include `{"ok": true, "sleepSchedule": { ... }}` where `sleepSchedule` matches `sleepschedule.Status` JSON: `enabled` (bool), `sleepTime` / `wakeTime` (HH:MM strings), optional `days` (canonicalized weekday tokens; omitted means every day), `currentState` (`awake` | `sleeping`), optional `overrideState`, optional `overrideUntil` / `nextTransitionAt` (RFC3339 timestamps when present). The same object shape appears under `device_status.message.sleepSchedule` when the schedule file is readable (omitted when the file is missing or unreadable without blocking status).

**Command type constants** are defined in `components/feral-controld/commands/types.go`. New remote commands must be added there with a corresponding entry in `deviceCtlCommands` if they require executor handling.

The `uploadLogs` command accepts `userId`, `apiKey`, and `title`, plus optional `supportBundleID` or `support_bundle_id` (the camelCase form wins when both are present). `feral-controld` performs the upload **in-process** (the feral-setupd `log_uploader.rs` flow ported into `devicectl/loguploader.go` — no D-Bus signal is emitted; the retired `upload_logs` / `upload_logs_with_bundle` signals no longer exist). The command ACKs as soon as the upload is scheduled; a detached worker then zips the device logs and submits them through the v2 pre-sign API (JSON pre-sign request returning a pre-signed S3 URL, then an `application/zip` PUT), bounded by a 10-minute budget and single-flighted so a duplicate command while an upload is running is ACKed and ignored. `userId`/`title` are validated for parity with the old contract but unused by the v2 API; upload failures are logged on-device, not surfaced to the caller.

The `startMintPairingSession` command is a controller-to-controld request to create one Mint Pairing Broker channel and display its pairing code through the player overlay via CDP command `mintPairingDisplay`. The command returns explicit `RPC` payloads with `ok`, `status`, `channelID`, `pairingCode`, and `expiresAt` on success; failures return `ok: false` with `error.code`. If a non-expired session is already active, it returns `already_started` and re-displays the same code. The broker short code is intentionally visible; raw browser session tokens are not. Any terminal pairing state hides the overlay so the bundled local player continues normal artwork playback. Shutdown cleanup is bounded within `feral-controld`'s process-level forced-exit window, so late terminal delivery is explicitly best-effort once that internal budget is exhausted.

The `mintPairingApprovalDecision` command is a controller-to-controld approval response for browser-session mint pairing. It is handled inside `feral-controld`, not forwarded to Chromium. Success and validation failures both return explicit `RPC` payloads with `ok`, `status` or `error.code`, and `approvalRequestID` where available. Raw browser session tokens and DP1 playlist content must never appear in this command or in `mint_pairing_approval_request` / `mint_pairing_approval_outcome` relayer messages.

**Outbound notifications (`feral-controld`):** The device periodically pushes status notifications over the relayer WebSocket and local hub clients with an envelope that includes `notification_type` and a structured `message`. Mint-pairing approval notifications are relayer-only because the controller/mobile approval UI is reached through the relayer topic, not through the trusted-local hub socket. At minimum:

- `player_status` — playback/UI state from Chromium via CDP `checkStatus` (cast command, playlist, pause, etc.). This is not a substitute for hardware or OS-level facts. It now includes a numeric `renderStatus` beside `index` so consumers can branch on stable render outcome codes: `0` pending, `1` loading, `2` ready, `3` failed. `renderStatus` is the authoritative artwork render outcome and should be forwarded unchanged by controller relays and notifications.
- `device_status` — device-oriented fields assembled by `status.DeviceStatus.GetStatus` (screen rotation, Wi‑Fi name, installed/latest version, volume, feature toggles, MAC info, best-effort `displayURL`, and optional `sleepSchedule`). The `displayURL` field is the top-level URL of the sole Chromium **page** debug target (DevTools `/json`), when exactly one such target exists; it is omitted when the URL cannot be resolved. Consumers that previously read a Chrome document URL from player payloads should use `device_status.message.displayURL` instead. When present, `sleepSchedule` follows the same **sleep vs. DDC** eventual-consistency rules as the `setSleepSchedule` / `sleepNow` / `wakeNow` contract above.
- `mint_pairing_approval_request` — browser-session mint request details sent to controller/mobile approval UI, including browser information and the E2EE challenge.
- `mint_pairing_approval_outcome` — terminal mint-pairing result used to clear controller/mobile approval UI.

### LAN hub HTTP surface (port 1111)

`feral-controld`'s `hub` package binds `0.0.0.0:1111`. The listener is up unconditionally when `enableHub` is set (default on) — it is the BLE-replacement LAN recovery channel, not an optional feature. Every route is registered through one shared middleware (`hub/middleware.go`) that applies an in-flight storm cap (HTTP `429` over `MAX_INFLIGHT_REQUESTS = 64`) and per-request logging. That middleware is the single chokepoint and the designated insertion point for the future LAN-authorization layer (#3471); today all routes are **unauthenticated**.

| Route | Method | Purpose |
|---|---|---|
| `/api/cast` | POST | Same JSON command envelope as the relayer (`command` + `request`); routed through the same `commandrouter`, including the pre-CDP mint-pairing commands. Non-POST → `405`. A per-command token-bucket gate inside `commandrouter` can additionally return `429`. |
| `/api/status` | GET | LEGACY device/setup status JSON (below), `contract: "1"`. Kept for transitional tooling; not the pairing surface. Non-GET → `405`. |
| `/api/v2/status` | GET | The LAN **pairing** surface: identical payload, `contract: "2"`. The versioned route is the firmware gate — old firmware 404s here (and advertises no `api` mDNS TXT key), which is how the app tells LAN-pairable devices from old ones. Non-GET → `405`. |
| `/api/notification` | GET → WS | Upgrades to a WebSocket that streams the same outbound notifications the relayer receives. Non-GET → `405`. |
| `/metrics` | GET | Prometheus text exposition of playback metrics. |

The hub does not carry `messageID == "system"` messages; topic assignment is relayer-only.

**`GET /api/status` / `GET /api/v2/status` body** (`hub/status.go`; identical shape, only `contract` differs):

```json
{
  "device_id": "...",
  "version": "...",
  "branch": "...",
  "contract": "1",
  "claimed": true,
  "internet": true,
  "setup_state": "online",
  "connectivity": "...",
  "topic_id": "..."
}
```

- `contract` is owned by the hub (not the status provider): `"1"` on the legacy route, `"2"` on `/api/v2/status`. The versioned route — not the field — is the firmware gate the pairing app uses: a device that 404s on `/api/v2/status` (or lacks the `api` mDNS TXT key) is old firmware and must be treated as **not LAN-pairable** (no discovery notification, no pairing offer). The field remains the dual-running-window signal for retiring the open `:1111` surface.
- `setup_state` is a coarse provisioning-state string. When the provisioning machine is wired in (production), it is the live machine state: `starting`, `online`, `offline_retrying`, `unprovisioned`, `ap_active`, `joining`. The bare status provider falls back to `claimed` / `unclaimed`.
- `claimed` mirrors the mDNS TXT `claimed` value.
- `branch` and `version` are read from the same `ff1-config.json` the OTA gate uses, so the LAN payload can never disagree with the claim QR.
- `internet` is live internet reachability (sys-monitord's cached signal, one local D-Bus round-trip per poll). It is distinct from `connectivity`, which is LAN-link state: a device on a healthy LAN with a dead WAN is `connectivity: "connected"`, `internet: false`.

**Claim-QR parity.** The `device_connect` claim QR encodes `device_id|topic_id|internet|branch|version|setup_phase`. Every segment is recoverable from this endpoint, so a LAN client that discovered the device over mDNS needs nothing the QR has: `device_id` → `device_id`, `topic_id` → `topic_id` (served claimed or not: FF1 is multi-controller, so additional phones — and a replacement phone after the original is lost — pair over LAN without a QR; LAN-presence is the authorization boundary, matching the BLE-era posture), `internet` → `internet`, `branch` → `branch`, `version` → `version`, and the QR's constant `pairing` phase → derivable as `claimed == false` with a non-empty `topic_id` (`setup_state` + `claimed` carry strictly more information).

**mDNS advertisement** (`mdns` package): service type `_ff1._tcp` in `local.`, port `1111`. TXT keys: `id`, `name`, `claimed` (always published, even when `false`, so resolvers can rely on the key's presence), and `api` (always published; value mirrors the v2 status contract, currently `api=2`). `api` is the discovery-time firmware gate: the pairing app requires it before treating a discovered device as pairable, so old firmware — whose records lack the key — never triggers a pairing notification, without a per-device HTTP probe. Discoverability is link-keyed (advertised whenever there is any network link, torn down when the link drops), and a claim-state flip triggers a Stop+Start re-registration so the TXT `claimed` value is refreshed.

---

## SoftAP provisioning and captive portal

First-run and offline-recovery Wi-Fi provisioning is done over a NetworkManager hotspot plus a device-served captive portal. There is no BLE/GATT surface. Ownership is split across `softap` (raise/lower the AP), `portal` (HTTP), `provisioning` (state machine), and `wifictl` (nmcli scan/join).

### The access point

- **SSID:** `FF1-<device_id>` (constant prefix `FF1-`, device id read from `/etc/hostname`).
- **PSK (WPA2):** a deterministic **8-digit numeric code** derived from the device id: the first 4 bytes of `SHA-256(device_id)` reduced modulo 10⁸ and zero-padded to exactly 8 digits (WPA2's minimum key length). Digits-only because users type it on a phone keyboard while reading it off the TV; deterministic so the same device always advertises the same key. Same convenience-level security posture as the earlier id-derived scheme — the on-screen QR carries the credentials, and the AP is short-lived and offline.
- **Backend:** NetworkManager shared mode via `nmcli device wifi hotspot con-name ff1-softap ssid <SSID> password <PSK>`. NM runs the DHCP/NAT/gateway itself; the gateway IP is NM's shared-mode default and is not set by our code. The `softap.Backend` interface is the A/B containment boundary (NM hotspot vs. standalone hostapd+dnsmasq); flipping it touches only that package.

### Captive portal HTTP endpoints

The portal binds `:80` (permitted by the system-wide `net.ipv4.ip_unprivileged_port_start=80` sysctl shipped in the `ffos` image, since `feral-controld` runs as a `systemd --user` service where `CAP_NET_BIND_SERVICE` is inert).

| Route | Method | Behavior |
|---|---|---|
| `/` | GET/POST | Renders the network-picker page (SSID list from the pre-AP scan cache; falls back to a manual SSID field on scan failure). |
| `/connect` | POST | Credential submit. Parses form fields `ssid` and `password` and calls the provisioning machine's join. On acceptance renders a "connecting, reconnect if this drops" page; on outright rejection re-renders the picker. Non-POST → `303` to `/`. |
| `/status` | GET | JSON `{ "state", "ssid?", "reason?", "message?" }` where `state` ∈ `idle` / `joining` / `succeeded` / `failed`. Sourced from the provisioning machine so it survives a portal restart across the AP bounce. `Cache-Control: no-store`. |
| `/rescan` | GET | Plain-HTML confirmation page (`rescan_confirm.html`) warning that the setup Wi-Fi will restart and the QR must be re-scanned. A page rather than `window.confirm()` because captive-portal mini-browsers (iOS CNA, Android sign-in sheet) suppress JS dialogs. Viewing it does not bounce the AP. |
| `/rescan` | POST | Performs the bounce: the machine tears the AP down, runs a fresh station-mode scan, and re-raises — disconnecting the phone; the response page (sent before the bounce lands) tells the user to re-scan the QR code to reconnect. Renders `rescan.html` on acceptance, re-renders the picker on rejection. Other methods → `303` to `/`. |
| OS probe paths | GET | `/generate_204`, `/gen_204`, `/hotspot-detect.html`, `/library/test/success.html`, `/connecttest.txt`, `/ncsi.txt` all `302` to `/`. Any other unmatched non-root path is also redirected, covering unenumerated probe variants. |

Captive detection is a three-layer design split across the `ffos` image and this portal:

- **DNS layer (image-shipped, `ffos` repo):** the hotspot's dedicated dnsmasq instance resolves every name to `192.0.2.1` (`address=/#/192.0.2.1` in `archiso-ff1/airootfs/etc/NetworkManager/dnsmasq-shared.d/captive.conf`, plus an explicit `address=/ff1.config/192.0.2.1` pin for the canonical name). The answer is a public-looking RFC 5737 TEST-NET address, NOT the hotspot gateway: Samsung One UI's NetworkStack refuses captive detection when probe hostnames resolve to private IPs ("DNS response to the URL is private IP", verified on a Galaxy S23 Ultra), so answering with the gateway's RFC 1918 address would break the sign-in prompt on Samsung phones. The canonical **human-facing** portal address is `http://ff1.config` — it rides the catch-all like any other name, and it is what the frame's setup screen (ff-player `SetupOverlay`) and the portal pages tell users to type; the raw `192.0.2.1` form still works but is no longer surfaced, and the old `10.42.0.1` form is retired. The name is only resolvable on the isolated hotspot; keep it in sync across `captive.conf`, `SetupOverlay`, and the portal templates.
- **NAT layer (image-shipped, `ffos` repo):** `/etc/nftables.conf` redirects client traffic to `192.0.2.1:80` back to the local portal, and `192.0.2.1:443` to a closed local port so HTTPS probes fail fast with a RST instead of hanging. The fast `:443` RST also makes browsers' HTTPS-first upgrade of a typed `ff1.config` fall back to HTTP promptly. The NAT rule matches only the destination IP, so `ff1.config` needs no dedicated rule.
- **HTTP layer (this service):** once the redirected probe lands on the routes above, the `302`-on-probe (rather than returning the 204/success body each OS expects) is what makes the phone conclude it is behind a captive portal and auto-open the page.

The DNS and NAT layers only make the probe request arrive; the HTTP layer is what makes it look like a captive portal.

### AP trigger state machine (`provisioning`)

Machine states: `online`, `offline_retrying`, `unprovisioned`, `ap_active`, `joining`. The AP is raised or suppressed from connectivity and link signals:

- **Unprovisioned (no saved Wi-Fi profile) + offline + confirmed no link → raise the AP immediately at the boot assessment only** (a fresh device with no saved Wi-Fi and no ethernet needs the AP right away). Every later confirmed link loss — the online→offline edge (a LAN-switch reboot must not flash setup over artwork), the tick probe on a parked device (a cable unplug emits no connectivity event), or a redundant offline re-emission (a `sys-monitord` restart re-emits its first probe unconditionally) — gets the full continuous-confirmed-absence window, since a raised `ap_active` has no link-based exit. With no link guard wired the immediate raise keeps its original scope (nothing can confirm absence over time).
- **Provisioned + offline → arm a sustained link-loss window** (`defaultOfflineWindow = 5m`, probed on a `15s` tick); any tick that sees a link — or gets an inconclusive probe — disarms the window, and the clock restarts at the next confirmed absence, so the AP is raised only after a full window of **continuous, confirmed link absence** (not merely "offline at expiry"), and a brief router reboot never pops the AP. The window is armed by the first confirmed-absent probe, not by the offline reading that preceded it (with no link guard wired it keeps the original "5m from the offline event" baseline).
- **A redundant offline reading in `ap_active` keeps the AP up** while the AP is actually raised: the hotspot holds the radio, so "offline" is the definition of that state, not news; both trigger branches reconcile and stay put rather than tearing the portal down under a phone mid-setup. A *failed* raise (`ap_active`, hotspot not up) is the exception — a confirmed link-present reading, from an assessment or a tick probe, exits back to `offline_retrying`/`unprovisioned` so a late successful retry never drops a link that recovered while NM was refusing the raise.
- **Any live local link suppresses the AP** — wired (ethernet) or an associated Wi-Fi station — even while reported offline. The AP raises on **link loss, not internet loss**: broken credentials and vanished SSIDs present as link *down*, while up-but-offline means a dead upstream the AP cannot fix — and raising it would drop the station link on the single radio (#233). The device's own setup hotspot never counts as a link (`status.LinkChecker.ExternalLink` excludes the `ff1-softap` profile by name, covering leftovers from a failed teardown), and a failed `nmcli` probe reads as *unknown*, which defers the AP rather than authorizing it.
- **Any transition back online tears the AP down.**
- **Join sequencing (the "AP bounce"):** on credential submit the machine tears the AP down *before* the station-mode join (the single radio cannot host the AP and join at once), then joins via `wifictl`. On **any** join failure (including wrong password) the AP is re-raised so the user can retry; the portal `/status` reports `failed` with a reason.

`wifictl` wraps `nmcli` for saved-profile enumeration, scanning (with a pre-AP scan cache, TTL 10m, because NM serializes Wi-Fi operations on the single radio), and joining. Join errors are classified as auth / SSID-not-found / timeout / unknown and mapped to portal messages.

### Claim QR (`device_connect` URL)

After provisioning and the mandatory pre-claim OTA gate pass, `feral-controld` paints the claim QR through the `setupui` `setupDisplay` contract. The encoded URL is:

```
https://link.feralfile.com/device_connect/<device_id>|<topic_id>|<internet>|<branch>|<version>|<setup_phase>
```

- The payload is a **pipe-delimited string**, not base64/JSON. It is kept byte-identical to the string the former setupd/launcher path produced so the phone parser cannot tell the difference.
- `branch` is URL-safe encoded (`/` → `%2F`); nothing else is encoded.
- `internet` is the literal `"true"` / `"false"` (it is `"true"` here, since reaching the claim QR means the pre-claim live version check just succeeded).
- `setup_phase` is the literal `pairing` at this point (the one surviving `setup_phase`-shaped value; the durable setupd phase machine was not ported).

This string is a contract with the mobile app: field order and the `|` separator are fixed; the sixth field is an additive extension. Do not remove or reorder existing fields without a coordinated mobile-app release.

**Claim QR lifecycle (`showPairingQRCode`).** `show=true` runs the mandatory pre-claim OTA gate (`EnsureLatestBeforeClaim`) and only paints the claim QR on `no-update-needed`; if an update starts, the device is too old, or the version check fails, the QR is withheld. `show=false` (cloud ended pairing) records the `ready` narration state **before** hiding the overlay, so a durable "pairing succeeded" transition is never lost if the hide is interrupted.

---

## Current-v1 backward-compatibility posture

These rules preserve every device running the current-v1 or rollback image.
They do not require permanent v1/v2 dual semantics in a successfully promoted
v2 device image; device-image removal and hosted-infrastructure retirement
follow the separate explicit gates above and in the migration plan.

1. **Additive changes are always safe.** Add new D-Bus methods, new JSON fields, new portal/hub fields, or new relayer command types without breaking existing callers.
2. **Never rename or remove existing methods or fields** without a version bump or a coordinated multi-service release that updates all callers simultaneously.
3. **Never change D-Bus signal payload shapes** (member name, body types) without updating all subscribers in the same PR.
4. **Portal and hub wire surfaces** (captive-portal form fields `ssid`/`password`, `/status` JSON, `GET /api/status` fields, mDNS TXT keys) are contracts with phones, LAN clients, and resolvers. Change them additively; the `contract` field on `/api/status` is the escape hatch for a breaking change to the `:1111` surface.
5. **Relayer command field names** (`command`, `request`) are shared with the web app layer and potentially the mobile app. Do not rename them without coordinating with all consumers (the `FIXME` comments in `commands/types.go` acknowledge this debt).
6. **Claim QR `device_info` string** is parsed by the mobile app. Field order and separator (`|`) are fixed.

---

## Error Payload Conventions

### D-Bus errors

Use `dbus.NewError(message, []interface{}{})` for all D-Bus method errors. The first argument is a human-readable error message. The second is an empty slice (no additional error body values). Do not put structured data in the error body.

### Relayer errors

Most command failures are not standardized: when an executor command fails, `controld` logs the error and does not send an explicit error response to the relayer unless the command protocol requires a reply. When adding new commands that need error responses, document the response shape in code comments near the command handler.

**Command-storm rejection (standardized).** Command-storm protection (see below) is the one path with a defined controller-visible error envelope. When the command router rejects a command (rate limit or concurrency budget) or the relayer sheds a command under dispatch saturation, the controller receives an RPC response whose `message` body is:

```json
{
  "error": "rate_limited",
  "command": "displayPlaylist",
  "message": "human-readable reason"
}
```

The command-router rejection reply (rate limit / concurrency budget) is reliable. The relayer-side shed reply under **dispatch saturation** is **best-effort**: to avoid blocking its read loop under a sustained storm, the relayer drops the reply when its shed-response writers are all busy. Controllers must not rely on receiving it for that case and should fall back to a request timeout and retry.

The LAN-hub ingress reports the same condition with HTTP `429 Too Many Requests`. Controllers should treat both as "device busy" and back off; the command was not applied.

### Command-storm protection

`feral-controld` protects the shared command path from flooding across both the relayer and LAN-hub ingress (see feral-file/ffos-user#208). High-cost or disruptive commands are rate-limited, deduped, and bounded by a global concurrency budget; internal lifecycle flows (e.g. OOM recovery) bypass the gate so client traffic cannot shed them.

It is on by default with tuned defaults. The optional `commandStorm` config section tunes it:

```json
{
  "commandStorm": {
    "disabled": false,
    "maxConcurrent": 16
  }
}
```

- `disabled` (default `false`) — turn the gate off entirely.
- `maxConcurrent` (default `16`, used when `> 0`) — global in-flight command budget. A command's internal weight is clamped to this budget, so setting it below a heavy command's weight throttles that command (it reserves the whole budget while in flight) rather than rejecting it forever.

### Provisioning join errors

There is no BLE error-code table. Join failures surface through the captive portal instead: `wifictl` classifies an `nmcli` join failure as auth (wrong password), SSID-not-found, timeout, or unknown, and the provisioning machine renders the corresponding reason on the portal `/status` response (`state: "failed"`, plus `reason` / `message`) while re-raising the AP for a retry.

---

## Timeout and Retry Expectations Across Service Boundaries

### D-Bus call timeouts

| Caller | Callee | Method | Timeout |
|---|---|---|---|
| `feral-controld` | `feral-sys-monitord` | `GetConnectivityStatus` | 7 seconds |

RPCs that timeout should log the error and either fail the calling operation or fall back to a cached/default value. Do not silently swallow D-Bus timeouts. With the setupd merge, controld no longer waits on a peer daemon at startup or issues cross-process `GetRelayerTopicID` calls; the topic is read directly from local state.

### Relayer connection

`feral-controld` retries the relayer WebSocket connection with exponential back-off. The relayer connection is conditional on reachability ONLY (`GetConnectivityStatus` returning true) — never on a persisted `TopicID`. Connecting with an **empty** topic is the designed topic-assignment path: the connect URL omits the `topicID` parameter and the server answers with a `MESSAGE_ID_SYSTEM` carrying the assigned topic, which the mediator persists. Gating on a non-empty topic would deadlock a factory-fresh device that boots already online (no `connectivity_change` edge ever fires on it). When the device is offline, `controld` waits for the `connectivity_change` D-Bus signal before attempting to connect.

### OTA gate (`otagate`)

The OTA gate is single-flight across its entry points via one `singleflight` key (`"ota"`): concurrent callers **coalesce** onto the one in-flight update and share its result rather than being rejected. The entry points are:

- `RequestUpdate` — the user-triggered `updateToLatestVersion` command, mode `Available` (update to any newer version).
- `EnsureLatestBeforeClaim` — the mandatory pre-claim gate, mode `Required` (update only if a mandatory/minimum version demands it).
- `EnsureLatestAtStartup` — the boot-time mandatory check for **settled (claimed)** devices, mode `Required`, restoring the setupd-era "Required-mode check on every boot with internet" for the Ready phase. Triggered by the provisioning notifier's WAN-confirmed transitions (`StateOnline`, or `StateUnprovisioned` with reason `unprovisioned`), it runs once per process; a `VersionCheckFailed` outcome retries with the auto-claim backoff bounded at 8 attempts, after which the nightly updater timer owns the update. The trigger is wired **only when controld started within the two-minute kernel boot window** (`feral-controld.service` is `Restart=always`, so an ungated hook would let a mid-exhibition daemon crash-restart spring a required update and reboot on a healthy playing device). Its guard predicate is `claimSettled()` — the exact complement of the pre-claim gate's early return — so for any device state exactly one of the two online-triggered flows owns the boot gate. Independently of that trigger-wiring gate, the gate re-checks its OWN, WIDER `startupOTAGateEntryWindow` (30 minutes) at entry, every time the hook fires — deliberately wider than the two-minute wiring window above, because WAN routinely trails boot by several minutes on a site-wide power restore, exactly the boot this gate most needs to cover. Checked once, at entry only, not on every retry: a gate that started inside the 30-minute window may keep retrying a failing version check (bounded at ~22.5 minutes across `startupOTAGateMaxCheckAttempts`) well past it — boot-time DNS convergence is the common cause, and per-retry re-probing would defeat that rationale.

Updates are **always driven locally** now — the gate starts the updater systemd unit on-device and tails its log. There is no remote/BLE-triggered update path, and the setupd `setup_phase` machine and `pre_failure_phase` persistence were deliberately not ported; the gate tracks only in-memory `Mode`/`Result` enums and a permanent-failure latch.

Two retry ladders:

- **Version-check ladder:** up to 3 attempts, fixed 2-second wait between them, each attempt capped at a 10-second per-request timeout so an unstable connection fails fast with a classified network error instead of hanging. A failed version check returns `ResultVersionCheckFailed` and **does not latch**.
- **Update-spawn ladder:** up to `MaxUpdateRetries = 3` attempts; a transient failure before the final attempt backs off `2^attempt` seconds (2s then 4s) and retries. A permanent failure — or a transient failure on the final attempt — **latches** the in-memory permanent-failure state and fires the `OnPermanentFailure` callback. An explicit retry clears the latch. Transient vs. permanent is decided by exact-string matching against the `ffos` updater script messages (`classifyUpdaterMessage`), so that companion script output must stay aligned.

The setupd one-attempt BLE refresh variant (`RefreshRetries::Single`) is intentionally absent — there is no BLE response deadline to protect.

**Narrator policy on `OnPermanentFailure`** (all three entry points share one gate and one callback): decided at emit time by reading the LIVE claim state, not the state at flight start — a settled-device `updateToLatestVersion` call that joins an in-flight update another (pre-claim) caller started still gets the settled policy. Claim not settled: today's behavior, the pairing flow's `join_failed` narration. Claim settled: there is no "join" to fail, so the callback instead hides a stuck `updating` overlay (`HideIfShowing`) and logs — it never repaints `join_failed` over a claimed device. A separate post-ladder watchdog (`Deps.OnUpdateSucceededNoReboot`, default 5 minutes) hides a stuck `updating` overlay if a successful ladder's expected reboot never happens.

---

## Protocol Invariants Agents Must Not Break

1. The relayer `messageID == "system"` path is the canonical source of the device's `TopicID`. Do not add a second path that sets `TopicID` without going through this flow.
2. The `TopicID` is read from local state (`controld.state`); it is no longer fetched via a cross-process `GetRelayerTopicID` D-Bus RPC. Do not reintroduce a peer-daemon dependency for the topic.
3. `sysmetrics` signal body is a JSON-encoded byte slice. Consumers unmarshal it into the metrics struct. Adding fields to the struct is safe; removing or renaming fields is a breaking change.
   - `gpu.gpu_busy` is the driver-reported utilization field and should be preferred by app consumers when they need a direct busy percentage.
   - `gpu.current_frequency / gpu.max_frequency` remains available as a clock-ratio fallback, but it is not a substitute for actual utilization.
4. `connectivity_change` signal body is a single `bool`. It must stay a single `bool`. If more data is needed, add a new signal rather than replacing this one.
5. The claim QR `device_info` string is a single pipe-delimited field list kept byte-identical to the pre-merge format. Do not reorder or re-encode it without a coordinated mobile-app release.
6. The LAN hub `POST /api/cast` accepts exactly the same command envelope as the relayer, and both paths share `commandrouter`. Do not diverge them without explicit justification.
