# Network dead-end recovery UX

> ## Implementation status (updated 2026-08-05)
>
> **The entire `ffos-user` (device/controld) half of this plan is IMPLEMENTED,
> reviewed, and committed** on branch **`feat/wiredlink-seam`** (based on
> `docs/network-recovery-ux` = `develop` + the two plan docs). Every commit
> passed a fresh-context reviewer loop (`Verdict: accept`) and the full gates
> (gofmt, `go vet`, full `go test -count=1 ./...`, `-race` on touched
> packages, `golangci-lint --new-from-rev` — use the `~/go/bin` binary, the
> Homebrew one panics on this module's go 1.26). No `users/**` changes:
> package rail, no `RELEASES.md` entry needed. Branch NOT pushed, no PR yet.
>
> | Commit | Landed |
> |---|---|
> | `b976e5f` | `status.LinkChecker.WiredLink` seam (§4.5 amendment 3 semantics; `linkProbe` → `linkResult{link,wired}` one-pass refactor) |
> | `710be87` | **Stage 1** (§4.8+§4.3): portal picker+manual+hidden checkbox (`ssid_manual`/`hidden` form fields, manual wins non-blank, machine trims manual branch only); `wifictl.Join(ctx,ssid,psk,hidden)` (hidden ⇒ `hidden yes` + skip `waitForSSID`; empty PSK omits `password`); `wifiProfileList` (uuid/ssid/key-mgmt) + SSID-matched PSK/open-scoped deletion; `Config.JoinTimeout` 120s in `applyJoin` ⇒ `JoinErrTimeout`; offline window converted to SAMPLE-COUNTED (constraint 7) with bounded pause (`pauseOfflineWindow`: discard past one window, 2-fresh-sample debt) — existing wall-clock tests rewritten to tick cadence via `h.tickN`; relocation ladder tolerates 2 link-probe unknowns (`relocUnknowns`); `clearOffline` = single-truth full reset (window+ladder+raise-escalation) |
> | `435333c` | **Stage 2** (§4.6): `setup_error` extension state; `resolveConnectingState` generalized to `resolveExtensionState` + `sendFallbacks` table (union-shaped: state fallback or hide) + per-state `extSupport` verdict map; escalation latches `noteAPRaiseFailure`/`noteAPReleaseFailure` (streak 8, one notify, `scanning` push suppressed while latched, raise flavor also reset at every `clearOffline` episode boundary with explicit `ReasonSetupErrorCleared`); executor topic-wait expiry paints `ShowConnectingIfShowing(StateFinalizing)` on a CONFIRMED-offline tri-state probe (`SetInternetProbe`, error ⇒ silent hide); portal `result.html` copy |
> | `89b1a9c` | **Stage 3** (§4.1+§4.2+§4.4): `provisioning/session.go` (NEW — most escape-policy logic lives there). Session policy latched in `transition` via `latchSessionPolicy` (unknown reasons INHERIT — the one-typo rule); timers: `armSessionTimer` on `ensureAPUp` success / `applyRescan`, `cancelSessionTimer` in `applyJoin`, expiry in `onTick`'s raised-`StateAPActive` branch with portal-activity deferral (+15 min ceiling) and 2h cap; recheck blink `runRecheckBlink` runs SYNCHRONOUSLY on the loop goroutine (suppression is structural — no flag), `activateInRangeProfiles` (MRU, hidden last, listing error aborts), `skipNextPreAPScan` one-shot; episode `episodeSample` (4-term confirmed predicate, hub-contact budgeted pause charged per tick, `episodeFreshSamplesAfterPause`, ladder 5/10/20 via `episodeLadderSamples`, settle after `EpisodeRaiseCycles`); claim snapshot `SetClaimed` writes SYNCHRONOUSLY under `m.mu` (event is only a cancel nudge; loop reads via `isClaimed()`), boot seed moved to `Machine.Start` fed by `app.StateLoadKnown` (quarantined state file = unknown = claimed); §4.4: `bootNarration`→`episodeNarration` marker, `narratedOfflineReason`/`silentOfflineReason` sets, constraint-10 dedupe extension + constraint-4(b) narrated→silent rewrite in `transition`, D4 repaint in `onConnectivity`'s provisioned tail; wired exit `exitRaisedAPForWire`; hub `SetContactObserver` (routes cast/status/status_v2, non-loopback) + portal `ActivityObserved` (connect/rescan handlers only); `config.ProvisioningTuning` permissive `provisioning` JSON block (RawMessage) + `setupIncompleteDisabled` kill-switch + `config.example.json` |
> | `f565862` | **Stage 4** (`startWifiSetup`, sibling plan stage 2): `CMD_START_WIFI_SETUP`; `Machine.StartWifiSetup` admission (busy joining/starting; `ErrWiredLinkActive` fail-closed on wire/probe-error/nil-probe) queues `evUserSetup` → `applyUserSetup` runs the entry triple + `armSessionTimer` AFTER the transition (load-bearing for the ap_active accept); executor handler replies BEFORE radio work (`{ok,ssid}` / `wired_link_active` / `busy` / `unavailable`); `contract:"2"` on `DeviceStatusResponse` (no omitempty; hub test pins equality with `StatusContractV2`) |
> | `c594a17` | **§4.7** health surface: `Machine.Snapshot()` from mu-guarded caches (`linkOutcome*`/`wired*` written ONLY by real probes — apUp short-circuit never writes; `ExternalLinkDetail` gives link+wired from one nmcli pass via `Config.ActiveLinkDetail`); `status.NetworkHealth` on hub `/api/status` + `/api/v2/status` + `getDeviceStatus` (executor `SetNetworkHealth`); `deferred` sub-state mirrors the episode's contact pause |
>
> **Implementation amendments made under review (already reflected in the
> relevant sections/docs):** `startWifiSetup` also accepts from `ap_active`
> (idempotent refresh, fresh 30-min clock — recorded in the sibling plan §5);
> the user-requested expiry lands `ap-session-ended-silent` for BOTH claim
> states; the recheck blink is synchronous rather than flag-suppressed; the
> §4.6 `setup_error` raise-latch also resets at every episode boundary.
>
> **Cross-repo halves — ALL IMPLEMENTED (2026-08-05), each through its own
> repo's reviewer loop:**
> 1. **ff-player** — DONE: `setup_error` rendered ("Setup needs attention"
>    title + reason prose; bare-request fallback line), manifest + CDP
>    validator + tests, on branch `feat/setup-error-display-state`
>    (`ed32713`, not pushed). The shipping manifest and controld's setupui
>    testdata fixture are byte-identical again (fixture gained the
>    `stateFields.setup_error` entry here, commit `ca6a204`, which also
>    aligned ALL new on-screen controld copy to the player's "Art Computer"
>    voice — session.go narrations + both setup_error messages).
> 2. **ff-app** — DONE on branch `feat/app-triggered-wifi-setup`
>    (`99e1c286`, not pushed; four review rounds): capability gate behind
>    `FF1BluetoothDeviceActionsNotifier.resolveConfigureWifiRoute`
>    (live-endpoint-first under a browse hold → `Ff1LanPairableGate` →
>    relayer `contract:"2"`, fail closed to BLE); three-step flow with the
>    send inside the dialog's processing button, gate-confirmed LAN endpoint
>    tried before the router, timeout-is-success on EVERY transport (Dio
>    types included), refusal only on explicit `ok:false`, delivered sends
>    never downgraded (even on local removal failure); honest
>    `FF1DeviceNotFoundError` BLE-not-found copy; §4.7 health surface
>    (`FF1NetworkHealth` domain model on both status transports, LAN-read
>    `/api/v2/status` preferred, `deferred` copy) — the LAN health read is
>    deliberately one-shot per watch because `status_v2` is a counted
>    contact route (polling would pin the raise `deferred` warns about).
> 3. **ffos** — DONE: `docs/DEVICE_LIFECYCLE.md` documents the session
>    policy table, escape cadence, `startWifiSetup`, and the
>    `connecting`/`setup_error` states, on branch
>    `docs/network-recovery-lifecycle` (`d1b00a4`, not pushed).
>
> **NOT yet done — hardware-gated bench items only** (§5 list below):
> hidden-SSID join against a real hidden AP; open-network join; one full
> link-present episode on a bench frame; the router-cold-boot recheck
> observation; portal-activity deferral with a real phone (idle phone must
> NOT defer); LAN pairing of an unclaimed frame on a WAN-less network;
> venue-network counted-endpoint check; power-restore-to-WAN timing.
>
> Working notes for the next session: per-module gates run from
> `components/feral-controld`; provisioning tests drive ticks via
> `h.tick/h.tickN` (15s per tick, `windowSamples = 20`); the reviewer loop is
> mandatory before commit (see CLAUDE.md); memory file
> `network-recovery-implementation.md` mirrors this status.

Execution plan, written to the `PLANS.md` contract. Spans `ffos-user` (`feral-controld`),
`ff-player` (setup display contract + copy), and `ff-app` (health surface; the Wi-Fi
re-setup flow itself is specified in `docs/app-triggered-wifi-setup.md` and adopted here
with three named amendments, §4.5). No `users/**` changes, so the controld half ships on
the **package rail**.

**Delivery-skew posture.** The player bundle is OTA-replaced independently of the controld
package — the two are **not** deployed atomically (`docs/player-session-recovery.md` §9
names "new controld, old player" as the ordering that actually occurs during rollout
windows, and `feral-service-update.sh` does not carry the player). This plan therefore
adds contract states in the **extension tier** with send-time downgrades and leaves the
manifest's required set untouched. This is not old-player feature support (the product
decision is that both halves release together and no feature is designed for stale
players); it is tolerance for the hours-to-days ordering skew and rollbacks that the
delivery mechanism itself produces. Growing the required set would make
`narrationSupported` latch `supportNo` for the process lifetime on every device that takes
the controld package first (`setupui.go:727-757`, and the explicit warning at
`setupui.go:63-73`), disabling *all* setup narration — a dead end worse than any this plan
fixes. One skew-window consequence to handle rather than accept: `ap-recheck` (§4.2) is
the first *recurring* `connecting` push, and its cadence is unbounded on **claimed**
frames — a naive downgrade would flash a false "Couldn't connect" title over exhibition
artwork ~48 times a day for the whole skew window ("hours-to-days" is too long for
that). So `ap-recheck` specifically **downgrades to `Hide()`, not to `join_failed`**,
when the manifest provably lacks `connecting` (`connectingUnsupported()` positive
evidence): the blink clears the QR — satisfying constraint 4(a), never leaving a stale
hotspot QR painted with the AP down — asserts nothing false, and the re-raise repaints
`softap_qr` minutes later. The hide runs on the machine-owned narration path (the same
ownership as `ap-session-ended-silent`), so it cannot collide with an executor-held
overlay. The one-shot `connecting` pushes keep their existing `join_failed` downgrade.

