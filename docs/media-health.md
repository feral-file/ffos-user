# Media health: device-side media probes and the observation store

Issue: feral-file/ffos-user#392. Design discussion and decisions:
https://github.com/feral-file/ffos-user/issues/392#issuecomment-6626228523.

The FF1 keeps its own record of whether each artwork URL it is handed can be
played. `feral-controld` observes at three layers, writes one record per
source into a permanent store, hints the player past items it has concluded
are broken, and reports one verdict per item to the app. Nothing central is
consulted and nothing leaves the device.

| Layer | When | What it sees | Package |
|---|---|---|---|
| L0 | before play (the cast-time pass) and ahead of play (the lookahead) | the first 32 KiB of the source: HTTP status, declared vs sniffed type, gateway error pages and directory listings, corrupt containers | `mediahealth` (rules), `offlinecache` (the guarded fetch) |
| play | every `checkStatus` poll | what the player reported for each item it mounted: ready, or failed at which stage | `mediahealth` (drain), `status` (poll) |
| L1 | nightly, while the panel sleeps (own PR, flag off by default) | a rendered frame: blank, or a known error-page fingerprint | not shipped yet |

Two rules bind every writer:

1. **No observation hides a work.** The device skips an item only on its own
   conclusive verdict: a `broken` record inside its recheck window. A transient
   failure (429, 5xx, timeout, guard refusal) is never persisted as a verdict.
   The one exception is corroborated over time: a name that fails to resolve
   on every counted attempt for a day, each while the device resolved other
   names, is broken (see the DNS streak below).
   L1 verdicts are reported as `suspect` and never skip anything. No central
   block list is ever consumed.
2. **Source URLs stay on the device.** The record holds the source because
   hints are keyed by it over loopback CDP; status surfaces, error messages
   and log lines carry no URL (log lines use the redacted label: scheme, host,
   path).

## The record (`mediahealth.Record`)

One per source, keyed by `sha256(item.source)` — the same key the offline
cache uses (`offlinecache.SourceKey`), pinned equal by a test.

| Field | Meaning |
|---|---|
| `state` | `ok`, `broken`, `suspect`; absent = unknown |
| `category` | the app-facing class (table below) |
| `reason` | the detail code: the indexer's L0 taxonomy or a play stage |
| `layer` | `l0`, `play`, `l1`: which probe set the current state |
| `wireUrl`, `host` | what L0 actually dialed, after the kiosk gateway rewrite and redirects |
| `httpStatus`, `declaredType`, `sniffedType`, `mediaType` | evidence |
| `checkedAt`, `recheckAfter` | when the state was set and when it is due for another look |
| `lastError`, `lastErrorAt` | the most recent transient attempt; never changes state |
| `plays` | ok / fail / skipped counts, consecutive failures, the last failure (stage, code, media type, attempt) |
| `build` | the controld version that wrote the verdict |

**Recheck policy.** Records never expire.

| state | re-probed | note |
|---|---|---|
| broken | after 24 h; after 7 consecutive broken rechecks, after 7 d | the hint keeps skipping until a reprobe or a play success heals it |
| ok | after 7 d | a play success refreshes it for free |
| transient (any state) | next attempt after 1 h (`retryAfter`) | the stored state is untouched; an expired broken verdict is NOT revived — the hold delays the probe, never the skip |
| DNS streak (`dnsFailSince`, `dnsFailures`) | with the transient hold | broken (`dns`, `source_missing`, layer `l0`) once at least 3 counted failures span 24 h; any answer that got past DNS ends the streak |

A broken record **past** its recheck window is still shown to the app as
broken (it is the last thing observed) but no longer hints and no longer
counts toward rejecting a cast.

## Categories (what the app shows)

| category | reasons | state |
|---|---|---|
| `source_missing` | `http_status` 404/410/400-class, `dns` | broken |
| `source_blocked` | the player's own HEAD saw 401/403 (`render_detect` with that code). L0 treats those as transient because its request identity is not the kiosk's | broken, corroborated |
| `not_media` | `type_mismatch`, `directory_listing`, `known_error_page` | broken |
| `corrupt_file` | `zero_length`, `truncated`, `container_invalid` | broken |
| `unsupported` | `unsupported_scheme`, `data_uri_invalid`, `transport` (reserved) | broken |
| `render_failed` | `render_detect`, `render_load`, `render_decode`, `render_watchdog`, `render_exhausted` | broken, corroborated |
| `render_blank` | `render_blank`, `render_known_bad` (L1) | suspect |
| `device_limit` | `webgl_lost` | recorded on `plays.lastFail` only; never a verdict |

