# Behavior migration matrix

Audit against revisions in `evidence/baseline.json`. This inventory is not an implementation-completion claim. Verification remains pending unless an evidence file names a passing check. The historical enum contains some names with no live dispatcher; those are called out, not promoted into new APIs.

## Command inventory

| Existing command/capability | Source evidence | New owner / disposition | Verification |
|---|---|---|---|
| `analyticsToggle` | ffos-user/components/feral-controld/commands/types.go:201 | Controld/device service; preserved external contract, audit adapter dependency | Pending migration/behavior fixture |
| `betaFeaturesToggle` | ffos-user/components/feral-controld/commands/types.go:202 | Controld/device service; preserved external contract, audit adapter dependency | Pending migration/behavior fixture |
| `checkStatus` | ff-player/src/models/cast_info.model.ts:14 | Native host; redesigned typed operation preserving external adapter | Native status mapper and LAN/relayer egress tests |
| `clearPlaylistCache` | ffos-user/components/feral-controld/commands/types.go:267 | Controld/device service; preserved external contract, audit adapter dependency | Pending migration/behavior fixture |
| `clearPlaylistItemCache` | ffos-user/components/feral-controld/commands/types.go:266 | Controld/device service; preserved external contract, audit adapter dependency | Pending migration/behavior fixture |
| `clickAndDragGesture` | ffos-user/components/feral-controld/commands/types.go:195 | Native input state; preserved gesture adapter in controld | Go coordinator tests; FF1 controller-input.json and input-after-cutover.json. Installed normal-daemon cutover; see CUTOVER.md |
| `closeMintPairingSession` | ffos-user/components/feral-controld/commands/types.go:244 | Controld/device service; preserved external contract, audit adapter dependency | Pending migration/behavior fixture |
| `connect` | ff-player/src/models/cast_info.model.ts:12<br>ffos-user/components/feral-controld/commands/types.go:187 | Native host; redesigned typed operation preserving external adapter | Pending migration/behavior fixture |
| `cursorUpdate` | ff-player/src/models/cast_info.model.ts:20 | Native host; redesigned typed operation preserving external adapter | Pending migration/behavior fixture |
| `ddcPanelControl` | ffos-user/components/feral-controld/commands/types.go:248 | Controld/device service; preserved external contract, audit adapter dependency | Pending migration/behavior fixture |
| `ddcPanelStatus` | ffos-user/components/feral-controld/commands/types.go:250 | Controld/device service; preserved external contract, audit adapter dependency | Pending migration/behavior fixture |
| `deviceMetrics` | ffos-user/components/feral-controld/commands/types.go:189<br>ffos-user/components/feral-controld/commands/types.go:197 | Controld/device service; preserved external contract, audit adapter dependency | Pending migration/behavior fixture |
| `disconnect` | ff-player/src/models/cast_info.model.ts:13 | Native host; redesigned typed operation preserving external adapter | Pending migration/behavior fixture |
| `displayDefaultPlaylist` | ff-player/src/models/cast_info.model.ts:26<br>ffos-user/components/feral-controld/commands/types.go:211 | Native host; redesigned typed operation preserving external adapter | Pending migration/behavior fixture |
| `displayPlaylist` | ff-player/src/models/cast_info.model.ts:21<br>ffos-user/components/feral-controld/commands/types.go:205 | Native host; redesigned typed operation preserving external adapter | Pending migration/behavior fixture |
| `doubleTapGesture` | ffos-user/components/feral-controld/commands/types.go:193 | Native input state; preserved gesture adapter in controld | Go coordinator tests; FF1 controller-input.json and input-after-cutover.json. Installed normal-daemon cutover; see CUTOVER.md |
| `downloadPlaylist` | ffos-user/components/feral-controld/commands/types.go:265 | Controld/device service; preserved external contract, audit adapter dependency | Pending migration/behavior fixture |
| `downloadPlaylistItem` | ffos-user/components/feral-controld/commands/types.go:264 | Controld/device service; preserved external contract, audit adapter dependency | Pending migration/behavior fixture |
| `dragGesture` | ffos-user/components/feral-controld/commands/types.go:191 | Native input state; preserved gesture adapter in controld | Go coordinator tests; FF1 controller-input.json and input-after-cutover.json. Installed normal-daemon cutover; see CUTOVER.md |
| `factoryReset` | ffos-user/components/feral-controld/commands/types.go:206 | Controld/device service; preserved external contract, audit adapter dependency | Pending migration/behavior fixture |
| `getContentPolicy` | ffos-user/components/feral-controld/commands/types.go:269 | Controld/device service; preserved external contract, audit adapter dependency | Pending migration/behavior fixture |
| `getDeviceStatus` | ffos-user/components/feral-controld/commands/types.go:203 | Controld/device service; preserved external contract, audit adapter dependency | Pending migration/behavior fixture |
| `getMediaHealth` | ffos-user/components/feral-controld/commands/types.go:282 | Controld/device service; preserved external contract, audit adapter dependency | Pending migration/behavior fixture |
| `getMintPairingSession` | ffos-user/components/feral-controld/commands/types.go:239 | Controld/device service; preserved external contract, audit adapter dependency | Pending migration/behavior fixture |
| `getOfflineCacheStatus` | ffos-user/components/feral-controld/commands/types.go:268 | Controld/device service; preserved external contract, audit adapter dependency | Pending migration/behavior fixture |
| `getRecentlyPlayed` | ff-player/src/models/cast_info.model.ts:31<br>ffos-user/components/feral-controld/commands/types.go:216 | Native host; redesigned typed operation preserving external adapter | Go native history projection/privacy/failure tests; FF1 read-only list/resolve check |
| `getSeriesPlayback` | ffos-user/components/feral-controld/commands/types.go:276 | Controld/device service; preserved external contract, audit adapter dependency | Pending migration/behavior fixture |
| `handleConnectivityChange` | ff-player/src/services/cdp-handler/CDPRequestHandler.ts; ffos-user/components/feral-controld/mediahealth_wiring.go | Native host; redesigned typed operation preserving external adapter | Pending migration/behavior fixture |
| `handleWatchdogEvent` | ff-player/src/services/cdp-handler/CDPRequestHandler.ts; ffos-user/components/feral-controld/mediahealth_wiring.go | Native host; redesigned typed operation preserving external adapter | Pending migration/behavior fixture |
| `joinMintPairingChannel` | ffos-user/components/feral-controld/commands/types.go:243 | Controld/device service; preserved external contract, audit adapter dependency | Pending migration/behavior fixture |
| `longPressGesture` | ffos-user/components/feral-controld/commands/types.go:194 | Native input state; preserved gesture adapter in controld | Go coordinator tests; FF1 controller-input.json and input-after-cutover.json. Installed normal-daemon cutover; see CUTOVER.md |
| `mediaHealthHints` | ff-player/src/services/cdp-handler/CDPRequestHandler.ts; ffos-user/components/feral-controld/mediahealth_wiring.go | Controld/device service; preserved external contract, audit adapter dependency | Pending migration/behavior fixture |
| `mintPairingApprovalDecision` | ffos-user/components/feral-controld/commands/types.go:245 | Controld/device service; preserved external contract, audit adapter dependency | Pending migration/behavior fixture |
| `mintPairingDisplay` | ff-player/src/services/cdp-handler/CDPRequestHandler.ts; ffos-user/components/feral-controld/mediahealth_wiring.go | Native host; redesigned typed operation preserving external adapter | Pending migration/behavior fixture |
| `moveToArtwork` | ff-player/src/models/cast_info.model.ts:15 | Native host; redesigned typed operation preserving external adapter | Pending migration/behavior fixture |
| `ping` | ff-player/src/services/cdp-handler/CDPRequestHandler.ts; ffos-user/components/feral-controld/mediahealth_wiring.go | Native host; redesigned typed operation preserving external adapter | Pending migration/behavior fixture |
| `playRecentlyPlayed` | ffos-user/components/feral-controld/commands/types.go:217 | Controld/device service; preserved external contract, audit adapter dependency | Native lookup plus existing policy/source admission tests; typed native cast transport active |
| `reboot` | ffos-user/components/feral-controld/commands/types.go:200 | Controld/device service; preserved external contract, audit adapter dependency | Pending migration/behavior fixture |
| `refreshArtwork` | ff-player/src/models/cast_info.model.ts:17<br>ffos-user/components/feral-controld/commands/types.go:212 | Host creates a fresh occurrence; removed global CDP cache purge/page navigation because control no longer lives in the document | Native controller and recovery tests; empty selection rejects `no_artwork` |
| `refreshPlaylist` | ff-player/src/models/cast_info.model.ts:22 | Removed internal enum-only command: not handled by CanvasService; use explicit selection/source refresh | Pending migration/behavior fixture |
| `resolveRecentlyPlayed` | ff-player/src/models/cast_info.model.ts:32<br>ffos-user/components/feral-controld/commands/types.go:220 | Native host; redesigned typed operation preserving external adapter | Internal history.resolve only; matching record ID validated; external resolver rejected |
| `rotate` | ffos-user/components/feral-controld/commands/types.go:198 | Controld/device service; preserved external contract, audit adapter dependency | Pending migration/behavior fixture |
| `runNetworkDiagnostics` | ffos-user/components/feral-controld/commands/types.go:258 | Controld/device service; preserved external contract, audit adapter dependency | Pending migration/behavior fixture |
| `sendKeyboardEvent` | ffos-user/components/feral-controld/commands/types.go:190 | Native input state; preserved gesture adapter in controld | Go coordinator tests; FF1 controller-input.json and input-after-cutover.json. Installed normal-daemon cutover; see CUTOVER.md |
| `setContentPolicy` | ffos-user/components/feral-controld/commands/types.go:270 | Controld/device service; preserved external contract, audit adapter dependency | Pending migration/behavior fixture |
| `setDeviceName` | ffos-user/components/feral-controld/commands/types.go:230 | Controld/device service; preserved external contract, audit adapter dependency | Pending migration/behavior fixture |
| `setLoop` | ff-player/src/models/cast_info.model.ts:25 | Native host; redesigned typed operation preserving external adapter | Pending migration/behavior fixture |
| `setMediaHealth` | ffos-user/components/feral-controld/commands/types.go:286 | Controld/device service; preserved external contract, audit adapter dependency | Pending migration/behavior fixture |
| `setSeriesPlayback` | ffos-user/components/feral-controld/commands/types.go:277 | Controld/device service; preserved external contract, audit adapter dependency | Pending migration/behavior fixture |
| `setShuffle` | ff-player/src/models/cast_info.model.ts:24 | Native host; redesigned typed operation preserving external adapter | Pending migration/behavior fixture |
| `setSleepMode` | ff-player/src/models/cast_info.model.ts:23<br>ffos-user/components/feral-controld/commands/types.go:224 | Native host; redesigned typed operation preserving external adapter | Pending migration/behavior fixture |
| `setSleepSchedule` | ffos-user/components/feral-controld/commands/types.go:221 | Controld/device service; preserved external contract, audit adapter dependency | Pending migration/behavior fixture |
| `setVolume` | ffos-user/components/feral-controld/commands/types.go:208 | Controld/device service; preserved external contract, audit adapter dependency | Pending migration/behavior fixture |
| `setupDisplay` | ff-player/src/services/cdp-handler/CDPRequestHandler.ts; ffos-user/components/feral-controld/mediahealth_wiring.go | Native host; redesigned typed operation preserving external adapter | Pending migration/behavior fixture |
| `showPairingQRCode` | ffos-user/components/feral-controld/commands/types.go:188 | Controld/device service; preserved external contract, audit adapter dependency | Pending migration/behavior fixture |
| `shutdown` | ffos-user/components/feral-controld/commands/types.go:199 | Controld/device service; preserved external contract, audit adapter dependency | Pending migration/behavior fixture |
| `sleepNow` | ffos-user/components/feral-controld/commands/types.go:222 | Controld/device service; preserved external contract, audit adapter dependency | Pending migration/behavior fixture |
| `sshAccess` | ffos-user/components/feral-controld/commands/types.go:210 | Controld/device service; preserved external contract, audit adapter dependency | Pending migration/behavior fixture |
| `startMintPairingSession` | ffos-user/components/feral-controld/commands/types.go:238 | Controld/device service; preserved external contract, audit adapter dependency | Pending migration/behavior fixture |
| `startWifiSetup` | ffos-user/components/feral-controld/commands/types.go:237 | Controld/device service; preserved external contract, audit adapter dependency | Pending migration/behavior fixture |
| `tapGesture` | ffos-user/components/feral-controld/commands/types.go:192 | Native input state; preserved gesture adapter in controld | Go coordinator tests; FF1 controller-input.json and input-after-cutover.json. Installed normal-daemon cutover; see CUTOVER.md |
| `toggleMute` | ffos-user/components/feral-controld/commands/types.go:209 | Controld/device service; preserved external contract, audit adapter dependency | Pending migration/behavior fixture |
| `updateArtFraming` | ff-player/src/models/cast_info.model.ts:18 | Native host; redesigned typed operation preserving external adapter | Pending migration/behavior fixture |
| `updateDefaultDuration` | ff-player/src/models/cast_info.model.ts:27 | Native host; redesigned typed operation preserving external adapter | Pending migration/behavior fixture |
| `updateDisplaySettings` | ff-player/src/models/cast_info.model.ts:19 | Native host; typed saved defaults and occurrence-bound preview | Native controller tests and FF1 controller-controls.json; legacy per-side/token/autoplay flags remain unsupported |
| `updateIndex` | ff-player/src/models/cast_info.model.ts:16 | Removed internal enum-only command: not handled by CanvasService; use explicit selection/source refresh | Pending migration/behavior fixture |
| `updateToLatestVersion` | ffos-user/components/feral-controld/commands/types.go:204 | Controld/device service; preserved external contract, audit adapter dependency | Pending migration/behavior fixture |
| `uploadLogs` | ffos-user/components/feral-controld/commands/types.go:207 | Controld/device service; preserved external contract, audit adapter dependency | Pending migration/behavior fixture |
| `wakeNow` | ffos-user/components/feral-controld/commands/types.go:223 | Controld/device service; preserved external contract, audit adapter dependency | Pending migration/behavior fixture |
| `zoomGesture` | ffos-user/components/feral-controld/commands/types.go:196 | Native input state; preserved gesture adapter in controld | Go coordinator tests; FF1 controller-input.json and input-after-cutover.json. Installed normal-daemon cutover; see CUTOVER.md |

