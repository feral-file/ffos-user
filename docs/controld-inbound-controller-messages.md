# feral-controld Inbound Controller Messages

This document describes inbound messages from `ff-controller` clients to
`feral-controld`, their current payloads, current response behavior, and
mint-pairing messages for ephemeral browser-session minting.

`ff-controller` clients can reach `feral-controld` through the remote
`ff-relayer` WebSocket. Local hub clients use the same command envelope over
`POST /api/cast` on port `1111`, but this document focuses on the
controller-to-controld contract. When enabled, the local hub is a
trusted-local-network control surface and routes through the same
`commandrouter` as relayer commands, including mint-pairing commands.

## Current Message Envelope

Inbound relayer command:

```json
{
  "messageID": "controller-message-id",
  "message": {
    "command": "getDeviceStatus",
    "request": {}
  }
}
```

Outbound success response when a command returns a result:

```json
{
  "type": "RPC",
  "messageID": "controller-message-id",
  "message": {
    "ok": true
  }
}
```

System messages are not controller commands. `messageID: "system"` is reserved
for relayer topic assignment:

```json
{
  "messageID": "system",
  "message": {
    "topicID": "topic_ff1_abc123"
  }
}
```

## Current Routing

`feral-controld` routes inbound commands by `message.command`:

- Device-control commands are handled by the `devicectl` executor.
- `displayPlaylist` is resolved through DP1 first, then forwarded to Chromium
  through CDP as `window.handleCDPRequest(...)`. Controld defaults missing CDP
  `intent.action` to `now_display` so the player accepts the cast. When the
  playlist contains item-level `displayAt` values, controld filters them to the
  current active set before CDP, commits only the refreshable source identity to
  durable cache after the player accepts that filtered cast, keeps the resolved
  full playlist in memory, and advances the player on the next `displayAt`
  (timer), sleep-schedule wake, or CDP reconnect with a force cast
  (`intent.action=now_display`, not `refresh: true`) so cutover is not deferred
  until the current artwork duration ends. URL / dynamic playlist refresh still
  uses `refresh: true`, except the first scheduled reconstruction after a
  controld restart force-casts because scheduler ownership may need to be
  restored from persisted state. Playlists without item-level `displayAt` are
  otherwise forwarded unchanged.
- `startMintPairingSession` and `mintPairingApprovalDecision` are handled by
  `feral-controld` as commandrouter pre-CDP special cases.
- `downloadPlaylistItem`, `downloadPlaylist`, `clearPlaylistItemCache`,
  `clearPlaylistCache`, and `getOfflineCacheStatus` are likewise handled by
  `feral-controld` as commandrouter pre-CDP special cases, owned by the
  `offlinecache` package (see "Offline Artwork Caching Inbound Messages"
  below).
- `refreshArtwork` clears Chromium cache, then forwards to Chromium through
  CDP.
- Any other non-device command is forwarded to Chromium through CDP.

Current relayer error behavior is important: if command processing fails,
`feral-controld` logs and returns an internal handler error. It does not
send a standardized RPC error response over the relayer for most failures, so
new inbound message families that require controller-visible errors must define
their own response shape.

