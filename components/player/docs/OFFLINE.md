# Offline playback

The player owns disposable browser and libmpv media caches under
`~/.cache/player-artworks`. The shared limits default to 30 days and 1 GiB.
Local files and captured artwork bypass these caches. Controld retains ownership
of explicit downloads, automatic capture, content-addressed blobs and garbage
collection in its existing offline store. The player reads that store directly.

The host binds replay to the current artwork manifest. Explicit captures run in
a network-isolated worker. Automatic captures may use eligible fresh responses
and fall back to live artwork requests under the current network policy.
Legacy media blobs remain playable. A downloaded HTML capture that lacks the
original response headers requires recapture before its scripts can execute.

Small browser responses travel over the private engine pipe. For a large GET,
the worker receives read-only mounts for this artwork's eligible blobs and
serves the original URL through a CEF resource handler. The handler checks the
digest of the open file before streaming and retains the captured response
headers. The browser applies its normal origin and response policies. Native
media receives a read-only descriptor. No cache broker runs in controld.

See [the cutover mapping](MIGRATION.md) for ownership of the former kiosk CDP
consumers.

