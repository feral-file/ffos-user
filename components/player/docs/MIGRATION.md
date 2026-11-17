# Storage migration and rollback

The importer is implemented and tested independently of service activation.
The full-image migration is **not ready to activate**: native fallback,
boot execution, all controld consumers, and unit integration remain unfinished.

The legacy source is `FeralFileDisplayDB/keyValueStore`, with the old player's
localStorage fallback. `tools/legacy-export.mjs` reads those through a running
player, never through browser database files. Version 2 also reads the player's
live route, settled boot-hydration state and cast status before and after the
storage read. A cast that changes during export is rejected. Activation must
quiesce external cast writers while taking the final export, then stop the old
player before starting the new one. Export alone is not a cutover lock.

```sh
node tools/legacy-export.mjs http://127.0.0.1:9222 /private/path/legacy.json
python3 tools/migrate-legacy.py /private/path/legacy.json \
  /home/feralfile/.state/ff-player-native \
  --validator /usr/lib/ff-player-native/ff-player-import
```

Use an SSH tunnel for remote export; the exporter accepts only loopback CDP
endpoints. `--screenshot` optionally captures renderer output separately.
Screenshot failure does not invalidate an already written storage export.
Neither that screenshot nor a successful import proves physical display output.

The destination must not exist. The converter creates a private sibling staging
directory, normalizes the export, and invokes the C++ state validator/importer.
The C++ executable creates a SQLite transaction and exercises the same load
validation as the host. Files and directories are synced, then the complete
directory is published with Linux `renameat2(RENAME_NOREPLACE)` (macOS
`renamex_np(RENAME_EXCL)` for local tests). A concurrent creator wins without
being overwritten. Failed validation or writes remove staging and leave the
existing native/legacy state intact. An fsync error after publication is reported
as failure; retain and inspect the published directory rather than retrying over
it. The tool never overwrites a destination, even when called repeatedly.

The directory retains `legacy-export.json` verbatim and `migration.json` with
the source SHA-256, export time and conversion notes. Both are private mode 0600;
the directory is 0700. Do not upload these files in ordinary diagnostics: URLs,
playlists and metadata may be private. The original Chromium profile remains
untouched and is not copied into CEF.

| Legacy value | Native treatment |
|---|---|
| `castInfo` playlist/index/context/loop/shuffle/pause | Preserve playlist document and selected index; regenerate an order because the old player did not persist shuffle order |
| Live `/sleep` | Durable sleep; retained playlist does not resume on import |
| Live `/error` or cleared hydration halt | Durable stopped intent; preserve the stored playlist for an explicit later resume |
| Missing cast | Stopped/empty, never a fabricated active playlist |
| `displaySettings` | Scaling, framing and label mode; discard transport-only `isSaved` |
| `defaultItemDuration` | Positive seconds or absent auto mode |
| `viewMode` | Provenance only: `useDeviceRotation.ts` derives it from viewport width/height; it is not a rotation request |
| `contentPolicy` | Exact typed durable mirror; malformed policy aborts import rather than resetting it |
| `recentlyPlayed`, incomplete marker | Preserve opaque IDs, item/defaults/context and timestamps; no active occurrence is restored |
| `boot_playlist` | Preserve bare legacy or playlist/context envelope; bare legacy is curated |
| `dp1_scheduled_tasks` | Preserve the one stored DP1 task and timezone-qualified due time |
| `renderStatus`, showing key, timing counters | Deliberately not imported; new host must observe new rendering |
| Verification annotations | Not imported; controld must rebind trusted results to the new instance/generation |
| Thermal/daily fallback marker | Currently abort migration explicitly until native fallback is implemented |
| Old per-edge/asset display settings | Currently abort rather than silently discard values without a native mapping |

Corruption, unknown export versions, unsettled hydration, a live/durable cast
mismatch, unsupported fields, oversized input and duplicate JSON keys are errors.
A version-1 export lacks live stop/sleep evidence and must be re-exported.

Rollback switches the matching old player, controld and session units together
through the full-image rail. Stop the native host first and retain its state
directory for investigation. Restart the previous installation against its
original Chromium profile. Do not copy native SQLite into browser storage or
silently rewrite old storage from the native state. This rollback restores the
pre-cutover snapshot; playback/preferences changed after cutover will not be
backported automatically. Coordinated activation/rollback scripts and an actual
full-image rollback test remain acceptance work.

`tests/migration_test.py` invokes the real C++ validator and covers retained
intent/settings/history, malformed records, existing destinations, publication
races and incomplete exports. Homelab's actual version-2 export imported into an
isolated macOS state directory and a separate homelab state directory. Two core
host starts on homelab retained 100 playlist items, index 49, 50 history records
and saved intent/settings with distinct runtime IDs and no presented occurrence.
See `evidence/homelab-import-device-restart.json`. This is a storage/restart check,
not activation or visual portfolio acceptance.
