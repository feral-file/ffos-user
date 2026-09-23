# Player session & recovery: controld ↔ ff-player

This is the maintained design reference for `feral-controld`'s `playersession`
package and everything that depends on it: the boot player recovery state
machine, the startup OTA gate's narration policy, the four-value sleep
tracker, and the machine-readable player refusal/status contract that makes
all of it possible. It describes the system **as implemented**, not as
originally proposed — where the shipped code deliberately diverges from an
earlier draft (most notably the boot-recovery classification order), that is
called out explicitly rather than silently glossed over.

Historical context only: this replaces `.omc/plans/cross-repo-recovery-redesign.md`,
which is untracked and was the accepted design during implementation. Do not
cite it from code comments — cite this file instead, or the specific package.

Scope: `feral-controld` (this repo, `components/feral-controld`) and
`ff-player` (the bundled kiosk web app, separate repo). The two ship
together — see §9 for the compatibility contract between them.

---

## 1. Design principles

1. **Levels + reconciliation, not edge patches.** State (page generation,
   connectivity, sleep intent) is read fresh at the point of use, not
   snapshotted at the moment an edge event fired. A late reconcile or a
   missed edge self-heals on the next read instead of requiring its own patch.
2. **One session authority; honest lane scope.** `playersession.Session` owns
   the cross-cutting authority over the CDP-connected page: page generation,
   readiness barriers, screen-overlay coordination, and the one navigation
   primitive. Off-lane producers (setupui, mediator, mintpairing/qrdisplay,
   sleep_schedule, commandrouter) keep their own queues and locks; they
   consult the session rather than driving the page directly (§4).
3. **Bounded retries with re-arm; backoff is the primary trigger.** Every
   deferred state in the boot-recovery machine schedules a backoff timer as
   its primary re-entry. Generation bumps and confirmed wake edges are
   *accelerators* that re-enter early — never the only wake-up.
4. **Machine-readable player contract.** The status probe
   (`window.__ffosPlayerStatus`) reports the actual preconditions consumers
   need — route, handler/artwork readiness, boot-hydration outcome — not a
   synthetic "hydrated" boolean. Refusal replies carry a `code` field
   alongside the human-readable `error` string.
5. **The recovery primitive is navigate-to-entry, never reload-in-place.**
   The static export ff-player ships is flat files only
   (`playlist.html`, `sleep.html`, `error.html`, `daily.html`); a
   reload-in-place 404s on every client route. The one recovery primitive,
   `NavigateHome`/`NavigateHomeInline`, always navigates to the app root and
   is sleep-aware, error-page-aware, and can carry a cache purge.

---

## 2. Page generation and readiness (`playersession.Session`)

`Session.Generation() uint64` identifies which document is currently live.
It bumps from three independent sources, and every consumer that needs to
know "is this still the same page I was just talking to" reads this counter
rather than tracking its own notion of page identity.

### 2.1 Three bump sources

1. **CDP on-connect** (`OnConnect`, wired as `cdp.CDP.Start`'s connect
   callback). Every (re)connect means Chromium (re)loaded the web app from
   defaults, so this is always a fresh generation.
2. **Session-executed navigation** (`NavigateHome`/`NavigateHomeInline`).
   The session bumps synchronously right after sending `Page.navigate`.
