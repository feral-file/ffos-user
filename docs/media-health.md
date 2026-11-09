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
   The one exception is corroborated over time: an origin that gives no
   usable answer on every counted attempt for a day, each while the device
   reached other origins, is broken (see the unreachable streak below).
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
| unreachable streak (`unreachableSince`, `unreachableFailures`) | with the transient hold | broken (`unreachable` → `source_unreachable`, or `dns` → `source_missing` for a DNS-only failure; layer `l0`) once at least 3 counted failures span 24 h; any answer ends the streak |
| broken, from the play layer | after 24 h; after 7 consecutive re-conclusions, after 7 d | the same backoff as L0, so a work that stays dead costs one failed play a week, not a day |

A broken record **past** its recheck window is still shown to the app as
broken (it is the last thing observed) but no longer hints and no longer
counts toward rejecting a cast.

## Categories (what the app shows)

| category | reasons | state |
|---|---|---|
| `source_missing` | `http_status` 404/410/400-class, `dns` | broken |
| `source_unreachable` | `unreachable` (a day of attempts with no usable answer, each while other origins answered — including a gateway's unconfirmed 404 for a content-addressed source), and a play-layer `render_detect` 404/410-class answer for a content-addressed source (one gateway's answer; the play layer has no second gateway to confirm absence) | broken |
| `source_blocked` | the player's own HEAD saw 401/403 (`render_detect` with that code). L0 treats those as transient because its request identity is not the kiosk's | broken, corroborated |
| `not_media` | `type_mismatch`, `directory_listing`, `known_error_page` | broken |
| `corrupt_file` | `zero_length`, `truncated`, `container_invalid` | broken |
| `unsupported` | `unsupported_scheme`, `data_uri_invalid`, `unsupported_mime_type` (a `data:` URI declaring a type the kiosk cannot display), `transport` (reserved) | broken |
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
- **Gateway fallback for IPFS and Arweave.** On a content-addressed path —
  `/ipfs/<cid>` on any host, or `/<43-character transaction id>` on an
  Arweave gateway — a 404 or 410 means "this gateway could not retrieve it",
  not "it does not exist": on the office FF1, 7 of 60 healthy works in a
  labeled fixture read 404 from arweave.net while ar-io.dev served them. So
  the probe tries the network's other configured gateways
  (`mediaHealth.ipfsGateways`, `mediaHealth.arweaveGateways`), in order, at
  most two, inside the same item deadline, stopping at the first that serves:
  - **one serves it:** its bytes are judged by the normal content rules.
    Valid media is `ok` with `servedBy` = that gateway, and the kiosk loads
    the work through it (below), so the verdict and the wall agree;
  - **its content rule fails:** that is a verdict about the content, and it
    stands;
  - **every gateway says 404/410:** `broken`;
  - **nothing else answers** — a 5xx, a timeout, or any other status (402,
    403, 429: the gateway's policy, never the content's) — **or no other
    gateway is configured for the network:** a transient `server_error`
    feeding the unreachable streak; one gateway's 404 is never a verdict on
    its own. A deadline that runs out during the fallback at cast records
    nothing, like any cast timeout.

  The confirmation is the request the kiosk makes — **no `Range` header**,
  the body read capped at the probe window — not the probe's ranged read.
  ar-io.dev served a 32 KiB range of a 195 MB video and answered the
  whole-file load with `402 Payment Required` (an x402 offer for "data
  egress"); on the ranged read it looked like a serving gateway, and every
  item routed through it failed to play (office FF1, 2026-11-06). The
  shipped Arweave list is therefore `arweave.net` alone — a fallback for
  sources on any other Arweave gateway, none for sources on arweave.net
  itself — and ar-io.dev was dropped. Add a fallback only after a whole-file
  load of a large work has been seen to succeed from the device.

  **The kiosk applies it.** The kiosk's request interceptor (`uarewrite`)
  asks the store for every `/ipfs/` request on any host and every request on
  an Arweave gateway: when the content's id has a `servedBy` record, the
  request goes to the same path and query on that gateway, invisibly to the
  page — the source, the offline-cache identity and every log keep the
  original URL, and a work's own files under the same CID or transaction
  follow. The override targets only a configured gateway origin plus the
  request's own path and query, never a URL from playlist content. It is
  indexed by route — the content id plus the gateway the source names — with
  one entry per source that needs it, in the store's `servedBy` bucket
  (mirrored in memory for the per-request lookup). A route stays while any
  source on it still needs the fallback, so one source recovering never
  drops another's — another file under the same CID, or the same CID named
  through another gateway — and it survives a restart. The choice is per
  source, not per route: two files under one CID can need different
  fallbacks (the first gateway serves `a.html` but answers 404 for
  `b.html`, which the second serves), so a request that is a recorded
  source loads through the gateway that served that source — its own
  gateway included: every ok content-addressed source is indexed, with `""`
  for "its own gateway", so a source that recovered while another file
  under the CID still needs a fallback is never mistaken for an untied
  request — a work's own files follow the document that embeds them (the
  request's `Referer`, which a same-origin subresource carries in full), and
  only a request nothing ties to a source takes the route's stable
  best-effort fallback. Only routes with a fallback are held in memory;
  the rest stay in the bucket, so memory is bounded by the sources that
  need a fallback. The override only ever targets a gateway still on the
  operator's lists: at startup a recorded fallback that is no longer
  configured is dropped and its source made due, so removing a gateway
  from the config (as ar-io.dev was) takes effect on the next restart, not
  when the 7-day ok verdict expires. Every probe still
  tries the source's own gateway first, so when it serves again `servedBy`
  clears and the kiosk goes back to it. A `broken` conclusion from ANY
  layer clears it too: a play-layer verdict over a work "served by" a
  fallback must not leave the kiosk loading the gateway the verdict has just
  proven does not play it. Without the interceptor
  (`gatewayUserAgent.enabled: false`) a fallback gateway serving a work is
  reported as a gateway failure (`server_error`, an answer that ends the
  streak) instead of `ok`: the verdict never claims what the wall cannot do.
  The kiosk observer matches a request loaded through a fallback gateway to
  its wall source by content id.
- **Unreachable streak.** An attempt in which the origin gives no usable
  answer is transient on its own, because it can be the device's network.
  Three classes count: DNS short of NXDOMAIN (SERVFAIL, a resolver timeout —
  expired domains answer SERVFAIL on most resolvers), no response at all (a
  timeout before any header, a refused or reset connection, a TLS failure —
  a retired gateway whose DNS still points at servers that drop
  connections), and a 5xx without `Retry-After`. Any answer ends the
  streak, including a 401/403/429, a 503 that names a `Retry-After`, and a
  deadline that expires mid-body, so a slow but alive origin never
  concludes. An attempt counts only when the device demonstrably reaches
  other origins: controld's internet verdict is online, or a DIFFERENT host
  gave an end-to-end answer (a 2xx–4xx, or a definitive NXDOMAIN) within the
  last hour. The evidence is read before the attempt's own answer is added,
  the failing host never vouches for itself, and a 5xx is never evidence: a
  captive portal or broken proxy answers 502 for every host. At 3 counted attempts spanning 24 h
  the record is broken (`unreachable`, or `dns` when the last attempt was a
  DNS failure) with the normal broken recheck, so it heals on the first
  answer. A probe cut short by the cast pass's own 10 s ceiling, rather
  than its 5 s item deadline, records nothing at all: running out of our
  own budget says nothing about the source, and an unrecorded item is
  reached by the lookahead within seconds. Fetch
  errors carry redacted URLs, so a signed redirect hop never reaches a log
  line or a stored `lastError`.
- Content rules, first failing rule wins, anything ambiguous is healthy:
  `zero_length`, `truncated` (judged against what THIS response promised, never
  the Content-Range total), sniff with `mimetype` v1.4.9 (pinned: verdicts are
  version-sensitive), `directory_listing` (two Kubo signals together, never a
  lone substring), `known_error_page` (operator markers,
  `mediaHealth.knownBadPageMarkers`), `type_mismatch` (declared image/video/
  audio sniffed as html/plain/json; text-based media types exempt),
  `container_invalid` (PNG, GIF, WebP, JPEG headers). The JPEG walk reads
  segments the way Chromium's decoder does: stray bytes between segments
  are skipped to the next marker (libjpeg warns and decodes on), and only a
  segment length below 2 is corrupt. Rejecting stray bytes outright marked
  two complete images `corrupt_file` and skipped them (38 bytes of an
  orphaned Photoshop block after the Exif segment, office FF1, 2026-11-06).
- `data:` URIs never dial. Proven-malformed metadata is dead. A well-formed
  one has its payload judged in place, by ff-indexer-v2's `data:` rule with
  Chromium's decoding (percent-decoding, then forgiving base64; only the
  first probe window of a large payload is decoded): a declared type the
  kiosk cannot display (anything but image/video/audio/text/model,
  `application/json`, `application/pdf`) is `unsupported_mime_type`, an
  empty payload `zero_length`, a payload whose sniffed type does not match
  the declared one `type_mismatch`. The kiosk renders a `data:` URI by its
  declared type, so a mismatch is what the wall shows: an SVG declared
  `application/json` shows the JSON viewer, a poem declared `text/markdown`
  a blank frame, and the player reports both as `ready`. Two allowances
  for what Chromium draws anyway: an image/video/audio type matches any
  sniffed type in the same family (those elements sniff), and `text/html`
  matches any text, XML or JSON body. A payload that passes is recorded
  `ok` (layer `l0`); one that does not decode is a reasoned inconclusive (a
  hold, no verdict). Every verdict is recorded and hinted like any other;
  the cast pass writes a passing one only when its record is due, and the
  lookahead walk and the sweep settle due `data:` records without
  network.
