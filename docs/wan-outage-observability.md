# Execution plan: WAN-outage observability (flight recorder, stages 0–2)

Status: proposed 2026-08-12. Companion to feral-file/feral-file#3495 (moskovich's
"disconnect telemetry first" recommendation); this is the controld/monitord half.
Scope is deliberately capped at stage 2 — no LAN-served diagnostics surface. Any
LAN download path waits for hub authentication (feral-file#3471) and is out of
scope here.

Goal: a fielded device on a hostile customer network keeps dropping off the
backend, and nothing anywhere records why. After this work, every outage leaves
a classified, uploaded artifact, and the backend can tell "venue network" from
"Cloudflare/relayer" from "device" without a site visit.

---

## 1. Current-state summary

Verified in code 2026-08-12 (branch `fix/update-command-fire-and-forget`, which
includes the merged recovery-UX work).

**What "online" means today.** feral-sys-monitord owns internet reachability: a
raw TCP dial to `8.8.8.8:443` / `8.8.4.4:443`, 5 s timeout, first success wins
(`components/feral-sys-monitord/connectivity.go:23-26,260`). Adaptive cadence
30 s online / 3 s offline. Published as one edge-triggered D-Bus bool
(`connectivity_change`), self-healed by the ~2 s `sysmetrics` level signal.
controld separately polls *link* state via `nmcli` (`status/linkcheck.go`) and
keys nearly all recovery decisions on link, not internet.

**What the backend sees.** Relayer presence — a single long-lived WSS
(`components/feral-controld/relayer/relayer.go`) — plus the `device_status`
change-feed (5 s poll, MD5-deduped, `status/status.go`) carrying the cached
`network` object `{state, reason, ssid, link, internet, deferred}` from the
recovery-UX §4.7 work. All of it point-in-time; none of it history.

**What survives an outage.** Only vmagent's 256 MB disk spool
(`users/feralfile/scripts/run-vmagent.sh:83-84`), which queues remote-write
samples offline and flushes on reconnect — but no connectivity gauge is
exported anywhere. monitord's Prometheus registry has exactly two gauges (CPU
temp, uptime; `metric/metric.go:23-36`); controld's `:1111/metrics` has three
playback counters. So the one buffer that spans outages carries no network data.

**What persists on the device.** Nothing structured. No network state file of
any kind; journald is on tmpfs; the de-facto timeline is Info log lines in
`/home/feralfile/.logs/{controld,sys-monitord}.log`, rotated copy-and-truncate
daily with 7-day retention and **no size cap**
(`users/feralfile/scripts/log-rotation.sh`) — a flapping device grows
`controld.log` unbounded until midnight. `/home` lives in the root btrfs
subvolume and is discarded wholesale on OTA promotion; only `/var/log` (`@log`
subvolume) survives OTA.

**Failure taxonomy today.** One bucket. Captive portal, DNS failure, DHCP loss,
dead gateway, upstream WAN drop, and a network that blocks Google all read as
"offline". No NetworkManager reason codes are captured anywhere (all NM state is
polled via nmcli; no signals watched). Relayer close reasons are logged but not
correlated with reachability, so a middlebox killing the WSS is
indistinguishable from WAN loss.

**Existing egress paths to build on.** `uploadLogs`
(`devicectl/executor.go:2979`, `devicectl/loguploader.go`): fire-and-forget,
single-flighted, walks `/home/feralfile/.logs` recursively plus two `/var/log`
updater logs, 128 MB input cap, ZIP to `support-logs.feralfile.com` + presigned
S3 PUT. And the `device_status` notification feed over the relayer.

## 2. Constraints and invariants

- **Two-rail release guardrail.** Everything here must ship on the package rail.
  Therefore no edits under `users/**` in the same change as component behavior —
  including `log-rotation.sh`, vmagent scrape config, and any systemd
  `LogsDirectory=`/tmpfiles provisioning. Anything that genuinely needs those
  becomes a separate full-image declaration per `RELEASES.md`.
- **monitord must not grow recovery policy** (its `AGENTS.md`). It may grow
  *measurement* (a gauge for a verdict it already computes). The diagnosis
  ladder is measurement too, but it needs controld's nmcli/wifictl plumbing, so
  it lives in controld.
- **Status/metrics paths never do live probe work.** The §4.7 snapshot rule
  (`Machine.Snapshot()` serves mu-guarded caches written only by real probes)
  extends to every new gauge and status field: scrapes and polls read caches.
- **Hub-contact observer exclusions stand.** `/metrics` and the WS are excluded
  from the SoftAP-raise deferral (`hub/hub.go:45-57`); new gauges change no
  routes. **No new `:1111` routes at all** before #3471.
- **Edge + level connectivity pattern.** Any new consumer of
  `connectivity_change` must also reconcile off a level signal (the
  `mediator.go:597-623` pattern) or it will stick wrong after a missed edge.
