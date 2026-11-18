# Verification record

Status: in progress. No release or hardware acceptance declaration.

## 2026-11-17: native media on homelab

User selected homelab after FF1-SKYZ2E3A stopped resolving. Device: FFOS 3.0.0,
Radeon 680M/Rembrandt, Linux 6.18.38-2-lts, Mesa 26.1.4, Cage 0.3.1. The attached
HDMI display advertises **1920×1920 at 60 Hz**, scale 1. These are 4K-source
decode checks, not full-resolution 4K scanout acceptance.

The pinned FFmpeg 8.1.2, libplacebo 7.387.0, libmpv 0.42.0 and C++/CEF runtime
built natively under CPUQuota=300%, MemoryMax=6G and TasksMax=256. Initial build:
7m48s wall time, 2.9 GiB peak cgroup memory as reported by systemd. This is not a
CEF source build. The CEF distribution remains the pinned stock binary for
software; direct media does not use its codec implementation.

| Check | Observation | Evidence |
|---|---|---|
| Linux core, real socket framing/timeouts, media FD transfer, H.264/AAC decode | Four CTest groups passed | `evidence/homelab-native-rebuild.log` |
| Go module tests/vet/lint | Tests and vet passed; changed-diff lint reports 0 issues | `evidence/controld-*.log` |
| Real Go/C++ socket interoperability | Passed with the compiled macOS core and Go race detector | `evidence/go-cpp-interop.log` |
| 3840×2160 H.264, 30 fps source | VA-API decode; GPU buffers imported by native host; no reported decoder/VO drops in the short check | `evidence/homelab-media-check-extended.json` |
| Pause, resume, exact seek, volume, mute | Observed through backend properties; seeks preserve occurrence identity | Same record |
| autoPlay false | First frame held until explicit resume | Same record |
| Adjacent identical URLs | EOF advances to a different item-occurrence ID | Same record |
| 4096×4096 H.264, 2 fps fixture | VA-API rejects the dimensions, software decode presents successfully | Same record; not a high-frame-rate stress test |
| HLS VOD | Native decode and host composition passed | Same record; live streams/disconnects not tested |
| AAC and MP3 | Progressing media connected to PulseAudio; volume/mute applied | Same record; speaker audibility not established |
| Decoder SIGKILL | Host instance survives, reports failure and accepts next cast | Same record |
| Decoder SIGSTOP | Five-second heartbeat expiry reports failure and terminates worker | Same record |
| Device-owned failure UI | Compositor capture retains usable error text after decoder hang | Private `evidence/device/homelab-decoder-hang.png` |

The corrected compositor capture shows upright video with the native label. The
first capture exposed a vertical flip, fixed by matching libmpv's FBO orientation
to the host's texture convention. Initial namespace configuration omitted GPU
sysfs metadata; adding read-only `/sys/devices`, `/sys/dev/char` and
`/sys/class/drm` restored Mesa device discovery. These failures are retained in
the logs rather than excluded from the record.

The initial host run recorded 3,294 GPU import/copies, zero CPU artwork copies,
and 4,001 ms cumulative import/copy/fence time (about 1.21 ms per copy). Its
surface was 1920×1920. This is a short aggregate, **not** frame-pacing, latency or
full-resolution 4K qualification. The later timing protocol distinguishes
buffer-release ACK from host-swap ACK.

Screenshots from DevTools show a renderer; `grim` captures Cage composition.
Neither establishes physical HDMI/panel output. Raw old-player state and
artwork captures are excluded from git under `evidence/device/`. The old
Chromium kiosk, controld and watchdog remain installed and active. The native
test service was stopped after this test block.

## Remaining acceptance work

2026-11-17 input/status increment: five Linux CTest groups passed after adding
host-owned input state. `homelab-input-check.json` records real CEF artwork DOM
observations driven exclusively through the product Unix socket: drag with a
40-pixel matte, double click, wheel, physical KeyA/ArrowLeft/ShiftLeft, separate
Unicode text, held-input release on disconnect, same-URL stale-occurrence
rejection, and retained pointer position across replacement. CDP was read-only
test observation. `homelab-input-cursor.json` records pixels from a Cage capture:
native cursor center at output (400,500), artist marker at document (360,460)
plus the 40-pixel matte. The raw capture is in the private device directory.
This does not qualify rotation, scale factors above 1, input latency, physical
keyboard hardware, or physical panel output. The test runtime was stopped and
the original kiosk/controld/watchdog were confirmed active afterward.

