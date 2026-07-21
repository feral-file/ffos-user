# Capturing Web-Based Artwork for Offline Playback

This document describes how `feral-controld` captures a web-based
(HTML+CSS+JS, canvas, WebGL, WASM) DP-1 artwork so `ff-player` can play it
back with no network access, and how the result is stored and replayed.

**Status:** implemented in `components/feral-controld/offlinecache/`. This
document is the living reference for that package; the original design
draft was validated against real, live DP-1 feed content before
implementation (§4 below) and this text has been updated to match what was
actually shipped, including edge cases the draft did not anticipate.

---

## 1. Problem statement

A DP-1 playlist item's `source` is a single URL to an HTML entry point:

```json
{ "source": "https://cdn.example.com/work/index.html" }
```

Unlike single-file media (image/video/audio), that entry point can load an
arbitrary, **unenumerable** set of dependencies: scripts, stylesheets, fonts,
JSON/binary data, WASM modules, Web Worker scripts, and further resources
computed at runtime rather than declared statically in the HTML.

There is no manifest listing "every file this artwork needs" in the common
case (DP-1's `repro.assetsSHA256` is a hash list for *verification*, not a
fetchable manifest, and is optional). To cache such a work offline, the only
reliable method is to **run the code and observe what it actually
requests**, not to statically parse its HTML/CSS for references. Offline
caching in `feral-controld` is therefore restricted to **software-based**
items (`classify.go` distinguishes software from media by resolved
`Content-Type`); media items (video/image/audio) do not need this pipeline
at all and are rejected by the download commands.

## 2. Pipeline overview

```
Discover  →  Capture  →  Store  →  Replay
(what does   (fetch the   (content-  (serve
 it load?)    exact        addressed   locally,
              bytes)       dedup)      no network)
```

- **Discover + Capture**: `downloader.go` spawns a separate headless
  Chromium (`:9223`, its own user-data-dir); `capture.go` attaches an
  event-driven CDP session (`cdpsession.go`) to observe `Network` domain
  events for a bounded window, then fetches each observed URL's exact
  bytes out-of-band.
- **Store**: `store.go` content-addresses the bytes (sha256) into a shared
  blob store, deduplicated across items/playlists, plus one
  `items/<itemId>.json` record per edition.
- **Replay**: `replay.go` intercepts `Fetch.requestPaused` on the kiosk
  Chromium (`:9222`) and fulfills from the local blob store or a loopback
  static server, without rewriting a single byte of the artwork's own code
  or the signed playlist's `source` field.

Both browsers stay independent: the headless downloader (`:9223`) never
shares state with the kiosk (`:9222`), so downloading does not disturb
whatever is currently playing on the player surface.

---

## 3. Discovery and capture technique

### 3.1 Why static HTML/CSS parsing is insufficient alone

Parsing `<script src>`, `<link href>`, CSS `url(...)`/`@import` catches the
*declared* dependency graph only. It misses `fetch()`/`XMLHttpRequest` calls
to runtime-computed URLs, dynamic `import()`, WASM modules that fetch
further data based on internal logic, Web Worker-initiated requests,
absolute cross-origin URLs baked into JS string literals, and
randomized/branching behavior. `feral-controld` does not attempt static
parsing; it always runs the code and observes.

### 3.2 CDP `Network` domain — the chosen capture mechanism

`capture.go` enables the CDP `Network` and `Page` domains (not `Fetch` —
`Fetch` interception is what *replay* uses, §6) and observes
`Network.requestWillBeSent`/`responseReceived`/`loadingFailed` for a
bounded capture window (`offlineCache.captureWindowMs`) after navigating to
the item's `source`. For each distinct URL:

- **Found on a successful (2xx) response, captured via a safe method
  (`GET`/`HEAD`/`OPTIONS`)** → fetch the exact bytes via an out-of-band
  `http.Client` request using that SAME method (see §4 for why not
  `Network.getResponseBody`, and §4.7 for why method matters at all),
  hash into the blob store, record the resource. The response body is
  streamed straight from the HTTP
  connection into a temp file on disk while it is hashed —
  `store.WriteBlob` never buffers a whole body in memory first — since a
  single resource can be gigabyte-scale (§4.3's 1.1 GB video) and
  `feral-controld` runs alongside a kiosk Chromium already under its own
  memory pressure on a constrained device. `WriteBlob`'s `maxBytes`
  parameter additionally aborts the stream (before the oversized body
  ever fully lands on disk) if a single resource alone would already
  exceed `offlineCache.maxDiskBytes` — `capturer`'s `maxResourceBytes`
  passes that same config value through, so one pathological resource
  cannot silently blow past the whole cache's disk budget before
  `Service.enforceDiskLimit`'s post-capture eviction loop ever runs (that
  loop can only reclaim space by deleting *other* items, which cannot
  help if one resource alone already exceeds the entire budget).
- **3xx redirect** → record `status` + `redirectTo` (the `Location`
  header), no body. See §4.1.
- **206 Partial Content** → CDP cannot return a body for a range response
  either; `capture.go` falls back to an out-of-band full-URL fetch (no
  `Range` header), so the stored blob is always the complete asset
  regardless of what the browser's own request happened to ask for. See
  §4.4.
- **`blob:`/`data:` URL** → excluded entirely, never written to the index.
  See §4.2.
- **`Network.loadingFailed`** → recorded so "never requested" can be
  distinguished from "requested and failed" in `Coverage.Reason` (as
  `loading_failed(<errorText>):<url>`, or `csp_blocked` for the CSP case in
  §4.5).