- Everything the pass settles outside the worker pool (`data:` URIs, a shared verdict fanned out to wire-identical siblings) is written in
  one store transaction after the request barrier, so a playlist repeating
  one source thousands of times costs one request and one write.

### The cast-time pass (replaces the #304 preflight)

`commandrouter`'s `displayPlaylist` runs `mediahealth.Prober.Preflight` over
the resolved sources after DP-1 resolution and before the playback lock. Shape
kept from the preflight: a fixed worker pool, wire-identical sources probed
once (fragment-stripped key — the request is shared, the verdict is recorded
under every variant's own source, and a variant with its own stored verdict
keeps it), a per-pass budget of 256 distinct wire URLs, a 5 s per-item
deadline covering guard DNS, a 10 s phase ceiling, and `displayAt` playlists
skipped entirely. There is no item cap in front of the pass: the refresher
re-resolves a playlist with no item limit, so the wall holds everything the
query returns and is judged whole; the pass bounds its own work by probing
only its first 24 items. What changed: no early exit on the
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

From the status poller's playback observer, `mediahealth.Lookahead` runs one
rule: starting after the current item, walk the wall in play order and probe
the first source that is **due** — no record, an expired verdict, or a passed
retry hold. The next items come first because they are first in the walk; the
rest of the wall follows, and the current item comes last (already on screen,
but a one-item playlist would otherwise never get its recheck). A cursor
remembers where the walk stopped, so each step reads one chunk of records,
not the whole wall, and resets when the playlist or the index changes. There
is no cap on the wall. With nothing due on the wall, the background sweep
takes one due record **not** on the wall, under a budget of 20 per hour, so a
fixed source heals without being cast again and a 100k-record store never
becomes 100k probes a day.

