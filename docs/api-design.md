# API and Protocol Direction

This document defines the canonical API and protocol design direction for `ffos-user`.
Agents should treat these rules as stable constraints when adding, changing, or removing any interface.

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
| `com.feralfile.controld` | `/com/feralfile/controld` | `com.feralfile.controld.general` | RPC | `GetRelayerTopicID() → (string, error)` |
| `com.feralfile.controld` | `/com/feralfile/controld` | `com.feralfile.controld.general` | Signal emitter (received from setupd via controld bus) | `show_pairing_qr_code`, `factory_reset`, `system_update`, `upload_logs`, `upload_logs_with_bundle` |
| `com.feralfile.sysmonitord` | `/com/feralfile/sysmonitord` | `com.feralfile.sysmonitord` | RPC | `GetConnectivityStatus(refresh bool) → (bool, error)`, `GetSysMetrics() → (*SysDBusMetrics, error)` |
| `com.feralfile.sysmonitord` | `/com/feralfile/sysmonitord` | `com.feralfile.sysmonitord` | Signal emitter | `sysmetrics`, `connectivity_change`, `sysevent` |
| `com.feralfile.watchdog` | — | — | Bus name only (no exported RPCs currently) | — |

---

## Request and Response Schema Rules

### D-Bus RPC

- Methods return typed Go/Rust values plus `*dbus.Error` as the final return value.
- A nil `dbus.Error` means success. A non-nil `dbus.Error` means failure; the error message is a human-readable string.
- Callers must treat an error response as a signal to retry, fall back, or log — not silently ignore.
- Boolean parameters that control behavior (e.g. `refresh bool` on `GetConnectivityStatus`) should be explicit positional args, not buried in a map.

### D-Bus Signals

Signals carry either:
- A single primitive value (e.g. `connectivity_change` carries a single `bool`)
- A JSON-serialized byte slice for structured data (e.g. `sysmetrics` carries `[]byte` which is a JSON-encoded metrics struct)

Do not add ad-hoc fields to signal bodies without updating all consumers. Prefer the byte-slice JSON pattern for structured payloads so the schema can evolve with additive fields.

### Relayer WebSocket protocol

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

- `messageID == "system"`: a system message. The `message.topicID` field, if present, must be saved to state and returned to any pending `GetRelayerTopicID` callers.
- Any other `messageID`: a command message. Route to `commandrouter`.

**Outbound (device sends to relayer):**
```json
{
  "type": "<response-type>",
  "messageID": "<echoed-from-request>",
  "message": <any>
}
```

**Command routing logic (inside controld):**
- If `Command.DeviceCtlCommand()` returns true → route to the device executor (`devicectl`).
- Otherwise → route to Chromium via CDP (`Runtime.evaluate`).

**Device-control relayer commands**

The following command names are routed to `devicectl` and use the standard relayer/hub envelope (`command` plus `request`):

| Command | Request fields | Notes |
|---|---|---|
| `dragGesture` | `cursorOffsets` | Array of `{dx, dy}` step deltas. |
| `tapGesture` | `button` | `button` selects left, right, or middle; missing or empty defaults to left. |
| `doubleTapGesture` | `button` | Same button selection as `tapGesture`. |
| `longPressGesture` | `button` | Same button selection as `tapGesture`. |
| `clickAndDragGesture` | `cursorOffsets` | Press, move, then release. The executor treats release failure as an error because Chromium can remain pressed. |
| `zoomGesture` | `scaleSteps` | Array of positive float scale factors. The executor uses `Input.synthesizePinchGesture` with the default gesture source, and falls back to non-Ctrl wheel input only when pinch synthesis is unsupported. |
| `setSleepSchedule` | `enabled`, optional `sleepTime`, `wakeTime` (HH:MM) | Persists the FF1 sleep/wake window and enables or disables automatic transitions. |
| `sleepNow` | — | Manual override toward sleep until the next schedule boundary (when the schedule is enabled). |
| `wakeNow` | — | Manual override toward awake until the next schedule boundary (when the schedule is enabled). |