- **`uploadLogs` caps.** 128 MB input, oversized files skipped. The recorder's
  disk cap must be far below (target ≤ 8 MB) so it never evicts other logs from
  the bundle.
- **Probe discipline.** The ladder runs only on transitions and on-demand, never
  on a timer while healthy, and is rate-limited during flapping (min interval
  between full ladder runs, e.g. 60 s). A diagnostic tool must not become load
  on an already-sick network — nor a publish-cost driver later (#3495's cost
  analysis makes chattiness a billable property).
- **Recovery machine untouched.** The recorder observes the provisioning
  machine and monitord verdicts; it must not feed back into raise/join
  decisions. Observability change, zero behavior change.
- **API additivity** (`docs/api-design.md`): new `device_status` fields are
  additive and optional; the new command follows the
  `controld-inbound-controller-messages.md` envelope and gets documented there
  in the same change.
- **Timestamps.** Devices keep time by unauthenticated SNTP and boot with a
  wrong clock until sync. Recorder entries carry both wall clock and
  `CLOCK_MONOTONIC` (plus boot ID), so an outage spanning a clock step is still
  orderable.

## 3. Risks and unknowns

- **Probe-target false negatives (live suspicion for the failing site).** The
  Google-DNS dial misreads Google-blocking networks as offline, which would make
  today's "continuously disconnected" report partly an artifact. Stage 1's
  ladder measures backend + neutral host + captive-portal check, which resolves
  this; until then, stage 0's timeline may inherit the bias. Mitigation: export
  the *relayer* gauge alongside, so "probe says offline but relayer connected"
  becomes visible immediately — that contradiction alone diagnoses probe bias.
- **`analyticsToggle` opt-out** stops vmagent scraping. An opted-out device
  produces no stage-0 timeline. The stage-1 recorder and stage-2 upload paths
  are unaffected; note it in support runbooks rather than engineering around it.
- **Log-rotation interaction.** `log-rotation.sh` rotates by filename glob under
  `~/.logs`. The recorder must not be eligible for copy-truncate (it manages its
  own ring) — verify the script's matching rule against the chosen filename
  before shipping; if the glob would catch it, pick a name/subdir it ignores.
  (The script itself ships on the other rail and is not edited here.)
- **/home is lost on OTA.** Design branch B below. Accepted for the first
  release: outages under diagnosis rarely coincide with OTA promotion, and
  stage 2 uploads segments promptly anyway.
- **Wi-Fi telemetry availability.** RSSI/BSSID/bitrate come from
  `nmcli -f ... dev wifi` / `device show`; field coverage on the FF1 radio in
  AP-mode corner states is partially known (AP-mode scan returns EOPNOTSUPP —
  see AP-recheck notes). Gauges must tolerate "unknown" without fabricating 0.
- **DHCP lease events.** NM doesn't expose lease-renewal history via nmcli; we
  get current lease params only. Full renewal tracking would need NM D-Bus
  subscription (branch D). First release records lease snapshot + changes
  between snapshots, which is enough to catch churn.
- **Sentry noise.** The recorder must not route through `logger.Error` (every
  Error becomes a Sentry event). Classified outages are data, not errors.
- **Unknown: `/var/log` writability for the `feralfile` user.** If we later
  want the ring OTA-durable, provisioning a writable dir is a full-image
  change. Explicitly deferred.

## 4. Design branches

**A. Where the recorder lives.**
1. *New package in controld* (`components/feral-controld/netlog/`) —
   **recommended.** controld already owns nmcli, wifictl, the provisioning
   machine, the relayer, and uploadLogs; the recorder is a consumer of all
   five. No new daemon, no new D-Bus contract.
2. In monitord — keeps measurement together, but needs nmcli plumbing monitord
   doesn't have and a new D-Bus surface for controld to upload from. Rejected.
3. New daemon — maximal separation, maximal cost. Rejected (delete-before-add).

**B. Where the ring lives on disk.**
1. `/home/feralfile/.logs/netlog/` — **recommended for the first release.**
   Package-rail only, and `uploadLogs` picks it up with **zero code change**
   (it already walks `~/.logs` recursively). Cost: lost on OTA promotion,
   7-day-old segments pruned by our own ring anyway.
2. `/var/log/feral/` (`@log`, OTA-durable) — strictly better durability, but
   needs directory provisioning for the `feralfile` user → full-image rail.
   Deferred; revisit only if OTA-coincident outages actually bite.

**C. Ladder ownership of the "internet" verdict.**
1. Ladder in controld, monitord untouched except gauges — **recommended.**
   monitord's TCP-dial verdict remains the fleet-wide cheap signal; the ladder
   is the expensive, on-transition differential diagnosis. Two probes with two
   jobs, documented as such.
2. Move/extend the probe in monitord (add backend + 204 checks there). Better
   long-term home for "what does offline mean", but touches the
   generation-guarded watcher (`connectivity.go`), the most fragile file in
   monitord, for no stage-2 payoff. Deferred as a follow-up.