3. **Document-stamp mismatch**, detected via the status poller's existing 5s
   `checkStatus` round-trip (`status.POLL_INTERVAL`) — no second evaluate.
   This is the *three-way stamp contract*, and the "three-way" part is
   load-bearing:
   - The session writes `window.__ffosDocStamp` once a generation's barrier
     resolves (`onGenerationReady`), and remembers that value as the
     generation's baseline.
   - ff-player's `checkStatus` reply echoes `stamp: window.__ffosDocStamp ?? ''`
     on `CanvasService.getStatus`'s normal-path return — present even when
     the document hasn't been stamped yet (a fresh mount, or a document the
     session doesn't own). This is **not unconditional**: `getStatus` has
     two earlier-returning branches — the overheating check
     (`isOverheating` → `{ok:false, error:Overheating}`) and the top-level
     `catch` (`{ok:false, error:StatusCheckFailed}`) — that both return
     before ever reaching the `stamp` field, on a player that ships this
     feature just the same as one that doesn't. controld cannot tell those
     two branches apart from a genuinely **old** player (pre-stamp code,
     which omits the key on every reply): all three decode as
     `present == false`. See §10 for the residual this leaves (source 3
     going dark specifically while overheating).
   - `ObserveStatusStamp(stamp string, present bool)` carries the
     nil/absent-vs-empty distinction through: `present == false` means "old
     player, source unavailable" and never bumps, regardless of the string
     value. `present == true` is a real observation and is compared against
     the current generation's baseline: no baseline yet (`""`, including the
     session's own post-bump pre-stamp window) → nothing to compare, no
     bump; baseline matches → no bump; baseline differs (including a
     present-but-empty observation against a non-empty baseline — a document
     nobody stamped) → generation bump. This is what detects a
     feral-watchdog-driven navigation or a player-initiated reload within
     one poll interval, without a second CDP round-trip.

   A flattened two-way version of this contract (collapsing "absent" and
   "present-empty" to the same `""` value) cannot tell an old player apart
   from a foreign fresh document, which was exactly what source 3 exists to
   detect — keep the `present` bit distinct from the string value at every
   layer that touches it.

   Client-side route changes (e.g. `/playlist` → `/sleep`) replace no
   document and do not bump anything; they're classified at read time via
   the status probe's `route` field instead (§3, §5).

### 2.2 Readiness barrier

```go
type Stage int
const (
    StageTarget  Stage = iota // a CDP target is connected
    StageHandler               // window.handleCDPRequest installed
    StageStatus                // window.__ffosPlayerStatus answering (new players only)
)
```

`AwaitStage(ctx, st)` blocks until `st` resolves for the *current*
generation (snapshotted once at call entry), `ctx` is done, or the
generation is superseded. The cache is **positive-only**: a resolved `true`
is cached forever for that generation and never re-probed, but a *negative*
result (deadline reached on a live connection) is never cached — the very
next `AwaitStage` call for the same generation/stage re-polls from scratch.
This is what lets a slow cold boot (e.g. a Chromium cache purge) delay
readiness instead of latching a consumer permanently dead for the process.
Fails fast, with no polling at all, when `cdp.Initialized()` is false, both
at entry and on every poll tick.

`StageReady(st)` is the non-blocking, cache-only read of the same flag — the
cheap park-loop condition off-lane producers use (§3.2); it issues no CDP
traffic.

The generation-ready worker (`onGenerationReady`, spawned once per bump)
polls at a flat interval for the first minute, then backs off to a slower
cadence — a page that hasn't installed its handler within a minute is
unlikely to in the next tick, and this worker runs for the entire life of a
generation that might never become ready.

---

## 3. Navigation: gates, park contract, verification

### 3.1 `NavigateHome` / `NavigateHomeInline` gates

Both share one implementation (`navigateAndVerify`) and evaluate gates in
this order, before ever sending `Page.navigate`:

1. **Session shutting down** → `NavEvicted`.
2. **Sleep gate**: the desired player state is sleeping (or
   unknown-with-last-attempted-sleeping — see §7) → `NavSkippedAsleep`.
   Navigating would route a sleeping wall out of `/sleep`.
3. **Error-page gate** (`[NV2]`, "never navigate over `/error`"): the
   daemon-chosen error page deliberately takes the wall off playback;
   navigating to `/` would resume playback over it. This gate reads `route`
   from the status probe **independent of the probe's `protocol` field** —
   route is a plain string, stable across protocol versions, and this
   invariant must hold even against a future protocol bump the rest of the
   package can't otherwise decode (`rawPlayerStatus`, not the
   protocol-gated `currentRoute`). Classified as `NavSkippedOverlay` — the
   error page is treated as an implicit, unregistered overlay owner for
   outcome purposes.
4. **Overlay gate**: any registered owner (`RegisterOverlayOwner`) reporting
   active → `NavSkippedOverlay`.
5. Optional `Network.clearBrowserCache` (`NavOptions.PurgeCache`) — the
   authoritative cache bypass for recovery navigation. commandrouter's own
   pre-refresh `clearBrowserCache` and darkhttpd's `Cache-Control: no-cache`
   are separate, narrower defenses that stay in place independently.
6. Stamp the pre-navigate document with a nonce (`window.__ffosNavNonce` —
   **a separate global from `window.__ffosDocStamp`**, so the pre-bump nonce
   is never visible to the stamp observer in §2.1 and can never cause a
   spurious bump), send `Page.navigate`, bump the generation, then verify
   (§3.3).