## L0: the probe

Ported from `ff-indexer-v2/docs/media_viewability.md` with the rules in the
indexer's order, so a device verdict and an indexer verdict for the same bytes
agree. The regression inputs travel with the port (`mediahealth/validator_test.go`).

- One ranged GET of `bytes=0-32767` through the offline cache's guarded
  client (`offlinecache.NewMediaHealthFetcher`): source guard (reserved
  addresses, non-http schemes, every resolved address), guarded redirects (at
  most 10 hops, each re-checked, Referer stripped), no proxy, keep-alives off
  so `net/http` can never silently replay a request, 64 KiB response-header
  ceiling, a process-wide admission budget on in-flight parses (32 HTTP or 8
  TLS peers — re-admitted per hop, so a plain-HTTP request that redirects to
  HTTPS pays the TLS weight before the handshake; it releases its hold before
  acquiring the larger one, never hold-and-wait), and a hard cap on body
  bytes read.
- The probe dials the **wire the kiosk would**: a content-addressed path on a
  hostile gateway (`ipfs.io`, `dweb.link`) goes to the replacement gateway and
  any other path on those hosts gets the `uarewrite` User-Agent, exactly as the
  kiosk interceptor does — on the initial URL and on **every redirect hop**
  (the interceptor evaluates its policy per request), each re-pointed before
  the guard and the dial, so a source that redirects onto a retired gateway
  is judged at the replacement, not at the retired gateway's 404.
- A 206 is trusted only with a `Content-Range` starting at byte 0; otherwise,
  and on 416, and when a valid prefix is shorter than 512 bytes and shorter
  than the resource, the probe retries once without `Range`. A 206 to an
  unranged request is broken (`http_status`).
- Status table: 2xx → content rules; 400/404/410-class → dead; 401, 403, 406,
  407, 408, 416, 429 and every 5xx → transient; a 3xx that survives the
  redirect chain → transient. NXDOMAIN → dead (`dns`); every other transport
  failure → transient, including a deadline that expires mid-body (judged
  before any byte count: a timed-out short read is not truncation).
- **DNS streak.** A resolution failure short of NXDOMAIN (SERVFAIL, a
  resolver timeout, a refusal) is transient per attempt, because it can be
  the device's own resolver. It counts toward the source's streak only when
  the device demonstrably resolves other names: controld's internet verdict
  is online (that check needs DNS itself), or another probe's name resolved
  within the last hour. A failure while nothing resolves neither counts nor
  ends the streak. At 3 counted failures spanning 24 h the record is broken
  (`dns`) with the normal broken recheck, so it heals the day the zone
  returns. Expired domains answer SERVFAIL, not NXDOMAIN, on most resolvers,
  which is why NXDOMAIN alone was not enough. Fetch
  errors carry redacted URLs, so a signed redirect hop never reaches a log
  line or a stored `lastError`.
- Content rules, first failing rule wins, anything ambiguous is healthy:
  `zero_length`, `truncated` (judged against what THIS response promised, never
  the Content-Range total), sniff with `mimetype` v1.4.9 (pinned: verdicts are
  version-sensitive), `directory_listing` (two Kubo signals together, never a
  lone substring), `known_error_page` (operator markers,
  `mediaHealth.knownBadPageMarkers`), `type_mismatch` (declared image/video/
  audio sniffed as html/plain/json; text-based media types exempt),
  `container_invalid` (PNG, GIF, WebP, JPEG headers).
- `data:` URIs never dial: well-formed is inline (never recorded),
  proven-malformed is dead and recorded like any other broken source, so it
  gets a hint; the sweep re-settles such a record without network.
- Everything the pass settles outside the worker pool (malformed `data:`
  URIs, a shared verdict fanned out to wire-identical siblings) is written in
  one store transaction after the request barrier, so a playlist repeating
  one source thousands of times costs one request and one write.

### The cast-time pass (replaces the #304 preflight)

`commandrouter`'s `displayPlaylist` runs `mediahealth.Prober.Preflight` over
the resolved sources after DP-1 resolution and before the playback lock. Shape
kept from the preflight: a fixed worker pool, wire-identical sources probed
once (fragment-stripped key — the request is shared, the verdict is recorded
under every variant's own source, and a variant with its own stored verdict
keeps it), a per-pass budget of 256 distinct wire URLs, a 5 s per-item
deadline covering guard DNS, a 10 s phase ceiling, the 2048-item skip, and
`displayAt` playlists skipped entirely. What changed: no early exit on the
first alive source — every source the pass reaches is probed and recorded —
and a source with a **conclusive stored verdict** (broken or ok inside its
window) is answered from the store without a request, so a playlist seen
before costs no network at cast.

