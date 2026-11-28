# Public Chromium cutover on FF1-SKYZ2E3A

The [homelab follow-up](HOMELAB-PARITY.md) compares the public player with standalone Chromium at 1920×1920 and records separate shipping and CEF results for embedded video.


On 2026-11-19, the normal kiosk, controld and watchdog on FF1-SKYZ2E3A were
switched to the public Chromium adapter. The device had been running the
earlier CEF cutover. The new host uses Chrome for Testing 158.0.8127.36 and the
private wlroots 0.20 compositor. Native libmpv 0.42 with FFmpeg 8.1.3 and dav1d
handles video, audio and raster images. SVG remains a browser document.
The host has no runtime libcef dependency. It binds the native Unix socket;
controld opens the separate cache broker in the same private directory.

The build reused the pinned private compositor SDK already built on the same
FF1. FFmpeg, libplacebo, libmpv and the host were built on-device. The first
media build needed Jinja2 in the private Meson environment. NASM 3.02-1 was
extracted from a signature-verified Arch package into a private directory and
the system package set was left in place. All 17 Linux CTest groups, schema
generation and the repository Go checks passed. This was a staged CMake
install, with FFOS packaging and full-image release work tracked separately.

The prior kiosk, launcher, binaries, OOM settings, and stopped SQLite state
are backed up under `/home/feralfile/ff19-rollback` on the FF1. The previous
browser profile was retained. The rollback entry point on the device is
`bash /home/feralfile/ff19-payload/ff19-rollback.sh`; it restores the previous
binaries and startup files and keeps the current durable playback state. The
quiesced pre-cutover state copy is available if a state rollback is needed.

| Installed executable | SHA-256 |
| --- | --- |
| Native host | `8084b43a295572ae23b547c15114634e9dd65a43e6db4c4d00d6c13872de34d6` |
| Chrome for Testing | `27513c5ffbfcb1e98e29b78d8dceb752cd343a77968335904f1b45a1c555e7b0` |
| Controld | `7eadc3ed1b50f4e1d51242982c593b634d9ac5ff09b1f5746146e4b7a8c69caa` |
| Watchdog | `9de0623adb6e633a28c8dcc9ed70b82fe312d81c55b3cc9f73f67c8362d61709` |

## Casting and latency

| Enrolled FF-CLI cast | Controld HTTP handling | CLI exit | First host composition from CLI launch |
| --- | ---: | ---: | ---: |
| Social Codes, official 10-work playlist | 32 ms | 6.40 s | 8.84 s |
| Single holz work, player initially asleep | 396 ms | 6.54 s | 7.32 s |

The single-work command woke the player and composed the holz occurrence at
its original URL. A Cage screenshot shows artwork geometry and an incomplete
offline notice. A manually imported capture was already present on this test
device. The screenshot establishes compositor output only.

In the previous slow phone requests, the device spent 34.6 seconds handling
the cast, mostly in synchronous catalog expansion. That path now bounds
optional interactive expansion to two seconds and external preflight to one
second. Separately on the test Mac, `.local` lookup took 5.009 seconds and curl
reported 5.007 seconds before connecting. An enrolled `getDeviceStatus` took
5.16 seconds with the configured `.local` hostname and 263 ms with the same
device's IP. Controld logged 17 ms on the hostname call. These are different
latency sources, and the Mac result does not establish phone DNS behavior.

After recovery testing, the enrolled CLI transport sent the same Infinite
Entropy URL used in the earlier latency comparison directly to the device IP.
It received `accepted` in 2.68 seconds. The native host subsequently reported
the selected work presented in the 25-item playlist. The screen was left awake
on that playlist under the ordinary kiosk launcher.

## Transparent Grit

The two arms used the same Chrome executable, audited fixed seed
`feral-frame-pacing-20261118`, original artwork source hash, 2160×3840 viewport
and canvas, scale 1, Cage at 60 Hz, and ANGLE Vulkan on RADV Rembrandt. Both
ran under the same kiosk service. Each arm warmed for 20 seconds and sampled
six ten-second windows. The native run was first. The standalone Chrome arm
temporarily replaced the kiosk command for that measurement.

