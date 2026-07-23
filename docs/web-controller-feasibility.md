# Web-Based Claiming and Control: Feasibility Analysis

This document evaluates whether the shipped SoftAP + LAN hub architecture blocks a future browser-based claiming and control surface, or whether it provides forward-compatible ground truth to build one later. The answer is **the architecture does not block it**; this analysis describes the constraints and preferred path.

**Scope:** This is a feasibility note, not a feature announcement or product spec. No web UI is being built or shipped in this branch. The intent is to prove that design decisions made now do not foreclose a future app-less web claim and control experience.

---

## (a) Web Claiming Viability

### Current contract

The shipped claiming flow uses QR codes:

```
https://link.feralfile.com/device_connect/<device_info>
```

where `device_info` is a pipe-delimited plaintext string (byte-identical to the
pre-merge format; built in `devicectl`):

```
<device_id>|<topic_id>|<internet true/false>|<branch>|<version>|pairing
```

(The LAN API version lives on the hub's status routes, not in the QR payload:
`GET /api/v2/status` — the pairing surface the app gates on — serves
`contract: "2"`; the legacy `GET /api/status` keeps `contract: "1"` for
transitional tooling.)

The mobile app parses this QR, extracts the topic ID, and binds it to the user's account via a cloud pairing call. The device rotates its topic ID on factory reset; unclaim revokes binding server-side.

### A feralfile.com claim page needs nothing else

A future HTTPS page at `https://feralfile.com/device/connect` can:

1. **Accept the device_info as a URL fragment:** Receive the same pipe-delimited `device_info` string in the URL fragment (e.g. `#<device_id>|<topic_id>|...`) and parse it client-side. The fragment stays in the browser; it is never sent to any server.

2. **Perform the same binding:** Call the same cloud pairing endpoint (with user auth token) to link the topic ID to the user's account. This is the same operation the mobile app already does.

3. **Handle first-claim-wins:** The cloud already enforces this. A topic ID can only be bound once; re-binding fails with a clear error. The page shows "This device is already claimed" or prompts unclaim before retry.

4. **Check relayer liveness before binding:** Before sending the binding request, the page should confirm the relayer actually sees the device's topic as online, and fail with "Device is offline or unreachable" otherwise. This prevents binding a dead or spoofed device. (The concrete liveness API is future design work — nothing shipped today provides it, and nothing shipped today blocks it.)

5. **Support unclaim and factory-reset:** The page can offer an "Unclaim Device" button that hits the cloud revoke endpoint (same as the mobile app). Factory-reset on the device rotates the topic ID automatically; the pairing is severed by that rotation, not by explicit revocation. The page shows status until the device reboots and advertises the new ID.

### Fragment-based privacy

By keeping the device info in the URL fragment, the claiming page never transmits `deviceId` or `topicId` to the server in HTTP headers, query strings, or request bodies. Log aggregation systems and proxies see only `https://feralfile.com/device/connect`, not the device identity. This matches the privacy posture of QR-based mobile claiming: the secret stays in the user's hands until they explicitly bind it.

---

## (b) Browser-Control Constraints and Viable Shapes

### The mixed-content problem

An HTTPS page at `https://feralfile.com/...` **cannot** make direct HTTP requests to `http://<device-ip>:1111` due to:

- **Mixed-content blocking** — browsers block HTTP requests from secure contexts
- **CORS** — even if we could send the request, a device on a private network has no HTTPS certificate, so CORS preflight fails
- **mDNS in browsers** — browsers cannot resolve `device-hostname.local` names; only app frameworks and operating systems can

This is a hard constraint. No workaround exists without external infrastructure.

### Viable future approaches (two shapes)

#### Option 1: Device-served control page (PREFERRED)

`feral-controld`'s hub listener (port 1111) serves a static control UI from `https://<device-ip>:1111/control/` using a self-signed or device-specific certificate.

**Pros:**
- Same-origin requests; no CORS or mixed-content issues
- Precedent: the captive portal is device-served
- No cloud round-trip for each command
- Works on networks without internet

**Cons:**
- Users must navigate to an untrusted certificate warning
- The page must know the device's private IP or mDNS name
- UX cost for device discovery and manual navigation

**Forward-compat provisions made now:**
- The hub middleware is a single chokepoint; a future permissive CORS flag can be added in one place (not yet implemented, but the architecture does not prevent it)
- The hub speaks the same command envelope as the relayer, so command routing logic is shared

#### Option 2: Cloud-relayed control

A secure page at `https://feralfile.com/device/<device-id>/control` sends commands to the cloud, which routes them to the device via the relayer WebSocket.

**Pros:**
- No device certificate or IP discovery friction
- Works from any network

**Cons:**
- Identical to today's mobile app control path; provides no new capability
- Not a win for app-less access if the relayer is still required
- Continues the cloud dependency

**Status:** This already works. No new implementation needed. Viable for future feature completeness, but not a step toward decentralization.

---

## (c) Deferred Backlog

No features from this section are assigned or scoped for immediate implementation. They represent the surface area of a future web claiming and control experience:

| Item | Purpose | Status |
|---|---|---|
| Redirect fallback | If QR/device-info link fails, redirect to feralfile.com/device/connect with optional parameter | Not started |
| Claim page + binding API | HTTPS page at feralfile.com accepting fragment-based device info and calling cloud pairing | Not started |
| Relayer liveness ping | Pre-binding call to confirm device is reachable via relayer before binding | Not started |
| Unclaim UI | Page to revoke a device from user's account | Not started |
| Device-list sync API | Cloud endpoint to fetch user's claimed devices with status (online, offline, version) | Not started |
| Device-served control page | Hub-hosted HTTPS control UI for commands, accessible at same-origin | Not started |

---

## Forward Compatibility with Relayer Retirement

The LAN hub and SoftAP provisioning chain are designed to coexist with the relayer, not immediately replace it.

The implemented firmware gate is the **versioned status route plus the mDNS TXT key**: new firmware serves `GET /api/v2/status` (`contract: "2"`) and advertises `api=2` on `_ff1._tcp`; old firmware 404s on the v2 route and lacks the TXT key, so the pairing app treats it as not LAN-pairable without shape-sniffing. The legacy `GET /api/status` (`contract: "1"`) remains for transitional tooling, and the same versioned-route mechanism is the escape hatch for the future LAN-authorization break: when auth turns on, clients detect capability by route/TXT version instead of hitting a silent hard break.

The open, unauthenticated LAN hub is release-scoped, with a declared v2 end state: **screen-anchored LAN pairing** — a short code shown on the device's display anchors trust, the device stores authorized controller keys, the owner reviews/revokes them on-device, and factory reset clears them. The hub's single shared middleware is the designated insertion point for that authorization layer; the `setupDisplay` overlay contract is namespace-extensible so the pairing-approval overlay can be added without breaking older players.

See [#3471](https://github.com/feral-file/ffos-user/issues/3471) (relayer retirement track) for the broader context and timeline.