The pass is paced, not a burst. It probes the **first 24 sources in play
order** (`castItems`) that need a request with **4 workers**
(`castConcurrency`) and at most **2 in flight per host** — a playlist served
by one CDN runs at 2, whatever the pool — then hands the rest of the wall to
the lookahead. Items beyond the pass are unknown at cast: no record, no hint,
fail-open; their hint rides the next push once the lookahead has reached them.
The all-dead rejection keeps its reach: while **every** source seen so far is
dead (a stored alive verdict, an inline item or an inconclusive answer ends
it) the pass continues batch by batch up to the budget, so 200 dead items are
still refused and 24 dead items in front of a live one still play.

A host that answers **429 or 503** is cooled for its `Retry-After`
(60 s when it sends none, never more than 1 h) in a `HostLimiter` the pass and
the lookahead share: sources on a cooled host are stepped over without a
request and without a record (unknown, not held), and the probed source's own
transient hold is the origin's `Retry-After` bounded by the usual hour instead
of a flat hour.

The admission rule is unchanged: the cast is rejected with `sourceUnreachable`
only when EVERY item is dead (now including a conclusive broken record), and
the offline-cache rescue applies as before. The rejection message names items
by index and HTTP status, or by reason code for a content/DNS verdict, never
by URL.

### The lookahead

From the status poller's playback observer, `mediahealth.Lookahead` works one
request at a time with a 2 s gap, in this order:

1. the next 16 items after the current index (then the current item itself,
   so a one-item playlist still gets its recheck) whose record is missing or
   due;
2. the rest of the wall, in play order, for sources that have **never been
   probed** — this is how the items the cast pass left get their first
   verdict before playback reaches them (a 1,000-item wall is covered in
   about half an hour, never in a burst); due-but-recorded rows out there
   already show a verdict and are left to the sweep;
3. one due record not on the wall (excluded inside the index scan, so on-wall
   rows never shadow them) under a budget of 20 per hour, so a fixed source
   heals without being cast again and a 100k-record store never becomes 100k
   probes a day.

Every request goes through the shared `HostLimiter`: a source on a cooled
host is stepped over (not held — it is eligible again when the cooldown ends)
and a slot on its host is taken like the pass takes one. A source the worker
just probed is left alone for 1 h whatever its record says: a probe the source
guard or the URL-length limit refuses writes no record (a policy objection is
never a verdict), so without that hold it would stay due and block everything
behind it. New hints ride the next push.

## The hint (controld → player): `mediaHealth` on `displayPlaylist`

Manifest contract: `contracts.mediaHealth` version 1 in
`/opt/feral/feral-player/ffos-player-contract.json`. The field is sent only
when the installed player declares it (re-read when the manifest's mtime
changes), on every push: a cast, a scheduler cutover, a refresher soft refresh
or force cast. Any `mediaHealth` a controller puts on a cast is dropped.

```json
"mediaHealth": {
  "<item.source>": { "state": "broken", "category": "source_missing", "reason": "http_status" }
}
```

Sparse: conclusively broken items only, minus any source the offline cache
can replay from a confirmed capture (the hint must not hide what replay
interception would show — the same reason the cast rescue exists). The player
skips a hinted item in its timeline without mounting it, keeps it in `items`,
and reports `{kind:"skip", stage:"hint"}` in its outcome ring.

### The hint-only push: `setMediaHealth`

A verdict reached **between** pushes (the lookahead, the sweep, the outcome
drain) would otherwise reach the player only on the next cast, cutover or
refresh, and a static inline cast is never re-pushed. Since the cast pass
probes only its first 24 items, that is where most of a long wall's verdicts
land. `mediahealth.HintPusher` closes the gap with a player command that
replaces only the held hint map:

```json
{"command": "setMediaHealth", "request": {"castSeq": 7, "mediaHealth": {"<item.source>": {"state": "broken", "category": "source_missing", "reason": "http_status"}}}}
```

- **Player side.** Replace semantics: the map is the full hinted set for the
  wall, `{}` when nothing is broken any more, so a heal reaches the player as
  well. Applied only when `castSeq` is the outcome ring's current generation,
  else `{ok:false, error:"staleCast"}` with the held map untouched. The work on
  screen is never unmounted; the map is honoured at the next selection under
  the existing rules. Reply `{ok:true, skippedIndices}`.
