# FF1 homelab browser and embedded video comparison

On 2026-11-20 we ran shipping ff-player 3.0.1, standalone Chrome for Testing, the public native host, and CEF on the same Radeon 680M FF1. Cage drove HDMI-A-1 at 1920×1920, 60 Hz, on FFOS 3.0.0 with Linux 6.18.38-2-lts. The [original comparison](https://github.com/feral-file/feral-file/issues/3615#issuecomment-7032855497) used FF1-SKYZ2E3A at 2160×3840. Those 159/317 ms public and 144/167 ms standalone results remain specific to that output mode. The fixture SHA-256 was `16b0e4824ed2d7f3a017ce9e57287ee5be7979b618ac9034a01dcf4d5631b13c` in both investigations.

## Transparent Grit

Each run used the same fixed-seed document, at device pixel ratio 1, with the artwork label and cursor hidden. The benchmark checked the canvas, viewport, seed marker and WebGL renderer. All used ANGLE Vulkan on RADV Rembrandt. The shipping arm loaded the artwork inside the shipping application's iframe. The standalone arm launched the exact Chromium executable from the public player package. Each arm warmed up for 20 seconds and ran six ten-second windows. Browser arms used Chromium's compositor display events. Native hosts used changed-frame feedback from the outer Wayland compositor. These timings stop at the compositor. CPU is kiosk cgroup use as a percentage of one core. Memory is that cgroup's reading at the end of the run.

| Runtime | Chromium | Mean ms | p95 ms | p99 ms | CPU % | Memory MiB |
| --- | --- | ---: | ---: | ---: | ---: | ---: |
| Shipping ff-player | 150.0.7871.114 | 53.49 | 66.67 | 66.67 | 17.1 | 290 |
| Standalone Chrome for Testing | 158.0.8127.36 | 54.42 | 66.67 | 66.67 | 15.1 | 264 |
| Public host before, 3b8dfaf5 | 158.0.8127.36 | 66.95 | 116.67 | 133.33 | 22.3 | 349 |
| Public host with deferred fence polling, 25828c29 | 158.0.8127.36 | 59.00 | 66.67 | 83.33 | 26.9 | 327 |
| Public host with Wayland FD polling, e0e915e9 | 158.0.8127.36 | 70.12 | 83.33 | 116.67 | 22.0 | 369 |
| Public host with direct RGB import, e21063b4 | 158.0.8127.36 | 57.24 | 66.67 | 83.33 | 27.9 | 358 |
| **Final public host, e9d751f3** | **158.0.8127.36** | **57.94** | **66.67** | **83.33** | **28.2** | **344** |

The final public mean exceeded standalone by 3.53 ms (6.5%). Its p95 matched, and its p99 was one 60 Hz refresh longer. The standalone repeat measured 54.69 ms mean. Shipping measured 54.29 ms on repeat. Both repeated at 66.67 ms p95 and p99.

### CEF trials

| Runtime | Chromium | Mean ms | p95 ms | p99 ms | CPU % | Memory MiB |
| --- | --- | ---: | ---: | ---: | ---: | ---: |
| CEF off-screen, adaptive frame clock, 25828c29 | 156.0.8078.48 | 62.68 | 66.67 | 116.67 | 12.7 | 630 |
| CEF Views, fenced blit, e98c06e5 | 156.0.8078.48 | 58.09 | 66.67 | 83.33 | 34.2 | 531 |
| CEF Views, direct RGB import, e21063b4 | 156.0.8078.48 | 58.87 | 66.67 | 83.33 | 30.7 | 409 |
| CEF Views, direct RGB import, repeat | 156.0.8078.48 | 57.23 | 66.67 | 83.33 | 35.3 | 441 |
| **Final CEF Views, e9d751f3** | **156.0.8078.48** | **57.29** | **66.67** | **83.33** | **32.0** | **404** |

CEF matched shipping at p95. Its mean remained 3.0–3.8 ms above the two shipping runs, and its p99 was one refresh longer. The final kiosk cgroup used 404 MiB at the end of the run, compared with 264–290 MiB for shipping.

The two-submission experiment at afb23112 measured 59.60/116.67 ms mean/p95 on the public engine and 57.35/116.67 ms on CEF. The compositor recorded 124 intervals over 100.5 ms on the public engine and 70 on CEF. The final host limits the normal path to one outstanding browser submission. The diagnostic flag `--diagnostic-browser-render-ahead` retains the two-submission experiment. Each submitted browser commit receives feedback from its own Cage swap, including discards.

The CEF Views runtime launches a windowed CEF worker inside the same per-artwork Wayland compositor used by the public host. The native host retains the RGB DMA-BUF, samples it directly, and releases the wlroots buffer once the GPU finishes its last read. The final CEF run recorded 1,495 direct imports with zero host GPU blits and zero CPU copies, including warm-up. The final public run recorded 1,473 fenced GPU blits.

The final public Chromium host releases browser buffers after the fenced blit. Public direct-import results varied. A 30-second run dropped 12 of 892 frames, a later single-submission run dropped 0 of 1,791, and the two-submission run dropped 38 of 1,789 across all six ten-second windows. Restoring the fenced blit dropped 0 of 1,790 in the same minute-long check. The windowed CEF worker sustained the same input while holding the buffers for direct sampling.

The public host can queue the browser blit and display draw on the same GPU context. The fence protects producer buffer reuse. Waiting for that fence before queuing the display draw had introduced an extra host scheduling delay.

## Build and installation

GitHub-hosted Ubuntu x86-64 built the FF1 host and windowed CEF worker. Both the off-screen host and windowed engine use the SHA-256-pinned Linux codec SDK from `maintenance/cef-trial.json`, based on CEF 156.0.8 and Chromium 156.0.8078.48. The workflow strips the debug symbols and hard-links the engine files shared by both paths. The installed `libcef.so` is 310,406,296 bytes, down from 538,044,752 bytes. Stripping saved 217 MiB on disk. The memory column above is a separate on-device measurement. The public and CEF artifacts each include their own runtime.

Both Linux builds passed all 19 CTest groups. The public build also passed the Go/C++ protocol interoperability check.

| e9d751f3 Linux x86-64 artifact | Archive bytes | SHA-256 of tar.gz |
| --- | ---: | --- |
| [CEF Views bundle](https://github.com/feral-file/ffos-user/actions/runs/44705250538/artifacts/14614922640) | 187,044,649 | `a062b2826d62628e4e931e729721ec356b7d5fb4f16455512da3cc35a3e71223` |
| [Public Chromium bundle](https://github.com/feral-file/ffos-user/actions/runs/44705250597/artifacts/14615546333) | 229,394,534 | `14c67fa15faa50286a369c997ba8735ccfbeea0dc05478efd76a03d744d9a8e7` |

The CEF bundle occupies 442 MiB under `/opt/feral/trials/cef-views-e9d751f3` on homelab. The host SHA-256 is `e511cf74776651990b8fd3b314eeb3bd273b8de864249d412594c971596128ae`. The windowed worker is `b04ff5388e90784f66d1b5f1c12ac120d5fa3ee0dc75533a5534a823cbb455ad`. The host and worker share the same `libcef.so` inode, with SHA-256 `663a639b934c31499f4de2566875affce16e18af267191d88d35abea3b1592db`. The installed sandbox helper is root-owned, mode 4755.

After the final restart, selected and presented occurrence IDs matched, the native host reported `cef-views`, HDMI DPMS was On, and the kiosk, controld, watchdog and system monitor were active. The [final compositor capture](evidence/homelab-parity-20261120/final-grit.png) shows fixed-seed Transparent Grit with the label and pointer hidden. The [static artwork test](evidence/homelab-parity-20261120/static-ui-check.json) also verified pointer movement, its five-second expiry, and both immediate and timed label removal.

The homelab kiosk runs `/opt/feral/trials/cef-views-e9d751f3/start-kiosk.sh` through its user-service drop-in. Normal startup uses the private browser control pipe with the diagnostic TCP endpoint and timing capture disabled. To restore the previous homelab CEF trial, run these commands as `feralfile` on that FF1.

```sh
rm ~/.config/systemd/user/chromium-kiosk.service.d/zz-native-cef-homelab.conf
systemctl --user daemon-reload
systemctl --user restart chromium-kiosk
```

## Video inside web artwork

The fixture page used a muted, looping HTML video with `object-fit: contain` on a full-viewport element over a dark background. The controller changed the media URL on the existing element for each sample. The shipping arm ran it inside ff-player's iframe. We recorded Chromium Media domain decoder selection and platform status, then measured the change in total and dropped frames over 30 seconds after four seconds of warm-up in the early trials. Final H.264 qualification ran for 60 seconds per clip, split into six ten-second windows. One public trial was interrupted by a controller sleepNow command. We woke homelab and restarted both clips from the beginning. Two captures from Cage, 650 ms apart per clip, showed the moving test pattern and the expected wide or square framing.

| Runtime | 3840×2160 H.264, 30 fps | 4096×4096 H.264, 2 fps | HEVC Range Extensions decoded |
| --- | --- | --- | ---: |
| Shipping, 30 s | VA-API, 0 dropped / 892 | FFmpeg software, 0 / 55 | 0 / 19 |
| **Shipping, 60 s** | **VA-API, 0 / 1,782** | **FFmpeg software, 0 / 114** | **0 / 19** |
| Public 25828c29 | VA-API, 0 / 894 | FFmpeg software, 0 / 59 | 0 / 19 |
| CEF off-screen 25828c29 | VA-API, 0 / 895 | FFmpeg software, 0 / 58 | 0 / 19 |
| Public direct import e21063b4 | VA-API, 12 / 892 | FFmpeg software, 0 / 60 | 0 / 19 |
| CEF Views direct import e21063b4 | VA-API, 0 / 895 | FFmpeg software, 0 / 59 | 0 / 19 |
| **Final public e9d751f3, 60 s** | **VA-API, 0 / 1,789** | **FFmpeg software, 0 / 114** | **0 / 19** |
| **Final CEF Views e9d751f3, 60 s** | **VA-API, 0 / 1,790** | **FFmpeg software, 0 / 114** | **0 / 19** |

GPU process diagnostics reported AMD driver 26.1.4, RADV Rembrandt, hardware compositing, rasterization, Vulkan, WebGL, WebGPU and accelerated video decode on shipping, public Chromium and CEF. WebGL ran the artwork in each arm. The WebGPU value is a browser capability report.

The decoder names were `VaapiVideoDecoder` with platform decoding and `FFmpegVideoDecoder` with software decoding. FFOS caps H.264 hardware decoding at 4096×4080 on VCN 3.1.1. The square clip exceeds that height. For the 19 HEVC Range Extensions cases, 14 reported `DEMUXER_ERROR_NO_SUPPORTED_STREAMS`. The other five advanced playback time with zero decoded frames and zero picture dimensions. The Main-profile BT.709 control (sample 8) produced 3840×2160 frames in every runtime tested. A resolved play promise or an advancing clock alone would misclassify those five. These are browser-embedded results. Standalone media has its own libmpv path.

The [fixture hashes](evidence/homelab-parity-20261120/fixture-sha256.txt), [per-file decoder observations](evidence/homelab-parity-20261120/embedded-video.json), [compositor captures](evidence/homelab-parity-20261120/video-output.jpg) and [raw timing files](evidence/homelab-parity-20261120/) accompany this report. To reproduce the output statistics, decompress each benchmark JSON and its sibling timing JSON into one directory and pass them to `tools/summarize-matched.py`. Group runs with the same engine version and document identity.

## Workers as Cage clients

We compared the deployed hosted path with one visible worker connected directly to Cage on the same homelab FF1 and HDMI mode. The device ran one workload at a time. The direct CEF run used the deployed CEF Views executable in probe mode. It opened Grit as a top-level document and bypassed the native host, private compositor and per-artwork launcher. This measures the whole change in launch and presentation path. It does not isolate the cost of a single copy.

| Fixed-seed Transparent Grit, in run order | Mean interval | p95 | p99 | Kiosk CPU, one core | Final cgroup memory |
| --- | ---: | ---: | ---: | ---: | ---: |
| Direct CEF | 55.06 ms | 66.67 ms | 66.67 ms | 25.5% | 363 MiB |
| Hosted CEF | 58.06 ms | 66.67 ms | 83.33 ms | 31.4% | 337 MiB |
| Direct CEF repeat | 55.38 ms | 66.67 ms | 66.67 ms | 20.1% | 271 MiB |

All three runs used CEF's Chromium 156.0.8078.48, the same fixed-seed document, the 1920×1920 canvas and ANGLE Vulkan on RADV Rembrandt. Each run warmed for 20 seconds and sampled six ten-second windows. Direct CEF used Chromium's display trace and hosted CEF used the outer Wayland compositor's presentation feedback. Both direct runs saved one refresh at p99. End-of-run memory varied enough that these runs do not establish a CEF memory saving. [Raw measurements and summary](evidence/direct-cage-20261120/)

### Hardware-decoded standalone video

Both arms ran the same GitHub-built media worker from 0813f7c2 with the deployed libmpv 0.42.0 and libavcodec libraries. We repeated the 3840×2160 H.264 30 fps fixture to 90 seconds by remuxing its original H.264 and AAC packets. Both paths fetched the same file from a loopback server. They played audio through PulseAudio at full volume and used fit scaling on black. We captured two images from Cage before each measurement, warmed up to 10 seconds of media playback, and recorded six ten-second windows per run.

The hosted worker renders into a texture, synchronizes it and exports it via DMA-BUF to the player for composition. The direct worker keeps the same media sandbox and libmpv renderer, draws into its fullscreen SDL window and swaps directly to Cage. Its Python control process lives in the kiosk cgroup. It sends the initialize and stop commands and collects properties. The hosted measurement client runs outside the kiosk cgroup.

In the first direct trial, PulseAudio's shared-memory allocation failed near the worker's 3 GiB address-space limit. Setting `MALLOC_ARENA_MAX=2` inside the sandbox reduced the observed direct worker virtual size from about 3.0 GiB to 1.2 GiB. We applied that setting to both trial launchers and required active PulseAudio for every measured run. The default deployed launcher keeps its original configuration.

We measured Cage presentation feedback on the Linux monotonic clock. The video interval is the time between presented artwork swaps. A redraw can present the same video frame twice. The separate libmpv decoder and video-output drop counters show whether libmpv itself dropped frames. The DRM decode-engine counter comes from the worker's amdgpu file descriptors, with duplicate DRM client IDs counted once. CPU covers the kiosk cgroup and memory is the mean of seven cgroup readings. Compositor captures verify visible content and motion. These runs did not measure panel latency or audible output.

| 4K H.264, in run order | Mean swap interval | p95 | p99 | Kiosk CPU, one core | Mean cgroup memory | GPU decode-engine time in 60 s |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| Direct libmpv | 33.43 ms | 33.33 ms | 33.35 ms | 12.0% | 228 MiB | 10.46 s |
| Hosted libmpv | 33.42 ms | 50.00 ms | 50.00 ms | 17.4% | 246 MiB | 9.94 s |
| Hosted repeat | 33.42 ms | 50.00 ms | 50.00 ms | 17.6% | 282 MiB | 10.30 s |
| Direct repeat | 33.43 ms | 33.33 ms | 50.00 ms | 11.1% | 225 MiB | 10.49 s |

All four runs reported VA-API hardware decoding and VA-API video surfaces, with the AMD decode-engine counter increasing in every ten-second window. All 24 windows recorded zero additional decoder or video-output drops. Two compositor captures per run showed changing pixels in the same letterboxed region. Direct presentation reduced p95 by one 60 Hz refresh in both comparisons at essentially identical average cadence. Direct p99 varied by one refresh between repetitions. A hosted attempt interrupted by a sleep command during warm-up was discarded and repeated after waking the device.

The single-artwork trial bypasses the player's overlays, transitions and input routing. A fullscreen video path has a measurable benefit on this hardware. Integration would need a tested handoff to hosted composition for UI and transitions and equivalent isolation for a directly displayed browser. These runs establish no benefit from changing the decoder or rebuilding CEF. [Media samples, Cage captures and measured intervals](evidence/direct-cage-20261120/video-summary.json) · [GitHub build of the tested worker](https://github.com/feral-file/ffos-user/actions/runs/44709998436)