### 3.2 `NavigationPending` park contract

`NavigationPending() bool` reports an armed-or-executing navigation.
Off-lane producers that own their own send path consult it immediately
before each send and park while it's true — setupui narration parks inside
its own queue-worker loop, right before each `trySend`; mintpairing/qrdisplay
display sends park *before* taking their own display lock (never while
holding it), since a park can run for the full timeout and holding that lock
across it would serialize every other display mutation behind an unrelated
navigation (see §4).

The flag is a **refcount**, not a bool: `NavigateHomeInline` claims the same
single-flight slot the async `NavigateHome` worker uses
(`claimNavSlot`/`finishNavSlot`), so at most one navigation ever executes at
a time, and the pending count only reaches zero once every in-flight
navigation has cleared. A shared bool would let two overlapping navigations'
terminal-outcome clears race each other and let a parked send through
mid-navigation.

The park itself uses `NavigationTargetGeneration() uint64` — the generation
ID the *in-flight* navigation bumped to, `0` before the bump and reset to
`0` once the navigation finishes. Off-lane parkers exit their wait when
`target != 0 && target == Generation() && StageReady(StageHandler)`,
bounded by a park timeout (delivering best-effort past it). This is not the
same as comparing `Generation()` against a snapshot taken when the park
started: a park can start *after* the bump already happened (a common
timing — `NavigationPending` stays true for the whole verification window
while §3.3 polls), and a plain snapshot comparison can never distinguish
that from "no bump yet," stalling for the full timeout. The target-based
check identifies the specific generation the in-flight navigation produced,
independent of when the parker started waiting: pre-bump (`target == 0`) it
never exits early — preserving the original guarantee that a stale,
already-ready *old* generation can never fool the park — and post-bump it
exits the instant that specific generation is ready.

### 3.3 Verified navigation, and post-navigation route settling

`Page.navigate` always lands the document on `/` first; the client-side
auto-route to `/playlist` only fires once hydration finishes, well after
`StageHandler` resolves. `/` is also a legitimate steady state (nothing
currently cast), not only a transient. `awaitRouteSettled` polls the route
(bounded by the same cap the caller applies — 20s, see §3.4) until:

- it reaches `/playlist` or `/sleep` → verified success;
- it reaches `/error` → classified as `NavSkippedOverlay`
  (`error-page-gate`), **not** a `NavExecuted` failure — this is not a
  broken navigation to retry, and does not cost a boot-recovery attempt
  (§5.1 only counts `NavExecuted`);
- it reaches anything else → a genuine mismatch, `NavExecuted` with an
  error;
- the cap is reached while it's still `/` → accepted as a verified,
  settled landing, not a timeout failure — a navigation that repaired the
  wall into an idle steady state must not burn a boot-recovery attempt or
  report failure to a relayer caller.

An unavailable status probe (old player, CDP hiccup) is "not classifiable"
throughout — never treated as a specific route or a mismatch. One honest
consequence: against an old player, `awaitRouteSettled` returns success on
its FIRST route read (immediately "not classifiable," no polling, no retry),
so "verified success" is a fiction there — there is no actual route
confirmation at all, only the `StageHandler` barrier from the step before.
`NavExecuted` with a nil `Err` means "the barrier resolved and, if a route
could be read, it looked right" — not "the route was confirmed" — and that
distinction only matters in practice for players old enough to have no
status probe to read.

### 3.4 `NavigateHome` vs `NavigateHomeInline`

`NavigateHome(opts, done)` is asynchronous; concurrent calls coalesce to the
latest (a call arriving while another executes replaces any still-queued,
not-yet-started request, resolving the replaced one `NavSuperseded`).
`NavigateHomeInline(opts) error` is for callers that owe a *synchronous*
reply (commandrouter's relayer contract, which holds no lock — the sync
reply is the entire reason Inline exists). It claims the same navigation
slot `NavigateHome` uses; on a loss (something already in flight or queued)
it reports `NavSuperseded` immediately rather than waiting, since its
caller's bounded reply window (capped at 20s, comfortably under hub's 30s
HTTP timeout) cannot afford to wait out someone else's navigation.

---

## 4. Off-lane producers