`devicectl` also exposes two device-control commands for panel control over DDC/CI via `ddcutil`: `ddcPanelControl` (set brightness, contrast, speaker volume, mute, or power using a single JSON request body that selects the action) and `ddcPanelStatus` (query the same VCPs and return a structured status object). Both share the standard relayer/hub envelope; detailed field shapes live alongside the executor in `devicectl/ddc.go`.

**Sleep schedule vs. FFP panel power (contract):** `setSleepSchedule`, `sleepNow`, and `wakeNow` apply **FF1 player sleep mode** over CDP **synchronously** for the purpose of command success: if the handler returns success, the player has been asked to enter or leave sleep mode on that request path. **FFP panel power** (DDC standby / on) is aligned **asynchronously** in a dedicated worker so slow or flaky `ddcutil` calls do not block relayer or hub deadlines. DDC failures are **best-effort** (logged; command success is still possible). Rapid sleep/wake transitions are **coalesced** so an older in-flight DDC call cannot overwrite a newer intended state. **`device_status.message.sleepSchedule`** (and the `sleepSchedule` object returned on these commands) reflects the **schedule and player sleep intent**; **DDC-derived fields** (for example from `ddcPanelStatus`) may **temporarily disagree** until alignment completes (**eventual consistency**). **`device_status` refresh** after a transition may run before DDC finishes, so consumers must not assume panel power and player sleep mode flip in the same notification. On **process exit**, `feral-controld` does **not** wait for queued or in-flight DDC alignment work.

Successful `setSleepSchedule`, `sleepNow`, and `wakeNow` responses include `{"ok": true, "sleepSchedule": { ... }}` where `sleepSchedule` matches `sleepschedule.Status` JSON: `enabled` (bool), `sleepTime` / `wakeTime` (HH:MM strings), `currentState` (`awake` | `sleeping`), optional `overrideState`, optional `overrideUntil` / `nextTransitionAt` (RFC3339 timestamps when present). The same object shape appears under `device_status.message.sleepSchedule` when the schedule file is readable (omitted when the file is missing or unreadable without blocking status).

**Command type constants** are defined in `components/feral-controld/commands/types.go`. New remote commands must be added there with a corresponding entry in `deviceCtlCommands` if they require executor handling.

The `uploadLogs` command accepts `userId`, `apiKey`, and `title`, plus optional `supportBundleID` or `support_bundle_id`. Without a bundle id, `feral-controld` emits the original `upload_logs(user_id, api_key, title)` signal. With a bundle id, it emits additive `upload_logs_with_bundle(payload []byte)` where `payload` is JSON containing `user_id`, `api_key`, `title`, and `support_bundle_id`, so the old D-Bus signal payload shape stays unchanged and the new bundled upload payload can grow additively.

**Relayer outbound notifications (`feral-controld`):** The device periodically pushes JSON notifications over the relayer WebSocket (and local hub clients) with an envelope that includes `notification_type` and a structured `message`. At minimum:

- `player_status` — playback/UI state from Chromium via CDP `checkStatus` (cast command, playlist, pause, etc.). This is not a substitute for hardware or OS-level facts.
- `device_status` — device-oriented fields assembled by `status.DeviceStatus.GetStatus` (screen rotation, Wi‑Fi name, installed/latest version, volume, feature toggles, MAC info, best-effort `displayURL`, and optional `sleepSchedule`). The `displayURL` field is the top-level URL of the sole Chromium **page** debug target (DevTools `/json`), when exactly one such target exists; it is omitted when the URL cannot be resolved. Consumers that previously read a Chrome document URL from player payloads should use `device_status.message.displayURL` instead. When present, `sleepSchedule` follows the same **sleep vs. DDC** eventual-consistency rules as the `setSleepSchedule` / `sleepNow` / `wakeNow` contract above.

### Hub WebSocket protocol (port 1111)