Controld status and device display-URL reads now use typed native snapshots.
Tests preserve outgoing playlist metadata while incoming playback loads/fails
and prevent a URL-only verification verdict from crossing native generations.
The remaining command/session consumers still use CDP: the changed controld
has **not** been installed on the device and is not independently deployable.

Complete behavior/consumer migration, typed protocol schemas and broader
conformance cases, durable legacy import/rollback, offline resource replay and
its worker/frame semantics, input/rotation/focus, bounded renderer recovery,
full native setup/pairing UI, all portfolio works and GLB fidelity, transitions,
4K output, live-stream failures, physical audio/display verification, comparable
resource/frame/input measurements, overnight soak, pacman/full-image release
integration, reproducible clean build, CEF security-update rehearsal and a
fresh-context completion review. Passing media tests does not complete these.

## Protocol, persistence and packaging increment

Generated JSON Schema now covers request envelopes/method parameters, response,
snapshot, occurrence, settings, history, subscription and scheduled outcomes.
`tools/test-protocol-schema.py` validated 27 real C++ exchanges plus an event.
The same 24 negative request fixtures and framing/parser fixtures are consumed
by C++ and Go. Persistent command connections and separate snapshot watches
passed reconnect/cancellation/stale-generation interoperability with Go's race
detector. `schema-requirements.txt` pins the schema test environment.

`homelab-media-cleanup-check.json` and `homelab-input-cleanup-check.json` repeat
the 13 media and five native-input checks after worker cleanup/storage changes.
The isolated test runtime was stopped; the original kiosk, controld and watchdog
were confirmed active. Later scheduler/policy changes are core-tested and still
need the final packaged-runtime smoke test.

The version-2 legacy export and atomic importer now have refusal/restart tests.
Actual homelab state passed two host starts, preserving 100 items, index 49 and
50 history records without manufactured rendering/history. The importer
explicitly refuses unsupported fallback/per-edge-setting cases; see MIGRATION.md.

Two fresh-context intermediate reviews found and prompted fixes for scheduling,
URL admission, pause timing, policy retirement and malformed DP1 IDs. Findings
and dispositions are recorded in REVIEWS.md; final completion review is pending.

FFOS now contains local source-bundle/pacman tooling under
`packages/ff-player-native`. Two source-preparation runs produced SHA-256
`0bc68209aa262316308624e0f240c343a61d9719e986f1b514e7aab8174d92b3`.
This proves deterministic bundling for those inputs, not binary reproducibility.
Three clean makepkg builds completed. Package `.2` installed with 297 files
verified by pacman and a root-owned mode-4755 sandbox, but its device smoke
was rejected for `vaapi-copy`. Package `.3` restores explicit Wayland VA-API
support and passed eight Linux CTest groups; installed-package testing remains
pending. The equivalent staged runtime passed 13 media checks, five native
input checks and five bounded-recovery checks (`wayland-*-check.json`). No
publishing workflow ran. CEF debug packaging reports missing upstream DWO
objects; complete engine-symbol packaging remains open.

## Bounded renderer recovery increment

Six macOS and eight Linux CTest groups pass, including retry backoff, exhaustion,
stale callbacks, stop/sleep cancellation, budget reset after stable presentation,
and outgoing-renderer retirement. Go race tests with real C++ interoperability
and 27 schema-validated exchanges pass. On homelab, replacement presentation
followed decoder kills in 2.07, 3.05 and 5.07 seconds (including the 1/2/4-second
backoff and startup). The fourth failure remained terminal for the observation
window with the same responsive host. Explicit refresh recovered it. These are
short controlled tests, not an overnight stability claim.

Linux `ffos make verify` and `ffos-user make verify-scripts` passed in isolated
source checkouts on homelab; logs are `evidence/ffos-verify.log` and
`evidence/ffos-user-verify.log`. macOS-only failures in Linux-specific shell
fixtures were superseded by these Linux runs, not ignored.