- **Gate.** Sent only when the manifest's `mediaHealth` entry, still version
  1, lists `"commands": ["setMediaHealth"]`. Controld gates the contract on
  version 1 exactly, so a version bump would turn hints off on fielded
  daemons; the command is detected by name instead.
- **When.** Every 15 s the pusher recomputes the map for the wall with the
  same `Service.Hints` every push uses and sends it when it differs from what
  the player is believed to hold. Recomputing rather than listening for store
  writes also catches a verdict that **expires** past its recheck window. The
  belief is the map the last push path attached, or the last delivered
  push, bound to the cast generation. An unknown belief costs at most one
  redundant send, which replace semantics make harmless. Nothing is sent
  in the first 15 s after a new cast, since the cast's push carried the map.
- **Failure.** `staleCast` waits for the next poll's generation. A transport
  error or a reply that crossed a page generation change retries on the next
  tick. Any other refusal parks the pusher until the next cast, so it never
  loops against a player that does not implement the command.
- **Internal.** `setMediaHealth` is tier `internal` and the command router
  refuses it from a controller: only the daemon may install a skip map.

## The outcome ring (player → controld): `outcomes` on `checkStatus`

Beside the ring, the same contract carries the player's own report of what
it is skipping: `"skippedIndices": [1, 4]` — the indices into `items` the
timeline is currently resolving past because of a hint (empty or absent when
none; absent on a player without the contract). It is the one fact only the
player knows, which hint map it holds, and it feeds `mediaHealth[i].skipped`
(Status surfaces). Indices only, never sources. Live-only, like the ring.

```json
"outcomes": {
  "castSeq": 3,
  "entries": [
    { "seq": 17, "source": "<item.source>", "kind": "fail", "stage": "decode",
      "code": "decode", "mediaType": "video", "attempt": 2, "elapsedMs": 1532,
      "at": "2026-11-04T10:00:00.000Z" }
  ]
}
```

The last 64 entries; `seq` counts for the lifetime of the player page and
`castSeq` increments per cast. controld keeps a seq cursor that survives casts
(a cast must not re-apply the entries still in the ring), resets it when the
ring's highest seq falls below it (a page restart), and logs when the ring
wrapped between polls. A player-session generation bump only flags the cursor:
a bump can be a CDP reconnect to the surviving page as well as a new page, so
controld remembers the identity of the entries it consumed (`seq`, `source`,
stage, attempt, `at`) and treats the next ring as the same page when it still
holds any of them, re-applying nothing; a ring holding none is a new page.
`at` is what tells one page's `seq: 1` from the next page's — always send it.
It also dates the outcome: the record keeps the player's time (clamped to the
device clock), and an outcome older than the record's current verdict counts
in the play history but never changes the state, advances the corroboration
run or refreshes a verdict — an old ring drained after a restart cannot
overturn a newer probe.
The store keeps the same identity (hashed, no URLs) for the last 256 entries
it applied, written in the same transaction as the record, so a controld
restart under a surviving page — a fresh process draining the whole ring
again — applies each entry once for the life of the store. Live-only on the
player.

| stage | kind | code |
|---|---|---|
| `detect` | fail | the content-type HEAD's HTTP status, or `network` (the device is offline: device-local, never a verdict) |
| `load` | fail | the element kind (image, video, audio, iframe, object, model) |
| `decode` | fail | the `MediaError` name |
| `watchdog` | fail | the deadline (`15s`, `45s`) |
| `webgl` | fail | device-local; recorded, never a verdict |
| `exhausted` | fail | the retry budget was spent |
| `hint` | skip | skipped on controld's hint |
| `ready` | ok | `elapsedMs` = mount → ready; a verdict only for `image`, `video`, `audio`, `model` (see below) |

