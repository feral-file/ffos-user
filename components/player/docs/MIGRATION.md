# FF1 native player cutover

The kiosk unit starts Cage and a single project executable at
`/usr/lib/ff-player-native/ff-player-native`. A live change requires the image's
user session files together with the player, controld and watchdog binaries.
Pacman replaces the old web-player package. The new host resumes an existing
native SQLite database in `~/.state/player`. During a legacy OTA, candidate promotion reads the shut-down production root's kiosk
origin with a temporary, network-isolated Chromium storage reader. As the kiosk
user, it imports the saved cast, index, modes, content policy, display preferences, boot playlist and pending schedule
through the native player's validators into a staged SQLite database. An import
failure reboots into the previous root before boot files or the default root change. An active import requires
fresh presentation evidence for that playlist and artwork in the post-OTA success check. The rollback root
and the original Chromium profile retain their previous data.

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
