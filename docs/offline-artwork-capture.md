# Capturing DP-1 Artwork for Offline Playback

This document describes how `feral-controld` captures a DP-1 artwork —
web-based (HTML+CSS+JS, canvas, WebGL, WASM) via a headless-Chromium
capture, or any other single-file mime type (image, video, audio, SVG,
`model/gltf`, PDF, and unrecognized-but-still-single-file types) via a
browser-free direct HTTP download — so `ff-player` can play it back with
no network access, and how the result is stored and replayed. Live/VOD
manifest-based streaming — HLS (`.m3u8`) or DASH (`.mpd`) — is the one
source type this pipeline does not support at all (§3.3, §8).

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
requests**, not to statically parse its HTML/CSS for references.

Every other DP-1 item — image, video, audio, SVG, `model/gltf`, PDF, and
anything else that resolves to a single-file `source` (including an
empty/unrecognized `Content-Type`, treated as best-effort downloadable
rather than rejected) — is the opposite case: the kiosk player renders it
as a native element (`<img>`/`<video>`/`<audio>`/`<object>`/a
non-scripted `<iframe>`) that requests the bare `source` URL directly, so
there is exactly ONE dependency to cache: the file itself. Running a
whole headless browser just to observe that one request would cost a
second Chromium process and gigabyte-scale memory pressure for zero
benefit, so `feral-controld` downloads these directly over HTTP instead
(§3.3) — no browser involved at all.