**Goal.** Close every provisioning dead-end that has a sanctioned in-product fix, and
name and accept the residue explicitly. A full audit found eleven absorbing states; they
reduce to five structural defects. This plan fixes the defects, not the symptoms.

**Out of scope, by explicit decision:** physical escape hatches (power-cycle counting) and
LAN-surface authentication hardening (`:1111` posture is accepted as-is; the durable
answer is the v2 controller-authentication profile).

---

## 1. Current-state summary

### 1.1 The dead-end inventory

An exhaustive scan of the provisioning machine, the narration surface, and the control
channels found these absorbing states (file references are pre-change):

| # | State | Mechanism |
|---|---|---|
| D1 | **Joined a network with no usable WAN** (captive portal, dead upstream, wrong-but-real SSID) | Association is a live link → offline window never arms → AP never returns (`provisioning.go:1106-1141`). Unclaimed devices additionally never show the claim QR (`provisioning_wiring.go:294-341`). |
| D2 | **Claimed device whose WAN dies permanently** (ISP change, router demoted to AP) | Same guard; with the phone off-LAN there is zero control path (relayer dead, hub LAN-only, AP suppressed). |
| D3 | **Hidden SSID cannot be provisioned at all** | Portal renders a picker whenever the scan is non-empty — no manual entry (`portal/templates/index.html:167-175`); `wifictl.Join` never passes `hidden yes` (`wifictl.go:508-511`), so even manual entry fails "not found" with advice ("move closer") that cannot help. |
| D4 | **`joined-conn-unknown` terminal hedge** | Post-join reachability query failed → "Checking internet access…" is the terminal narration; the confirming re-query lands in the generic tail and is deduped (`provisioning.go:104-109`, `:900-919`, `:1463-1483`). |
| D5 | **`apDownPending` wedge** | NM refuses to delete the hotspot profile → `ensureAPUp` refuses forever (`provisioning.go:1522-1524`); the hotspot may still broadcast with nothing listening. Screen: black, `join_failed`, or "scanning" forever depending on entry path. |
| D6 | **AP raise fails persistently** (rfkill, empty `/etc/hostname`, `:80` bound) | `reconcile` retries every tick with no counter, no escalation, no error state (`provisioning.go:1486-1501`). Screen shows "scanning" forever. |
| D7 | **Flaky link probe starves the AP** | The 5-minute window needs 20 *consecutive* clean `linkAbsent` samples; one 3s timeout or parse failure resets the count to zero (`provisioning.go:1116-1141`), and a single `linkUnknown` permanently forfeits the boot relocation shortcut (`:1067-1076`). |
| D8 | **sys-monitord dead or probe endpoint blocked** | `Online()` can never confirm → parked in `StateOfflineRetrying` narrating "no internet access" on a possibly healthy device; unclaimed devices never get the on-screen claim QR. |
| D9 | **Changed password + profile not named after its SSID** | `deleteWifiProfiles` matches profile **name**, not target SSID (`wifictl.go:556`) → stale PSK reused → "Wrong Wi-Fi password" shown to a user typing the correct one, forever. |
| D10 | **`StateJoining` has no timeout** | A wedged NM call blocks the single loop goroutine with the AP down and the portal stopped (`provisioning.go:1370`). |
| D11 | **A raised AP can never observe recovery** | `StateAPActive` deliberately has no link-based exit (`provisioning.go:1250-1259`) and the hotspot holds the single radio. A router that reboots for ten minutes during an outage strands the frame in setup mode forever; a WAN-less LAN whose link returns does the same; replugging a cable does nothing. |

Adjacent findings folded in: open (passwordless) networks are likely un-joinable (empty
PSK still sent as `password`); manual-entry SSIDs keep leading/trailing whitespace; the
portal success page instructs the user to reconnect to an AP that no longer exists; an
unclaimed wired no-WAN device ends at a black screen after the 60s topic wait;
`join_failed` reaches the screen as a machine token while the actionable prose goes only
to a phone portal that is usually already gone.

### 1.2 The five structural defects

1. **One escape signal.** The only automatic recovery (raise the AP) listens to exactly
   one trigger: sustained link loss. Every "link alive but useless" state — the states
   users actually hit — has no exit.
2. **Unbounded waits.** `StateAPActive` and `StateOfflineRetrying` are level-triggered
   retry loops with no attempt counters and no failure states. "Still working" and "will
   never succeed" are indistinguishable on screen.
3. **Status without agency.** Screen copy reports state but never a next step, a time
   bound, or the device's identity.
4. **No user-intent channel.** Nothing lets a user say "change network" or "restart
   setup". (`docs/app-triggered-wifi-setup.md` fixes this; this plan integrates with it.)
5. **Join-path correctness gaps** (hidden SSIDs, open networks, stale profiles) that turn
   the one working recovery flow into its own dead end.

### 1.3 A load-bearing fact the original audit under-weighted

**Claiming does not require WAN.** `connect` (`devicectl/executor.go:539-595`) is a purely
local command — it validates, persists the claim, and fires the claim observer, with no
relayer or network I/O — and it is reachable over the LAN hub (`setup-flow.md`, hub
`connect` is the claim step). What requires WAN is only the on-screen claim-QR *painting*
(the relayer topic). So an unclaimed frame associated to a dead-WAN network is claimable
today by a phone on the same LAN via the app's normal mDNS pairing flow
(`_ff1._tcp`, TXT `claimed=false`). This fact shapes the whole escape policy in §4.1: the
first-choice escape from every live-link dead end is to **stay on the network and keep the
LAN control plane reachable**, because that is where both claiming and `startWifiSetup`
live. Raising the AP — which drops the station and takes the frame *off* that LAN — is the
fallback, not the policy.

### 1.4 Two evidence shapes, two opposite policies

Every dead-end above falls into one of two shapes, and they want opposite radio policies:

- **Link alive but useless** (D1, D2, D4, D8): the association is the asset — it carries
  the LAN control plane (§1.3) and it is the only vantage from which WAN recovery is
  observable. Policy: keep the station up as much as possible; AP phases are short
  fallbacks.
- **Link confirmed absent** (relocation, changed password, vanished SSID — the
  `sustained-offline`/`relocated` raises): the setup QR is the asset — a walk-up user must
  find it. But a permanently-raised AP is blind (D11), so policy: keep the AP up almost
  all the time, with brief periodic rechecks in station mode so a recovered network is
  noticed within one cycle.

§4.1/§4.2 implement exactly this split. Conflating the two shapes — one cadence for both —
is how a previous draft of this plan produced new dead ends; the split is load-bearing.

---

## 2. Constraints and invariants

All verified against source; violating any of these is a regression:

1. **Single radio.** Raising the AP drops any station association; a raised AP means the
   device cannot observe WAN recovery on Wi-Fi and cannot be reached over the LAN.
   Corollary (§1.4): for the link-alive shape, AP phases must be the minority of each
   cycle; for the link-absent shape, station rechecks must exist but stay brief.
2. **#233 stands for claimed devices.** A live local link on a claimed device never
   triggers an automatic raise. The mid-life exhibition outage stays silent on screen —
   artwork over cached content is the correct behavior, and this plan does not add a
   persistent badge or overlay to it (rejected alternative, §4.8).
3. **Every AP raise site pairs `clearOffline()` + `resetJoinStatus()` + `transition()`**
   (`provisioning.go:767`, `:1186`, `:1228`, `:1290`).