Pace: the next probe starts as soon as the previous one finishes — no gap.
Politeness is the shared `HostLimiter` alone: at most 2 in flight per host
(the walk itself is one request at a time), and a host answering 429/503 is
paused for its `Retry-After`; a source on a paused host is stepped over, not
held. Each background request may take up to 30 s
(`Prober.BackgroundItemTimeout`), not the cast pass's 5 s: nothing waits on
it, and a short deadline only turned slow answers into timeouts (arweave.net
serves some 404 pages after 7–27 s). A probe that wrote a record is governed
by that record alone, so a 429 with `Retry-After: 30` is due again after 30 s.
A probe that wrote **no** record is held in memory for 1 h: the source guard
or the URL-length limit refuses some sources without a record (a policy
objection is never a verdict), and so does a write the store refuses. Without
the hold such a source would stay due and block everything behind it. New
hints ride the next push, or the hint-only push below.

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
  push, bound to the cast generation. An unknown belief sends the current
  map, **the empty one included**: after a controld restart under a
  surviving page, the page may still hold hints that have since healed or
  expired, and only an explicit empty replacement clears them. That costs
  one send per cast with no belief, which replace semantics make harmless. Nothing is sent
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
exhaustion) count once; the last stage stays as the diagnostic. Only an
entry that can count takes part in a run: a device-local entry
(`detect`/`network`, `webgl`) is history only and never opens, continues or
ends one. Otherwise the uncountable `network` entry a play often starts with
opened the run and made every load timeout after it "the same play", so a
work that never rendered counted nothing. A `ready` from
a renderer that reports its own failures (`image`, `video`, `audio`, `model`)
writes `ok` and heals a broken record: the wall itself is the strongest
probe. A `ready` from an opaque embed (`iframe`, `object`, `pdf`) or with no
`mediaType` is counted as a play but writes no verdict, heals nothing and
leaves the record due: Chromium reports ready for its own error page inside
an iframe, so that ready proves the mount, not the work, and L0 has to keep
looking. A `ready` from an opaque embed that follows, in the same play, a
content-type check refused with a definite 4xx (any 4xx but 405, 408 and
416, which are about the check, not the resource) is the error page: it is
neither a success nor a reset, so a work refused on every play (429, 403,
404) concludes `broken` on its second play. A 401/403/407/429 seen by the
kiosk itself is `source_blocked`. After any other content-type failure (a
network failure, a 5xx, a 405) the fallback may well show the work, and its
`ready` ends the run as before. Two classes never count: device-local failures
(`webgl`, `detect`/`network`) and any failure observed while controld's own
internet verdict is offline (or not yet known) — an outage says nothing about
a source, and a verdict earned during one would be skipped before the play
layer could ever heal it. A real-renderer `ready` counts whatever the verdict
says: a work that rendered from cache rendered.

