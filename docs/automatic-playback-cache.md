# Automatic playback cache

Normal playback should warm persistent artwork storage without a Download
command. The existing offline store, guarded downloader, admission gate and
replay session already provide the necessary device boundaries.

## Execution plan

1. Current behavior: only explicit downloads populate the store. Eviction uses
   capture time, and a fixed byte ceiling does not reserve filesystem space.
   Office's five Coral images replay quickly once downloaded (31.5 MB total).
2. Preserve signed source URLs, manual offline downloads, clear/capture fencing,
   bounded background work, resource admission and the playback-generation
   fence. Capture must never navigate the kiosk or delay a cast acknowledgement.
3. Risks: caching mutable content forever, prefetch competing with playback,
   repeating downloads for a playlist larger than storage, and unrelated disk
   writers consuming the remaining space during a transfer.
4. Extending the browser's trusted-origin list only fixes selected hosts.
   Automatically snapshotting every software artwork would freeze live data.
   Reuse the device store for fresh, single-file media; leave software and live
   requests on the normal network path unless explicitly downloaded offline.
5. Test before rollout: repeated playback and daemon restart reuse stored bytes;
   current/next ordering; expiry and no-store rejection; bounded retries; clear
   and playlist-switch races; percentage/absolute/free-space limits; streaming
   writes abort under disk pressure; recently played entries survive eviction.
   Run the controld module tests, race checks on changed packages, vet and lint.
6. Rollout: run a reversible Office development trial and review a PR to develop,
   preserving its owner-Tailscale build. Compare repeated-loop bytes and visible
   Loading against the recorded baseline, restore the original playlist and
   close diagnostic SSH. Production promotion remains on the normal release rail.

## Policy

With offline caching enabled, automatic playback caching is enabled by default.
`offlineCache.automaticPlaybackDisabled` opts out. The shared store is limited to
50% of its filesystem (`maxDiskPercent`), additionally capped by a positive
`maxDiskBytes`. Keep the larger of 5 GiB (`minFreeDiskBytes`) or 10% of the
filesystem free. Recheck space while streaming; a cache failure must leave live
playback available. These are ceilings, not preallocated files.

Cache current and upcoming items from the player's actual active item list,
with bounded work and backoff. Discard expired automatic records first. Warming
may reclaim least recently played automatic items outside the active playlist,
but preserves explicit offline snapshots and fresh upcoming works. Hard storage
ceilings may reclaim either kind when necessary to keep the filesystem reserve.
Expired records do not receive playback-recency updates. Usable entries
evicted under disk pressure are not downloaded again in that same observed
playlist. Refused and oversized sources stay suppressed until a different
playlist is observed; transient failures keep bounded retries. Persist playback
recency across daemon restarts. An explicit clear suppresses automatic
repopulation for the current playback session, including temporary status
failures or reconnects. Existing explicit downloads retain their offline
snapshot semantics. Deferred software downloads do not block admissible media.

Speculative attempts do not emit queued/downloading/failure notifications or
leave failed entries in whole-store status. Eviction removes records from that
listing; explicit source queries and eviction notifications still say not_cached.
Successful captures publish their cached state;
an explicit Download joining an automatic attempt retains manual semantics.

Automatic entries require an explicit HTTP freshness lifetime. Honor no-store,
no-cache, private, Age and expiry, and decline request-dependent responses that
the shared source-keyed store cannot represent. Expired entries pass through to
the origin, even before scope refresh. HTML/JS software, streams, responses reached
through redirects and ambiguous types are not automatically snapshotted. Native browser caching still applies
to their network requests. Missing/expired automatic files never block a live
request under the explicit offline cache's fail-closed setting.

Automatic records use a version-1 envelope at the existing item-record path.
Pre-feature binaries fail their existing source-identity check and quarantine
these records, making downgrade a cold automatic cache. Explicit downloads and
clears still use that same path, so no hidden record can resurrect a cleared
item. Manual records remain in the existing bare format.
