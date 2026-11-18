# Native player execution record

Owner: Anh. Started 2026-11-17. Status: implementation in progress; not release qualified.

## Current system and constraints

The current player is a Next/React application driven by controld's CDP evaluations. It owns browser-storage playback and UI state. Controld also resolves and refreshes DP1, applies content policy, schedules item displayAt cohorts, owns encrypted external control, provisioning, pairing, cache capture, media-health observations, and series resolution. Watchdog owns process/device escalation. The supplied initial ffos-user checkout was older than develop; `evidence/baseline.json` records both revisions. Uncommitted ff-player changes remain untouched.

Read the proposal and its complete discussion and prototype report (retained in evidence). The prototype's error-free navigation log is not visual validation of every work. No native replacement is established by that prototype.

## Ownership decision

Native C++ host: durable playback intent, occurrence identity, progression, media EOS, playlist schedule execution, settings, history, pointer/key state, native device UI, renderer lifecycle and bounded renderer recovery. Controld: external authentication/admission, source resolution/ref manifests, trusted verification annotations, provisioning/pairing, device power and sleep schedules, refresh of source documents, offline store/capture and media-health ledger. Watchdog/systemd: process failures, GPU hangs, resource limits and escalation with existing suppression gates.

No artwork gets a privileged bridge. Software loads as a top-level document at its original URL. Bundled image/GLB renderers receive only renderer-scoped configuration and emit readiness/errors. Direct video and audio artworks use a separate libmpv worker, following Anh's 2026-11-17 instruction to use an alternative to stock CEF for video. Nothing emitted by a renderer can choose a playlist or attest verification. The same host occurrence fences every async callback, input target and history commit.

## Composition branches and tests before selection

1. CEF native browser views: minimal copying but must prove independent native overlays and Wayland/Cage embedding on the pinned release. Linux window handles remain X11-oriented in the binary headers. Not assumed viable merely because Chrome uses Wayland.
2. Accelerated offscreen surfaces: explicit native composition, rotation, matte, labels and transitions; one host-owned GPU copy per artwork frame is required by CEF's callback lifetime. Verify DMA-BUF import, producer synchronization, callback-local lifetime and frame pacing. CPU `OnPaint` is diagnostic fallback, not an accepted production high-resolution path.

CEF 156.0.30 / Chromium 156.0.8078.96 is the current stable binary in the official build index, downloaded and hashed. CEF #4313 reports producer readiness problems fixed in M158: examine selected source and test on AMD before qualifying. Shared GPU/browser processes remain failure domains. Request-context storage isolation does not prove process isolation.

## Verification sequence

- Protocol shared fixtures: framing fragmentation, malformed/duplicate fields, version negotiation, bounds, stalled clients, reconnect snapshots/gaps, stale generations, deadline expiry and idempotency.
- Core deterministic clock tests: selected/loading/committed/presented separation, same-URL occurrences, shuffle/repeat, EOS, stopped/sleep/boot/schedule, policy, history and failed persistence.
- Go/C++ real socket interoperability, not only two parsers tested independently.
- Linux build/install/sandbox/resource and license checks; native input/overlay correctness and renderer recovery.
- FF1-SKYZ2E3A (then homelab, selected by Anh after the first device became unreachable): backup, old-player baseline, all Living Code works plus holz, every media type, offline cold restart, failure injection, AMD large-H264 regression, process/GPU memory and pacing/input metrics, physical-output observation distinct from screenshots, overnight soak with per-work evidence.
- Engine update rehearsal with reproducible rebuild and repeated conformance/runtime suite.
- Required repository checks and independent fresh-context completion review. Document unresolved findings and exact evidence limits.

## Product decisions

Anh approved overlay labels including exhibition name, curator cards only when provided, configurable transitions. Default preserves timed/on/off labels. Do not imply the exact final visual design has been approved. No hardware acceptance may be marked complete before observations exist.

Video decision: use libmpv 0.42.0's supported GPU render API, with libplacebo 7.387.0. A private worker owns demux/decode only, and sends DMA-BUF descriptors over a bounded inherited socket. The host owns selection, timing, transitions and labels. Video does not enter a CEF video element. Videos embedded inside an artist's software document still belong to that original browser document; browser codec coverage remains a separate portfolio check. See `MEDIA.md` for the comparison and remaining verification.

## Rollout and rollback

Develop branches only. Build and test the runtime before altering the installed launch path. Export legacy storage through a running old-player API into a versioned interchange document; never open live Chromium databases. Keep export and old installation for deliberate rollback. Import atomically; failed/corrupt storage never becomes an empty cast. Activate the runtime and matching controld/session changes together in a full-image release. Prepare release ledger and exact human promotion/dispatch instructions; do not dispatch staging or production publishing workflows.