| Producer | Disposition |
|---|---|
| setupui narration | Own worker + queue. Parks sends on `NavigationPending` (§3.2); registers `Narrating()` as an overlay owner. |
| mediator connectivity | Registers a reconciler that enqueues through the SAME single push worker the edge-triggered `connectivity_change` handler uses (never calls the push body directly) — cache-read-then-send only ever happens in one place at a time. |
| mintpairing / qrdisplay | Overlay owner registration (`DisplayActive`); same `NavigationPending` park at each of its three send sites, entered *before* taking its own display lock. |
| sleep_schedule `applySleepTransition*` | Generation re-check around the CDP send; a moved generation is recorded as `playerUnknownFailed` (§7), never surfaced as an error to a manual-override caller. |
| commandrouter `sendCDPRequest` | Generation re-check; a moved generation returns `ErrGenerationRace` (`errors.Is`-able) — excluded from the refreshArtwork recovery escalation below, since a generation race is not itself evidence the page is broken. |
| commandrouter refreshArtwork recovery | `NavigateHomeInline({PurgeCache:true})` on a refused/failed refresh — the caller holds no external lock. |
| offlinecache kiosk replay | **Split across both lanes.** `AttachOnReconnect` runs INLINE on the CDP connect callback (`main.go`), not as a reconciler: it arms Fetch interception on offlinecache's own socket, needs no page JS, and must be armed before the reloaded document requests assets. Re-applying the item SCOPE is the `replay-scope-resync` reconciler, because the pass it triggers resolves what is playing via `FetchPlayerStatus`, which evaluates `window.handleCDPRequest` and therefore needs a hydrated page. |
| uarewrite kiosk User-Agent rewrite | **Inline only, no reconciler half.** `AttachOnReconnect` runs INLINE on the CDP connect callback (`main.go`) for the same reason as offline replay: it arms Fetch interception on its own socket, needs no page JS, and must be armed before the reloaded document requests assets. Unlike replay there is no page-dependent half — the policy is resolved from config, never from what the player reports. Its socket is independent of the primary CDP client, so a failure that kills only that socket must be recovered without the primary's reconnect, which would otherwise never fire and leave the rewrite retired. A `superviseInterval` supervisor loop is that guarantee — a session that dies IDLE produces no failed request, so nothing event-driven can ever notice it — and the request path is only an accelerator on top of it. Both go through one paced single-flight gate (`redialCooldown`); the constants are named rather than quoted here because they carry a binding relationship (`redialCooldown + probeTimeout < superviseInterval`, or the supervisor rejects its own next tick) documented at their declaration in `uarewrite/interceptor.go`. Note this is where uarewrite and offline replay diverge: replay needs no supervisor because `playlist-refresher` already touches its session every few minutes, whereas the rewrite has no periodic caller. Moving this attach into the reconciler lane reintroduces the request race and the rewrite silently stops taking effect. |
| scheduler/refresher/status/input/viewport | Off-lane, unchanged — read-only or self-retrying. |

**feral-watchdog** is a second, ungated navigation authority
(`systemd_service.go`, navigates the app root on service recovery). The
session's gates are best-effort against it; source 3 (§2.1) detects its
navigation within one poll interval and every registered reconciler re-runs.
Teaching the watchdog a sleep-aware guard is an accepted follow-up, not yet
done (§10).

---

## 5. Boot player recovery state machine

`devicectl` (`boot_recovery.go`) replaces the old ad-hoc atomics choreography
with a bounded, re-entrant machine:

```
Idle → Armed(bootWindow) → Attempting(n) → Succeeded
                          ↘ Deferred(reason, backoff) → Attempting
                          ↘ Expired | Exhausted(maxExecuted=3)
```

Two entry points, both boot-scoped: `MaybeRecoverPlayerOnBootOnline` arms
the machine on the first WAN-confirmed online transition (may run before
DevTools ever attaches) and attempts inline, with **no boot-window gate** —
whatever page is on screen predates WAN and is the broken load this hook
exists to repair, however late WAN arrives. `CompletePendingBootPlayerRecovery`
completes a parked machine on every CDP (re)connect, gated on the boot
window — a connection arriving after the window closed means Chromium just
started with the network already up, a healthy load the boot scoping
promises never to disturb.

### 5.1 Re-entry and budget