- **Successful (2xx) but an unsafe method** (`POST`/`PUT`/`PATCH`/`DELETE`/
  ...) → left unfetched and recorded as `unsupported_method(<method>):<url>`
  instead. See §4.7.
- **Non-2xx and not a valid redirect** (a 4xx/5xx, or a 304 the page's own
  request observed via HTTP cache revalidation) → recorded as
  `http_error(<status>):<url>`. See §4.7.

`downloader.go` runs one capture job at a time and tears the headless
Chromium down when idle — the device already carries OOM pressure from the
kiosk Chromium, so a second one is not left resident. `Downloader.Close()`
also tears it down immediately and rejects further jobs; `capturer.Close()`
delegates to it, and `Service.Stop()` calls that once the worker goroutine
has fully exited, so daemon shutdown (`main.go`) reaches this second
Chromium process without needing its own direct handle to the downloader
(it is private to `bootstrap.go`'s wiring).

Tearing down and starting a replacement both target the SAME fixed
`--remote-debugging-port`/`--user-data-dir`, so `Acquire` must never start
a replacement while a prior process is still exiting: the two could
momentarily race for that port/profile lock, and `waitForDebugEndpoint`
could observe the OLD (dying) process's endpoint as "ready" instead of
the new one's. `downloader.go` closes this with a generation-tracked
`procDone` channel that `stopLocked` deliberately leaves set (only
`cmd`/`procCancel` are cleared) for the whole window between "kill signal
sent" and "the reaper's `cmd.Wait()` actually returned" — `Acquire` waits
on that channel before deciding whether to start anything, and the reaper
itself only clears shared state if no newer generation has since replaced
it (an identity check, guarding against this goroutine waking up late and
clobbering a replacement it knows nothing about).

## 4. Validated edge cases

The pipeline was validated end-to-end against real, live DP-1 feed content
(every publisher channel in `https://feed.feralfile.com/api/v1/registry/channels`,
17 deliberately hard items sampled for technical diversity: on-chain
hash-in-URL items, `<model-viewer>` WebGL + external CDN + `.glb`/HDR
assets, Web Audio DSP runtimes, pre-rendered-variant patterns, and a
different publisher/CDN). Result: 15/17 fully byte-for-byte playable
offline with zero network egress; the other 2 surfaced the specific,
previously-undocumented findings below rather than generic failures. Full
validation artifacts: `tmp/dp1-offline-capture/` (`discover.mjs`,
`select.mjs`, `pipeline.mjs`, `report.md`) — a standalone Node/Playwright
harness, not part of the shipped Go implementation, used only to validate
the design before porting it into `capture.go`/`replay.go`.

### 4.1 Redirects need their own schema, not a null-body resource

Two sample items loaded an *unversioned* "latest" CDN URL
(`https://unpkg.com/@google/model-viewer/dist/model-viewer.min.js`) that
Chromium resolves via HTTP 302 to a versioned URL. CDP's
`Network.getResponseBody` cannot return a body for a redirect response at
all (`Response body is unavailable for redirect responses`), so treating a
redirect as "a resource with a missing blob" reports a false cache miss
even though the real target was captured perfectly. `types.go`'s
`Resource.RedirectTo` + `Resource.IsRedirect()` and `replay.go` fix this:
capture records the `Location` header and status per hop; replay fulfills
the redirect itself (status + `Location` header, no body) and lets the
browser naturally re-request the already-cached resolved URL, which is
captured and cached as its own `Resource` entry.

### 4.2 `blob:`/`data:` URLs are not cacheable dependencies

A common pattern for glTF/WebGL loaders is to re-slice an already-downloaded
binary into `URL.createObjectURL()` object URLs for internal use. These are
**not real network requests** — they are single-session, freshly-minted
UUIDs on every page load and can never be looked up by URL on a later load.
`capture.go` excludes `blob:`/`data:` URLs from the resource index entirely
rather than recording them as same-class resources; no replay-side handling
is needed since Chromium never routes them through `Fetch` interception
either.

### 4.3 A single asset over ~400 MB breaks `Fetch.fulfillRequest` itself

One sample item's background video (a 1.1 GB `.mp4`, requested as an HTTP
206 range request) was captured correctly — all ~1.1 GB streamed to disk
and hashed on the fly (see §3.2; capture never buffers the whole body in
memory) — but fulfilling it via `Fetch.fulfillRequest` failed with
`Cannot create a string longer than 0x1fffffe8 characters`. This is a hard
ceiling in Chromium DevTools Protocol's `Fetch.fulfillRequest`: the response
body is transmitted as a base64 **string**, and V8 caps string length at
`0x1fffffe8` (~536,870,904) characters — a 1.1 GB video base64-encodes to
~1.46 GB of text, over the ceiling by ~3x. This is why replay (§6) is a
**hybrid**, not `Fetch.fulfillRequest`-only: `staticserver.go` streams any
blob over a 200 MB threshold (comfortably below the actual ~400-536 MB V8
ceiling) from a loopback `http.Server` instead, and `replay.go` redirects
large assets to it. Because the large asset is still fully captured and
servable (just through a different path), this alone does not mark the
item's `Coverage.Complete` false; the capturer records incompleteness
only for genuine failures — see §4.5's `csp_blocked`, §4.7's
`http_error(...)`/`unsupported_method(...)`, and the free-text
`loading_failed(...)`/`fetch_failed:...`/`unresolved_at_deadline:...`
reasons `capture.go` actually emits (`types.go`'s `Coverage.Reason` doc
comment and `docs/controld-inbound-controller-messages.md`'s
`getOfflineCacheStatus` section are the two places these are enumerated;
keep both in sync with what `capture.go` actually produces rather than
adding aspirational/unused reason constants — a prior revision briefly
carried `large_asset_static`/`capture_window_elapsed`/`download_failed`
constants for exactly this kind of future signal, and they went stale
and unreferenced immediately, which is why they were removed rather than
kept "for later").