The one standardized exception is **command-storm rejection**. When the device
sheds a command to protect itself from flooding (rate limit, concurrency
budget, or relayer dispatch saturation — see feral-file/ffos-user#208), it
sends an RPC response whose `message` body is:

```json
{
  "error": "rate_limited",
  "command": "displayPlaylist",
  "message": "human-readable reason"
}
```

The command-router rejection reply (rate limit / concurrency budget) is
reliable. The relayer-side shed reply under **dispatch saturation** is
**best-effort**: to keep its read loop responsive under a sustained storm, the
relayer drops the reply when its shed-response writers are all busy. Controllers
must not depend on receiving it in that case and should fall back to a request
timeout and retry.

The LAN-hub ingress reports the same condition as HTTP `429`. Controllers
should treat this as "device busy", back off, and retry; the command was not
applied. Control-plane messages (e.g. the `system`/topic-state message above)
are never shed by command pressure.

## Shared Success Responses

Most side-effect commands return:

```json
{
  "ok": true
}
```

Commands with command-specific responses are documented below. If a command
returns `nil`, `feral-controld` sends no relayer RPC response.

## Current Command Registry

### connect

Purpose: record the connected controller client in local `feral-controld` state.

Example:

```json
{
  "messageID": "msg-connect-1",
  "message": {
    "command": "connect",
    "request": {
      "clientDevice": {
        "device_id": "ios-device-123",
        "device_name": "Alice iPhone",
        "platform": 1
      },
      "primaryAddress": "192.168.1.50"
    }
  }
}
```

Current success response:

```json
{
  "type": "RPC",
  "messageID": "msg-connect-1",
  "message": {
    "ok": true
  }
}
```

Current error cases:

- Invalid JSON shape under `request` causes command failure.
- State persistence failure causes command failure.
- `primaryAddress` is accepted but not currently used by the executor.

Current relayer error response: none standardized; command failure is logged.

### showPairingQRCode

Purpose: show or hide the setup pairing (claim) QR code. Handled in-process by `feral-controld`: `show=true` runs the mandatory pre-claim OTA gate and, on no-update-needed, paints the claim QR through the `setupDisplay` narration contract; `show=false` records the `ready` state before hiding the overlay.

Example:

```json
{
  "messageID": "msg-pairing-1",
  "message": {
    "command": "showPairingQRCode",
    "request": {
      "show": true
    }
  }
}
```

Current success response: `{"ok": true}`.

Current error cases:

- Invalid `request.show` shape causes command failure.

Current relayer error response: none standardized; command failure is logged.

### getDeviceStatus

Purpose: return device-oriented status for controller UI.

Example:

```json
{
  "messageID": "msg-status-1",
  "message": {
    "command": "getDeviceStatus",
    "request": {}
  }
}
```

Current success response example:

```json
{
  "type": "RPC",
  "messageID": "msg-status-1",
  "message": {
    "contract": "2",
    "screenRotation": "normal",
    "connectedWifi": "Studio WiFi",
    "installedVersion": "1.2.3",
    "latestVersion": "1.2.4",
    "analyticsDisabled": false,
    "betaFeaturesEnabled": true,
    "macInfo": {
      "eth0": "00:11:22:33:44:55"
    },
    "volume": 75,
    "isMuted": false,
    "displayURL": "http://127.0.0.1:8080/"
  }
}
```

`contract` is always `"2"` on this firmware (equal to the hub's
`/api/v2/status` contract — one firmware gate, two transports): it lets the
app identify a v2 frame over the relayer when mDNS is unavailable. Its
PRESENCE is the capability
signal — old firmware's reply simply lacks the key.

The reply additionally carries the additive `network` health object — the
same diagnosis the on-screen
narration shows, also served on the hub status routes:

```json
"network": {
  "state": "offline_retrying",
  "reason": "joined-no-internet",
  "ssid": "Studio WiFi",
  "link": "wifi",
  "internet": false
}
```

(`deferred` is `omitempty`: it appears only while true.)

`link` ∈ `wifi`/`ethernet`/`none`/`unknown` from the machine's cached probe
evidence (a status poll never runs a probe); `deferred` tells the app its own
control-plane contact is holding a pending setup-mode raise down, so it should
surface the pairing/`startWifiSetup` action instead of waiting.

Current error cases:

- Status collection dependencies may fail; unavailable fields are usually
  omitted when best-effort collection can continue.
- A hard status collection error causes command failure.

Current relayer error response: none standardized; command failure is logged.

### startWifiSetup

Purpose: put the frame into its existing SoftAP setup mode on the app's
request, so a user can re-configure Wi-Fi.
Reachable over the relayer and the LAN hub `POST /api/cast` like every
device-control command. Ships with the initial v2 release, so the v2 gate
(mDNS TXT `api=2` + `/api/v2/status` → `contract:"2"`, or the relayer
`contract` above) is the capability gate.

Example:

```json
{
  "messageID": "msg-wifisetup-1",
  "message": {
    "command": "startWifiSetup",
    "request": {}
  }
}
```

Current success response example (produced before the raise is queued, so it
normally wins the race by a wide margin — the raise is seconds of `nmcli` work
— but it is NOT synchronized against the transport, and raising the AP severs
the station link that carries the reply, so the app must treat a send timeout
as success):

```json
{
  "type": "RPC",
  "messageID": "msg-wifisetup-1",
  "message": { "ok": true, "ssid": "FF1-8EVTK3RE" }
}
```

Rejections are normal replies, not transport errors:

```json
{ "ok": false, "code": "wired_link_active", "message": "…" }
```

| Code | When |
|---|---|
| `wired_link_active` | a live ethernet link, or the wire probe errored or is unavailable (fail closed) |
| `busy` | the provisioning machine is in `joining` or `starting` |
| `unavailable` | the provisioning seam is not wired (test/partial builds) |

Acceptance queues the raise on the provisioning loop: the standard entry
sequence runs there, and the session is bounded by the `user-requested` row of
the AP session policy (30 minutes, portal-activity deferral, 2h cap — see
`setup-flow.md`). Everything after the raise is the unchanged out-of-box flow.

### deviceMetrics

Purpose: return the last system metrics JSON received from
`feral-sys-monitord`.

Example:

```json
{
  "messageID": "msg-metrics-1",
  "message": {
    "command": "deviceMetrics",
    "request": {}
  }
}
```

Current success response: the latest metrics object, or `null`/empty if no
metrics have been received yet.

Current error cases:

- Stored metrics JSON cannot be unmarshaled.

Current relayer error response: none standardized; command failure is logged.

### displayPlaylist

Purpose: display a DP1 playlist on the FF1 player.

When forwarding to Chromium, controld sets `intent.action=now_display` on the
CDP payload if the controller request did not already include an intent action.
The player rejects `displayPlaylist` without a known DP1 action
(`Unknown DP1 action: undefined` → `ok: false`). Soft refresh remains on the
5-minute URL/dynamic refresher path (`refresh: true`), which does not use this
cast default.

When the resolved playlist contains item-level `displayAt` values, controld
computes an active set
(`max(displayAt <= now)` items plus items without `displayAt`) and sends only
that filtered playlist to Chromium. Timezone-less `displayAt` values use device
local time; values with `Z`/offset are absolute. Date-only (`YYYY-MM-DD`) is
rejected per DP-1 §3.5.2 (not evergreen). If no playlist item has `displayAt`,
controld forwards the full playlist unchanged. Controld keeps the resolved full
playlist in memory while casting, persists only its refreshable source identity
after the player accepts the filtered cast, and uses the in-memory playlist to
arm the next `displayAt` transition and to recompute after wake or CDP
reconnect. After a controld-only restart, the refresher must fetch the source
again before scheduler cutovers resume; if that fetch fails, controld leaves the
current player artwork alone and retries later. Static inline player status
contains only the filtered active set and no stable full-playlist identity, so
controld does not restart-resume a persisted static inline schedule from player
status alone.
Initial casts and timed / wake / reconnect pushes are force casts
(`intent.action=now_display`
without `refresh`) so the player applies the playlist immediately even if the
current artwork still has remaining duration; the 5-minute URL/dynamic
playlist-refresher path continues to use `refresh: true`. The legacy
`displayDefaultPlaylist` command is forwarded to the player as a player-owned
fallback and does not clear controld's displayAt cache; with
`onlyIfNoPlaylist`, a successful response may mean the player no-opped because
content was already playing.

Playlist URL example:

```json
{
  "messageID": "msg-display-1",
  "message": {
    "command": "displayPlaylist",
    "request": {
      "playlistUrl": "https://gallery.example/dp1/feed.json"
    }
  }
}
```

Inline DP1 example:

```json
{
  "messageID": "msg-display-2",
  "message": {
    "command": "displayPlaylist",
    "request": {
      "dp1_call": {
        "items": [
          {
            "id": "work-1",
            "title": "Work 1",
            "source": "https://cdn.example/work-1.mp4",
            "duration": 300
          }
        ]
      }
    }
  }
}
```

Dynamic DP1 example:

```json
{
  "messageID": "msg-display-3",
  "message": {
    "command": "displayPlaylist",
    "request": {
      "dp1_call": {
        "items": [],
        "dynamicQuery": {
          "profile": "graphql-v1",
          "endpoint": "https://api.example/graphql",
          "query": "query { items { id title source } }",
          "responseMapping": {
            "itemsPath": "data.items",
            "itemSchema": "dp1/1.0"
          }
        }
      }
    }
  }
}
```

Current success response: Chromium/player response from
`window.handleCDPRequest(...)`, commonly:

```json
{
  "type": "RPC",
  "messageID": "msg-display-1",
  "message": {
    "message": {
      "ok": true
    }
  }
}
```

If offline caching is enabled and `playlistUrl` was previously used with
`downloadPlaylist` for this exact URL, a live DP1 fetch/processing
failure (e.g. no network) falls back to that downloaded copy instead of
failing the command — see `offline-artwork-capture.md` §6. This is a
"last known good" copy: it will not reflect anything republished at that
URL since it was downloaded.

If offline caching is enabled, this command also switches the kiosk's
live offline-replay `Fetch`-interception scope to the newly-requested
playlist's cached items *before* the CDP send below (so replay is ready
before the player starts requesting the new playlist's resources — see
`offline-artwork-capture.md` §6). If the CDP send then fails, or the
player rejects the command (`ok:false`), that scope switch is reverted:
the command re-queries whatever the player actually reports as currently
displayed and re-syncs replay scope to that, since the kiosk never
actually switched away from it. This revert is itself best-effort and
does not change the error/response shape below in any way.

Current error cases:

- Missing both `playlistUrl` and `dp1_call`: command failure with
  `unknown payload type`.
- `playlistUrl` is not a non-empty string.
- DP1 URL fetch or processing fails, and no cached fallback exists for
  that URL (never downloaded, offline caching disabled, or since
  cleared).
- `dp1_call` is not an object.
- Inline DP1 cannot be marshaled/unmarshaled.
- Dynamic query resolution fails.
- CDP send to Chromium fails.
- Player response is not `{"message":{"ok":true}}`; this records playback
  failure metrics but the raw player response is still returned if CDP
  succeeded.

Current relayer error response: none standardized for processing failures;
command failure is logged.

### displayDefaultPlaylist

Purpose: tell the player to resume or display its default playlist. This is
forwarded to Chromium through CDP as a legacy player-owned fallback. It does
not clear controld's cached `displayAt` playlist because a successful player
response does not prove that default playback replaced the current playlist.

Example:

```json
{
  "messageID": "msg-default-playlist-1",
  "message": {
    "command": "displayDefaultPlaylist",
    "request": {}
  }
}
```

Current success response: Chromium/player response.

Current error cases:

- Command JSON marshal failure.
- CDP send failure.

Current relayer error response: none standardized; command failure is logged.

### refreshArtwork

Purpose: clear Chromium browser cache, then forward an artwork refresh command
to the player.

Example:

```json
{
  "messageID": "msg-refresh-1",
  "message": {
    "command": "refreshArtwork",
    "request": {}
  }
}
```

Current success response: Chromium/player response — or, when the player page
is unresponsive, a synthesized `{"message": {"ok": true, "recovered": "navigate"}}`
(see below).

Current error cases:

- Cache clear failure is logged as a warning and does not stop the command.
- CDP page-evaluate failure does NOT fail the command: controld falls back to
  the `playersession.Session` recovery primitive
  (`NavigateHomeInline({PurgeCache: true})`) — navigate-to-entry, not
  reload-in-place: the static export is flat files only, so a client-route
  reload 404s. A refresh is most needed exactly when the page is broken (e.g.
  Chromium serving stale cached chunks after a player bundle swap), so cache
  clear + navigate is the complete recovery, and it runs through the same
  sleep/error-page/overlay gates every recovery navigation does. The
  synthesized success response carries `recovered: "navigate"` so controllers
  can distinguish it from a normal player reply.
- Only when the navigate escalation also fails (dead CDP connection, or no
  session wired) does the command fail, surfacing the original evaluate
  error.

