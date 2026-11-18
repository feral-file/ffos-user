# Frame pacing on homelab

Measured on 2026-11-18. This is an isolated performance increment, not
whole-player or physical-panel acceptance. The production CEF pin remains
Chromium 156.0.8078.96. The matching-engine experiment uses CEF
156.0.15+gcf671c0 and Chrome for Testing, both Chromium 156.0.8078.80. The official
Chrome for Testing index has no .96 build; known .96 Chrome package URLs also
returned 404. Do not silently substitute a different Chrome engine or deploy the
older benchmark CEF as the production security pin.

## Reproduction

Engine URLs and archive hashes are in `evidence/pacing/engines.json`. Unpack CEF
into a separate directory and configure the same source with `-DCEF_ROOT=...`;
use a separate CMake build and DESTDIR installation. Follow the normal README for
sandbox ownership, runtime resources and private media dependencies. Do not copy
an uninstalled executable: its build RPATH differs from its install RPATH.

Download Transparent Grit from the source URL in `evidence/pacing/fixture.json`.
Run `tools/prepare-grit-benchmark.py SOURCE.html OUTPUT/grit-fixed.html`. The
script asserts exactly one canvas-sketch random seed-selection call, replaces it
with an explicit constant, and writes the original/fixture SHA256 values. This
is a benchmark-only modified document; it is not an offline/original-origin
fidelity test and is never used for normal playback. Both engines receive the
same fixture bytes from `tools/test-media-server.py` on loopback port 18990.