The Hub uses the same JSON command envelope as the relayer. The Hub does not carry `messageID == "system"` messages. A Hub client sends a command; `controld` routes it through the same `commandrouter` as relayer commands.

### BLE GATT protocol (feral-setupd)

The BLE command characteristic uses a binary encoding:

**Write (mobile → device):** `[cmd_string]\x00[reply_id_string]\x00[param1_string]\x00...`

- `cmd` is a null-terminated string matching one of the constants in `constant.rs`.
- `reply_id` is a null-terminated string used to correlate the response.
- Zero or more `params` follow, each null-terminated.

**Notification (device → mobile):** `[reply_id_string]\x00[status_code_u8][result_string1]\x00...`

- `reply_id` echoes the request's `reply_id`.
- `status_code` is a single byte. `0` = success; non-zero values are defined in `constant.rs` (`BLE_ERR_CODE_*`).
- Zero or more result strings follow, each null-terminated.

**BLE command registry:**

| Command constant | String | Description |
|---|---|---|
| `CMD_CONNECT_WIFI` | `connect_wifi` | Connect to a WiFi network |
| `CMD_SCAN_WIFI` | `scan_wifi` | Scan for available SSIDs |
| `CMD_GET_INFO` | `get_info` | Returns `device_info` string |
| `CMD_SET_TIME` | `set_time` | Set device system time |
| `CMD_KEEP_WIFI` | `keep_wifi` | Keep current WiFi and proceed |
| `CMD_FACTORY_RESET` | `factory_reset` | Initiate factory reset |
| `CMD_SEND_LOGS` | `send_log` | Upload device logs |

`send_log` accepts `user_id`, `api_key`, and `title` parameters, plus an optional fourth `support_bundle_id` parameter. When present, `feral-setupd` includes it in the FF1 `/v2/ff1/log-submissions` request so support-logs can join FF1 evidence into the support bundle.

**`device_info` string format** (returned by `get_info`):

```
<device_id>|<topic_id>|<internet>|<branch>|<version>
```

- `branch` is URL-safe encoded: `/` replaced with `%2F`.
- `internet` is the string `"true"` or `"false"` (cached connectivity value).
- `topic_id` may be empty string if not yet assigned.

This format is a contract between `feral-setupd` and the mobile app. Do not add, remove, or reorder fields without a coordinated mobile-app release.

---

## Backward-Compatibility Posture

1. **Additive changes are always safe.** Add new D-Bus methods, new JSON fields, new BLE commands, or new relayer command types without breaking existing callers.
2. **Never rename or remove existing methods or fields** without a version bump or a coordinated multi-service release that updates all callers simultaneously.
3. **Never change D-Bus signal payload shapes** (member name, body types) without updating all subscribers in the same PR.
4. **BLE payload format changes** require a coordinated mobile-app release. Treat the BLE encoding as a stable wire format between releases.
5. **Relayer command field names** (`command`, `request`) are shared with the web app layer and potentially the mobile app. Do not rename them without coordinating with all consumers (the `FIXME` comments in `commands/types.go` acknowledge this debt).
6. **`device_info` string** is parsed by the mobile app. Field order and separator (`|`) are fixed.

---

## Error Payload Conventions

### D-Bus errors

Use `dbus.NewError(message, []interface{}{})` for all D-Bus method errors. The first argument is a human-readable error message. The second is an empty slice (no additional error body values). Do not put structured data in the error body.

### Relayer errors

There is no standardized error response envelope defined yet. When an executor command fails, `controld` logs the error and does not send an explicit error response to the relayer unless the command protocol requires a reply. When adding new commands that need error responses, document the response shape in code comments near the command handler.

### BLE error codes

Error codes are single bytes defined in `constant.rs`. Use the most specific code available. Do not invent new codes without updating `constant.rs` and the mobile app in a coordinated release.