Current relayer error response: none standardized; command failure is logged.

### sendKeyboardEvent

Purpose: dispatch a keyboard event to Chromium.

Example:

```json
{
  "messageID": "msg-keyboard-1",
  "message": {
    "command": "sendKeyboardEvent",
    "request": {
      "code": 13
    }
  }
}
```

Current success response: `{"ok": true}`.

Current error cases:

- Invalid `request.code` shape.
- Unsupported key code. Supported values are printable ASCII `32` through
  `126`, plus `Tab` `9`, `Enter` `13`, `Escape` `27`, and `Backspace` `8`.
- CDP key-down failure causes command failure.
- CDP key-up failure is logged but does not fail the command.

Current relayer error response: none standardized; command failure is logged.

### dragGesture

Purpose: move the on-screen cursor using relative offsets, then dispatch the
final mouse move to Chromium.

Example:

```json
{
  "messageID": "msg-drag-1",
  "message": {
    "command": "dragGesture",
    "request": {
      "messageID": "cursor-ui-correlation-id",
      "cursorOffsets": [
        { "dx": 10, "dy": -5 },
        { "dx": 15, "dy": 2 }
      ]
    }
  }
}
```

Current success response: `{"ok": true}`.

Current error cases:

- Invalid `cursorOffsets` shape.
- Failure to marshal cursor positions for the player UI.
- CDP failure while updating cursor UI.
- CDP failure while dispatching final mouse move.

Current relayer error response: none standardized; command failure is logged.

### tapGesture

Purpose: dispatch a mouse click at the current tracked cursor position.

Example:

```json
{
  "messageID": "msg-tap-1",
  "message": {
    "command": "tapGesture",
    "request": {}
  }
}
```

Current success response: `{"ok": true}`.

Current error cases:

- CDP mouse press failure.
- CDP mouse release failure.

Current relayer error response: none standardized; command failure is logged.

### rotate

Purpose: rotate the display orientation.

Example:

```json
{
  "messageID": "msg-rotate-1",
  "message": {
    "command": "rotate",
    "request": {
      "clockwise": true
    }
  }
}
```

Current success response:

```json
{
  "type": "RPC",
  "messageID": "msg-rotate-1",
  "message": {
    "orientation": "portrait"
  }
}
```

Possible `orientation` values are `landscape`, `portrait`,
`landscapeReverse`, and `portraitReverse`.

Current error cases:

- Invalid `request.clockwise` shape.
- `wlr-randr` query fails.
- Active output cannot be found.
- Rotation command fails.
- Saving orientation can fail; this is logged as a warning and does not fail the
  command.

Current relayer error response: none standardized; command failure is logged.

### setVolume

Purpose: set device audio volume from a controller-visible percentage.

Example:

```json
{
  "messageID": "msg-volume-1",
  "message": {
    "command": "setVolume",
    "request": {
      "percent": 80
    }
  }
}
```

Current success response: `{"ok": true}`.

Current error cases:

- Invalid `request.percent` shape.
- `percent` outside `0..100`.
- `pamixer --set-volume` fails.
- Persisting saved volume can fail; this is logged as a warning and does not
  fail the command.

Current relayer error response: none standardized; command failure is logged.

### toggleMute

Purpose: toggle device audio mute state.

Example:

```json
{
  "messageID": "msg-mute-1",
  "message": {
    "command": "toggleMute",
    "request": {}
  }
}
```

Current success response: `{"ok": true}`.

Current error cases:

- `pamixer --toggle-mute` fails.

Current relayer error response: none standardized; command failure is logged.

### analyticsToggle

Purpose: enable or disable analytics collection by updating the local sentinel
file.

Example:

```json
{
  "messageID": "msg-analytics-1",
  "message": {
    "command": "analyticsToggle",
    "request": {
      "enabled": false
    }
  }
}
```

Current success response: `{"ok": true}`.

Current error cases:

- Invalid `request.enabled` shape.
- State directory creation fails.
- Sentinel file write/remove fails.

Current relayer error response: none standardized; command failure is logged.

### betaFeaturesToggle

Purpose: enable or disable beta features by updating the local sentinel file.

Example:

```json
{
  "messageID": "msg-beta-1",
  "message": {
    "command": "betaFeaturesToggle",
    "request": {
      "enabled": true
    }
  }
}
```

Current success response: `{"ok": true}`.

Current error cases:

- Invalid `request.enabled` shape.
- State directory creation fails.
- Sentinel file write/remove fails.

Current relayer error response: none standardized; command failure is logged.

### sshAccess

Purpose: enable or disable temporary SSH access.

Enable example:

```json
{
  "messageID": "msg-ssh-1",
  "message": {
    "command": "sshAccess",
    "request": {
      "enabled": true,
      "publicKey": "ssh-ed25519 AAAAC3Nza... alice@example",
      "ttlSeconds": 3600
    }
  }
}
```

Disable example:

```json
{
  "messageID": "msg-ssh-2",
  "message": {
    "command": "sshAccess",
    "request": {
      "enabled": false
    }
  }
}
```

Current success response:

```json
{
  "type": "RPC",
  "messageID": "msg-ssh-1",
  "message": {
    "enabled": true,
    "ttlSeconds": 3600,
    "expiresAt": "2026-06-16T04:00:00Z"
  }
}
```

Disable response:

```json
{
  "type": "RPC",
  "messageID": "msg-ssh-2",
  "message": {
    "enabled": false
  }
}
```

Current error cases:

- Invalid request shape.
- `publicKey` missing when enabling SSH.
- Authorized key write fails.
- `systemctl start sshd.service` fails; `feral-controld` attempts rollback.
- Scheduling the disable timer fails; `feral-controld` attempts rollback.
- Disabling can fail while clearing timer, stopping SSH, or removing
  authorized keys.

Current relayer error response: none standardized; command failure is logged.

### updateToLatestVersion

Purpose: run a system update. Handled in-process by `feral-controld`'s OTA gate (`otagate.RequestUpdate`, mode `Available`), which narrates progress and drives the local updater.

Example:

```json
{
  "messageID": "msg-update-1",
  "message": {
    "command": "updateToLatestVersion",
    "request": {}
  }
}
```

Current success response: `{"ok": true}`.

Current error cases:

- Starting or running the local updater fails.

Current relayer error response: none standardized; command failure is logged.

### factoryReset

Purpose: execute factory reset. Handled in-process by `feral-controld`: it clears the persisted relayer topic, narrates `factory_reset`, and starts `set-factory-boot.service` (which stages a one-shot boot into the pristine factory snapshot and reboots, abandoning the running subvolume).

Example:

```json
{
  "messageID": "msg-reset-1",
  "message": {
    "command": "factoryReset",
    "request": {}
  }
}
```

Current success response: `{"ok": true}`.

Current error cases:

- Starting `set-factory-boot.service` fails.

Current relayer error response: none standardized; command failure is logged.

### uploadLogs

Purpose: upload device logs. Handled in-process by `feral-controld` and
**fire-and-forget** (see [`api-design.md`](api-design.md)): the command
validates the request, schedules the upload, and ACKs immediately; a detached
worker then zips the device logs and submits them (JSON pre-sign request to the
FF1 log-submissions endpoint returning a pre-signed S3 URL, then a PUT of the
zip), bounded by a 10-minute budget and single-flighted — a duplicate command
while an upload is running is ACKed and ignored. The optional support bundle id
is included in the submission request.

Example:

```json
{
  "messageID": "msg-logs-1",
  "message": {
    "command": "uploadLogs",
    "request": {
      "userId": "user_123",
      "apiKey": "redacted-api-key",
      "title": "Living room issue",
      "supportBundleID": "sb_123"
    }
  }
}
```

Current success response: `{"ok": true}`.

Current error cases (returned to the caller — only pre-schedule validation can
fail the command):

- Invalid request shape.
- Missing `userId`, `apiKey`, or `title`.

Failures after the ACK (zipping, obtaining the presigned URL, or uploading the
zip) are logged on-device by the detached worker and are **not** surfaced to
the caller.

