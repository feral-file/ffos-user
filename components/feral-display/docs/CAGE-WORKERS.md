# FF1 native workers on homelab

Controld remains the device command owner. The CLI verifies the DP-1 signature
and sends the playlist to controld. The native player accepts the playlist on
its Unix socket, resolves the offline-cache policy and selects a worker. The
ordinary Cage session runs one playback host. On homelab the legacy
`feral-player.service` is inactive.

| Input | Rendering path |
| --- | --- |
| JPEG, PNG, AVIF and other raster images | libmpv and FFmpeg in the sandboxed media worker |
| Direct video or audio | libmpv with FFmpeg. VA-API depends on the stream profile and dimensions. |
| SVG, web and software artwork | Windowed CEF Views. The host authorizes the initial window after the main document loads. |
| GLB/glTF | CEF with the bundled model-viewer and its resource policy |

Raster images decode at their original dimensions. If an edge exceeds 8192
pixels or the GPU's texture limit, the media worker uses FFmpeg to fit the
image within a 4096-pixel square (smaller on GPUs with a lower limit) before
upload. The worker reports the decoded dimensions in `video-params/w` and
`video-params/h` and the filtered dimensions in `video-out-params/w` and
`video-out-params/h`.

The libmpv trial preserves PNG transparency. On homelab the auxiliary-alpha AVIF fixture renders opaque blue, so transparent AVIF parity remains open.

A stable fullscreen media worker submits its own Wayland buffers to Cage. The
host requests composited frames when it owns the label, cursor, margins or a
transition. CEF uses a capability-filtered Wayland relay to send its buffers
to Cage. It falls back to composition when the layout changes or presentation
cannot be confirmed. A browser loaded composited for a fade stays in that mode
to preserve document state. Embedded HTML video remains inside CEF.

## Device and procedure

Test hardware was homelab, Radeon 680M, FFOS 3.0.0, Linux 6.18.38-2-lts,
Cage 0.3.1, HDMI-A-1 at 1920×1920 and 60 Hz. The earlier raster measurements below used glycin 2.1.5-2 and the device libheif package. The media worker uses libmpv 0.42.0 and FFmpeg 8.1.3.
The CEF SDK is 156.0.8 with Chromium 156.0.8078.48.

```sh
ff-cli display https://feed.feralfile.com/api/v1/playlists/unsupervised-sla -d homelab
ff-cli sign bbb.json
ff-cli display bbb.json -d homelab
```

ff-cli 4.1.0 sent the original Feral File playlist. To cover each item
without waiting through its full run, the test selected each index through
the same native playback socket used by controld. A separate signed playlist
with 15-second items exercised automatic handoff from a channel JPEG to video,
SVG, HTML, fixed-seed *Transparent Grit* and a GLB model. No debugger was
enabled on the product worker.

For each media sample the test recorded the presented occurrence, libmpv
properties, per-client AMD decode-engine counter delta and two compositor
captures. DRM clients were deduplicated by client ID. The drop counts are
deltas over each observation window. A static single-frame clip requires
comparison with source pixels. An advancing clock or a successful decode
alone cannot establish visible output.