Every `Deferred` schedules a backoff timer (15s → 60s → 240s, then holds)
as the **primary** re-entry trigger. A session-generation reconciler and the
sleep tracker's `onAwake` hook (§7) are *accelerators* that re-enter early
via `RetryBootRecovery` — never the only wake-up. **Every** re-entry point
(the backoff timer firing, the accelerator, and the CDP-connect completion)
independently re-checks the boot-window probe before attempting a round: a
device that boots into its sleep window must not keep a 240s probe loop
alive all night and `NavigateHome` a healthy page at the wake edge, hours
past boot.

`n` (executed attempts) counts an evaluate or a navigation that actually
ran — no-connection deferrals and `NavSuperseded`/`NavEvicted`/`NavSkipped*`
outcomes don't count. A single round can reach both a refused
`refreshArtwork` evaluate **and** an escalated navigation; that pair
consumes exactly **one** budget slot, not two — a per-round latch
(`bootRecoveryAttemptRecordedThisRound`) is cleared at the start of every
new round. `Exhausted` is terminal at `n == 3`. Every terminal-state
transition function (`succeedBootRecovery`/`expireBootRecovery`/
`deferBootRecovery`) guards against moving an already-terminal state — a
late `NavResult` callback arriving after the machine settled some other way
must never re-transition it.

### 5.2 Classification, as implemented

Each round issues **one** `window.__ffosPlayerStatus` evaluate
(`readPlayerStatusRaw`), shared by two consumers of that single result:

1. **The `/error` safety check runs first, unconditionally, independent of
   `protocol`.** This is a deliberate divergence from the original design
   draft, which listed `bootHydration=failed` and `hasArtwork=false` above
   the route rows. A hydration-failed page and an error-routed wall can
   co-occur (the watchdog's own restart path, or a stale error carrying
   into a fresh hydration attempt), and "never navigate over `/error`" must
   hold regardless of what `bootHydration` says, and regardless of a future
   `protocol` bump this classifier can't otherwise decode. Route gates are
   safety, not just another classification signal — they go first,
   structurally, rather than relying on every future callsite re-deriving
   the ordering correctly.
2. **The rest of the classification is protocol-gated** (`decodePlayerStatus`
   — a `protocol` value other than the one this build understands is
   treated as "probe unavailable," same as a malformed or absent response,
   never misdecoded against a shape it may no longer match).

Total table, in the order actually checked:

| Row | Condition | Outcome |
|---|---|---|
| 1 | `cdp.Initialized() == false` | Deferred, **no** attempt counted |
| 2 | `route == /error` (protocol-independent) | Expired — never navigate |
| 3 | `route == /sleep` | Deferred (backoff + wake accelerator) |
| 4 | `bootHydration == pending` | Deferred, no attempt |
| 5 | `bootHydration == halted_cleared` | Succeeded — disconnect cleared the cast mid-boot, wall intentionally dark |
| 6 | `bootHydration == failed` (scoped to `initCastInfo` failures only, never a display-settings read failure) | attempt: NavigateHome |
| 7 | `bootHydration == halted_preserving` | no separate action — already covered by rows 2/3 when applicable, otherwise falls through |
| 8 | `hasArtwork == false && bootHydration == ok` | Succeeded — nothing cast |
| 9 | `hasArtwork == true && !handlerRegistered && route == /playlist` | Deferred — route mounted, handler not yet |
| 10 | structured status answered but nothing above matched | fall through to the live refresh probe below |
| 11 | structured status unavailable, OR fell through from row 10: `StageHandler` timeout on a live connection | attempt: NavigateHome |
| 12 | `refreshArtwork` ACKed | Succeeded |
| 13 | `code=handler_pending` | Deferred (player replays it) |
| 14 | `code=no_artwork` | Deferred (state regressed between probe and call) |
| 15 | `code=preview_update_failed`, first occurrence this boot | Deferred once |
| 16 | `code=preview_update_failed`, repeat | attempt: NavigateHome |
| 17 | bare non-ACK / unknown code, this round already read structured status successfully (live proof of capability) OR the capability fuse reads present (§5.3) | attempt: NavigateHome |
| 18 | bare non-ACK / unknown code, capability fuse reads absent | conservative: log + Deferred — **never** navigate |
| 19 | `NavResult` Executed, verified | Succeeded |
| 20 | `NavResult` Executed + err, or post-nav verify failed | Deferred (counts as attempt) |
| 21 | `NavResult` SkippedOverlay (registered overlay, pre-nav error-page gate, **or** a post-nav landing on `/error` — §3.3) | Expired |
| 22 | `NavResult` SkippedAsleep | Deferred (backoff + wake accelerator) |
| 23 | `NavResult` Superseded/Evicted | Deferred, no attempt count |

