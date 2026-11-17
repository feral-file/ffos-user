# Transparent Grit comparison — 2026-11-17

Native presentation **failed this comparison**. The current branch must not be
qualified for the full artwork portfolio or represented as faster than Chrome.

Device: homelab, AMD Radeon 680M, FFOS 3.0.0, Mesa 26.1.4, Cage 0.3.1,
1920 × 1920 at 60 Hz, scale 1. Standalone Arch Chromium 150.0.7871.114
opened the original Transparent Grit URL as the top-level document in a fresh
profile. Native uses CEF 156.0.30 / Chromium 156.0.8078.96, accelerated offscreen
DMA-BUF composition, ANGLE gl-egl, Vulkan and the AMD nodcc workaround. These
different engine/backend versions prevent attribution solely to host overhead.
The original kiosk and other device services remained active during both arms.
Only one test runtime ran at a time, each limited to 6 GiB and 512 tasks.

| Measurement | Standalone Chromium | Native host |
|---|---:|---|
| Artwork visible in captured output | Yes, renderer capture inspected | Intermittent first frame / no usable presentation; timed out |
| CPU, percentage of one core | 16.77% | Not comparable: failed presentation |
| End-of-sample cgroup memory | 276.1 MiB | Not comparable |
| Peak cgroup memory including startup | 284.5 MiB | Not comparable |
| Median / p95 rAF interval | 50.0 / 50.1 ms | No valid successful-playback sample |
| p99 rAF interval | 66.7 ms | No valid sample |

Chromium was sampled for eight 15-second rAF windows. The observer adds one
rAF callback per browser frame; intervals measure renderer scheduling, **not**
actual canvas updates, dropped display frames, scanout, or input latency.
The visible captured pattern and 1920-square canvas establish more than a
successful navigation, but do not establish artist-reference equivalence or
physical panel output. Raw measurements and the renderer capture are
`evidence/chrome-grit-benchmark.json` and its adjacent JPEG.

Native tracing showed 1,743 busy producer-buffer observations but only 23 GPU
copies over a failed/retrying invocation. Polling without waiting can repeatedly
skip this canvas's frames. A nominal two-millisecond wait did not resolve it.
Submitting the implicit-fence GPU copy and waiting for completion stalled the
host command interface. Systemd stopped the isolated experiment; original
controld/watchdog/kiosk services stayed active. Both experiments were reverted. The isolated staging tree was rebuilt/restored to the prior bounded path without restarting playback; the installed development package was unchanged.
The shipping candidate still needs a CEF/Chromium buffer-ownership/synchronization
fix and another matched run; renderer screenshots alone do not fix this defect.

After testing, the owner CLI sent `sleepNow` successfully. Controld reported
`currentState: sleeping`, `overrideState: sleeping`. Test runtimes were stopped.
This is device-state confirmation, not a photograph or measurement of panel power.
