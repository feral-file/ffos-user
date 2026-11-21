# FF1 native player cutover

The kiosk unit starts Cage and a single project executable at
`/usr/lib/ff-player-native/ff-player-native`. A live change requires the image's
user session files together with the player, controld and watchdog binaries.
Pacman replaces the old web-player package. The new host resumes an existing
native SQLite database in `~/.state/player`. A device with only the legacy
Chromium profile starts with a fresh native database. The profile can be kept
with the rollback backup.

## Former kiosk CDP consumers

| Responsibility | Native path |
|---|---|
| Cast, playlist refresh, pause, sleep, schedule, input and settings | Controld translates controller requests into typed, fenced Unix socket commands. The host replies after applying or accepting an operation. |
| Status, playlist history, playback outcomes and device UA | Snapshots, history reads and unsolicited events on the same socket. A new player instance triggers reconciliation of device-owned intent. |
| Pairing and setup narration | Host-owned overlays survive artist document navigation. |
| Liveness and recovery | Socket handshake and health reads serve readiness and watchdog. Systemd owns process restarts. |
| Routing and request headers | Controld sends operator gateway settings over the socket and attaches the media-health store's observed fallback to a cast. The player applies routing inside browser and standalone media playback. |
| Offline capture and replay | Controld retains its independent headless capture browser and capture store. The host reads local manifests and verified blobs. Older HTML captures without original response headers need recapture. |
| Screenshots and wall observations | The existing screenshot service captures compositor output. The LAN hub applies the device enrollment gate. |
| Page navigation and JavaScript handler readiness | Player instance and occurrence identifiers replace page generations. The player manages CEF through private pipes within the artwork process tree. |

The browser HTTP cache and standalone media cache share the player's retention
budget under `~/.cache/player-artworks`. This directory is separate from
controld's downloaded artwork store. Save the prior session units, runtime,
configuration, package list and a consistent SQLite backup before an in-place
trial. Restore those files together if playback fails.
