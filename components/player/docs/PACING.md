# Frame pacing on homelab

Work in progress, 2026-11-18. This is an isolated performance experiment, not
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
The native tombstone is off for this comparison. Existing device services remain
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
