# feral-display

These instructions apply to `components/feral-display/`. The player takes admitted artwork and playback intent from controld, renders it under Cage, and reports what reached the display. Keep device enrollment, controller authentication, provisioning and system-wide recovery in their owning services.

**Critical: one FF1 display path.** Live, downloaded, and signed capsule artwork use the same player. CEF renders web works, and libmpv renders standalone media. Capsule engine pins are provenance for off-device reproduction. Never install or launch a second display engine for a work.

## Where a change belongs

| Responsibility | Starting point | Ownership rule |
| --- | --- | --- |
| Process startup and wiring | `src/player_main.cpp`, `src/public_main.cpp` | Ship one project executable. Select the host or sandboxed worker role at startup and load its supporting library. Keep build and diagnostic tools outside the installed runtime. |
| Controld commands, replies and events | `src/server.cpp`, `src/protocol.cpp`, `protocol/` | The Unix socket is the control boundary. The protocol owns negotiation, idempotency and stale-command checks. |
| Playback lifecycle | `src/playback.cpp`, `src/playlist_update.cpp`, `src/store.cpp` | Playback owns selected and presented occurrences, scheduling and recovery. Store owns SQLite. Save durable intent before publishing the resulting state. |
| Display decisions | `src/display_policy.cpp`, `src/display.cpp` | Resolve device settings, authored layers and temporary adjustments in the core. Give renderers one effective display description. |
| Rendering and presentation | `src/public_browser.cpp`, `src/media_client.cpp`, `src/presentation.cpp` | CEF and libmpv produce frames and observations. The host owns composition, input routing and the decision to report presentation. |
| Artwork access | `src/sandbox.cpp`, `egress/`, `src/artwork_cache.cpp`, `src/cache_client.cpp` | Preserve worker isolation and network grants. Remote CEF and eligible media share a disposable cache budget of 30 days and 1 GiB by default. Local inputs bypass it. Controld owns its separate downloaded bundles. |

## Coding rules

- Keep decisions that can be computed from values in `ff_core`. Pass the needed settings, occurrence or time explicitly. A test for display precedence should run with plain values and no SQLite, socket, graphics device or clock.
- Keep effects at the edge. Have the host apply a decision and own its resources. Extend an existing adapter when introducing renderer-specific behavior. Add an interface when there are distinct implementations or a concrete testing need.
- Keep selection, frame readiness and actual composition distinct. Fence asynchronous results by occurrence and by display revision where relevant. Two adjacent items can have the same URL. Keep the outgoing artwork visible until its replacement can be presented.
- Preserve the wire representation, error codes and existing stored state when moving code. Keep one implementation of each policy. Give locals names that indicate ownership or units, and explain ordering when changing it could alter output.
- Bound queues and asynchronous cache work. Preserve cancellation and the online/offline distinction. Slow storage and cache admission must allow live playback to progress while offline replay continues to enforce network isolation.

## Verification

Run these commands from this directory for playback and display changes. On a fresh checkout, fetch the pinned core headers first with `python3 tools/fetch-dependencies.py --without-cef`.

```sh
cmake -S . -B build-core
cmake --build build-core --parallel
ctest --test-dir build-core --output-on-failure
```

Test pure decisions directly, then cover the observable effect through Playback or the socket. Use a temporary real Store for persistence and restart behavior. For socket changes, run controld's `playerlink` tests with `FF_NATIVE_CORE` pointing to the absolute path of the freshly built `ff-player-core`. For renderer, isolation or packaging changes, use the Linux `tools/build-ff1.sh` path from the Build Native Player workflow. Verify device-specific claims with actual frames, the active decoder or worker namespace as appropriate.

The [migration guide](docs/MIGRATION.md) maps the external service boundaries. The [socket contract](protocol/README.md) defines what callers can observe.