### 5.3 Capability fuse

`checkPlayerStatusCapability` resolves whether the connected player
advertises `contracts.playerStatus` in its on-disk manifest
(`/opt/feral/feral-player/ffos-player-contract.json`). A read **failure**
(unreadable — boot ordering, an OTA mid-replace of the player bundle) is
re-checked on every call and never latches. **Absence** from a manifest that
*was* successfully read latches conservative mode for the rest of the
boot's recovery lifecycle.

The fuse's scope is narrower than "never navigates": it gates **only** row
17/18's unclassified-refusal fallback. Rows 6, 11, and 16 (hydration
failed, handler-never-ready, repeat preview-update-failed) navigate
unconditionally regardless of the fuse's state — each already has its own
independent evidence of a dead or wedged page that doesn't depend on
classifying the structured-status capability at all.

---

## 6. Startup OTA gate: claim-primary narration policy

One `otagate.Gate` narrator instance is shared by all three entry points
(`RequestUpdate`/`EnsureLatestBeforeClaim`/`EnsureLatestAtStartup`), so a
single `OnPermanentFailure`/`OnProgress`/`OnUpdateSucceededNoReboot` policy
governs every caller. The policy is decided **live, at emit time**, from
the claim snapshot — never captured at flight start — so a settled device's
`updateToLatest` RPC that happens to join a flight another (pre-claim)
caller started still gets the settled-device policy:

- **Claim not settled**: today's pairing-flow behavior — `ShowUpdating`
  during the ladder, `ShowJoinFailed` on permanent failure.
- **Claim settled**: `ShowUpdating` during the ladder; on permanent
  failure, `HideIfShowing(StateUpdating)` + a `Warn` log — never
  `ShowJoinFailed` (a settled/claimed device has no "join failed" to
  report; that narration belongs to the pre-claim pairing flow only).
- **Post-ladder watchdog**: a successful ladder that doesn't reboot within
  the watchdog timeout clears the stuck "updating" overlay unconditionally
  (not claim-gated) — whichever entry point's narration is still up, it no
  longer reflects real progress either way.

---

## 7. Sleep tracker: four-value state + `onAwake` edge

```go
type playerSleepState int
const (
    playerAwake         playerSleepState = iota // zero value == pre-tracker nil semantics
    playerSleeping
    playerUnknownFailed                          // an apply FAILED
    playerFreshDocument                          // invalidatePlayerSleepState: page reloaded awake
)
```

Alongside the enum (all guarded by `sleepApplyMu`): `sleepAttempted` (state
of a *failed* apply), `sleepLastGood` (last successfully-applied state),
`sleepDesiredAtInvalidate` (the schedule's desired state at invalidation
time — read from the schedule loop's own per-tick cache, never a fresh file
load on the CDP connect-loop goroutine).

- Successful apply → `playerAwake`/`playerSleeping` (updates `lastGood`).
- Failed apply (including a generation-race failure on the send — a
  distinct sentinel error, `errSleepTransitionGenerationRace`, suppresses
  the `onAwake` edge below and skips the panel-power/status-refresh steps,
  while still reporting success to a manual-override RPC caller, since the
  send itself did not fail) → `playerUnknownFailed`.
- `InvalidatePlayerSleepState` (called on every CDP reconnect — a
  restarted kiosk reloads awake) → `playerFreshDocument`.
- `playerAligned` ⟺ `(playerAwake ∧ target==awake) ∨ (playerSleeping ∧ target==sleeping)`.
  `playerUnknownFailed` and `playerFreshDocument` are **never** aligned —
  that's the entire point of both: force a re-drive.
- **Navigation gate** (§3.1 step 2, `playerSleepGateBlocked`): blocked when
  `playerSleeping`, OR `playerUnknownFailed` with the last *attempted*
  state sleeping, OR — independent of the tracker's applied state — the
  **schedule's desired state** (`sleepScheduleDesiredCache`, refreshed
  immediately on every schedule/override write, not only each loop tick)
  reads sleeping. The desired-state check closes the window where a fresh
  document (tracker at `playerFreshDocument`, not yet re-driven) sits
  inside a sleep window and a recovery navigation would otherwise route it
  out of `/sleep`.