### The kiosk navigation observer (controld sees the iframe's real status)

A page may not learn anything about a cross-origin iframe's document, so the
player reports `ready` for Chromium's own 404 page. DevTools is not bound by
that rule. `offlinecache.KioskNavigationObserver` holds one passive CDP
session to the kiosk — `Network.enable` only, no interception, nothing that
can pause a frame — and reports the DOCUMENT load of every artwork frame to
`Service.ObserveNavigation`: the final HTTP status, or Chromium's error text
(`net::ERR_NAME_NOT_RESOLVED`, ...). The kiosk's root target reports the
document request of every cross-origin iframe the player mounts, so one
session is enough (verified on the office FF1). It is re-dialed on every
primary-CDP reconnect, like the User-Agent rewrite, and closed with it.

An observation is matched to a wall source by URL once the fragment and the
player's own query params (`display_mode`) are stripped, from the current
index forward. Then it is folded through the SAME play-run and corroboration
rules as a ring entry: a status ≥ 400 is a `detect` failure with that status
(404/410 → `source_missing`, or `source_unreachable` for a content-addressed
source; 401/403/429 → `source_blocked`, 5xx →
`render_failed`), a network error is a `load` failure with Chromium's reason,
a canceled load (the player replacing or giving up on a frame) and a 2xx/3xx
are ignored — a 2xx proves the mount, not the work. An observation never
concludes on its own: two failed plays, or one over a standing L0 verdict,
and only while the device is online. The observed 404 also marks the play
refused, so the error page's `ready` that follows is neither a success nor a
reset. `mediaHealth.kioskObserverDisabled: true` turns the observer off.

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
  `issue` (below) rides beside `state` when there is one.
  Nothing else is pushed; freshness (`checkedAt`) stays on `getMediaHealth`.
  The raw `outcomes` ring and `skippedIndices` are stripped from the
  notification and from direct `checkStatus` replies (hub and relayer): they
  are inputs, consumed on the device only.