`classify.go` decides which path an item takes from its resolved
`Content-Type` (falling back to a bounded ranged-`GET` probe when an
origin rejects `HEAD` — see `ClassifyProbeRangeBytes`'s doc): `text/html`/
`application/xhtml+xml`/JS content types are `ClassSoftware` (headless
capture, this section); everything else is `ClassMedia`/`ClassUnknown`
(direct download, §3.3) **except** an HLS or DASH live/VOD manifest
(`application/vnd.apple.mpegurl`/`application/x-mpegurl`/`audio/mpegurl`/
`application/dash+xml`, or a `source` path ending in `.m3u8`/`.mpd`,
checked by URL extension BEFORE any network round trip), which
classifies as `ClassStreaming` — the only class `DownloadItem`/
`DownloadPlaylist` reject outright
(`ErrUnsupportedMediaClass`), since a live/VOD manifest has no fixed
byte-for-byte content a one-shot download or a static blob-store replay
could faithfully serve (§3.3, §8).

Two source shapes never reach a network probe at all:

- An RFC 2397 **`data:` URI** is `ClassInline`. Its media type is parsed
  straight out of the URI (a bounded scan for the metadata-terminating
  comma — the payload is never decoded), and the item is then *skipped*
  rather than queued: its bytes already travel inside the playlist body
  that `SavePlaylist` persists, so the player reads them back offline
  from the playlist record itself and there is nothing to fetch. This is
  a legitimate exclusion, not a failure — it does not count toward
  `classifyFailed` and does not fail the command. Handing one to
  `http.Client` (which cannot dial `data:`) is what previously made every
  inline cover image a permanent, unretryable classification failure.
- A source that is **not safe to dial** is rejected with
  `ErrUnsafeSource` before any probe, download, or `Page.navigate` — see
  §9.

## 2. Pipeline overview

```
Discover  →  Capture  →  Store  →  Replay
(what does   (fetch the   (content-  (serve
 it load?)    exact        addressed   locally,
              bytes)       dedup)      no network)
```

- **Discover + Capture** (software only, `ClassSoftware`): `downloader.go`
  spawns a separate headless Chromium (`:9223`, its own user-data-dir);
  `capture.go` attaches an event-driven CDP session (`cdpsession.go`) to
  observe `Network` domain events for a bounded window, then fetches each
  observed URL's exact bytes out-of-band.
- **Direct download** (everything else but `ClassStreaming`,
  `ClassMedia`/`ClassUnknown`): `mediacapture.go`'s `MediaCapturer` issues
  one plain HTTP `GET` for `item.Source` (redirects followed
  transparently by the shared `http.Client`) and streams the response
  straight into the blob store — no browser process at all. See §3.3.
- **Store**: `store.go` content-addresses the bytes (sha256) into a shared
  blob store, deduplicated across items/playlists, plus one
  `items/<sha256(item.source)>.json` record per source — shared unchanged by both
  capture paths above.
- **Replay**: `replay.go` intercepts `Fetch.requestPaused` on the kiosk
  Chromium (`:9222`) and fulfills from the local blob store or a loopback
  static server, without rewriting a single byte of the artwork's own code
  or the signed playlist's `source` field.

Both browsers stay independent: the headless downloader (`:9223`) never
shares state with the kiosk (`:9222`), so downloading does not disturb
whatever is currently playing on the player surface. (State isolation is
only half of that promise — resource contention is the other half, handled
by the admission gate in §2.1.) This separation is
enforced only by the two processes listening on different ports, so
`bootstrap.go`'s `safeHeadlessDebugPort` defends against
`offlineCache.headlessDebugPort` being misconfigured to collide with the
kiosk's own CDP port: without that guard, `Downloader.Acquire`'s
readiness probe would succeed against the already-running kiosk endpoint
instead of a freshly spawned headless process, and capture would
discover and navigate the kiosk's own live page target — visibly
corrupting whatever is on screen. Ports are compared numerically, and a
colliding configured port is coerced to a port guaranteed distinct from
the kiosk's — normally `DefaultHeadlessDebugPort`, but stepped past it if
the kiosk's own (unusually configured) port happens to equal that default
too, so the fallback itself can never reintroduce the same collision
(logged at `Error`, mirroring `staticserver.go`'s `safeLoopbackAddr` guard
for the same class of "config typo breaks an isolation invariant" hazard).

### 2.1 Resource-aware admission (`admission.go`)

Process separation keeps capture from *touching* the kiosk's state, but not
from competing with it for the machine: the device runs the kiosk Chromium
resident at all times, chronically near its memory limit, and the second
headless Chromium a `ClassSoftware` capture spawns has field precedent for
destabilizing playback (the real-GPU capture flags had to be replaced with
software WebGL after device-wide freezes — see §3's flag notes). An
admission gate therefore sits in front of the capture worker and defers
STARTING queued jobs while the device is under pressure:

- **Signals**: memory used% and CPU temperature, decoded from the
  `sysmetrics` D-Bus signal monitord already publishes (~2s cadence). The
  mediator forwards the raw payload to the gate via
  `Runtime.SysMetricsSink` → `mediator.SetSysMetricsObserver`; decode and
  policy live entirely in `offlinecache`. GPU busy% is deliberately NOT
  gated on (kiosk WebGL art keeps it chronically high; the iGPU shares the
  package thermal domain, so CPU temperature proxies GPU heat; the capture
  Chromium renders with software WebGL anyway).
- **Class-aware thresholds**: `ClassSoftware` (headless Chromium, hundreds
  of MB to >1 GiB) blocks at 80% memory / 75°C; everything else (a plain
  streamed GET) only at 90% / 85°C. Temperature is deliberately the
  strict axis and memory the generous one: the capture Chromium renders
  WebGL on the CPU (SwiftShader), so its dominant hazard is sustained CPU
  load heating the package and degrading live playback. Every threshold
  is written as a **derived berth beneath the layer it protects against**
  rather than as an independent number — 75°C is `WatchdogCriticalCPUTempC
  (93) − softwareThermalHeadroomC (18)`, and that 18°C is sized to cover
  the heat a capture bounded by `headlessLimits` can add. Latching
  hysteresis (resume = block − 5 units) prevents flapping at the metrics
  cadence.

  > **Calibration warning — the derivation is not self-evidently
  > well-sized.** It reserves an 18°C berth beneath the watchdog, but on
  > the FF1 target `k10temp` measures **77.8–79.2°C during normal WebGL
  > playback** (40/40 samples over two minutes above the 75°C block line,
  > 0/40 below the 70°C resume line). The berth therefore exceeds the
  > ~13.8°C of headroom the hardware actually has, and the software bucket
  > is not "defer under pressure" but "never admit" outside a cold-boot
  > window. Combined with the no-deadline policy above, that means software
  > captures queue forever. **This number, not the queue policy, is where
  > the intended behavior lives** — resizing it (or scheduling captures for
  > genuinely idle windows instead of gating on temperature) is the open
  > work, and it needs the thermal delta of one pinned capture measured
  > before picking a value.
- **Deferral is not an error**: a deferred item stays `queued` on the wire
  (no new state in the app contract), remains clearable without `busy`,
  and in-flight captures are never aborted.
- **Selection is skip-scan, not head-only** (`dequeueAdmitted`). The gate's
  verdict depends only on a job's CLASS, and the two classes are gated very
  differently — software strictly on temperature, media permissively. Under
  the original head-only pop, one thermally deferred software job blocked
  media downloads behind it in the shared FIFO that the gate would have
  admitted instantly. The worker now takes the first job the gate currently
  admits, so a hot device keeps draining media work while its software jobs
  wait. FIFO order is preserved **within** each class, which is the only
  ordering anything depends on; only cross-class overtaking is intended.
- **Deferral never drops a job, for any class.** A job leaves the queue
  only by being processed or by an explicit clear — never on a timer.
  Caching an artwork is optional, deferrable work; keeping the panel stable
  is not, so a device under pressure postpones downloads rather than
  turning them into client-visible errors it had no way to avoid.

  This replaced a `maxDeferSeconds` bound that failed a job deferred too
  long. Its justification was that "the FIFO cannot be wedged forever
  behind one item on a persistently hot device" — which skip-scan answers
  directly: nothing is wedged, so nothing has to be failed to unwedge it.
  The config key is still accepted but **inert**, and setting it logs a
  warning saying so rather than being quietly ignored.

  What is unbounded is the WAIT, not any resource: the queue is
  memory-only, capped by `maxQueueLen` (4096) with `enqueue` idempotent per
  source, and dropped on restart. Saturating it would take thousands of
  *distinct* permanently-deferred sources, so `ErrQueueFull` is the
  theoretical backstop, not the realistic outcome. Starvation is not the
  mirror risk either: software is not waiting behind media, it is waiting
  on temperature, and media draining meanwhile costs it nothing.

  **Known gap — the software path can be silently non-functional.** The
  realistic consequence is that `queued` persists indefinitely with the
  cause visible only in the daemon log, where it is indistinguishable from
  healthy in-progress work. On a device whose steady-state temperature sits
  above the software block threshold (see the calibration warning above),
  software captures never start *and* never fail. The daemon log is not
  reachable from the mobile app, so today there is no supported way for a
  client or a field engineer to tell "deferred" from "wedged". Surfacing
  the deferral reason on the status payload — an additive field, not a new
  state — is what closes this, and it is a correctness gap rather than
  polish.
- **Fail-open**: no sysmetrics for `metricsStaleAfterSeconds` (default
  15s), or nonsensical readings (zero capacity/temperature), admit
  unconditionally — absence of metrics is not evidence of pressure,
  downloads are user-initiated, and the watchdog/firmware backstops hold
  either way. Shutdown likewise bypasses the gate: `Stop()` drains via the
  ungated path so a hot device can never stall daemon shutdown.
- **Config**: `offlineCache.resourceGate` — on by default whenever the
  offline cache is enabled; `"disabled": true` is the kill switch, and
  every threshold is overridable (see `config.example.json`).

The admission gate covers *starting* work; the **headless resource cap**
(`offlineCache.headlessLimits`, also on by default) covers the window the
gate cannot: a capture already in flight is never aborted and can run for
up to the 30-minute transfer ceiling. `downloader.go` therefore wraps the
capture Chromium spawn in a transient systemd scope (`systemd-run --user
--scope`, a runtime invocation — deliberately NOT unit-file properties,
which would drag this onto the full-image rail) **plus an argv wrapper**
(`env -u DBUS_SESSION_BUS_ADDRESS taskset -c <cpus>`). Both halves are
load-bearing; see "Why the scope alone does not hold" below.

- `CPUQuota` (default 300%) caps total cycles — bounding the heat and
  scheduling pressure an in-flight capture can generate; the CPU pin
  (default: first quarter of the machine's logical CPUs, "0-3" on the
  16-thread target) additionally pins it so short bursts cannot light up
  every core's boost clocks (a quota alone still allows that); `MemoryMax`
  (default 2 GiB) turns a runaway capture into a clean cgroup OOM kill of
  the *capture* process — a failed, retryable job — instead of a global
  OOM-killer roll of the dice against the live kiosk.
- Teardown of a scoped generation goes through `systemctl --user stop`
  (cgroup kill: Chromium first, then `systemd-run` exits and the normal
  reaper observes it) — never a raw kill of `systemd-run`, which would
  orphan Chromium inside the scope still holding the debug port and
  profile lock. A scope also survives a crashed daemon by design, so the
  first spawn of each daemon run sweeps stale `feral-offline-capture-*`
  scopes before probing.
- Support is probed once, against the **exact spawn shape** a real capture
  uses (scope properties *and* argv wrapper, running `/bin/true`). The two
  capabilities are probed **independently** and then composed — a combined
  probe cannot say which half failed, so a broken wrapper would surrender
  the scope with it and blame systemd in the log while systemd was fine.
  The resulting matrix, each cell logging what is left:

  | available | in force |
  |---|---|
  | scope + pinned wrapper | quota, memory ceiling, CPU pin |
  | scope + env-only wrapper | quota, memory ceiling |
  | scope, no wrapper | quota and memory ceiling *until Chromium escapes* |
  | pinned wrapper only | CPU pin |
  | neither | nothing (pre-limits behavior) |

  A broken environment slows nothing and blocks nothing. The third row is
  the one cell where the `resource limits active` line reads more
  optimistically than reality: with `env` itself unusable there is nothing
  stopping the escape, so the cgroup limits hold only until Chromium
  re-parents. The containment check below corrects the record on that very
  spawn.

#### Why the scope alone does not hold

An FF1 hard-reset at the instant a capture started, with the cap nominally
enabled. Neither limit was actually in force, for two independent reasons
— either alone is enough to void the protection:

1. **Chromium escapes the scope.** Given a reachable user session bus,
   Chromium moves its own browser process into a transient
   `app-org.chromium.Chromium-<pid>.scope` under `app.slice` at startup —
   a sibling of ours with every property at its default (`CPUQuota=
   infinity`, `MemoryMax=infinity`). Everything set on our scope is
   silently voided. `env -u DBUS_SESSION_BUS_ADDRESS` denies the bus;
   headless capture has no other use for it.
2. **`AllowedCPUs=` writes nowhere.** systemd accepts and reports the
   property, but applying it needs the `cpuset` controller delegated to
   the user manager. On the FF1 only `cpu`, `memory` and `pids` are
   (`user@.service` `DelegateControllers`), so no `cpuset.cpus` file is
   ever created and the pin lands nowhere. `taskset` sets CPU *affinity*
   instead — a plain process attribute that children inherit and that a
   cgroup migration does not reset, so the pin survives even an escape
   that voids everything else.

The pin is the limit that matters most, because it is the one that bounds
package power draw — and an unpinned SwiftShader capture lighting up all 16
threads was the load in flight when the device reset. Whether that draw was
the mechanism is the leading hypothesis, not a proven fact (see **Open:**
below); the pin is the cheapest defense against it either way, which is why
the matrix above drops it last rather than first. Note that this is a
property of the matrix, not an ordering: the scope and the wrapper are
probed independently, so neither is ever surrendered to buy the other.

Two guards keep this from silently regressing. The probe exercises the
real shape (the previous `/bin/true`-only probe passed happily while both
limits were inert, then logged that they were "active"). And every scoped
spawn checks containment directly: `systemd-run --scope` creates the unit
and then **execs in place**, so the PID `Start()` returned is the capture
Chromium's own browser process, and `/proc/<pid>/cgroup` must name the unit
we spawned it into. An escape moves that process to another cgroup but
never changes its PID, so the check is exact — and it warns loudly instead
of reporting limits that enforce nothing.

One trap worth naming, since the CPU spec crosses two parsers: systemd's
`AllowedCPUs=` accepts whitespace-separated lists (`"0 1 2 3"`) that
`taskset -c` rejects outright. Handing such a spec to taskset would make
every spawn fail to exec and drop capture to a bare, fully uncapped
Chromium — strictly worse than not pinning. The wrapper therefore gates the
taskset element on `countAllowedCPUs`, the same parser `alignHeadlessLimits`
uses, so the two always agree about what a CPU list is.

**Open:** whether the default pin (4 CPUs @ 300%) is actually *sufficient*
to prevent the brownout is unmeasured. Nothing was capped during the field
reset, so it yields no data on real draw; the fix restores the intended
protection without proving the protection is enough. If a reset recurs with
the pin verifiably applied, the lever is a tighter pin (`0-1`) via
`offlineCache.headlessLimits` — a config change, no rebuild.

Delegating `cpuset` to the user manager would make `AllowedCPUs=` real and
would also cap the escaped scope, but it is a system-level unit change —
the full-image rail — so it is deliberately *not* part of this
package-rail fix. The `--property=AllowedCPUs=` is still passed: it costs
nothing, documents intent where an operator looks first, and becomes the
enforcement the day that delegation lands.

**The gate and the cap are one system, not two knobs.** Three couplings
keep them from drifting into a combination that only works on one device:

1. **Memory** — the software memory threshold is *derived* from the cap:
   `effective = min(softwareMaxMemoryPercent, memorySafetyCeilingPercent −
   memoryMaxBytes as a percentage of this device's RAM)`. Taking the
   minimum means the derived term can only tighten an operator's setting,
   never loosen it. This is not cosmetic: with a 2 GiB cap, admitting at a
   static 80% peaks near 92% on a 16 GB device (safe, under the watchdog's
   95%) but at **105% on an 8 GB device** — the OOM the gate exists to
   prevent. Deriving it yields 77.5% and 65% respectively, correct on both.
   With the cap disabled the reserve is 0, the derived term drops out, and
   the static threshold governs.
2. **CPU quota vs. CPU set** — `cpuQuotaPercent` can never exceed
   `100 × len(allowedCpus)`; above that the cpuset is the real limit and
   the quota silently stops being one. A configured pair that violates
   this is clamped to the reachable value and the correction is logged.
   The defaults are derived together (`DefaultHeadlessCPUShareOfPinned`),
   so they stay aligned on any core count instead of only on the
   16-thread target.
3. **Temperature** — the gate's 18°C berth below the watchdog's 93°C is
   the budget for the heat a capture may add, and `headlessLimits` is what
   bounds that addition. Raising the quota or widening the cpuset without
   revisiting the berth is the amendment hazard to watch.

**Not** attempted: telling Chromium how many CPUs to assume. There is no
reliable flag for it, and whether Chromium's thread-pool sizing observes
its CPU restriction at all is glibc-version dependent. The pin is enforced
by the kernel regardless of how many threads Chromium spawns, so the worst
case is some extra mostly-idle threads — cheap, and not a heat or
correctness problem. Speculative flags (`--renderer-process-limit`,
`--single-process`) were rejected as capture-fidelity risks for no bounded
gain.

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

`capture.go` enables the CDP `Network` and `Page` domains and observes
`Network.requestWillBeSent`/`responseReceived`/`loadingFailed` for a
bounded capture window (`offlineCache.captureWindowMs`) after navigating to
the item's `source`. For each distinct URL:

> `Fetch` is enabled on the capture session too, but for a different
> purpose than replay's: capture uses it only to **police** where the page
> may connect (§9's capture-side source guard), never to fulfill a request
> from cache. Discovery of which URLs were requested still comes from the
> `Network` events described here.

- **Found on a successful (2xx) response, captured via `GET`/`HEAD`** →
  fetch the exact bytes via an out-of-band `http.Client` request using
  that SAME method (see §4 for why not `Network.getResponseBody`, and
  §4.7 for why method matters at all), hash into the blob store, record
  the resource. The response body is streamed straight from the HTTP
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
- **Found on a successful (2xx) `OPTIONS` response (a CORS preflight)**
  → store a canonical EMPTY body directly (`store.WriteBlob` on an empty
  reader), never an out-of-band re-fetch. A bare re-issued `OPTIONS`
  carries none of the real browser preflight's `Origin`/
  `Access-Control-Request-Method`/`Access-Control-Request-Headers`
  request headers, and many CORS-aware servers only special-case
  `OPTIONS` (returning their `Access-Control-Allow-*` response headers)
  when those preflight-specific request headers are present — otherwise
  treating it as an ordinary request. Re-fetching could therefore
  observe a genuinely different status/headers than the live browser
  preflight did, or fail outright. This is safe to skip entirely because
  a CORS preflight's response BODY is never exposed to page JS per the
  Fetch spec — replay only needs the status/headers already captured
  live from `Network.responseReceived`, which are recorded regardless of
  this shortcut. See §4.7.
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

The out-of-band fetches above (everything in the first bullet, plus the
206 fallback) happen AFTER `captureWindowMs` closes, in a second,
separate phase ("finalization") whose START of new fetches is bounded by
its own fixed internal deadline (`captureFinalizeWindowDefault`, 60s),
independent of `captureWindowMs` — each transfer itself is bounded
separately, see "Why both download paths get their own HTTP client". `captureWindowMs` only bounds *passive* CDP
observation; finalization makes *active* outbound HTTP requests, one
resource at a time. Without a
finalization-phase deadline, a page whose observation window closes with
many stalled/slow-responding resources still outstanding could keep
`capture.go` — and therefore the single download worker slot it holds for
the ENTIRE capture — busy far longer than `captureWindowMs` alone would
suggest. Once the finalization deadline elapses, every resource whose turn had
not yet started when the check next runs is left unfetched and recorded
as `finalization_deadline_exceeded:<url>`; anything already fetched
before that point keeps its result. (A fetch already in flight when the
deadline hits is a separate, pre-existing case: its outbound HTTP
request is itself bound to the same deadline via `req.WithContext`, so
the transport typically cancels it and it fails through the ordinary
`fetch_failed:<url>` path — this fix does not change that.) Either way,
the record `Capture` saves is an honestly PARTIAL one
(`Coverage.Complete: false`), never a hang.

### Why both download paths get their own HTTP client

Neither body-download path uses the daemon-wide HTTP client. That client
carries a 30-second `http.Client.Timeout`, and in Go that timeout covers
the **entire** request including the response body — so while it was
wired in, every asset that took more than 30 seconds to transfer failed
unconditionally, regardless of how healthily it was progressing. That is
incompatible with what this subsystem is for: the store's budget is
measured in GiB, and the loopback static server below only earns its keep
on blobs over 200 MB, a size no device uplink moves in 30 seconds. The
large-asset replay path was therefore unreachable in practice, not merely
slow.

`bootstrap.go` gives `capture.go` and `mediacapture.go` a client built
with `wrapper.NewHTTPClientWithoutTimeout` instead, and each path bounds
itself explicitly, as that constructor's contract requires:

Both paths bound each body transfer with the SAME pair of limits
(`transfer.go`'s `resourceTransfer`), because both are downloading the
same kind of asset — §4.4's 1.1 GB video is reachable either way — and so
should tolerate the same slowness and give up on the same stall:

- `resourceStallTimeout` (60s) aborts a transfer that stops delivering
  bytes. This is the bound that does the real work: a wedged origin is
  detected in a minute no matter how large the asset is.
- `resourceTransferTimeout` (30 minutes) is the absolute ceiling, so a
  byte-per-second trickle that never technically stalls still cannot run
  forever. Sized for ~1 GB at roughly 5 Mbps, the slow end of a real
  device uplink.

Splitting "is it stalled?" from "how long may it take?" is what lets the
ceiling be generous. A single fixed deadline cannot serve both: sized to
cut off a pile of wedged resources it kills a healthy gigabyte transfer,
and sized for the gigabyte transfer it lets wedged resources hold the
single capture worker for half an hour.

The software path additionally keeps its finalization window
(`captureFinalizeWindowDefault`, 60s), but strictly as a gate on whether
another fetch may **start** — never as a bound on a transfer already
streaming. An earlier revision passed it down into each fetch too, which
meant a healthy multi-hundred-MB asset was cancelled mid-stream at 60s
and recorded as a permanent `partial` however fast it was downloading:
the documented §4.4 case could not actually reach `ready`. Worst-case
wall clock for the phase is therefore the window plus one in-flight
transfer's own ceiling, not (resource count) x anything.

Everything else in the subsystem keeps the daemon default: the
classifier's probe and the CDP calls to localhost are small, fast
requests that *should* fail fast. `DialPageSession` additionally bounds
itself at 15s via `req.WithContext`, so the capturer's timeout-free
client does not loosen the dial path.

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

`Release` frees the single-job capture slot only AFTER recording (under
its mutex) whether an idle-teardown should be scheduled for the process
it is releasing — never before. An earlier version freed the slot first;
that left a window where a concurrent `Acquire` could take the
newly-freed slot, see no pending teardown yet (there was nothing to
cancel), and start reusing the process, while `Release` — resuming a
moment later — would still go on to schedule that teardown anyway,
unaware a new job had already claimed the process. The teardown's timer
firing later would then kill Chromium out from under that active job.
Recording the decision before freeing the slot closes the window: any
`Acquire` that manages to take the slot is guaranteed (by the channel
send/receive happens-before relationship) to observe `Release`'s
teardown decision already made, and correctly cancels it via the
existing reuse path instead of racing past it unaware.

`Capture`'s bounded observation wait (`waitForObservationWindow`) blocks
on a `select` between the per-navigation timeout (`navCtx`) and the
caller's own `ctx`. Because `navCtx` is derived from `ctx` via
`context.WithTimeout`, canceling `ctx` (e.g. `Service.Stop` racing an
in-flight recapture) closes BOTH `Done()` channels, and Go's `select` is
free to resolve via either case once both are ready. `Capture` must not
rely on which case actually fired: falling through to
`resolveResources`/`SaveItem` on a canceled `ctx` would resolve
everything still in flight as a failure and could overwrite an item that
was already fully captured and ready with an incomplete record.
`waitForObservationWindow` re-checks `ctx.Err()` after the `select`
unconditionally, so the outcome is correct regardless of which branch
the runtime happened to pick.

Because the headless Chromium is one long-lived process reused across
every capture job, `Capture` also resets per-origin browser state
immediately before each navigation (`resetTargetState`): an unconditional
`Network.clearBrowserCache` plus a `Storage.clearDataForOrigin` scoped to
the item's own origin (cookies, IndexedDB, Cache Storage, Service
Workers, local storage — everything `storageTypes: "all"` covers). A
plain same-origin HTTP cache hit from an earlier item is harmless on its
own (`fetchAndStoreBody` always re-fetches bytes directly over HTTP
regardless of the browser's cache state), but a Service Worker or
Cache-Storage entry left behind by an earlier item sharing this item's
origin (e.g. two playlist items hosted on the same generative-art
platform's domain) could intercept or seed this item's requests entirely
within the browser, so the request would never reach the network and
`resolveResources` would never learn the URL exists at all. This clear
step removes that cross-item leakage without the cost of a fresh
profile/process per job. If the origin cannot be derived from a
malformed `item.Source` (which is about to fail `Page.navigate` moments
later anyway with a clearer error), only the origin-scoped clear is
skipped; the unconditional cache flush still runs.

`offlineCache.maxDiskBytes` deliberately does NOT default to "unlimited"
the way most `OfflineCacheConfig` fields default to "off"/"unset" —
`OptionsFromConfig` falls back to `DefaultMaxDiskBytes` (10 GiB) whenever
config omits or zeroes it. This feature exists specifically to cache
potentially gigabyte-scale software-artwork assets (§4.3's 1.1 GB video)
on disk-constrained embedded devices; a config that merely flips
`offlineCache.enabled` to `true` without also setting `maxDiskBytes`
must not silently mean "fill the disk" — `Service.enforceDiskLimit` is a
complete no-op when `maxDiskBytes <= 0` (see its doc), so an unset budget
combined with `downloadPlaylist` against large assets could exhaust the
filesystem.

`maxDiskBytes` is enforced as a genuine hard bound, not just a
"try to make room" heuristic: `enforceDiskLimit`'s eviction loop normally
protects the just-captured item (`justCapturedID`) while evicting older
items oldest-first, but if the cache is *still* over budget once every
older item has been evicted — i.e. the just-captured item's own size
alone exceeds `maxDiskBytes` — it evicts the just-captured item too
rather than leaving the cache permanently over budget. The caller sees
this as the item transitioning to `not_cached` immediately after
appearing to complete, with `Coverage.Reason` explaining that it alone
exceeded the disk budget.

The bound is enforced on BOTH sides of a capture, because eviction only
on the way out is not enough for a cache that must roll over.
Post-capture, `enforceDiskLimit` trims back under `maxDiskBytes` as
above. Pre-capture, `service.process` first runs
`Service.reclaimDiskForCapture`, which evicts oldest-first (never the
item about to be captured, and returning quietly rather than warning
when the target is unreachable — see its doc) until at least
`maxDiskBytes/8` of the budget is free. Without that step, a store
sitting at its ceiling starved every new capture up front: both
capture pipelines seed their disk
budget with the store's *remaining* room, and the post-capture eviction
only runs after a *successful* capture, so once full the cache froze on
its oldest contents forever (feral-file/ffos-user#229 review finding).
An item larger than the 1/8th headroom floor still captures with
whatever room eviction actually freed — degrading only itself (partial
coverage) rather than wiping the rest of the cache for one oversized
item.

### 3.3 Direct-download path for media and other single-file items

`mediacapture.go`'s `MediaCapturer` is the browser-free counterpart to
`capture.go`'s `Capturer` for every class but `ClassSoftware` and
`ClassStreaming` (see §1 and `classify.go`'s `MediaClass` doc). It mirrors
`Capturer`'s shape (`Capture(ctx, item) (*ItemRecord, error)`) closely
enough to slot into `service.go`'s existing job queue/state machine
unchanged — `service.captureForClass` is the one place that branches
between the two, based on the `MediaClass` sampled once at classify time
and carried on the queued job (`captureJob.class`), rather than
re-classifying (a second network round trip) or risking a different
answer the second time around.

- **One fetch, one resource.** Unlike `capture.go`'s open-ended
  dependency discovery, a `ClassMedia`/`ClassUnknown` item has exactly
  one thing to cache: `item.Source` itself. `MediaCapturer.Capture` issues
  a single `GET` via the shared `httpClient` (redirects followed
  transparently — `http.Client`'s default behavior, never handled
  manually here) and streams the response body straight into
  `store.WriteBlob`, exactly like `capture.go`'s `fetchAndStoreBody` does,
  so a gigabyte-scale video is never buffered whole in memory here
  either.
- **Stored under the ORIGINAL source URL, never a resolved redirect
  target.** The resulting `Resource.URL` is always `item.Source` — even
  though the underlying `*http.Response` may reflect a fully-resolved
  redirect chain. This is safe (unlike capture.go's software path, which
  must preserve each redirect hop as its own `Resource` — §4.1) because
  the kiosk's native `<img>`/`<video>`/`<audio>` element only ever
  requests the bare `item.Source`; there is no redirect hop for replay to
  faithfully reproduce, so storing the final resolved bytes directly
  under the request URL the kiosk will actually make is both correct and
  simpler.
- **CORS headers are captured the same allowlisted way** as `capture.go`
  (`filterReplayableHeaders`, §4.6/§5's `Resource.Headers` bullet):
  `ff-player`'s `<video crossOrigin="anonymous">` element CORS-checks its
  response exactly like a cross-origin `fetch()`/XHR would, so an offline
  replay missing those headers would still fail Chromium's own CORS
  enforcement even with byte-correct status/body. Getting those headers
  onto the wire in the first place requires sending an `Origin` header on
  the capture request too — see §4.6's expanded note — since this path has
  no browser to send one on its own.
- **A fetch failure is a hard error, not a partial-coverage record.**
  Because there is only one resource, a failed `GET`
  (`http_error(<status>)`, a transport-level `fetch_failed`, or the disk
  budget already exhausted before the fetch is even attempted — next
  bullet) means the WHOLE item failed to download; `Capture` returns an
  error rather than saving a permanently-broken single-resource record,
  the same way `capturer.Capture` already reports a failed
  `Page.navigate` as a hard error rather than an honest-partial one — the
  entry point itself never loaded either way.
- **A JSON `.gltf` manifest with external dependencies is the one case
  this path downgrades its own coverage.** After a successful download,
  a manifest identified by Content-Type (`model/gltf+json`) or URL
  extension is parsed for its spec-defined `buffers[].uri`/
  `images[].uri` entries; any non-`data:` URI is a separate external
  file this path does not capture, so the record is saved with
  `Coverage.Complete=false` (`gltf_external_dependency:<uri>` reasons)
  and the item reports `partial` instead of dishonestly claiming
  `ready` while fail-closed replay would fail those requests offline.
  glTF is checked because its manifest makes the dependency set exact
  and enumerable; SVG's cannot be (see §7's known-limitations entry).
  Binary `.glb` (self-contained by design) and manifests whose URIs are
  all embedded `data:` payloads keep complete coverage, and a manifest
  the checker cannot read or parse also keeps it — the checker's own
  limits must never turn a working download into a downgrade.
- **Reuses `capture.go`'s disk-budget machinery**, factored out as
  `newDiskBudgetFromStore` (seeded with the store's REMAINING room —
  `maxDiskBytes` minus current usage, never the full configured ceiling
  — see that function's doc): a single `budget.reserve()` call covers the
  item's one resource (no per-resource loop needed, unlike `capture.go`'s
  multi-resource case), and `service.process`'s post-capture
  `enforceDiskLimit` eviction runs unchanged afterward for both paths.
- **Needs no `Downloader`/dialer at all** — `bootstrap.go` wires
  `NewMediaCapturer` with just the shared `httpClient`, `store`, clock,
  and `maxDiskBytes`; there is no second Chromium process, no CDP
  session, and no `downloader.go` single-job-slot contention for this
  path.

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
kept "for later"). Note that the joined reason is stored in full on disk
but **truncated to ~512 bytes when reported** over
`getOfflineCacheStatus`/`offline_cache_status` (`service.go`'s
`truncateReason`) — one entry per failed resource with its full URL
inline is unbounded in a way an HTTP/WebSocket response body cannot be.

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

This only captures what the origin actually sent, though — and many
CORS-configured origins (CloudFront/S3-backed CDNs in particular) only
emit `Access-Control-Allow-Origin` et al. when the *request* itself
carries an `Origin` header, answering an Origin-less request with a
byte-identical body but none of those headers. `capture.go`'s
CDP-observed path never hits this: a real browser's cross-origin
`fetch()`/`<video crossOrigin>` request always sends its own `Origin`.
`mediacapture.go`'s browser-free `MediaCapturer`, however, used to issue
a bare `GET` with no `Origin` header at all — silently starving
`filterReplayableHeaders` of anything to capture for every
`ClassMedia`/`ClassUnknown` item (the majority of plain image/video/audio
artwork previews) even though the CDN would have happily returned the
CORS headers to a real player request. `MediaCapturer.fetchResource` now
sets `Origin` to the kiosk's own origin (`constant.WEBAPP_URL`) so its
capture request matches what the live `<video crossOrigin="anonymous">`
element would send, and the CDN's response — and therefore
`Resource.Headers` — matches reality. See §3.3's CORS bullet.

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

This does not mean every method is captured the same way, though.
`GET`/`HEAD`/`OPTIONS` are HTTP's own safe/idempotent methods, so none
of the three can have a server-side side effect just from being
re-acquired — but `resolveResources` still treats `OPTIONS`
differently from the other two: a successful `GET`/`HEAD` is re-fetched
over HTTP, while a successful `OPTIONS` (a CORS preflight) instead gets
a stored EMPTY body with no network round-trip at all, since re-issuing
a bare preflight (missing the real browser's `Origin`/
`Access-Control-Request-*` request headers) risks a different, or
outright failing, response than the live one — see the capture-flow
bullet list in §3.2 for why that shortcut is safe. A successful (2xx)
`POST`/`PUT`/`PATCH`/`DELETE`/... response is
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
everything else.** Three directories, one essential record per SOURCE:

```
offline-artworks/
  blobs/<sha256>                          # shared content-addressed bytes (the ONLY place binary payloads live)
  items/<sha256(item.source)>.json        # the ONE per-source record (ItemRecord in types.go)
  playlists/<playlistId>.json             # resolved DP-1 playlist (see note below), stored only for whole-playlist downloads
  playlists/by-url/<sha256(url)>.json     # {"playlistId": "..."} pointer, only when DownloadPlaylist was given a sourceURL
```

**An item's cache identity is its `source` URL, never its DP-1 `id`**
(`SourceKey` in `types.go`: `hex(sha256(source))` over the exact byte
string, no normalization).

The argument is the spec, not any particular resolver's behavior: the
DP-1 core schema makes `id` **optional** (only `source` is required) and
defines it as a UUID v4 — a random identifier derived from nothing about
the artwork — so a conforming playlist may omit it or change it freely.
Nothing durable may key on a field with that contract. `source` is the
one field that is simultaneously mandatory in DP-1, what capture actually
navigates to/downloads, and what replay matches paused requests against,
so keying on it makes the storage identity and the lookup identity the
same thing.

Observed instability is the symptom that surfaced this, not the
justification. Items materialized from the spec `dynamicQuery` profile
carry whatever id the remote resolver returned (`dp1-go` mints none), and
in the field those ids arrived fresh on each resolution — orphaning
id-keyed records, so replay scope missed, status lied `not_cached`, and
re-downloads stormed. Do **not** generalize that to "dynamic playlists
always regenerate ids": the legacy `dynamicQueries`/FFIndexer path in
`dp1.go` mints a deterministic UUIDv5 over (contract, chain,
tokenNumber). That is an implementation detail of one resolver rather
than a contract — and the spec above is why neither behavior may be
relied on.

**The trade-off, stated plainly: `source` is mutable where an id may not
be.** An FFIndexer-resolved item's source is the token's
`animation_url`/`image_url`, so a CDN migration or a re-rendered preview
changes it and orphans the cached record, costing a re-download. That is
the correct outcome rather than a regression — the captured bytes are
keyed to the exact URL replay will request, so a record under the old URL
cannot serve the new one — but it is a real cost. Recovering "same
artwork, new source" would need a separate provenance-based alias
(`chain:contract:tokenId`), deliberately not built here; it is not
something a change to this key could provide. Hashing is deliberately byte-exact: replay
also matches exact URLs, so a "normalized" key could claim a cache hit
for bytes captured under a different URL, the one direction that serves
wrong content; under-normalization merely costs a duplicate capture for
trivially different spellings. The hex name also keeps arbitrary-length,
externally-controlled URL strings out of filenames — the same convention
`playlists/by-url/` already uses.

**The record is per-source, not per-playlist-item.** Items sharing a
`source` — within one playlist or across playlists — converge on one
record and therefore one status entry, with their blobs shared.

How far up the pipeline that dedup reaches depends on the scope, and the
distinction is worth stating precisely rather than claiming more than
holds:

- **Within one `downloadPlaylist` call**, a repeated source costs one
  classify probe and one capture: `classifyPlaylistItems` dedups by
  source key before probing (first occurrence wins, playlist order
  preserved).
- **Across separate requests**, each call classifies independently — two
  concurrent `downloadPlaylist`/`downloadPlaylistItem` requests naming
  the same source do issue two probes, since classification happens
  before either can observe the other's queued state. The duplicate
  probe is a bounded, accepted cost (one `HEAD`, or a small ranged `GET`
  fallback); coalescing it would need an in-flight-classify registry with
  its own synchronization on a path whose real dedup — the capture — is
  already correct. **They converge to a single capture precisely when the
  second `enqueue` still observes the first job as `queued`/
  `downloading`**: that check and the `queue.push` are one critical
  section, so the loser returns `enqueueAlreadyQueued` and schedules
  nothing. That is the common case, since classification is bounded by
  `classifyItemTimeout` while a capture holds the worker for far
  longer. It is not an unconditional guarantee, and the boundary is worth
  naming: if the first capture *completes* before the second request's
  own classify returns, the second sees a terminal state and legitimately
  schedules a fresh capture — which is the recapture case in the next
  bullet, reached by an overlapping request rather than a later one. (An
  eviction cannot manufacture that outcome by clearing an in-flight
  item's tracked state: it may still reclaim such a source's stale
  record, but `notifyEvicted` compare-and-sets the state downgrade under
  the same lock `enqueue` commits `queued` under, so a scheduled job is
  never downgraded and never becomes invisible to the idempotency check.)
- **A later request for an already-captured source is a deliberate
  recapture**, not a missed dedup: it re-probes, re-captures, and
  refreshes the existing record in place under the same key.

The flip side
is deliberate and refcount-free: clearing a source via one playlist
(`clearPlaylistCache`) makes it `not_cached` for every other cached
playlist that contains it. The record's verbatim `item` field holds
whichever resolution captured last — informational drift only, since
replay reads `resources` and the DP-1 id inside is not an identity.

`items/<sha256(item.source)>.json` (`ItemRecord`, see `types.go`):

```json
{
  "item":    { "id": "work-1", "source": "...", "...": "verbatim DP-1 item, source NEVER rewritten; item.source is the record's identity" },
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

**This layout is already sufficient for a future `.dp1c` capsule export**
(DP-1's offline transport: a tar+zstd archive of `playlist.json` +
`assets/`, integrity-checked via sha256 — see dp1-validator's
`validator/capsule.go`), with no migration: a cached playlist's body and
its item records are produced from the same resolved playlist struct, so
their `source` strings are byte-identical, and an exporter derives
everything by walking `playlists/<id>.json` → `items[].source` →
`sha256(source)` → `items/<key>.json` → `resources[].sha256` →
`blobs/<sha256>`. Changes to this on-disk format must keep that
derivation chain intact.

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
- `DownloadPlaylist` persists the playlist body and its `by-url` pointer
  (`savePlaylistAndURLIndex`) only *after* the classification/queuing loop
  finishes, never before. If every eligible item in the playlist fails
  classification (`queued == 0 && classifyFailed > 0`), the whole call
  returns an error and neither the playlist body nor its `by-url` pointer
  is written — a "downloaded" playlist that
  `displayPlaylist`'s offline fallback (§6) can later load must mean at
  least one item was genuinely queueable, not a playlist record whose own
  items can never be classified into anything playable. Saving eagerly
  and only failing loudly on the RPC response would leave a broken
  "last known good" fallback on disk despite the caller having been told
  the download failed.
- `downloadPlaylistItem` (single-item download, resolved via `playlistUrl`)
  queues only the one requested item through `DownloadItem` and does not
  itself run `DownloadPlaylist`'s classification loop — but a device that
  only ever calls `downloadPlaylistItem` for a given `playlistUrl` still
  needs `displayPlaylist` by that same URL to work offline later.
  `commandrouter.handleDownloadPlaylistItem` closes this gap with a
  best-effort call to `Service.IndexPlaylistForOfflineDisplay` (marshal
  the already-resolved `*dp1.Playlist` back to JSON, then call the same
  `savePlaylistAndURLIndex` helper `DownloadPlaylist` uses) right after
  `DownloadItem` succeeds. This is deliberately best-effort and does not
  affect the RPC's own success/failure: the requested item is genuinely
  queued either way, and only the separate by-URL fallback record would
  be missing if indexing fails — a failure there is logged, not
  propagated to the caller.
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
  same atomicity pattern every other write in this store already uses:
  the content-addressed final name is only known once the stream has
  been fully hashed, so the write lands in a uniquely-named
  `blobs/incoming-*.tmp` file first (`os.CreateTemp`) and is renamed
  into place only after a successful, complete write; any early return
  (oversized body, I/O error) removes the temp file instead.
  `writeFileAtomic` (item/playlist/playlist-URL-index JSON) uses this
  same `CreateTemp`-based unique-temp-name pattern rather than a fixed
  `path+".tmp"` suffix, for the same reason: `DownloadPlaylist` calls
  `SavePlaylist`/`SavePlaylistURLIndex` directly on the caller's own
  goroutine (unlike per-item `SaveItem`, which the single-worker capture
  queue already serializes), and the command gate's dedupe only
  collapses byte-identical arguments — so two overlapping
  `downloadPlaylist` calls for the same playlist id with slightly
  different raw JSON are not guaranteed to be serialized upstream. A
  fixed shared temp name would let one writer's in-progress bytes be
  clobbered by the other, or let one rename fail after the first already
  moved the shared temp file; a unique-per-call name means the two
  independent renames simply race harmlessly onto the same destination —
  POSIX rename is atomic, so whichever runs last wins outright with its
  own complete content, never a torn mixture of both.
  `GC()` and `DiskUsage()` both deliberately skip any
  `*.tmp` name — a write interrupted by the process dying mid-stream
  (SIGKILL, power loss — not a handled Go error, so the `defer`-based
  cleanup above never runs) leaves a temp file that is neither an
  orphan blob GC's keep-set logic can recognize nor safe for GC to
  delete unconditionally (GC can run concurrently with an active
  capture — see below — and an in-progress temp file is exactly the
  kind of not-yet-referenced write that concurrency fence protects).
  Left unaddressed this would be a silent, unbounded disk leak: a
  crash-orphaned temp file would sit on disk forever, uncounted against
  `maxDiskBytes`, on a device meant to capture gigabyte-scale assets.
  `Store.SweepIncompleteBlobs` reclaims these instead, but only from
  `Service.Start`, before the worker goroutine exists and before any
  caller could have enqueued a job — the one point in this package's
  lifecycle where "no capture can possibly be in flight" is true by
  construction, so unconditionally deleting every `*.tmp` file there is
  safe in a way it would not be from `GC()`. `Start` also runs one full
  `GC()` pass in that same window: GC is otherwise only reached through
  clears and eviction, and eviction can only free bytes by deleting
  records `ListItemKeys` can see — so records GC must retire (a legacy
  id-keyed cache from before source keying, or any invalid/mismatched
  filename) would otherwise pin their blobs against `maxDiskBytes` where
  no eviction pass could ever reclaim them, starving every new capture's
  budget on a full store. **Operationally this means a device upgrading
  from the id-keyed format loses its entire offline cache on the first
  boot** — the records are unreadable under the new keying and their
  blobs are reclaimed — and the transition is silent: `Start`'s rebuild
  never sees those records, so no `offline_cache_status` notification is
  emitted and clients learn of it at their next `getOfflineCacheStatus`.
  Re-downloading is the recovery, and it is the accepted cost of shipping
  this as a new on-disk format rather than carrying migration code.
- Blobs are freed by a **sweep, not a refcount**: `store.go`'s `GC()` walks
  every saved item record's `Resources` to build the "keep" set, then
  deletes any blob not in it. There is no separate reference count kept in
  sync with saves/deletes — the saved item records are already the source
  of truth for what is live.
- GC's mark phase retires two further unreachable-by-construction
  shapes alongside genuinely unparsable records, and treats them
  differently on purpose. A `.json` file whose name is not a valid source
  key is **deleted**: that is the expected bulk state of a pre-source-keying
  cache, and renaming a whole store's worth of records to `*.corrupt`
  would strand those bytes permanently inside the `maxDiskBytes` budget
  (`DiskUsage` counts them, `DeleteItem` only targets `<key>.json`, and
  eviction only walks `ListItemKeys`, so nothing could reclaim them);
  they are a stale record of a format the daemon no longer reads, and the
  blobs they referenced are freed by the same sweep. A parseable record
  at a VALID key whose own `item.source` does not hash to that filename
  is instead **quarantined**: bytes written under an identity they do not
  carry are a genuine anomaly worth preserving evidence of, they are rare
  by construction (so the quarantine comment's "a few KB of JSON" bound
  holds), and `LoadItem` rejects the same mismatch as corrupt — mirroring
  `ReadBlob`'s hash-vs-name verification. No reader can load either shape,
  so keeping them "live" would pin their blobs forever for content nothing
  can serve.
- A record the mark phase cannot load splits two ways, because the two
  failure shapes demand opposite responses. A **transient read error**
  (EIO/EMFILE) aborts the whole sweep with an error: the record still
  references blobs, and sweeping over a silently narrowed keep-set would
  delete them as "orphans" while the record recovers on a later read —
  an unrecoverable loss from a recoverable error. Callers treat the abort
  as a non-fatal warning; the eviction loop stops for that pass (having
  already deleted its current victim's record — those bytes wait for the
  next successful sweep). A **deterministically unparsable record**
  (`ErrItemRecordCorrupt` — bytes read fine, JSON never will parse) is
  instead **quarantined** (renamed to `<id>.json.corrupt`, out of
  `ListItemKeys`' view but preserved for forensics) and the sweep
  continues. Aborting on it too would wedge GC on every future pass —
  and since GC is the only path that frees blob bytes (`DeleteItem`
  removes just the record JSON), that would permanently disable the disk
  budget while eviction kept deleting healthy records for zero reclaimed
  bytes, with nothing able to remove the corrupt record (eviction's
  victim scan skips records it cannot read). The quarantine is safe
  because no reader can use an unparsable record anyway: status reports
  it `not_cached` and `Start`'s rebuild skips it, so blobs only it
  referenced are already unreachable, and shared blobs stay in the
  keep-set via the readable records that also reference them.
- That sweep is dangerous to run concurrently with an in-flight capture:
  `capturer.Capture` writes each resource's blob as it observes it and only
  calls `SaveItem` once, at the end, so for the whole capture window there
  can be blobs on disk that no saved record yet references. `GC()` would
  treat those as orphans. `Service` fences this: `ClearItem`/`ClearPlaylist`
  and the disk-limit eviction path all run `GC()` through a shared
  `captureMu` that the active `Capture` call also holds for its whole
  duration, so a clear that races an unrelated in-flight capture simply
  waits for that capture to finish saving before sweeping, rather than
  risking deleting its not-yet-referenced blobs. (A `ClearItem` that only
  cancelled a queued download deleted no record, so it orphaned no blobs and
  skips `GC()` entirely rather than waiting on that fence for nothing.)
  `ClearItem`/`ClearPlaylist`
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
- A clear reports **`not_found` only when the item is entirely unknown** —
  no record on disk *and* nothing tracked in memory. Dropping a queued job
  (above) is real work, so an item whose only trace is a not-yet-started
  first-time download — which has no record on disk yet — is cleared
  successfully rather than reported as `not_found`, which the router maps
  to a *non-retryable* error. Answering `not_found` there told the client
  its clear did nothing while the download it asked to cancel was in fact
  gone, leaving a queued entry the client had no reason to ever re-issue or
  retire. The same holds for an item whose only trace is a *failed* capture
  (tracked `failed`, no record): clearing it retires an entry every client
  can still see. Two signals decide this, and the notification below:
  `reserveForClear` reports the ids it actually moved to `not_cached`
  (tracked state removed, queued job dropped), and `store.DeleteItem`
  reports whether a record really was removed. `ClearItem` deliberately has
  no `LoadItem` existence probe in front of the delete — the record is
  about to be deleted either way, so reading and unmarshaling it to learn
  only whether it exists is wasted work, and a record too corrupt to
  unmarshal would fail the probe and leave the client unable to ever clear
  it. An item already tracked as `not_cached` (the disk-budget eviction
  path leaves exactly that) is *not* settled by a clear: it transitioned to
  nothing, so it stays `not_found`.
- Both clears **emit an `offline_cache_status` `not_cached` notification**
  per item they settle, matching what the disk-budget eviction path already
  does. The clearing client already has its `ok:true` response, but that
  notification is the documented push mechanism for per-item transitions,
  so without it every *other* connected controller (and every local hub WS
  client) keeps rendering a stale `queued`/`ready` entry until it next
  polls `getOfflineCacheStatus`. Two deliberate exclusions keep the push
  honest: items that were neither tracked nor on disk (nothing transitioned
  — this is what stops a mostly-uncached playlist from fanning out one
  no-op notification per member), and items whose `DeleteItem` failed (the
  record is still on disk, so `not_cached` would be contradicted by the
  next status query). The call is `notifyObserver`, *not* `notify`:
  `notify` would write the state back into `s.state`, re-adding the entry
  the clear just removed — and since `s.state` is also the in-memory half
  of the known-item set (`allKnownItemKeys`), every cleared item would then
  linger in whole-store status pages, and grow that map without bound,
  purely as a side effect of having been cleared.
- That push is **ordered against a racing `enqueue`'s own `queued`
  notification** via the existing `captureJob.queuedNotified` barrier
  (`clearReservation.awaitQueuedNotifications`). `enqueue` commits a job to
  the queue under `s.mu` but emits its `queued` notification only after
  releasing that lock — it must never call the observer while holding it —
  and a clear needs nothing but the same lock, so without the barrier a
  clear could cancel the job and announce `not_cached` first, with the
  `queued` arriving afterwards. The client would then be parked forever on
  a `queued` entry for work that no longer exists: the exact failure mode
  `enqueue`'s own notify-ordering already guards against on the worker side
  (`process` waits on the same barrier before emitting `downloading`). The
  barrier closes *that* inversion, not notification ordering in general.
  One window is knowingly left open: a clear cannot pass the busy check
  until `process`'s `notify` has already written the capture's terminal
  state, but `notify` releases `s.mu` before calling the observer, so a
  clear can slip in there, delete the record the capture just saved, emit
  `not_cached`, and have that capture's `ready`/`failed` arrive afterwards.
  The item really is gone and the client is merely left on a stale terminal
  state until its next `getOfflineCacheStatus` — which is exactly what
  happened before clears notified at all — and the window is a few
  instructions wide rather than a whole observer callback. Closing it would
  additionally mean ordering against `process`'s `notify`, which has no
  existing barrier to reuse. Two paths are also knowingly left silent:
  a `ClearItem` whose `DeleteItem` failed (the record survived, so
  `not_cached` would be false; the error is the caller's signal), and
  `ClearPlaylist`'s per-item failures, for the same reason.
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

The 15s ceiling above bounds the REPLY wait, not the raw write itself:
`Send` also sets a socket write deadline (`SetWriteDeadline`) immediately
before its own `WriteMessage` call — the one socket-level deadline that
IS safe to set here, since writes are already fully serialized by
`writeMu` and a write deadline only ever affects the very next write, not
any other pending call's read-side wait. Without it, a write that blocks
at the syscall level (the kiosk accepted the TCP connection but stopped
reading, filling the OS send buffer) would hold `writeMu` forever,
wedging every future `Send` on the session too — not just the one whose
write is stuck — which the ceiling on the reply-wait alone cannot catch,
since that select is never reached until the blocked write itself
returns.

That write deadline reuses `ctx.Deadline()` — the value already produced
by the `context.WithTimeout(callerCtx, s.sendTimeout)` above, i.e. the
EARLIER of the caller's own deadline (if any) and this call's internal
15s ceiling — rather than a fresh `time.Now().Add(s.sendTimeout)`.
`Send` cannot reach its `ctx.Done()` select case until `WriteMessage`
itself returns, so if the write deadline ignored a tighter caller
deadline in favor of the full internal ceiling, a blocked write would
silently stretch a short caller deadline out to the full 15s — exactly
the "caller's tighter deadline wins" contract this ctx construction
otherwise promises everywhere else in `Send`.

### 6.1 Cross-origin iframes are separate CDP targets and must be attached individually

`feral-player` renders a software artwork inside an `<iframe>`. When that
iframe is cross-origin from the kiosk page (the common case — the artwork
is served from a different origin than the player shell), Chromium's site
isolation runs it in its own renderer process with its **own CDP target**
(an out-of-process iframe, OOPIF). A single CDP session attached only to
the top-level page target never sees that iframe's requests at all, so
`Fetch` interception scoped to the page silently misses everything the
artwork itself loads — including its very first document request — and
those requests fall through to the (offline) network. Field testing
confirmed this made cross-origin iframe artworks fail offline even when
fully cached; the page-only session had effectively never intercepted
them, relying on live connectivity.

Replay handles this with CDP **flat-mode multi-target** attach:

- `kiosktargets.go`'s `enableChildTargetAutoAttach` issues
  `Target.setAutoAttach{autoAttach:true, flatten:true,
  waitForDebuggerOnStart:true, filter:[{type:"iframe"}]}` on the
  freshly-dialed top-level session (once per reconnect, from
  `KioskReplay.AttachOnReconnect`).
- Each auto-attached child target arrives as a `Target.attachedToTarget`
  event carrying its own flat-mode `sessionId`. `cdpsession.go` now stamps
  that `sessionId` on outbound commands (`ForSession(sessionID)`) and
  routes inbound events to handlers registered for that same
  `(method, sessionId)` pair, so several targets multiplex over the one
  websocket. `replay.go` keeps a `sessionId -> session` target map (the
  empty id is the top-level page) and enables `Fetch` on, and answers
  paused requests from, each target independently — `Fetch` is a
  per-target domain, and a `requestId` is only unique within its own
  target's session.
- **`waitForDebuggerOnStart:true` is load-bearing, not optional.** It
  pauses a newly created child target before it issues even its first
  document request. On `Target.attachedToTarget` replay (1) attaches its
  handler and enables `Fetch` on the child, THEN (2) calls
  `Runtime.runIfWaitingForDebugger` to let it proceed. Without the pause,
  the child's first request could race ahead of `Fetch.enable` and reach
  the network before interception is armed — reintroducing the exact
  "first request always misses" failure, intermittently instead of
  deterministically. The attach/detach handlers hand off to their own
  goroutines because they issue CDP `Send`s (child `Fetch.enable`,
  `Runtime.runIfWaitingForDebugger`) that would deadlock the read pump if
  run inline (see `cdpsession.go`'s `On` doc).
- A child that attaches while a scope is already enabled is armed
  immediately (attach-time `Fetch.enable`), since in the display path the
  scope is enabled *before* the kiosk navigates and creates the iframe.
- A fresh top-level connection (a kiosk/OOM reconnect) is a generation
  boundary: it supersedes and closes the old top-level session and drops
  every child target from the prior connection, whose `sessionId`s are
  meaningless against the new socket. `Target.detachedFromTarget` drops an
  individual child mid-session (e.g. an iframe navigation) so a long-lived
  connection does not accumulate dead per-target state.
- **The attach/detach handoff itself races the generation boundary.**
  Because `handleTargetAttached`/`handleTargetDetached` hand off to their
  own goroutines (see above), a `Target.attachedToTarget` or
  `Target.detachedFromTarget` event can be read on an OLD top-level
  session's pump and then not actually get processed until AFTER a
  reconnect has already superseded that session via a fresh
  `Attach("", newRoot)`. Plain `Attach`/`Detach` would happily act on the
  stale `root` anyway, injecting a child bound to a dead socket into the
  CURRENT generation's target map — the next `EnableForPlaylist`/
  `Disable` fan-out would then try (and fail or stall on the dead
  socket's send timeout) to `Fetch.enable`/`disable` it. `replay.go`
  closes this with `AttachChild`/`DetachChild`: both take the `root` the
  event was delivered on and verify, atomically under the same
  `transitionMu` a reconnect's `attachRoot` swap holds, that `root` is
  still the current top-level session before touching the target map.
  `kiosktargets.go` calls these instead of plain `Attach`/`Detach` for
  exactly this reason.

This covers exactly one level of cross-origin nesting (kiosk page ->
artwork iframe), which is the reproduced topology. It is deliberately not
recursive — see §8.

On every `Fetch.requestPaused` event, keyed on the **exact original URL**
(never a rewritten relative path — this is what makes replay work for
absolute and cross-origin URLs without touching the artwork's own code):

**Player-appended query params are stripped before the miss check.**
ff-player's `ArtworkPlayer.tsx` unconditionally appends
`&display_mode=fit` or `&display_mode=crop` to a software/iframe
artwork's URL right before navigating to it — a UI-local rendering hint
derived from the device's own display settings, added strictly after the
signed DP-1 `item.Source` was already finalized. `capture.go` always
navigates to (and therefore stores every resource keyed on) the *bare*
`item.Source` — it never sees this parameter. Field testing found this
made the exact-URL lookup above miss **unconditionally** for every
iframe-type item's own top-level document request, not as a
partial-capture edge case: under `fail_closed` (the effective policy the
instant a displayed playlist's every item is cached, i.e. `mixed=false`
— exactly the case offline mode exists to serve), that guaranteed miss
failed the navigation itself with `net::ERR_FAILED` before the artwork
ever started loading, surfacing on-device as Chromium's own broken-frame
icon inside the iframe. `replay.go`'s `stripPlayerAppendedParams` retries
a miss once with a small, explicit allowlist of such params removed
(currently just `display_mode`) — order- and encoding-preserving for
every surviving query parameter, since a naive
parse-then-`net/url.Values.Encode()` round trip would alphabetically
resort them and reintroduce a mismatch for a different reason. This is
deliberately an allowlist, not a blanket "ignore extra query params"
normalization: an unknown extra param still misses (and fails closed),
so a genuinely different URL can never be silently served the wrong
cached bytes. If a future `ff-player` version starts appending another
UI-only param, add it to `playerAppendedQueryParams` in `replay.go`.

**The player's `HEAD` content-type probe is answered from the matching
`GET` resource, not a separately-captured `HEAD` entry.** Before
rendering a media item, `ff-player`'s `getContentTypeFromURL` issues a
`HEAD` request to `source?v=<cache-busting-timestamp>&x-request=xhr` to
decide which native element to render it with. Neither `capture.go` nor
`mediacapture.go` ever issues or stores a `HEAD` resource (both only ever
`GET` — §4.7's `Resource.Method` identity rule still holds: a paused
request is never fulfilled from a different method's resource, with this
one deliberate exception), and the cache-busting `v` param changes on
every single probe, so this `HEAD` is an **unconditional** miss for every
offline media item, exactly like the `display_mode` case above is for
every software item's first request. Left unanswered, the probe fails
offline and the player falls back to its `<iframe>` renderer instead of
the correct native `<img>`/`<video>`/`<audio>` element. `replay.go`'s
`processRequestPaused` retries a `HEAD` miss by stripping the probe's own
`v`/`x-request` params (`headProbeQueryParams`, the same order/encoding-
preserving stripping as `display_mode` above) and looking up the `GET`
resource for that same URL; if found, it fulfills with that resource's
status/`Content-Type`/allowlisted headers but an **empty body** — correct
HTTP semantics for `HEAD`, and exactly what the probe needs to pick the
right renderer. This substitutes method only, never URL, so it carries
none of the "could serve the wrong bytes" risk a URL-based normalization
would. Since native media elements render on the kiosk's TOP-LEVEL page
(not inside a cross-origin iframe the way software artworks are — see
§6.1), the existing top-level `Fetch` interception already covers them;
no new per-target machinery was needed for this.

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
  `StaticServer.Listen()`/`Serve()` are deliberately split (rather than
  one `ListenAndServe()`): `main.go` calls `Listen()` synchronously at
  startup so a bind failure (e.g. port already in use) is caught and
  logged immediately, then only launches `Serve()` in a background
  goroutine once the bind actually succeeded. Before this redirect
  bullet runs, `fulfillFromBlob` checks `staticServer.IsListening()` and
  treats the asset as a miss if it is not — otherwise a bind failure at
  startup would silently turn every large-asset replay into a dead
  redirect (or, worse, a redirect to whatever unrelated service now owns
  that port) with no indication in the replay path itself that the
  static server was never actually serving.
  `staticServerAddr` may configure port `0` to request an OS-assigned
  ephemeral port (e.g. to avoid a fixed-port collision); `Listen()`
  updates the address `URLFor`/`BaseURL` build against to the REAL bound
  `listener.Addr()` once binding succeeds, so a `:0` config never leaks
  into an unusable redirect target.
  Because `handleBlob` serves `BlobPath` directly rather than through
  `Store.ReadBlob` (which hash-verifies but reads the whole blob into
  memory — unusable for gigabyte-scale assets), it re-verifies the
  content hash itself via `verifyBlobContent` before serving. A
  successful verification is cached for this process's lifetime (never a
  failed one) so a `Range` request scrubbing through a large cached video
  is not forced to re-hash the entire file on every request — the
  accepted trade-off being that corruption introduced strictly after a
  blob's first successful verification in this process's lifetime would
  not be caught by a later request.
- **Miss** (URL not in the currently-enabled scope's resource set) →
  governed by `offlineCache.missPolicy` (`MissPolicy` in `replay.go`) when
  the scope is a single item or a playlist whose every item is cached:
  `fail_closed` (default) fails the request visibly rather than silently
  substituting or passing through, which makes offline behavior
  deterministic and surfaces partial captures honestly (with one
  deliberate exception — see "Admission saturation suspends interception"
  below); `pass_through` lets the
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
item source URLs before/after the CDP display call, which enables `Fetch`
interception scoped to whichever of those sources are actually cached
(`EnableForPlaylist` in `replay.go`) and disables it entirely when none are.
`commandrouter`'s `displayPlaylist` handler calls `SyncPlaylist` for the
*new* playlist **before** asking CDP to actually display it — deliberately,
so `Fetch` interception is already scoped correctly by the time the kiosk
starts requesting that playlist's resources, rather than racing the first
few requests against a scope switch that has not happened yet. "Before
the send" never means "before the send is even certain to happen",
though: the sync runs only after the `displayAt` scheduler has filtered
the cast and something will actually be pushed. A future-only cast that
the scheduler defers (`ok: true, deferred: true`, no player write)
leaves scope untouched, because the previous playlist keeps displaying
and switching interception to the deferred playlist would — under a
fail_closed scope — block the on-screen playlist's own requests even
with live network. When a filtered cast IS pushed, the scope is synced
with the FULL playlist's item sources rather than the active cohort: the
scheduler's timer/wake/retry cutovers push later cohorts of the same
playlist directly with no replay-scope hook of their own, so the
cast-time scope must already cover every cohort a cutover can display
(uncached future items only dilute the scope to `mixed`, whose miss
policy is pass-through — the safe direction — and the
`playlist-refresher`'s periodic pass re-syncs as downloads complete).
The refresher guards its own pre-send sync the same way it already
guards the send: a playlist-authority change observed under the
playback lock skips both, so a stale refresh pass can no longer
overwrite the scope a newer scheduler-owned cast just installed. If the
CDP send itself then fails, or the player rejects the command (`ok:false`),
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

Two loopback origins are exempt from interception outright, on every
scope and under every miss policy (`processRequestPaused` and the
admission-overflow path both check them before anything else): the
cache's own static server (its large-asset `302` follow-up would
otherwise loop back into the interceptor) and the ff-player shell's
origin (`constant.WEBAPP_URL`). The shell exemption is what keeps
daemon-driven recovery navigations working while a fail-closed non-mixed
scope is armed — `playersession.navigateAndVerify`'s `Page.navigate`
back to the shell (boot recovery, `refreshArtwork` escalation, watchdog
navigate) is by construction never in the captured resource map (capture
navigates to `item.Source`, never the shell), so without it the
navigation's own document request would be `Fetch.failRequest`-ed,
leaving Chromium on its error page with no command handler until a
browser restart. Both exemptions match by parsed scheme+host equality,
never a string prefix (see `isStaticServerFollowUp`'s userinfo-lookalike
hazard), and both origins are served locally, so passing them through is
offline-safe and never reaches an attacker-controlled host.

`KioskReplay.AttachOnReconnect` re-attaches the replay CDP session in
`main.go`'s CDP `onConnect` hook, so a kiosk Chromium restart (including OOM
recovery) does not leave replay silently detached.

### What `maxDiskBytes` actually bounds

`Store.DiskUsage` sums **every** directory the cache persists — blobs,
item records, playlist bodies, and the by-url index — not just `blobs/`.
It once counted blobs alone, which left real cache data outside the
ceiling `offlineCache.maxDiskBytes` is documented to enforce.

Playlist metadata was the sharp edge: `downloadPlaylist` persists a
playlist's raw JSON (and its URL-index entry) *before* any item is
queued, and does so even when nothing in the playlist is cacheable at
all. A device asked to download a series of distinct all-streaming
playlists therefore accumulated metadata that no eviction pass could
see — `enforceDiskLimit` walks items by `CapturedAt` and GCs the blobs
they release, and a playlist body is neither.

Counting it is only half the fix, since counting alone would make the
overage visible while leaving nothing able to act on it. Playlist bodies
are additionally bounded **by count** (`MaxPlaylistRecords`, 256) at the
point they are written, oldest pruned first, along with any by-url entry
left pointing at a pruned body. The cost of pruning is the
displayPlaylist-by-URL offline fallback for playlists that far back —
`CachedPlaylistForURL` already fails closed when a record is absent.

Item records are counted but not separately bounded: unlike playlist
bodies they are deleted with their item, so they scale with a population
eviction already controls.

### A child target is only resumed once interception is armed

Flat-mode child targets (cross-origin OOPIF iframes) attach paused, and
`kiosktargets.go` resumes them with `Runtime.runIfWaitingForDebugger`
once `Replayer.AttachChild` authorizes it. That authorization is a
correctness gate, not bookkeeping: a child resumed with `Fetch.enable`
NOT armed runs completely outside replay, so every request the iframe
makes goes straight to the network — silently, while the scope still
claims `fail_closed` and status still reports the item cached.

So when arming a newly attached child fails, the child is left paused
(and a transport failure additionally retires the session, see below). A
visibly stalled iframe is the honest outcome; an invisible bypass of the
guarantee offline mode exists to make is not. The one exception is a
scope where the network is already permitted for a miss —
`pass_through`, or a `mixed` scope — where a hung iframe buys nothing.

### Admission saturation suspends interception

There is one path that knowingly stops enforcing `fail_closed`, and it is
deliberately the opposite call from the stalled-iframe rule above.
`onRequestPaused` admits at most `RequestPausedAdmission` (64) concurrent
handlers plus `OverflowAdmission` (512) cheap overflow resolutions. When
*both* fill, `retireOnSaturation` closes the **root** CDP session, which
makes Chromium release every request it had paused — on every target on
that connection — to the live network.

The root specifically, whichever target's event tripped it. Saturation is
a property of the socket, not the target: both semaphores are
per-replayer, and every flat-mode child shares the one connection's write
path, so exhausting them means that connection has stopped serving replay
for everything on it. Retiring the tripping target instead would be a hang
rather than a release when that target is a child — `flatSession.Close` is
a per-target detach that only unregisters handlers (it must never close
the shared socket), so Chromium would keep the connection, keep `Fetch`
armed at `"*"` on that iframe, and keep its requests paused, with no
handler left to answer them and no `targets` entry to re-arm. `RootAttached`
would still report true, so `SyncPlaylist` — including the one the
scope-lost recovery below triggers — would re-arm scope on the still-live
root and return without ever re-dialing, leaving that iframe stranded. On
a real device the burst comes from the cross-origin artwork iframe, so
that is the likely path into saturation, not an exotic one.

The two cases differ in what the alternative buys. A child that cannot be
armed is one iframe: stalling it costs that iframe and keeps the guarantee
for everything else. Saturation means 576 sends are already outstanding on
one socket — replay has stopped answering *anything*, and answering is
itself a `Send`, which is exactly what has run out. The options are a page
whose every request hangs (an earlier revision did this, and the hang
persists until something else retires that session — not something the
daemon guarantees will happen) or a page that proceeds unintercepted.
Neither preserves `fail_closed`; only one leaves a usable screen.

Consequences to be aware of when reasoning about a device in this state:

- The scope is **not enforced at all** until the next `SyncPlaylist`
  re-arms it. Scope is deliberately never re-applied from cached state
  (see `AttachOnReconnect`'s doc for why), so recovery has to be
  caller-driven — which is why retirement fires `SetOnScopeLost`
  (`ScopeLostRegistrar`), wired in `main.go` to
  `PlaylistRefresher.ForceRefresh`. That is the same recovery the
  kiosk-reconnect path uses for the identical problem.
- That acceleration is **best-effort, not a bound**. It usually replaces
  the up-to-`PLAYLIST_REFRESH_INTERVAL` (5 minute) wait with a re-dial,
  but several paths fall back to the periodic tick: `redialIfDue` has a
  30-second cooldown and skips the dial entirely if a replay-owned re-dial
  happened recently, and the single buffered signal is consumed by one
  refresher pass, so if that pass returns early (CDP not initialized, a
  player-status or DP-1 resolve failure, nothing being displayed) nothing
  retries it. Re-arming also still needs a working CDP dial, and the page
  runs unintercepted until it completes.
- A fully cached artwork can therefore render from the network during that
  window, so "it played" is not evidence it is genuinely offline-capable.
  On a device with no connectivity the requests simply fail instead.
- `getOfflineCacheStatus` keeps reporting those items `ready`, because they
  genuinely are cached — the degradation is in interception, not in the
  cache.
- The operator-visible signal is the retirement warning
  `offline cache replay: retiring CDP session` with reason
  `admission saturated; releasing paused requests to the network rather
  than hanging them`. That log line is currently the *only* surface for
  this state; there is no wire-level degraded/unavailable signal, so a
  controller cannot tell the difference. Adding one is a coordinated
  cross-repository change (see the `offline_cache_status` compatibility
  work) rather than something this daemon can do unilaterally.

### The replay session recovers independently of the primary CDP connection

That `onConnect` hook is not sufficient on its own. The replay session is
a **separate websocket** from the daemon's synchronous `cdp.CDP` client —
its own read pump, its own write deadlines — so it can die while the
primary connection stays perfectly healthy, in which case no reconnect
event ever fires and the hook never runs. Two mechanisms close that:

- **Retirement.** `cdpsession.go` stamps `ErrCDPTransport` on exactly the
  Send failures that leave a connection unusable (socket already closed,
  write deadline unsettable, write failed). `replay.go`'s
  `retireIfDeadLocked` then drops that session from the target set and
  **closes** it. Closing matters beyond bookkeeping: CDP releases paused
  requests when a client disconnects, so a half-dead socket left open
  with `Fetch` armed at pattern `*` can leave requests paused
  indefinitely and hang playback. A CDP error *reply* is deliberately not
  a retirement reason — the peer answered, so the connection is fine and
  only the command was refused.
- **Replay-owned re-dial.** `KioskReplay.SyncPlaylist` checks
  `Replayer.RootAttached()` when a scope call fails. A missing root means
  the socket died, so it re-dials (same dial + attach + child
  auto-attach sequence as `AttachOnReconnect`) and re-applies the scope
  within that same sync. Scope restoration is free: `SyncPlaylist`
  recomputes what is replayable from current store state at the top of
  every call, so there is no cached "what was enabled before" to go
  stale. Re-dials are spaced by `redialCooldown` (30s) because a dial
  against a down kiosk costs a real blocking round trip
  (`defaultDialTimeout`, 15s) and `SyncPlaylist` runs on every
  `displayPlaylist` — without spacing, a kiosk that is simply gone would
  put that cost on the front of every display command.

`Replayer.Attach`'s `Fetch.requestPaused` handler is bound (via closure)
to the exact session it was registered on, and `processRequestPaused`
answers every request using that bound session, never by re-reading the
replayer's current session field. A prior version re-read the current
session, which meant a `Fetch.requestPaused` event already dispatched
from an OLD, about-to-be-superseded connection's read pump — but not yet
processed by the time a reconnect swapped in a replacement — would
answer using the NEW connection's `Send`, carrying a `requestId` that
only ever existed on the old one; CDP's DevTools protocol has no
cross-connection request-ID namespace, so that call would at best fail
silently. `Attach` and `EnableForPlaylist`/`EnableForItem`/`Disable` are
additionally serialized against each other by a dedicated
`transitionMu` (deliberately separate from the fast, per-request
session/resources lock): without it, `EnableForPlaylist` could read the
current session, then have a concurrent `Attach` swap and close that
exact session before `EnableForPlaylist`'s own `Fetch.enable` call ran —
silently leaving the replacement session's `Fetch` domain never actually
enabled even though local scope bookkeeping said interception should be
active.

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
- **Nested targets are not separately attached on the CAPTURE side.**
  Capture (`capture.go`) only observes the top-level page target's
  `Network` events; it does not attach `Target.setAutoAttach` for Web
  Workers or nested iframes, so requests issued purely from within a
  worker or a nested iframe (rather than proxied through the top-level
  page) can be invisible to capture. Service Workers registered by the
  artwork itself compound this: they can intercept and serve their own
  responses, further hiding requests from top-level `Network` domain
  capture. Note this is a capture-side statement: capture navigates the
  headless page's *top-level document* directly to `item.Source`
  (`Page.navigate`), so `item.Source`'s own top-level document and the
  resources it requests directly ARE observed — the gap is resources
  requested only from a worker or a *further* nested iframe inside that
  page.
- **Replay attaches cross-origin iframe targets, but only one level
  deep.** Replay (§6.1) DOES attach the kiosk page's direct cross-origin
  iframe child targets via flat-mode `Target.setAutoAttach`, because the
  artwork is embedded in an iframe by `feral-player` and would otherwise
  be un-intercepted. It is deliberately not recursive: a *further* nested
  cross-origin iframe (inside the artwork's own iframe) would need
  `Target.setAutoAttach` reissued on that child's session, and Web Workers
  are excluded by the `iframe` filter. Combined with the capture-side
  limitation above, content whose resources live two or more cross-origin
  levels deep, or behind a worker, is not covered end to end.
- **WebSocket data** cannot be captured-and-replayed this way — such
  requests are out of scope for this pipeline (§3.2).
- **Live/VOD HLS or DASH streaming (`.m3u8`/`.mpd`) is explicitly rejected
  up front, not merely uncovered.** `classify.go` detects an HLS or DASH
  manifest by URL extension (checked before any network round trip) or
  by `Content-Type` (`application/vnd.apple.mpegurl`/`application/
  x-mpegurl`/`audio/mpegurl`/`application/dash+xml`) and returns
  `ClassStreaming`, the only class `DownloadItem`/`DownloadPlaylist`
  reject outright (`ErrUnsupportedMediaClass`) rather than queuing — see
  §1/§3.3. A manifest points at a set of segments fetched progressively
  during playback, not one fixed byte sequence, so there is nothing a
  one-shot download or a static blob-store replay could faithfully
  serve; this is a deliberate scope boundary, not a gap left for a
  future revision to
  close.
- **SVG/`model/gltf` items that reference EXTERNAL subresources are only
  partially covered by the direct-download path.** `MediaCapturer`
  downloads exactly the one file at `item.Source` (§3.3); a self-contained
  `.glb`/inline-everything SVG is fully cached, but an SVG with an
  external `<image href="...">` or a `.gltf` (as opposed to binary
  `.glb`) referencing separate external buffer/texture files has those
  further dependencies uncached, since capturing them would require the
  same "run the code and observe" approach §1 restricts to
  `ClassSoftware`. Not capturing them is a known, accepted limitation
  rather than a special case worth the cost of routing these through
  the headless browser — but the two formats REPORT it differently:
  - `.gltf` is no longer silent about it: the manifest's
    `buffers[].uri`/`images[].uri` entries are the exact, spec-defined
    list of out-of-band files, so `MediaCapturer` parses them and saves
    honest partial coverage when any are external (§3.3).
  - SVG keeps `Coverage.Complete=true`, deliberately. The kiosk player
    renders an `image/*` item with an `<img>` element, and Chromium's
    SVG-as-image mode never loads external references at all — so the
    single cached file replays exactly what the live render showed, and
    downgrading every SVG to `partial` for references that would not
    have loaded online either would be the dishonest direction. There
    is also no exact dependency list to parse: external references can
    hide in `href`/`xlink:href` attributes, CSS `url()` values and
    `@import`s, so any check would be a heuristic with false verdicts
    in both directions.
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
- **Headless GPU rendering path is not identical to the kiosk's, and
  deliberately does not touch real GPU hardware.** `downloader.go`
  launches headless Chromium with Chromium's software WebGL backend
  forced on (`--use-gl=angle --use-angle=swiftshader-webgl
  --enable-unsafe-swiftshader`, no `--disable-gpu`) so WebGL/canvas
  artworks take the same context-available code path during capture as
  they do live — plain `--disable-gpu` makes `canvas.getContext("webgl")`
  return `null` during capture, which could make a feature-detecting
  artwork silently skip GL-dependent resource fetches that the live
  kiosk does make. `--enable-unsafe-swiftshader` is required from
  Chromium 130+: automatic SwiftShader-as-WebGL-fallback was deprecated,
  so software WebGL must be explicitly requested or context creation
  fails the same way `--disable-gpu` does.
  An earlier version of this flag set instead mirrored the kiosk's REAL
  GPU hardware acceleration (`--ignore-gpu-blocklist
  --enable-gpu-rasterization`) on the theory that it would narrow the
  rendering-fidelity gap. Field testing found this caused device-wide
  hard freezes (no OOM-killer trace, no clean shutdown logged — the
  signature of a GPU-driver-level lockup, not a memory issue) when a
  download ran while the kiosk was actively using the same physical GPU
  for live hardware-accelerated playback. Since capture never renders to
  a visible surface — it uses CDP `Network` events only to learn which
  URLs a page requested, then re-fetches bytes directly over HTTP (see
  §3, `capture.go`'s doc) — pixel-accurate or fast rendering was never
  actually required, only a non-null WebGL context; forcing the software
  backend gives that same successful-context-creation behavior with zero
  contention for the kiosk's GPU. This still does not close the fidelity
  gap for artworks whose *visual output* depends on real GPU rendering
  characteristics (headless capture renders off-screen via SwiftShader
  rather than through the kiosk's Wayland surface), but capture only
  needs the resource-fetch side effects of that rendering, not its visual
  accuracy — see `start-kiosk.sh`.

## 9. Source safety: what a playlist is allowed to point at

A playlist body is untrusted input. It arrives over the LAN hub — which
binds `0.0.0.0:1111` and is **unauthenticated** — and over the relayer,
and every `source` inside it is a URL this daemon will dial on the
playlist's behalf from three separate places:

- `classify.go`'s `HEAD` / ranged-`GET` probe,
- `mediacapture.go`'s direct body download,
- `capture.go`'s `Page.navigate` in the headless browser.

The device runs privileged, unauthenticated services on loopback that
those paths would otherwise reach: Chromium's DevTools endpoints on
`127.0.0.1:9222` (kiosk) and `:9223` (capture), the hub itself on
`:1111`, `feral-sys-monitord` on `:9001`, and the blob static server on
`:8082`. Without a guard, a `source` of `file:///etc/shadow` is local
file disclosure through the headless browser, and
`http://127.0.0.1:9222/json/new?...` is full control of the kiosk
browser — both reachable by anyone on the LAN.

`sourceguard.go` closes that off inside `Classify`, which is the single
function BOTH enqueue paths (`DownloadItem` and `DownloadPlaylist`) call
before any I/O and before any job is queued. A rejected source is
therefore never probed, never downloaded, and never navigated to. It
rejects, with `ErrUnsafeSource`:

- any scheme outside `http`/`https` — an allowlist, not a denylist, since
  the set of schemes a browser or `http.Client` will act on (`file`,
  `ftp`, `gopher`, `ws`, `chrome`, `devtools`, `about`, `blob`,
  `javascript`, …) is long and grows;
- a literal address in a reserved range: loopback, RFC 1918, IPv6 ULA,
  link-local (including `169.254.169.254`), CGNAT, multicast, broadcast,
  unspecified, and the IPv4-mapped / NAT64 / 6to4 forms that wrap one;
- a hostname that RESOLVES to any of the above — checked across *every*
  answer, not just the first, so a name returning one public and one
  loopback address cannot be admitted here and then dialed round-robin
  later.

The userinfo trick (`http://cdn.feralfileassets.com@127.0.0.1:9222/…`)
is handled by resolving the real host via `url.Hostname()` rather than
matching on the visible prefix — the same hazard `replay.go` documents
for its own origin checks.

A DNS **resolution failure** is deliberately NOT tagged `ErrUnsafeSource`:
it is a transient network fault, and reporting it as a security rejection
would make an offline device look like it is under attack in its logs.

**A URL check alone is not sufficient**, for two reasons that need no
hostile DNS at all:

- **Redirects.** `http.Client` follows up to ten hops by default, and only
  the FIRST URL ever reaches the check. A source that passes and then
  `302`s to `http://127.0.0.1:1111/` walked straight to the
  unauthenticated hub. Demonstrated: with the enforcement removed, the
  test client follows the hop and really does dial the private address
  (`dial tcp 10.0.0.1:80: connect: operation timed out`).
- **DNS rebinding.** The check resolves a name and discards the addresses;
  the real dial resolves again, so an answer that flips to a reserved
  address in between was never re-examined.

Both are closed by applying the SAME policy **at the socket** rather than
at the URL. `newGuardedHTTPClient` puts `addrsFor` in the transport's
`DialContext`, which every hop and every re-resolution must pass through,
and then dials the exact address it just validated so no second lookup can
substitute another (TLS still verifies against the original hostname, so
pinning the address costs nothing). `checkRedirect` adds the one thing a
dialer cannot see — the scheme — plus an explicit hop cap. Both the
classify probe and the media body client are built this way.

**The transport carries no proxy**, deliberately. With one configured,
`Transport` dials the *proxy* and hands it the origin host, so
`DialContext` would validate the proxy's address while the proxy resolved
and connected to whatever the URL named — the destination would never be
examined, and everything above would be worth nothing. An earlier revision
copied `http.DefaultTransport`'s settings wholesale and inherited
`ProxyFromEnvironment` with them; a regression test now asserts
`transport().Proxy` is nil, on the field rather than through the
environment because `ProxyFromEnvironment` caches behind a `sync.Once`.

The trade-off is accepted knowingly: a deployment that can only egress
through a proxy cannot fetch artwork on these two paths. That is the right
way round here — artwork origins are public CDNs reached directly on this
device, and the daemon-wide client (relayer, indexer, OTA) still honors
proxy environment variables, so only untrusted playlist-source fetches go
direct. Supporting a proxy safely would mean enforcing the destination
*through* it (CONNECT-aware checking), not re-enabling the field.

`sourceGuard.check` still runs first: it is what stops a bad source from
ever being queued, and it produces the error the caller reports.

**Which client goes where matters, and getting it wrong is silent.** The
capturer holds two: `cdpClient` reaches our OWN capture Chromium's
DevTools endpoint on loopback (`127.0.0.1:9223/json`) and must be
UNGUARDED; `fetchClient` pulls resource bytes from artwork origins and
must be GUARDED. They have opposite trust properties, and one client
cannot serve both — an earlier revision of this work passed the guarded
client to both roles, and because the guard correctly rejects loopback,
every `ClassSoftware` capture failed at `DialPageSession` before
navigation. `NewCapturer` takes the two separately so the compiler is
what keeps them apart, and
`TestCapturer_CDPDiscoveryClientIsNotTheGuardedOne` pins both halves.

**The capture browser is guarded too, at the request level.** The Go
transport cannot cover Chromium: `capture.go` hands the artwork to
`Page.navigate`, and the page then issues its own requests, which never
pass through a Go client. So the capture CDP session enables `Fetch` with
pattern `*` (the same machinery `replay.go` uses on the kiosk) and answers
every `Fetch.requestPaused` with `continueRequest` or `failRequest` after
running the URL's host through the same `addrsFor` policy. Armed before
`Page.navigate`, so the page's very first request is already covered.

Three details that are easy to get wrong:

- **Handlers must not `Send` inline.** `CDPSession.On` runs handlers on the
  read pump, and the reply a `Send` waits for can only arrive on that same
  pump — so every decision is handed to a goroutine, exactly as
  `replay.go` does.
- **Saturation fails closed.** Concurrent decisions are bounded
  (`captureGuardConcurrency`); beyond that a request is left unanswered
  rather than admitted, so flooding cannot push an unchecked request
  through. The cost is that request stalling until the bounded capture
  window ends.
- **Only http(s) is checked.** `data:`/`blob:` carry their own bytes and
  open no socket, so blocking them would break ordinary artwork for no
  security gain.

**Residual gaps, stated rather than hidden.** This is a URL-time check, so
two things remain open, and both need Chromium's egress taken away
entirely (a loopback filtering proxy it must dial through) rather than
more interception:

- **DNS rebinding.** Chromium resolves the host itself *after* we continue
  the request, and can get a different answer than we did. A name that
  answers public to us and loopback to Chromium still lands. This is the
  same class of gap that made the original pre-flight URL check
  insufficient for the Go paths — closed there by moving to `DialContext`,
  which has no equivalent here.
- **WebSocket.** CDP `Fetch` does not intercept WebSocket handshakes, so
  `ws://` egress from the page is uncovered. Mitigated in practice because
  reaching a DevTools socket requires its target UUID, which is read over
  HTTP (`/json`) and therefore blocked — but not closed.

Both intersect an exposure `docs/architecture.md` already accepts as
release-scoped (the open, unauthenticated `:1111` surface, end state
#3471), and are the reason a filtering proxy remains the eventual
complete answer.

**Operational consequence.** Playlist sources on private or loopback
addresses are refused. Artwork origins are public CDNs, so this does not
affect normal operation, but a developer pointing a test playlist at a
LAN-hosted asset server will see `ErrUnsafeSource` — that is the guard
working, and would need an explicit opt-in config knob to relax.

## 10. See also

- `components/feral-controld/offlinecache/` — the implementation;
  `classify.go` (routing), `capture.go` (software/headless path),
  `mediacapture.go` (direct-download path, §3.3), `replay.go` (serving
  both back to the kiosk, §6).
- `docs/controld-inbound-controller-messages.md` — the 5 controller-visible
  commands (`downloadPlaylistItem`, `downloadPlaylist`,
  `clearPlaylistItemCache`, `clearPlaylistCache`, `getOfflineCacheStatus`)
  and the `offline_cache_status` notification.
- `components/feral-controld/config/config.go` — `OfflineCacheConfig`
  (`offlineCache.*`) tuning knobs, and their defaults in
  `offlinecache/bootstrap.go`.
- `components/feral-controld/offlinecache/sourceguard.go` — the source
  allowlist and reserved-address checks described in §9.