## Repository ownership update

Anh requested the source move on 2026-11-17. The runtime now lives in
`ffos-user/components/player`, next to controld; the old sibling contains only
local build caches. Runtime/package identity remains `ff-player-native`.
Development CI builds and tests the complete CEF/media runtime without publishing.

## Progress

- [x] Initial repository revisions/worktree changes and proposal recorded.
- [x] Test device identified and read-only inspection completed.
- [ ] Complete behavior matrix and source audit.
- [ ] Protocol, core, persistence and interop implementation/tests.
- [ ] CEF native presentation, input, portfolio and offline integration.
- [ ] Controld and FFOS migration, build/package and rollback.
- [ ] FF1 validation, update rehearsal, overnight evidence and review.

2026-11-17 media milestone: native FFmpeg/libmpv build and sandboxed DMA-BUF playback verified on homelab. Details and limits are in `VERIFICATION.md`; this does not complete the migration.


2026-11-18 failure investigation: user authorized waking homelab and correcting
Transparent Grit. The device had a fresh filesystem; isolated staging was
restored from the verified CI artifact, without activating partial controld.
The host's RunUntilIdle loop and poll-and-drop path caused frame/control
starvation. Scheduled UI ticks, callback coalescing and external BeginFrames
passed the real artwork/control regression. Whole rewrite and coordinated FFOS
activation remain incomplete; BENCHMARK.md records comparison and limitations.

2026-11-18 frame-pacing optimization, requested by Anh: establish an isolated
CEF/Chrome for Testing pair at Chromium 156.0.8078.80 (the production .96 pin has
no published matching CfT binary). Generate a benchmark-only Transparent Grit
fixture with an explicit fixed canvas-sketch seed and hashes of source/fixture.
Keep original artwork and production dependency pin unchanged. Assert engine,
viewport, WebGL backend and seed before measurement; alternate fresh native and
Chrome processes with warmup and multiple animation cycles. Measure renderer rAF
separately from native GPU copies, swap/copy waits and control responsiveness.
Profile before changing synchronization; borrowed CEF buffers must still be
consumed within their documented lifetime. Validate any optimization on both the
matched engine and production engine, run input/media/recovery checks, obtain a
fresh-context review, push only the feature branch, and restore homelab to sleep.
No renderer timing or compositor screenshot constitutes physical scanout proof.

Pacing increment outcome: nonblocking, damage-based composition and a stable
60 Hz frame clock implemented and tested. Same-engine/same-seed measurements
show matching p95 but not complete Chrome parity; PACING.md retains mean, tail,
CPU and memory differences. Production .96 regressions and clean CI passed;
fresh-context final review had no material findings. Feature branch pushed,
no PR or activation. Homelab was returned to sleep.

2026-11-18 hands-on replacement request: wake homelab and leave it awake for
Anh's artwork testing. Baseline ffos-user 3cf6844e, ffos a9f51875 and ff-player
07805905; ffos-user/ffos clean, ff-player's existing artwork-inspection and
setup/pairing edits belong to other work and remain untouched. Match native
QR/setup and tombstone presentation to the current ff-player sources, including
PP Mori, short-edge sizing and timer. Verify raster geometry, QR decoding,
on-device captures and ordinary external controller commands. Complete the
normal-control/session/watchdog migration before switching the installed kiosk;
the isolated performance stage is not yet an installable replacement. Preserve
legacy state/export and installed files for explicit rollback. Report coverage
by capability and distinguish acceptance from presentation. Do not claim full
coverage from the existing synthetic renderer tests.


2026-11-18 display-control continuation: the current runtime saves margin and
background but omits them from effective fallback settings, ignores matte color
in host composition, and computes percentage margins against the shorter edge
instead of DP1's per-axis viewport percentages. The phone's ephemeral adjustment
and composition revision contract has no native equivalent yet. Implement a
strict occurrence-fenced presentation.set method (no implicit current target in
the native protocol), separate desired settings from host-composed settings,
and clear transient adjustments on new occurrences/restart. Preserve authored
layers and device framing precedence; legacy keyless phone writes are resolved
at the controld adapter boundary. A receipt reports the last composed revision,
not success on screen. Backend configuration/frame acknowledgment must precede
a new composition status. Test stale/same-URL selections, no-op acceptance,
restart, writer failures, per-axis percentages, actual matte pixels and paused
media settings. Do not activate partial controls before this contract is wired.