Current relayer error response: none standardized; command failure is logged.

Security note: `apiKey` is currently part of the inbound payload. Logs should
continue to truncate command payloads and should avoid exposing full secrets.

### shutdown

Purpose: shut down the device.

Example:

```json
{
  "messageID": "msg-shutdown-1",
  "message": {
    "command": "shutdown",
    "request": {}
  }
}
```

Current success response: `{"ok": true}` if the command returns before shutdown
takes effect.

Current error cases:

- `sudo shutdown -h now` fails.

Current relayer error response: none standardized; command failure is logged.

### reboot

Purpose: reboot the device.

Example:

```json
{
  "messageID": "msg-reboot-1",
  "message": {
    "command": "reboot",
    "request": {}
  }
}
```

Current success response: `{"ok": true}` if the command returns before reboot
takes effect.

Current error cases:

- `sudo reboot -h now` fails.

Current relayer error response: none standardized; command failure is logged.

### ddcPanelControl

Purpose: control attached panel settings through DDC/CI.

Examples:

```json
{
  "messageID": "msg-ddc-1",
  "message": {
    "command": "ddcPanelControl",
    "request": {
      "action": "brightness",
      "value": 65
    }
  }
}
```

```json
{
  "messageID": "msg-ddc-2",
  "message": {
    "command": "ddcPanelControl",
    "request": {
      "action": "mute",
      "value": "on"
    }
  }
}
```

Supported actions:

- `brightness`: integer `0..100`
- `contrast`: integer `0..100`
- `volume`: integer `0..100`
- `mute`: string `on` or `off`
- `power`: string `standby`, `off`, or `on`

Current success response: `{"ok": true}`.

Current error cases:

- Invalid request shape.
- Unknown `action`.
- Missing `value`.
- Value type/range mismatch for the action.
- `ddcutil` control failure. Some display-not-found failures trigger a detect
  and retry path before failing.

Current relayer error response: none standardized; command failure is logged.

### ddcPanelStatus

Purpose: read attached panel settings through DDC/CI.

Example:

```json
{
  "messageID": "msg-ddc-status-1",
  "message": {
    "command": "ddcPanelStatus",
    "request": {}
  }
}
```

Current success response:

```json
{
  "type": "RPC",
  "messageID": "msg-ddc-status-1",
  "message": {
    "brightness": 65,
    "contrast": 70,
    "volume": 40,
    "mute": "off",
    "power": "on",
    "monitor": "DEL 4098",
    "errors": {
      "contrast": "VCP read failed"
    }
  }
}
```

Current error cases:

- Total DDC status collection failure.
- Partial read failures can be returned in the `errors` map while other fields
  are still present.
- Some display-not-found or empty-output failures trigger a detect and retry
  path.

Current relayer error response: none standardized; command failure is logged.

### Unknown or Empty Commands

Empty command:

```json
{
  "messageID": "msg-empty-1",
  "message": {
    "request": {}
  }
}
```

Current behavior: logged warning, no command result, no relayer RPC response.

Unknown command:

```json
{
  "messageID": "msg-unknown-1",
  "message": {
    "command": "someUnknownCommand",
    "request": {}
  }
}
```

Current behavior: forwarded to Chromium/CDP unless the command is added to the
device-control map. If Chromium rejects or CDP fails, command failure is logged
without a standardized relayer error response.

## Mint-Pairing Inbound Messages

The mint-pairing flow adds an approval decision message from
`ff-controller` to `feral-controld`. The surrounding flow is:

1. Browser requester sends encrypted `mint_request` to `feral-controld` through
   the Mint Pairing Broker.
2. `feral-controld` decrypts the request and sends an approval request to
   controller clients through `ff-relayer`.
3. A controller client sends `mintPairingApprovalDecision` inbound to
   `feral-controld`.
4. `feral-controld` accepts exactly one valid decision.
5. On approval, `feral-controld` creates an ephemeral browser session through
   `ff-relayer` and sends the raw token only inside encrypted
   `mint_succeeded` to the browser.
6. On rejection or terminal failure, `feral-controld` sends encrypted
   `mint_rejected` to the browser.

`ff-controller` must not receive raw browser session tokens or DP1 playlist
content.

Implementation note: `feral-controld` embeds the temporary Go minter client from
`ff-art-computer-handoff` for Mint Pairing Broker channels, encrypted browser
requests, and encrypted browser results. Relayer approval dispatch and
`POST /api/ephemeral-sessions?topicID=...` session creation are owned by
`feral-controld`; the minter library does not know relayer API keys, approval
transport, or token minting policy. Runtime support is opt-in through
`mintPairing.enabled` in `feral-controld` config and starts only after a
controller sends `startMintPairingSession`.

### startMintPairingSession

Purpose: create one Mint Pairing Broker channel, display the broker pairing
code on the Art Computer QR screen, and wait for the browser requester to
connect through the broker.

Example:

```json
{
  "messageID": "msg-start-mint-pairing-1",
  "message": {
    "command": "startMintPairingSession",
    "request": {}
  }
}
```

Success response:

```json
{
  "type": "RPC",
  "messageID": "msg-start-mint-pairing-1",
  "message": {
    "ok": true,
    "status": "started",
    "channelID": "ch_pQ9Yab...",
    "pairingCode": "PAIR-123",
    "expiresAt": "2026-06-16T03:05:00Z"
  }
}
```

If a non-expired pairing session is already active and still waiting for a
browser request, `status` is `already_started` and `feral-controld` re-displays
the same pairing code. If a browser request is already pending controller
approval, `status` is `pending_approval`; `feral-controld` re-displays the
request-received overlay instead of showing the QR/code again.

Error response:

```json
{
  "type": "RPC",
  "messageID": "msg-start-mint-pairing-1",
  "message": {
    "ok": false,
    "error": {
      "code": "topic_not_ready",
      "message": "relayer topic is not ready",
      "retryable": true
    }
  }
}
```

Error cases:

- `disabled`: `mintPairing.enabled` is false.
- `invalid_config`: broker base URL is missing, or mint pairing is enabled
  without a valid player contract manifest.
- `topic_not_ready`: device has no current relayer topic ID.
- `broker_unavailable`: broker channel creation failed before a pairing code
  was available.
- `broker_response_invalid`: broker did not return a pairing code.
- `display_unavailable`: Chromium/CDP did not accept the player overlay
  display command.

On success, `feral-controld` sends CDP command `mintPairingDisplay` to the
bundled player with `state: "pairing_code"` and `pairingCode`. The player
renders a QR overlay above active artwork playback and shows the same code in
large text for long-distance readability. When a browser sends a mint request,
`feral-controld` updates the overlay to `state: "request_received"` with the
browser name. After an approve decision, it updates the overlay to
`state: "creating_token"` before creating and returning the ephemeral token.
The deployed player must accept `mintPairingDisplay` requests with states
`pairing_code`, `request_received`, `creating_token`, and `hidden`, and must
return an application response equivalent to `{"ok": true}` through
`Runtime.evaluate` when it accepts the display update. When
`mintPairing.enabled` is true, `feral-controld` validates
`/opt/feral/feral-player/ffos-player-contract.json` before creating a broker
channel or sending the first display command. Deployments that enable mint
pairing should also set `FF_PLAYER_REQUIRE_MINT_PAIRING_CONTRACT=1` for
`feral-player.service`; in that mode the player service validates the same
manifest before reporting readiness. Legacy/default player boot does not require
the manifest so older static bundles can still start when mint pairing is
disabled.

When the mint-pairing attempt reaches a terminal state, `feral-controld`
hides the overlay with `state: "hidden"` so normal artwork playback remains on
the same player page. This hide is attempted after success, controller
rejection, approval expiry, controller/service cancellation, and terminal
failure. During process shutdown, terminal broker/relayer delivery and display
cleanup are bounded so mint-pairing cleanup fits within `feral-controld`'s
two-second forced-exit guard; if a terminal delivery exceeds that budget, it is
logged and treated as best-effort.

### Outbound Approval Request

This is included for context because it creates the pending inbound decision.