## Lifecycle behavior beyond command names

| Existing behavior | Source evidence | New owner / disposition | Verification |
|---|---|---|---|
| Boot playlist takes priority except version-update reload; retained cast otherwise | ff-player/src/context/AppContext.tsx:255–297; utils/DeviceManager.ts:246–276 | Host owns boot/restart distinction; boot intent imported/stored, execution still pending | Migration preserves boot intent; boot execution not qualified |
| Live casts and deliberate stops win over asynchronous storage hydration | ff-player/src/context/AppContext.tsx:308–350 | Preserved by loading durable host state before opening the command socket; no page hydration race | Core/import restart tests; native startup-corruption UI remains pending |
| Sleep retains playback for wake; disconnect clears/stops it | ff-player/src/services/CanvasService.ts:1398–1402,1497–1540 | Host owns durable sleeping/stopped flags and independent selection | Core tests; external adapters still pending |
| One persisted scheduled playlist, including execution after restart | ff-player/src/services/DP1ScheduleService.ts:31–162 | Host owns execution; redesigned to persist terminal failures, use absolute epoch time and never fire a second early | scheduling-tests: policy rejection, storage retry, restart; timezone conversion tested by importer |
| Failed artwork leaves a usable explanation; renderer retries bounded independently of process escalation | ff-player/src/context/AppContext.reconnectRecovery.test.tsx; ffos-user/feral-watchdog recovery ownership | Deliberately redesigned: host retries three fresh occurrences at 1/2/4 seconds, then leaves failure visible; systemd/watchdog remain process owners | recovery-tests: late callbacks, crash loops, sleep/stop cancellation, healthy-budget reset; device retry test pending |

