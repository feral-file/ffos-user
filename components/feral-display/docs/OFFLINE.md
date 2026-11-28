# Offline playback

The player owns disposable browser and libmpv media caches under
`~/.cache/player-artworks`. The shared limits default to 30 days and 1 GiB.
Local files and captured artwork bypass these caches. Controld retains ownership
of explicit downloads, automatic capture, content-addressed blobs and garbage
collection in its existing offline store. The player reads that store directly.

The host binds replay to the current artwork manifest. Explicit captures run in
a network-isolated worker. Automatic captures may use eligible fresh responses
and fall back to live artwork requests under the current network policy.
Legacy media blobs remain playable. When an older website download lacks original
response headers, an online cast opens the website through the normal live path.
Offline playback requires a fresh download. Signed capsules and virtual origins
retain their network isolation.
Unsupported writes remain coverage misses. A missing analytics POST does not invalidate a
captured GET. The downloader uses software WebGPU to discover work dependencies. When a
dedicated worker must remain paused, its entry script can be fetched independently through
the guarded HTTP client, with the headers and digest from that response. The download
remains partial because requests inside that worker were unobserved. An explicit download
can preserve works that the automatic cache declines under the origin’s freshness policy.

An occurrence pins its capture manifest and holds temporary hardlinks to the available blobs.
Retiring the occurrence releases those links. An atomic recapture takes effect on
the next play. Mixed online playback fetches large full GETs live so a cache
miss always reaches the selected gateway.

Small browser responses travel over the private engine pipe. For a large GET,
the worker receives read-only mounts for this artwork's eligible blobs and
serves the original URL through a CEF resource handler. The handler checks the
digest of the open file before streaming and retains the captured response
headers. The browser applies its normal origin and response policies. Native
media receives a read-only descriptor. No cache broker runs in controld.

See [the cutover mapping](MIGRATION.md) for ownership of the former kiosk CDP
consumers.