| Code | Constant | Meaning |
|---|---|---|
| `0` | `BLE_SUCCESS_CODE` | Success |
| `1` | `BLE_ERR_CODE_WRONG_WIFI_PWD` | Wrong WiFi password |
| `2` | `BLE_ERR_CODE_NO_INTERNET` | WiFi connected but no internet |
| `3` | `BLE_ERR_CODE_SERVER_UNREACHABLE` | Server unreachable |
| `4` | `BLE_ERR_CODE_WIFI_REQUIRED` | WiFi required but not connected |
| `5` | `BLE_ERR_CODE_DEVICE_UPDATING` | Device is currently updating |
| `6` | `BLE_ERR_CODE_VERSION_CHECK_FAILED` | Version check failed |
| `7` | `BLE_ERR_CODE_INVALID_PARAMS` | Invalid parameters |
| `9` | `BLE_ERR_CODE_NETWORK_ERROR` | Generic network error |
| `10` | `BLE_ERR_CODE_VERSION_TOO_OLD` | Device version too old for auto-upgrade |
| `255` | `BLE_ERR_CODE_UNKNOWN_ERROR` | Unknown error |

---

## Timeout and Retry Expectations Across Service Boundaries

### D-Bus call timeouts

| Caller | Callee | Method | Timeout |
|---|---|---|---|
| `feral-controld` | `feral-sys-monitord` | `GetConnectivityStatus` | 7 seconds |
| `feral-setupd` | `feral-sys-monitord` | `GetConnectivityStatus` | 7 second (`DBUS_INTERNET_CHECK_TIMEOUT`) |
| `feral-setupd` | `feral-controld` | `GetRelayerTopicID` | 31 seconds (`DBUS_RELAYER_CHECK_TIMEOUT`) |
| `feral-setupd` | — | Wait for controld to appear on bus | 30 seconds (`WAIT_FOR_CONTROLD_TIMEOUT`) |

RPCs that timeout should log the error and either fail the calling operation or fall back to a cached/default value. Do not silently swallow D-Bus timeouts.

### D-Bus signal retries

`feral-controld` uses `RetryableSend` for D-Bus signals that must not be dropped (e.g. sending event signals to `feral-setupd`). Retryable sends should back off and log on repeated failure.

### Relayer connection

`feral-controld` retries the relayer WebSocket connection with exponential back-off. The relayer connection is conditional on `GetConnectivityStatus` returning true and the persisted `TopicID` being non-empty. If either precondition is missing, `controld` waits for the `connectivity_change` D-Bus signal before attempting to connect.

### BLE response

The mobile app expects a BLE notification within a reasonable time after a write. Long-running BLE commands (e.g. `connect_wifi`, `send_log`) must either:
- Return quickly with `BLE_SUCCESS_CODE` and perform the work asynchronously (`UpdateExecution::NonBlocking`), or
- Return the result directly if the operation completes quickly enough.

`feral-setupd` uses `UpdateExecution::Blocking` for flows started from D-Bus (which can wait) and `UpdateExecution::NonBlocking` for flows started from BLE (which must respond quickly to the mobile app).

### Updater version check

`feral-setupd` retries the remote version check up to `UPDATER_VERSION_CHECK_RETRIES` (3) times with a 2-second delay between retries before treating the check as failed.

---

## Protocol Invariants Agents Must Not Break

1. The relayer `messageID == "system"` path is the canonical source of the device's `TopicID`. Do not add a second path that sets `TopicID` without going through this flow.
2. `GetRelayerTopicID` on D-Bus blocks until the topic ID is available (up to 31 seconds). Callers must account for this latency. Do not convert it to an async signal without updating all callers.
3. `sysmetrics` signal body is a JSON-encoded byte slice. Consumers unmarshal it into the metrics struct. Adding fields to the struct is safe; removing or renaming fields is a breaking change.
4. `connectivity_change` signal body is a single `bool`. It must stay a single `bool`. If more data is needed, add a new signal rather than replacing this one.
5. BLE `get_info` returns exactly one string element (the `device_info` string). Do not add a second element without updating the mobile app.
6. Hub WebSocket accepts exactly the same command envelope as the relayer. The Hub and relayer command paths share `commandrouter`. Do not diverge them without explicit justification.
