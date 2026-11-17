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