4. **Every AP teardown site repaints the screen, and every narrated episode ends with an
   explicit repaint or hide.** New invariant, symmetric to constraint 3, in two halves:
   (a) **after a teardown, with no raise in progress**, no state may leave `softap_qr`
   (or `scanning`) on screen while `apUp == false` — the scoping clause matters:
   `scanning` with the AP still down is the *correct* screen during an in-progress raise
   (`ensureAPUp` narrates before `ap.Up` and retries across ticks), so the invariant
   binds teardowns, not raises. Every teardown transitions to a *named* state carrying a
   reason the notifier explicitly handles (§4.4 lists them; the machine — which holds the claim snapshot —
   picks the narrated or the silent-hide variant, so the notifier stays a pure reason
   switch); (b) any transition from a **narrated** `StateOfflineRetrying` reason into a
   **silent** reason (`offline`, `link-present`) emits the explicit silent-hide reason
   instead — relying on the notifier's leave-the-screen-as-is default there would strand
   the last narrated panel (e.g. `ap-recheck`'s "Setup mode will return in a moment") as
   a permanent lie over a claimed frame's artwork. The silent-hide variant is
   `narrating`-guarded like every other hide (`provisioning_wiring.go:228-235` documents
   why an unguarded hide is a regression). Pinned by a generic test in §5 covering both
   halves.
5. **All machine transitions run on the single loop goroutine** (`provisioning.go:531`).
6. **Wired devices never auto-raise the AP.** The AP cannot fix a wired network, an
   `online` reading tears it down anyway, and the portal on a routable interface is an
   unauthenticated credential form. The probe is the `WiredLink` seam specified in
   `app-triggered-wifi-setup.md` (not `ExternalLink`, which counts stations), with its
   survey semantics pinned here (and back-ported to the sibling plan — §4.5 amendment 3):
   **the survey is valid when the nmcli output contains at least one ethernet or Wi-Fi
   device row** (the existing `surveyed` rule, `status/linkcheck.go:128-134` — corrupt or
   empty output proves nothing and must surface as an error, exactly as today); given a
   valid survey, the **wire verdict is computed from ethernet rows only**, and a valid
   survey with no ethernet row is confirmed-no-wire (`false, nil`). A probe error pauses
   the §4.1 window (§4.3 semantics), it never arms or advances it.
7. **The offline window is sample-counted, not wall-clock.** Its contract: the raise
   requires 20 confirmed-`linkAbsent` samples at tick cadence with **no `linkPresent`
   sighting** since arming. §4.3 changes only how inconclusive samples are treated
   (bounded pause, not reset). A `linkPresent` sighting always fully resets — that half of
   the current contract (`provisioning.go:1119-1131`) is untouched.
8. **Claim state is derived, not stored twice** ("ConnectedDevice with a non-empty ID",
   `executor.go:1300-1310`). The machine does not call into `state` from the loop
   goroutine (that would take the state lock mid-loop — the same class of coupling commit
   `116a8ed` just removed elsewhere). Instead it keeps a loop-visible claim snapshot fed
   by the existing claim observer — which fires on both claim (`executor.go:571`) and
   unclaim (`executor.go:2641`) — plus one boot-time read. **Unknown claim state (state
   file unreadable at boot) is treated as claimed** — the fail-safe direction, since the
   worst mistake is auto-raising the AP over a claimed exhibition frame.
9. **Contract states are added in the extension tier** with send-time downgrades
   (the `stateConnecting`/`resolveConnectingState` pattern, `setupui.go:289-303`). The
   required set stays at its current seven states. The downgrade machinery is
   load-bearing skew tolerance and is **kept**, not deleted.
10. **`transition` dedupes same-state re-emissions**; the link-lost window path relies on
    that (`provisioning.go:1460-1462`). §4.4 extends reason-change notification to
    `StateOfflineRetrying` for transitions **into the narrated reason set only**;
    transitions into `offline`/`link-present` legs keep today's dedupe, pinned by a
    regression test (this is what keeps the exhibition path silent).
11. **Automatic escapes act only on confirmed evidence.** The machine never compounds an
    *assumed* verdict into a one-way radio action — the same rule the relocation ladder
    already documents (`provisioning.go:1152-1158`). A failing `Online()` query or a
    failing link/wire probe defers; only confirmed verdicts arm anything.
12. **The two evidence shapes never share an episode** (§1.4). The setup-incomplete
    episode (§4.1) requires a **confirmed live Wi-Fi link** to arm and is **cancelled by
    confirmed link loss**, which hands the device to the untouched link-absent machinery
    (`sustained-offline`/`relocated`, now with the recheck cadence). Nothing in the
    link-present episode — including its settled state — ever suppresses a raise for a
    device whose link is confirmed gone. **Link evidence for the episode is sampled only
    during station phases**: while the episode's own AP is up, `probeLink` short-circuits
    to `linkAbsent` without running nmcli (`provisioning.go:1716-1725`), so a
    short-circuited probe is a fourth outcome — *not sampled* — and is evidence of
    nothing, for arming, cancelling, or the window. Without this rule the episode would
    read its own AP phase as link loss and self-cancel into the link-absent cadence on
    cycle one (§5 pins "an episode's own AP phase does not cancel the episode").
13. **On-screen time promises are floor-safe.** `bootOfflineDetail`'s rationale stands
    (`provisioning.go:926-948`: the wait is a floor, not a bound). Copy states a number
    only while the governing timer is running normally; while the window is paused
    (inconclusive probes) or the raise is deferred (hub contact, portal activity), the
    repaint switches to the floor-safe variant ("…if the connection does not return" /
    "…while the app is connected"). Numbers in copy are read from the same named
    constants the machine runs on.

---

## 3. Risks and unknowns

| Risk | Assessment |
|---|---|
| **Transient WAN outage during first-boot setup** (user joined the right network; ISP down 40 min) | Trace under §4.1 defaults: the 5-min window arms at t≈0 — a WAN transient shorter than 5 min (the routine modem re-sync) triggers **no raise at all**, exactly as today; if the user's phone touches the hub (likely — they are mid-setup) the raise defers; otherwise AP up t=5–10, station t=10–15, and so on up the ladder. An ISP recovery landing in a station phase is observed immediately; one landing in an AP phase is observed at the next teardown — worst added delay **≤ one AP phase (5 min)** against today's behavior; no dead end. Cycle 2 behaves the same because the AP phase's short-circuited probes are not samples (constraint 12) — the episode survives its own AP phases. |
| **Claimed frame, link and WAN both die** (router power cut mid-exhibition) | Trace under §4.2: `sustained-offline` raise at t≈5 as today; recheck cadence bounces the AP down every 30 min for a forced reactivation attempt (`nmcli connection up` after scan-readiness — not a passive autoconnect wait, §4.2 recheck mechanics). Router restored at any point → the next recheck reassociates → online → artwork back; worst-case blindness is one 30-min AP phase (today: forever, D11). A frame whose SSID is truly gone keeps its QR ≥90% of the time, blinking only for narrated rechecks. |
| **Oscillation confuses a user who walks up mid-cycle** | Every phase narrates itself (§4.6) with floor-safe or numeric copy per constraint 13. The link-present episode is bounded: after 4 raise cycles with no WAN and no claim (**≈60 min** from entry — the escalating station ladder front-loads the AP availability a nearby user needs), the machine settles in **station mode** with a terminal diagnosis — station is the terminal state because the LAN escape (§1.3) lives there. The link-absent recheck cadence is deliberately unbounded (the QR *is* the correct steady state for a frame whose network is gone), but every cycle re-tests the world. |
| **Claim arriving mid-episode** (LAN pairing while the window is armed) | The claim observer updates the loop-visible snapshot immediately; the window and any active episode cancel on the next loop event. Checked again at expiry before raising. |
| **Corrupt/unreadable state file mis-reads a claimed frame as unclaimed** | Constraint 8: unknown claim = claimed; no auto-raise. Costs the unclaimed escape on a device with a corrupt state file, which factory provisioning makes vanishingly rare. |
| **D8 (monitord dead) on an unclaimed frame with healthy WAN** | Constraint 11: query failure never arms the window, so a healthy association is never dropped on an observer fault. The permanently-dead-monitord case remains a narrated dead end for auto-escape — but the LAN pairing path (§1.3) still works, and §4.4 keeps the wording hedged ("checking…"). Accepted. |
| **Hub-contact deferral pinned by the user's own app checks** | The deferral draws from a budget of **5 min per cycle, 15 min per episode** (§4.1 — scaled to the shortened windows; a single deferral must not dwarf the station phase it delays): it can delay but never pin the fallback, and later cycles keep their own allowance so a user pairing mid-episode is not rug-pulled by an exhausted global budget. While deferred, the copy switches to the app-directed variant (constraint 13) instead of promising a countdown it cannot keep. The §4.7 status object tells the app the device is deferring, so the app can surface the pairing/`startWifiSetup` action instead of silently resetting the clock. |
| **Mid-portal rug-pull at a session bound** (phone on the hotspot, user typing) | Teardown defers while the captive portal has served a **human-caused** request in the last 2 min — the `/connect` and `/rescan` handlers only; `GET /` cannot be counted because `/` is the ServeMux catch-all and the OS captive-probe routes redirect idle phones' automatic probes to it (§4.2 has the full rule and names the new `portal.Config` callback and its synchronization) — with a +15 min ceiling. `applyRescan` re-entering `ensureAPUp` re-arms the session timer (an actively rescanning user is present — intended) under a **2 h absolute cap** per bounded session. |
| **Enterprise/unjoinable SSIDs leave D2's hard core open** (claimed frame on a network the phone cannot join: WPA2-EAP, per-device captive sessions) | **Accepted gap**, recorded in §4.8: no relayer, no LAN, no AP. The contact-gated claimed auto-raise is the named future option if field data shows this shape matters. |
| **`hidden yes` behavior across NM versions** | `nmcli … hidden yes` is stable API since NM 1.4; the target image pins NM. Verified on-device before release (§5, hardware-gated). |
| **Error-state narration flaps against `scanning`** | `ensureAPUp` pushes `scanning` before attempting the raise (`provisioning.go:1530-1533`), so a naïve error screen would alternate at 15 s cadence. §4.6 latches the error and suppresses the `scanning` push while latched. |
| **`setup_error` on a sleeping panel is invisible** | Same accepted gap as `app-triggered-wifi-setup.md` §6; the user can wake the panel. |
| **Health object leaks the venue SSID on unauthenticated `:1111`, including to hotspot clients during AP phases** | Accepted under the out-of-scope security decision, but recorded here rather than silently: the durable fix is the v2 controller-authentication profile. |
| **Cadence constants wrong in the field** | All §4.1/§4.2 durations, budgets, and the cycle cap ride the **existing on-device JSON config** (`config.Load` reads `constants.CONFIG_FILE` at startup — the same mechanism as `enableHub`), applied to `provisioning.Config` at wiring time; the setup-incomplete fallback additionally gets a config kill-switch (disabling it reverts unclaimed devices to narration-plus-LAN-pairing only). Reverting or retuning is therefore a config edit plus a daemon restart — no package rebuild — but there is **no hot reload**. **The edit itself is a hazard**: `config.Load` fails on any unmarshal error and `main` treats that as fatal under `Restart=always`, so a typo'd document would crash-loop the daemon and take down every recovery surface at once. The new knobs therefore live in their **own sub-object decoded permissively** (`json.RawMessage`: a syntactically valid but wrong-typed provisioning block logs and falls back to defaults without failing `Load`; a **syntax** error anywhere in the file still fails the whole parse and the daemon start — that is the existing loader behavior, unchanged — so the operator guidance is "validate the JSON before restarting". Existing keys keep the existing strict behavior), and they follow the existing integer-with-unit-in-the-name convention (`ApPhaseSeconds`, not a `time.Duration`, which JSON would read as nanoseconds). |

Unknowns called out rather than decided silently: neither `docs/architecture.md` nor
`docs/api-design.md` addresses AP session policy, teardown narration, or escalation; this
document establishes those rules and both docs get pointers in the same change (§6).
Whether a topic-less LAN claim leaves any downstream surface (OTA gate, claim QR hooks)
half-initialized when WAN later returns is a named stage-3 verification item (§5).
Whether any non-app LAN client routinely hits the counted hub endpoints on venue networks
is checked during stage 3 bench testing; the deferral cap bounds the damage either way.
**The NM activation questions are ANSWERED** (bench 2026-08-04, FF1-8EVTK3RE, NM 1.56.1,
station `wlp2s0`): explicit `nmcli connection up` on a profile whose SSID is absent
fails with "network could not be found" in **~28 s** (not the 90 s worst case — the
per-activation deadline has ~3× margin, and a 4-min blink fits 2–3 profile attempts);
after **three consecutive failed activations, a fourth explicit activation still runs
the full attempt** (journal shows `connection-activate` → `Activation: starting` →
failed, identical to attempt one), so explicit activation is not gated by any
autoconnect block and the forced-reactivation design genuinely sidesteps the backoff
question; NM **autoconnected back to the saved in-range profile in 6 s** after every
failed activation, validating both the `startWifiSetup` abandonment assumption and the
teardown→rejoin step of every cadence here. Same bench, hub-traffic baseline: the only
`:1111` client on an idle claimed frame is local `feral-vmagent` scraping `/metrics`
every 60 s over loopback — zero non-app LAN traffic — so the `/metrics` exclusion is
mandatory, not prudent, and the contact signal additionally ignores loopback sources
(§4.1). Still open for the stage-3 bench frame: the router-cold-boot recheck
observation and the captive-probe cadence check (both need AP mode).

---

## 4. Design

Six workstreams. A–D are `feral-controld`; E touches the player contract (ff-player); F
is the app surface (ff-app). Wire-surface changes are additive per `docs/api-design.md`;
the manifest change is extension-tier per the delivery-skew posture (header).

### 4.1 Escape policy for the link-present shape, unclaimed (fixes D1/D4/D8-as-dead-ends)

**Primary escape: stay reachable, get claimed over LAN.** Per §1.3, the narration for
every unclaimed live-link-no-WAN state points the user at the app on the same network,
where the standard pairing flow can claim the frame with no WAN. Once claimed,
`startWifiSetup` (§4.5) is the sanctioned way to change networks. This escape requires
dropping nothing and works during every station phase. It is also **zero-wait**: for a
claimed frame the app sends `startWifiSetup` directly; for an unclaimed frame the path
is pair-over-LAN first, then "Change Wi-Fi" — two taps, still immediate (the device
admission accepts `offline_retrying`/`unprovisioned` with no claim gate, but the app's
"Configure Wi-Fi" entry hangs off a paired device per the sibling plan §4.3, so pairing
is the app-side step, not a wait). Every wait defined below exists **only for the
app-less user**.

**Fallback: the setup-incomplete episode.** For the user who is not reachable by that
path (no app, phone cannot join the network), the machine auto-returns to setup.
Arming conditions (the first four — all on the loop goroutine, all confirmed evidence,
constraint 11) plus one deferral modifier (the fifth bullet, which pauses rather than
gates — see the window definition below):

- claim snapshot says unclaimed (constraint 8; unknown = claimed = never arm), and
- the Wi-Fi link is **confirmed present** (`probeLink == linkPresent`) — this episode
  exists only for the link-alive-but-useless shape; constraint 12 keeps it disjoint from
  the link-absent machinery, and
- WAN is **confirmed** offline (a failing `Online()` query defers), and
- `WiredLink` confirms no wire (constraint 6 semantics), and
- **no LAN control-plane contact** within the last **3 minutes** (one freshness constant,
  shared with the pause rule below; long enough that an app user deliberating over the
  pairing sheet does not lapse between probes, and still below the 5-min cycle
  allowance so a single stale check cannot consume a whole cycle's deferral; the portal
  window stays at its own 2 min — a portal user is actively clicking): any request to the hub's `cast`/`status`/`status_v2`
  endpoints marks contact (`/metrics`, **any loopback source**, the catch-all, and the
  long-lived `notification` WebSocket — whose persistence is not fresh evidence — are
  excluded; the loopback exclusion is load-bearing, not hygiene: `feral-vmagent`
  scrapes `/metrics` over 127.0.0.1 every 60 s on every device, and any future local
  poller of a counted endpoint would otherwise pin the deferral permanently). A phone with the
  app open defers the raise, because yanking the link out from under an app that can see
  the device is strictly worse than waiting.

**The episode window, defined in its own terms** (it must not borrow constraint 7's
rules — that window counts `linkAbsent` samples toward a raise on a *gone* link, the
opposite polarity; importing its reset-on-`linkPresent` rule here would make this window
unexpirable by construction):

- One sample per tick. A sample **counts** when the four evidence conditions
  (unclaimed ∧ confirmed link-present ∧ confirmed offline ∧ confirmed no-wire) are
  confirmed in it; **hub contact is a pause modifier, not part of the sample
  predicate** — while its budget lasts it pauses counting, and once the budget is spent
  samples count regardless of contact (otherwise "the count proceeds" below would
  contradict a contact-inclusive predicate). The window is **20 counted samples
  (≈5 min)** for every entry: five minutes is the shortest wait that still covers the
  home transient a raise cannot fix — a modem/router that just resynced has identical
  physics whether the frame arrived here via a portal join or a boot, so a shorter
  joined-entry window was considered and **rejected** (it would convert every routine
  2–5 min WAN re-sync during setup into a spurious QR raise; the responsiveness the
  present user needs comes from the short AP phase and station ladder below instead —
  see the rejected-alternatives entry in §4.8). On expiry the machine raises through
  the standard entry sequence (constraint 3) with `Reason: "setup-incomplete"`.
- An **inconclusive** sample (probe or query failure — constraint 11) pauses: nothing is
  counted, nothing is discarded, and after any pause ≥ 2 fresh counted samples are
  required before expiry can fire (no stale-evidence raise off a single sample).
- A **hub-contact** sample (human contact within the last **3 min** — the same freshness
  the arming condition uses) pauses the same way but draws from a deferral budget:
  **5 min per cycle, 15 min per episode, charged per tick actually paused** — a
  30-second app check pauses for its ~3-min freshness tail and charges only that, not a
  whole allowance. Within budget, contact defers; once the cycle's allowance or the
  episode's ceiling is exhausted, contact no longer pauses and the count proceeds.
  Per-cycle allowance guarantees later cycles keep some deferral (a user who starts
  pairing during cycle 3's station phase is not rug-pulled just because cycle 1 consumed
  the budget); the episode ceiling keeps the fallback from being pinned forever. While
  any pause is active, copy switches per constraint 13.
- A sample confirming **link loss** cancels the episode (constraint 12 — station-phase
  samples only; the AP phase's short-circuited probes are not samples). A sample
  confirming WAN or a wired link cancels it too, as does a claim event, and so does a
  **completed portal join** (`applyJoin` success is the strongest world-changed signal
  there is — a user who picked a wrong-but-live SSID first must get a full-length
  episode on the network they actually meant, not a shortened runway).

**Bounded episode.** The raised AP session is bounded at **5 min** (§4.2 — half the
earlier draft's 10: constraint 1's corollary requires AP phases to be the minority of
every cycle, and a 10-min AP against the shortened station ladder inverted that, taking
the frame off the LAN — where the primary escape lives — for most of the episode);
teardown re-parks the machine in station mode (constraint 4), NM autoconnect restores
the association, and the episode re-raises after an **escalating station phase — 5 min
after cycle 1, 10 min after cycle 2, 20 min after cycle 3** (the station phase *is* the
re-arm interval for cycles 2+ — one ladder, no second window; its samples follow the
same rules above, so hub contact during any cycle's station phase defers via that
cycle's allowance). The ladder shape is deliberate: early cycles favor the user who is
probably still nearby (missing an AP phase costs 5 min, not 20), later cycles favor
observing WAN recovery; AP is ≤33% of every cycle and of the episode. After **4
raises** with no WAN confirmation and no claim (**≈60 min** nominal from entry; up to
~15 min more if every deferral budget is fully drawn), the machine **settles in station
mode** — terminal narration (`setup-incomplete-settled`, §4.4/§4.6), health object live
(§4.7), LAN pairing still available. Episode cancellation, at any point including settled: WAN confirmation, a
claim, a wired-link sighting, or **confirmed link loss** — the last hands the device to
the untouched link-absent path (constraint 12), whose own cadence then owns recovery.

**Claimed devices are untouched by this section.** A claimed device in the link-present
shape keeps #233's behavior: no automatic raise, silent screen mid-life, escape via
`startWifiSetup` or router-side link loss. The residue is the accepted gap in §3/§4.8.

### 4.2 AP session policy (fixes D11, and closes §4.1's loop)

**The policy is latched from the raise reason at raise time** — not derived from
`hasProfile` at expiry, which is falsified by `wifictl.Join`'s pre-delete (a failed join
can leave zero profiles on a claimed frame) and biased wrong on nmcli errors:

| Raise reason | Evidence shape (§1.4) | Session policy |
|---|---|---|
| `unprovisioned` | no saved profile | unbounded, no recheck — with no saved profile there is nothing a station blink could autoconnect to, so a recheck can learn nothing; the portal's own rescan serves the "a network appeared" case. Today's out-of-box behavior. |
| `sustained-offline`, `relocated` | link confirmed absent | **AP-dominant recheck cadence**, both claim states: AP up **30 min**, then a narrated recheck (mechanics below) — then re-raise if still no association. Unbounded cycles: the QR is the correct steady state for a gone network, and every cycle re-tests the world, so a router that comes back is noticed within one cycle (D11's fix). |
| join-failure re-raise | a human just typed credentials — the network is demonstrably in range (an auth failure proves the SSID was found); NOT the link-absent shape | **Inherits the originating session's policy** and resets its clock. Mechanically: the policy is **latched in a machine field at raise time and retained across the join attempt** — `applyJoin` cancels the session *timer*, not the policy field, so the failure re-raise re-arms a fresh timer under the retained policy. Inside a `user-requested` or `setup-incomplete` session the session's own bound therefore continues (an attempted join is positive evidence of a present human — one typo must not escalate a bounded session into an unbounded cadence); with no bounded session (e.g. failure off an `unprovisioned` or recheck-cadence AP), today's behavior exactly: AP stays up, no recheck. **The re-raise never runs `resetJoinStatus`** — the phone re-associates and polls `/status` for precisely that outcome (`provisioning.go:1743-1751`); constraint 3's triple is amended with this documented exemption, which is today's code (`provisioning.go:1410` is a bare transition). |
| `setup-incomplete` | link was present, no WAN, unclaimed | **5 min**, then the §4.1 station ladder (AP must stay the minority of every cycle — constraint 1) |
| `user-requested` (`startWifiSetup`) | human present | **30 min** — always, including from `StateUnprovisioned` (the abandonment net the sibling plan exists for); on expiry, teardown and resume normal state handling |

**Recheck mechanics.** The blink is an **active** reactivation, not a passive wait — the
repo's own evidence says passive autoconnect after an AP→station flip is unreliable
(`provisioning.go:196-211`: the post-bounce BSS list is empty or stale;
`wifictl.go:565-570` and `waitForScanReady` exist for exactly this), and NM's autoconnect
backoff on a repeatedly-failed profile is an open bench question that must be answered
before stage 3 is scoped (§5). Sequence: teardown → a forced scan through the
`WifiController` interface (the scan calls already embed the radio-settling wait —
`waitForScanReady` is unexported and stays that way) → **activate only saved profiles
whose SSID the scan shows in range** (relocation is definitionally multi-profile, and a
blind `connection up` on an out-of-range profile blocks ~90 s, half the budget, so the
wrong pick would starve the right one; most-recently-used order among in-range
candidates; hidden-SSID profiles, invisible to scans, get one attempt last if budget
remains). This needs a **new wifictl read helper returning
`(uuid, ssid, hidden, timestamp)` per profile** — named as a new seam because no
existing function can serve it: `SavedWifiSSIDs` returns no UUIDs to activate, ORs
`hidden` into a single aggregate bool, and reads no `connection.timestamp` for MRU
ordering. It extends the same nmcli read as §4.8's stage-1 `(uuid, ssid, key-mgmt)`
helper (one read path, two field sets — stage 3 already depends on stage 1). Fail-bias:
a listing error during a blink **aborts the blink and re-raises** — never a blind
activation off an unreadable profile list. The
blink's scan result feeds the shared scan cache, and a failed recheck's re-raise skips
`ensureAPUp`'s own `RefreshScanCache` pass **only if the blink's scan returned without
error and non-empty** — an empty post-bounce scan is the documented common failure shape
the 3-attempt retry loop exists for (`provisioning.go:202-209`), so on error or empty
the re-raise runs its normal retry loop instead. One radio pass instead of two in the
good case, and a fresher portal picker. Each
activation runs under a **90 s context deadline**, and the whole blink has a **hard
ceiling of 4 min** — "extends while an activation is in progress" never exceeds that
ceiling, which is deliberately below the 5-min `OfflineWindow` so the blink cannot race
it; belt-and-braces, **every tick-driven AP raise is suppressed while a recheck-cadence
session is active** — the session's own re-raise is the only raise path out of a blink;
otherwise the tick's competing `sustained-offline` raise (`provisioning.go:1228-1233`;
the `unprovisioned` flavor at `:1290-1295` calls it too) would fire `resetJoinStatus`
and wipe the very status the recheck preserves. **"Active" ends explicitly** when the
blink's reactivation associates (the shape transition — online or link-present — is the
session's exit), when a wired link is sighted, or when a portal join completes; the
suppression cannot outlive any of those edges (§5 pin — the predecessor of this rule was
a suppression that outlived its justification, and that must not recur). Failure
handling: an activation error logs and the re-raise proceeds
(fail-open); a recheck whose `ensureAPDown` fails aborts the blink (the hotspot may
still hold the radio — a false "still gone" must not be recorded) and retries next
cycle, with persistent teardown failure escalated by `setup_error(ap_release_failed)`.
The recheck re-raise performs the `transition` only — no `clearOffline` (the raise
decision was already made) and no `resetJoinStatus` (preserves a join outcome a phone
may be polling). `applyRescan` is unreachable during a blink (it admits only
`StateAPActive`, `provisioning.go:1420-1424`), so the two do not interact. The
boot-scoped relocation ladder cannot re-arm across rechecks (`provisioning.go:1039-1041`,
`:1067-1076`: armed once at boot assessment, disarmed terminally) — asserted, not
assumed, in §5.

Mechanics, all loop-goroutine:

- Timer armed on `ensureAPUp` success, cancelled by `applyJoin`. `applyRescan` re-arming
  it is intended (an actively rescanning user is present); a **2 h absolute cap** from the
  first raise of the session backstops the bounded rows. On a recheck-cadence session a
  rescan resets only the current 30-min AP phase (the cadence is unbounded by design, so
  there is no cap to consume).
- **Portal-activity deferral:** a teardown (session bound and recheck alike) defers
  while the captive portal has served a **human-caused** request within the last 2 min.
  Only the two action handlers count — `POST /connect` and `GET /rescan` (the rescan
  form submits as GET, `portal/templates/index.html:189-191`; the classification is
  by handler, not method) — they are unambiguous human actions.
  `GET /` is deliberately **not** counted: in `net/http.ServeMux` the `/` pattern *is*
  the catch-all, so root fetches cannot be separated from stray paths by routing alone,
  and the OS captive-probe routes (`/generate_204`, `/hotspot-detect.html`,
  `/connecttest.txt`, `/ncsi.txt`, `portal.go:194-201`) exist precisely to redirect an
  idle phone's automatic probes **to** the root page — counting `/` would let an idle
  associated phone pin every teardown to its ceiling. Ceiling: +15 min. Seam: the portal
  sees every request but exposes none today — `portal.Config` gains a request-observed
  callback (invoked only from the two counted handlers), and the timestamp it feeds
  lives in the machine's `m.mu`-guarded block (portal handlers run on `net/http`
  goroutines; the reader is the loop goroutine — same treatment as §4.7's link cache).
- **Teardown landing (constraint 4):** tear down, then transition to
  `StateOfflineRetrying` with a reason the machine picks from its claim snapshot and the
  session's policy row: `ap-session-ended` (narrated — §4.1 station phases, unclaimed),
  `ap-recheck` (narrated — the recheck blink, both claim states), or
  `ap-session-ended-silent` (claimed §4.1-adjacent cases and `user-requested` expiry on a
  claimed frame: the notifier's explicit case for it hides the overlay, `narrating`-
  guarded, and artwork returns). The notifier stays a pure reason switch — no claim seam
  in the wiring layer.
- **Recheck outcome routing:** if the blink's reactivation associates, normal machinery
  takes over — an online event ends everything; associated-but-no-WAN lands in the
  link-present shape (§4.1 for unclaimed; for claimed, the machine emits
  `ap-session-ended-silent` on that edge per constraint 4(b), so the `ap-recheck` panel
  is hidden and artwork returns rather than "Setup mode will return in a moment"
  stranding as a permanent lie over the exhibition). If it does not associate, re-raise
  with the original reason; the recheck is not a user-visible "cycle" of anything, just a
  blink (see Recheck mechanics above for what the re-raise does and does not perform).
- **Wired exit from a raised AP:** extend the existing failed-raise probe exit
  (`provisioning.go:1300-1327`) to successfully raised APs when a **wired** link is
  sighted. The `probeLink` short-circuit exists for Wi-Fi's own-hotspot ambiguity
  (`provisioning.go:1705-1725`); an ethernet row in `nmcli device show` has no such
  ambiguity (constraint 6's survey rule). This also lowers a `user-requested` session
  when a cable is plugged in mid-setup — intended. Costs one nmcli read per tick for the
  AP session's duration; accepted.

D11 resolution, by flavor: router-reboots-during-outage and WAN-less-LAN-link-returns are
fixed by the recheck cadence (recovery within one 30-min cycle, then the `online` event
or the link-present shape takes over); the wired flavor exits via the wired sighting; the
genuinely-gone network keeps a near-permanent QR, which is the correct terminal UX for
it. `sustained-offline` raises stay effectively permanent for walk-up discoverability —
what changes is only that the frame is no longer blind between walk-ups.

### 4.3 Window and probe semantics (fixes D7, D10)

- **Inconclusive samples pause, bounded.** `linkUnknown` (and a `WiredLink` error) stops
  the sample count and resumes on the next confirmed sample. Two bounds keep the
  accumulated evidence honest — the hazard the current disarm-on-unknown exists for
  (`provisioning.go:1119-1131`) is stale evidence crossing the one-way raise:
  - a pause longer than one full window discards the accumulation (evidence older than
    one window-length of silence is stale);
  - after any pause, expiry requires ≥ 2 fresh consecutive confirmed-absent samples (a
    single absent sample after a long unknown run can never fire the raise).
  `linkPresent` still fully resets (constraint 7).
- **`clearOffline`'s dual duty is split deliberately.** Today it zeroes the window *and*
  disarms relocation as a single point of truth (`provisioning.go:1768-1778`). The split:
  window state gets pause/reset/discard as above; relocation keeps its own disarm with a
  new invariant — the ladder tolerates **2** interleaved `linkUnknown` samples, forfeits
  on the 3rd and on any `linkPresent`, and remains boot-scoped. The replacement
  single-truth rule: *any `linkPresent` sighting resets both mechanisms at once* (one
  helper, one call site class), so a stale armed relocation still cannot survive into a
  later offline episode.
- **Join timeout.** `wifi.Join` runs under a 120 s context deadline (nmcli's own `--wait`
  default is 90 s; the margin covers profile cleanup). Expiry surfaces as
  `JoinErrTimeout` — the existing failure path re-raises the AP with the existing copy.
  Removes the only unbounded call on the loop goroutine (D10).

### 4.4 Verdict resolution and narration repaint (fixes D4, D8-narration)

The current repaint machinery is probe-driven and boot-scoped (`bootNarration` marker +
`bootOfflineDetail`, a link-probe→prose table, `provisioning.go:922-966`). It cannot
express D4's flip, which is a *connectivity-verdict* change. The fix generalizes it:

- **Episode narration marker.** `bootNarration` generalizes to an episode marker set at
  every narrated `StateOfflineRetrying` entry and cleared on leaving the state — same
  lifecycle as today's marker, wider entry set. A device that never leaves the state
  keeps its marker and stays governed by the verdict table below (deliberate: that is
  the repaint path, not a leak — and the generalized field's code comment must say so,
  because `bootNarration`'s current doc reasons about one-boot staleness and a future
  reader could otherwise restore the narrower scoping as a "fix").
- **Verdict→prose table.** One table keyed on (link probe outcome × connectivity verdict
  × claim state × pause/deferral status — constraint 13 picks numeric vs floor-safe
  variants), replacing `bootOfflineDetail` as the single probe/verdict→copy mapping.
  The existing tick re-query (`provisioning.go:1084-1092` — no new probe) consults it: a
  `joined-conn-unknown` episode whose re-query confirms offline repaints to the
  `joined-no-internet` wording; one that confirms online exits to `StateOnline` as today.
  The `onConnectivity` provisioned-offline tail (`provisioning.go:900-919`) checks the
  episode marker and emits the episode's specialized reason instead of the generic
  `"offline"` — this is the piece that makes the repaint reachable at all (the generic
  reason is deliberately outside the narrated set and stays there).
- **Narrated reason set** (the whitelist for the dedupe extension, constraint 10):
  `boot-offline`, `boot-no-internet`, `boot-link-unknown`, `joined-no-internet`,
  `joined-conn-unknown`, `ap-session-ended`, `ap-recheck`, `setup-incomplete-settled`.
  `ap-session-ended-silent` is handled by its own explicit notifier case (guarded hide,
  constraint 4), not by the narration path. Transitions into `offline` / `link-present`
  keep today's dedupe — exhibition-silence regression pin in §5.

### 4.5 User-intent channel

`docs/app-triggered-wifi-setup.md` is adopted with three amendments (per the repository's
amendment duty, the sibling doc is updated in the same change):

1. Its bespoke 30-minute timeout is **subsumed** by §4.2's session mechanism (same bound,
   plus the portal-activity deferral and the 2 h cap it did not have).
2. Its app-flow copy names the up-but-offline scenario explicitly: the phone must join
   **the same network the frame is on** (the network exists; it merely has no internet),
   at which point LAN discovery, claiming, and the command all work. This is the plan's
   highest-value path (D2's main escape and §4.1's primary escape both route through it).
3. Its `WiredLink` seam gets the survey semantics pinned in constraint 6, and its test
   list gains the no-ethernet-row and corrupt-output cases.

**Cross-plan ordering:** the sibling plan's stage 1 (the `WiredLink` seam) is a
prerequisite for this plan's stage 3 — declared in §6.

### 4.6 Narration: bounded failure states and actionable copy (fixes D5, D6, C-class gaps)

**Contract extension** (extension tier, constraint 9): one new `setupDisplay` state
`setup_error`, `stateFields: { setup_error: { optional: ["reason"] } }` — `reason` carries
the prose, matching the `connecting` convention (`setupui.go:279`); no new `message`
field. Send-time downgrade to `join_failed` on a manifest that provably lacks it, via the
same `resolveConnectingState` mechanism (generalized to a small state→fallback table
rather than a second bespoke copy). Note the table's value type is a union: most entries
map to a fallback *state* (`connecting`→`join_failed`, `setup_error`→`join_failed`), but
the `ap-recheck` push maps to a fallback *operation* (`Hide()`, per the delivery-skew
posture in the header) — the table shape must admit both. The required set is untouched. ff-player renders
`setup_error` (reason → title/body) in its own release; until the bundle lands, the
downgrade shows the copy under the `join_failed` title — degraded, never dark.

**Escalation counters** (loop-goroutine state):

- `ensureAPUp` failure streak ≥ **8** (~2 min at tick cadence — above transient NM
  restarts, an order of magnitude below "user walks away"): latch
  `setup_error(ap_start_failed)`. **While latched, `ensureAPUp` suppresses its `scanning`
  push** (`provisioning.go:1530-1533`) so the error does not flap at 15 s cadence; the
  latch clears on the first successful `ap.Up`, whose own `softap_qr` narration then
  repaints. Retries continue underneath throughout.
- `ensureAPDown`/`apDownPending` failure streak ≥ 8: latch
  `setup_error(ap_release_failed)`. (This path has no `scanning` push to suppress —
  `ensureAPUp` returns before it while a teardown is pending.)

**Copy formula** for every trouble-state message: *what happened* + *what the device does
next* (a number only when the governing timer is live — constraint 13) + *what the user
can do now* + *device identity*. Representative strings (final copy is a product pass;
these pin the required content; all numbers read the §4.1/§4.2 constants):

- `joined-no-internet`, unclaimed, timer live: *"Connected to "X", but this network has
  no internet access. To finish setup, connect your phone to "X" and open the Feral File
  app — or wait about 5 minutes and the frame will reopen setup mode. (FF1-XXXX)"*
  (the number reads the episode-window constant)
- Same, while deferred/paused: *"…To finish setup, connect your phone to "X" and open the
  Feral File app. Setup mode will reopen if the connection does not return. (FF1-XXXX)"*
- `boot-no-internet`, unclaimed: same shape as the two above. Claimed flavor: unchanged
  wording plus identity suffix.
- `ap-session-ended` (station phase, unclaimed): *"Retrying "X" — setup mode will reopen
  in about 5 minutes if this network still has no internet. Or connect your phone to "X"
  and open the Feral File app. (FF1-XXXX)"* (the number reads the current station-ladder
  rung — 5, 10, or 20 minutes)
- `ap-recheck` (both claim states): *"Checking for your Wi-Fi network… Setup mode will
  return in a moment. (FF1-XXXX)"*
- `setup-incomplete-settled`: *"This network has no internet access. Connect your phone
  to "X" and open the Feral File app to finish setup, or restart the frame to try again.
  (FF1-XXXX)"*
- `setup_error(ap_start_failed)`: *"The frame could not start setup mode. It will keep
  trying automatically. If this persists, disconnect power for ten seconds and restart.
  (FF1-XXXX)"* — non-destructive expectation first.
- `setup_error(ap_release_failed)`: same shape for the hotspot-release failure.

The portal result page drops its impossible "reconnect to FF1-xxxx and reload"
instruction for *"Watch the frame's screen — it will show its progress. You can close
this page."*

The unclaimed wired no-WAN black screen is fixed **in the executor's claim flow** (it
owns the topic-wait): on topic-wait expiry with the device unclaimed and not online, it
paints `connecting` **conditionally** — a `ShowConnectingIfShowing(StateFinalizing, msg)`
sibling of `HideIfShowing`, routed through the same `pushIf` critical section — because
the call site's own comment documents the race an unconditional paint loses (a link drop
during the 60 s wait can raise the AP and paint `softap_qr`, which must not be
overwritten; `executor.go:932-941`). Copy: *"Connected by cable, but there is no internet
access. Setup will continue when the connection is restored. (FF1-XXXX)"*. The overlay is
cleared by the claim flow's own later narrations or any provisioning transition — the
same ownership it has today.