**D. How NM state changes are observed.**
1. Poll-based transitions (existing 15 s tick + monitord edges) with
   `nmcli device show` reason/state capture at snapshot time —
   **recommended.** No new dependency; reason codes are fetched when a
   transition is noticed, which is adequate at outage timescales.
2. Subscribe to NM D-Bus `StateChanged` signals for exact reasons/timing.
   Strictly richer (sub-tick timing, wpa_supplicant reason codes), but
   introduces the repo's first NM D-Bus client. Follow-up if poll-resolution
   evidence proves insufficient.

## 5. Test and verification plan (first)

Per-seam, per the testing contract; all runnable via `make verify-go`.

- **Ladder classifier (the core):** table-driven tests over a fake prober
  interface — each row a combination of {link, lease, gateway ping, DNS-local,
  DNS-public, TCP-backend, TCP-neutral, portal-204} results → expected
  classification. Every taxonomy value gets at least one row; ambiguous
  combinations (e.g. DNS-local fails, DNS-public succeeds) pin their verdict
  explicitly. Unknown/error probe results must map to `unknown-*`, never to a
  confident class.
- **Ring store:** size-cap eviction, segment boundaries on state transition,
  reopen-after-crash (torn final record tolerated), monotonic+wall+bootID
  ordering across a simulated clock step, fsync policy.
- **Recorder wiring:** fake provisioning-machine transitions and fake monitord
  edges drive the recorder; assert one snapshot per transition, ladder invoked
  only on failure edges, rate limit honored under synthetic flapping.
- **Gauges:** registry contents and cache-only reads — a scrape while probes
  are wedged returns last-cached values and never blocks (mirrors the §4.7
  stale-type pin). monitord gauge follows the existing two-gauge pattern in
  `metric/metric.go`.
- **Stage 2:** uploadLogs bundle test proving netlog segments are included and
  the 128 MB behavior is unchanged; `device_status` golden-payload test for the
  additive outage-summary field (and MD5-dedupe still suppresses no-change
  ticks); `runNetworkDiagnostics` command handler test via the executor's
  existing table, including the reply-before-probe/timeout semantics chosen.
- **On-device bench (manual, gated before each stage ships):** pull the AP,
  block DNS only, block 8.8.8.8 only, and portal-intercept on a bench network;
  confirm the four scenarios classify differently, the timeline appears in
  VictoriaMetrics after reconnect, and the recorder ring stays under cap after
  24 h of forced flapping.
- Gates that cannot run here (on-device bench, VictoriaMetrics end-to-end) are
  named in the PR as operator steps, not reported as passed.

## 6. Recommended staged rollout

Each stage is one PR into `develop`, independently shippable on the package
rail, and useful for the failing site on its own.

**Stage 0 — gauges into the existing spool (days).**
monitord: `net_internet_reachable` (0/1) + probe latency on `:9001`. controld:
link state/type, Wi-Fi RSSI/BSSID-hash/channel, relayer-connected +
last-close-code on the `:1111/metrics` registry, cache-only. Payoff: server-side
outage timeline for the failing device on the next package release, including
the "probe offline but relayer up" contradiction that would implicate the probe
target itself. (vmagent already scrapes both endpoints every 60 s — verified in
`users/feralfile/vmagent/scrape.yml`; no scrape-config change needed.)

**Stage 1 — flight recorder + diagnosis ladder (~1 week).**
New `netlog` package in controld: JSONL ring under `~/.logs/netlog/`, ≤ 8 MB.
Records provisioning-machine transitions, monitord edges, relayer
connect/disconnect/close-reason, and on failure edges runs the rate-limited
ladder: link/carrier → lease + gateway ARP/ping → DNS (configured vs public) →
TCP 443 (backend + neutral) → captive-portal 204 check → classification
(`link-down | no-lease | gateway-dead | dns-broken | captive-portal | wan-down
| backend-only-down | unknown-*`). NM reason codes captured via nmcli at
snapshot time.

**Stage 2 — automatic egress (days).**
(a) Recorder segments ride `uploadLogs` (free via branch B1) and controld
triggers a self-upload after a reconnect stability window (~2 min), reusing the
single-flight CAS. (b) Additive `lastOutage` summary
(`{start, end, class, count24h}`) in `device_status` — backend sees the
diagnosis without polling. (c) New `runNetworkDiagnostics` command (documented
in `controld-inbound-controller-messages.md`): runs the ladder once on demand,
returns the classification and probe detail; ACK/reply semantics follow the
existing fire-and-forget vs reply conventions in `devicectl`.

Out of scope (explicitly): any LAN-served download of the recorder (waits for
#3471), moving the ring to `/var/log` (full-image rail), NM D-Bus event
subscription (branch D2), probe-target redesign inside monitord (branch C2),
and the log-rotation size cap (real hazard, but it lives in
`users/feralfile/scripts/` — other rail, separate change).