## Setup narration migration detail

| Existing behavior | Source evidence | New owner / disposition | Verification |
|---|---|---|---|
| Wi-Fi join QR, escaped SSID/password, attached phone switches to portal QR; invalid portal keeps join QR | ff-player/src/components/setup/SetupOverlay.tsx:51–216 | Preserved by host `overlay_panel`; controld supplies credentials/phase | overlay-tests; exact QR payload decode at four output sizes and native compositor (DEVICE-UI.md) |
| Claim QR, mDNS device-name guidance, conditional URL/name refresh | SetupOverlay.tsx:418–453; controld/setupui refresh methods | Preserved: controld owns claim token, native UI encodes/displays it | Go ownership/refresh tests, C++ admission; native panel/copy and synthetic QR decode (DEVICE-UI.md) |
| Scanning, joining, finalizing, neutral connecting, join/setup failure, update and reset narration | SetupOverlay.tsx:224–411 | Native copy; ready/hidden clear only setup owner | overlay-tests; unknown state rejects and retains previous QR |
| Narration never stalls provisioning; stale sweep/conditional hide/reconnect replay do not erase newer owner | controld/setupui and overlay/controller.go | Preserved arbitration; socket delivery and host snapshot watch replace CDP navigation park | retained Go race regressions and native reconnect/status tests |
| Old-player manifest downgrade/no-op rules | controld/setupui prior implementation | Removed: coordinated native runtime requires `overlay.setup.v1`; unsupported is an explicit failure | Go capability check and shared unknown-state fixture |
| Animated setup artwork backdrop and exit transition | SetupArtworkBackground.tsx; SetupOverlay.tsx:507+ | Original bundled artwork in isolated decorative renderer; live artwork retained under panels; 650 ms fade | Vulkan compositor and covered-media tests (DEVICE-UI.md); offline-degraded no-panel chip pending |