### 4.7 Unified network health surface (app parity)

Additive `network` object on hub `/api/status`, `/api/v2/status`, and relayer
`getDeviceStatus`:

```json
"network": {
  "state": "offline_retrying",
  "reason": "joined-no-internet",
  "ssid": "Studio WiFi",
  "link": "wifi" | "ethernet" | "none" | "unknown",
  "internet": false
}
```

Sourcing, with no new probes on the polling path: the machine adds a `Snapshot()`
accessor reading the `m.mu`-guarded block (`provisioning.go:322-325` — the block already
exists for external readers; the new cached fields live there, not with the loop-only
counters, so `go test -race` stays clean). The link outcome+type cache is written **only
by real probes** — the `apUp` short-circuit (`provisioning.go:1720-1725`) does not touch
it, so a stale absent-with-no-type can not persist past an AP phase. `internet` comes
from the monitord cache exactly as `internetProbeFrom` does today. The app renders the
same diagnosis the screen shows and surfaces "Change Wi-Fi" whenever the device is
LAN-reachable (gate per `app-triggered-wifi-setup.md` §4.3); a `deferred` sub-state lets
the app know its own presence is holding the AP down (§3). During AP phases the frame is
off the LAN and this surface is dark — one more reason the link-present cadence keeps AP
phases short.