Direction: `feral-controld` -> `ff-relayer` -> `ff-controller`.

```json
{
  "type": "notification",
  "notification_type": "mint_pairing_approval_request",
  "messageID": "mpa_01JZ6Y9M7S0H9G9ER4T52Q70W8",
  "persist_record_count": 10,
  "message": {
    "v": 1,
    "topicID": "topic_ff1_abc123",
    "approvalRequestID": "mpa_01JZ6Y9M7S0H9G9ER4T52Q70W8",
    "channelID": "ch_pQ9Yab...",
    "requestMessageID": "msg_2WaF8D7xV9zJvdm8SK5LSA",
    "origin": "https://gallery.example",
    "browserInfo": {
      "name": "Chrome",
      "userAgent": "Mozilla/5.0 ...",
      "label": "Living room laptop"
    },
    "requestedExpiresInSeconds": 86400,
    "effectiveExpiresInSeconds": 86400,
    "requestedAt": "2026-06-16T03:00:00Z",
    "expiresAt": "2026-06-16T03:05:00Z",
    "challenge": {
      "algorithm": "P256-HKDF-SHA256-AES-256-GCM",
      "browserPublicKeyFingerprint": "sha256-base64url...",
      "minterPublicKeyFingerprint": "sha256-base64url..."
    }
  }
}
```

`requestedExpiresInSeconds` is the browser-supplied session lifetime request.
`effectiveExpiresInSeconds` is the actual session lifetime `feral-controld`
will request from `ff-relayer` if the controller approves. `feral-controld`
owns this policy: omitted or non-positive requests default to 3600 seconds,
requests below 90 seconds are raised to 90 seconds, and requests above 86400
seconds are capped at 86400 seconds.

### mintPairingApprovalDecision

Purpose: approve or reject one pending browser-session mint request.

Approve example:

```json
{
  "messageID": "mpa_01JZ6Y9M7S0H9G9ER4T52Q70W8",
  "message": {
    "command": "mintPairingApprovalDecision",
    "request": {
      "v": 1,
      "approvalRequestID": "mpa_01JZ6Y9M7S0H9G9ER4T52Q70W8",
      "topicID": "topic_ff1_abc123",
      "channelID": "ch_pQ9Yab...",
      "requestMessageID": "msg_2WaF8D7xV9zJvdm8SK5LSA",
      "decision": "approve",
      "decidedAt": "2026-06-16T03:00:20Z",
      "controller": {
        "clientID": "ios_abc123",
        "platform": "ios"
      }
    }
  }
}
```

Reject example:

```json
{
  "messageID": "mpa_01JZ6Y9M7S0H9G9ER4T52Q70W8",
  "message": {
    "command": "mintPairingApprovalDecision",
    "request": {
      "v": 1,
      "approvalRequestID": "mpa_01JZ6Y9M7S0H9G9ER4T52Q70W8",
      "topicID": "topic_ff1_abc123",
      "channelID": "ch_pQ9Yab...",
      "requestMessageID": "msg_2WaF8D7xV9zJvdm8SK5LSA",
      "decision": "reject",
      "reason": "rejected_by_user",
      "retryable": true,
      "decidedAt": "2026-06-16T03:00:20Z"
    }
  }
}
```

Required fields:

- `v`: `1`
- `approvalRequestID`
- `topicID`
- `channelID`
- `requestMessageID`
- `decision`: `approve` or `reject`

Optional fields:

- `reason`: required for `reject`, ignored for `approve`
- `retryable`: meaningful for `reject`, default `false`
- `decidedAt`
- `controller`

Success response:

```json
{
  "type": "RPC",
  "messageID": "mpa_01JZ6Y9M7S0H9G9ER4T52Q70W8",
  "message": {
    "ok": true,
    "status": "accepted",
    "approvalRequestID": "mpa_01JZ6Y9M7S0H9G9ER4T52Q70W8"
  }
}
```

Duplicate success response for a replay of the same accepted decision:

```json
{
  "type": "RPC",
  "messageID": "mpa_01JZ6Y9M7S0H9G9ER4T52Q70W8",
  "message": {
    "ok": true,
    "status": "already_accepted",
    "approvalRequestID": "mpa_01JZ6Y9M7S0H9G9ER4T52Q70W8"
  }
}
```

Error response:

```json
{
  "type": "RPC",
  "messageID": "mpa_01JZ6Y9M7S0H9G9ER4T52Q70W8",
  "message": {
    "ok": false,
    "approvalRequestID": "mpa_01JZ6Y9M7S0H9G9ER4T52Q70W8",
    "error": {
      "code": "topic_mismatch",
      "message": "approval decision does not match this device topic",
      "retryable": false
    }
  }
}
```

Error cases:

| Case | Detection | controld response to controller | Browser result |
|---|---|---|---|
| Malformed decision payload | Missing required fields, invalid `decision`, non-object `request` | `ok: false`, `invalid_request`, `retryable: false` | Keep waiting until approval timeout |
| Unknown approval request | No pending request for `approvalRequestID` | `ok: false`, `not_found`, `retryable: false` | No change |
| Topic mismatch | Decision `topicID` differs from current device topic | `ok: false`, `topic_mismatch`, `retryable: false` | Keep waiting until timeout |
| Channel/request mismatch | `channelID` or `requestMessageID` differs from pending request | `ok: false`, `request_mismatch`, `retryable: false` | Keep waiting until timeout |
| Expired decision | Request deadline passed before valid decision | `ok: false`, `expired`, `retryable: false` | Encrypted `mint_rejected` with `approval_expired` |
| Duplicate same decision | Same accepted decision delivered again | `ok: true`, `status: "already_accepted"` | No duplicate minting |
| Conflicting duplicate decision | Different terminal decision after one was accepted | `ok: false`, `already_decided`, `retryable: false` | No change |
| Controller rejects | Valid `decision: "reject"` | `ok: true`, `status: "accepted"` | Encrypted `mint_rejected` with controller reason or `rejected_by_user` |
| Topic changes after approval | Current device topic no longer matches the approval request topic before relayer session creation or browser delivery | Optional outcome `failed`; ACK remains accepted | Encrypted `mint_rejected` with `topic_changed` |
| Session creation fails after approval | `ff-relayer` ephemeral-session creation fails | Optional outcome `failed`; ACK remains accepted | Encrypted `mint_rejected` with `session_create_failed` |
| Broker response send fails | Encrypted browser response cannot be delivered | Optional outcome `failed`; ACK remains accepted | Browser times out or observes broker terminal state |

Recommended mint-pairing error codes:

- `invalid_request`
- `not_found`
- `topic_mismatch`
- `request_mismatch`
- `expired`
- `already_decided`
- `session_create_failed`
- `browser_delivery_failed`

### Optional mint_pairing_approval_outcome

Purpose: clear pending approval UI on all controller clients after terminal
processing.

Direction: `feral-controld` -> `ff-relayer` -> `ff-controller`.

```json
{
  "type": "notification",
  "notification_type": "mint_pairing_approval_outcome",
  "messageID": "mpa_01JZ6Y9M7S0H9G9ER4T52Q70W8",
  "persist_record_count": 10,
  "message": {
    "v": 1,
    "approvalRequestID": "mpa_01JZ6Y9M7S0H9G9ER4T52Q70W8",
    "channelID": "ch_pQ9Yab...",
    "requestMessageID": "msg_2WaF8D7xV9zJvdm8SK5LSA",
    "status": "completed",
    "completedAt": "2026-06-16T03:00:22Z"
  }
}
```

Allowed `status` values:

- `completed`
- `rejected`
- `failed`
- `expired`
- `cancelled`

The outcome must not include the browser session token.

## Offline Artwork Caching Inbound Messages