| Playback detail | Source evidence | New owner / disposition | Verification |
|---|---|---|---|
| Fresh Now Display resets loop to playlist and shuffle off | CanvasService.ts:2099–2115 | Preserved in host replacement transaction | modes-tests |
| Shuffle keeps current slot first, shuffles remaining slots, and disabling restores document order | CanvasService.ts:2176–2238 | Host preserves document and durable order separately, avoiding URL identity collisions | modes-tests, restart tests; native controller translation and FF1 loop/shuffle checks |
| Refresh retains the currently presented work; if removed, defer replacement until its slot ends | CanvasService.ts:2285–2394 | Preserved in typed `playback.update`; host owns durable pending replacement and boundary | playlist-update-tests: duplicate slots, restart, write failure, unchanged shuffle; Go/C++ interoperability; external refresher migration pending |
| Fresh content labels retire blocked selected/outgoing work immediately; refresh cannot widen curated to personal | CanvasService.ts:1942–2040 | Preserved; explicit cast identity keeps retirement correct across successive refreshes; rewritten projections lose signatures | playlist-update-tests and independent review reproduction; device check pending |
| Saved framing vs ephemeral showing-scoped display changes with accepted composition revision | CanvasService.ts:1670–1716 | Host presentation.set validates explicit occurrence and userOverrides; saved fallbacks and frame-fenced composition revisions | display-tests, Go mapping, device matte/paused-media checks; external phone adapter pending |
| Rotation transforms the Cage output; persisted perceived orientation survives hotplug | controld/devicectl/executor.go:2051–2175; start-kiosk.sh:3–15; display-restore.sh | Controld retains output-device operation; native host receives logical resize and maps input in those coordinates. Avoid a second content rotation | Native resize path exists; rotated/high-DPI hardware tests pending; internal rotation-setting ownership must be resolved before adapter activation |