Start only one test arm at a time, under a user systemd service with MemoryMax=6G,
TasksMax=512, RuntimeMaxSec=900 and TimeoutStopSec=8. Both use Cage/Wayland,
`AMD_DEBUG=nodcc`, `--ozone-platform=wayland`, `--use-angle=gl-egl`, and
`--autoplay-policy=no-user-gesture-required`. Chrome additionally uses a separate
profile, kiosk and no-first-run. `--test-type=gpu` suppresses the Chrome for
Testing banner: without it, the artwork viewport was only 1920x1864. This behavior
is explicit in the matching Chromium tag's
[infobar utility](https://chromium.googlesource.com/chromium/src/+/refs/tags/156.0.8078.80/chrome/browser/ui/startup/infobar_utils.cc).
The native tombstone is off for this comparison. Native runs additionally use
`--diagnostic-presentation-timing=/home/feralfile/ff-native-pacing/timing.json`;
the artifact is written at graceful shutdown, outside the timing window. Existing device services remain
running in both arms. Compilation and failure tests are outside timing windows.

Run `tools/benchmark-matched.mjs CDP-ENDPOINT SSH-HOST TEST-UNIT FIXTURE.json OUTPUT.json 6`.
It refuses mismatched engine, source hash, seed, viewport, canvas, visibility or
WebGL backend; warms up for 20 seconds; then samples six ten-second windows,
covering repeated complete artwork animation cycles. It collects cgroup CPU and
memory plus renderer rAF intervals. A narrow Chromium trace collects only display
feedback events. Native CEF offscreen display events are not output presentation:
the native host separately collects Wayland presentation-time feedback. Filter
its bounded timestamp ring to the remote monotonic start/end clock in each run.
Neither Wayland feedback nor screenshots prove that physical pixels appeared.

## Experiments

- Baseline: blocking host swap, callback-local synchronized GPU copies, external
  BeginFrames tied to host progress.
- Nonblocking swap: remove the second display wait on CEF's UI thread, cap host
  composition and external BeginFrames at 60 Hz, retain callback-local GPU copy
  completion. The compositor owns vsync; the host does not request tearing.
- Rejected internal-clock variant: independent CEF BeginFrames appeared reasonable
  in rAF but accumulated GPU work. Wayland new-frame intervals averaged 813 ms,
  control calls reached 1.67 seconds, and copy waits averaged 408 ms. Its sources
  are not retained as a runtime option.

CEF's Linux accelerated paint handles remain borrowed only until callback return.
A `glFinish` after the GPU copy preserves this constraint; this change does not
claim zero-copy or repair CEF's NVIDIA producer-fence issue. Browser/GPU failures
still need external watchdog recovery. Native playback/input/status remain owned
by the host.


## Measured result

The implementation is in `786b476f`, with additional regression tooling in
`07fb9d85`. Both are on `codex/native-player`; no PR or activation was created.
The binary hashes and exact base Chromium/V8/WebKit revision are retained in
`evidence/pacing/engines.json` and each run's JSON.

The initial order was Chrome/native/native/Chrome, each with a fresh process,
20-second warmup and six ten-second windows. An exposure/remap invalidation fix
was then rebuilt for both engines; the final native binary was measured twice
again (`native-final-3/4`). The comparison below uses those final native runs
and `chrome-final-1/2`. The exposure change does not alter steady-state cadence;
its earlier runs remain separately identified, not silently relabeled.

| Metric | Pure Chrome, Chromium .80 | Native, Chromium .80 |
|---|---:|---:|
| Mean compositor interval, pooled | 55.39 ms | 58.74 ms |
| Effective output cadence | 18.05 fps | 17.02 fps |
| Median compositor interval | 50.0 ms | 66.7 ms |
| p95 compositor interval, both runs | 66.7 ms | 66.7 ms |
| p99 compositor interval, per-run range | 66.7 ms | 66.7–83.3 ms |
| Intervals above 100.5 ms | 0 / 2,250 | 14 / 2,119 |
| Mean CPU, percent of one core | 14.16% | 19.15% |
| Process-tree PSS, separate 20-second warm snapshot | 394.8 MiB | 598.3 MiB |

Source: `evidence/pacing/qualified-summary.json`, reproduced by
`tools/summarize-pacing.py` from the four raw runs and the native timing artifacts.
The summarizer requires matching engine, V8/WebKit revision, fixture hash/seed,
viewport and graphics backend, plus native timestamps covering the sampled window.
Chrome's `Display::FrameDisplayed` trace event explicitly uses the presentation
feedback timestamp in the
[matching source tag](https://chromium.googlesource.com/chromium/src/+/refs/tags/156.0.8078.80/components/viz/service/display/display.cc).
Native output uses Wayland presentation feedback, not CEF's offscreen compositor
feedback. Recorded native flags were 15 (vsync, hardware clock/completion and
compositor zero-copy presentation); the CEF-to-host GPU copy still exists.

This matches Chrome's p95, but **does not establish full performance parity**.
Native mean intervals are 6.0% longer, it consumes about five additional percent
of one CPU core, and rare long intervals remain. The first two native runs also
matched p99; the final rebuilt runs exposed the tail variation above. No run was
dropped from the evidence. MemoryCurrent includes cgroup/shared-page accounting
and varied substantially across Chrome launches; it is not a reliable memory-win
claim. Separate read-only `/proc/*/smaps_rollup` snapshots of every process in
each isolated test cgroup, after 20 seconds of Grit warmup, measured 394.8 MiB
Chrome versus 598.3 MiB native PSS. Those single snapshots are not a peak or soak
measurement, but show that this implementation has not matched Chrome memory
use. The retained `measure-tree.py`, `chrome-pss.json` and `native-pss.json` make
the process membership and sums auditable. Device-wide VRAM/GTT counters are in the clock samples, and must not be
mistaken for per-player GPU allocation.

The fair pre-optimization native baseline had rAF timestamp intervals averaging
62.72 ms, p95 102.8 ms and CPU 26.25%. The final runs averaged 58.34 ms rAF,
p95 63.1–65.4 ms and CPU 18.86–19.44%. These renderer observations demonstrate an
improvement, but are not substituted for compositor timing in the table. One
rejected internal-clock experiment made this distinction concrete: rAF averaged
57 ms while actual host output averaged 813 ms because GPU work queued up.

## Regression checks and limits

Both the matched .80 build and unchanged production .96 pin passed all 12 Linux
CTest groups; the macOS core passed all 10. Build logs and binary hashes are
retained. Production .96 device checks passed:

- Original-URL Transparent Grit, two distinct same-URL occurrences, continued
  animation and 186 native control calls: median 69.1 ms, p95 181.6 ms, maximum
  499.1 ms. The two compositor captures differ; the first was visually reviewed
  with native artist/title/exhibition label visible.
- Five input checks: dragging, double-click/scroll, physical keys versus Unicode
  text, held-key/button release on disconnect, stale-occurrence rejection.
- Static-artwork compositor pixel checks for cursor movement and five-second
  expiry, label settings and thirty-second label expiry. Artist animation cannot
  conceal a missing native redraw in this test.
- Twelve setup narration/state checks and compositor captures; the synthetic
  pairing QR screen was visually reviewed. No real pairing secret was captured.
- Thirteen libmpv media checks, including VA-API 4K, 4096-square software fallback,
  seek/pause/resume/EOS, HLS VOD, AAC/MP3 output and mute/volume; five decoder
  recovery checks. Audio output connection is not an audibility claim.
- Synthetic image orientation, animation, alpha and aspect-ratio pixel checks;
  three GLB/Draco/KTX2 decode/input checks.
- Active CEF renderer SIGKILL: the same host and playback generation recovered
  to a new occurrence in 1.79 seconds. Browser/GPU death still needs external
  recovery; this does not test survival of the host's own process death.

The fixed-seed fixture changes only benchmark seed selection and adds a marker.
It does not validate original artist HTTP headers or offline origin fidelity;
the separate original-URL regression covers live loading. Screenshots and
Wayland feedback are not physical-panel photographs. Display hotplug/exposure,
other rotations/scales, overnight behavior, whole-portfolio frame pacing and
security-update qualification are not completed by these short tests. The full
controld/FFOS migration remains incomplete.


The simple full-display 2D animation fixture remained at 60 fps: renderer rAF
mean 16.6666 ms in Chrome and 16.6702 ms in native; native compositor mean
16.6666 ms, p95 16.667 ms and p99 16.688 ms over 914 sampled intervals.
This verifies that heavy-artwork pacing changes did not impose a 50 fps cap on
light content. Raw artifacts are `*-light.json` and `native-light-timing.json`.
Both heavy final runs and this light run recorded zero CPU artwork copies.

CI [44205900548](https://github.com/feral-file/ffos-user/actions/runs/44205900548)
passed the clean container build, host/CEF-wrapper/libmpv compilation, 12 tests, schema check,
installation and artifact upload at `07fb9d85`. The independent final review
reported no material findings and `Verdict: accept`; its scope and limitations
are recorded in `REVIEWS.md`. No PR, publishing workflow or device activation
was performed. All isolated test services were stopped and signed `sleepNow`
returned `currentState=overrideState=sleeping` for homelab. The established kiosk,
controld and watchdog services remained running.

A local wakeNow request arrived 14 seconds after the first cleanup sleep. The
existing Home Assistant integration can send that command; the exact caller was
not established and no unrelated automation was changed. Sleep was reissued at
00:31:02 UTC; panel DDC standby was logged and a later signed device-status read
confirmed current/override state sleeping. `final-sleep.json` retains both events.
