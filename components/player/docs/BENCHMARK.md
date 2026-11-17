# Transparent Grit comparison and failure correction

## Failure and correction, 2026-11-18

The initial comparison failed. Polling a borrowed DMA-BUF with zero timeout and
skipping busy producers discarded almost every heavy WebGL frame. Waiting for
the GPU alone exposed another host defect: CEF's pinned `CefDoMessageLoopWork`
runs until idle, and continuous capture callbacks can prevent return to command
processing or presentation. Traces showed repeated completed `glFinish` calls
while native requests timed out; this was not evidence of a permanently deadlocked
GPU. A 20 ms producer wait also failed (one copy, 621 skipped frames in 71 s).

The host now schedules control/presentation/shutdown work as CEF UI tasks, copies
through Mesa implicit synchronization during the borrowed callback, coalesces
surplus callbacks, and drives external BeginFrames from host progress. Scheduling
alone still left a GPU backlog: a regression run saw 4,172 ms maximum command
latency. Coalescing without external pacing still saw 2,027 ms. With external
pacing, two consecutive occurrences of the same original URL stayed presented,
154 native calls completed (hello + command, median 120 ms, p95 224 ms, max 311 ms),
and both native compositor captures visibly show changing artwork plus the
separate device label. SIGTERM shutdown completed in 174 ms with metrics emitted.

The successful runtime invocation recorded 3,181 GPU copies, zero CPU copies,
972 coalesced callbacks, 147,515 ms aggregate import/copy/synchronization time
(46.37 ms per copy), and 5,597 swaps over 232 s. This includes selection tests and
benchmarking; it is not a benchmark-window-only frame count. The wait is part of
the reported copy time; these numbers do not imply the blit itself costs 46 ms.

Source evidence: pinned CEF
[`main_runner.cc`](https://github.com/chromiumembedded/cef/blob/6028375/libcef/browser/main_runner.cc#L131)
and [`video_consumer_osr.cc`](https://github.com/chromiumembedded/cef/blob/6028375/libcef/browser/osr/video_consumer_osr.cc).
No CEF engine patch is required for this host correction. The separate AMD nodcc
workaround and NVIDIA missing-fence limitation remain in ENGINE.md.

## Measured comparison

Device: homelab, Radeon 680M, FFOS 3.0.0 (image 2026.11.17), Linux 6.18.38-2-lts,
Mesa 26.1.4, Cage 0.3.1, 1920 × 1920 at 60 Hz, scale 1. The device had been
reimaged since the November 17 comparison; both arms below ran after that reset.
The existing kiosk and unrelated services remained active. Only one test runtime
ran at a time, with 6 GiB/512-task bounds, without builds or other playback tests
during each measurement. Native uses CEF 156.0.30 / Chromium 156.0.8078.96,
ANGLE gl-egl, accelerated OSR, host external frame pacing and AMD nodcc.
Standalone Chromium is Arch 150.0.7871.114 on Wayland, a fresh profile, and the
original artwork as a top-level document.

| Measurement | Standalone Chromium | Native host |
|---|---:|---:|
| CPU, percentage of one core | 15.70% | 21.42% |
| End-of-sample process-group memory | 180.2 MiB | 264.8 MiB |
| Process-group peak including run startup/history | 203.1 MiB | 287.3 MiB |
| Median rAF interval | 50.0 ms | 59.1 ms |
| p95 / p99 rAF interval | 50.1 / 66.7 ms | 127.8 / 133.0 ms |
| Sample duration | 122.75 s | 123.39 s |

Native rendered and remained controllable, but this sample used more CPU/memory
and had less even renderer scheduling. Do not describe it as faster than Chrome.
The peak is not a matched cold-start comparison: native's invocation also ran
occurrence-replacement checks before its benchmark window.

Each arm uses eight 15-second rAF windows. The observer adds one rAF callback per
browser frame. These intervals measure renderer scheduling, **not** actual canvas
updates, dropped display frames, scanout or input latency. Different engine and
backend versions prevent attribution solely to host overhead. The artwork itself
chooses a random seed at load, so identical URLs do not establish identical scene
instances; one sample per arm does not establish a statistical performance claim.

Evidence: `evidence/grit-native-fixed-benchmark.json`,
`evidence/grit-chrome-repeat-benchmark.json` and adjacent renderer JPEGs;
`grit-render-loop-check.json`, the two `grit-native-compositor-*.png` captures,
and `grit-fixed-runtime.log`. Captures were reviewed separately from navigation
success. They do not prove physical panel output or equivalence to an artist
reference. The original failed-run report remains reproducible from
`chrome-grit-benchmark.json` and `grit-sync-experiments.log`.

A further experiment moved BeginFrame before the host's vsync wait. Its separate
123.53 s sample used 18.05% of a core but had median/p95/p99 rAF intervals of
61.6/160.1/182.5 ms. It did not demonstrate improved pacing and was reverted;
`grit-native-overlap-benchmark.json` retains that result. Randomized scene content
means these samples do not isolate the timing change as the sole cause.

## Remaining qualification

The frame starvation/control failure is corrected for this tested FF1 path.
Frame pacing still needs optimization and portfolio verification. This is not
whole-player, offline, GPU-hang, physical-display, overnight, or release acceptance.

The final regression suite passed native input, media/decoder recovery, CEF
renderer recovery, image pixels and GLB interaction. Related corpus checks now
show native output for holz and 36 Points; Returns and Slime Skelly still time
out before navigation commits at their original IPFS URLs. They remain
unqualified. Test processes were stopped and the signed owner command confirmed
homelab sleeping after the tests (`grit-final-sleep.json`).