## 2026-11-18 normal control cutover

| Existing behavior | Source evidence | New owner/disposition | Verification |
|---|---|---|---|
| Normal displayPlaylist and startItem | commandrouter/handler.go, playercontrol/native.go | Controld admission, host selection; preserved external envelope | Signed FF-CLI Social Codes and holz; typed adapter tests |
| displayAt and schedule/boot intent | playlistschedule/scheduler.go, player/src/playback.cpp | Redesigned: host timer/persistence only; Go no longer executes cohorts | Scheduling regression suite, boot-ID, deferred supersession, storage lock |
| Source refresh | playlist-refresher/refresher.go | Controld fetch/trust, native atomic update; no recast on unchanged items | Stale generation/outgoing presentation and timed refresh tests |
| Reconnect after player failure | playerlink/reconcile.go, main.go | Runtime-instance replay of device intent; removed page-navigation recovery | FF1 controld restart retains exact occurrence; KILL/STOP restore playlist |
| Watchdog events | feral-watchdog/player_health.go, cpu.go, systemd_service.go | Typed transient health levels; removed browser navigation | Host STOP recovery, thermal hold/release, race/policy tests |
| Screenshot | screenshot/native.go, hub/control.go | Compositor capture; enrolled-device protection preserved | Native capture 607×1080; enrolled HTTP endpoint refuses with 403 |
| Readiness and OOM | player-ready-check.sh, ffos earlyoom-hook.sh | Native command-loop readiness; native process accounting | Active target, startup contract tests; OOM names inspected |

Device admission/signature checks remain in controld. No renderer may supply
trusted annotations. Offline resource replay/onchfs and full portfolio acceptance
remain gaps; see CUTOVER.md rather than interpreting this table as completion.