- `getMediaHealth` (controller tier, query class, no arguments) returns the
  whole playlist on screen: `{ok, status, items:[{index, id, title, state,
  category, reason, layer, skipped, unskipped, checkedAt, issue, issueSince,
  nextCheckAt, failedPlays, plays:{ok,fail}}], total, truncated, castSeq}`,
  `skipped`/`unskipped` by the same rule as the push. The empty request
  returns the first 2048 rows (`truncated`); a request with `offset` (≥ 0),
  `limit` (1–1000, default 500) or `filter` (`all` | `attention`: broken,
  suspect, any `issue`, or skipped) is paged — `matched` counts the rows the
  filter admits across the wall, `nextOffset` is present while more remain,
  every row keeps its absolute `index`, and `castSeq` is the consistency
  token (a change between pages means the wall changed). Anything else is
  rejected: the storm gate dedupes on type + arguments, so the argument space
  stays bounded by the playlist's page count. No source URLs.

### Honest reporting of temporary failures: `issue`

A temporary failure has no verdict, so it reads `state: unknown` — before this
field, exactly like a source never checked. `issue` is the latest REAL
attempt's problem, beside `state`, never instead of it. It is omitted while
`state` is `broken` (the category says it) and when there is none.

| `issue` | Set by | Cleared by |
|---|---|---|
| `no_answer` | a background probe (30 s) got no response: timeout, refused, reset, TLS | any answer, from a probe or the kiosk |
| `dns` | the name did not resolve, short of NXDOMAIN (which is `broken` at once) | any answer |
| `server_error` | a 5xx without `Retry-After` | any answer |
| `rate_limited` | 429, or 503 with `Retry-After` | the next probe answering, or a kiosk 2xx |
| `access_denied` | 401/403/407 to the probe (may be the probe's identity) | the next probe answering, or a kiosk 2xx |
| `play_failed` | one counted failed play (player ring or kiosk observer) | any `ready` that ends the failure run (an opaque embed's included — not the error page shown after a refused content-type check), or the verdict at 2 |

Rules: only real attempts write an issue — the cast pass's 5 s deadline
expiring writes nothing at all (no record, no hold, no streak count; the
lookahead's 30 s probe is the real attempt), and neither does an unreachable
result while the device reached no other origin, nor a play while offline. A
2xx document the kiosk observer sees clears a probe-side issue and ends the
unreachable streak, but never writes `ok` (a 200 does not prove the work
rendered) and never clears `play_failed`. A probe answer the vocabulary does
not name (a 406, 408, 416) still clears a probe-side issue — the source
responded — while an attempt with no answer during an outage leaves the
standing issue as it is. `issueSince`
is when the current run of trouble began, kept across a change of kind and
reset when the issue clears. `nextCheckAt` is the later of the verdict's
recheck and a transient hold. `failedPlays` counts the failed plays in the
current failure run (a verdict at 2); any `ready` but the error page ends it. The raw `lastError` text never leaves the
device: it carries the host and path.

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
  "castConcurrency": 4,
  "ipfsGateways": ["https://ipfs.filebase.io", "https://gateway.pinata.cloud"],
  "arweaveGateways": ["https://arweave.net"]
}
```

`disabled` is the kill switch: no store, no pass (casts forwarded unprobed,
the pre-#304 behavior), no hints, no lookahead, no per-item status, no kiosk
observer. `kioskObserverDisabled: true` turns off only the kiosk navigation
observer (the one media-health piece that holds a socket to the kiosk). The
retired `sourceProbe.disabled` is read as `mediaHealth.disabled`. `lookahead:
-1` disables the lookahead and the background sweep. `castItems` and
`castConcurrency` tune the cast-time pass for an origin that still
rate-limits the defaults; the per-host cap (2 in flight) is fixed, and the
lookahead has no gap — it is paced by the per-host rules alone.

## Boundaries

- `mediahealth` never imports the transports, the router, `offlinecache` or
  `uarewrite` (pinned by `mediahealth/importlint_test.go`); it never builds an
  HTTP client. `offlinecache` implements its `Fetcher`.
- The store observes. Playback is gated only by the hint and by the existing
  all-dead rejection; L1 never gates.