### 4.8 Join-path correctness (fixes D3, D9, open networks, whitespace)

- **Hidden SSIDs.** The portal always renders the manual-entry field (picker *plus* text
  input, not either/or) with a "hidden network" checkbox. `RequestJoin` carries
  `hidden bool` and `manual bool`; `wifictl.Join(ctx, ssid, psk, hidden)` appends
  `hidden yes` and skips `waitForSSID` (a hidden SSID never appears in scans).
- **Profile deletion by target SSID, scoped to PSK/open profiles.** A new wifictl helper
  (same nmcli read technique as `SavedWifiSSIDs`, new function — that one returns no
  UUIDs) lists `(uuid, ssid, key-mgmt)` per Wi-Fi profile; `deleteWifiProfiles` deletes
  profiles whose **target SSID** matches *and* whose key-mgmt is PSK/none. Enterprise
  (802.1X) and other non-PSK profiles are never deleted — a portal PSK join must not
  destroy an EAP profile for the same SSID.
- **Open networks.** An empty PSK omits the `password` argument entirely instead of
  sending an empty WPA-PSK.
- **Whitespace.** Trimmed only on the **manual-entry** branch (the `manual` flag above);
  picker values pass through verbatim. Blanket trimming at `RequestJoin` is wrong twice:
  it cannot distinguish the branches today, and leading/trailing SSID bytes are valid and
  deliberately preserved elsewhere (`wifictl.go:194-200`).