## GLB and native setup increment

`homelab-models-fence/result.json` records three successful model checks: plain
GLB, Draco geometry, and Draco plus KTX2 textures. The exact legacy model-viewer
4.3.1 bundle is retained; Draco 1.5.6/Basis decoder assets and notices are now
pinned and bundled. Host logs show local decoder opens. Actual socket-delivered
drag changes the camera. The chess renderer PNG was visually inspected and
contains the model. These captures are renderer-only evidence, not physical
panel output or comparison against the old viewer. Animation equivalence and
offline cached-model tests remain pending.

The same build passed eight Linux CTest groups, 13 media checks, five input
checks and five bounded-recovery checks after the producer-fence/readiness
fixes (`fence-*-check.json`). The isolated runtime was stopped and original
kiosk/controld/watchdog services were confirmed active.

Native setup admission/panel/QR tests bring local CTest to seven groups. The
setup transport retains the prior ownership-race tests and adds host-observation
and reconnect tests. Go race tests with the real C++ socket pass. The standalone
Go module tests and vet pass; final lint and device setup checks are in progress.

`homelab-setup/result.json` records 11 native setup compositor captures and
ready-clear behavior on homelab. Swift Vision independently decoded all three
QR modes (Wi-Fi credentials, attached portal and synthetic claim URL); exact
payloads are retained in `decoded-qr.jsonl`. The join-panel capture was visually
inspected: instructions and code are visible without clipping. These are Cage
compositor captures, not photographs of a panel or proof of a successful phone
join/claim. All credentials in this test are synthetic. Nine Linux CTest groups
pass. The full Go module tests, vet and changed-diff lint pass (zero issues).

The post-fence test invocation logged 2,459 GPU copies, zero CPU copies, 98
busy producer frames skipped, and 3,655.93 ms aggregate GPU-copy/synchronization
time. This mixed smoke run is not a matched baseline or frame-pacing benchmark.

## Installed package qualification, development build .4

`ff-player-native-0.1.0.dev20261117.4-1-x86_64.pkg.tar.zst` built on homelab
in 9m34s wall time with a reported 4G cgroup memory peak. Its SHA-256 is
`71700d1c03cf63e7b7653dc1d8ffdbc9488ef272a80155e86a109c9dc41146a7`;
the source bundle is recorded in `evidence/package-setup.json`. Pacman installed
it and verified 306 files with zero alterations. The sandbox is root-owned,
mode 4755. This package includes the final setup credential bound and GLB/fence
fixes, but **not** the later playback-mode and source-refresh implementation.

The executable/resources under `/usr` passed 13 media checks, five input checks,
five bounded-recovery checks, three GLB decode/input checks, and 11 setup-panel
captures plus ready-clear (`evidence/package4-*`). Recovery after the first three
decoder kills took 2.05, 3.03 and 5.02 seconds, including configured backoff; a
fourth remained terminal until explicit refresh. Native audio observations
establish PulseAudio connection/progression, not speaker audibility. Renderer
and compositor captures still do not establish physical display correctness.

The test service was stopped and the existing kiosk, controld and watchdog
confirmed active. Full build/runtime logs are retained. Missing upstream CEF
DWO objects and absolute source paths in private media-library build metadata
remain recorded packaging limitations; binary reproducibility is not claimed.

## Source refresh and playback modes

Nine macOS and eleven Linux CTest groups pass after the mode/refresh increment.
The full controld module tests and vet pass; changed-diff lint reports zero
issues. Uncached Go race tests with the real C++ host pass for playerlink,
status and setup narration. The schema suite validates 28 real exchanges and
one subscription event. Logs are retained under `evidence/refresh-*` and
`evidence/native-refresh-go-*`.

`refresh-device.json` records three native socket checks on the newly staged
Linux/CEF runtime: a duplicate-slot refresh retains the same decoder PID and
occurrence without another history entry; native video EOF selects a deferred
software playlist; fresh mature labels retire the presented work and expose
the policy outcome. Core tests additionally cover restart, storage-lock failure
and retry, pending cancellation, exact duplicate ordinals, signature removal,
boot projection, successive outgoing refreshes and policy retirement during a
pending update. This increment is **not** in the installed `.4` package yet.

