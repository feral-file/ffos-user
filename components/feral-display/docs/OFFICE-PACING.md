# 4K Transparent Grit comparison, FF1-SKYZ2E3A

The installed offscreen host is still slower than standalone Chrome. A native
CEF Views diagnostic removes the observed frame-pacing gap in one run, but uses
more CPU and is not a replacement implementation.

All measured arms use Chromium **156.0.8078.80**, the same audited fixed-seed
Transparent Grit fixture, a 3840x2160 canvas/viewport at scale 1, ANGLE Vulkan on
RADV Rembrandt, and `AMD_DEBUG=nodcc`. CEF is 156.0.15+gcf671c0. The installed
runtime's .96 engine pin remains unchanged; the .80 build is isolated. Both
archives matched the previously recorded SHA-256 hashes. Each run warms for
20 seconds then samples six ten-second windows. Run order was Chrome, native,
native, Chrome, then the Views diagnostic. Build/script-check services were
stopped before measured windows. Original kiosk/controld/watchdog services
remained running for every arm.

| Arm | Mean output interval | p95 | p99 | CPU (% of one core) | Process-tree PSS |
|---|---:|---:|---:|---:|---:|
| Chrome, run 1 | 80.96 ms | 83.33 ms | 100.00 ms | 13.70% | 591.1 MiB |
| Chrome, run 2 | 81.07 ms | 83.33 ms | 100.00 ms | 13.84% | 587.6 MiB |
| Native offscreen, run 1 | 93.46 ms | 100.00 ms | 116.67 ms | 17.50% | 648.8 MiB |
| Native offscreen, run 2 | 93.37 ms | 100.00 ms | 100.00 ms | 16.54% | 648.5 MiB |
| CEF Views diagnostic | 80.34 ms | 83.34 ms | 100.00 ms | 21.84% | 554.1 MiB |

The offscreen host takes approximately 15.3% longer per output frame in these
runs. Both native runs recorded zero CPU artwork copies. GPU-copy callback time
includes producer synchronization and averaged roughly 89 ms in run 1; it must
not be interpreted as 89 ms of blit work. The observation points to a
synchronization/composition cost, not a demonstrated byte-copy bandwidth limit.

Output intervals come from Chrome/Views `Display::FrameDisplayed` timestamps
and native Wayland changed-frame presentation feedback, restricted to the same
monotonic sample window. `summarize-matched.py` rejects mismatched engine,
fixture, viewport, canvas or graphics renderer, incomplete native samples and
non-monotonic-clock feedback. It reproduces the earlier retained pacing
summary before processing these new runs. Renderer rAF timestamps/delivery
intervals are retained separately; they are not substituted for output timing.
PSS is a read-only per-process snapshot after each sample, not peak allocation.
Cgroup memory varied with shared-page/cache accounting and is retained without
using it to claim a memory advantage. These short runs are not a statistical
performance or physical scanout qualification.

## Native Views evaluation

`tools/cef-views-probe.cpp` creates a fullscreen Alloy `CefBrowserView` in a
native `CefWindow`, on Cage/Wayland, with an isolated request context. It loads
only the fixed benchmark fixture. It has no playback state, product protocol,
labels, input coordinator, media worker or recovery policy. It is built by CI
to catch pinned-header/API breakage and excluded from normal installation.

The probe also ran on the production pin, Chromium 156.0.8078.96, with a
3840x2160 canvas and the same Vulkan renderer; its capability snapshot is
retained separately and is not a matched performance measurement.

This demonstrates that native Views works on both tested CEF binaries and device,
and provides a measured alternative to offscreen DMA-BUF capture. It does not
establish native-UI overlay fidelity, mixed-media composition, phone input
ownership, screenshot semantics, failure behavior or full performance parity.
The higher CPU result also needs investigation. Native Views should be evaluated
for the production composition path before further optimizing the synchronous
offscreen copy; implementation effort is not a reason to retain the slower path.

To stage the diagnostic beside the matching engine/resources:

```sh
cmake --build build --target cef-views-probe
DESTDIR=/absolute/stage cmake --install build --component diagnostic-probes
```

Use the staged binary, whose RPATH is `$ORIGIN`. A raw build-tree binary copied
beside resources still loads its build-tree libcef and failed ICU discovery in
the first attempt; that packaging failure is not evidence against Views.
`build.sh`, `launch.sh`, engine hashes, fixture metadata and all raw samples are
in `evidence/office-pacing-20261118`. `benchmark-matched.mjs` accepts the retained
`config.json` after the sample-window count; without it the older 1920-square GL
benchmark remains reproducible. The fixture generator and measurement scripts
are under `tools/`. Renderer captures and a separately reviewed Cage compositor
capture show the work; no physical-panel photograph was taken.

The production preview is restored to “24” and remains awake. Full controld
playback/session/watchdog activation, offline execution, full portfolio and
overnight acceptance remain incomplete. No PR or publishing workflow was used.

Fresh-context review found F1 (Moderate): incomplete feedback could be summarized
as a full run. The correction restores window coverage, single trace stream
and strictly increasing timestamp checks; five regression cases pass. Original
recordings pass the stricter checks with identical results. Linux
`make verify-scripts` passed on the device against source 9ad0b05a.

The final fresh-context review found no material findings (`Verdict: accept`),
reproduced all five retained summaries exactly, and checked evidence hashes and
the diagnostic-only install boundary. Twelve local CTest groups passed after
adding the benchmark validation regression test.