| Measurement | Standalone Chrome | Native public adapter |
| --- | ---: | ---: |
| Mean output interval | 143.96 ms | 159.07 ms |
| p95 output interval | 166.67 ms | 316.67 ms |
| p99 output interval | 166.67 ms | 350.00 ms |
| CPU, percent of one core | 6.87% | 18.93% |
| Kiosk cgroup memory at end | 227.93 MiB | 363.88 MiB |

The native mean interval was 10.5% longer and p95 was 1.90× Chrome's.
The output comparison uses Chrome display trace timestamps and native Cage
Wayland feedback in the same monotonic time window per arm. Separate rAF
observations reproduced the disparity. The native sample clustered around one
refresh interval and 17–19 refresh intervals, while Chrome's rAF sample was
concentrated near nine intervals. This is a two-arm run of one work on one device.

Native telemetry recorded zero CPU artwork copies, 727 GPU copies including
warm-up, and 0.49 ms mean copy submission time. The fence was observed ready
after 67.68 ms on average, with a 153 ms p95 bin. The host received matching
Cage feedback after another measured stage averaging 81.47 ms from submission,
with zero fallback acknowledgements. These intervals include scheduling and
waiting on GPU work. They do not measure GPU blit execution time and should
not be added to estimate a causal delay. Device-wide GPU memory at sample end
was about 502 MiB VRAM and 571 MiB GTT for native vs. 423 and 319 MiB for Chrome.
Those counters include other device allocations.

The measurements make the extra composition and synchronization stages the
leading suspects. The timing counters cannot isolate the contributing stages
from one another. An earlier queue change, early
frame callbacks with later actual presentation feedback, did not materially
change the prior homelab result (67.37 → 66.81 ms mean rAF on 1920×1920).
Producer-buffer import into the final composition pass would test the extra
GPU copy directly. It needs a consumer fence and explicit buffer retirement
to preserve isolation and the last presented frame during transitions.

Raw samples, renderer callback intervals, the fixed fixture manifest, and
cast evidence are under [evidence/public-engine-ff1-20261119](evidence/public-engine-ff1-20261119/).

## Recovery

| Injection on the normal kiosk | Observation |
| --- | --- |
| Kill the fixture's Chromium renderers | Host reports renderer crash and remains responsive. A new occurrence composes after 2.08 s, in the same runtime and generation. |
| Kill the fixture's browser process group | Host reports browser exit and remains responsive. The second bounded retry composes after 3.16 s, in the same runtime and generation. |
| Restart controld | Playback retains the runtime, generation and exact occurrence. |
| Stop the host with SIGSTOP | Watchdog restarts the kiosk and playback recovers in 31.29 s. The enrolled device-status request returns during the outage, after 5.63 s. |
| Kill the host with SIGKILL | systemd creates a new host and restores the playlist in 8.09 s. The enrolled device-status request returns during the outage, after 0.53 s. |

The recovery timings are single observations. The fixture uses the same
production launcher, units and socket as ordinary playback. The tests inject
process failures. GPU resets, thermal hardware escalation, physical panel
timing and an overnight portfolio run remain unverified on this build.

On the installed runtime, compositor pixel checks passed for JPEG EXIF rotation,
PNG straight alpha, AVIF auxiliary alpha and GIF animation. The raster cases
all selected the native backend. This extends the homelab test to FF1-SKYZ2E3A.

Service-worker replay remains incomplete. The original-URL cold replay
fixture on homelab exercised fonts, WASM, redirects, frames and dedicated
workers. The cache reports its uncovered dependencies. The capsule stays
deferred, and fleet image qualification still requires native package staging
in the image pipeline.

The later legacy-fade update replaced the native host and media worker on the
same FF1, keeping the device's playback state. The previous pair is under
`/home/feralfile/ff-transition-rollback.JrsZ2g`. The four normal services and
readiness targets were active, playback had a presented occurrence, and the
monitor reported DPM On after the update. See the
[transition evidence](evidence/legacy-transition-20261119/compositor-pixels.json).