The isolated runtime was stopped after checks; original kiosk, controld and
watchdog services remained active. Fresh-context review accepted the updated
slice (REVIEWS.md). External command/refresher consumers still need migration;
these tests do not establish full player replacement acceptance.

## Corpus and synthetic image checks (2026-11-17)

`evidence/corpus-first-review.json` records all 18 Living Code plus holz corpus
entries with compositor capture hashes and individual visual observations.
Twelve entries show artwork content. Two dweb.link entries returned HTTP 403;
36 Points, Transparent Grit (retry), Returns and Slime Skelly did not produce
a presentable frame within the host's 60-second deadline. Diagnostic screenshot
connections failed for Arrels and Gazers, but their Cage captures show artwork.
These observations do not establish reference equivalence or physical output.
Transparent Grit is under further standalone-Chromium comparison.

Synthetic EXIF-6 JPEG orientation, GIF animation, PNG alpha and aspect framing
passed dimension and screenshot-pixel assertions (`evidence/images-device.json`).
This does not cover every image format, color profile or very large image.

The isolated CEF rehearsal built 156.0.28 (Chromium .88), then 156.0.30
(Chromium .96), with all eleven Linux tests passing in each phase. Only the
updated engine was launched. Its staged runtime passed media, recovery, input
and model checks; retained engine evidence is still being collected.

## Relocation and CI qualification

The runtime source now lives in `ffos-user/components/player`. All nine macOS
core test groups, the 28-exchange protocol schema suite, real C++/Go
connectivity interop, Go race tests, full controld tests, vet and lint pass.
The ffos-user script suite passes in Linux; root-only permission cases and the
missing darkhttpd check are explicitly skipped by that suite. FFOS `make verify`
passes in a non-root Linux container. macOS attempts encountered Linux-only
tool assumptions and are not counted as passing. Logs use the `reorg-*` prefix.