### 4.4 206 Partial Content also has no CDP body

The same 1.1 GB video is requested by the `<video>` element as an HTTP 206
range request, and CDP cannot return a body for a 206 any more than for a
3xx. `capture.go`'s fallback is the same as for redirects at the transport
level: it issues a supplementary out-of-band fetch of the same URL
(ignoring the original `Range` header) to obtain the full body once. The
stored `Resource.Status` still records the originally-observed `206` (it
is not rewritten at capture time), but the blob itself is always the
complete asset — capture never has a partial body to store.

Replay's two paths handle that stored `206` differently, and only one of
them is a genuine `Range` response:

- **Over `largeAssetThreshold` (§6, e.g. this 1.1 GB video)** → redirected
  to `staticserver.go`'s loopback server, which serves the blob through
  `http.ServeContent` and so honors any `Range` header on the *replayed*
  request with a real `206`/`Content-Range` response.
- **Under `largeAssetThreshold`** → fulfilled inline via
  `Fetch.fulfillRequest` with the complete blob body. Replaying the stored
  `206` verbatim here would be invalid (a `206` without a matching
  `Content-Range` header, body length equal to the *whole* asset rather
  than the requested range) and can break range-aware `<video>`/`<audio>`
  elements; `replay.go`'s `inlineFulfillStatus` normalizes this case to
  `200` instead, which is the honest status for "here is the entire
  asset" — this is the only place capture's observed status is not used
  as-is at replay time.

### 4.5 A CSP-broken-online item is not a caching failure

One sample item rendered blank in **both** the live online load and the
offline replay: the CDN hosting the artifact served a Content-Security-Policy
that blocked the artwork's own first-party script dependency, so the piece
never worked even with live internet access. This is not a caching gap.
`capture.go` records this as `Coverage.Reason` containing `csp_blocked`
(from CDP `Network.loadingFailed`'s `blockedReason=="csp"`); `service.go`'s
`stateFromCoverage` reports the item's `ItemState` as `broken_online`
rather than `partial` when every recorded failure was CSP-related, so a
mobile app does not present "the download failed" when the real finding is
"the piece was already broken publisher-side." A capture with any non-CSP
failure alongside a CSP one keeps the more general `partial` classification,
since a mix of failure types is closer to an ordinary incomplete capture
than a fully broken page.

### 4.6 Cross-origin resources need their CORS headers replayed, not just their bytes

Some artworks load a cross-origin dependency (a CDN-hosted module script
with `crossorigin=""`, a font, or a `fetch()`/XHR request in `cors` mode)
whose response only carries the correct bytes/status *because* the origin
also sent CORS headers (`Access-Control-Allow-Origin` and friends).
Fulfilling such a request offline with the right body but only a
`Content-Type` header — capture's original behavior — replays the bytes
correctly but Chromium's own CORS enforcement then rejects the response
anyway, since it has no way to tell the fulfilled response apart from a
same-origin server that forgot to send CORS headers. The fix threads a
small, curated allowlist of CORS/cross-origin-relevant headers
(`Resource.Headers`, populated by `filterReplayableHeaders`) all the way
from capture through both replay paths — see §5's `Resource.Headers` bullet
and §6's per-path bullets for the mechanics.

### 4.7 A resource's identity is method+URL, not URL alone

A non-`GET`/`HEAD`/`OPTIONS` request (a `POST`/`PUT`/`PATCH`/`DELETE`
call, e.g. an analytics beacon or a stateful API call an artwork happens
to make) and a CORS preflight (`OPTIONS`) both share the SAME URL as
whatever "actual" request they precede or accompany. Two correctness
problems follow if capture and replay only ever key on the URL:

1. **Cross-contamination.** If the URL is captured once as a `GET` (say,
   because some OTHER page load hit it with `GET`) and later a `POST` to
   the identical URL is paused during replay, a URL-only lookup would
   serve the `GET`'s cached bytes for the `POST` — the wrong response for
   a method-sensitive endpoint, with `Coverage.Complete` still reporting
   the item as faithfully cached.
2. **Collision.** An `OPTIONS` preflight response (small/empty body,
   `Access-Control-Allow-*` headers) and its paired actual request's
   response would overwrite each other in a URL-only map, silently
   losing whichever one lost the race to be recorded last.

`types.go`'s `Resource.Method` (empty means `GET`, the overwhelmingly
common case, so the on-disk record stays free of redundant text) and the
shared `resourceKey(method, url)` helper close both: `capture.go`'s
tracker and `replay.go`'s replayer both index resources by that composite
key, never by URL alone, and `Fetch.requestPaused`'s own `request.method`
(not just `request.url`) is what replay matches against.