The source clips for the compatibility playlist were the 30-second AVC 1080p
and HEVC 2160p *Big Buck Bunny* samples. MKV and TS inputs were remuxed. Main10,
4:4:4 Range Extensions, VP9 and AV1 inputs were encoded from the source
material. The tests used a range-capable loopback server on the device.
Film copyright 2008 Blender Foundation, CC BY 3.0.
[Sample provenance](https://github.com/renesas-rz/media/blob/main/Big_Buck_Bunny/README.md)

The [shipping ff-player comparison](HOMELAB-PARITY.md) covers browser-embedded
video. The worker results here cover direct media URLs on the same device.
Captures show Cage output. The test did not use a camera or measure physical
HDMI latency.

## Observations

*Unsupervised* contains nine JPEGs and four H.264 videos. The nine JPEG captures
differed from the color-managed source thumbnails by less than 0.55 levels per
channel on an 8-bit scale. All four videos moved, with zero additional
libmpv drops in their five-second sample windows. Three used VA-API.
*Data Universe* used FFmpeg software decoding. Its 4096×4096 frame exceeds
the Radeon driver's 4080-pixel H.264 height limit. The shipping browser also
selected software at that size.

The signed *Big Buck Bunny* playlist covered AVC in MP4, HEVC Main in MP4
and MKV, Main10 in MP4 and MPEG-TS, 10-bit 4:4:4 HEVC, VP9 and AV1. Seven
used VA-API with a positive per-worker hardware decoder counter. The 4:4:4
clip used software. The two sustained MP4 tests used 95-second remuxes so
the counters did not reset at a loop boundary. Over 60.75 seconds of
observation, HEVC Main 2160p30 added 12.38 seconds of Radeon decode-engine
time and Main10 1080p added 4.73 seconds. Both recorded zero additional output
or decoder drops. Separate source-pixel checks covered JPEG EXIF orientation,
PNG and AVIF alpha over black, and two distinct frames of an animated GIF.

The supplementary comparison used player build `469da8cf`. Direct libmpv
presented all 19 Range Extensions inputs using software. The twelve
single-frame inputs required a fullscreen repaint when handing the surface
to Cage. High-rate 4K software decoding exceeded the device's real-time
budget. The shipping comparison recorded zero decoded frames for all 19
inside an HTML video element.

On fixed-seed *Transparent Grit*, CEF averaged 53.97 ms per presented interval
over 60 seconds. Shipping ff-player averaged 53.49 ms on this display. The
CEF run counted outer Wayland presentations and the shipping run used
Chromium tracing. The 0.48 ms difference is descriptive. The CEF run captured
aggregate counters, so it cannot establish p95 parity.

[Per-item measurements and control results](evidence/cage-workers-20261120/results.json)
· [Unsupervised captures](evidence/cage-workers-20261120/ff1-original.jpg)
· [Big Buck Bunny captures](evidence/cage-workers-20261120/ff1-bbb.jpg)
· [HEVC corpus captures](evidence/cage-workers-20261120/native-corpus.jpg)

GitHub Actions built the installed revision, `c20fdb7b`, with the pinned CEF
SDK. The CEF build passed all 19 CTest groups. The archive on the device
matched the local SHA-256,
`a9c589d9a3cf03102db36f386bbc184855b31a497ec312f4f422c28880552345`.
The media worker in this archive is byte-for-byte identical to the worker in
`469da8cf`. The FFOS package recipe accepted the archive and FF1's
`makepkg --printsrcinfo` resolved the generated metadata. Homelab ran the
unpacked archive through a persistent kiosk service override.
[CEF build](https://github.com/feral-file/ffos-user/actions/runs/44725695811)
· [FFOS package recipe](https://github.com/feral-file/ffos/commit/f7d09d8)

The signed mixed playlist advanced through all six items automatically. The
control checks observed pause, seek, resume, mute, volume, a black sleep
capture and wake. A CEF page with a CSS breakpoint rendered green at a
5-pixel margin and red at 100 pixels, with black outer corners. Captures
taken immediately at the layout acknowledgment could still be black. The
pixel check sampled two seconds after acknowledgment and confirmed the
new layout in each direction. During the fade from SVG to HTML the
compositor captured the outgoing color fading through near-black into the
incoming page.

Homelab rebooted unexpectedly three times during accelerated playlist
traversal. The previous-boot journal ended abruptly, with no recorded
cause. The installed runtime presented every channel item across resumed
checks. The final image sweep allowed five extra seconds between selections.
The JSON records the build that produced each retained capture. Unsupervised
resumed after a kiosk restart, with HDMI enabled, the player awake and the
legacy service inactive. Unattended device stability remains open.

## Rapid traversal regression, November 20

The original failing workload cast the signed *Unsupervised* playlist to homelab, ran the normal native playback selection command through all 13 items without an extra inter-item dwell, and took two Cage captures per item. The tester waited for each direct surface to present, then waited 1.3 s before the first capture. The interval between captures was 0.8 s for a JPEG and 5 s for a video. The nine JPEGs each measure 3384 × 6016. This is an accelerated traversal, separate from unattended playback.

To reproduce, we ran the previously affected build `c20fdb7b` in an isolated playback-state directory on the same FF1. The original worker completed *MoMA Dreams — F* (index 8) at 19:37:44 ICT. Sensor and package-power telemetry streamed over Ethernet stopped in the same second. The control port, SSH and ping all became unreachable. The next boot was observed after the operator re-powered the device. We have no independent measurement of whether power was lost or the system hung before that intervention.

The last sampled CPU and GPU temperatures were 78°C and 57°C. The system reported 9,560 MiB of available memory, one media worker and a GTK-associated SVG loader alongside the artwork JPEG loader. The final CPU package power estimate was 19.88 W, averaged over 150 ms. It measures neither wall power nor sub-sample transients. The remote kernel stream had no terminal panic, OOM, GPU reset or thermal-trip message. The next boot had no pstore record.

A separate media-worker SIGSEGV at 19:04:03 followed an authenticated display wake. Its stack entered libdecor-gtk from Wayland dispatch inside SDL3. The FF1 remained on the same boot following that process crash. The Cage media launcher now sets `SDL_VIDEO_WAYLAND_ALLOW_LIBDECOR=0`. Its fullscreen and hidden windows have no use for client-side decorations. The isolated comparison used the earlier host, CEF and media binaries and changed only the media launcher's decoration setting. Inspection of the running worker confirmed the setting and zero mappings of libdecor-gtk or GTK.

With the original binaries and the launcher change, three consecutive traversals produced 39/39 direct presentations with zero kiosk restarts. The nine JPEG captures from the last pass were pixel-identical to the earlier reference captures at 480 × 480. A preceding JPEG-only test on build `7c987a14` also completed 18/18 presentations with decorations still enabled. The full mixed workload is the reproduction case. A sample of the packaged FF1 worker on the same device confirmed the GTK library was absent.

GitHub built the candidate archive from `4cdf23fa`. All 19 CTest groups and the Go/C++ command interoperability check passed. The transferred archive matched its checksum. On this archive, the CLI cast the original channel and the test observed 26/26 direct presentations over two sweeps. An incoming `sleepNow` command interrupted the first sweep at 19:46:56; the device remained reachable and resumed after wake. All nine final JPEG captures matched the prior 480 × 480 reference captures. [Build and artifact](https://github.com/feral-file/ffos-user/actions/runs/44737069377).

Each hardware-decoded HEVC run lasted more than 60 s with zero additional output or decoder drops. Three authenticated screen sleep/wake cycles each returned to the playing H.264 artwork. The final boot ID matched the start of the patched comparison, the service reported zero restarts, and the journal listed no new process core dumps. The FF1 was left awake on the original signed playlist. This qualifies a mitigation for the reproduced workload. The underlying machine-level reset mechanism and long-run failure rate remain unknown. [Per-run observations](evidence/reset-reproduction-20261120/results.json)