The full CI container requires Wayland >=1.23 and wayland-protocols >=1.38
for the pinned mpv build. Ubuntu 24.04 was rejected; the development container
now pins Ubuntu 26.04 and its manifest digest. The clean [GitHub run 44124375553](https://github.com/feral-file/ffos-user/actions/runs/44124375553)
passed at source commit `0b7156f0873383b01caad7325ec14b46fc7c57e1`: full CEF and
private-media compilation, all eleven Linux tests, schema freshness, installation,
and artifact upload. `evidence/ci-linux-build.log.gz` retains the full log;
`evidence/ci-artifacts.json` records artifact size/digest. Later commits only
update documentation/evidence. This does not qualify GPU presentation or replace
the pinned FFOS pacman package qualification.

## 2026-11-18: Transparent Grit frame-loop correction

Homelab was awake and running a fresh FFOS filesystem; the prior native staging
and package were absent. Restaged the pinned CEF and private media libraries from
successful CI artifact 14347008935, then rebuilt the changed host on the device.
The full CMake build and eleven CTest groups passed. Nine macOS core groups passed.
The original kiosk, controld and watchdog remained active; native tests use an
isolated systemd unit, socket and state directory.

The GPU-copy experiment did not deadlock permanently inside `glFinish`: tracing
showed repeated completed copies while commands timed out. CEF's pinned
`CefMainRunner::DoMessageLoopWork` invokes `RunUntilIdle`, which continuous capture
callbacks prevent from returning. Scheduling host work inside the CEF loop fixes
that starvation, but independent frame production still built a long GPU queue.
One admitted copy per host turn plus host-driven external BeginFrames removed
that backlog in the regression run.

`grit-render-loop-check.json` records two consecutive occurrences of the original
Transparent Grit URL, sustained native state queries, no recovery/reselection,
and two changing Cage compositor captures. All 154 calls completed (each includes
hello plus the command): median 120.0 ms, p95 223.6 ms, maximum 310.8 ms. The two
`grit-native-compositor-*.png` captures were visually inspected: both show the
animated granular artwork and separate native artist/title/exhibition label,
with different artwork geometry. They are compositor evidence, not photographs
of the panel or artist-reference equivalence. Additional benchmark and renderer
regression results follow in BENCHMARK.md. This is not whole-rewrite acceptance.

Final increment checks on the restored reviewed source (`689933a3`):

- CEF renderer SIGKILL while Grit was active: same host instance/socket and
  playback generation survived, bounded recovery presented a new occurrence in
  1.776 s (`grit-cef-recovery.json`). Renderer PID was identified using this
  isolated browser's diagnostic SystemInfo endpoint, then checked against its
  systemd cgroup before signalling; CEF children retain zygote argv on this build.
- Five native-input checks, 13 native-media checks and five media-recovery checks
  passed. These include VA-API 4K H.264, 4096-square software decode fallback,
  pause/seek/resume, same-URL EOS progression, HLS, audio controls and decoder
  kill/hang recovery. The first media rerun used Python's generic HTTP server,
  which does not advertise ranges; `seekable:false` correctly failed its fixture
  precondition. Repeating with the repository's `test-media-server.py` passed.
- EXIF orientation, animation, alpha/aspect-ratio pixel checks and all three
  plain/Draco/KTX2 model decode/input checks passed (`grit-image-check.json`,
  `grit-model-check.json`). Static documents still acquire post-ready frames.
- The complete clean Linux CI build at `689933a3` passed all eleven CTest groups,
  schema freshness, installation and artifact upload:
  https://github.com/feral-file/ffos-user/actions/runs/44197619371 . Its compressed
  log and artifact digest are retained as `grit-ci-linux-build.log.gz` and
  `grit-ci-artifacts.json`.

The BeginFrame-before-vsync experiment was reverted after it failed to show
better frame pacing. It is not in the final source. See BENCHMARK.md for both
measurements, engine/scene differences and remaining performance limitations.

The related-work rerun (`grit-related-corpus.json`) presented holz and Sage
Jenson's 36 Points #25. Their native compositor captures were visually reviewed:
blue simulated logs for holz, granular circular forms for 36 Points, both with
separate native labels. holz remained a top-level original URL with its CSP
recorded in the response evidence. Returns and Slime Skelly still failed before
navigation committed at their original ipfs.io URLs; their 60-second failures
remain unresolved and are not counted as displayed works. Independent curl HEAD
requests to both original URLs timed out after 15 seconds without an HTTP
response (`grit-gateway-probe.log`); this is additional network evidence, not
proof of a permanent gateway outage. Slime Skelly's compositor capture shows a
readable native failure explanation. Raw captures are
retained locally under `evidence/device/grit-related-corpus`, with hashes in the
tracked evidence. This rerun is not full-corpus or physical-display acceptance.

After verification, the isolated runtime and fixture server were stopped. The
original kiosk, controld and watchdog were active. The owner CLI's signed
`sleepNow` returned success and `currentState: sleeping`, `overrideState: sleeping`
(`grit-final-sleep.json`). Native code is staged for development; partial controld
migration has not been activated as the production session.


## 2026-11-18: matched-engine pacing optimization

See [PACING.md](PACING.md) for the fixed-seed Chromium .80 paired comparison,
raw compositor measurements and limitations. Native p95 matched Chrome at
66.7 ms; mean throughput, rare tails, CPU and PSS still differ. The production
.96 pin was unchanged and passed the repeated input/static UI/setup/media/
image/GLB/recovery regression set. Lightweight native compositor output remained
60 fps. Both Linux engine builds passed 12 CTest groups; macOS core passed 10.
CI 44205900548 passed for source 07fb9d85. Final fresh-context review found no
material issues in this increment. Full migration, hotplug/rotation, physical
output, overnight and security-update acceptance are still incomplete.
Homelab test units were stopped and signed sleepNow confirmed sleeping.


## 2026-11-18: native QR, tombstone and setup presentation

See DEVICE-UI.md and evidence/ui-20261118 for packaged PP Mori, exact QR payload
decoding, setup/backdrop ownership, timer/fade, initial media hold and early EOF.
Thirteen Linux and ten macOS CTest groups passed. Media/image/GLB/input and
static presentation regressions passed on ANGLE Vulkan, now the default after
reproducing the p5 line regression in both CEF and standalone Chrome's GL path.
A final launch with no backend override verified Vulkan and compositor QR.
Fresh-context final review: accept; whole-rewrite activation remains incomplete.


## 2026-11-18: occurrence-scoped display controls (in progress)

Implemented presentation.set, per-axis percentage margins, saved matte defaults,
causal configuration/composition revisions and Go status mapping. Before the
review fixes, 14 Linux and 11 macOS CTest groups passed; all controld Go tests,
vet, scoped lint, playerlink/status race tests and 28 real schema exchanges
passed. Homelab checks exercised actual software/video matte pixels, paused
video reconfiguration, stale targets, no-op completion, media/image/GLB/input
and the 65-second covered-video regression. Raw development evidence remains
under ignored evidence/device/display; it does not qualify the final fixes.

Review fixes add shared CSS color normalization, separate captured/submitted
textures and retention across covered changes. All 11 macOS CTest groups and
schema freshness pass after these changes. Homelab became unreachable during
its rebuild, before installation of those fixes. Cause is unknown. Anh selected
FF1-SKYZ2E3A (192.168.31.52) instead; SSH and signed wakeNow succeeded, with
currentState/overrideState awake. Final Linux build/device checks are pending.
The original installed kiosk/services are preserved. Normal command/session/
watchdog migration still prevents declaring a complete installed replacement.


FF1-SKYZ2E3A continuation: 14 Linux CTest groups passed on source 69fddedd
with the pinned CEF and private codec libraries. Clean Linux CI 44221971963
also passed. At 3840x2160/60 Hz, display controls (including review fixes),
12 setup state checks, three exact compositor QR decodes, static cursor/label
expiry, five native input checks, EXIF/GIF/alpha pixels and three GLB/decoder/
interaction checks passed. A one-shot original state export imported into
`.state/native-office-preview`; no original browser database was changed.

The newly generated 4096-square H.264 fixture exposed a decoder crash after
VA-API rejected the dimensions. Host and socket survived. This was outside the
previous core/CI coverage. A four-thread software decode bound passed the same fixture, followed by all
13 media checks and 14 Linux CTest groups. The 65-second covered-video
regression also passed. Startup/import is verified; sustained large-video
fallback throughput, pacing and physical output remain unqualified. The new
fixture generator records its FFmpeg version, commands and hashes, so this
regression no longer depends on files generated only on homelab.


Transparent Grit at 4K passed two same-URL occurrence cycles with changing Cage
compositor captures and 127 native control requests (median 130.39 ms, p95
405.28 ms, maximum 779.07 ms). Capture review shows the work and native
artist/title/exhibition panel. This is a responsiveness regression, not a
matched-seed/engine Chrome benchmark or physical scanout evidence. Retained
evidence is in `evidence/office-20261118`, including a SHA-256 manifest.


## 2026-11-18: extensionless media and HTTPS on FF1-SKYZ2E3A

The restored work “24” exposed two gaps: its extensionless video URL was routed
to a stock CEF video document, and the isolated mpv worker could not resolve
Arch's CA-bundle symlink outside `/etc/ssl`. Added occurrence-scoped response
MIME classification and the read-only CA target mount. CEF callback wrappers
do not preserve C++ pointer identity; probe lifetime uses terminal state and
owned references instead of comparing wrapper addresses.

All 11 macOS core and 14 Linux CTest groups passed. Five FF1 source checks
passed: opaque video, HEAD rejection with GET fallback, redirect, superseded
slow response and a 3.08-second timeout with a responsive host and no false
presentation. “24” then played through H.264/VA-API at a 3840x2160 surface; the
retained sample reports 764 imported frames and zero decoder/frame drops. A
self-signed HTTPS direct-media test failed before presentation and restored the
prior playlist afterward. These short observations do not qualify sustained
frame pacing. Compositor capture showed the portrait work fitted inside the
4K display; physical scanout remains unverified. Evidence: source.json,
source-ctest.log and https-media.json in evidence/office-20261118.

The preview remains awake with the original work restored. It is a transient
service, not the completed phone/session/watchdog replacement. HANDS-ON.md
documents access and rollback.


## 2026-11-18: controld input boundary on FF1-SKYZ2E3A

Deleted devicectl's CDP cursor/viewport/keyboard implementation. The new
coordinator sends whole validated gestures through a dedicated native socket
connection, with one snapshot fence, bounded ordering and disconnect cleanup.
The C++ host resolves reference-width deltas using its current output geometry.
Eleven macOS and fourteen Linux CTest groups passed.

The real Go coordinator (opt-in test, not the full running daemon) passed four
FF1 checks observed in the artwork document: relative drag coordinates at 4K,
DOM double-click sequence, both pinch wheel directions, and Shift/ASCII text
plus Enter newline, Tab focus and Enter form submission. This caught and fixed
two transport differences: native CEF wheel direction is opposite CDP; Enter
requires a separate carriage-return character event for default editing. The
existing five-check native input suite passed afterward, including disconnect
release and same-URL stale occurrences. Evidence: controller-input.json,
input-after-cutover.json and input-ctest.log. No phone-to-daemon deployment is
claimed yet. The test units were stopped and “24” restored in the awake preview.

The complete controld Go suite, go vet, changed-diff lint and devicectl/playerlink
race tests passed for this cutover. Clean Linux CI 44226732243 passed for the
preceding source/TLS revision 3d0734a6; this does not substitute for the input
increment's own device/build results.


The 2026-11-18 FF1-SKYZ2E3A 4K/Vulkan follow-up is in OFFICE-PACING.md.
It qualifies neither the older homelab numbers nor whole-player parity: the
current offscreen host remains slower; a native Views diagnostic is measured
separately. The source/input commits 3d0734a6 and 9ad0b05a passed clean Linux CI
runs 44226732243 and 44228336255 respectively.


## 2026-11-18: native controller controls

Six external commands now use the native snapshot/command boundary. The real
Go adapter passed on FF1-SKYZ2E3A for saved settings, duration/loop/shuffle,
same-URL artwork selection, stale preview rejection, composed percentage
margin/color, malformed parameter rejection and duration reset. Evidence is
`evidence/office-20261118/controller-controls.json`. The daemon itself remains
unactivated while cast/session/watchdog migration continues.

Fresh review found F1 (Moderate): raw native transport errors would be logged
without a reply by the legacy relayer. Snapshot failures now produce an explicit
unavailable reply before mutation; lost mutation replies report `outcome_unknown`
and require status reconciliation, with no automatic retry. Invalid host state
and receipt failures also produce explicit replies. Focused tests, vet and lint
passed after this correction. The full Go suite, vet and commandrouter/playerlink
race tests passed before and after it; one initial relay timing test failed under
concurrent checks, passed three isolated reruns, then the full suite passed.

The preceding 4K benchmark revision 37028ec4 passed clean Linux CI 44231894052.

The final fresh-context controls review found no material findings
(`Verdict: accept`) after the error-reply correction.


## 2026-11-18: refresh independent of artwork and page recovery

`refreshArtwork` now uses native `playback.refresh`. Deleted the router's
Chromium cache purge, page-navigation escalation and session dependency.
Empty/stopped/sleeping selection returns `no_artwork` without state changes.
Twelve local and fourteen device Linux CTest groups passed; Go tests/vet/lint
passed. The isolated Go control suite also passed with refresh preserving the
cast generation/index and creating a new occurrence.

A source-probe failure first demonstrated control after exhausted loading
retries (`controller-recovery.json`); this was not a renderer crash. A separate
`Page.crash` diagnostic did not terminate within the test window and was not
counted as a pass. Deterministic SIGKILL injection then killed only renderer PIDs
reported by the test browser and independently checked against its systemd
cgroup, using pidfds to avoid PID reuse. Four renderer terminations left the
same host instance/generation responsive, with bounded retries then exhaustion.
The real Go refresh command created a new occurrence and the host composed the
fixture again. Evidence: `controller-renderer-kill.json`. This is renderer-process
recovery, not browser/GPU survival or physical-panel verification. The temporary
test services were stopped and the installed preview restored to “24”, awake.

The refresh completion review found no material findings (`Verdict: accept`).
The additional idle/sleep/stopped no-mutation tests passed in the twelve-group
local suite. The installed preview reports responsive/presented, title “24”,
VA-API decode and sleeping=false after the update.