- `onAwake` edge predicate, on a successful transition to awake:
  ```
  wasSleeping := state == playerSleeping
              || (state == playerUnknownFailed && attempted == sleeping)   // a failed SLEEP counts
              || (state == playerUnknownFailed && lastGood == sleeping)    // a failed WAKE retry counts
              || (state == playerFreshDocument && desiredAtInvalidate == sleeping)
  ```
  The fourth disjunct closes the gap where a kiosk restart at, say, 03:00
  (schedule says sleeping) leaves the sleeping re-drive skipped or
  deferred; without it, the wake boundary at 07:00 would never fire the
  `displayAt` recompute. The predicate fires on the sleep→awake **edge**
  only, never on a routine re-drive of an already-awake player (every CDP
  reconnect invalidates the tracker, and `wakeNow` re-drives
  unconditionally) — firing on those would force-push and visibly remount
  a byte-identical active set on top of the on-connect recompute that
  already serves a reloaded page.

---

## 8. Machine-readable refusal codes

`playerresponse.Refusal(result)` is the single place that boundary-classifies
a refused player reply — every consumer goes through it. `code` is the
reply's own `message.code` field when present; otherwise a fallback match
against the exact `error` reason string. That reason-string match is
**same-release redundancy**, not a legacy/backward-compatibility path: the
strings and the `code` field shipped in the same ff-player release (an old
build with neither has no `code` and no matching reason string either —
its refusal replies predate this contract entirely). `code` is `""` when a
refusal is genuine but unclassifiable — a bare non-ACK, or an unrecognized
reason.

```go
const (
    CodeHandlerPending      = "handler_pending"
    CodeNoArtwork           = "no_artwork"
    CodePreviewUpdateFailed = "preview_update_failed"
)
```

---

## 9. Paired-rollout contract

`feral-controld` and `ff-player` ship from separate repositories and are
**not** deployed atomically. The contract in §2–§8 is designed so a device
running any combination of the two repos' recent builds degrades safely,
never destructively:

- **Merge order: ff-player merges first.** The player-side PR
  (`fix/offline-network-state-recovery` or its successor) must land and
  ship before the controld-side PR that depends on it. controld's decode
  paths (`rawPlayerStatus`/`decodePlayerStatus`, the stamp observer,
  `playerresponse.Refusal`) are all written to degrade gracefully against
  an old player that predates this contract entirely — see below — so the
  reverse merge order is safe by construction, but the forward order (new
  controld, old player) is the one that actually ships in practice during a
  rollout window and is the one this section is about.
- **New controld against an old player enters conservative mode by
  design — but "conservative" is narrower than "never navigates," and this
  section must not overclaim it.** An old player's manifest lacks
  `contracts.playerStatus` (§5.3): the structured-status probe is
  unavailable everywhere it's consulted (never misread — every decode path
  treats "unavailable" as "not classifiable," never as a specific value),
  and the stamp contract's source 3 (§2.1) is permanently inert (an old
  player never echoes the `stamp` key at all). The boot-recovery capability
  fuse latches conservative and gates **only** rows 17/18 (the unclassified-
  refusal fallback, §5.3) — rows 6, 11, and 16 (hydration failed,
  handler-never-ready, repeat `preview_update_failed`) still escalate to
  `NavigateHome` against an old player exactly as they would against a new
  one, since each already has its own independent evidence of a dead page
  that doesn't depend on the status probe at all. The `/error` safety gate
  (§3.1 step 3) is itself **fail-open** against an old player: with no
  status probe to read `route` from, `isErrorPage` reads "unavailable" and
  never gates. This is mitigated, not closed: an `/error` page — even on an
  old player — is still a live React page with `window.handleCDPRequest`
  installed, so the classifier's own dead-page rows (11, and the
  handler-presence check inside row 17/18) never see the "genuinely dead
  document" evidence they require, and an unclassified refusal off that
  page is conservatively deferred anyway. The net effect is real but
  narrow: a build that can't confirm what an old player is doing withholds
  escalation specifically from the one row that has no independent evidence
  of its own, while every row with independent evidence — including the
  liveness check that stands in for the fail-open `/error` gate — behaves
  identically to a new player. The accepted cost is reduced recovery
  coverage against old players, never incorrect recovery.