`feral-controld` can download a DP-1 playlist item into a local cache so
`ff-player` can play it back without internet access — a software
(HTML/JS) item via a headless-Chromium capture, or any other single-file
mime type (image/video/audio/SVG/`model/gltf`/PDF/unrecognized) via a
browser-free direct HTTP download; see `docs/offline-artwork-capture.md`
§1/§3.3. This is a `commandrouter`-owned, pre-CDP command family (same
precedent as mint-pairing): these commands never reach
Manifest-based streaming sources — HLS (`.m3u8`) and DASH (`.mpd` /
`application/dash+xml`) alike — are the one class rejected outright; see
`classify.go`'s `ClassStreaming`. Both families must be excluded, not
just HLS: a manifest points at segments fetched progressively during
playback rather than a single fixed byte sequence, so a manifest that
fell through to the single-file download path would cache only the
manifest itself with `coverageComplete: true`, then fail every segment
request offline under the default fail-closed miss policy while status
still reported the item as fully cached.

The subsystem is opt-in through `offlineCache.enabled` in `feral-controld`
config. When disabled (or the config is absent), every command below returns:

```json
{
  "ok": false,
  "error": {
    "code": "disabled",
    "message": "offline caching is not enabled",
    "retryable": false
  }
}
```

`downloadPlaylistItem`/`downloadPlaylist` also return this same `disabled`
shape (message: `"offline cache: service is not started"`) if the offline
cache's background worker never started successfully — `feral-controld`
treats a `Service.Start` failure (e.g. an unreadable cache root) as
best-effort and keeps running rather than crashing, so this guards against
silently queuing a download nothing will ever process. As with the
feature-disabled case, this is not retryable by the client; it clears only
on a daemon restart (or after the underlying startup failure is fixed).