**Rejected alternatives and accepted gaps.**

- *Automatic escape for claimed devices in the link-present shape* — including the
  contact-gated variant (raise only after N hours with zero LAN control-plane contact).
  Rejected for this iteration: #233's blast radius on exhibitions outweighs the residue,
  which is narrow (claimed frame on a network the phone cannot join — WPA2-EAP,
  per-device captive sessions). That residue is **accepted and recorded**; the
  contact-gated variant is the named future option if field data shows the shape matters.
- *A cycle cap / settle state for the link-absent recheck cadence.* Rejected: settling in
  station on a gone network is a dark screen with no QR — a discoverability regression on
  the moved-frame scenario. The recheck cadence is unbounded by design and §3 records it.
- *A shorter (2-min) presence-keyed window for joined-* entries.* Rejected: a modem or
  router mid-resync has identical physics whichever way the frame arrived at
  "associated, no WAN", so a 2-min joined window would convert every routine 2–5 min
  WAN transient during setup into a spurious QR raise that re-entering the same correct
  credentials cannot fix. The present user's responsiveness comes from the 5-min AP
  phase and the 5/10/20 station ladder instead; the uniform 5-min window keeps
  sub-window transients invisible, exactly as today.
- *A station-association probe to defer teardowns.* Rejected: **both** candidate signals
  need a new seam (`softap.Backend` is Up/Down/Status only; the portal exposes no
  request hook today either), but the portal callback is in-process, needs no radio
  command with its own timeout/error-bias contract, and — once scoped to the two
  human-caused action handlers (`/connect` and `/rescan`, §4.2) — carries strictly
  better signal: a station-association
  probe cannot tell a phone in a pocket from a human mid-join, which is the same
  idle-phone false positive the portal's OS-probe routes had to be excluded for.