- **`protocol` and the manifest's `contracts.playerStatus.version` must
  bump together.** Both are checked independently
  (`statusProtocolVersion` in Go, `PlayerStatusContractVersion` — currently
  `1` in both places) and both gate independently: a `protocol` mismatch on
  the live `__ffosPlayerStatus()` payload falls back to "unavailable" for
  everything except the protocol-independent `/error` safety check (§3.1,
  §5.2); a manifest version mismatch on `contracts.playerStatus` is treated
  identically to the contract being absent (§5.3's fuse). A future breaking
  change to the status payload's shape must bump both together, or a
  player could advertise capability its live payload no longer honors.
- **Version skew is intended to be silent and safe, not loud.** There is no
  hard version-negotiation handshake; an unrecognized `protocol` or
  contract version is simply treated as "this source is unavailable," and
  every consumer already has a defined behavior for "unavailable" (defer
  conservatively, skip the check, fall back to the live refresh probe).
  New protocol/contract versions should preserve this property: prefer
  additive fields a mismatched consumer can ignore over changes that would
  make an old consumer misread a new payload.

---

## 10. Accepted residual risks

- **Source 3 goes dark on a NEW player specifically while it is overheating
  (or on any other exception inside `getStatus`).** `CanvasService.getStatus`
  has two branches — the overheating check and its top-level `catch` — that
  return before ever reaching the `stamp` field (§2.1). controld cannot
  distinguish that reply shape from a genuinely old player; both decode as
  `present == false`, "source unavailable, never bump." A feral-watchdog
  navigation or foreign document replacement occurring during an overheating
  episode is therefore invisible to the generation model until the episode
  ends and `getStatus` reaches its normal-path return again. Not yet fixed;
  the fix (adding `stamp` to the two early-return shapes) belongs to
  ff-player.
- **feral-watchdog is a second, ungated navigation authority** (§4). The
  session's gates are best-effort against it; source 3 detects its
  navigation within one status-poll interval (~5s) on new players only —
  old players have no stamp carrier at all, so a watchdog navigation on an
  old player is invisible to the generation model until the next CDP
  reconnect. Teaching the watchdog a sleep-aware guard is a known follow-up,
  not yet implemented.
- **Casts remain off-lane.** A cast can race a navigation's teardown
  window; the generation re-check (§4) turns silence into a loud failure
  for the caller to retry, it does not prevent the race.
- **The stamp-mismatch generation source has a narrow, accepted false-negative
  window.** Two rapid foreign-document replacements landing within the same
  ~5s poll interval, where the second document happens to produce a
  colliding stamp value, would self-heal on the *next* poll rather than the
  first — estimated well under 0.1% of generations in practice, given the
  stamp's entropy (a per-write timestamp-derived value) and the rarity of
  back-to-back foreign navigations. Not a correctness issue, since the
  eventual detection still lands within one additional poll interval — a
  latency characteristic, not a lost-forever one.
- **Conservative mode on old players narrows one classification row to
  logging** (§5.3, §9 — not "all boot recovery," see §9 for the precise
  scope) — by design, not a gap to close.
- **`NavigateHomeInline`'s 20s verification wait** normally costs one
  relayer handler slot out of the pool for its duration and fits under
  hub's 30s HTTP timeout with headroom. The commandrouter refreshArtwork
  escalation path (§4) can extend this: a `sendCDPRequest` that hangs on a
  wedged (but not yet torn-down) socket can burn cdp's own 15s
  `sendRequestTimeout` BEFORE Inline's 20s cap even starts, for a ~35s
  worst-case handler — over hub's 30s timeout. In practice this is rare:
  a send timeout that severe normally also tears the underlying CDP
  connection down, so the Inline call that follows fails fast (CDP no
  longer initialized) rather than running out its own full 20s.

---

## See also

- `components/feral-controld/playersession/session.go` — package doc and
  the primary implementation this document describes.
- `components/feral-controld/devicectl/boot_recovery.go` — the boot
  recovery state machine (§5).
- `docs/architecture.md` — "Player page authority: `playersession.Session`"
  for the one-paragraph summary in the context of the daemon's overall
  architecture.
