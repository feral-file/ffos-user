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
  through CDP as `window.handleCDPRequest(...)`.
- `startMintPairingSession` and `mintPairingApprovalDecision` are handled by
  `feral-controld` as commandrouter pre-CDP special cases.
- `refreshArtwork` clears Chromium cache, then forwards to Chromium through
  CDP.
- Any other non-device command is forwarded to Chromium through CDP.

Current relayer error behavior is important: if command processing fails,
`feral-controld` logs and returns an internal handler error. It does not
currently send a standardized RPC error response over the relayer. New inbound
message families that require controller-visible errors must define their own
response shape.

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

Purpose: ask `feral-setupd` to show or hide the setup pairing QR code.

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
- D-Bus send failure to `feral-setupd` causes command failure.

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

Current error cases:

- Status collection dependencies may fail; unavailable fields are usually
  omitted when best-effort collection can continue.
- A hard status collection error causes command failure.

Current relayer error response: none standardized; command failure is logged.

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

Current error cases:

- Missing both `playlistUrl` and `dp1_call`: command failure with
  `unknown payload type`.
- `playlistUrl` is not a non-empty string.
- DP1 URL fetch or processing fails.
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
forwarded to Chromium through CDP.

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

Current success response: Chromium/player response.

Current error cases:

- Cache clear failure is logged as a warning and does not stop the command.
- CDP command forwarding failure causes command failure.

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

Purpose: signal `feral-setupd` to show update UI and execute a system update.

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

- D-Bus send failure to `feral-setupd`.

Current relayer error response: none standardized; command failure is logged.

### factoryReset

Purpose: signal `feral-setupd` to show reset UI and execute factory reset.

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

- D-Bus send failure to `feral-setupd`.

Current relayer error response: none standardized; command failure is logged.

### uploadLogs

Purpose: signal `feral-setupd` to upload logs. The optional support bundle id
uses an additive D-Bus signal.

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

Current error cases:

- Invalid request shape.
- Missing `userId`, `apiKey`, or `title`.
- Bundled upload payload cannot be marshaled.
- D-Bus send failure to `feral-setupd`.

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

If a non-expired pairing session is already active, `status` is
`already_started` and `feral-controld` re-displays the same pairing code.

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
- `invalid_config`: broker base URL is missing.
- `topic_not_ready`: device has no current relayer topic ID.
- `broker_unavailable`: broker channel creation failed before a pairing code
  was available.
- `broker_response_invalid`: broker did not return a pairing code.
- `display_unavailable`: Chromium/CDP did not accept the pairing QR page
  navigation.

On success, `feral-controld` navigates Chromium to the dedicated mint-pairing
QR page and passes `pairing_code` in the page URL. The QR code encodes that
pairing code and the same code is rendered below the QR code in large text for
long-distance readability.

When the mint-pairing attempt reaches a terminal state, `feral-controld`
restores Chromium to the bundled local player at `http://127.0.0.1:8080/`.
This restore is attempted after success, controller rejection, approval expiry,
controller/service cancellation, and terminal failure. During process shutdown,
terminal broker/relayer delivery and display restoration are bounded so
mint-pairing cleanup fits within `feral-controld`'s two-second forced-exit
guard; if a terminal delivery exceeds that budget, it is logged and treated as
best-effort.

### Outbound Approval Request

This is included for context because it creates the pending inbound decision.

Direction: `feral-controld` -> `ff-relayer` -> `ff-controller`.

```json
{
  "type": "mint_pairing_approval_request",
  "messageID": "mpa_01JZ6Y9M7S0H9G9ER4T52Q70W8",
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
  "type": "mint_pairing_approval_outcome",
  "messageID": "mpa_01JZ6Y9M7S0H9G9ER4T52Q70W8",
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