- *Persistent on-screen badge for claimed mid-life outages.* Violates constraint 2's
  exhibition rationale; the app-side health surface (§4.7) carries the diagnosis instead.
- *Growing the manifest required set / deleting the downgrade machinery.* Rejected — see
  the delivery-skew posture in the header.
- *In-app portal completion.* Already rejected in `app-triggered-wifi-setup.md` §4.5.
- *Physical escape (power-cycle counter) and LAN auth.* Out of scope by decision;
  recorded so the next session does not re-derive them.

---

## 5. Test and verification plan

**`components/feral-controld`** (table-driven, existing fakes; per-module gates:
`gofmt -s -w`, `go vet ./...`, `go test ./...`,
`golangci-lint run --new-from-rev=HEAD~1 ./...`):

- *Setup-incomplete episode:* arms only on unclaimed ∧ link-present ∧ confirmed-offline ∧
  not-wired, with hub contact acting as a budgeted pause modifier rather than a
  predicate term; a failing `Online()`/`WiredLink`/link probe never
  arms (constraint 11 pin); each cancel edge (WAN confirm, claim via observer, wired
  sighting, **confirmed station-phase link loss — including from the settled state**);
  **an episode's own AP phase never cancels it** (the `probeLink` short-circuit is
  not-sampled, constraint 12 pin — the fake must model the `apUp` coupling for this test
  to mean anything); window expiry counts 20 full-predicate samples; inconclusive pauses
  require ≥2 fresh counted samples before expiry; hub contact defers within the 5-min
  cycle allowance and 15-min episode ceiling, **charged per paused tick** (a 30 s
  contact does not burn a full allowance), and the raise proceeds once either is
  exhausted; a completed portal join cancels the episode and resets the cycle counter;
  claimed and claim-unknown devices never arm (#233 + corrupt-state pins).
- *Episode cadence:* first raise at the 5-min window (a sub-window WAN transient
  triggers no raise — the modem-resync pin); 5-min AP phases (AP ≤33% of every cycle —
  the constraint-1 corollary pin); subsequent raises follow the 5/10/20 station ladder
  after each teardown (one ladder — no separate second window); 4 raises then settle
  with
  `setup-incomplete-settled` narration; WAN/claim reset the counter; settle → confirmed
  link loss → the `sustained-offline` path raises normally (the D12 pin — suppression
  never outlives the link).
- *Session policy:* latched by raise reason per the §4.2 table — including
  `user-requested` from `StateUnprovisioned` (bounded); **a join failure inside a
  `user-requested` or `setup-incomplete` session keeps and resets that session's own
  bound** (never escalates to the recheck cadence — the one-typo pin) **and preserves the
  join status for the phone's `/status` poll** (no `resetJoinStatus` on the re-raise,
  `provisioning.go:1743-1751` pin); a session-less join failure keeps today's behavior
  (AP stays up, no recheck); `applyJoin` cancels; portal-activity deferral defers on
  human-caused routes only (an OS captive-probe request does **not** defer) and its
  +15 min ceiling fires; `applyRescan` re-arms under the 2 h cap (recheck sessions: AP
  phase only).