**Corroboration.** One failure is recorded, not concluded. A record becomes
`broken` (layer `play`) on a second failure in a distinct play, or on one
failure when an L0 broken verdict is already standing. A play is a run of
consecutive failing entries for one source whose attempt counter does not go
backwards; the run ends on another source, a success, a skip, or an
`exhausted` entry. The several stages of one attempt (detect, load, retries,
exhaustion) count once; the last stage stays as the diagnostic. A `ready` from
a renderer that reports its own failures (`image`, `video`, `audio`, `model`)
writes `ok` and heals a broken record: the wall itself is the strongest
probe. A `ready` from an opaque embed (`iframe`, `object`, `pdf`) or with no
`mediaType` is counted as a play but writes no verdict, heals nothing and
leaves the record due: Chromium reports ready for its own error page inside
an iframe, so that ready proves the mount, not the work, and L0 has to keep
looking. Entries
are applied in `seq` order, so a detect failure whose iframe fallback then
rendered nets out to `ok`. Two classes never count: device-local failures
(`webgl`, `detect`/`network`) and any failure observed while controld's own
internet verdict is offline (or not yet known) — an outage says nothing about
a source, and a verdict earned during one would be skipped before the play
layer could ever heal it. A real-renderer `ready` counts whatever the verdict
says: a work that rendered from cache rendered.

Without the contract, controld falls back to inference: a `renderStatus == 3`
sighting for the item at `index` counts as one failure per mount (stage
`inferred`).

## Status surfaces

- `player_status` gains a controld-owned `mediaHealth` array, index-aligned
  with `items`: `{"state":"ok","skipped":false}`, `{"state":"unknown",…}`,
  `{"state":"broken","category":"…","reason":"…","layer":"l0","skipped":true}`
  or `{"state":"suspect","category":"render_blank","layer":"l1",…}`. Two facts
  per item, kept apart on purpose: `state` (with its category, reason and
  layer) is **the last observation** this device made; `skipped` is whether
  **the player is resolving past the item right now**, from the player's own
  report of the hint map it holds (`skippedIndices` below). They differ in
  four cases, and the app's "Skip" badge must come from `skipped` only:
  an expired verdict (past `recheckAfter`: still reported `broken`, no longer
  hinted), a source the offline cache can replay (hint dropped), a player
  without the contract (nothing hinted), and push lag in either direction
  (a verdict concluded mid-playlist reaches the player on the next push; a
  record healed to `ok` is still skipped until then). When `state` is
  `broken` and `skipped` is false, `unskipped` says which:
  `expired` | `replayable` | `player` | `pending` (hinted now, push not yet
  received — or the player chose to play it: repeat-one, an all-broken map).
  Nothing else is pushed; freshness (`checkedAt`) stays on `getMediaHealth`.
  The raw `outcomes` ring and `skippedIndices` are stripped from the
  notification and from direct `checkStatus` replies (hub and relayer): they
  are inputs, consumed on the device only.
- `getMediaHealth` (controller tier, query class, no arguments) returns the
  whole playlist on screen: `{ok, status, items:[{index, id, title, state,
  category, reason, layer, skipped, unskipped, checkedAt, plays:{ok,fail}}],
  total, truncated, castSeq}`, `skipped`/`unskipped` by the same rule as the
  push. Bounded at 2048 rows. No source URLs.

## Store

`/home/feralfile/.state/media-health/store.db`, a bbolt file: buckets `items`
(key → record), `recheck` (`<recheckAfter>|<key>` index) and `meta`. Point
reads for hints and status; batched, fsynced writes; no eviction and no count
bound (~300 B per record). A disk guard refuses writes while the filesystem is
under the offline cache's 5 GiB reserve and logs once per episode. A record
written by a newer binary is neither read nor rewritten.

Lifetime: survives reboot; cleared by factory reset
(`devicectl.SetMediaHealthResetter`); survives a full-image OTA once the ffos
updater's carry step includes the directory (a separate ffos change on the
image rail — until then the store is lost on OTA, which costs one re-probe
cycle).

## Configuration (`controld.json`)

```json
"mediaHealth": {
  "disabled": false,
  "knownBadPageMarkers": ["gateway time-out"],
  "probeMaxBytes": 32768,
  "lookahead": 16,
  "castItems": 24,
  "castConcurrency": 4
}
```

`disabled` is the kill switch: no store, no pass (casts forwarded unprobed,
the pre-#304 behavior), no hints, no lookahead, no per-item status. The
retired `sourceProbe.disabled` is read as `mediaHealth.disabled`. `lookahead:
-1` disables the lookahead and the background sweep. `castItems` and
`castConcurrency` tune the cast-time pass for an origin that still
rate-limits the defaults; the per-host cap (2 in flight) and the lookahead's
2 s gap are fixed.

## Boundaries

- `mediahealth` never imports the transports, the router, `offlinecache` or
  `uarewrite` (pinned by `mediahealth/importlint_test.go`); it never builds an
  HTTP client. `offlinecache` implements its `Fetcher`.
- The store observes. Playback is gated only by the hint and by the existing
  all-dead rejection; L1 never gates.