All five commands use the explicit RPC ok/error shape from
["Response Shape Recommendation for New Inbound Commands"](#response-shape-recommendation-for-new-inbound-commands)
below. Downloads are asynchronous: the command ACKs `queued` immediately and
per-item progress arrives later over the `offline_cache_status` notification.

Common error codes across this command family:

- `disabled`: offline caching is not enabled.
- `invalid_request`: a required field (`itemId`, `playlistId`) is missing,
  neither `playlistUrl` nor `dp1_call` was supplied, or — for
  `getOfflineCacheStatus` only — an argument is the wrong type or out of
  range (see that command's own section).
- `resolve_failed`: DP1 playlist resolution failed (bad URL, fetch failure,
  malformed `dp1_call`); `retryable: true`.
- `not_found`: the requested `itemId` was not found in the resolved
  playlist (`downloadPlaylistItem`), the item being *cleared* is entirely
  unknown to the device — neither cached nor queued nor otherwise tracked
  (`clearPlaylistItemCache`; see that command for why a clear that cancels
  a still-queued download is a success instead) — or the playlist being
  cleared is not cached (`clearPlaylistCache`, which is unaffected by that
  distinction: `downloadPlaylist` writes the playlist record before queuing
  any item, so a cached playlist always has a record).
  `getOfflineCacheStatus` never returns `not_found` for an unrecognized
  `itemId` — it always answers `ok: true` with that item reported as
  `state: "not_cached"` (see below), since querying an item that simply
  has no cache yet is not itself an error condition.
- `unsupported_media`: the item's source classifies as HLS/DASH manifest streaming
  (see `classify.go`'s `ClassStreaming`); this item can never be cached
  offline, so `retryable: false`. Every other class (software, media,
  unknown) is queueable.
- `offline_cache_error`: a store/disk/network failure inside the offline
  cache service; `retryable: true`.

### downloadPlaylistItem

Purpose: resolve a playlist, verify one item is not a live/streaming
source, and queue it for offline capture.

Example:

```json
{
  "messageID": "msg-dl-item-1",
  "message": {
    "command": "downloadPlaylistItem",
    "request": {
      "playlistUrl": "https://gallery.example/dp1/feed.json",
      "itemId": "work-1"
    }
  }
}
```

`dp1_call` (inline/dynamic DP1) is also accepted in place of `playlistUrl`,
using the same shapes as `displayPlaylist`.

Success response:

```json
{
  "ok": true,
  "status": "queued",
  "itemId": "work-1"
}
```

Error cases: `invalid_request` (missing `itemId`), `resolve_failed`,
`not_found` (itemId not in the resolved playlist), `unsupported_media`,
`busy`, `offline_cache_error`.

`busy` here (retryable) covers a `clearPlaylistItemCache`/
`clearPlaylistCache` for the same item that landed while this download was
still resolving and queuing. The clear wins by design — the alternative
would resurrect a record the device already told a client was deleted — so
nothing was queued, and this command reports that rather than answering
`status: "queued"` for work no worker will run. Re-issue the download once
the clear has settled and it queues normally. No `offline_cache_status`
notification is emitted for an item in this case, so a client must not
wait on one.

When the request was resolved via `playlistUrl` (not `dp1_call`) and the
item is queued successfully, `feral-controld` also best-effort indexes the
resolved playlist body under that same `playlistUrl` so a later offline
`displayPlaylist` by that URL can fall back to it (see
`docs/offline-artwork-capture.md`'s on-disk-format section). This indexing
is best-effort and never changes this command's own response: a failure
to index is logged, not surfaced as an error here, since the requested
item is genuinely queued either way.

### downloadPlaylist

Purpose: resolve a playlist and queue every cacheable item it contains (up
to `dp1.MAX_PLAYLIST_ITEMS_LIMIT` items) — every class except HLS/DASH manifest
streaming; streaming items are silently skipped rather than failing the
whole request.

Example:

```json
{
  "messageID": "msg-dl-playlist-1",
  "message": {
    "command": "downloadPlaylist",
    "request": {
      "playlistUrl": "https://gallery.example/dp1/feed.json"
    }
  }
}
```

Success response:

```json
{
  "ok": true,
  "status": "queued",
  "total": 12,
  "queuedCount": 5
}
```

`total` is every item in the resolved playlist; `queuedCount` is how many
were actually queued for offline capture — every class except HLS/DASH manifest
streaming (software via headless Chromium, media/unknown via direct HTTP
download; see `docs/offline-artwork-capture.md` §3.3). An item classified as
HLS/DASH manifest streaming (or missing an `id`/`source`) is simply excluded from
`queuedCount` with `ok: true` — that is the normal, successful shape for
a playlist with few or no cacheable items. If classification itself fails
(e.g. a transient network error reaching the classify target) for every
eligible item so nothing could be queued at all, this command instead
fails with `offline_cache_error` rather than returning that same
`ok: true`/`queuedCount: 0` shape: a broken classifier must not look
identical to "this playlist genuinely has no cacheable items" to the
controller. A classify failure for only *some* items still returns
`ok: true` with `queuedCount` reflecting whatever did queue
successfully; the skipped item(s) are logged server-side but not
individually reported here.

Classification of the playlist's items is **time-bounded** (10s total,
run concurrently) so this command acknowledges promptly regardless of
item count. Each item needs a network probe to classify, so an
unbounded serial pass over a large playlist of unreachable sources could
otherwise hold the command far past the LAN hub's own 30s response
deadline. An item not classified before that bound is treated exactly
like a classification failure: logged, skipped, and absent from
`queuedCount`. Re-issuing the command retries those items.

An item excluded because a concurrent clear won the race (see
`downloadPlaylistItem`'s `busy` case) is likewise absent from
`queuedCount` without failing the whole command: a playlist download is an
aggregate whose other items may well have queued fine, and `queuedCount`
already reports exactly how many did.

The resolved playlist (as `dp1.DP1` returns it —
`dynamicQuery` items already materialized, all field values including
`source` intact) is stored as-is (`playlists/<playlistId>.json`, no further
mutation) so a later `clearPlaylistCache` can operate on it. When the
request carried `playlistUrl` (as opposed to `dp1_call`), that URL is
additionally indexed so `displayPlaylist` with the same `playlistUrl` can
still find and display this exact cached playlist offline if live DP1
resolution later fails — see `displayPlaylist`'s section above and
`offline-artwork-capture.md` §6. Both the playlist body and its `playlistUrl`
index are written only after classification/queuing finishes, never
before — a request that ends up returning `offline_cache_error` above
(every eligible item failed classification) persists neither, so a
failed download can never leave a "last known good" offline fallback
that looks like a successful one. This is not
guaranteed to be byte-identical to whatever a publisher
originally served (`dp1` resolution re-serializes the Go struct, so key
order/whitespace can differ), but DP-1 signatures verify against a
JCS-canonicalized form rather than raw bytes, so this does not affect
signature validity — see `docs/offline-artwork-capture.md`.

Error cases: `resolve_failed`, `offline_cache_error`.

### clearPlaylistItemCache

Purpose: delete one cached item's record and garbage-collect any blobs it
was the last referent of.

Example:

```json
{
  "messageID": "msg-clear-item-1",
  "message": {
    "command": "clearPlaylistItemCache",
    "request": {
      "itemId": "work-1"
    }
  }
}
```

Success response:

```json
{
  "ok": true,
  "itemId": "work-1"
}
```

A clear that finds no cached record but *does* cancel a still-queued
download for `itemId` — including a first-time download that has not
captured anything yet — is also a success, not a `not_found`: work really
was canceled, and the item ends up `not_cached` either way.

Every item this command settles at `not_cached` (a deleted record, or a
canceled queued download) is pushed as an `offline_cache_status`
notification, so a connected controller does not have to poll
`getOfflineCacheStatus` to learn the item is gone. An item that was already
`not_cached` produces no notification — nothing transitioned.

Error cases: `invalid_request` (missing `itemId`), `not_found` (the device
has no cached record, queued download, or other tracked state for `itemId`
— nothing to clear), `busy` (retryable — `itemId` is the one item currently
mid-capture; retry once its in-flight download finishes, typically within a
few seconds up to the configured capture window), `offline_cache_error`.

### clearPlaylistCache

Purpose: delete a cached playlist's record and every one of its cached
items, garbage-collecting shared blobs.

Example:

```json
{
  "messageID": "msg-clear-playlist-1",
  "message": {
    "command": "clearPlaylistCache",
    "request": {
      "playlistId": "playlist-1"
    }
  }
}
```

Success response:

```json
{
  "ok": true,
  "playlistId": "playlist-1"
}
```

As with `clearPlaylistItemCache`, each member item this command settles at
`not_cached` is pushed as its own `offline_cache_status` notification.
Member items that were already `not_cached`, and any whose deletion failed
(the record — and therefore its `ready`/`partial` status — is still on
disk), are deliberately not announced.

Error cases: `invalid_request` (missing `playlistId`), `not_found` (playlist
is not cached), `busy` (retryable — one of the playlist's items is
currently mid-capture; the whole clear is rejected rather than clearing
everything else and leaving that one item to reappear once its capture
finishes — retry once it completes), `offline_cache_error` (also covers a
genuine per-item deletion failure partway through the sweep — e.g. a
permissions/I/O error deleting one item's on-disk record; every item that
*did* delete successfully, plus the playlist record and GC, still ran
before this is reported, so a retry only needs to contend with whatever
actually failed).

### getOfflineCacheStatus

Purpose: return a cache-state snapshot for the mobile app to render.

Example (specific items):

```json
{
  "messageID": "msg-status-1",
  "message": {
    "command": "getOfflineCacheStatus",
    "request": {
      "itemIds": ["work-1", "work-2"]
    }
  }
}
```

Omitting `itemIds` (or passing an empty array) reports on every item this
process currently knows about, on disk and in flight.

Request fields, all optional:

| Field | Type | Meaning |
|---|---|---|
| `itemIds` | string[] | Restrict the report to these items. Omitted or `[]` means every known item. At most **1024** ids per request. |
| `limit` | integer | Cap on how many entries `items` carries. Omitted, `0`, or above the cap is clamped to **1000**, which is also the maximum. |
| `cursor` | string | The `nextCursor` from the previous page. Omitted means the first page. |
| `totalsOnly` | boolean | Return `totals`/`diskUsed` with an empty `items`, for a summary view. Cannot be combined with `cursor`. |

Unlike the other commands in this family, these arguments are validated
strictly: a wrong type (for example `"itemIds": "work-1"` instead of an
array) is rejected with `invalid_request` rather than ignored, because
every one of these fields decides how much work the device does and how
large the response gets.

Success response:

```json
{
  "ok": true,
  "items": [
    {
      "itemId": "work-1",
      "state": "ready",
      "percent": 100,
      "bytes": 4213456,
      "coverageComplete": true
    },
    {
      "itemId": "work-2",
      "state": "partial",
      "percent": 100,
      "bytes": 189234,
      "coverageComplete": false,
      "reason": "loading_failed(net::ERR_CONNECTION_RESET):https://cdn.example/track.mp3"
    }
  ],
  "totals": {
    "total": 2,
    "ready": 1,
    "downloading": 0,
    "failed": 0
  },
  "diskUsed": 4402690
}
```

`items` is always ordered by `itemId` — including when `itemIds` was
given, so the response order does not follow the request order, and
duplicate ids collapse to one entry.

**Paging.** `items` never carries more than 1000 entries. When more
remain, the response adds `"truncated": true` and `"nextCursor": "<last
itemId in items>"`; pass that value back as `cursor` for the next page,
and repeat until `nextCursor` is absent. Both fields are absent on the
last page, so a client can treat "no `nextCursor`" as "that was
everything". Because paging is by sort order rather than by position, a
cursor stays valid even if the item it names is cleared or evicted
between pages.

`totals` and `diskUsed` describe the **whole requested set**, not the
current page, and for that reason are returned **only on the first page**
(a request with no `cursor`). Deriving them costs one on-disk read per
item in the set, so recomputing them for every page would make walking a
large cache cost far more than it needs to. A continuation page omits
both fields; carry forward what the first page reported. Use
`totalsOnly: true` when the summary is all you need — it skips the
per-item disk measurements and the response body entirely.

`state` is one of `not_cached`, `queued`, `downloading`, `ready`, `partial`,
`failed`, `broken_online`. `percent` is coarse (`0` or `100`): capture is a
single bounded-window operation, not chunked, so there is no meaningful
mid-download progress beyond queued/downloading vs. done. `100` covers
every state where the capture window already finished — `ready`,
`partial`, and `broken_online` (a finished capture whose only failures
were CSP blocks, so the artwork itself, not the download, is what is
broken — see below) — since none of those three will ever progress
further on their own; `0` covers `not_cached`, `queued`, `downloading`,
and `failed` (no successful capture ever completed to report progress
on). `reason` is only
present when `coverageComplete` is `false` and is free text, not a fixed
enum — it is a semicolon-joined list of per-resource capture outcomes,
each one of:
- `csp_blocked` embedded inside `loading_failed(csp_blocked):<url>` — the
  resource's own request failed even with live network, because the
  origin's Content-Security-Policy blocked it (see
  `offline-artwork-capture.md` §4.5). Clients should treat this as
  permanently degraded, not a transient capture failure.
- `loading_failed(<errorText>):<url>` — the browser's own
  `Network.loadingFailed` event for that URL, with `errorText` as CDP
  reported it (e.g. `net::ERR_CONNECTION_RESET`).
- `fetch_failed:<url>` — the resource loaded successfully in-browser but
  controld's own out-of-band re-fetch of its bytes failed.
- `unresolved_at_deadline:<url>` — the page requested this URL but it
  never reached a terminal `Network.responseReceived`/`loadingFailed`
  event before the capture window closed (e.g. a hanging/slow origin).
- `over_disk_budget:<url>` — the resource was seen but deliberately not
  stored because writing it would have pushed the cache past its
  configured `maxDiskBytes` ceiling. Retrying after older items have
  been evicted (or after the budget is raised) can succeed.
- `gltf_external_dependency:<uri>` — the item is a JSON `.gltf` manifest
  whose spec-defined `buffers[].uri`/`images[].uri` entries reference
  separate external files the direct-download path does not capture
  (see `offline-artwork-capture.md` §3.3). Like `csp_blocked`, treat
  this as permanently degraded for this source, not a transient capture
  failure — re-downloading the same manifest can never capture the
  missing files.

Clients should match on the fixed prefix before the first `:`/`(` rather
than the whole string, and must not assume the reason list is exhaustive
or stable in wording — only `coverageComplete` itself is a stable
boolean contract.

`reason` is **truncated to roughly 512 bytes** on the wire (both here and
in the `offline_cache_status` notification). One entry is emitted per
failed resource with that resource's full URL inline, and nothing bounds
how many resources an artwork loads, so an item captured with no network
can otherwise produce tens of KB of reason text on its own. Entries are
kept whole — a truncated list ends with `…(+N more)`, where `N` is how
many entries were dropped, so a partial list never reads as a complete
one. The untruncated text stays in the device's on-disk record for
support/debugging.

Error cases: `invalid_request` (an argument of the wrong type, more than
1024 `itemIds`, a negative or non-integral `limit`, an empty `cursor`, or
`totalsOnly` combined with `cursor`), `offline_cache_error`.

### offline_cache_status notification

Purpose: push per-item state transitions (queued, downloading, terminal
state) to the mobile app as they happen, so it does not need to poll
`getOfflineCacheStatus` to keep a live view current. Delivery is
best-effort, not guaranteed — see the delivery rules below, and prefer
`getOfflineCacheStatus` whenever a definitive answer is needed.

That includes transitions the app did not itself cause or cannot see the
result of: an item evicted by the disk-budget sweep, and an item cleared by
`clearPlaylistItemCache`/`clearPlaylistCache`, both push `not_cached`.
Clears are pushed even though the clearing client already got an `ok: true`
response, because *other* connected controllers (and local hub WebSocket
clients) would otherwise keep rendering a stale `queued`/`ready` entry
until they next poll.

Direction: `feral-controld` -> `ff-relayer` -> `ff-controller`, and mirrored
to local hub WebSocket clients.

```json
{
  "type": "notification",
  "notification_type": "offline_cache_status",
  "persist_record_count": 1,
  "message": {
    "itemId": "work-1",
    "state": "ready",
    "percent": 100,
    "bytes": 4213456,
    "coverageComplete": true
  }
}
```

Delivery is best-effort on both transports, and **neither one runs on the
goroutine that produced the notification**. Both are enqueued
(non-blocking) onto one bounded background queue (`notifyQueueCapacity`,
1024 entries in `notifier.go` — the DP-1 per-playlist item cap), drained
one notification at a time by a dedicated worker that performs the relayer
send and then the hub-WS send for each.

**Successive states of the same item coalesce while queued.** The queue
holds at most one pending notification per `itemId` — the latest state
that item has reached — so an item may go straight from `queued` to
`ready` on the wire with no `downloading` in between, and may appear only
once for a whole burst of transitions. Clients must therefore treat each
notification as *the item's current state*, never as a step in a sequence
they can count on receiving in full. Delivered notifications are never
reordered — one worker sends them in queue order, so `downloading` can
never arrive after `ready` for the same item — intermediate states are
simply skipped.

This is what keeps the bound safe: the queue's capacity counts in-flight
*items*, which a single command's playlist size bounds, rather than
*transitions*, which nothing bounds. A max-size playlist emits 1024 `queued` notifications from
command admission alone, before any `downloading`/terminal transitions —
with one slot per transition, those overran the buffer and accepted items
silently lost progress.

The capacity is one max-size playlist, which is the largest burst a single
command can produce — not a guarantee that drops cannot happen. Several
back-to-back `downloadPlaylist` calls (the rate limiter admits a burst of
3), or a disk-budget eviction sweep (bounded by the device's record count,
not by any playlist size), can exceed it in aggregate. That is what the
reconciliation advice below is for.

That matters because notifications are produced from two places that
must not pay for delivery: the single capture worker (which would
otherwise stall the whole download queue behind a slow transport) and
command admission itself — `enqueue` emits `queued` per item, and
`downloadPlaylist` drives that in a loop, so an inline relayer send
bounded at 5s each turned one playlist request into (item count) x 5s of
blocking, minutes past the LAN hub's own response deadline.

Per transport, at delivery time:

- **Relayer**: skipped silently (no log) when the relayer is not
  connected; otherwise write-deadline-bounded to 5s via
  `notifySendTimeout`, and only a failed `Send` is logged.
- **Hub WS**: `ws.WS.SendAll` is itself a no-op (debug-logged, not a
  drop/warning) when no hub clients are connected — that is `SendAll`'s
  own behavior (`ws.go`), not a decision `Notifier` makes before
  enqueueing. Its per-connection writes are each bounded
  (`ws.go`'s `sendWriteWait`), but the loop across however many clients
  are connected has no aggregate bound, which is the other reason
  delivery cannot run inline.

Two situations drop a notification outright, logged as a warning each
time: the queue is full (more than 1024 *distinct items* are pending
delivery at once — more than a whole max-size playlist; an update to an
already-pending item is coalesced into its existing slot and so is never
dropped for lack of room), or the notification was still queued when the
daemon began shutting down
(`Notifier.Close` does not drain the remainder — see its doc for why
flushing to a client the process is about to stop serving has no value).
Shutdown bounds how long it waits on the delivery already in flight:
`main.go` uses `Notifier.CloseWithin` with a budget well inside the
daemon's own shutdown timeout, because neither leg finishes fast enough
for it — the relayer send waits on that connection's mutex (bounded, but
several times over the whole shutdown timeout), and the hub fan-out
bounds each per-connection write but not the loop across clients, so that
leg grows with client count. Note this bounds that *step*, not shutdown
as a whole: the abandoned delivery keeps the transport mutex it was
using, and both transports take that same mutex for their own teardown
later in the shutdown sequence, so a wedged delivery delays shutdown
either way. A connection that hits its write deadline (either transport)
is dropped as a failed write and closed, same as any other write error;
the
queued/downloading captures behind it are unaffected either way.

The `message` body is one `items[]` entry of `getOfflineCacheStatus` and
follows the same rules, including the ~512-byte `reason` truncation
described there.

Clients that need a definitive current state should still poll
`getOfflineCacheStatus` rather than relying on every notification having
been delivered.

This notification is attempt-level, not cache-level: it reports the
outcome of one specific capture attempt for `itemId`, which is not always
the same thing as whether that item is currently playable offline. The
one case where they diverge: re-downloading an item that already has a
successful cached copy (`downloadPlaylistItem` on an already-`ready` item)
and that re-download fails. This notification fires `state: "failed"` for
the attempt, but the earlier successful capture's blobs and record on
disk were never touched by the failed attempt, so a `getOfflineCacheStatus`
call made right after (or the next `offline_cache_status` notification for
an unrelated event) will still report `ready`/`partial` for that same
`itemId` — the old cached copy remains valid and playable offline the
entire time. Clients should treat this notification as "this attempt's
result", and use `getOfflineCacheStatus` as the source of truth for
"is this item currently cached" when the two might disagree.

## Response Shape Recommendation for New Inbound Commands

Existing commands keep current behavior unless changed intentionally. New
controller-visible command families should use explicit RPC success/error
responses:

```json
{
  "type": "RPC",
  "messageID": "same-as-inbound",
  "message": {
    "ok": false,
    "error": {
      "code": "invalid_request",
      "message": "human-readable sanitized detail",
      "retryable": false
    }
  }
}
```

Rules:

- Echo the inbound `messageID`.
- Use stable `error.code` values for client behavior.
- Keep `error.message` sanitized; do not include API keys, browser session
  tokens, DP1 playlist content, or raw decrypted mint payloads.
- Treat unknown fields in `request` as forward-compatible unless the command
  has a security reason to reject them.
- Make commands idempotent when relayer delivery can be retried.

## Security Notes

- `ff-controller` inbound messages are user/client controlled; validate type,
  bounds, and required fields before side effects.
- The current command envelope can contain secrets such as `uploadLogs.apiKey`;
  avoid full-payload logs.
- Mint-pairing approval messages must never include raw browser session tokens.
- DP1 feed URLs are allowed in `displayPlaylist`, but DP1 playlist content must
  not be sent to `ff-controller` as part of mint approval.
- Device-control commands with destructive side effects (`shutdown`, `reboot`,
  `factoryReset`, `updateToLatestVersion`, `sshAccess`) should remain explicit
  command names with narrow request bodies.
