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
  TLS peers), and a hard cap on body bytes read.
- The probe dials the **wire the kiosk would**: a content-addressed path on a
  hostile gateway (`ipfs.io`, `dweb.link`) goes to the replacement gateway and
  any other path on those hosts gets the `uarewrite` User-Agent, exactly as the
  kiosk interceptor does.
- A 206 is trusted only with a `Content-Range` starting at byte 0; otherwise,
  and on 416, and when a valid prefix is shorter than 512 bytes and shorter
  than the resource, the probe retries once without `Range`. A 206 to an
  unranged request is broken (`http_status`).
- Status table: 2xx → content rules; 400/404/410-class → dead; 401, 403, 406,
  407, 408, 416, 429 and every 5xx → transient; a 3xx that survives the
  redirect chain → transient. NXDOMAIN → dead (`dns`); every other transport
  failure → transient, including a deadline that expires mid-body (judged
  before any byte count: a timed-out short read is not truncation). Fetch
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
kept from the preflight: a fixed worker pool (16), wire-identical sources
probed once (fragment-stripped key — the request is shared, the verdict is
recorded under every variant's own source, and a variant with its own stored
verdict keeps it), a per-pass budget of 256 distinct wire URLs, a 5 s per-item deadline covering guard DNS, a 10 s phase ceiling, the
2048-item skip, and `displayAt` playlists skipped entirely. What changed: no
early exit on the first alive source — every source inside the budget is
probed and recorded — and a source with a **conclusive stored verdict**
(broken or ok inside its window) is answered from the store without a request,
so a playlist seen before costs no network at cast.

The admission rule is unchanged: the cast is rejected with `sourceUnreachable`
only when EVERY item is dead (now including a conclusive broken record), and
the offline-cache rescue applies as before. The rejection message names items
by index and HTTP status, or by reason code for a content/DNS verdict, never
by URL.

### The lookahead

From the status poller's playback observer, `mediahealth.Lookahead` probes the
next 8 items after the current index (then the current item itself, so a
one-item playlist still gets its recheck) whose record is missing or due, one
at a time with a 1 s gap, and — when nothing is ahead — one due record not on
the wall (excluded inside the index scan, so on-wall rows never shadow them)
under a budget of 20 per hour, so a fixed source heals without being cast
again and a 100k-record store never becomes 100k probes a day. A source the
worker just probed is left alone for 1 h whatever its record says: a probe the
source guard or the URL-length limit refuses writes no record (a policy
objection is never a verdict), so without that hold it would stay due and
block everything behind it. New hints ride the next push.

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

## The outcome ring (player → controld): `outcomes` on `checkStatus`

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
Live-only on the player.

| stage | kind | code |
|---|---|---|
| `detect` | fail | the content-type HEAD's HTTP status, or `network` (the device is offline: device-local, never a verdict) |
| `load` | fail | the element kind (image, video, audio, iframe, object, model) |
| `decode` | fail | the `MediaError` name |
| `watchdog` | fail | the deadline (`15s`, `45s`) |
| `webgl` | fail | device-local; recorded, never a verdict |
| `exhausted` | fail | the retry budget was spent |
| `hint` | skip | skipped on controld's hint |
| `ready` | ok | `elapsedMs` = mount → ready |

**Corroboration.** One failure is recorded, not concluded. A record becomes
`broken` (layer `play`) on a second failure in a distinct play, or on one
failure when an L0 broken verdict is already standing. A play is a run of
consecutive failing entries for one source whose attempt counter does not go
backwards; the run ends on another source, a success, a skip, or an
`exhausted` entry. The several stages of one attempt (detect, load, retries,
exhaustion) count once; the last stage stays as the diagnostic. A `ready` writes `ok`
and heals a broken record: the wall itself is the strongest probe. Entries
are applied in `seq` order, so a detect failure whose iframe fallback then
rendered nets out to `ok`. Two classes never count: device-local failures
(`webgl`, `detect`/`network`) and any failure observed while controld's own
internet verdict is offline (or not yet known) — an outage says nothing about
a source, and a verdict earned during one would be skipped before the play
layer could ever heal it. A `ready` counts whatever the verdict says: a work
that rendered from cache rendered.

Without the contract, controld falls back to inference: a `renderStatus == 3`
sighting for the item at `index` counts as one failure per mount (stage
`inferred`).

## Status surfaces

- `player_status` gains a controld-owned `mediaHealth` array, index-aligned
  with `items`: `{"state":"ok"}`, `{"state":"unknown"}`,
  `{"state":"broken","category":"…","reason":"…","layer":"l0"}` or
  `{"state":"suspect","category":"render_blank","layer":"l1"}`. Nothing else is
  pushed. The raw `outcomes` ring is stripped from the notification.
- `getMediaHealth` (controller tier, query class, no arguments) returns the
  whole playlist on screen: `{ok, status, items:[{index, id, title, state,
  category, reason, layer, checkedAt, plays:{ok,fail}}], total, truncated,
  castSeq}`. Bounded at 2048 rows. No source URLs.

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
  "lookahead": 8
}
```

`disabled` is the kill switch: no store, no pass (casts forwarded unprobed,
the pre-#304 behavior), no hints, no lookahead, no per-item status. The
retired `sourceProbe.disabled` is read as `mediaHealth.disabled`. `lookahead:
-1` disables the lookahead and the background sweep.

## Boundaries

- `mediahealth` never imports the transports, the router, `offlinecache` or
  `uarewrite` (pinned by `mediahealth/importlint_test.go`); it never builds an
  HTTP client. `offlinecache` implements its `Fetcher`.
- The store observes. Playback is gated only by the hint and by the existing
  all-dead rejection; L1 never gates.