- *Recheck bounce:* teardown → `waitForScanReady` → scan → activation of **in-range
  profiles only**, MRU order, hidden profiles last (multi-profile pin: with profiles A
  out of range and B in range, B is attempted and A never consumes the budget); each
  activation under a 90 s deadline; **the blink's total ceiling is 4 min and a blink
  that runs it does not produce a competing `sustained-offline` raise** (the
  suppression pin — the fake clock drives the full window past 5 min); a hanging
  activation surfaces at its deadline (mirror of the `JoinErrTimeout` row); a failed
  `ensureAPDown` aborts the blink without recording a false "still gone";
  non-association re-raises with the original reason, no `offlineSince` churn, no
  `resetJoinStatus`; the boot-scoped relocation ladder stays disarmed across rechecks;
  on a manifest provably lacking `connecting`, the `ap-recheck` push downgrades to
  `Hide()` — never to `join_failed`, and never a skipped push that strands the stale QR
  (hide-blink pin); **the tick-raise suppression ends at each named session exit**
  (association, wired sighting, completed join — a suppression outliving its session is
  the regression class this rule replaced).
- *Claimed link-absent trace:* `sustained-offline` raise → 30-min AP → narrated
  `ap-recheck` blink → re-raise; a link restored during the blink exits the cycle
  (online → artwork; associated-no-WAN → `ap-session-ended-silent` hides the recheck
  panel, then the silent link-present shape).
- *Teardown invariant (constraint 4, both halves):* every teardown path repaints —
  `ap-session-ended` and `ap-recheck` narrate, `ap-session-ended-silent` hides via its
  explicit `narrating`-guarded notifier case; **after a teardown with no raise in
  progress, no test path may leave `softap_qr`/`scanning` as the last narration, and no
  test path may enter a silent reason while the last narration is any narrated
  `StateOfflineRetrying` panel** (both asserted generically over the machine's
  notification log, with the raise-in-progress carve-out matching constraint 4(a)'s
  scoping — `scanning` during an in-progress raise is correct and must not trip the
  assertion).
- *Wired exit:* `WiredLink` sighting lowers a raised AP (including `user-requested`);
  Wi-Fi `linkPresent` still does not (hotspot ambiguity pin). `WiredLink` semantics:
  ethernet-row-present ⇒ verdict from ethernet rows; wifi-only survey ⇒ `false, nil`;
  corrupt/empty output ⇒ error (the `surveyed` regression pin).
- *Window semantics:* unknown pauses and resumes; pause > one window discards
  accumulation; ≥ 2 fresh absent samples required post-pause; `linkPresent` resets window
  and relocation together (single-truth pin); relocation tolerates 2 unknowns, forfeits
  on the 3rd and on any `linkPresent`.
- *Join timeout:* a hanging fake Join surfaces as `JoinErrTimeout` and re-raises.
- *Verdict repaint:* `joined-conn-unknown` episode + re-query-confirms-offline repaints
  to `joined-no-internet` wording; re-query-confirms-online exits to `StateOnline`; the
  `onConnectivity` tail emits the episode reason only while the marker is set; paused/
  deferred episodes repaint to the floor-safe copy variant (constraint 13 pin);
  transitions into `offline`/`link-present` stay deduped (exhibition-silence pin).
- *Escalation:* 8th consecutive `ensureAPUp` failure latches `setup_error`; **while
  latched no `scanning` push occurs** (flap pin); first `ap.Up` success clears the latch
  and repaints via `softap_qr`; same for `apDownPending`.
- *Contract:* `setup_error` validates as extension state (absent from required set);
  send-time downgrade to `join_failed` on a manifest lacking it; manifest fixture
  updated; downgrade table covers `connecting` and `setup_error` through one mechanism.
- *Executor narration:* `ShowConnectingIfShowing(StateFinalizing)` paints only over
  `finalizing` (a concurrently painted `softap_qr` survives — the race pin from
  `executor.go:932-941`'s comment).
- *wifictl:* `hidden yes` present iff requested; `waitForSSID` skipped for hidden; empty
  PSK omits `password`; SSID-matched deletion hits name≠SSID PSK profiles and **never**
  deletes an 802.1X profile for the same SSID; manual-entry trim only (picker verbatim).
- *Status surface:* `network` object on all three replies; values from the `m.mu`-guarded
  snapshot + monitord cache; the link cache is untouched by the `apUp` short-circuit
  (stale-type pin); a poll performs no nmcli/probe work (fake-clock pin); `-race` clean.
- *LAN claim end-to-end (stage 3 verification):* a topic-less LAN `connect` on a no-WAN
  device flips every claim-derived surface (mDNS TXT, machine snapshot, escape policy)
  and acquires the relayer topic cleanly when WAN later returns.
- *Config:* every §4.1/§4.2 duration, budget, and the cycle cap is readable from the
  on-device JSON config (`config.Load`), defaulting to the values in this document when
  absent; the setup-incomplete kill-switch reverts unclaimed devices to
  narration-plus-LAN-pairing with no other behavior change; **a malformed provisioning
  sub-object logs and falls back to defaults without failing `Load`** (the
  `json.RawMessage` mechanism in §3 — the loader's strict behavior for existing keys is
  untouched, and a test pins that a malformed sub-object does not fail the daemon
  start); knob names follow the integer-with-unit-in-the-name convention.

**`ff-player`:** renders `setup_error` (reason → title/body); snapshot tests for the new
copy; no manifest required-set change to coordinate.

**`ff-app`:** health-surface rendering states (§4.7, including the `deferred` sub-state);
"Change Wi-Fi" gate and flow tests per `app-triggered-wifi-setup.md` §5; pairing flow
reachable for an unclaimed no-WAN device on LAN.

**Hardware-gated (named, not skipped silently):** `hidden yes` join against a real hidden
AP; open-network join; one full link-present episode (raise → station → settle → link
loss → `sustained-offline` raise) observed on a bench frame behind a dead-WAN router;
**the recheck validated against the scenario it exists for — router powered off during an
AP phase, powered back on, and the next recheck's forced reactivation observed to
reassociate** (a dead-WAN-but-present router validates nothing: autoconnect succeeds
trivially there); ~~the NM autoconnect-backoff measurement~~ **done 2026-08-04** — §3
records the results (28 s not-found failure, explicit activation unblocked after
repeated failures, 6 s autoconnect recovery); portal-activity deferral with a real
phone mid-join, and an idle
associated phone observed **not** to defer; LAN pairing of an unclaimed frame on a
WAN-less network; venue-network check that no non-app client hits the counted hub
endpoints; **time from power restore to confirmed WAN on the bench router/modem** —
the measurement the 5-min episode window's coverage claim actually rests on (the
2026-08-04 bench measured association recovery, not WAN readiness).

**Whole-repo:** `make verify`. No `users/**` changes → package rail;
`check-release-rail.sh` is not tripped and no `RELEASES.md` full-image entry is required.
The ff-player release is independent by construction (extension tier); its only coupling
is that the new copy renders under the `join_failed` title until the bundle lands.

---

## 6. Staged rollout

1. **Join-path correctness + loop hygiene** (§4.8, §4.3): hidden SSID, scoped
   SSID-matched deletion, open networks, manual-entry trim, join timeout, window
   pause-not-reset with its bounds, relocation tolerance, `clearOffline` split. Inert,
   self-contained controld + portal-template changes. Package rail.
2. **Failure visibility** (§4.6): `setup_error` extension state + downgrade table,
   escalation latches, portal result-page copy, executor `ShowConnectingIfShowing`.
   Controld ships alone; ff-player renders the new state in its own release.
   Rollback-safe in both directions by the extension-tier construction.
3. **Escape policy** (§4.1, §4.2, §4.4): claim snapshot seam, hub-contact seam with its
   budgets, setup-incomplete episode + settle, session policy table + recheck cadence
   (forced reactivation) + teardown invariant (both halves), portal request-observed
   callback + deferral, episode narration marker + verdict table, wired exit, copy
   rollout, JSON-config knobs + kill-switch. The NM-backoff bench measurement is
   **answered** (§3, 2026-08-04) — this stage is clear to scope; the router-cold-boot
   recheck observation remains a stage-3 exit criterion, not an entry one.
   **Prerequisite:** the `WiredLink` seam from `app-triggered-wifi-setup.md` stage 1,
   with the survey semantics pinned in constraint 6.
4. **`startWifiSetup`** per its own staged plan, with the three §4.5 amendments (its
   session uses stage 3's timer).
5. **Health surface + app** (§4.7): status `network` object (controld), then app
   rendering and the "Change Wi-Fi" entry (ff-app).

**Documentation updated in the same changes:** `docs/api-design.md` (`setup_error`
extension state and the downgrade convention, `network` status object, `startWifiSetup`
cross-reference), `docs/app-triggered-wifi-setup.md` (the three §4.5 amendments),
`docs/setup-flow.md` and its state diagram (`setup-incomplete`, `ap-session-ended`,
`ap-recheck`, `setup-incomplete-settled` edges; session policy table),
`docs/architecture.md` (AP session policy pointer), `docs/player-session-recovery.md` §9
(extension-tier precedent), `ffos/docs/DEVICE_LIFECYCLE.md`, and the component
`AGENTS.md` files whose hazard lists mention the changed invariants.