This does not mean every method is captured faithfully, though.
`resolveResources` only re-issues the byte-fetch for `GET`/`HEAD`/
`OPTIONS` — HTTP's own safe/idempotent methods, which cannot have a
server-side side effect from being re-issued purely to read their bytes.
A successful (2xx) `POST`/`PUT`/`PATCH`/`DELETE`/... response is
deliberately left unfetched and reported as `unsupported_method(<method>):
<url>` in `Coverage.Reason` instead: re-issuing it here to inspect its
bytes would risk re-triggering the exact side effect (a mutation, an
analytics/provenance call) the original request caused, which capture
must never do just to build an offline cache. On replay this resource has
no `SHA256`, so it falls through to the normal miss path (`Fetch.
failRequest` under `fail_closed`, or pass-through under `pass_through`/
mixed scope) — an honest degradation rather than either a side effect or
a silently wrong replay.

A non-2xx, non-redirect response (a 4xx/5xx, or a `304` the page's own
request observed via HTTP cache revalidation) is likewise left unfetched
and reported as `http_error(<status>):<url>`: `fetchAndStoreBody`'s
unconditional re-request cannot reproduce a conditional-request `304`,
and there is no body to faithfully store for most of these short of
capturing the exact live error response — which offline replay would
otherwise need to fulfill from a byte-for-byte error body rather than
just a status code. Recording it as incomplete rather than silently
promising `Coverage.Complete=true` keeps that promise honest; replay
treats it the same honest-miss way described above.

---

## 5. On-disk format (simplified, no redundancy)

Root: `offlineCache.rootDir` (default
`/home/feralfile/.cache/offline-artworks/`).

Design rule: **keep only what replay and status reporting need; derive
everything else.** Three directories, one essential record per item:

```
offline-artworks/
  blobs/<sha256>                      # shared content-addressed bytes (the ONLY place binary payloads live)
  items/<itemId>.json                 # the ONE per-item record (ItemRecord in types.go)
  playlists/<playlistId>.json         # resolved DP-1 playlist (see note below), stored only for whole-playlist downloads
  playlists/by-url/<sha256(url)>.json  # {"playlistId": "..."} pointer, only when DownloadPlaylist was given a sourceURL
```

`items/<itemId>.json` (`ItemRecord`, see `types.go`):

```json
{
  "itemId":  "work-1",
  "item":    { "id": "work-1", "source": "...", "...": "verbatim DP-1 item, source NEVER rewritten" },
  "entry":   "https://host/index.html",
  "resources": [
    { "url": "https://host/app.js",  "status": 200, "sha256": "ab12…", "contentType": "application/javascript" },
    { "url": "https://cdn.example.com/lib.js", "status": 200, "sha256": "cd34…", "contentType": "application/javascript",
      "headers": { "Access-Control-Allow-Origin": "https://host" } },
    { "url": "https://host/mv.min.js", "status": 302, "redirectTo": "https://host/mv@1.2/mv.min.js" }
  ],
  "coverage": { "complete": true, "reason": "" },
  "capturedAt": "2026-07-17T04:55:00Z"
}
```

`playlists/<playlistId>.json` is the playlist as `commandrouter` resolved it
through `dp1.DP1` before calling `Service.DownloadPlaylist` — `dynamicQuery`
items already materialized, all field values (including every item's
`source`) intact and never mutated by `offlinecache` itself. It is **not**
guaranteed to be byte-identical to whatever a publisher originally served:
`dp1` resolution parses the fetched JSON into a Go struct and re-serializes
it, so key order and whitespace can differ from the original wire bytes.
This is safe for DP-1 signature validity because DP-1 signatures verify
against a JCS-canonicalized form of the document (`dp1-go`'s `sign`
package), not raw bytes — see §7's Signing row. If a future caller needs
byte-exact reproduction of the original document for a reason JCS
canonicalization does not cover, that caller must capture the raw fetch
bytes itself before `dp1` resolution; `offlinecache` does not do so today.

There is deliberately **no** top-level manifest, no separate
`capsules/{key}/assets/index.json`, and no `playlists/{id}/items.json`:

- `Store` rebuilds its in-memory index by scanning `items/` + `playlists/`
  at `Service.Start` — no persisted manifest to drift out of sync with disk.
  `main.go` treats a `Start` failure (e.g. the root directory being
  unreadable) as best-effort — it logs and keeps `feral-controld` running
  rather than crashing, since the daemon's core playback/command path
  never depended on this feature before it existed — but that means the
  worker goroutine that drains `s.queue` never starts. `DownloadItem`/
  `DownloadPlaylist` guard against silently queuing into that void with
  `ErrServiceNotStarted` (backed by an atomic flag `Start`/`Stop` set),
  which `commandrouter` surfaces as the same `disabled` RPC error code
  used for the feature being config-disabled entirely (see
  `docs/controld-inbound-controller-messages.md`'s offline-cache section).
  A plain top-of-function `started.Load()` is not enough on its own:
  `Classify` does a network round trip, so a `Stop()` can land in the
  middle of `DownloadItem`/`DownloadPlaylist` after that check passes but
  before the job is actually queued. `enqueue()` re-checks `started`
  immediately before `queue.push`, under the same mutex `Start`/`Stop`
  use to flip the flag, so the push and the flag flip are strictly
  ordered relative to each other — closing the window where a job could
  land in `s.queue` after the worker has already exited and nothing is
  left to drain it. `run()`'s shutdown path additionally drains any
  remaining queued jobs before returning, since Go's `select` does not
  prefer whichever channel became ready first: a job's wake signal and
  the shutdown context's cancellation can both be ready by the time the
  worker's `select` runs, and without an explicit final drain the worker
  could pick the cancellation and exit while a job that already made it
  into the queue sits there unprocessed.
- Ordering and membership for a whole-playlist download already live in the
  verbatim `playlists/<id>.json`'s own `items[]`.
- `playlists/by-url/<sha256(url)>.json` is the one deliberate exception to
  "everything else derives" — it exists purely to make `displayPlaylist`
  by `playlistUrl` work offline (§6), and cannot itself be derived from
  `items/`/`playlists/` scanning since a DP-1 playlist document has no
  self-describing source URL field. It is a small pointer
  (`{"playlistId": "..."}`), not a duplicate of the playlist body, and is
  intentionally not kept in lockstep with `DeletePlaylist`: a stale
  pointer to a since-cleared playlist still fails closed correctly (the
  subsequent `LoadPlaylist` call returns `ErrPlaylistNotFound`), so there
  is no correctness reason to reclaim it eagerly, and `ListPlaylistIDs`
  never sees it since it only matches `*.json` directly inside
  `playlists/`, not this nested subdirectory.
- Only fields replay routing and status reporting actually consume are
  kept on `Resource`: `url`/`status`/`redirectTo` drive replay's
  fulfill-or-redirect decision, `sha256`/`contentType` drive the fulfill
  body. `size` is not persisted — it derives from the blob file's own size
  on disk. Capture-time diagnostics collapse into `Coverage.{Complete,Reason}`
  (free text — `csp_blocked`, `loading_failed(<errorText>):<url>`,
  `fetch_failed:<url>`, `unresolved_at_deadline:<url>`, `http_error(<status>):
  <url>`, `unsupported_method(<method>):<url>`; see §4.3/§4.5/§4.7 and
  `types.go`'s `ReasonCSPBlocked` doc comment) rather than a separate
  `failedRequests` list.
- `Resource.Method` (omitted for `GET`, the common case) is part of this
  resource's identity, not just a descriptive field — see §4.7 for why a
  paused request must only ever be fulfilled from a `Resource` captured
  for the SAME method, never matched on URL alone.
- `Resource.Headers` (omitted entirely when empty) holds a curated
  allowlist of response headers (`types.go`'s
  `replayableResponseHeaders`): `Access-Control-Allow-{Origin,
  Credentials, Methods, Headers}`, `Access-Control-Expose-Headers`,
  `Cross-Origin-{Resource-Policy, Embedder-Policy}`, and
  `Timing-Allow-Origin`. These are exactly the headers Chromium's own
  CORS/cross-origin enforcement checks against a *fetched* resource —
  without them, a captured cross-origin module script, font, or
  `fetch()`/XHR response could replay with the correct status and body
  bytes yet still be rejected by the browser's own CORS check, silently
  breaking offline an artwork that worked fine online. The allowlist
  deliberately excludes hop-by-hop/transport headers
  (`Content-Length`/`Content-Encoding`/`Transfer-Encoding`/`Connection`):
  capture always re-fetches with a plain unranged GET and stores the
  fully-decoded body (`fetchAndStoreBody`), so persisting the *original*
  transfer's framing headers would describe a transfer that never
  actually happens on replay. It also never persists `Set-Cookie` — doing
  so would let an offline replay silently set stale/foreign cookies for
  the artwork's origin. `capture.go`'s `filterReplayableHeaders` applies
  this allowlist (case-insensitively, canonicalizing the header name) to
  both `Network.responseReceived` and each hop's `redirectResponse`, since
  a `cors`-mode `fetch()` CORS-checks every hop of a cross-origin redirect
  chain, not only the final response.
- `WriteBlob` streams into a temp file and renames it into place, the
  same atomicity pattern every other write in this store already uses,
  but two-step: the content-addressed final name is only known once the
  stream has been fully hashed, so the write lands in a uniquely-named
  `blobs/incoming-*.tmp` file first (`os.CreateTemp`, not the fixed
  `.tmp` suffix `writeFileAtomic` uses for item/playlist JSON, since a
  fixed name would collide across writes if this were ever called
  concurrently — today it never is, since captures are serialized to one
  at a time) and is renamed into place only after a successful, complete
  write; any early return (oversized body, I/O error) removes the temp
  file instead. `GC()` already skips any `*.tmp` name so it never treats
  one as an orphan blob, but a write interrupted by the process dying
  mid-stream (SIGKILL, power loss — not a handled Go error, so the
  `defer`-based cleanup above never runs) leaves a temp file GC will
  never remove either, since its random suffix is never reused by a
  later write. This is an accepted, narrow trade-off (the same class of
  "unsupervised headless capture can be killed mid-write" risk
  `writeFileAtomic`'s own doc already calls out) rather than adding
  age-based temp-file sweeping just for a crash-only leak.
- Blobs are freed by a **sweep, not a refcount**: `store.go`'s `GC()` walks
  every saved item record's `Resources` to build the "keep" set, then
  deletes any blob not in it. There is no separate reference count kept in
  sync with saves/deletes — the saved item records are already the source
  of truth for what is live.
- That sweep is dangerous to run concurrently with an in-flight capture:
  `capturer.Capture` writes each resource's blob as it observes it and only
  calls `SaveItem` once, at the end, so for the whole capture window there
  can be blobs on disk that no saved record yet references. `GC()` would
  treat those as orphans. `Service` fences this: `ClearItem`/`ClearPlaylist`
  and the disk-limit eviction path all run `GC()` through a shared
  `captureMu` that the active `Capture` call also holds for its whole
  duration, so a clear that races an unrelated in-flight capture simply
  waits for that capture to finish saving before sweeping, rather than
  risking deleting its not-yet-referenced blobs. `ClearItem`/`ClearPlaylist`
  additionally drop any same-item job still sitting in the (single-worker)
  capture queue, so a clear cannot be silently undone by a re-download that
  was merely queued, not yet running. A capture that is already *active*
  for the item(s) being cleared (`state == StateDownloading`) is rejected
  outright with `ErrItemBusy` (RPC `busy`, retryable) instead of being
  allowed to proceed and delete-then-let-GC-run: allowing that case
  through would let the active capture still save a fresh record once it
  finished, making the just-cleared item "legitimately reappear" moments
  later with no signal to the caller. Rejecting the clear is simpler and
  safer than canceling the in-flight capture, which
  would need per-job cancellation threaded through the single-worker
  queue; `ClearPlaylist` checks every item in the playlist for this before
  deleting anything, so the outcome is all-or-nothing rather than leaving
  one item partially cleared.
- `ClearPlaylist`'s deletion sweep itself (each item's `store.DeleteItem`,
  the playlist record's own `DeletePlaylist`, and `GC()`) is best-effort
  *per step* — one bad item, or a `GC()` hiccup, does not stop the rest
  of the sweep from still running — but every failure along the way is
  collected (`errors.Join`) and returned together rather than logged and
  swallowed. This matters because `store.DeleteItem` already treats
  "record does not exist" as success (`Remove`-if-exists), so any error
  it does return here is a genuine deletion failure (permissions, I/O);
  reporting `ok:true` to the caller while an item's record — and
  therefore its blobs — may still be on disk would misreport what the
  call actually did. `commandrouter`'s `offlineCacheErrorResponse` maps
  the aggregated error through its generic `offline_cache_error`
  (retryable) fallback, same as any other unclassified `Service` error.

## 6. Replay: hybrid `Fetch` interception + static-file fallback

`replay.go` attaches to the kiosk Chromium's existing CDP endpoint (`:9222`)
through a second, event-driven CDP session (`cdpsession.go`) — a separate
connection from `feral-controld`'s existing synchronous `cdp.go` client, so
enabling replay never blocks or interferes with normal command handling.

`cdpsession.go`'s `Send` multiplexes many concurrent in-flight calls over
one connection (keyed by request ID), unlike `cdp.go`'s synchronous
client, which serializes one write+read at a time and can safely poison
the whole connection via socket-level read/write deadlines on a stuck
send. That per-connection approach would be wrong here — it would
spuriously fail every other in-flight call sharing the connection, not
just a stuck one — so `Send` instead imposes a 15s ceiling (`defaultSendTimeout`)
per call via `context.WithTimeout`, on top of whatever deadline the
caller's own `ctx` already carries. Several production callers pass a
long-lived daemon/background context with no deadline of its own —
`replay.go`'s per-`Fetch.requestPaused`-event goroutine, `playlist-refresher`'s
periodic `SyncPlaylist` call, and `main.go`'s CDP `onConnect` hook — so
without this internal ceiling, a kiosk DevTools socket that accepts the
write but never replies (a nonresponsive/wedged target) would wedge
`Send`, and therefore its caller, forever; for `playlist-refresher`'s
single-goroutine background loop specifically, that would have stalled
*all* future playlist refreshing, not just offline-cache resync. `DialPageSession`
(`dial.go`) carries the same 15s ceiling (`defaultDialTimeout`) around its
own targets-fetch + websocket-dial sequence, for the same reason: `main.go`'s
`onConnect` hook calls it (via `KioskReplay.AttachOnReconnect`) synchronously
on `cdp.CDP`'s own connect-loop goroutine, so a hung dial there would have
stalled all future reconnect attempts, not just offline replay's own
re-attach.

On every `Fetch.requestPaused` event, keyed on the **exact original URL**
(never a rewritten relative path — this is what makes replay work for
absolute and cross-origin URLs without touching the artwork's own code):

- **Redirect resource** → `Fetch.fulfillRequest` with the recorded status,
  `Location` header, and the resource's captured `Headers` (below), no
  body (§4.1). Headers are threaded through this redirect hop, not only
  its eventual target, because a `cors`-mode `fetch()` CORS-checks every
  hop of a cross-origin redirect chain.
- **Small resource (under 200 MB)** → `Fetch.fulfillRequest` with the
  blob's bytes read from `blobs/<sha256>` directly, plus `Content-Type`
  and any captured `Headers`. `replay.go` uses a 200 MB threshold rather
  than pushing all the way to the ~400 MB ceiling found in §4.3 —
  comfortably under it so the CDP path never gets close to the actual V8
  string-length limit. A stored `206` status is normalized to `200` on
  this path — see §4.4 for why.
- **Large resource (200 MB or over)** → redirect the request to
  `staticserver.go`'s loopback `http.Server`
  (`offlineCache.staticServerAddr`, default `127.0.0.1:8082`), which streams
  the blob (with real `Range` support for the 206 case, §4.4) instead of
  base64-encoding it through CDP. `StaticServer.URLFor` embeds the
  resource's captured `Headers` as repeated `h=Name=Value` query params
  (rather than one JSON blob, to stay eyeball-legible in logs), and
  `handleBlob` echoes them back on the actual served response — so the
  redirect *and* the final loopback response both carry the same
  Access-Control-* headers, matching the redirect-resource bullet above.
  `handleBlob` re-validates each decoded header name against the same
  allowlist as defense in depth, since this loopback endpoint has no
  authentication of its own.
- **Miss** (URL not in the currently-enabled scope's resource set) →
  governed by `offlineCache.missPolicy` (`MissPolicy` in `replay.go`) when
  the scope is a single item or a playlist whose every item is cached:
  `fail_closed` (default) fails the request visibly rather than silently
  substituting or passing through, which guarantees deterministic offline
  behavior and surfaces partial captures honestly; `pass_through` lets the
  request continue to the real network and is only sensible when the
  device is known to be online (progressive capture) — it is a config
  toggle, not the default, and today is a plain pass-through rather than
  an implementation of progressive re-capture. When the scope is a
  *mixed* playlist (some items cached, some not — see below), a miss
  always passes through regardless of the configured policy, since it
  cannot be told apart from a legitimate request belonging to an
  uncached sibling item still sharing the same CDP target.

Replay is only enabled while a cached item is on screen:
`commandrouter`'s `displayPlaylist` path and `playlist-refresher` call
`KioskReplay.SyncPlaylist` (`kioskreplay.go`) with the current playlist's
item IDs before/after the CDP display call, which enables `Fetch`
interception scoped to whichever of those IDs are actually cached
(`EnableForPlaylist` in `replay.go`) and disables it entirely when none are.
`commandrouter`'s `displayPlaylist` handler calls `SyncPlaylist` for the
*new* playlist **before** asking CDP to actually display it — deliberately,
so `Fetch` interception is already scoped correctly by the time the kiosk
starts requesting that playlist's resources, rather than racing the first
few requests against a scope switch that has not happened yet. If the CDP
send itself then fails, or the player rejects the command (`ok:false`),
the kiosk never actually switched — it is still genuinely showing whatever
it displayed before — so that early scope switch left replay pointed at a
playlist that never loaded. `Process`'s `CMD_DISPLAY_PLAYLIST` branch
reverts this in its own deferred failure/rejection handling by calling
`resyncKioskReplayScopeToCurrentDisplay` (below), which re-queries the
player's actual current status live and re-syncs scope to that instead.
Like the clear-time resync it shares an implementation with, this revert
is itself best-effort: if the revert's own `FetchPlayerStatus` call also
fails, scope is simply left stale until the next periodic
`playlist-refresher` pass corrects it — the same bounded-staleness
trade-off already accepted for that path.
`Replayer.Disable` only clears its local resources/scope AFTER the
underlying `Fetch.disable` CDP call actually succeeds — if that call
itself fails, the previous scope is left in place but with its miss
policy forced to pass-through (the same relaxation `mixed` uses above),
rather than clearing to a fail-closed state while Chromium's own
interception might still be live: doing the latter used to turn every
subsequent request on whatever is now on screen into a `Fetch.failRequest`,
breaking normal online playback outright the moment a device moved to an
uncached/online playlist.
Because `Fetch.enable`'s pattern is `"*"` (every request on the page, not
just the cached items' own URLs — DP-1 playback advances between a
playlist's items client-side, without telling `feral-controld` which one
is currently on screen), `SyncPlaylist` also tells `EnableForPlaylist`
whether the enabled set is a strict subset of the displayed playlist's
items (`mixed`); `replay.go` uses that to relax `fail_closed` to
pass-through for exactly that scope, so an uncached sibling item can still
reach the live network instead of having every one of its requests failed
just because it happens to share a CDP target with a cached item.
`KioskReplay.AttachOnReconnect` re-attaches the replay CDP session in
`main.go`'s CDP `onConnect` hook, so a kiosk Chromium restart (including OOM
recovery) does not leave replay silently detached.

**`displayPlaylist` by `playlistUrl` falls back to the cached copy when
offline.** `commandrouter`'s `displayPlaylist` handler calls
`dp1.ProcessPlaylistURL` first; if that fails (most commonly: no
network) and the command carries a `playlistUrl` (as opposed to an
inline `dp1_call` payload), it tries `Service.CachedPlaylistForURL(url)`
before giving up. That method looks up `store.LoadPlaylistIDForURL`
(`playlists/by-url/<sha256(url)>.json`, a small pointer file distinct
from the playlist record itself) and then `store.LoadPlaylist` for the
resolved ID — both populated by `DownloadPlaylist`'s `sourceURL`
parameter whenever a download was originally requested by URL (an
inline `dp1_call` download passes `""` and is deliberately not
URL-recoverable, only ID-recoverable, same as before this fallback
existed). The fallback playlist is a "last known good" copy: it does not
reflect anything republished at that URL since it was downloaded, and —
since it can only exist by having been downloaded successfully once
before — was already signature-verified and fully DP-1-resolved
(dynamic content materialized) at that time, so no further DP-1
processing happens on the cached copy. If there is nothing to fall back
to (never downloaded, offline caching disabled, or the download was
since cleared — `LoadPlaylistIDForURL` intentionally is not kept in
lockstep with `DeletePlaylist`, see its doc), `displayPlaylist` reports
the original live-resolution error, not a confusing "no cached copy"
error about a fallback the caller never asked for.

`resolveDisplayedPlaylist` (`commandrouter/offlinecache.go`), used by
`resyncKioskReplayScopeToCurrentDisplay` to re-sync replay scope against
whatever the player actually reports as currently displayed, reuses this
exact same `loadCachedPlaylistForURL` fallback for its own `PlaylistURL`
branch. An earlier revision resolved only via live `ProcessPlaylistURL`
here, which meant a device that was offline and already displaying a
playlist through the fallback above would never successfully resync scope
after a clear — the resolve would fail every time with no cache
alternative, silently skipping the resync (best-effort, so the clear
itself still succeeded, but replay's Fetch-interception scope could keep
stale entries for the just-cleared item). Sharing the same fallback here
closes that gap.
`resyncKioskReplayScopeToCurrentDisplay` has two call sites:
`handleClearPlaylistItemCache`/`handleClearPlaylistCache` (immediately
after a successful clear) and `Process`'s `CMD_DISPLAY_PLAYLIST` branch's
deferred failure/rejection handling (immediately above). Both react to
replay's scope having drifted from what the player is actually showing,
for different reasons (a clear invalidating the currently-scoped
resources vs. a display command that never actually took effect), so both
resolve through the same live `FetchPlayerStatus` → `resolveDisplayedPlaylist`
→ `SyncPlaylist` shape rather than each reimplementing it.

`playlist-refresher`'s own `processPlayingPlaylist` — the periodic re-sync
pass, and the one path `main.go`'s CDP `onConnect` hook actually drives via
`ForceRefresh` after every kiosk/CDP reconnect — carries an equivalent
`PlaylistURL` fallback (`refresher.loadCachedPlaylistForURL`, same shape as
`commandrouter`'s). Before this existed, a device that went offline while
already displaying a downloaded-by-URL playlist would fail this pass's live
`ProcessPlaylistURL` call and return early on *every* periodic tick and on
every reconnect-triggered `ForceRefresh`, permanently skipping the
`kioskReplay.SyncPlaylist` call further down — so a Chromium restart that
happened while offline would leave Fetch interception disabled for that
playlist until connectivity returned, defeating the point of having
downloaded it for offline playback in the first place. `commandrouter` and
`playlist-refresher` intentionally do not share a single fallback
implementation (they live in different packages with narrow interfaces
onto `offlinecache.Service`/`offlinecache.KioskReplay`), but both resolve
through the same `Service.CachedPlaylistForURL` seam, so they observe
identical cache state.

## 7. Relationship to the DP-1 spec

| Spec reference | Relevance here |
|---|---|
| §5 `repro` block | `assetsSHA256` is the optional, publisher-supplied completeness/verification signal; not consulted automatically today, but a future coverage cross-check could use it |
| §8 Transport Profile | Confirms `file://`/offline transport is in-scope for DP-1 generally |
| §7.1 Signing | `source` is never rewritten in `ItemRecord.Item` or the stored playlist document; replay interception keys on the original URL. Signatures verify against a JCS-canonicalized form (`dp1-go`'s `sign` package), not raw bytes, so re-serializing the resolved playlist through `dp1.DP1` (§5) does not affect signature validity even though it is not byte-identical to the original wire document |

## 8. Known limitations

- **Not provably complete.** Capture is observation-based over a bounded
  window, not manifest-based; no capture is a formal guarantee. Coverage
  (`Coverage.Complete`/`Reason`) is the best-effort signal surfaced to the
  mobile app, not a certification.
- **Nested targets are not separately attached.** Capture only observes the
  top-level page target's `Network` events; it does not attach
  `Target.setAutoAttach` for Web Workers or nested iframes, so requests
  issued purely from within a worker or iframe (rather than proxied through
  the top-level page) can be invisible to capture. Service Workers
  registered by the artwork itself compound this: they can intercept and
  serve their own responses, further hiding requests from top-level
  `Network` domain capture.
- **WebSocket / streaming data** cannot be captured-and-replayed this way —
  such items are out of scope for this pipeline (§3.2).
- **Personalized/authenticated responses** captured once may not be valid
  to replay for a different session; this pipeline does not attempt to
  detect or special-case per-session content beyond what URL-keying already
  handles (on-chain hash-in-URL items are deterministic per edition and work
  cleanly; true per-session/cookie-gated content is not covered).
- **A live on-chain "provenance check" API call** is a real pattern seen in
  production content (fired unconditionally by first-party code on load,
  distinct from third-party analytics); such calls are recorded as a normal
  capture miss/failure like any other unreachable-offline request rather
  than special-cased (if it is a `POST`/similar unsafe method, specifically
  as `unsupported_method(<method>):<url>` — see §4.7), and artworks that
  degrade gracefully when it is unavailable are unaffected.
- **Headless GPU rendering path is not identical to the kiosk's.**
  `downloader.go` launches headless Chromium with GPU acceleration enabled
  (`--ignore-gpu-blocklist`/`--enable-gpu-rasterization`, no
  `--disable-gpu`) so WebGL/canvas artworks take the same
  context-available code path during capture as they do live — the prior
  `--disable-gpu` flag made `canvas.getContext("webgl")` return `null`
  during capture, which could make a feature-detecting artwork silently
  skip GL-dependent resource fetches that the live kiosk does make. This
  narrows, but does not close, the gap: headless capture still renders
  off-screen rather than through the kiosk's Wayland surface
  (`start-kiosk.sh`), and the two Chromium instances are not forced onto
  the same ANGLE/Vulkan backend. This has not been validated against the
  actual device GPU/driver under concurrent kiosk load; if field capture
  results diverge from live rendering for GPU-heavy artworks, start by
  comparing `chrome://gpu` output between the two Chromium instances on
  the actual hardware.

## 9. See also

- `components/feral-controld/offlinecache/` — the implementation.
- `docs/controld-inbound-controller-messages.md` — the 5 controller-visible
  commands (`downloadPlaylistItem`, `downloadPlaylist`,
  `clearPlaylistItemCache`, `clearPlaylistCache`, `getOfflineCacheStatus`)
  and the `offline_cache_status` notification.
- `components/feral-controld/config/config.go` — `OfflineCacheConfig`
  (`offlineCache.*`) tuning knobs, and their defaults in
  `offlinecache/bootstrap.go`.
